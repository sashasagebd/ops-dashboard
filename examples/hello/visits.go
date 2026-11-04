package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// visits counts requests to /visits in the app's database, if the platform
// gave it one (DATABASE_URL). It's the smallest proof that the database
// works and keeps its data: across deploys, a recreated database container,
// and a backup restored.
type visits struct {
	db    *pgxpool.Pool // nil without a database
	ready atomic.Bool   // its table exists
}

// newVisits connects to DATABASE_URL, if set, and makes the table in the
// background, retrying until the database answers: it may still be starting
// when the app does, and the app's health check doesn't wait for it.
func newVisits(ctx context.Context) *visits {
	v := &visits{}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return v
	}
	pool, err := pgxpool.New(ctx, url) // connects lazily
	if err != nil {
		slog.Error("DATABASE_URL", "err", err) // never the URL: it has the password
		return v
	}
	v.db = pool
	go func() {
		for ctx.Err() == nil {
			_, err := pool.Exec(ctx, `
				CREATE TABLE IF NOT EXISTS visits (id int PRIMARY KEY, count bigint NOT NULL);
				INSERT INTO visits VALUES (1, 0) ON CONFLICT DO NOTHING`)
			if err == nil {
				v.ready.Store(true)
				slog.Info("database ready")
				return
			}
			slog.Warn("database not ready yet; retrying in 2s", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return v
}

// ServeHTTP counts a visit and says how many there have been.
func (v *visits) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case v.db == nil:
		http.Error(w, "no database: DATABASE_URL isn't set", http.StatusNotFound)
		return
	case !v.ready.Load():
		http.Error(w, "the database isn't ready yet", http.StatusServiceUnavailable)
		return
	}
	var n int64
	err := v.db.QueryRow(r.Context(), "UPDATE visits SET count = count + 1 WHERE id = 1 RETURNING count").Scan(&n)
	if err != nil {
		// The table may have gone (a restore in progress, say).
		slog.Error("counting a visit", "err", err)
		http.Error(w, "couldn't count the visit", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "Visits: %d\n", n)
}
