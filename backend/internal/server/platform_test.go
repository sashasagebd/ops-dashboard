package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// fakePlatform records the last request it received and answers 201.
type fakePlatform struct {
	calls                      int
	method, path, query, ctype string
	body                       string
	forwardedHost              string
}

func newFakePlatform(t *testing.T) (*fakePlatform, *url.URL) {
	t.Helper()
	f := &fakePlatform{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.method, f.path, f.query, f.ctype = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		f.body = string(b)
		f.forwardedHost = r.Header.Get("X-Forwarded-Host")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f, u
}

func TestPlatformRequestsAreForwarded(t *testing.T) {
	f, target := newFakePlatform(t)
	h := New(&fakeSource{}, nil, NewPlatformProxy(target), nil)

	req := httptest.NewRequest(http.MethodPost, "/api/platform/apps/3/deployments?x=1", strings.NewReader(`{"commitSha":"abc"}`))
	req.Header.Set("Content-Type", "application/json")
	// What a browser sends for a request from the dashboard's own page.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Forwarded-Host", "spoofed.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || strings.TrimSpace(rec.Body.String()) != `{"id":1}` {
		t.Errorf("response = %d %s, want the platform's 201", rec.Code, rec.Body)
	}
	if f.method != http.MethodPost || f.path != "/api/platform/apps/3/deployments" || f.query != "x=1" ||
		f.ctype != "application/json" || f.body != `{"commitSha":"abc"}` {
		t.Errorf("platform got %s %s?%s (%s) %q", f.method, f.path, f.query, f.ctype, f.body)
	}
	if f.forwardedHost == "spoofed.example" {
		t.Error("a client's X-Forwarded-Host reached the platform")
	}
}

// As production wires it: the platform and the built frontend together.
// (Registering both once panicked at startup: overlapping ServeMux patterns.)
func TestPlatformAlongsideStaticFiles(t *testing.T) {
	f, target := newFakePlatform(t)
	static := fstest.MapFS{"index.html": {Data: []byte("<h1>dashboard</h1>")}}
	h := New(&fakeSource{}, nil, NewPlatformProxy(target), static)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/platform/apps", nil))
		if rec.Code != http.StatusCreated || f.method != method {
			t.Errorf("%s: status %d, platform saw %q; want it forwarded", method, rec.Code, f.method)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "dashboard") {
		t.Errorf("GET / = %d %q, want the frontend", rec.Code, rec.Body)
	}
}

func TestCrossOriginWritesAreRefused(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		headers       map[string]string
		wantForwarded bool
	}{
		{"same-origin POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"cross-site POST", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"same-site POST from another subdomain", http.MethodPut, map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		// Older browsers without Sec-Fetch-Site: Origin is compared to Host.
		{"foreign Origin", http.MethodDelete, map[string]string{"Origin": "https://evil.example"}, false},
		// Not a browser, so not CSRF: curl and scripts work.
		{"no browser headers", http.MethodPost, nil, true},
		// Reads are never blocked: a cross-site page can't read the
		// response anyway (no CORS headers).
		{"cross-site GET", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, target := newFakePlatform(t)
			h := New(&fakeSource{}, nil, NewPlatformProxy(target), nil)
			req := httptest.NewRequest(tt.method, "/api/platform/apps", strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if forwarded := f.calls > 0; forwarded != tt.wantForwarded {
				t.Errorf("forwarded = %v, want %v (status %d)", forwarded, tt.wantForwarded, rec.Code)
			}
			if !tt.wantForwarded && rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestPlatformUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(srv.URL)
	srv.Close() // nothing listens there now

	rec := httptest.NewRecorder()
	New(&fakeSource{}, nil, NewPlatformProxy(target), nil).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/platform/apps", nil))

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	// Generic: the error (with the platform's address) goes to the log only.
	if rec.Code != http.StatusBadGateway || body["error"] != "the platform isn't reachable" {
		t.Errorf("response = %d %v", rec.Code, body)
	}
}

func TestPlatformLogStreamsPassThroughLive(t *testing.T) {
	// A platform log stream that sends one event, then waits: the browser
	// must get that event while the stream is still open, not when it ends.
	release := make(chan struct{})
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\ndata: {\"line\":\"Building\"}\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer platform.Close()
	defer close(release)
	target, _ := url.Parse(platform.URL)
	dashboard := httptest.NewServer(New(&fakeSource{}, nil, NewPlatformProxy(target), nil))
	defer dashboard.Close()

	res, err := http.Get(dashboard.URL + "/api/platform/apps/1/deployments/7/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := io.ReadAtLeast(res.Body, buf, len("id: 1\n"))
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if !strings.HasPrefix(s, "id: 1\n") {
			t.Errorf("first bytes = %q, want the event", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event didn't come through while the stream was open: the proxy is buffering")
	}
}
