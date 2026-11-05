package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

type kindHandler struct {
	ran, gaveUp []int64
}

func (h *kindHandler) Run(_ context.Context, job store.Job) error {
	h.ran = append(h.ran, job.ID)
	return nil
}

func (h *kindHandler) GiveUp(_ context.Context, job store.Job, _ error) {
	h.gaveUp = append(h.gaveUp, job.ID)
}

func TestKindsDispatchesByKind(t *testing.T) {
	deploys, backups := &kindHandler{}, &kindHandler{}
	k := Kinds{"deploy": deploys, "backup": backups}

	if err := k.Run(t.Context(), store.Job{ID: 1, Kind: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if err := k.Run(t.Context(), store.Job{ID: 2, Kind: "backup"}); err != nil {
		t.Fatal(err)
	}
	k.GiveUp(t.Context(), store.Job{ID: 3, Kind: "backup"}, errors.New("x"))
	if len(deploys.ran) != 1 || deploys.ran[0] != 1 || len(backups.ran) != 1 || backups.ran[0] != 2 ||
		len(backups.gaveUp) != 1 || len(deploys.gaveUp) != 0 {
		t.Errorf("deploys %+v, backups %+v", deploys, backups)
	}

	// An unknown kind fails at once, and giving up on it doesn't panic.
	if err := k.Run(t.Context(), store.Job{ID: 4, Kind: "mystery"}); !IsPermanent(err) {
		t.Errorf("unknown kind: error = %v, want permanent", err)
	}
	k.GiveUp(t.Context(), store.Job{ID: 4, Kind: "mystery"}, errors.New("x"))
}
