-- UI extension contributions: the neutral seam that lets an ADDON surface native
-- action/link buttons inside product apps (e.g. chino's search-empty slot) and
-- register its own portal app/tiles — without the core knowing what the addon
-- does. A row is one contribution to a named slot; the product SPA fetches the
-- enabled rows for a slot and renders native buttons. Zero rows => zero UI, so an
-- uninstalled addon leaves no trace. Idempotent (applied on every boot).
CREATE TABLE IF NOT EXISTS ui_extensions (
  key        text PRIMARY KEY,
  addon      text NOT NULL DEFAULT '',      -- owning addon id (for bulk uninstall)
  slot       text NOT NULL,                 -- e.g. 'search.empty', 'item.detail.actions'
  kind       text NOT NULL DEFAULT 'link',  -- link (navigate) | action (POST url)
  label      text NOT NULL DEFAULT '',
  icon       text NOT NULL DEFAULT '',      -- lucide name (client falls back if unknown)
  url        text NOT NULL DEFAULT '',      -- destination; {q} etc. substituted client-side
  method     text NOT NULL DEFAULT 'POST',  -- for kind=action
  status_url text NOT NULL DEFAULT '',      -- optional: live status feed for the button
  ord        int  NOT NULL DEFAULT 0,
  enabled    boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ui_extensions_slot_idx ON ui_extensions(slot);
