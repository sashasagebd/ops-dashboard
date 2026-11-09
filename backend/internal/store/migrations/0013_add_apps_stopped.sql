-- Stopping an app: its container is removed and its routes go, but the app,
-- its live deployment (the version Start runs again), its database and its
-- history stay. The reconciler leaves a stopped app's container stopped, and
-- no deploy or rollback can start until it's started again (the store checks
-- this under the app's row lock).
ALTER TABLE apps ADD COLUMN stopped boolean NOT NULL DEFAULT false;
