-- Core entries: the registry rows the platform stands on. The media app
-- (chino) is the product the launchpad exists to open; the apps and manage
-- spaces are where the seed and every addon place their tiles, and deleting
-- a space deletes every tile in it. None of them can be deleted — not from
-- the registry console, not by removing an addon that declared one as its
-- space — and an addon's manifest cannot retitle a core space. They can be
-- edited, and the app disabled.
--
-- core is the platform's to set, never the API's: no write path names it.
-- Idempotent: applied on every boot.

ALTER TABLE apps   ADD COLUMN IF NOT EXISTS core boolean NOT NULL DEFAULT false;
ALTER TABLE spaces ADD COLUMN IF NOT EXISTS core boolean NOT NULL DEFAULT false;

UPDATE apps   SET core = true WHERE key = 'chino' AND NOT core;
UPDATE spaces SET core = true WHERE key IN ('apps', 'manage') AND NOT core;
