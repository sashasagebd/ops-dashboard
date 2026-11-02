-- Apps registered on the platform: where the code is and how to run it.
-- Env vars, deployments and jobs get their own tables in later migrations.
--
-- The constraints here are the last line of defence; the API validates input
-- first and gives friendlier errors.
CREATE TABLE apps (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- Used in container names, image tags and Docker labels, so it's kept to
    -- a DNS-label-like shape: lowercase letters, digits and inner hyphens,
    -- starting with a letter, at most 32 characters.
    name text NOT NULL UNIQUE
        CHECK (name ~ '^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$'),

    repo_url text NOT NULL,
    branch   text NOT NULL DEFAULT 'main',

    -- The full public hostname, e.g. myapp.example.com. Lowercase only, so
    -- UNIQUE can't be dodged with different capitalisation.
    hostname text NOT NULL UNIQUE
        CHECK (hostname <> '' AND hostname = lower(hostname)),

    -- The port the app listens on inside its container.
    port integer NOT NULL
        CHECK (port BETWEEN 1 AND 65535),

    health_check_path text NOT NULL DEFAULT '/'
        CHECK (health_check_path LIKE '/%'),

    -- Every app has limits (NOT NULL, with defaults), so nothing deployed can
    -- starve the game servers. Memory defaults to 256 MiB; Docker refuses
    -- limits below 6 MiB. CPU is in thousandths of a core (500 = half a
    -- core), converted to Docker's NanoCPUs when the container is created.
    memory_limit_bytes bigint NOT NULL DEFAULT 268435456
        CHECK (memory_limit_bytes >= 6291456),
    cpu_limit_millicores integer NOT NULL DEFAULT 500
        CHECK (cpu_limit_millicores > 0),

    created_at timestamptz NOT NULL DEFAULT now()
);
