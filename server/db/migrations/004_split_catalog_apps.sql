-- Split the catalog console into TWO distinct launchpad apps:
--   • Catalog            (app 'katalog', /katalog/)        — browse: item list + detail
--   • Catalog Management (app 'katalog-manage', /katalog-manage/) — scan + settings
-- Both are admin apps in the 'manage' space; each is its own deployment of the
-- katalog image at its own mount path (see the platform chart). Runs after 002
-- each boot; idempotent.

-- The management app + its tile (no-op once present).
INSERT INTO apps (key, title, description, base_url, kind, icon, enabled) VALUES
  ('katalog-manage', 'Catalog Management', 'scan & settings', '/katalog-manage/', 'manage', 'wrench', true)
ON CONFLICT (key) DO NOTHING;

INSERT INTO tiles (key, app_key, space_key, title, description, icon, target, ord, badge, badge_tone, status, external, enabled) VALUES
  ('katalog-manage.open', 'katalog-manage', 'manage', 'Catalog Management', 'scan & settings', 'wrench', '', 20, 'admin', 'info', 'online', false, true)
ON CONFLICT (key) DO NOTHING;

-- Relabel the existing catalog app/tile to 'Catalog' (browse only now). Guarded on
-- the old default so an admin's own rename via the settings console is not clobbered.
UPDATE apps  SET title = 'Catalog', description = 'browse the catalog'
  WHERE key = 'katalog' AND title = 'katalog';
UPDATE tiles SET title = 'Catalog', description = 'browse the catalog'
  WHERE key = 'katalog.catalog' AND title = 'katalog';
