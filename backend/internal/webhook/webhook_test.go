package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

const (
	secret = "test-webhook-secret-0123456789"
	sha    = "0123456789abcdef0123456789abcdef01234567"
)

type fakeStore struct {
	apps       []store.App
	deliveries map[string]bool
	appsErr    error
	forgotten  []string
}

func (f *fakeStore) RecordDelivery(_ context.Context, id string) (bool, error) {
	if f.deliveries[id] {
		return false, nil
	}
	f.deliveries[id] = true
	return true, nil
}

func (f *fakeStore) ForgetDelivery(_ context.Context, id string) error {
	delete(f.deliveries, id)
	f.forgotten = append(f.forgotten, id)
	return nil
}

func (f *fakeStore) Apps(context.Context) ([]store.App, error) { return f.apps, f.appsErr }

// fakeDeployer records pushes; apps in busy wait for a deploy in progress.
type fakeDeployer struct {
	pushed []string // "appID sha"
	busy   map[int64]bool
	err    error
	known  map[int64]bool // apps that have the pushed commit already (the poller got there first)
}

func (f *fakeDeployer) Push(_ context.Context, appID int64, commit string) (store.Deployment, store.RequestOutcome, error) {
	if f.err != nil {
		return store.Deployment{}, 0, f.err
	}
	f.pushed = append(f.pushed, commit[:7]+" to "+string(rune('0'+appID)))
	switch {
	case f.known[appID]:
		return store.Deployment{}, store.RequestKnown, nil
	case f.busy[appID]:
		return store.Deployment{}, store.RequestWaiting, nil
	}
	return store.Deployment{ID: 40 + appID, AppID: appID}, store.RequestStarted, nil
}

func newTestWebhook() (http.Handler, *fakeStore, *fakeDeployer) {
	st := &fakeStore{deliveries: map[string]bool{}, apps: []store.App{
		{ID: 1, Name: "web", RepoURL: "https://github.com/Owner/Repo", Branch: "main", AutoDeploy: true},
		{ID: 2, Name: "api", RepoURL: "https://github.com/owner/repo.git", Branch: "main", AutoDeploy: true},
		{ID: 3, Name: "staging", RepoURL: "https://github.com/owner/repo", Branch: "staging", AutoDeploy: true},
		{ID: 4, Name: "paused", RepoURL: "https://github.com/owner/repo", Branch: "main", AutoDeploy: false},
		{ID: 5, Name: "other", RepoURL: "https://github.com/owner/other", Branch: "main", AutoDeploy: true},
	}}
	dep := &fakeDeployer{busy: map[int64]bool{}, known: map[int64]bool{}}
	return New(secret, st, dep), st, dep
}

func pushBody(ref string) string {
	return `{"ref":"` + ref + `","after":"` + sha + `","deleted":false,` +
		`"repository":{"html_url":"https://github.com/owner/repo","full_name":"owner/repo"}}`
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// deliver sends a delivery the way GitHub does, signed unless signature is
// given.
func deliver(t *testing.T, h http.Handler, event, delivery, body string, signature ...string) (*httptest.ResponseRecorder, response) {
	t.Helper()
	req := httptest.NewRequest("POST", Path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if len(signature) > 0 {
		req.Header.Set("X-Hub-Signature-256", signature[0])
	} else {
		req.Header.Set("X-Hub-Signature-256", sign(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res response
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return rec, res
}

func TestPushOfACommitAlreadyKnown(t *testing.T) {
	h, _, dep := newTestWebhook()
	dep.known[1] = true // the branch poller deployed it a moment ago
	_, res := deliver(t, h, "push", "d-known", pushBody("refs/heads/main"))
	if len(res.Apps) == 0 || res.Apps[0].App != "web" || res.Apps[0].Result != "already deployed, or on its way" {
		t.Errorf("apps = %+v, want web reported as already deployed", res.Apps)
	}
}

func TestPushDeploysMatchingApps(t *testing.T) {
	h, _, dep := newTestWebhook()
	dep.busy[2] = true // api is deploying: this push waits for it

	rec, res := deliver(t, h, "push", "d1", pushBody("refs/heads/main"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	// The repo matches whatever its case or ".git"; another branch, another
	// repo, or auto-deploy off doesn't deploy.
	if want := []string{"0123456 to 1", "0123456 to 2"}; !slices.Equal(dep.pushed, want) {
		t.Errorf("pushed %q, want %q", dep.pushed, want)
	}
	var got []string
	for _, a := range res.Apps {
		got = append(got, a.App+": "+a.Result)
	}
	want := []string{"web: deploying", "api: deploys next, after the deploy in progress", "paused: auto-deploy is off"}
	if !slices.Equal(got, want) {
		t.Errorf("results %q, want %q", got, want)
	}
	if res.Apps[0].DeploymentID == nil || *res.Apps[0].DeploymentID != 41 {
		t.Errorf("web's result has no deployment ID: %+v", res.Apps[0])
	}

	// GitHub redelivers it: nothing happens twice.
	rec, res = deliver(t, h, "push", "d1", pushBody("refs/heads/main"))
	if rec.Code != http.StatusOK || !strings.HasPrefix(res.Result, "ignored: already handled") || len(dep.pushed) != 2 {
		t.Errorf("redelivery: %d %q, %d pushes; want ignored", rec.Code, res.Result, len(dep.pushed))
	}
}

func TestSignatureIsCheckedFirst(t *testing.T) {
	body := pushBody("refs/heads/main")
	tests := []struct{ name, signature string }{
		{"missing", ""},
		{"wrong secret", func() string {
			mac := hmac.New(sha256.New, []byte("not-the-secret"))
			mac.Write([]byte(body))
			return "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}()},
		{"another body's", sign(body + " ")},
		{"not hex", "sha256=zz"},
		{"sha1", "sha1=" + strings.Repeat("0", 40)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, st, dep := newTestWebhook()
			rec, _ := deliver(t, h, "push", "d1", body, tt.signature)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status %d, want 401", rec.Code)
			}
			// Nothing was touched, not even the delivery record.
			if len(dep.pushed) != 0 || len(st.deliveries) != 0 {
				t.Errorf("pushed %v, deliveries %v; want nothing", dep.pushed, st.deliveries)
			}
		})
	}
}

func TestIgnoredDeliveries(t *testing.T) {
	tests := []struct {
		name, event, body string
		wantStatus        int
		wantResult        string
	}{
		{"ping", "ping", `{"zen":"Keep it logically awesome."}`, 200, "pong"},
		{"another event", "issues", `{}`, 200, "ignored: not a push event"},
		{"a tag", "push", pushBody("refs/tags/v1.0.0"), 200, "ignored: not a branch (a tag?)"},
		{"branch deleted", "push", strings.Replace(pushBody("refs/heads/main"), `"deleted":false`, `"deleted":true`, 1), 200, "ignored: the branch was deleted"},
		{"no app for it", "push", pushBody("refs/heads/feature"), 200, "no app deploys owner/repo feature"},
		{"garbage", "push", `{"ref":`, 400, "not a push payload"},
		{"bad sha", "push", strings.Replace(pushBody("refs/heads/main"), sha, "main", 1), 400, "not a push payload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, dep := newTestWebhook()
			rec, res := deliver(t, h, tt.event, "d1", tt.body)
			if rec.Code != tt.wantStatus || res.Result != tt.wantResult {
				t.Errorf("%d %q, want %d %q", rec.Code, res.Result, tt.wantStatus, tt.wantResult)
			}
			if len(dep.pushed) != 0 {
				t.Errorf("pushed %v, want nothing", dep.pushed)
			}
		})
	}
}

func TestOnlyTheWebhookIsServed(t *testing.T) {
	h, _, _ := newTestWebhook()
	for _, tt := range []struct{ method, path string }{
		{"GET", Path},
		{"GET", "/api/platform/apps"},
		{"POST", "/api/platform/apps"},
		{"GET", "/healthz"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 404/405", tt.method, tt.path, rec.Code)
		}
	}
}

func TestDeliveryLimits(t *testing.T) {
	h, _, _ := newTestWebhook()
	big := strings.Repeat("x", maxBodyBytes+1)
	if rec, _ := deliver(t, h, "push", "d1", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status %d, want 413", rec.Code)
	}

	// Form-encoded (GitHub's other content type option) is refused, with a
	// hint, after the signature check.
	body := pushBody("refs/heads/main")
	req := httptest.NewRequest("POST", Path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "d2")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form-encoded: status %d, want 415", rec.Code)
	}

	if rec, _ := deliver(t, h, "push", "", body); rec.Code != http.StatusBadRequest {
		t.Errorf("no delivery ID: status %d, want 400", rec.Code)
	}
}

func TestFailures(t *testing.T) {
	t.Run("listing apps fails: the delivery can be redelivered", func(t *testing.T) {
		h, st, _ := newTestWebhook()
		st.appsErr = errors.New("connection refused")
		rec, res := deliver(t, h, "push", "d1", pushBody("refs/heads/main"))
		if rec.Code != http.StatusInternalServerError || strings.Contains(res.Result, "refused") {
			t.Errorf("%d %q; want 500 with a generic message", rec.Code, res.Result)
		}
		if st.deliveries["d1"] {
			t.Error("the failed delivery is still recorded, so redelivering it would be ignored")
		}
	})
	t.Run("deploying fails", func(t *testing.T) {
		h, _, dep := newTestWebhook()
		dep.err = errors.New("pq: deadlock detected")
		rec, res := deliver(t, h, "push", "d1", pushBody("refs/heads/main"))
		if rec.Code != http.StatusInternalServerError || len(res.Apps) == 0 ||
			strings.Contains(rec.Body.String(), "deadlock") {
			t.Errorf("%d %s; want 500, per-app results, nothing internal", rec.Code, rec.Body)
		}
	})
}
