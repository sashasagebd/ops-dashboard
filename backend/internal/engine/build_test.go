package engine

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestBuildSendsContextToBuildKit(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /build", respondJSON(http.StatusOK,
		`{"id":"moby.buildkit.trace","aux":"CgQ="}`+"\n"+
			`{"aux":{"ID":"sha256:abc"}}`+"\n"))

	err := c.Build(t.Context(), strings.NewReader("tar bytes"), "hello:abc123", map[string]string{"ops.platform.app": "hello"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	req, ok := f.request("POST /build")
	if !ok {
		t.Fatalf("no build request; got %v", f.routes())
	}
	q, _ := url.ParseQuery(req.query)
	if q.Get("version") != "2" {
		t.Errorf("builder version = %q, want 2 (BuildKit)", q.Get("version"))
	}
	if q.Get("t") != "hello:abc123" {
		t.Errorf("tag = %q, want hello:abc123", q.Get("t"))
	}
	if !strings.Contains(q.Get("labels"), `"ops.platform.app":"hello"`) {
		t.Errorf("labels = %q, want the app label", q.Get("labels"))
	}
	if string(req.body) != "tar bytes" {
		t.Errorf("build context sent = %q, want the reader's bytes", req.body)
	}
}

func TestBuildReportsFailureFromStream(t *testing.T) {
	// A failed build still answers 200; the failure is in the stream, and
	// output may continue after it.
	f, c := newFakeDaemon(t)
	f.handle("POST /build", respondJSON(http.StatusOK,
		`{"id":"moby.buildkit.trace","aux":"CgQ="}`+"\n"+
			`{"errorDetail":{"message":"process \"/bin/sh -c npm ci\" did not complete successfully: exit code: 1"},"error":"process ... exit code: 1"}`+"\n"+
			`{"errorDetail":{"message":"a later error"}}`+"\n"))

	err := c.Build(t.Context(), strings.NewReader(""), "hello:abc", nil)
	var buildErr *BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build error = %v, want a *BuildError", err)
	}
	if !strings.Contains(buildErr.Message, "npm ci") {
		t.Errorf("message = %q, want the first error (from errorDetail)", buildErr.Message)
	}
}

func TestBuildDockerUnreachableIsNotABuildError(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /build", respondJSON(http.StatusInternalServerError, `{"message":"daemon broke"}`))

	err := c.Build(t.Context(), strings.NewReader(""), "hello:abc", nil)
	var buildErr *BuildError
	if err == nil || errors.As(err, &buildErr) {
		t.Errorf("Build error = %v, want a plain (non-build) error", err)
	}
}

func TestReadBuildOutputRejectsGarbage(t *testing.T) {
	if err := readBuildOutput(strings.NewReader(`{"stream":"ok"}` + "\nnot json")); err == nil {
		t.Error("readBuildOutput accepted a corrupt stream")
	}
}
