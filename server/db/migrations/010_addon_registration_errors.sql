-- Why a chart addon could not be registered.
--
-- The registration loop runs in every portal-api replica. Its verdict is
-- recorded here, so whichever replica answers a request reports the same
-- thing, and it survives a restart. A row goes when the addon registers or is
-- removed. It holds a message, never a value the addon is configured with.
--
-- Idempotent: applied on every boot.

CREATE TABLE IF NOT EXISTS addon_registration_errors (
  name       text PRIMARY KEY,               -- the ZaentrumAddon's name
  error      text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
