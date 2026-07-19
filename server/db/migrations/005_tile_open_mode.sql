-- Tile open mode: how a tile launches its destination — 'inline' (same tab) or
-- 'newtab'. The column default is '' (unset): render resolves '' to newtab when
-- the legacy external flag is set, else inline. Because migrations re-run every
-- boot, the backfills below are guarded on open_mode='' so an admin's explicit
-- choice (always written as 'inline'/'newtab' by the settings console) is never
-- clobbered.
ALTER TABLE tiles ADD COLUMN IF NOT EXISTS open_mode TEXT NOT NULL DEFAULT '';

-- Products open in their own tab: chino is a full app of its own — launching it
-- should not navigate the portal away.
UPDATE tiles SET open_mode = 'newtab' WHERE key = 'chino.open' AND open_mode = '';

-- Legacy external tiles already meant "new tab" — make that explicit.
UPDATE tiles SET open_mode = 'newtab' WHERE external AND open_mode = '';
