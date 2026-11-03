-- Push-to-deploy: a push to an app's branch (GitHub's webhook) deploys the
-- pushed commit.

-- Whether pushes deploy the app. On by default; manual deploys work either
-- way.
ALTER TABLE apps ADD COLUMN auto_deploy boolean NOT NULL DEFAULT true;

-- A pushed commit waiting for the deploy in progress to finish (only one can
-- be in progress per app). The newest push replaces an older waiting one:
-- deploying the commit in between would only be replaced straight away.
ALTER TABLE apps ADD COLUMN next_commit_sha text
    CONSTRAINT apps_next_commit_sha_check CHECK (next_commit_sha ~ '^[0-9a-f]{40}$');

-- Webhook deliveries already handled, by GitHub's delivery ID
-- (X-GitHub-Delivery), so a redelivery (GitHub retrying, or someone pressing
-- "Redeliver") doesn't deploy again. Kept for 30 days.
CREATE TABLE webhook_deliveries (
    id          text PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 100),
    received_at timestamptz NOT NULL DEFAULT now()
);
