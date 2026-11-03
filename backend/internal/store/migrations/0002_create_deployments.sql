-- One row per attempt to deploy a commit of an app. A deployment moves
-- forward only:
--
--   queued -> building -> starting -> healthy -> superseded / rolled_back
--        \-------------\-----------\-> failed
--
-- (rolled_back is used from P3.) The store's methods check the current
-- status on every change, so a deployment can't skip a stage or come back
-- from failed.
CREATE TABLE deployments (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Deleting an app deletes its history too.
    app_id bigint NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    -- A full commit SHA, never a branch name: a deployment always says
    -- exactly which code it ran.
    commit_sha text NOT NULL
        CHECK (commit_sha ~ '^[0-9a-f]{40}$'),

    status text NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'building', 'starting', 'healthy',
                          'failed', 'superseded', 'rolled_back')),

    -- Set once the image is built; P3's rollback reuses it with no rebuild.
    image_tag text,
    -- Why a failed deployment failed, in words safe to show in the dashboard.
    error     text,

    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz, -- left queued
    finished_at timestamptz  -- became healthy or failed
);

-- An app's deployments, newest first.
CREATE INDEX deployments_app_id_idx ON deployments (app_id, id DESC);

-- At most one deploy in progress per app, enforced by the database rather
-- than by Go code: two requests (or, from P2, two workers) racing to deploy
-- the same app can't both succeed, even if a bug lets both through.
CREATE UNIQUE INDEX deployments_one_in_progress_per_app
    ON deployments (app_id)
    WHERE status IN ('queued', 'building', 'starting');

-- At most one live deployment per app: the one its route points at.
-- Marking a new deployment healthy supersedes the old one in the same
-- transaction.
CREATE UNIQUE INDEX deployments_one_healthy_per_app
    ON deployments (app_id)
    WHERE status = 'healthy';
