-- Rollbacks: a rollback is a new deployment that runs an earlier one's image
-- instead of building (see docs/decisions/platform.md). It goes queued ->
-- starting -> healthy (or failed) with no building stage, and when it goes
-- live, the deployment it replaces becomes rolled_back rather than
-- superseded.

-- The deployment whose image a rollback runs; NULL for a normal deploy.
-- Always one of the same app's deployments: the foreign key is on
-- (app_id, rollback_of), so the database itself refuses another app's.
ALTER TABLE deployments ADD CONSTRAINT deployments_app_id_id_key UNIQUE (app_id, id);
ALTER TABLE deployments ADD COLUMN rollback_of bigint;
ALTER TABLE deployments ADD CONSTRAINT deployments_rollback_of_fkey
    FOREIGN KEY (app_id, rollback_of) REFERENCES deployments (app_id, id);

-- A rollback knows its image from the start (it's never built), and isn't a
-- rollback of itself.
ALTER TABLE deployments ADD CONSTRAINT deployments_rollback_check
    CHECK (rollback_of IS NULL OR (image_tag IS NOT NULL AND rollback_of <> id));
