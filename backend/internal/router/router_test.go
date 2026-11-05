package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// sentRoute and sentServer decode an Apply body as Caddy will read it, not
// through our own structs.
type sentRoute struct {
	Match []struct {
		Host []string `json:"host"`
	} `json:"match"`
	Handle []struct {
		Handler   string `json:"handler"`
		Upstreams []struct {
			Dial string `json:"dial"`
		} `json:"upstreams"`
		StatusCode int `json:"status_code"`
	} `json:"handle"`
	Terminal bool `json:"terminal"`
}

type sentServer struct {
	Listen         []string    `json:"listen"`
	Routes         []sentRoute `json:"routes"`
	AutomaticHTTPS struct {
		Disable bool `json:"disable"`
	} `json:"automatic_https"`
}

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
		{Host: "hello.example.com", Upstream: "app-hello-7:8080", Public: true},
		{Host: "probe-8.platform.internal", Upstream: "app-hello-8:8080"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPatch || gotPath != serversPath || gotType != "application/json" {
		t.Errorf("request = %s %s (%s), want PATCH %s (application/json)", gotMethod, gotPath, gotType, serversPath)
	}
	var servers map[string]sentServer
	if err := json.Unmarshal(gotBody, &servers); err != nil {
		t.Fatalf("body %s: %v", gotBody, err)
	}
	if len(servers) != 2 {
		t.Fatalf("servers = %s, want apps and public only", gotBody)
	}
	tailnet, public := servers["apps"], servers["public"]
	if len(tailnet.Listen) != 1 || tailnet.Listen[0] != ":80" || !tailnet.AutomaticHTTPS.Disable {
		t.Errorf("apps server = %+v, want listen [:80] and automatic HTTPS off", tailnet)
	}
	if len(public.Listen) != 1 || public.Listen[0] != ":8080" || !public.AutomaticHTTPS.Disable {
		t.Errorf("public server = %+v, want listen [:8080] and automatic HTTPS off", public)
	}
	if len(tailnet.Routes) != 2 {
		t.Fatalf("apps server has %d routes, want 2: %s", len(tailnet.Routes), gotBody)
	}
	r := tailnet.Routes[0]
	if r.Match[0].Host[0] != "hello.apps.test" || r.Handle[0].Handler != "reverse_proxy" ||
		r.Handle[0].Upstreams[0].Dial != "app-hello-7:8080" || !r.Terminal {
		t.Errorf("first route = %+v", r)
	}
	// The public server: the public route, then a 404 for anything else.
	if len(public.Routes) != 2 {
		t.Fatalf("public server has %d routes, want 2: %s", len(public.Routes), gotBody)
	}
	if r := public.Routes[0]; r.Match[0].Host[0] != "hello.example.com" || r.Handle[0].Upstreams[0].Dial != "app-hello-7:8080" {
		t.Errorf("public route = %+v", r)
	}
	if r := public.Routes[1]; len(r.Match) != 0 || r.Handle[0].Handler != "static_response" || r.Handle[0].StatusCode != 404 || !r.Terminal {
		t.Errorf("last public route = %+v, want a 404 for every request", r)
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
		if r.URL.Path != serversPath {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Write(saved)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	for _, want := range [][]Route{
		{
			{Host: "hello.apps.test", Upstream: "app-hello-7:8080"},
			{Host: "api.apps.test", Upstream: "app-api-3:3000"},
			{Host: "api.example.com", Upstream: "app-api-3:3000", Public: true},
		},
		{}, // nothing routed: both servers still there, the 404 too
	} {
		if err := c.Apply(t.Context(), want); err != nil {
			t.Fatal(err)
		}
		got, err := c.Routes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("Routes = %+v, want %+v", got, want)
		}
	}
}

func TestTunnelGuard(t *testing.T) {
	var saved []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			saved, _ = io.ReadAll(r.Body)
			return
		}
		w.Write(saved)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.TunnelSubnet = "10.203.0.8/29"
	want := []Route{{Host: "hello.apps.test", Upstream: "app-hello-7:8080"}}
	if err := c.Apply(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	var servers map[string]struct {
		Routes []struct {
			Match []struct {
				Host     []string `json:"host"`
				RemoteIP struct {
					Ranges []string `json:"ranges"`
				} `json:"remote_ip"`
			} `json:"match"`
			Handle []struct {
				Handler    string `json:"handler"`
				StatusCode int    `json:"status_code"`
			} `json:"handle"`
			Terminal bool `json:"terminal"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(saved, &servers); err != nil {
		t.Fatal(err)
	}
	// First on the tailnet server, before any host: 403 for the tunnel.
	guard := servers["apps"].Routes[0]
	if len(guard.Match) != 1 || len(guard.Match[0].Host) != 0 || !slices.Equal(guard.Match[0].RemoteIP.Ranges, []string{"10.203.0.8/29"}) ||
		guard.Handle[0].Handler != "static_response" || guard.Handle[0].StatusCode != 403 || !guard.Terminal {
		t.Errorf("first tailnet route = %+v, want a 403 for the tunnel's subnet", guard)
	}
	// Not on the public server: that's where the tunnel belongs.
	if strings.Contains(string(saved), `"public":{"listen":[":8080"],"routes":[{"match":[{"remote_ip"`) {
		t.Error("the public server refuses the tunnel too")
	}
	// Read back without the guard; a table without it gets applied again.
	if got, _ := c.Routes(t.Context()); !slices.Equal(got, want) {
		t.Errorf("Routes = %+v, want %+v", got, want)
	}
	c.TunnelSubnet = "10.203.0.16/29" // a guard for another subnet isn't this one
	if got, _ := c.Routes(t.Context()); len(got) == 0 || got[0] != (Route{}) {
		t.Errorf("Routes = %+v, want a route nothing wants first", got)
	}
}

func TestRoutesOddShapesAndMissingServers(t *testing.T) {
	// What Apply sends for no routes, to build the odd cases from.
	const tailnetEmpty = `"apps":{"listen":[":80"],"routes":[]}`
	const publicEmpty = `"public":{"listen":[":8080"],"routes":[{"handle":[{"handler":"static_response","status_code":404}],"terminal":true}]}`
	tests := []struct {
		name   string
		status int
		body   string
		want   []Route
	}{
		// Missing servers come back as a route nothing wants, so the
		// table is applied again.
		{name: "no servers", status: http.StatusBadRequest, body: `{"error":"invalid traversal path"}`,
			want: []Route{{}, {Public: true}}},
		{name: "servers null", status: http.StatusOK, body: `null`, want: []Route{{}, {Public: true}}},
		{name: "public missing", status: http.StatusOK, body: `{` + tailnetEmpty + `}`, want: []Route{{Public: true}}},
		{name: "public without its 404", status: http.StatusOK,
			body: `{` + tailnetEmpty + `,"public":{"listen":[":8080"],"routes":[]}}`, want: []Route{{Public: true}}},
		{name: "no routes key", status: http.StatusOK, body: `{"apps":{"listen":[":80"]},` + publicEmpty + `}`, want: []Route{}},
		{
			// Edited by hand: a file server, not a proxy. It must not look
			// like any route the platform wants.
			name:   "a route Apply never makes",
			status: http.StatusOK,
			body:   `{"apps":{"routes":[{"match":[{"host":["hello.apps.test"]}],"handle":[{"handler":"file_server"}],"terminal":true}]},` + publicEmpty + `}`,
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
			if !slices.Equal(got, tt.want) {
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

func TestSyncMetrics(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantChanged bool
	}{
		{name: "never set", status: http.StatusOK, body: "null", wantChanged: true},
		{name: "path missing", status: http.StatusBadRequest, body: `{"error":"invalid traversal path"}`, wantChanged: true},
		{name: "on but not per host", status: http.StatusOK, body: `{}`, wantChanged: true},
		{name: "already per host", status: http.StatusOK, body: `{"per_host":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posted string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/config/apps/http/metrics" {
					http.Error(w, "wrong path", http.StatusNotFound)
					return
				}
				if r.Method == http.MethodPost {
					b, _ := io.ReadAll(r.Body)
					posted = string(b)
					return
				}
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			changed, err := New(srv.URL, "").SyncMetrics(t.Context())
			if err != nil || changed != tt.wantChanged {
				t.Fatalf("SyncMetrics = %v, %v; want changed %v", changed, err, tt.wantChanged)
			}
			if tt.wantChanged && posted != `{"per_host":true}` {
				t.Errorf("posted %q, want per_host on", posted)
			}
			if !tt.wantChanged && posted != "" {
				t.Errorf("posted %q although it was already on", posted)
			}
		})
	}
}

func TestMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		io.WriteString(w, "# HELP x\nx 1\n")
	}))
	defer srv.Close()
	got, err := New(srv.URL, "").Metrics(t.Context())
	if err != nil || string(got) != "# HELP x\nx 1\n" {
		t.Errorf("Metrics = %q, %v", got, err)
	}
}
