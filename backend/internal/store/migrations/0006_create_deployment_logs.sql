-- What happened during each deploy, for the dashboard's log viewer: the
-- platform's own stage messages ('platform') and Docker's build output
-- ('build'). App (runtime) output isn't stored; it's read live from Docker
-- (see docs/decisions/platform.md).
--
-- Bounded twice, by the platform (internal/deploy): build output is capped
-- per deployment, and only an app's most recent deployments keep their logs.
CREATE TABLE deployment_logs (
    -- Global and increasing, so it doubles as the log stream's resume point
    -- (Server-Sent Events' Last-Event-ID). Only one worker writes a
    -- deployment's log at a time (the job's lease), in order, so within a
    -- deployment, id order is the order lines were written.
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deployment_id bigint NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    at     timestamptz NOT NULL,
    stream text NOT NULL CHECK (stream IN ('platform', 'build')),
    line   text NOT NULL
);

-- A deployment's lines in order, from a given point.
CREATE INDEX deployment_logs_deployment_id_idx ON deployment_logs (deployment_id, id);
