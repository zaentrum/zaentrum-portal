-- First-run setup: that an admin marked it done.
--
-- The launchpad shows an admin the setup checklist until one marks it done:
-- the TMDB key, the library, the media pipeline, https for phones and TVs,
-- the people who use the server. Every step's state is read live from where
-- it is configured — the catalog, the operator's resource, the cluster — and
-- none of it is stored here. What is stored is the one thing nothing else
-- holds: that setup was marked done, when and by whom. One row per instance,
-- since the portal's database is the instance's; POST
-- /api/portal/setup/complete writes it, DELETE removes it and the checklist
-- shows again.
--
-- Idempotent: applied on every boot.

CREATE TABLE IF NOT EXISTS setup_completion (
  id           boolean PRIMARY KEY DEFAULT true CHECK (id), -- one row, at most
  completed_at timestamptz NOT NULL DEFAULT now(),
  completed_by text NOT NULL DEFAULT ''
);
