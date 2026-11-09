// Package webhook receives GitHub's push events and deploys the pushed
// commit to every app on that repo and branch with auto-deploy on.
//
// It's the platform's only endpoint reachable from the internet (through
// Tailscale Funnel), so the platform serves it on a listener of its own with
// nothing else on it (see docs/decisions/platform.md), and it trusts nothing
// in a request until its signature checks out: GitHub signs each delivery's
// body with the webhook's secret (HMAC-SHA256), and only GitHub and the
// platform know the secret.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/sashasagebd/ops-dashboard/backend/internal/source"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Path is where GitHub sends deliveries.
const Path = "/webhooks/github"

// maxBodyBytes caps a delivery. Push payloads are a few kilobytes (GitHub
// includes at most 20 commits); this leaves plenty of room while bounding
// what an unauthenticated request can make the platform read and hash.
const maxBodyBytes = 1 << 20

// Store is what the webhook needs from the database; in production it's a
// *store.Store.
type Store interface {
	RecordDelivery(ctx context.Context, id string) (bool, error)
	ForgetDelivery(ctx context.Context, id string) error
	Apps(ctx context.Context) ([]store.App, error)
}

// Deployer deploys pushed commits; in production it's a *deploy.Deployer.
type Deployer interface {
	Push(ctx context.Context, appID int64, commitSHA string) (store.Deployment, store.RequestOutcome, error)
}

// New returns the webhook listener's handler: Path and nothing else. secret
// must not be empty.
func New(secret string, st Store, dep Deployer) http.Handler {
	if secret == "" {
		panic("webhook: empty secret")
	}
	h := &handler{secret: []byte(secret), store: st, deployer: dep}
	mux := http.NewServeMux()
	mux.Handle("POST "+Path, h)
	return mux
}

type handler struct {
	secret   []byte
	store    Store
	deployer Deployer
}

// pushEvent is the part of GitHub's push payload the platform reads.
type pushEvent struct {
	Ref        string `json:"ref"`   // refs/heads/<branch>, or refs/tags/<tag>
	After      string `json:"after"` // the branch's new head
	Deleted    bool   `json:"deleted"`
	Repository struct {
		HTMLURL string `json:"html_url"` // https://github.com/owner/repo
	} `json:"repository"`
}

// appResult says what a push did to one app, in the response GitHub shows
// on the webhook's "Recent deliveries" page.
type appResult struct {
	App          string `json:"app"`
	Result       string `json:"result"`
	DeploymentID *int64 `json:"deploymentId,omitempty"`
}

type response struct {
	Result string      `json:"result"`
	Apps   []appResult `json:"apps,omitempty"`
}

var (
	shaPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	deliveryPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
)

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			reply(w, http.StatusRequestEntityTooLarge, response{Result: "body too large"})
			return
		}
		reply(w, http.StatusBadRequest, response{Result: "couldn't read the body"})
		return
	}
	if !validSignature(h.secret, body, r.Header.Get("X-Hub-Signature-256")) {
		slog.Warn("webhook: refused a delivery with a missing or wrong signature")
		reply(w, http.StatusUnauthorized, response{Result: "bad signature"})
		return
	}

	// From here on, the request is GitHub's.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		reply(w, http.StatusUnsupportedMediaType, response{Result: "set the webhook's content type to application/json"})
		return
	}
	event, delivery := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	if !deliveryPattern.MatchString(delivery) {
		reply(w, http.StatusBadRequest, response{Result: "missing X-GitHub-Delivery"})
		return
	}
	switch event {
	case "ping": // sent once when the webhook is created
		reply(w, http.StatusOK, response{Result: "pong"})
		return
	case "push":
	default:
		reply(w, http.StatusOK, response{Result: "ignored: not a push event"})
		return
	}

	var push pushEvent
	if err := json.Unmarshal(body, &push); err != nil {
		reply(w, http.StatusBadRequest, response{Result: "not a push payload"})
		return
	}
	branch, isBranch := strings.CutPrefix(push.Ref, "refs/heads/")
	switch {
	case !isBranch:
		reply(w, http.StatusOK, response{Result: "ignored: not a branch (a tag?)"})
		return
	case push.Deleted:
		reply(w, http.StatusOK, response{Result: "ignored: the branch was deleted"})
		return
	case !shaPattern.MatchString(push.After):
		reply(w, http.StatusBadRequest, response{Result: "not a push payload"})
		return
	}
	owner, repo, err := source.ParseRepoURL(push.Repository.HTMLURL)
	if err != nil {
		reply(w, http.StatusBadRequest, response{Result: "not a GitHub repository URL"})
		return
	}

	// A redelivery (GitHub's retry, or "Redeliver" pressed) of a push
	// already handled would deploy again.
	ctx := r.Context()
	isNew, err := h.store.RecordDelivery(ctx, delivery)
	if err != nil {
		internalError(w, "recording the delivery", err)
		return
	}
	if !isNew {
		reply(w, http.StatusOK, response{Result: "ignored: already handled this delivery"})
		return
	}

	apps, err := h.store.Apps(ctx)
	if err != nil {
		// Nothing was deployed: forget the delivery, so redelivering it
		// works.
		if err := h.store.ForgetDelivery(context.WithoutCancel(ctx), delivery); err != nil {
			slog.Error("webhook: forgetting a failed delivery", "err", err)
		}
		internalError(w, "listing apps", err)
		return
	}

	res := response{Result: "no app deploys " + owner + "/" + repo + " " + branch, Apps: []appResult{}}
	status := http.StatusOK
	for _, app := range apps {
		appOwner, appRepo, err := source.ParseRepoURL(app.RepoURL)
		// GitHub's owner and repo names aren't case-sensitive.
		if err != nil || !strings.EqualFold(appOwner, owner) || !strings.EqualFold(appRepo, repo) || app.Branch != branch {
			continue
		}
		if !app.AutoDeploy {
			res.Apps = append(res.Apps, appResult{App: app.Name, Result: "auto-deploy is off"})
			continue
		}
<<<<<<< Updated upstream
		dep, started, err := h.deployer.Push(ctx, app.ID, push.After)
=======
		if app.Stopped {
			res.Apps = append(res.Apps, appResult{App: app.Name, Result: "the app is stopped"})
			continue
		}
		dep, outcome, err := h.deployer.Push(ctx, app.ID, push.After)
>>>>>>> Stashed changes
		switch {
		case err != nil:
			slog.Error("webhook: deploying a push", "app", app.Name, "commit", push.After, "err", err)
			res.Apps = append(res.Apps, appResult{App: app.Name, Result: "internal error; see the platform's logs"})
			status = http.StatusInternalServerError
		case outcome == store.RequestKnown:
			// The branch poller got there first, or the same commit was
			// pushed again.
			res.Apps = append(res.Apps, appResult{App: app.Name, Result: "already deployed, or on its way"})
		case outcome == store.RequestStarted:
			slog.Info("webhook: deploying a push", "app", app.Name, "commit", push.After, "deployment", dep.ID)
			res.Apps = append(res.Apps, appResult{App: app.Name, Result: "deploying", DeploymentID: &dep.ID})
		default:
			slog.Info("webhook: push waits for the deploy in progress", "app", app.Name, "commit", push.After)
			res.Apps = append(res.Apps, appResult{App: app.Name, Result: "deploys next, after the deploy in progress"})
		}
	}
	if len(res.Apps) > 0 {
		res.Result = "handled"
	}
	reply(w, status, res)
}

// validSignature checks GitHub's X-Hub-Signature-256 ("sha256=" and the hex
// HMAC-SHA256 of the body), in constant time so the comparison can't leak
// how much of a guess was right.
func validSignature(secret, body []byte, header string) bool {
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func internalError(w http.ResponseWriter, what string, err error) {
	slog.Error("webhook: "+what, "err", err)
	reply(w, http.StatusInternalServerError, response{Result: "internal error; see the platform's logs"})
}

func reply(w http.ResponseWriter, status int, res response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}
