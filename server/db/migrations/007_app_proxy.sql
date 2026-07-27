-- Embedded apps: the in-cluster address the portal proxies to.
--
-- base_url is the PUBLIC address a tile links out to. Embedding needs a
-- different thing: an internal service URL the portal-api can reach and forward
-- the caller's identity to. Keeping it a separate, explicitly-set column means a
-- registry row can never turn the proxy into an open relay by accident — an
-- app is embeddable only when an operator deliberately gives it a proxy target.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS proxy_url text NOT NULL DEFAULT '';
