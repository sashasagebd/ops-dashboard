-- Background work the platform must not lose: a row here outlives the
-- process, so a deploy cut off by a crash or restart is picked up again
-- instead of being lost (see docs/decisions/platform.md). Workers claim jobs
-- with SELECT ... FOR UPDATE SKIP LOCKED (store/jobs.go).
--
--   queued -> running -> done
--     ^          |  \-> failed
--     \----------/  (retried after an error that may pass, or released at
--                    shutdown)
--
-- A running job holds a lease (locked_by, locked_until) that its worker keeps
-- renewing. If the worker dies, the lease runs out and the job can be
-- claimed again; locked_by stops the old worker from finishing a job someone
-- else now holds.
CREATE TABLE jobs (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Only deploys for now. Each kind's input is a typed column with a
    -- foreign key, rather than a JSON payload, so the database can check it.
    kind text NOT NULL CHECK (kind IN ('deploy')),
    -- One job per deployment, deleted with it (and so with its app).
    deployment_id bigint NOT NULL UNIQUE REFERENCES deployments (id) ON DELETE CASCADE,

    status text NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'failed')),
    -- Counted when the job is claimed, not when it fails, so a job whose
    -- worker keeps dying (e.g. a build that takes the platform down) still
    -- runs out of attempts.
    attempts  integer NOT NULL DEFAULT 0,
    run_after timestamptz NOT NULL DEFAULT now(), -- later than now() while backing off

    locked_by    text,
    locked_until timestamptz,
    CHECK ((status = 'running') = (locked_by IS NOT NULL AND locked_until IS NOT NULL)),

    -- The last attempt's error, for the log; what the dashboard shows is the
    -- deployment's own error.
    last_error  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

-- What workers look for: jobs that may be claimable (queued and due, or
-- running with a lease that may have run out). Finished jobs, which are
-- nearly all of them, aren't in it.
CREATE INDEX jobs_claimable_idx ON jobs (id) WHERE status IN ('queued', 'running');

-- Deployments still in progress from before this migration have no job, and
-- nothing would ever finish them. (P1 marked them failed at startup; that
-- code is gone now that jobs resume them.)
UPDATE deployments
SET status = 'failed', error = 'interrupted: the platform restarted during this deploy', finished_at = now()
WHERE status IN ('queued', 'building', 'starting');
