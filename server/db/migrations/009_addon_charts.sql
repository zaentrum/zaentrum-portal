-- Addons installed from a Helm chart.
--
-- The zaentrum-operator installs such an addon from a ZaentrumAddon resource;
-- once it is ready, portal-api registers it from its primary Service exactly
-- like an addon added by address. The registry records which chart it came
-- from, so settings can tell the two apart and the registration loop can tell
-- a new chart version from one it already registered. Empty for an addon added
-- by address. Still no configuration value: the chart's values live in the
-- ZaentrumAddon and its values Secret, never here.
--
-- Idempotent: applied on every boot.

ALTER TABLE addons ADD COLUMN IF NOT EXISTS chart_ref     text NOT NULL DEFAULT '';
ALTER TABLE addons ADD COLUMN IF NOT EXISTS chart_version text NOT NULL DEFAULT '';
