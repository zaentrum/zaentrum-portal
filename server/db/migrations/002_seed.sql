-- Seed the launchpad the portal shipped hardcoded, so a fresh registry renders
-- the same thing. Idempotent (ON CONFLICT DO NOTHING) — never overwrites edits
-- an admin has made via the settings console.

INSERT INTO spaces (key, title, ord) VALUES
  ('apps',   'apps',   10),
  ('manage', 'manage', 20)
ON CONFLICT (key) DO NOTHING;

-- Apps. Products (chino/tv/musig) + the katalog management app. tv/musig are
-- registered but disabled → their tiles render as "coming soon".
-- "glyph:x" icons are the >x brand marks; other icons are lucide names.
INSERT INTO apps (key, title, description, base_url, kind, icon, enabled) VALUES
  ('chino',   'chino',   'movies & shows',     '/chino/',   'product', 'glyph:c', true),
  ('tv',      'tv',      'live channels',      '',          'product', 'glyph:t', false),
  ('musig',   'musig',   'music',              '',          'product', 'glyph:m', false),
  ('katalog', 'katalog', 'catalog management', '/katalog/', 'manage',  'library', true)
ON CONFLICT (key) DO NOTHING;

-- Tiles. chino/tv/musig each open their product in the "apps" space; katalog is
-- ONE app with THREE tiles (catalog / scan / downloads) in the "manage" space —
-- the "an app can have many tiles" model.
INSERT INTO tiles (key, app_key, space_key, title, description, icon, target, ord, badge, badge_tone, status, external, enabled) VALUES
  ('chino.open',        'chino',   'apps',   'chino',     'movies & shows',     'glyph:c',  '',          10, 'ready', 'success', 'online',  false, true),
  ('tv.open',           'tv',      'apps',   'tv',        'live channels',      'glyph:t',  '',          20, 'soon',  '',        'offline', false, false),
  ('musig.open',        'musig',   'apps',   'musig',     'music',              'glyph:m',  '',          30, 'soon',  '',        'offline', false, false),
  ('katalog.catalog',   'katalog', 'manage', 'katalog',   'catalog management', 'library',  '',          10, 'admin', 'info',    'online',  false, true),
  ('katalog.scan',      'katalog', 'manage', 'scan',      'run a library scan', 'radar',    'scan',      20, '',      '',        'online',  false, true),
  ('katalog.downloads', 'katalog', 'manage', 'downloads', 'download activity',  'download', 'downloads', 30, '',      '',        'online',  false, true)
ON CONFLICT (key) DO NOTHING;
