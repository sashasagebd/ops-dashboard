// Package source fetches an app's code at an exact commit and turns it into a
// Docker build context, without git: GitHub serves any commit as a tarball,
// and a build context is a tar stream too, so the code is streamed from one
// to the other without being written to disk.
//
// Public repos only for now; private ones need just a token header later.
package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Default size limits, so a huge (or malicious) repo can't fill the disk or
// memory: the download itself, and the unpacked total, which also catches a
// small archive that decompresses to something enormous.
const (
	DefaultMaxDownloadBytes = 100 << 20 // 100 MiB
	DefaultMaxUnpackedBytes = 500 << 20 // 500 MiB
)

// ErrNotFound means GitHub has no such repo or commit (or the repo is private,
// which looks the same without a token).
var ErrNotFound = errors.New("repository or commit not found (private repos aren't supported yet)")

// ErrTooLarge means the code exceeded a size limit.
var ErrTooLarge = errors.New("repository is larger than the platform's limit")

// GitHub fetches code from GitHub's API.
type GitHub struct {
	HTTPClient       *http.Client
	APIBase          string // https://api.github.com, or a test server
	MaxDownloadBytes int64
	MaxUnpackedBytes int64
}

// NewGitHub returns a GitHub client with the default limits. It has no
// client-wide timeout: a download legitimately takes as long as it takes,
// and the caller's context bounds it.
func NewGitHub() *GitHub {
	return &GitHub{
		HTTPClient:       &http.Client{},
		APIBase:          "https://api.github.com",
		MaxDownloadBytes: DefaultMaxDownloadBytes,
		MaxUnpackedBytes: DefaultMaxUnpackedBytes,
	}
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// BuildContext returns the code of repoURL at commitSHA as an uncompressed tar
// stream, ready to send to Docker as a build context. The caller must close
// it. Errors that happen mid-stream (a size limit, a broken download) arrive
// from Read, so the build fails rather than seeing truncated code.
func (g *GitHub) BuildContext(ctx context.Context, repoURL, commitSHA string) (io.ReadCloser, error) {
	owner, repo, err := ParseRepoURL(repoURL)
	if err != nil {
		return nil, err
	}
	// Checked here as well as by the database, because it goes into a URL.
	if !shaPattern.MatchString(commitSHA) {
		return nil, fmt.Errorf("commit %q: want a full 40-character SHA", commitSHA)
	}

	u := fmt.Sprintf("%s/repos/%s/%s/tarball/%s", g.APIBase, url.PathEscape(owner), url.PathEscape(repo), commitSHA)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	// GitHub answers with a redirect to its download host, which the client
	// follows.
	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading code: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		resp.Body.Close()
		return nil, fmt.Errorf("downloading code: GitHub answered %s", resp.Status)
	}

	// Repacking runs in a goroutine writing into a pipe, so the code streams
	// straight through to Docker. If the reader is closed early (e.g. the
	// build is cancelled), the next write fails and the goroutine stops.
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		download := &limitedReader{r: resp.Body, remaining: g.MaxDownloadBytes}
		pw.CloseWithError(repack(download, pw, g.MaxUnpackedBytes))
	}()
	return pr, nil
}

// repack copies GitHub's .tar.gz to an uncompressed tar, removing the
// top-level folder GitHub wraps everything in ("owner-repo-sha/"), so the
// repo's root (and its Dockerfile) is the root of the build context.
func repack(gz io.Reader, out io.Writer, maxUnpacked int64) error {
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return fmt.Errorf("reading code archive: %w", err)
	}
	tr := tar.NewReader(zr)
	tw := tar.NewWriter(out)
	var unpacked int64

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading code archive: %w", err)
		}

		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader:
			// GitHub's first entry: metadata (the commit SHA), not a file.
			continue
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		default:
			return fmt.Errorf("code archive: %s: unsupported entry type %q", hdr.Name, hdr.Typeflag)
		}

		name, ok := stripTopFolder(hdr.Name)
		if !ok {
			return fmt.Errorf("code archive: unsafe path %q", hdr.Name)
		}
		if name == "" {
			continue // the top-level folder itself
		}

		unpacked += hdr.Size
		if unpacked > maxUnpacked {
			return ErrTooLarge
		}

		// A fresh header with only what a build needs: no owner names or
		// other metadata from GitHub's side.
		out := &tar.Header{
			Typeflag: hdr.Typeflag,
			Name:     name,
			Linkname: hdr.Linkname, // symlinks; BuildKit keeps them inside the context
			Mode:     hdr.Mode,
			Size:     hdr.Size,
			ModTime:  hdr.ModTime,
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return fmt.Errorf("reading code archive: %w", err)
			}
		}
	}
	return tw.Close()
}

// stripTopFolder removes the first path element. ok is false for a path that
// could land outside the build context (absolute, or using ".."). GitHub never
// produces those, but the archive comes from the network, so it isn't
// trusted.
func stripTopFolder(name string) (stripped string, ok bool) {
	if strings.HasPrefix(name, "/") {
		return "", false
	}
	_, rest, found := strings.Cut(name, "/")
	if !found || rest == "" {
		return "", true
	}
	for _, part := range strings.Split(rest, "/") {
		if part == ".." {
			return "", false
		}
	}
	return path.Clean(rest), true
}

// ParseRepoURL accepts https://github.com/owner/repo, with or without a
// trailing ".git" or "/".
func ParseRepoURL(s string) (owner, repo string, err error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("repo URL %q: want https://github.com/owner/repo", s)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("repo URL %q: want https://github.com/owner/repo", s)
	}
	owner, repo = parts[0], strings.TrimSuffix(parts[1], ".git")
	if !namePattern.MatchString(owner) || !namePattern.MatchString(repo) ||
		owner == "." || owner == ".." || repo == "." || repo == ".." || repo == "" {
		return "", "", fmt.Errorf("repo URL %q: want https://github.com/owner/repo", s)
	}
	return owner, repo, nil
}

// GitHub owner and repo names: letters, digits, '-', '_' and '.'.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// limitedReader is io.LimitReader, but reaching the limit is an error
// (ErrTooLarge) rather than a quiet, truncated EOF.
type limitedReader struct {
	r         io.Reader
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		// Probe for one more byte: exactly at the limit is fine.
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			return 0, ErrTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}
