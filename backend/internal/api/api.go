// Package api is the platform's HTTP API: registering apps, setting their env
// vars and deploying them. The browser reaches it through the dashboard,
// which forwards /api/platform/* here, so the paths keep that prefix.
//
// There's no login: like the dashboard, it's only reachable over Tailscale.
// Two things still matter without one. Env var values are write-only (no
// endpoint returns them). And every request with a body must be JSON, so a
// web page on another site can't send one from your browser without a CORS
// preflight, which this API never approves.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/source"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Store is what the API needs from the database; in production it's a
// *store.Store.
type Store interface {
	Apps(ctx context.Context) ([]store.App, error)
	App(ctx context.Context, id int64) (store.App, error)
	CreateApp(ctx context.Context, a store.NewApp) (store.App, error)
	EnvVarKeys(ctx context.Context, appID int64) ([]string, error)
	SetEnvVar(ctx context.Context, appID int64, key, value string) error
	DeleteEnvVar(ctx context.Context, appID int64, key string) error
	Deployments(ctx context.Context, appID int64, limit int) ([]store.Deployment, error)
	CurrentDeployment(ctx context.Context, appID int64) (store.Deployment, error)
}

// Deployer starts deploys; in production it's a *deploy.Deployer.
type Deployer interface {
	Start(ctx context.Context, appID int64, commitSHA string) (store.Deployment, error)
}

// New returns the platform API's handler. Apps get the hostname
// <name>.<appsDomain>.
func New(st Store, dep Deployer, appsDomain string) http.Handler {
	a := &api{store: st, deployer: dep, appsDomain: appsDomain}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/platform/apps", a.listApps)
	mux.HandleFunc("POST /api/platform/apps", a.createApp)
	mux.HandleFunc("GET /api/platform/apps/{id}", a.getApp)
	mux.HandleFunc("PUT /api/platform/apps/{id}/env/{key}", a.setEnvVar)
	mux.HandleFunc("DELETE /api/platform/apps/{id}/env/{key}", a.deleteEnvVar)
	mux.HandleFunc("GET /api/platform/apps/{id}/deployments", a.listDeployments)
	mux.HandleFunc("POST /api/platform/apps/{id}/deployments", a.createDeployment)
	return mux
}

type api struct {
	store      Store
	deployer   Deployer
	appsDomain string
}

// Response types: the API's contract, separate from the store's types so it
// doesn't change by accident when they do. Mirrored in frontend/src/api.ts.

type appResponse struct {
	ID                 int64               `json:"id"`
	Name               string              `json:"name"`
	RepoURL            string              `json:"repoUrl"`
	Branch             string              `json:"branch"`
	Hostname           string              `json:"hostname"`
	Port               int                 `json:"port"`
	HealthCheckPath    string              `json:"healthCheckPath"`
	MemoryLimitBytes   int64               `json:"memoryLimitBytes"`
	CPULimitMillicores int                 `json:"cpuLimitMillicores"`
	CreatedAt          time.Time           `json:"createdAt"`
	CurrentDeployment  *deploymentResponse `json:"currentDeployment"` // null if nothing is live
}

type appDetailResponse struct {
	appResponse
	EnvKeys     []string             `json:"envKeys"`     // names only; values are write-only
	Deployments []deploymentResponse `json:"deployments"` // newest first
}

type deploymentResponse struct {
	ID         int64      `json:"id"`
	AppID      int64      `json:"appId"`
	CommitSHA  string     `json:"commitSha"`
	Status     string     `json:"status"`
	ImageTag   *string    `json:"imageTag"` // null until built
	Error      *string    `json:"error"`    // null unless failed
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func newAppResponse(a store.App, current *store.Deployment) appResponse {
	r := appResponse{
		ID: a.ID, Name: a.Name, RepoURL: a.RepoURL, Branch: a.Branch, Hostname: a.Hostname,
		Port: a.Port, HealthCheckPath: a.HealthCheckPath, MemoryLimitBytes: a.MemoryLimitBytes,
		CPULimitMillicores: a.CPULimitMillicores, CreatedAt: a.CreatedAt,
	}
	if current != nil {
		d := newDeploymentResponse(*current)
		r.CurrentDeployment = &d
	}
	return r
}

func newDeploymentResponse(d store.Deployment) deploymentResponse {
	r := deploymentResponse{
		ID: d.ID, AppID: d.AppID, CommitSHA: d.CommitSHA, Status: string(d.Status),
		CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
	}
	if d.ImageTag != "" {
		r.ImageTag = &d.ImageTag
	}
	if d.Error != "" {
		r.Error = &d.Error
	}
	return r
}

func newDeploymentResponses(ds []store.Deployment) []deploymentResponse {
	out := make([]deploymentResponse, 0, len(ds)) // [] not null when empty
	for _, d := range ds {
		out = append(out, newDeploymentResponse(d))
	}
	return out
}

// Handlers

func (a *api) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := a.store.Apps(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]appResponse, 0, len(apps))
	for _, app := range apps {
		current, err := a.current(r.Context(), app.ID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		out = append(out, newAppResponse(app, current))
	}
	writeJSON(w, http.StatusOK, out)
}

type createAppRequest struct {
	Name               string `json:"name"`
	RepoURL            string `json:"repoUrl"`
	Port               int    `json:"port"`
	Branch             string `json:"branch"`             // optional, default main
	HealthCheckPath    string `json:"healthCheckPath"`    // optional, default /
	MemoryLimitBytes   int64  `json:"memoryLimitBytes"`   // optional, default 256 MiB
	CPULimitMillicores int    `json:"cpuLimitMillicores"` // optional, default 500
}

// Mirrors the database's checks, so users get a clear 400 rather than a
// constraint error.
var (
	appNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)
	envKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)
	shaPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

const (
	maxMemoryLimitBytes = 8 << 30 // half the server's RAM: no app can take more
	maxCPUMillicores    = 4000    // the server has 4 cores
	maxEnvValueBytes    = 32 << 10
)

func (a *api) createApp(w http.ResponseWriter, r *http.Request) {
	var req createAppRequest
	if !readJSON(w, r, &req) {
		return
	}
	switch {
	case !appNamePattern.MatchString(req.Name):
		badRequest(w, "name: lowercase letters, digits and inner hyphens, starting with a letter, at most 32 characters")
		return
	case req.Port < 1 || req.Port > 65535:
		badRequest(w, "port: the port the app listens on inside its container, 1–65535")
		return
	case req.HealthCheckPath != "" && req.HealthCheckPath[0] != '/':
		badRequest(w, "healthCheckPath: must start with /")
		return
	case req.MemoryLimitBytes != 0 && (req.MemoryLimitBytes < 6<<20 || req.MemoryLimitBytes > maxMemoryLimitBytes):
		badRequest(w, "memoryLimitBytes: between 6 MiB and 8 GiB")
		return
	case req.CPULimitMillicores != 0 && (req.CPULimitMillicores < 1 || req.CPULimitMillicores > maxCPUMillicores):
		badRequest(w, "cpuLimitMillicores: between 1 and 4000 (4 cores)")
		return
	}
	if _, _, err := source.ParseRepoURL(req.RepoURL); err != nil {
		badRequest(w, "repoUrl: want https://github.com/owner/repo")
		return
	}

	app, err := a.store.CreateApp(r.Context(), store.NewApp{
		Name: req.Name, RepoURL: req.RepoURL, Hostname: req.Name + "." + a.appsDomain, Port: req.Port,
		Branch: req.Branch, HealthCheckPath: req.HealthCheckPath,
		MemoryLimitBytes: req.MemoryLimitBytes, CPULimitMillicores: req.CPULimitMillicores,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, newAppResponse(app, nil))
}

func (a *api) getApp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	app, err := a.store.App(ctx, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	current, err := a.current(ctx, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	keys, err := a.store.EnvVarKeys(ctx, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	deployments, err := a.store.Deployments(ctx, id, 20)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, appDetailResponse{
		appResponse: newAppResponse(app, current),
		EnvKeys:     keys,
		Deployments: newDeploymentResponses(deployments),
	})
}

type setEnvVarRequest struct {
	Value string `json:"value"`
}

func (a *api) setEnvVar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if !envKeyPattern.MatchString(key) {
		badRequest(w, "key: letters, digits and underscores, not starting with a digit")
		return
	}
	var req setEnvVarRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Value) > maxEnvValueBytes {
		badRequest(w, "value: at most 32 KiB")
		return
	}
	if err := a.store.SetEnvVar(r.Context(), id, key, req.Value); err != nil {
		writeError(w, r, err)
		return
	}
	// The value is never echoed back.
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) deleteEnvVar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteEnvVar(r.Context(), id, r.PathValue("key")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listDeployments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.store.App(r.Context(), id); err != nil {
		writeError(w, r, err) // 404 for an unknown app, not an empty list
		return
	}
	ds, err := a.store.Deployments(r.Context(), id, 50)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newDeploymentResponses(ds))
}

type createDeploymentRequest struct {
	CommitSHA string `json:"commitSha"`
}

func (a *api) createDeployment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req createDeploymentRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !shaPattern.MatchString(req.CommitSHA) {
		badRequest(w, "commitSha: the full 40-character commit SHA (lowercase hex)")
		return
	}
	d, err := a.deployer.Start(r.Context(), id, req.CommitSHA)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 202: the deploy has started and runs in the background; poll the app
	// or the deployment list for its status.
	writeJSON(w, http.StatusAccepted, newDeploymentResponse(d))
}

// current returns an app's live deployment, or nil if it has none.
func (a *api) current(ctx context.Context, appID int64) (*store.Deployment, error) {
	d, err := a.store.CurrentDeployment(ctx, appID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// Helpers

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeJSON(w, http.StatusNotFound, errorResponse{"not found"})
		return 0, false
	}
	return id, true
}

// readJSON decodes a JSON body of at most 64 KiB, rejecting unknown fields
// and any content type other than JSON (see the package comment for why).
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{"Content-Type must be application/json"})
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		badRequest(w, "body: not valid JSON for this request")
		return false
	}
	return true
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, errorResponse{msg})
}

// writeError maps store errors to statuses. Their messages are written to be
// shown to users; anything else is logged and answered generically.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{"not found"})
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrDeployInProgress):
		writeJSON(w, http.StatusConflict, errorResponse{err.Error()})
	case errors.Is(err, store.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, errorResponse{err.Error()})
	default:
		slog.Error("platform API request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{"internal error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing JSON response", "err", err)
	}
}
