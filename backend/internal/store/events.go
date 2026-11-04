package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventKind is what the reconciler did (see the
// 0009_create_reconciler_events migration).
type EventKind string

const (
	EventRestarted       EventKind = "restarted"
	EventRecreated       EventKind = "recreated"
	EventCrashLooping    EventKind = "crash_looping"
	EventRemovedOrphan   EventKind = "removed_orphan"
	EventRoutesReapplied EventKind = "routes_reapplied"
	EventFixFailed       EventKind = "fix_failed"
	EventDatabaseCreated EventKind = "database_created"
)

// Event is one thing the reconciler found and fixed (or failed to).
type Event struct {
	ID      int64     `db:"id"`
	At      time.Time `db:"at"`
	AppID   int64     `db:"app_id"` // 0 if it isn't about one app
	Kind    EventKind `db:"kind"`
	Message string    `db:"message"` // shown in the dashboard: no secrets or internal detail
	Output  []string  `db:"output"`  // a crashed container's last lines, oldest first
}

// NewEvent is what AddEvent records.
type NewEvent struct {
	AppID   int64 // 0 if it isn't about one app
	Kind    EventKind
	Message string
	Output  []string
}

// AddEvent records an event.
func (s *Store) AddEvent(ctx context.Context, e NewEvent) error {
	var appID *int64
	if e.AppID != 0 {
		appID = &e.AppID
	}
	output := e.Output
	if output == nil {
		output = []string{}
	}
	_, err := s.pool.Exec(ctx,
		"INSERT INTO reconciler_events (app_id, kind, message, output) VALUES ($1, $2, $3, $4)",
		appID, string(e.Kind), e.Message, output)
	return translate(err)
}

// Events returns the most recent events, newest first: one app's if appID
// isn't 0, otherwise every app's (and those about no app).
func (s *Store) Events(ctx context.Context, appID int64, limit int) ([]Event, error) {
	const cols = "id, at, COALESCE(app_id, 0) AS app_id, kind, message, output"
	var rows pgx.Rows
	if appID != 0 {
		rows, _ = s.pool.Query(ctx,
			"SELECT "+cols+" FROM reconciler_events WHERE app_id = $1 ORDER BY id DESC LIMIT $2", appID, limit)
	} else {
		rows, _ = s.pool.Query(ctx,
			"SELECT "+cols+" FROM reconciler_events ORDER BY id DESC LIMIT $1", limit)
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByName[Event])
	return events, translate(err)
}

// PruneEvents deletes events older than before, returning how many.
func (s *Store) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM reconciler_events WHERE at < $1", before)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}
