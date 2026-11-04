package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func TestAppImagesFiltersOnDockersSide(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("GET /images/json", respondJSON(http.StatusOK, `[
		{"Id":"sha256:aaa","RepoTags":["ops-platform/hello:0123"],"Labels":{"ops.platform.app":"hello"}},
		{"Id":"sha256:bbb","RepoTags":[],"Labels":{"ops.platform.app":"hello"}}
	]`))

	got, err := c.AppImages(t.Context(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "sha256:aaa" || got[0].Tags[0] != "ops-platform/hello:0123" || len(got[1].Tags) != 0 {
		t.Errorf("AppImages = %+v", got)
	}

	req, _ := f.request("GET /images/json")
	q, _ := url.ParseQuery(req.query)
	var filters map[string]map[string]bool
	if err := json.Unmarshal([]byte(q.Get("filters")), &filters); err != nil {
		t.Fatalf("filters %q: %v", q.Get("filters"), err)
	}
	if !filters["label"][LabelApp+"=hello"] || len(filters["label"]) != 1 {
		t.Errorf("label filters = %v, want app=hello", filters["label"])
	}
}

func TestImageExists(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		want    bool
		wantErr bool
	}{
		{name: "there", status: http.StatusOK, want: true},
		{name: "gone", status: http.StatusNotFound},
		{name: "daemon error", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeDaemon(t)
			f.handle("GET /images/ops-platform/hello:0123/json", respondJSON(tt.status, `{"Id":"sha256:aaa","message":"x"}`))
			got, err := c.ImageExists(t.Context(), "ops-platform/hello:0123")
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("ImageExists = %v, %v; want %v, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestRemoveImage(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr error // nil, ErrImageInUse, or errAny
	}{
		{name: "removed", status: http.StatusOK},
		{name: "already gone", status: http.StatusNotFound},
		{name: "in use", status: http.StatusConflict, wantErr: ErrImageInUse},
		{name: "daemon error", status: http.StatusInternalServerError, wantErr: errAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeDaemon(t)
			f.handle("DELETE /images/ops-platform/hello:0123", respondJSON(tt.status, `[{"Untagged":"ops-platform/hello:0123"}]`))
			err := c.RemoveImage(t.Context(), "ops-platform/hello:0123")
			switch {
			case tt.wantErr == nil && err != nil,
				tt.wantErr == ErrImageInUse && !errors.Is(err, ErrImageInUse),
				tt.wantErr == errAny && (err == nil || errors.Is(err, ErrImageInUse)):
				t.Errorf("RemoveImage error = %v, want %v", err, tt.wantErr)
			}

			req, _ := f.request("DELETE /images/ops-platform/hello:0123")
			q, _ := url.ParseQuery(req.query)
			if q.Get("force") == "1" || q.Get("force") == "true" {
				t.Errorf("query %q: removal must never be forced", req.query)
			}
		})
	}
}

// errAny stands for "some other error" in table tests.
var errAny = errors.New("any error")
