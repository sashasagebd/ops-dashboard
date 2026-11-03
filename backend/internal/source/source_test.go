package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

type entry struct {
	name     string
	typeflag byte
	body     string
	linkname string
}

// githubTarball builds a .tar.gz shaped like GitHub's: a pax global header
// carrying the commit, then everything under one top-level folder.
func githubTarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
		PAXRecords: map[string]string{"comment": sha},
	}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "owner-repo-0123456/", Mode: 0o755}))
	for _, e := range entries {
		hdr := &tar.Header{Typeflag: e.typeflag, Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.linkname}
		if e.typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		must(tw.WriteHeader(hdr))
		if e.typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			must(err)
		}
	}
	must(tw.Close())
	must(zw.Close())
	return buf.Bytes()
}

// fakeGitHub serves tarball at the API path, via a redirect to a download
// path as the real API does.
func fakeGitHub(t *testing.T, tarball []byte) *GitHub {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/owner/repo/tarball/"+sha, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/codeload/owner/repo/legacy.tar.gz/"+sha, http.StatusFound)
	})
	mux.HandleFunc("GET /codeload/owner/repo/legacy.tar.gz/"+sha, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	g := NewGitHub()
	g.HTTPClient = srv.Client()
	g.APIBase = srv.URL
	return g
}

// readContext reads a build context, returning its entries as name ->
// body (or "-> target" for symlinks, "/" for directories).
func readContext(t *testing.T, rc io.ReadCloser) (map[string]string, error) {
	t.Helper()
	defer rc.Close()
	files := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			files[hdr.Name] = "/"
		case tar.TypeSymlink:
			files[hdr.Name] = "-> " + hdr.Linkname
		default:
			body, err := io.ReadAll(tr)
			if err != nil {
				return files, err
			}
			files[hdr.Name] = string(body)
		}
	}
}

func TestBuildContextStripsGitHubsFolder(t *testing.T) {
	g := fakeGitHub(t, githubTarball(t,
		entry{name: "owner-repo-0123456/Dockerfile", typeflag: tar.TypeReg, body: "FROM scratch\n"},
		entry{name: "owner-repo-0123456/src/", typeflag: tar.TypeDir},
		entry{name: "owner-repo-0123456/src/main.go", typeflag: tar.TypeReg, body: "package main\n"},
		entry{name: "owner-repo-0123456/current", typeflag: tar.TypeSymlink, linkname: "src"},
	))

	rc, err := g.BuildContext(t.Context(), "https://github.com/owner/repo", sha, "")
	if err != nil {
		t.Fatal(err)
	}
	files, err := readContext(t, rc)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"Dockerfile":  "FROM scratch\n",
		"src":         "/",
		"src/main.go": "package main\n",
		"current":     "-> src",
	}
	if len(files) != len(want) {
		t.Errorf("build context has %v, want %v", keys(files), keys(want))
	}
	for name, body := range want {
		if files[name] != body {
			t.Errorf("%s = %q, want %q", name, files[name], body)
		}
	}
}

func TestBuildContextFromADirectory(t *testing.T) {
	g := fakeGitHub(t, githubTarball(t,
		entry{name: "owner-repo-0123456/Dockerfile", typeflag: tar.TypeReg, body: "root\n"},
		entry{name: "owner-repo-0123456/examples/", typeflag: tar.TypeDir},
		entry{name: "owner-repo-0123456/examples/hello/", typeflag: tar.TypeDir},
		entry{name: "owner-repo-0123456/examples/hello/Dockerfile", typeflag: tar.TypeReg, body: "hello\n"},
		entry{name: "owner-repo-0123456/examples/hello/cmd/main.go", typeflag: tar.TypeReg, body: "package main\n"},
		// A sibling whose name starts the same: not part of examples/hello.
		entry{name: "owner-repo-0123456/examples/hello-two/Dockerfile", typeflag: tar.TypeReg, body: "two\n"},
	))

	rc, err := g.BuildContext(t.Context(), "https://github.com/owner/repo", sha, "examples/hello")
	if err != nil {
		t.Fatal(err)
	}
	files, err := readContext(t, rc)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Dockerfile": "hello\n", "cmd/main.go": "package main\n"}
	if len(files) != len(want) {
		t.Errorf("build context has %v, want %v", keys(files), keys(want))
	}
	for name, body := range want {
		if files[name] != body {
			t.Errorf("%s = %q, want %q", name, files[name], body)
		}
	}
}

func TestBuildContextDirectoryErrors(t *testing.T) {
	g := fakeGitHub(t, githubTarball(t,
		entry{name: "owner-repo-0123456/Dockerfile", typeflag: tar.TypeReg, body: "root\n"},
	))

	// Missing: found only by reading the whole archive, so it arrives from Read.
	rc, err := g.BuildContext(t.Context(), "https://github.com/owner/repo", sha, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readContext(t, rc); !errors.Is(err, ErrDirNotFound) {
		t.Errorf("missing dir: error = %v, want ErrDirNotFound", err)
	}

	// Paths that aren't clean and relative are refused before downloading.
	for _, dir := range []string{"..", "../x", "/etc", "a/../b", "a/", "."} {
		if _, err := g.BuildContext(t.Context(), "https://github.com/owner/repo", sha, dir); err == nil {
			t.Errorf("dir %q: accepted", dir)
		}
	}
}

func TestBuildContextErrors(t *testing.T) {
	big := strings.Repeat("x", 4096)

	tests := []struct {
		name        string
		entries     []entry
		maxDownload int64
		maxUnpacked int64
		want        error  // checked with errors.Is, if set
		wantText    string // or a substring of the error
	}{
		{
			name:     "path escaping the context",
			entries:  []entry{{name: "owner-repo-0123456/../../etc/passwd", typeflag: tar.TypeReg, body: "x"}},
			wantText: "unsafe path",
		},
		{
			name:     "absolute path",
			entries:  []entry{{name: "/etc/passwd", typeflag: tar.TypeReg, body: "x"}},
			wantText: "unsafe path",
		},
		{
			name:     "hard link",
			entries:  []entry{{name: "owner-repo-0123456/link", typeflag: tar.TypeLink, linkname: "x"}},
			wantText: "unsupported entry type",
		},
		{
			// Compresses to almost nothing, so only the unpacked limit
			// catches it: the "zip bomb" case.
			name:        "unpacked too large",
			entries:     []entry{{name: "owner-repo-0123456/big", typeflag: tar.TypeReg, body: big}},
			maxUnpacked: 1024,
			want:        ErrTooLarge,
		},
		{
			name:        "download too large",
			entries:     []entry{{name: "owner-repo-0123456/big", typeflag: tar.TypeReg, body: big}},
			maxDownload: 50,
			want:        ErrTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := fakeGitHub(t, githubTarball(t, tt.entries...))
			if tt.maxDownload > 0 {
				g.MaxDownloadBytes = tt.maxDownload
			}
			if tt.maxUnpacked > 0 {
				g.MaxUnpackedBytes = tt.maxUnpacked
			}

			rc, err := g.BuildContext(t.Context(), "https://github.com/owner/repo", sha, "")
			if err == nil {
				// Archive problems surface while reading the stream.
				_, err = readContext(t, rc)
			}
			switch {
			case err == nil:
				t.Fatal("got a build context, want an error")
			case tt.want != nil && !errors.Is(err, tt.want):
				t.Errorf("error = %v, want %v", err, tt.want)
			case tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText):
				t.Errorf("error = %v, want one containing %q", err, tt.wantText)
			}
		})
	}
}

func TestTokenGoesToTheAPIOnly(t *testing.T) {
	var apiAuth, downloadAuth string
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadAuth = r.Header.Get("Authorization")
		_, _ = w.Write(githubTarball(t))
	}))
	defer download.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth = r.Header.Get("Authorization")
		// A different host, like GitHub's API redirecting to codeload.
		http.Redirect(w, r, download.URL+"/archive.tar.gz", http.StatusFound)
	}))
	defer api.Close()

	g := NewGitHub()
	g.APIBase = api.URL
	g.Token = "ghp_secret"
	rc, err := g.BuildContext(t.Context(), "https://github.com/owner/repo", sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readContext(t, rc); err != nil {
		t.Fatal(err)
	}
	if apiAuth != "Bearer ghp_secret" {
		t.Errorf("API request Authorization = %q, want the token", apiAuth)
	}
	if downloadAuth != "" {
		t.Errorf("download host received Authorization %q; the token must not follow the redirect", downloadAuth)
	}
}

func TestBuildContextNotFound(t *testing.T) {
	g := fakeGitHub(t, nil)
	other := "fedcba9876543210fedcba9876543210fedcba98" // not served

	if _, err := g.BuildContext(t.Context(), "https://github.com/owner/repo", other, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown commit: error = %v, want ErrNotFound", err)
	}
	if _, err := g.BuildContext(t.Context(), "https://github.com/owner/repo", "main", ""); err == nil {
		t.Error("a branch name instead of a SHA was accepted")
	}
}

func TestParseRepoURL(t *testing.T) {
	tests := []struct {
		in          string
		owner, repo string
		wantErr     bool
	}{
		{in: "https://github.com/sashasagebd/ops-dashboard", owner: "sashasagebd", repo: "ops-dashboard"},
		{in: "https://github.com/sashasagebd/ops-dashboard.git", owner: "sashasagebd", repo: "ops-dashboard"},
		{in: "https://github.com/sashasagebd/ops-dashboard/", owner: "sashasagebd", repo: "ops-dashboard"},
		{in: "https://github.com/a/b.c_d-e", owner: "a", repo: "b.c_d-e"},
		{in: "http://github.com/a/b", wantErr: true},
		{in: "https://gitlab.com/a/b", wantErr: true},
		{in: "https://github.com/a", wantErr: true},
		{in: "https://github.com/a/b/tree/main", wantErr: true},
		{in: "https://github.com/a/..", wantErr: true},
		{in: "https://github.com/a/b?x=1", wantErr: true},
		{in: "git@github.com:a/b.git", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			owner, repo, err := ParseRepoURL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseRepoURL(%q) = %q, %q; want error", tt.in, owner, repo)
				}
				return
			}
			if err != nil || owner != tt.owner || repo != tt.repo {
				t.Errorf("ParseRepoURL(%q) = %q, %q, %v; want %q, %q", tt.in, owner, repo, err, tt.owner, tt.repo)
			}
		})
	}
}

func keys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func TestLatestCommit(t *testing.T) {
	const head = "89abcdef0123456789abcdef0123456789abcdef"
	var gotPath, gotAccept, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAccept, gotAuth = r.URL.EscapedPath(), r.Header.Get("Accept"), r.Header.Get("Authorization")
		switch r.URL.EscapedPath() {
		case "/repos/o/r/commits/main", "/repos/o/r/commits/feature/x%3Fy":
			_, _ = io.WriteString(w, head)
		case "/repos/o/r/commits/weird":
			_, _ = io.WriteString(w, "<html>not a sha</html>")
		case "/repos/o/r/commits/broken":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	g := NewGitHub()
	g.APIBase = srv.URL
	g.Token = "tok"

	sha, err := g.LatestCommit(t.Context(), "https://github.com/o/r", "main")
	if err != nil || sha != head {
		t.Fatalf("LatestCommit(main) = %q, %v; want %s", sha, err, head)
	}
	if gotAccept != "application/vnd.github.sha" || gotAuth != "Bearer tok" {
		t.Errorf("Accept %q, Authorization %q", gotAccept, gotAuth)
	}
	// Slashes in a branch name are kept; anything else odd is escaped.
	if _, err := g.LatestCommit(t.Context(), "https://github.com/o/r", "feature/x?y"); err != nil {
		t.Errorf("branch with a slash: %v (path %s)", err, gotPath)
	}

	for branch, want := range map[string]error{"gone": ErrBranchNotFound, "": ErrBranchNotFound} {
		if _, err := g.LatestCommit(t.Context(), "https://github.com/o/r", branch); !errors.Is(err, want) {
			t.Errorf("LatestCommit(%q) error = %v, want %v", branch, err, want)
		}
	}
	for _, branch := range []string{"weird", "broken"} {
		if _, err := g.LatestCommit(t.Context(), "https://github.com/o/r", branch); err == nil || errors.Is(err, ErrBranchNotFound) {
			t.Errorf("LatestCommit(%q) error = %v, want a plain error", branch, err)
		}
	}
}
