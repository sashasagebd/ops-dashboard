// Package api is the platform's HTTP API: registering apps, setting their env
// vars, deploying them, the reconciler's events, and streaming deploy and app
// logs (logs.go). The
// browser reaches it through the dashboard,
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
	"math"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/deploy"
	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/metrics"
	"github.com/sashasagebd/ops-dashboard/backend/internal/source"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Store is what the API needs from the database; in production it's a
// *store.Store.
type Store interface {
	Apps(ctx context.Context) ([]store.App, error)
	App(ctx context.Context, id int64) (store.App, error)
	CreateApp(ctx context.Context, a store.NewApp) (store.App, error)
	SetAutoDeploy(ctx context.Context, id int64, on bool) (store.App, error)
	SetPublicName(ctx context.Context, id int64, name string) (store.App, error)
	EnvVarKeys(ctx context.Context, appID int64) ([]string, error)
	SetEnvVar(ctx context.Context, appID int64, key, value string) error
	DeleteEnvVar(ctx context.Context, appID int64, key string) error
	Deployments(ctx context.Context, appID int64, limit int) ([]store.Deployment, error)
	Deployment(ctx context.Context, id int64) (store.Deployment, error)
	CurrentDeployment(ctx context.Context, appID int64) (store.Deployment, error)
	DeploymentLogs(ctx context.Context, deploymentID, afterID int64, limit int) ([]store.LogLine, error)
	DeploymentLogComplete(ctx context.Context, deploymentID int64) (bool, error)
	KeptImages(ctx context.Context, appID int64) ([]string, error)
	Events(ctx context.Context, appID int64, limit int) ([]store.Event, error)
	Database(ctx context.Context, appID int64) (store.Database, error)
	CreateDatabase(ctx context.Context, appID int64) (store.Database, error)
	Backups(ctx context.Context, appID int64, limit int) ([]store.Backup, error)
}

// Backups starts backups now; in production it's a *backup.Backuper, or nil
// if backups aren't set up (no BACKUP_VOLUME).
type Backups interface {
	Start(ctx context.Context, appID int64) (store.Backup, error)
}

// Requests is apps' recent request numbers, by app name (its tailnet and
// public hostnames together); in production it's a *metrics.Collector, or
// nil in tests that don't need it.
type Requests interface {
	Window(host string, window time.Duration) metrics.Series
	Total(host string, window time.Duration) (float64, bool)
}

// errBackupsOff answers "Back up now" on a platform without backups set up.
var errBackupsOff = errors.New("backups aren't set up on this server (see BACKUP_DIR in the README)")

// Deployer starts deploys; in production it's a *deploy.Deployer.
type Deployer interface {
	Start(ctx context.Context, appID int64, commitSHA string) (store.Deployment, error)
	StartLatest(ctx context.Context, appID int64) (store.Deployment, error)
	Rollback(ctx context.Context, appID, deploymentID int64) (store.Deployment, error)
	ApplyRoutes(ctx context.Context) error
	StopApp(ctx context.Context, appID int64) (store.App, error)
	StartApp(ctx context.Context, appID int64) (store.App, error)
	DeleteApp(ctx context.Context, appID int64) (store.DeletedApp, error)
}

// Engine reads app containers' output; in production it's an *engine.Client.
type Engine interface {
	ManagedContainers(ctx context.Context, app string) ([]engine.ManagedContainer, error)
	FollowLogs(ctx context.Context, id string, tail int, since time.Time, emit func(engine.LogLine)) error
}

// errPublicOff answers making an app public on a platform without a public
// domain.
var errPublicOff = errors.New("public hostnames aren't set up on this server (see PUBLIC_DOMAIN in the README)")

// New returns the platform API's handler. Apps get the hostname
// <name>.<appsDomain>, and public ones <public name>.<publicDomain> ("": no
// app can be public).
func New(st Store, dep Deployer, eng Engine, bk Backups, req Requests, appsDomain, publicDomain string) http.Handler {
	a := &api{store: st, deployer: dep, engine: eng, backups: bk, requests: req,
		appsDomain: appsDomain, publicDomain: publicDomain}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/platform/apps", a.listApps)
	mux.HandleFunc("POST /api/platform/apps", a.createApp)
	mux.HandleFunc("GET /api/platform/apps/{id}", a.getApp)
	mux.HandleFunc("PATCH /api/platform/apps/{id}", a.updateApp)
	mux.HandleFunc("DELETE /api/platform/apps/{id}", a.deleteApp)
	mux.HandleFunc("POST /api/platform/apps/{id}/stop", a.stopApp)
	mux.HandleFunc("POST /api/platform/apps/{id}/start", a.startApp)
	mux.HandleFunc("PUT /api/platform/apps/{id}/env/{key}", a.setEnvVar)
	mux.HandleFunc("DELETE /api/platform/apps/{id}/env/{key}", a.deleteEnvVar)
	mux.HandleFunc("GET /api/platform/apps/{id}/deployments", a.listDeployments)
	mux.HandleFunc("POST /api/platform/apps/{id}/deployments", a.createDeployment)
	mux.HandleFunc("POST /api/platform/apps/{id}/deployments/latest", a.deployLatest)
	mux.HandleFunc("POST /api/platform/apps/{id}/deployments/{deploymentId}/rollback", a.rollback)
	mux.HandleFunc("GET /api/platform/apps/{id}/deployments/{deploymentId}/logs", a.deploymentLogs)
	mux.HandleFunc("GET /api/platform/apps/{id}/logs", a.appLogs)
	mux.HandleFunc("GET /api/platform/events", a.listEvents)
	mux.HandleFunc("POST /api/platform/apps/{id}/database", a.createDatabase)
	mux.HandleFunc("POST /api/platform/apps/{id}/backups", a.createBackup)
	mux.HandleFunc("GET /api/platform/apps/{id}/metrics", a.appMetrics)
	return mux
}

type api struct {
	store      Store
	deployer   Deployer
	engine     Engine
	backups    Backups  // nil if backups aren't set up
	requests   Requests // nil: no request numbers
	appsDomain string

	publicDomain string // "" if nothing can be public
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
	SourceDir          string              `json:"sourceDir"`     // "" is the repo's root
	AutoDeploy         bool                `json:"autoDeploy"`    // pushes to Branch deploy it
	Stopped            bool                `json:"stopped"`       // stopped on purpose: not running, not routed, no deploys
	NextCommitSHA      *string             `json:"nextCommitSha"` // a pushed commit waiting for the deploy in progress
	CreatedAt          time.Time           `json:"createdAt"`
	CurrentDeployment  *deploymentResponse `json:"currentDeployment"` // null if nothing is live
	// The name in front of PUBLIC_DOMAIN it's public at, and the full
	// hostname, null if it's tailnet-only. The hostname is also null while
	// no public domain is set up, though the name is kept.
	PublicName     *string `json:"publicName"`
	PublicHostname *string `json:"publicHostname"`
	// Requests to its hostnames in the last hour, from Caddy; null if there
	// are no readings (the platform just started, or Caddy can't be read).
	RequestsLastHour *int64 `json:"requestsLastHour"`
}

// metricsResponse is a window of an app's request numbers, one value per
// step from start, oldest first; null where there was no reading.
type metricsResponse struct {
	Start       time.Time  `json:"start"`
	StepSeconds int        `json:"stepSeconds"`
	Requests    []*float64 `json:"requests"`
	Errors      []*float64 `json:"errors"` // 5xx responses
	P95Ms       []*float64 `json:"p95Ms"`  // 95th percentile response time; null also when there were no requests
}

type appDetailResponse struct {
	appResponse
	EnvKeys     []string             `json:"envKeys"`     // names only; values are write-only
	Deployments []deploymentResponse `json:"deployments"` // newest first
	Events      []eventResponse      `json:"events"`      // the reconciler's, newest first
	Database    *databaseResponse    `json:"database"`    // null if the app has none
	Backups     []backupResponse     `json:"backups"`     // its database's, newest first
	// The server's public domain, which a public name goes in front of;
	// null if public hostnames aren't set up.
	PublicDomain *string `json:"publicDomain"`
}

// backupResponse is one backup of an app's database. The dump is a file in
// the backup directory, under the app's name.
type backupResponse struct {
	ID         int64      `json:"id"`
	Status     string     `json:"status"`    // queued, running, done or failed
	File       *string    `json:"file"`      // the dump's name; null until done
	SizeBytes  *int64     `json:"sizeBytes"` // null until done
	Error      *string    `json:"error"`     // null unless failed
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

func newBackupResponse(b store.Backup) backupResponse {
	r := backupResponse{ID: b.ID, Status: string(b.Status), CreatedAt: b.CreatedAt, FinishedAt: b.FinishedAt}
	if b.Status == store.BackupDone {
		r.File, r.SizeBytes = &b.File, &b.SizeBytes
	}
	if b.Error != "" {
		r.Error = &b.Error
	}
	return r
}

// databaseResponse is an app's own Postgres. Its password is never shown:
// the app gets it inside DATABASE_URL.
type databaseResponse struct {
	Name      string    `json:"name"` // its container, volume and network, e.g. ops-db-hello
	CreatedAt time.Time `json:"createdAt"`
	// "creating" until the platform has first created its container, then
	// Docker's state of it ("running", "exited", ...), or "missing" while it's
	// being created again.
	State string `json:"state"`
}

// eventResponse is something the reconciler found and fixed (or failed to).
type eventResponse struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	AppID   *int64    `json:"appId"` // null if it isn't about one app
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
	Output  []string  `json:"output"` // a crashed container's last lines, oldest first; [] if none
}

type deploymentResponse struct {
	ID        int64   `json:"id"`
	AppID     int64   `json:"appId"`
	CommitSHA string  `json:"commitSha"`
	Status    string  `json:"status"`
	ImageTag  *string `json:"imageTag"` // null until built
	Error     *string `json:"error"`    // null unless failed
	// The deployment whose image this one runs, if it's a rollback.
	RollbackOf *int64 `json:"rollbackOf"`
	// Whether the app can be rolled back to this deployment (it was live
	// before, its image is kept, and it isn't the live version). Only set in
	// an app's deployment lists; false elsewhere.
	Rollbackable bool       `json:"rollbackable"`
	CreatedAt    time.Time  `json:"createdAt"`
	StartedAt    *time.Time `json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// appResponse is newAppResponse with the app's public hostname and
// last-hour requests.
func (a *api) appResponse(app store.App, current *store.Deployment) appResponse {
	r := newAppResponse(app, current)
	if host := deploy.PublicHost(app.PublicName, a.publicDomain); host != "" {
		r.PublicHostname = &host
	}
	if a.requests != nil {
		if n, ok := a.requests.Total(app.Name, time.Hour); ok {
			v := int64(math.Round(n))
			r.RequestsLastHour = &v
		}
	}
	return r
}

func newAppResponse(a store.App, current *store.Deployment) appResponse {
	r := appResponse{
		ID: a.ID, Name: a.Name, RepoURL: a.RepoURL, Branch: a.Branch, Hostname: a.Hostname,
		Port: a.Port, HealthCheckPath: a.HealthCheckPath, MemoryLimitBytes: a.MemoryLimitBytes,
		CPULimitMillicores: a.CPULimitMillicores, SourceDir: a.SourceDir, AutoDeploy: a.AutoDeploy, Stopped: a.Stopped,
		CreatedAt: a.CreatedAt,
	}
	if a.NextCommitSHA != "" {
		r.NextCommitSHA = &a.NextCommitSHA
	}
	if a.PublicName != "" {
		r.PublicName = &a.PublicName
	}
	if current != nil {
		d := newDeploymentResponse(*current)
		r.CurrentDeployment = &d
	}
	return r
}

func newEventResponses(es []store.Event) []eventResponse {
	out := make([]eventResponse, 0, len(es)) // [] not null when empty
	for _, e := range es {
		r := eventResponse{ID: e.ID, At: e.At, Kind: string(e.Kind), Message: e.Message, Output: e.Output}
		if e.AppID != 0 {
			r.AppID = &e.AppID
		}
		if r.Output == nil {
			r.Output = []string{}
		}
		out = append(out, r)
	}
	return out
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
	if d.RollbackOf != 0 {
		r.RollbackOf = &d.RollbackOf
	}
	return r
}

// deploymentList is an app's deployments, each saying whether it can be
// rolled back to, given the app's kept images and live version (nil if none).
func (a *api) deploymentList(ctx context.Context, appID int64, ds []store.Deployment, current *store.Deployment) ([]deploymentResponse, error) {
	kept, err := a.store.KeptImages(ctx, appID)
	if err != nil {
		return nil, err
	}
	live := ""
	if current != nil {
		live = current.ImageTag
	}
	out := make([]deploymentResponse, 0, len(ds)) // [] not null when empty
	for _, d := range ds {
		r := newDeploymentResponse(d)
		r.Rollbackable = store.CheckRollback(d, kept, live) == nil
		out = append(out, r)
	}
	return out, nil
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
		out = append(out, a.appResponse(app, current))
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
	SourceDir          string `json:"sourceDir"`          // optional, default the repo's root
}

// Mirrors the database's checks, so users get a clear 400 rather than a
// constraint error.
var (
	appNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)
	envKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)
	shaPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// Path segments of a source directory; "." and ".." are refused
	// separately.
	dirPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
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
	case !validSourceDir(req.SourceDir):
		badRequest(w, "sourceDir: a folder in the repo, e.g. services/web (no leading or trailing slash, no . or ..)")
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
		SourceDir: req.SourceDir,
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
	list, err := a.deploymentList(ctx, id, deployments, current)
	if err != nil {
		writeError(w, r, err)
		return
	}
	events, err := a.store.Events(ctx, id, appEvents)
	if err != nil {
		writeError(w, r, err)
		return
	}
	db, err := a.database(ctx, app)
	if err != nil {
		writeError(w, r, err)
		return
	}
	bs, err := a.store.Backups(ctx, id, appBackups)
	if err != nil {
		writeError(w, r, err)
		return
	}
	backups := make([]backupResponse, 0, len(bs)) // [] not null when empty
	for _, b := range bs {
		backups = append(backups, newBackupResponse(b))
	}
	var publicDomain *string
	if a.publicDomain != "" {
		publicDomain = &a.publicDomain
	}
	writeJSON(w, http.StatusOK, appDetailResponse{
		appResponse:  a.appResponse(app, current),
		PublicDomain: publicDomain,
		EnvKeys:      keys,
		Deployments:  list,
		Events:       newEventResponses(events),
		Database:     db,
		Backups:      backups,
	})
}

// updateAppRequest is the settings that can change; at least one, and
// those left out stay as they are.
type updateAppRequest struct {
	AutoDeploy *bool `json:"autoDeploy"`
	// PublicName makes the app public at <publicName>.<PUBLIC_DOMAIN>; ""
	// makes it tailnet-only again.
	PublicName *string `json:"publicName"`
}

// publicNameRe is one DNS label, lowercase, as the database checks it.
var publicNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (a *api) updateApp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req updateAppRequest
	if !readJSON(w, r, &req) {
		return
	}
	switch {
	case req.AutoDeploy == nil && req.PublicName == nil:
		badRequest(w, "nothing to change: autoDeploy (true or false) or publicName")
		return
	case req.PublicName != nil && *req.PublicName != "" && !publicNameRe.MatchString(*req.PublicName):
		badRequest(w, "publicName: lowercase letters, digits and inner hyphens, at most 63 characters (one part of a hostname, e.g. api)")
		return
	case req.PublicName != nil && *req.PublicName == "hooks":
		writeJSON(w, http.StatusConflict, errorResponse{"publicName: hooks is the GitHub webhook's"})
		return
	case req.PublicName != nil && *req.PublicName != "" && a.publicDomain == "":
		writeJSON(w, http.StatusConflict, errorResponse{errPublicOff.Error()})
		return
	}
	// Each setting is its own update; both are idempotent, so a failure
	// halfway leaves nothing that a retry doesn't fix.
	var app store.App
	var err error
	if req.AutoDeploy != nil {
		if app, err = a.store.SetAutoDeploy(r.Context(), id, *req.AutoDeploy); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if req.PublicName != nil {
		if app, err = a.store.SetPublicName(r.Context(), id, *req.PublicName); err != nil {
			writeError(w, r, err)
			return
		}
		// Caddy's routes follow at once, rather than at the reconciler's
		// next pass.
		if err := a.deployer.ApplyRoutes(r.Context()); err != nil {
			slog.Warn("applying routes after a public name change; the reconciler will retry", "app", app.Name, "err", err)
		}
	}
	current, err := a.current(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.appResponse(app, current))
}

// stopApp stops the app: its container is removed and its routes go; its
// database keeps running. 409 while a deploy runs.
func (a *api) stopApp(w http.ResponseWriter, r *http.Request) {
	a.lifecycle(w, r, a.deployer.StopApp)
}

// startApp runs the app's live version again (no build) and restores its
// routes.
func (a *api) startApp(w http.ResponseWriter, r *http.Request) {
	a.lifecycle(w, r, a.deployer.StartApp)
}

func (a *api) lifecycle(w http.ResponseWriter, r *http.Request, change func(context.Context, int64) (store.App, error)) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	app, err := change(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	current, err := a.current(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.appResponse(app, current))
}

// deletedResponse is what deleting an app left behind for the owner to
// remove by hand, if they want: the database's volume (the platform can't
// remove volumes), null if it had no database. Backups stay too.
type deletedResponse struct {
	Name           string  `json:"name"`
	DatabaseVolume *string `json:"databaseVolume"`
}

// deleteApp deletes the app and everything the platform holds for it, apart
// from its database's data and its backups. 409 while a deploy or a backup
// runs.
func (a *api) deleteApp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	deleted, err := a.deployer.DeleteApp(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	res := deletedResponse{Name: deleted.App.Name}
	if deleted.HadDatabase {
		v := deploy.DatabaseName(deleted.App.Name)
		res.DatabaseVolume = &v
	}
	writeJSON(w, http.StatusOK, res)
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
	current, err := a.current(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	list, err := a.deploymentList(r.Context(), id, ds, current)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
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

// deployLatest deploys the newest commit on the app's branch. No body.
func (a *api) deployLatest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	d, err := a.deployer.StartLatest(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, newDeploymentResponse(d))
}

// rollback rolls the app back to one of its earlier deployments: a new
// deployment running that one's image, with no build. No body. It also turns
// the app's auto-deploy off (see store.CreateRollback). 409 with the reason
// if that deployment can't be rolled back to, or a deploy is in progress.
func (a *api) rollback(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target, err := strconv.ParseInt(r.PathValue("deploymentId"), 10, 64)
	if err != nil || target < 1 {
		writeJSON(w, http.StatusNotFound, errorResponse{"not found"})
		return
	}
	d, err := a.deployer.Rollback(r.Context(), id, target)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, newDeploymentResponse(d))
}

// How many of the reconciler's events each view gets: the recent ones, for
// a glance at what has needed fixing.
const (
	appEvents = 20 // in an app's detail
	allEvents = 50 // GET /api/platform/events
	// An app's backups in its detail: as many as the platform keeps dumps of.
	appBackups = 7
)

// createBackup backs up the app's database now (no body). 202: it runs in
// the background; the app's detail shows how it went. 409 if the app has no
// database (yet), a backup is in progress, or backups aren't set up.
func (a *api) createBackup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.store.App(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	if a.backups == nil {
		writeJSON(w, http.StatusConflict, errorResponse{errBackupsOff.Error()})
		return
	}
	b, err := a.backups.Start(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, newBackupResponse(b))
}

// listEvents is the reconciler's most recent events, for every app and those
// about no app, newest first.
func (a *api) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := a.store.Events(r.Context(), 0, allEvents)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newEventResponses(events))
}

// createDatabase gives the app a database (no body): the platform creates it
// within seconds, and the app gets DATABASE_URL from its next deploy. 201;
// 409 if it has one already.
func (a *api) createDatabase(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	app, err := a.store.App(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := a.store.CreateDatabase(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	db, err := a.database(r.Context(), app)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, db)
}

// database is the app's database as the API shows it, or nil if it has none.
// Its state comes from Docker; if Docker can't be asked, it's "unknown"
// rather than failing the whole page.
func (a *api) database(ctx context.Context, app store.App) (*databaseResponse, error) {
	db, err := a.store.Database(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := &databaseResponse{Name: deploy.DatabaseName(app.Name), CreatedAt: db.CreatedAt, State: "creating"}
	if db.ProvisionedAt == nil {
		return r, nil
	}
	containers, err := a.engine.ManagedContainers(ctx, app.Name)
	if err != nil {
		slog.Warn("reading a database's state", "app", app.Name, "err", err)
		r.State = "unknown"
		return r, nil
	}
	r.State = "missing"
	for _, c := range containers {
		if c.Labels[engine.LabelRole] == engine.RoleDatabase {
			r.State = c.State
		}
	}
	return r, nil
}

// metricsWindows are the windows GET .../metrics serves, as the dashboard's
// history does.
var metricsWindows = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour}

// appMetrics is the app's request numbers over the last hour or day
// (?window=1h|24h, default 1h), one value a minute.
func (a *api) appMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	param := r.URL.Query().Get("window")
	if param == "" {
		param = "1h"
	}
	window, ok := metricsWindows[param]
	if !ok {
		badRequest(w, "window: 1h or 24h")
		return
	}
	app, err := a.store.App(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if a.requests == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{"not found"})
		return
	}
	s := a.requests.Window(app.Name, window)
	p95 := make([]*float64, len(s.P95))
	for i, v := range s.P95 {
		if v != nil {
			ms := math.Round(*v*1000*10) / 10 // ms, one decimal
			p95[i] = &ms
		}
	}
	writeJSON(w, http.StatusOK, metricsResponse{
		Start: s.Start, StepSeconds: int(s.Step / time.Second),
		Requests: s.Requests, Errors: s.Errors, P95Ms: p95,
	})
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

// validSourceDir matches the database's apps_source_dir_check: "" (the repo's
// root) or a relative path without ".", ".." or empty parts.
func validSourceDir(dir string) bool {
	if dir == "" {
		return true
	}
	for part := range strings.SplitSeq(dir, "/") {
		if part == "." || part == ".." || !dirPartPattern.MatchString(part) {
			return false
		}
	}
	return true
}

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
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrDeployInProgress),
		errors.Is(err, store.ErrNotRollbackable), errors.Is(err, store.ErrNoDatabase),
		errors.Is(err, store.ErrBackupInProgress), errors.Is(err, store.ErrAppStopped):
		writeJSON(w, http.StatusConflict, errorResponse{err.Error()})
	case errors.Is(err, store.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, errorResponse{err.Error()})
	case errors.Is(err, source.ErrBranchNotFound):
		writeJSON(w, http.StatusBadRequest, errorResponse{"the app's branch wasn't found on GitHub"})
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
