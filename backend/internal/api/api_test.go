package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/source"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// fakeStore keeps apps and env vars in memory; deployments come from the
// fake deployer.
type fakeStore struct {
	apps        []store.App
	env         map[string]string // "appID/KEY" -> value
	deployments []store.Deployment
	kept        []string // KeptImages' answer
	events      []store.Event
	databases   map[int64]store.Database
	err         error // if set, every method fails with it

	mu       sync.Mutex
	logs     []store.LogLine
	complete bool
	onPoll   func(n int) // called with the poll's number (from 1) as each log poll starts
	polls    int
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
		Port: n.Port, Branch: "main", HealthCheckPath: "/", MemoryLimitBytes: 256 << 20, CPULimitMillicores: 500,
		SourceDir: n.SourceDir}
	f.apps = append(f.apps, a)
	return a, nil
}

func (f *fakeStore) SetAutoDeploy(_ context.Context, id int64, on bool) (store.App, error) {
	for i, a := range f.apps {
		if a.ID == id {
			f.apps[i].AutoDeploy = on
			return f.apps[i], nil
		}
	}
	return store.App{}, store.ErrNotFound
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

func (f *fakeStore) KeptImages(context.Context, int64) ([]string, error) { return f.kept, f.err }

func (f *fakeStore) Database(_ context.Context, appID int64) (store.Database, error) {
	db, ok := f.databases[appID]
	if !ok {
		return store.Database{}, store.ErrNotFound
	}
	return db, nil
}

func (f *fakeStore) CreateDatabase(_ context.Context, appID int64) (store.Database, error) {
	if _, ok := f.databases[appID]; ok {
		return store.Database{}, fmt.Errorf("%w: the app already has a database", store.ErrConflict)
	}
	if f.databases == nil {
		f.databases = map[int64]store.Database{}
	}
	db := store.Database{AppID: appID, Password: "secret-pw", CreatedAt: time.Now()}
	f.databases[appID] = db
	return db, nil
}

func (f *fakeStore) Events(_ context.Context, appID int64, limit int) ([]store.Event, error) {
	var out []store.Event
	for _, e := range f.events {
		if (appID == 0 || e.AppID == appID) && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, f.err
}

func (f *fakeStore) Deployment(_ context.Context, id int64) (store.Deployment, error) {
	for _, d := range f.deployments {
		if d.ID == id {
			return d, nil
		}
	}
	return store.Deployment{}, store.ErrNotFound
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
	err        error
	started    []string
	latest     string // the branch head StartLatest finds; "" for none
	rolledBack []int64
}

func (f *fakeDeployer) Rollback(_ context.Context, appID, target int64) (store.Deployment, error) {
	if f.err != nil {
		return store.Deployment{}, f.err
	}
	f.rolledBack = append(f.rolledBack, target)
	return store.Deployment{ID: 9, AppID: appID, CommitSHA: sha, Status: store.StatusQueued, RollbackOf: target}, nil
}

func (f *fakeDeployer) StartLatest(ctx context.Context, appID int64) (store.Deployment, error) {
	if f.latest == "" {
		return store.Deployment{}, source.ErrBranchNotFound
	}
	return f.Start(ctx, appID, f.latest)
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
	return New(st, dep, &fakeEngine{}, "apps.test"), st, dep
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

	rec := do(t, h, "POST", "/api/platform/apps", `{"name":"pokewordle-api","repoUrl":"https://github.com/o/pw","port":3000,"sourceDir":"backend"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	got := decode[map[string]any](t, rec)
	if got["hostname"] != "pokewordle-api.apps.test" {
		t.Errorf("hostname = %v, want <name>.<apps domain>", got["hostname"])
	}
	if got["sourceDir"] != "backend" {
		t.Errorf("sourceDir = %v, want backend", got["sourceDir"])
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
		{"source dir with ..", `{"name":"x","repoUrl":"https://github.com/o/r","port":80,"sourceDir":"web/../.."}`, "application/json", 400},
		{"absolute source dir", `{"name":"x","repoUrl":"https://github.com/o/r","port":80,"sourceDir":"/web"}`, "application/json", 400},
		{"source dir trailing slash", `{"name":"x","repoUrl":"https://github.com/o/r","port":80,"sourceDir":"web/"}`, "application/json", 400},
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
	rec := do(t, New(st, &fakeDeployer{}, &fakeEngine{}, "apps.test"), "GET", "/api/platform/apps", "")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %s, want []", rec.Body)
	}
}

func TestAutoDeploySwitch(t *testing.T) {
	h, st, _ := newTestAPI()
	st.apps[0].AutoDeploy = true
	st.apps[0].NextCommitSHA = sha

	got := decode[appDetailResponse](t, do(t, h, "GET", "/api/platform/apps/1", ""))
	if !got.AutoDeploy || got.NextCommitSHA == nil || *got.NextCommitSHA != sha {
		t.Errorf("app = autoDeploy %v, next %v; want on, the waiting commit", got.AutoDeploy, got.NextCommitSHA)
	}

	rec := do(t, h, "PATCH", "/api/platform/apps/1", `{"autoDeploy":false}`)
	if rec.Code != http.StatusOK || decode[appResponse](t, rec).AutoDeploy || st.apps[0].AutoDeploy {
		t.Errorf("turning it off: %d %s", rec.Code, rec.Body)
	}
	for body, want := range map[string]int{
		`{}`:                   http.StatusBadRequest, // nothing to change
		`{"autoDeploy":"yes"}`: http.StatusBadRequest,
		`{"name":"renamed"}`:   http.StatusBadRequest, // not changeable
	} {
		if rec := do(t, h, "PATCH", "/api/platform/apps/1", body); rec.Code != want {
			t.Errorf("PATCH %s: status %d, want %d", body, rec.Code, want)
		}
	}
	if rec := do(t, h, "PATCH", "/api/platform/apps/9", `{"autoDeploy":true}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing app: status %d, want 404", rec.Code)
	}
}

func TestDeployLatest(t *testing.T) {
	h, _, dep := newTestAPI()
	dep.latest = sha
	rec := do(t, h, "POST", "/api/platform/apps/1/deployments/latest", "")
	if rec.Code != http.StatusAccepted || decode[deploymentResponse](t, rec).CommitSHA != sha {
		t.Errorf("deploy latest: %d %s; want 202 with the branch head", rec.Code, rec.Body)
	}

	dep.latest = ""
	rec = do(t, h, "POST", "/api/platform/apps/1/deployments/latest", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "branch wasn't found") {
		t.Errorf("unknown branch: %d %s; want 400 saying so", rec.Code, rec.Body)
	}
}

func TestRollback(t *testing.T) {
	h, st, dep := newTestAPI()
	img := func(n string) string { return "ops-platform/hello:" + n }
	st.deployments = []store.Deployment{
		{ID: 5, AppID: 1, CommitSHA: sha, Status: store.StatusHealthy, ImageTag: img("c"), RollbackOf: 3},
		{ID: 4, AppID: 1, CommitSHA: sha, Status: store.StatusRolledBack, ImageTag: img("b")},
		{ID: 3, AppID: 1, CommitSHA: sha, Status: store.StatusSuperseded, ImageTag: img("c")}, // same image as live
		{ID: 2, AppID: 1, CommitSHA: sha, Status: store.StatusFailed, ImageTag: img("x")},
		{ID: 1, AppID: 1, CommitSHA: sha, Status: store.StatusSuperseded, ImageTag: img("a")}, // image no longer kept
	}
	st.kept = []string{img("c"), img("b")}

	// Each deployment says whether it can be rolled back to, in both lists.
	got := decode[appDetailResponse](t, do(t, h, "GET", "/api/platform/apps/1", ""))
	list := decode[[]deploymentResponse](t, do(t, h, "GET", "/api/platform/apps/1/deployments", ""))
	for name, ds := range map[string][]deploymentResponse{"app detail": got.Deployments, "deployment list": list} {
		var rollbackable []int64
		for _, d := range ds {
			if d.Rollbackable {
				rollbackable = append(rollbackable, d.ID)
			}
		}
		if len(rollbackable) != 1 || rollbackable[0] != 4 {
			t.Errorf("%s: rollbackable %v, want only 4", name, rollbackable)
		}
		if ds[0].RollbackOf == nil || *ds[0].RollbackOf != 3 || ds[1].RollbackOf != nil {
			t.Errorf("%s: rollbackOf = %v, %v; want 3 on the rollback, null otherwise", name, ds[0].RollbackOf, ds[1].RollbackOf)
		}
	}

	rec := do(t, h, "POST", "/api/platform/apps/1/deployments/4/rollback", "")
	if rec.Code != http.StatusAccepted || len(dep.rolledBack) != 1 || dep.rolledBack[0] != 4 {
		t.Fatalf("rollback: %d %s, rolled back %v; want 202, to 4", rec.Code, rec.Body, dep.rolledBack)
	}
	if r := decode[deploymentResponse](t, rec); r.RollbackOf == nil || *r.RollbackOf != 4 || r.Status != "queued" {
		t.Errorf("response %+v, want a queued rollback of 4", r)
	}

	for _, tt := range []struct {
		err      error
		path     string
		wantCode int
		wantBody string
	}{
		{fmt.Errorf("%w: its image is no longer kept", store.ErrNotRollbackable), "/api/platform/apps/1/deployments/1/rollback", http.StatusConflict, "no longer kept"},
		{store.ErrDeployInProgress, "/api/platform/apps/1/deployments/4/rollback", http.StatusConflict, "already in progress"},
		{store.ErrNotFound, "/api/platform/apps/1/deployments/77/rollback", http.StatusNotFound, "not found"},
		{nil, "/api/platform/apps/1/deployments/abc/rollback", http.StatusNotFound, "not found"},
	} {
		dep.err = tt.err
		rec := do(t, h, "POST", tt.path, "")
		if rec.Code != tt.wantCode || !strings.Contains(rec.Body.String(), tt.wantBody) {
			t.Errorf("POST %s with %v: %d %s, want %d mentioning %q", tt.path, tt.err, rec.Code, rec.Body, tt.wantCode, tt.wantBody)
		}
	}
}

func TestEvents(t *testing.T) {
	h, st, _ := newTestAPI()

	// None yet: [] in both places.
	if rec := do(t, h, "GET", "/api/platform/events", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("no events: body %s, want []", rec.Body)
	}
	if rec := do(t, h, "GET", "/api/platform/apps/1", ""); !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Errorf("app detail %s lacks events: []", rec.Body)
	}

	st.events = []store.Event{
		{ID: 3, AppID: 1, Kind: store.EventRestarted, Message: "Restarted hello", Output: []string{"panic: nil map"}},
		{ID: 2, Kind: store.EventRoutesReapplied, Message: "re-applied routes"}, // about no app
		{ID: 1, AppID: 2, Kind: store.EventRemovedOrphan, Message: "removed app-api-3"},
	}
	all := decode[[]eventResponse](t, do(t, h, "GET", "/api/platform/events", ""))
	if len(all) != 3 || all[0].Kind != "restarted" || all[0].Output[0] != "panic: nil map" ||
		all[1].AppID != nil || all[1].Output == nil || *all[2].AppID != 2 {
		t.Errorf("all events = %+v", all)
	}
	// An app's detail has only its own.
	got := decode[appDetailResponse](t, do(t, h, "GET", "/api/platform/apps/1", ""))
	if len(got.Events) != 1 || got.Events[0].ID != 3 {
		t.Errorf("hello's events = %+v, want only its restart", got.Events)
	}
}

func TestDatabase(t *testing.T) {
	st := &fakeStore{env: map[string]string{}}
	st.apps = []store.App{{ID: 1, Name: "hello", Hostname: "hello.apps.test", Port: 8080}}
	eng := &fakeEngine{}
	h := New(st, &fakeDeployer{}, eng, "apps.test")

	if rec := do(t, h, "GET", "/api/platform/apps/1", ""); !strings.Contains(rec.Body.String(), `"database":null`) {
		t.Errorf("no database yet: body %s, want database null", rec.Body)
	}

	rec := do(t, h, "POST", "/api/platform/apps/1/database", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	got := decode[databaseResponse](t, rec)
	// Not created by the reconciler yet.
	if got.Name != "ops-db-hello" || got.State != "creating" || got.CreatedAt.IsZero() {
		t.Errorf("created = %+v", got)
	}
	// The password never comes back, here or in the app's detail.
	for _, path := range []string{"/api/platform/apps/1"} {
		if body := do(t, h, "GET", path, "").Body.String(); strings.Contains(body, "secret-pw") {
			t.Errorf("GET %s shows the database password: %s", path, body)
		}
	}
	if strings.Contains(rec.Body.String(), "secret-pw") {
		t.Errorf("create response shows the password: %s", rec.Body)
	}

	// Once created, its state is Docker's; gone is "missing".
	now := time.Now()
	db := st.databases[1]
	db.ProvisionedAt = &now
	st.databases[1] = db
	eng.containers = []engine.ManagedContainer{
		{ID: "a", Name: "app-hello-7", State: "running", Labels: map[string]string{engine.LabelDeployment: "7"}},
		{ID: "d", Name: "ops-db-hello", State: "exited", Labels: map[string]string{engine.LabelRole: engine.RoleDatabase}},
	}
	if d := decode[appDetailResponse](t, do(t, h, "GET", "/api/platform/apps/1", "")).Database; d == nil || d.State != "exited" {
		t.Errorf("database = %+v, want exited", d)
	}
	eng.containers = eng.containers[:1]
	if d := decode[appDetailResponse](t, do(t, h, "GET", "/api/platform/apps/1", "")).Database; d == nil || d.State != "missing" {
		t.Errorf("database = %+v, want missing", d)
	}

	// One per app; unknown apps are 404.
	if rec := do(t, h, "POST", "/api/platform/apps/1/database", ""); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "already has a database") {
		t.Errorf("second: %d %s, want 409 saying so", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/platform/apps/9/database", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing app: %d, want 404", rec.Code)
	}
}
