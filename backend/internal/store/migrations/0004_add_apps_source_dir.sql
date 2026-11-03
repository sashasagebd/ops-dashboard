-- The folder inside the repo that holds the app's Dockerfile and is sent to
-- Docker as the build context. '' is the repo's root. Lets one repo hold
-- several apps (a monorepo, or this repo's examples/hello).
--
-- A relative path without "." or ".." parts or a trailing slash, so it can
-- only ever select a folder inside the repo.
ALTER TABLE apps
    ADD COLUMN source_dir text NOT NULL DEFAULT ''
        CONSTRAINT apps_source_dir_check
        CHECK (source_dir ~ '^([A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*)?$'
               AND source_dir !~ '(^|/)\.\.?(/|$)');
