-- Backups of app databases: one row per backup, run as a job in the queue
-- (so it's retried, and survives the platform restarting). The dump itself
-- is a file in the backup directory (BACKUP_DIR, ~/backups on the server),
-- under the app's name; this row says how it went.
--
--   queued -> running -> done
--        \-----------\-> failed
CREATE TABLE backups (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_id bigint NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    status text NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'failed')),
    -- The dump's file name inside the app's folder, and its size, once done.
    file       text CHECK (file ~ '^[a-z0-9-]+-[0-9]{8}-[0-9]{6}\.dump$'),
    size_bytes bigint CHECK (size_bytes >= 0),
    -- Why it failed, in words safe to show in the dashboard.
    error text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz,
    CHECK ((status = 'done') = (file IS NOT NULL AND size_bytes IS NOT NULL))
);

-- An app's backups, newest first.
CREATE INDEX backups_app_id_idx ON backups (app_id, id DESC);

-- At most one backup in progress per app, enforced here rather than in Go:
-- a scheduled backup and "Back up now" racing can't both start.
CREATE UNIQUE INDEX backups_one_in_progress_per_app
    ON backups (app_id)
    WHERE status IN ('queued', 'running');

-- Jobs gain a second kind. Each kind's input is its own typed column with a
-- foreign key, as for deploys, and exactly the one its kind needs is set.
ALTER TABLE jobs ALTER COLUMN deployment_id DROP NOT NULL;
ALTER TABLE jobs ADD COLUMN backup_id bigint UNIQUE REFERENCES backups (id) ON DELETE CASCADE;
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('deploy', 'backup'));
ALTER TABLE jobs ADD CONSTRAINT jobs_input_check CHECK (
    (kind = 'deploy' AND deployment_id IS NOT NULL AND backup_id IS NULL) OR
    (kind = 'backup' AND backup_id IS NOT NULL AND deployment_id IS NULL)
);
