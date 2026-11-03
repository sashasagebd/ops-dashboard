package engine

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeDaemon is a stand-in for Docker's API: tests register handlers by
// "METHOD /path" (without the /v1.xx version prefix the client adds), and it
// records every request it gets.
type fakeDaemon struct {
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	requests []recordedRequest
}

type recordedRequest struct {
	method, path, query string
	body                []byte
}

var versionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func newFakeDaemon(t *testing.T) (*fakeDaemon, *Client) {
	t.Helper()
	f := &fakeDaemon{handlers: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("Api-Version", "1.56")
			_, _ = w.Write([]byte("OK"))
			return
		}
		path := versionPrefix.ReplaceAllString(r.URL.Path, "")
		body, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{r.Method, path, r.URL.RawQuery, body})
		h, ok := f.handlers[r.Method+" "+path]
		f.mu.Unlock()

		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such thing in the fake daemon: ` + r.Method + " " + path + `"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	c, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return f, c
}

func (f *fakeDaemon) handle(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[route] = h
}

// json responds with status and a JSON body.
func respondJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// routes returns the "METHOD /path" of each request, in order.
func (f *fakeDaemon) routes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.method+" "+r.path)
	}
	return out
}

func (f *fakeDaemon) request(route string) (recordedRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.method+" "+r.path == route {
			return r, true
		}
	}
	return recordedRequest{}, false
}
