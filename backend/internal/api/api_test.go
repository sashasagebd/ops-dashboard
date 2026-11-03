package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// fakeStore keeps apps and env vars in memory; deployments come from the
// fake deployer.
type fakeStore struct {
	apps        []store.App
	env         map[string]string // "appID/KEY" -> value
	deployments []store.Deployment
	err         error // if set, every method fails with it
}

func (f *fakeStore) Apps(context.Context) ([]store.App, error) { return f.apps, f.err }

func (f *fakeStore) App(_ context.Context, id int64) (store.App, error) {
	if f.err != nil {
		return store.App{}, f.err
	}
	for _, a := range f.apps {
		if a.ID == id {
			return a, nil
		}
	}
	return store.App{}, store.ErrNotFound
}

func (f *fakeStore) CreateApp(_ context.Context, n store.NewApp) (store.App, error) {
	for _, a := range f.apps {
		if a.Name == n.Name {
			return store.App{}, fmt.Errorf("%w: an app with that name already exists", store.ErrConflict)
		}
	}
	a := store.App{ID: int64(len(f.apps) + 1), Name: n.Name, RepoURL: n.RepoURL, Hostname: n.Hostname,
		Port: n.Port, Branch: "main", HealthCheckPath: "/", MemoryLimitBytes: 256 << 20, CPULimitMillicores: 500}
	f.apps = append(f.apps, a)
	return a, nil
}

func (f *fakeStore) EnvVarKeys(_ context.Context, appID int64) ([]string, error) {
	var keys []string
	for k := range f.env {
		if id, key, _ := strings.Cut(k, "/"); id == fmt.Sprint(appID) {
			keys = append(keys, key)
		}
	}
	return keys, f.err
}

func (f *fakeStore) SetEnvVar(_ context.Context, appID int64, key, value string) error {
	if _, err := f.App(context.Background(), appID); err != nil {
		return err
	}
	f.env[fmt.Sprintf("%d/%s", appID, key)] = value
	return nil
}

func (f *fakeStore) DeleteEnvVar(_ context.Context, appID int64, key string) error {
	k := fmt.Sprintf("%d/%s", appID, key)
	if _, ok := f.env[k]; !ok {
		return store.ErrNotFound
	}
	delete(f.env, k)
	return nil
}

func (f *fakeStore) Deployments(context.Context, int64, int) ([]store.Deployment, error) {
	return f.deployments, f.err
}

func (f *fakeStore) CurrentDeployment(context.Context, int64) (store.Deployment, error) {
	for _, d := range f.deployments {
		if d.Status == store.StatusHealthy {
			return d, nil
		}
	}
	return store.Deployment{}, store.ErrNotFound
}

type fakeDeployer struct {
	err     error
	started []string
}

func (f *fakeDeployer) Start(_ context.Context, appID int64, commit string) (store.Deployment, error) {
	if f.err != nil {
		return store.Deployment{}, f.err
	}
	f.started = append(f.started, commit)
	return store.Deployment{ID: 9, AppID: appID, CommitSHA: commit, Status: store.StatusQueued}, nil
}

func newTestAPI() (http.Handler, *fakeStore, *fakeDeployer) {
	st := &fakeStore{env: map[string]string{}}
	st.apps = []store.App{{ID: 1, Name: "hello", RepoURL: "https://github.com/o/hello", Hostname: "hello.apps.test", Port: 8080}}
	dep := &fakeDeployer{}
	return New(st, dep, "apps.test"), st, dep
}

// do sends a request; a non-empty body is sent as JSON.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestCreateApp(t *testing.T) {
	h, _, _ := newTestAPI()

	rec := do(t, h, "POST", "/api/platform/apps", `{"name":"pokewordle-api","repoUrl":"https://github.com/o/pw","port":3000}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if got["hostname"] != "pokewordle-api.apps.test" {
		t.Errorf("hostname = %v, want <name>.<apps domain>", got["hostname"])
	}
	if got["currentDeployment"] != nil {
		t.Errorf("currentDeployment = %v, want null", got["currentDeployment"])
	}
}

func TestCreateAppRejects(t *testing.T) {
	tests := []struct {
		name, body  string
		contentType string
		want        int
	}{
		{"bad name", `{"name":"Hello","repoUrl":"https://github.com/o/r","port":80}`, "application/json", 400},
		{"bad port", `{"name":"x","repoUrl":"https://github.com/o/r","port":0}`, "application/json", 400},
		{"not github", `{"name":"x","repoUrl":"https://gitlab.com/o/r","port":80}`, "application/json", 400},
		{"bad health path", `{"name":"x","repoUrl":"https://github.com/o/r","port":80,"healthCheckPath":"healthz"}`, "application/json", 400},
		{"too much memory", `{"name":"x","repoUrl":"https://github.com/o/r","port":80,"memoryLimitBytes":17179869184}`, "application/json", 400},
		{"unknown field", `{"name":"x","repoUrl":"https://github.com/o/r","port":80,"privileged":true}`, "application/json", 400},
		{"name taken", `{"name":"hello","repoUrl":"https://github.com/o/r","port":80}`, "application/json", 409},
		// A cross-site form or text/plain fetch can't set JSON without a
		// preflight, so these are refused.
		{"form content type", `{"name":"x","repoUrl":"https://github.com/o/r","port":80}`, "application/x-www-form-urlencoded", 415},
		{"text/plain", `{"name":"x","repoUrl":"https://github.com/o/r","port":80}`, "text/plain", 415},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _ := newTestAPI()
			req := httptest.NewRequest("POST", "/api/platform/apps", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestGetAppDetail(t *testing.T) {
	h, st, _ := newTestAPI()

	// No env vars or deployments yet: lists are [], not null.
	rec := do(t, h, "GET", "/api/platform/apps/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{`"envKeys":[]`, `"deployments":[]`, `"currentDeployment":null`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %s lacks %s", rec.Body, want)
		}
	}

	st.deployments = []store.Deployment{
		{ID: 2, AppID: 1, CommitSHA: sha, Status: store.StatusFailed, Error: "build failed: x"},
		{ID: 1, AppID: 1, CommitSHA: sha, Status: store.StatusHealthy, ImageTag: "ops-platform/hello:" + sha},
	}
	got := decode[appDetailResponse](t, do(t, h, "GET", "/api/platform/apps/1", ""))
	if got.CurrentDeployment == nil || got.CurrentDeployment.ID != 1 {
		t.Errorf("currentDeployment = %+v, want deployment 1", got.CurrentDeployment)
	}
	if len(got.Deployments) != 2 || *got.Deployments[0].Error != "build failed: x" || got.Deployments[0].ImageTag != nil {
		t.Errorf("deployments = %+v", got.Deployments)
	}

	for _, path := range []string{"/api/platform/apps/99", "/api/platform/apps/abc", "/api/platform/apps/0"} {
		if rec := do(t, h, "GET", path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestEnvVarsAreWriteOnly(t *testing.T) {
	h, st, _ := newTestAPI()

	if rec := do(t, h, "PUT", "/api/platform/apps/1/env/TOKEN", `{"value":"s3cret"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT = %d, body %s", rec.Code, rec.Body)
	}
	if st.env["1/TOKEN"] != "s3cret" {
		t.Errorf("stored %v", st.env)
	}

	rec := do(t, h, "GET", "/api/platform/apps/1", "")
	if !strings.Contains(rec.Body.String(), `"envKeys":["TOKEN"]`) {
		t.Errorf("detail %s, want TOKEN listed", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Errorf("an env var value appears in the API response: %s", rec.Body)
	}

	if rec := do(t, h, "PUT", "/api/platform/apps/1/env/BAD-KEY", `{"value":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad key: %d, want 400", rec.Code)
	}
	if rec := do(t, h, "PUT", "/api/platform/apps/1/env/BIG", `{"value":"`+strings.Repeat("x", 33<<10)+`"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized value: %d, want 400", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/platform/apps/1/env/TOKEN", ""); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE = %d, want 204", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/platform/apps/1/env/TOKEN", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE again = %d, want 404", rec.Code)
	}
}

func TestCreateDeployment(t *testing.T) {
	h, _, dep := newTestAPI()

	rec := do(t, h, "POST", "/api/platform/apps/1/deployments", `{"commitSha":"`+sha+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if got := decode[deploymentResponse](t, rec); got.Status != "queued" || len(dep.started) != 1 {
		t.Errorf("response %+v, started %v", got, dep.started)
	}

	if rec := do(t, h, "POST", "/api/platform/apps/1/deployments", `{"commitSha":"main"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("branch name: %d, want 400", rec.Code)
	}
	dep.err = store.ErrDeployInProgress
	rec = do(t, h, "POST", "/api/platform/apps/1/deployments", `{"commitSha":"`+sha+`"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already in progress") {
		t.Errorf("in progress: %d %s, want 409 saying so", rec.Code, rec.Body)
	}
}

func TestInternalErrorsAreGeneric(t *testing.T) {
	h, st, _ := newTestAPI()
	st.err = errors.New(`pq: relation "apps" does not exist at 10.0.0.3:5432`)

	rec := do(t, h, "GET", "/api/platform/apps", "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.3") || strings.Contains(rec.Body.String(), "relation") {
		t.Errorf("internal detail leaked: %s", rec.Body)
	}
}

func TestListAppsEmptyIsArray(t *testing.T) {
	st := &fakeStore{env: map[string]string{}}
	rec := do(t, New(st, &fakeDeployer{}, "apps.test"), "GET", "/api/platform/apps", "")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %s, want []", rec.Body)
	}
}
