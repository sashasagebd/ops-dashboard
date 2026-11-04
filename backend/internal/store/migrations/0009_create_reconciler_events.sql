-- What the reconciler (internal/reconcile) found wrong and did about it:
-- restarting a crashed app, recreating a deleted container, removing an
-- orphan, re-applying routes Caddy had lost. Shown in the dashboard. Kept
-- for 30 days (the reconciler prunes them).
CREATE TABLE reconciler_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    -- NULL for an event that isn't about one app (routes, or a container of
    -- an app that no longer exists). Deleting an app deletes its events.
    app_id bigint REFERENCES apps (id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN (
        'restarted',        -- a stopped live container started again
        'recreated',        -- a missing live container created again
        'crash_looping',    -- restarts backing off: it keeps stopping
        'removed_orphan',   -- a container no live deployment owns, removed
        'routes_reapplied', -- Caddy's routes differed from the database's
        'fix_failed'        -- one of the above, tried and failed
    )),
    message text NOT NULL,
    -- For a crash: the container's last lines of output, oldest first, so
    -- the dashboard can show why it stopped. Capped by the reconciler.
    output text[] NOT NULL DEFAULT '{}'
);

-- Newest first, for one app or all; and old ones, for pruning.
CREATE INDEX reconciler_events_app_id_idx ON reconciler_events (app_id, id DESC);
CREATE INDEX reconciler_events_at_idx ON reconciler_events (at);
