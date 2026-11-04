package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplyReplacesRoutes(t *testing.T) {
	var gotMethod, gotPath, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	err := c.Apply(t.Context(), []Route{
		{Host: "hello.apps.test", Upstream: "app-hello-7:8080"},
		{Host: "probe-8.platform.internal", Upstream: "app-hello-8:8080"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPatch || gotPath != serverPath || gotType != "application/json" {
		t.Errorf("request = %s %s (%s), want PATCH %s (application/json)", gotMethod, gotPath, gotType, serverPath)
	}
	// Checked as Caddy will read it, not against our own structs.
	var server struct {
		Listen         []string        `json:"listen"`
		Routes         json.RawMessage `json:"routes"`
		AutomaticHTTPS struct {
			Disable bool `json:"disable"`
		} `json:"automatic_https"`
	}
	if err := json.Unmarshal(gotBody, &server); err != nil {
		t.Fatalf("body %s: %v", gotBody, err)
	}
	if len(server.Listen) != 1 || server.Listen[0] != ":80" || !server.AutomaticHTTPS.Disable {
		t.Errorf("server = %s, want listen [:80] and automatic HTTPS off", gotBody)
	}
	var routes []struct {
		Match []struct {
			Host []string `json:"host"`
		} `json:"match"`
		Handle []struct {
			Handler   string `json:"handler"`
			Upstreams []struct {
				Dial string `json:"dial"`
			} `json:"upstreams"`
		} `json:"handle"`
		Terminal bool `json:"terminal"`
	}
	if err := json.Unmarshal(server.Routes, &routes); err != nil {
		t.Fatalf("routes %s: %v", server.Routes, err)
	}
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2: %s", len(routes), gotBody)
	}
	r := routes[0]
	if r.Match[0].Host[0] != "hello.apps.test" || r.Handle[0].Handler != "reverse_proxy" ||
		r.Handle[0].Upstreams[0].Dial != "app-hello-7:8080" || !r.Terminal {
		t.Errorf("first route = %+v", r)
	}
}

func TestApplyEmptyListIsAnEmptyArray(t *testing.T) {
	// Caddy needs [] to clear the routes; null would be rejected.
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	if err := New(srv.URL, "").Apply(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"routes":[]`) {
		t.Errorf("body = %q, want routes []", gotBody)
	}
}

func TestApplyReportsCaddyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"loading new config: bad dial address"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	if err := New(srv.URL, "").Apply(t.Context(), []Route{{Host: "x", Upstream: "y"}}); err == nil {
		t.Error("Apply succeeded although Caddy refused the config")
	}
}

func TestRoutesReadsWhatApplySent(t *testing.T) {
	// Round trip: whatever Apply sends, Routes reads back the same.
	var saved []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			saved, _ = io.ReadAll(r.Body)
			return
		}
		if r.URL.Path != serverPath {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Write(saved)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	want := []Route{{Host: "hello.apps.test", Upstream: "app-hello-7:8080"}, {Host: "api.apps.test", Upstream: "app-api-3:3000"}}
	if err := c.Apply(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	got, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Routes = %+v, want %+v", got, want)
	}
}

func TestRoutesOddShapesAndMissingServer(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   []Route
	}{
		{name: "server missing", status: http.StatusBadRequest, body: `{"error":"invalid traversal path"}`, want: []Route{}},
		{name: "server null", status: http.StatusOK, body: `null`, want: []Route{}},
		{name: "no routes key", status: http.StatusOK, body: `{"listen":[":80"]}`, want: []Route{}},
		{
			// Edited by hand: a file server, not a proxy. It must not look
			// like any route the platform wants.
			name:   "a route Apply never makes",
			status: http.StatusOK,
			body:   `{"routes":[{"match":[{"host":["hello.apps.test"]}],"handle":[{"handler":"file_server"}],"terminal":true}]}`,
			want:   []Route{{Host: "hello.apps.test"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			got, err := New(srv.URL, "").Routes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) || (len(got) > 0 && got[0] != tt.want[0]) {
				t.Errorf("Routes = %+v, want %+v", got, tt.want)
			}
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "").Routes(t.Context()); err == nil {
		t.Error("Routes succeeded although Caddy answered 500")
	}
}

func TestInFlight(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		// As Caddy answers, including an address listed twice.
		io.WriteString(w, `[{"address":"app-hello-6:8080","num_requests":2,"fails":0},
			{"address":"app-hello-7:8080","num_requests":0,"fails":0},
			{"address":"app-hello-6:8080","num_requests":1,"fails":0}]`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").InFlight(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotPath != "/reverse_proxy/upstreams" {
		t.Errorf("request = %s %s, want GET /reverse_proxy/upstreams", gotMethod, gotPath)
	}
	if got["app-hello-6:8080"] != 3 || got["app-hello-7:8080"] != 0 || len(got) != 2 {
		t.Errorf("in flight = %v, want app-hello-6:8080 3, app-hello-7:8080 0", got)
	}
}

func TestInFlightReportsCaddyErrors(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusInternalServerError) },
		"body":   func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "not json") },
	} {
		srv := httptest.NewServer(handler)
		if _, err := New(srv.URL, "").InFlight(t.Context()); err == nil {
			t.Errorf("%s: InFlight succeeded, want an error", name)
		}
		srv.Close()
	}
}

func TestProbeSendsHostAndDoesNotFollowRedirects(t *testing.T) {
	var gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()

	status, err := New("", srv.URL).Probe(t.Context(), "probe-8.platform.internal", "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if gotHost != "probe-8.platform.internal" || gotPath != "/healthz" {
		t.Errorf("probe went to Host %q path %q", gotHost, gotPath)
	}
	if status != http.StatusFound {
		t.Errorf("status = %d, want 302 (not followed)", status)
	}
}
