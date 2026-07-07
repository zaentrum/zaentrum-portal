-- Collapse the katalog launchpad tiles to one. The katalog app's own tabs
-- (catalog / scan / settings) are the app's internal navigation, not launchpad
-- entry points — surfacing scan/downloads as sibling tiles duplicated that nav.
-- Downloads is removed entirely (tile + the app's Downloads view).
--
-- Runs on every boot after 002 (which no longer seeds these); idempotent — a
-- DELETE of absent rows is a no-op. Only the katalog.catalog tile remains.
DELETE FROM tiles WHERE key IN ('katalog.scan', 'katalog.downloads');
