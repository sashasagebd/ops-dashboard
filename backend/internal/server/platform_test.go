package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
