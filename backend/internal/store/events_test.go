package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestEvents(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	hello := createTestApp(t, s, "hello")
	api := createTestApp(t, s, "api")

	mustDo(t, s.AddEvent(ctx, NewEvent{AppID: hello.ID, Kind: EventRestarted, Message: "restarted hello: exited (code 137)",
		Output: []string{"listening", "panic: nil map"}}))
	mustDo(t, s.AddEvent(ctx, NewEvent{Kind: EventRoutesReapplied, Message: "re-applied routes"}))
	mustDo(t, s.AddEvent(ctx, NewEvent{AppID: api.ID, Kind: EventRemovedOrphan, Message: "removed app-api-3"}))

	all, err := s.Events(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []EventKind
	for _, e := range all {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, []EventKind{EventRemovedOrphan, EventRoutesReapplied, EventRestarted}) {
		t.Errorf("all events = %v, want newest first", kinds)
	}
	// Not about one app: app 0, and no output is [], not nil.
	if all[1].AppID != 0 || all[1].Output == nil || len(all[1].Output) != 0 {
		t.Errorf("routes event = %+v, want app 0 and empty output", all[1])
	}

	mine, err := s.Events(ctx, hello.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].AppID != hello.ID || !slices.Equal(mine[0].Output, []string{"listening", "panic: nil map"}) ||
		mine[0].At.IsZero() {
		t.Errorf("hello's events = %+v, want the restart with its output", mine)
	}
	if got, _ := s.Events(ctx, 0, 1); len(got) != 1 {
		t.Errorf("limit 1 gave %d events", len(got))
	}

	// Kinds are checked by the database.
	if err := s.AddEvent(ctx, NewEvent{Kind: "exploded", Message: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown kind: error = %v, want ErrInvalid", err)
	}

	// Old ones go.
	if _, err := s.pool.Exec(ctx, "UPDATE reconciler_events SET at = now() - interval '31 days' WHERE app_id = $1", hello.ID); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneEvents(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Errorf("PruneEvents = %d, %v; want 1 pruned", n, err)
	}
	if left, _ := s.Events(ctx, 0, 10); len(left) != 2 {
		t.Errorf("%d events left, want 2", len(left))
	}
}
