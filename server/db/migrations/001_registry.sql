-- Portal registry schema. Idempotent (CREATE ... IF NOT EXISTS) — applied on
-- every boot by the service (the demo Postgres is ephemeral).

CREATE TABLE IF NOT EXISTS spaces (
  key        text PRIMARY KEY,
  title      text NOT NULL,
  ord        int  NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS apps (
  key         text PRIMARY KEY,
  title       text NOT NULL,
  description text NOT NULL DEFAULT '',
  base_url    text NOT NULL DEFAULT '',
  kind        text NOT NULL DEFAULT 'tool',   -- product|manage|tool|external
  health_url  text NOT NULL DEFAULT '',
  icon        text NOT NULL DEFAULT '',
  enabled     boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tiles (
  key         text PRIMARY KEY,
  app_key     text NOT NULL REFERENCES apps(key) ON DELETE CASCADE,
  space_key   text NOT NULL REFERENCES spaces(key) ON DELETE CASCADE,
  title       text NOT NULL,
  description text NOT NULL DEFAULT '',
  icon        text NOT NULL DEFAULT '',
  target      text NOT NULL DEFAULT '',   -- path within the app, or an absolute url
  ord         int  NOT NULL DEFAULT 0,
  badge       text NOT NULL DEFAULT '',
  badge_tone  text NOT NULL DEFAULT '',
  status      text NOT NULL DEFAULT '',   -- online|offline|''
  external    boolean NOT NULL DEFAULT false,
  enabled     boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tiles_space_idx ON tiles(space_key);
CREATE INDEX IF NOT EXISTS tiles_app_idx ON tiles(app_key);

-- Audience: the realm roles that may see a space or a tile on the launchpad;
-- empty is everyone signed in. portal-api filters the launchpad by it.
--
-- The columns are added here, with the schema, because the seed (002, 004)
-- puts back missing seed rows on every boot and names their audience — they
-- must exist before it runs. When the tiles column arrives (once: the first
-- boot that has it) every tile that was an admin tile until then becomes
-- admin-only — the catalog tiles, any tile badged admin, any tile of a manage
-- app. Later boots never repeat that, so an admin who opens one of them to
-- everyone keeps the choice. The admin role is the one portal-api runs with:
-- the migration session sets zaentrum.admin_role (store.Migrate).
DO $$
DECLARE
  admin_role text := coalesce(nullif(current_setting('zaentrum.admin_role', true), ''), 'zaentrum-admin');
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                 WHERE table_schema = current_schema() AND table_name = 'tiles' AND column_name = 'audience') THEN
    ALTER TABLE tiles ADD COLUMN audience text[] NOT NULL DEFAULT '{}';
    UPDATE tiles t SET audience = ARRAY[admin_role]
     WHERE t.key IN ('katalog.catalog', 'katalog-manage.open')
        OR t.badge = 'admin'
        OR EXISTS (SELECT 1 FROM apps a WHERE a.key = t.app_key AND a.kind = 'manage');
  END IF;
END $$;

ALTER TABLE spaces ADD COLUMN IF NOT EXISTS audience text[] NOT NULL DEFAULT '{}';
