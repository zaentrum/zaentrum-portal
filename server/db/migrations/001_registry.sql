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
