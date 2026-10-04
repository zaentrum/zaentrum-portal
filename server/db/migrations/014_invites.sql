-- Invites: how a person added on the People page signs in the first time.
--
-- An admin adds a person — an account with no password — and shares a link
-- that carries a random token; on the invite page the person chooses their
-- own password. The token is 32 random bytes and never stored: a row keeps
-- its SHA-256, so the table, a backup or the database browser hands nobody a
-- link that works. A row says whose account it is for (the Keycloak user's
-- id), who made it, until when it holds (seven days unless
-- PORTAL_INVITE_TTL says otherwise), and when it was used or revoked. It is
-- used once: the password is set and used_at written in one transaction, the
-- row locked. A new invite for the same person revokes the ones still open;
-- so does switching them off, and deleting them removes their rows.
--
-- Idempotent: applied on every boot.

CREATE TABLE IF NOT EXISTS invites (
  id          bigserial PRIMARY KEY,
  token_hash  bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  user_id     text NOT NULL CHECK (user_id <> ''),
  created_by  text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  revoked_at  timestamptz
);

CREATE INDEX IF NOT EXISTS invites_user ON invites (user_id, created_at DESC);
