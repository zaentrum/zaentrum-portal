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
  ('chino',          'chino',              'movies & shows',    '/chino/',          'product', 'glyph:c', true),
  ('tv',             'tv',                 'live channels',     '',                 'product', 'glyph:t', false),
  ('musig',          'musig',              'music',             '',                 'product', 'glyph:m', false),
  ('katalog',        'Catalog',            'browse the catalog','/katalog/',        'manage',  'library', true),
  ('katalog-manage', 'Catalog Management', 'scan & settings',   '/katalog-manage/', 'manage',  'wrench',  true)
ON CONFLICT (key) DO NOTHING;

-- Tiles. A tile is an APP you open, not a deep link into an app's own nav:
-- chino/tv/musig each open their product in the "apps" space, and katalog gets
-- ONE tile in the "manage" space. The katalog app owns its internal sections
-- (catalog / scan / settings) as tabs — they are NOT launchpad tiles (that just
-- mirrors the app's nav and fragments it). See 003 for the removal of the old
-- scan/downloads sub-tiles from live registries.
--
-- The catalog tiles are for admins (audience: the admin role portal-api runs
-- with, see 001); the products are for everyone signed in. A seed tile an
-- admin deleted comes back on the next boot with that audience, not with
-- everyone's.
INSERT INTO tiles (key, app_key, space_key, title, description, icon, target, ord, badge, badge_tone, status, external, enabled, audience) VALUES
  ('chino.open',        'chino',   'apps',   'chino',     'movies & shows',     'glyph:c',  '',          10, 'ready', 'success', 'online',  false, true, '{}'::text[]),
  ('tv.open',           'tv',      'apps',   'tv',        'live channels',      'glyph:t',  '',          20, 'soon',  '',        'offline', false, false, '{}'::text[]),
  ('musig.open',           'musig',          'apps',   'musig',              'music',              'glyph:m', '', 30, 'soon',  '',     'offline', false, false, '{}'::text[]),
  ('katalog.catalog',      'katalog',        'manage', 'Catalog',            'browse the catalog', 'library', '', 10, 'admin', 'info', 'online',  false, true,
     ARRAY[coalesce(nullif(current_setting('zaentrum.admin_role', true), ''), 'zaentrum-admin')]),
  ('katalog-manage.open',  'katalog-manage', 'manage', 'Catalog Management', 'scan & settings',    'wrench',  '', 20, 'admin', 'info', 'online',  false, true,
     ARRAY[coalesce(nullif(current_setting('zaentrum.admin_role', true), ''), 'zaentrum-admin')])
ON CONFLICT (key) DO NOTHING;
