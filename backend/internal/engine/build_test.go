package engine

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestBuildSendsContextToLegacyBuilder(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /build", respondJSON(http.StatusOK,
		`{"stream":"Step 1/2 : FROM busybox"}`+"\n"+
			`{"aux":{"ID":"sha256:abc"}}`+"\n"+
			`{"stream":"Successfully tagged hello:abc123"}`+"\n"))

	err := c.Build(t.Context(), strings.NewReader("tar bytes"), "hello:abc123", map[string]string{"ops.platform.app": "hello"}, func(string) {})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	req, ok := f.request("POST /build")
	if !ok {
		t.Fatalf("no build request; got %v", f.routes())
	}
	q, _ := url.ParseQuery(req.query)
	if q.Get("version") != "1" {
		t.Errorf("builder version = %q, want 1 (the legacy builder: BuildKit needs a session)", q.Get("version"))
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
		`{"stream":"Step 2/3 : RUN npm ci"}`+"\n"+
			`{"errorDetail":{"code":1,"message":"The command '/bin/sh -c npm ci' returned a non-zero code: 1"},"error":"The command ... returned a non-zero code: 1"}`+"\n"+
			`{"errorDetail":{"message":"a later error"}}`+"\n"))

	err := c.Build(t.Context(), strings.NewReader(""), "hello:abc", nil, func(string) {})
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

	err := c.Build(t.Context(), strings.NewReader(""), "hello:abc", nil, func(string) {})
	var buildErr *BuildError
	if err == nil || errors.As(err, &buildErr) {
		t.Errorf("Build error = %v, want a plain (non-build) error", err)
	}
}

func TestReadBuildOutputRejectsGarbage(t *testing.T) {
	if err := readBuildOutput(strings.NewReader(`{"stream":"ok"}`+"\nnot json"), func(string) {}); err == nil {
		t.Error("readBuildOutput accepted a corrupt stream")
	}
}

func TestBuildProgress(t *testing.T) {
	// Shaped like the legacy builder's real output: stream text in chunks
	// that don't line up with lines, pull progress per layer (with an id),
	// and an overall pull status (without).
	output := strings.Join([]string{
		`{"stream":"Step 1/3 : FROM golang:1.25\n"}`,
		`{"status":"Pulling from library/golang","id":"1.25"}`,
		`{"status":"Downloading","progressDetail":{"current":1,"total":9},"id":"a1b2c3"}`,
		`{"status":"Status: Downloaded newer image for golang:1.25"}`,
		`{"stream":" ---> 9f8e7d\n"}`,
		`{"stream":"Step 2/3 : RUN npm ci\n"}`,
		`{"stream":"\u001b[32madded\u001b[0m 12 pack"}`,
		`{"stream":"ages\n"}`,
		`{"stream":"fetching 10%\rfetching 100%\n"}`,
		`{"stream":"no newline at the end"}`,
	}, "\n")
	var got []string
	if err := readBuildOutput(strings.NewReader(output), func(l string) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Step 1/3 : FROM golang:1.25",
		"Status: Downloaded newer image for golang:1.25",
		" ---> 9f8e7d",
		"Step 2/3 : RUN npm ci",
		"added 12 packages", // joined across chunks, colours gone
		"fetching 100%",     // what a terminal would show after the \r
		"no newline at the end",
	}
	if !slices.Equal(got, want) {
		t.Errorf("progress lines:\n  got  %q\n  want %q", got, want)
	}
}

func TestCleanLine(t *testing.T) {
	long := strings.Repeat("é", MaxLineBytes) // 2 bytes each: the cut lands mid-rune
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"windows\r", "windows"},
		{"nul\x00byte", "nulbyte"},
		{"bad \xff utf8", "bad � utf8"},
		{"\x1b[1;31merror\x1b[0m: \x1b[2Kgone", "error: gone"},
		{long, strings.Repeat("é", MaxLineBytes/2) + " …(line cut)"},
	}
	for _, tt := range tests {
		if got := cleanLine(tt.in); got != tt.want {
			t.Errorf("cleanLine(%.20q…) = %.40q…, want %.40q…", tt.in, got, tt.want)
		}
	}
}
