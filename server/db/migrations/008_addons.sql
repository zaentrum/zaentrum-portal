-- Installed addons and the workloads they consist of.
--
-- Until now "installed" was derived: an app that owned addon.<key> tiles or
-- slot rows. That cannot describe an addon with no UI, nor record which
-- workloads make up an addon. An addon is one primary component that serves
-- its manifest plus the components it declares; the platform records them so
-- the console can show their live state. It never stores the addon's
-- configuration — no column here holds a value an addon is configured with.
--
-- Labels on the workloads (zaentrum.io/addon, zaentrum.io/component) are
-- metadata for grouping only; the component rows below are what the platform
-- matches by, via workload = Deployment name.
--
-- Idempotent: applied on every boot.

CREATE TABLE IF NOT EXISTS addons (
  key             text PRIMARY KEY REFERENCES apps(key) ON DELETE CASCADE,
  address         text NOT NULL DEFAULT '',   -- in-cluster address the manifest was pulled from
  version         text NOT NULL DEFAULT '',
  manifest        jsonb,                      -- the descriptor as installed; NULL when backfilled
  manifest_sha256 text NOT NULL DEFAULT '',
  installed_at    timestamptz NOT NULL DEFAULT now(),
  refreshed_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS addon_components (
  addon_key text NOT NULL REFERENCES addons(key) ON DELETE CASCADE,
  name      text NOT NULL,
  workload  text NOT NULL,                    -- Deployment and Service name, one DNS label
  role      text NOT NULL CHECK (role IN ('primary', 'required', 'optional')),
  summary   text NOT NULL DEFAULT '',
  ord       int  NOT NULL DEFAULT 0,
  PRIMARY KEY (addon_key, name),
  UNIQUE (workload)                           -- one workload is never two addons' component
);

-- Backfill: addons installed before this table existed are the apps that own
-- an addon.<key> tile (or addon.<key>.<x>) or slot rows. Their address is the
-- app's proxy url. ON CONFLICT keeps every later refresh authoritative, and an
-- addon removed since is gone from apps, so nothing is resurrected.
INSERT INTO addons (key, address)
SELECT a.key, a.proxy_url
FROM apps a
WHERE EXISTS (
        SELECT 1 FROM tiles t
        WHERE t.key = 'addon.' || a.key
           OR left(t.key, length(a.key) + 7) = 'addon.' || a.key || '.')
   OR EXISTS (SELECT 1 FROM ui_extensions e WHERE e.addon = a.key)
ON CONFLICT (key) DO NOTHING;

-- Each backfilled addon is its implicit primary component: named by its key,
-- running as the workload its address names (the first DNS label of the host,
-- i.e. the Service name). Only for addons without a manifest and without
-- components yet, and only when the host is a valid label — an app with no
-- proxy url has no workload to point at.
INSERT INTO addon_components (addon_key, name, workload, role)
SELECT ad.key, ad.key, h.workload, 'primary'
FROM addons ad
CROSS JOIN LATERAL (
  SELECT lower(split_part(
           substring(ad.address FROM '^[A-Za-z][A-Za-z0-9+.-]*://(?:[^@/?#]*@)?([^:/?#]+)'),
           '.', 1)) AS workload
) h
WHERE ad.manifest IS NULL
  AND h.workload ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'
  AND length(h.workload) <= 63
  AND NOT EXISTS (SELECT 1 FROM addon_components c WHERE c.addon_key = ad.key)
ON CONFLICT DO NOTHING;
