-- Public hostnames: an app can also be served on the internet, through
-- Cloudflare's tunnel, at <public_name>.<PUBLIC_DOMAIN>. Off (NULL) unless set:
-- every app is tailnet-only until it's made public.
--
-- Only the name in front is stored, not the full hostname, so the domain
-- comes from the platform's config and changing it carries every app over.
-- One DNS label (Cloudflare's free certificate covers one level of
-- subdomains), lowercase so UNIQUE can't be dodged with capitals. "hooks" is
-- the GitHub webhook's (hooks.<PUBLIC_DOMAIN>).
ALTER TABLE apps ADD COLUMN public_name text
    CONSTRAINT apps_public_name_key UNIQUE
    CONSTRAINT apps_public_name_check
        CHECK (public_name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$' AND public_name <> 'hooks');
