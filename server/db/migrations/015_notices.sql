-- Notices: what an addon tells one person — "your title is ready" — shown by
-- every client that person uses.
--
-- A notice is plain text from an installed addon to one person: a title (at
-- most 80 characters) and a body (at most 280), and optionally a link — held
-- to the rule a slot row's link is held to — and the id of a catalog item a
-- client can open. The core does not know what a notice says; it keeps it,
-- shows it to its person and forgets it.
--
-- The person is the subject of their token: the account's id in the realm,
-- as an invite names it (014). The platform trusts one issuer, so a subject
-- is a person. The addon is the key of an installed addon, and removing the
-- addon removes its notices with it: an uninstalled addon leaves nothing in
-- anyone's list either.
--
-- A person reads their own, marks them read and deletes them, and deleting
-- the person deletes theirs. A person keeps their newest 100: an older one
-- goes when a new one comes. Every notice goes after the retention — 90 days
-- unless PORTAL_NOTICE_RETENTION says otherwise — swept every hour.
--
-- Idempotent: applied on every boot.

CREATE TABLE IF NOT EXISTS notices (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  sub        text NOT NULL CHECK (sub <> '' AND length(sub) <= 255),
  addon      text NOT NULL REFERENCES addons(key) ON DELETE CASCADE,
  title      text NOT NULL CHECK (title <> '' AND char_length(title) <= 80),
  body       text NOT NULL CHECK (body <> '' AND char_length(body) <= 280),
  link       text NOT NULL DEFAULT '',
  item_id    text NOT NULL DEFAULT '' CHECK (length(item_id) <= 128),
  created_at timestamptz NOT NULL DEFAULT now(),
  read_at    timestamptz
);

-- A person's list, newest first, and their unread count.
CREATE INDEX IF NOT EXISTS notices_sub ON notices (sub, created_at DESC);
-- An addon's notices: removing it, and counting them.
CREATE INDEX IF NOT EXISTS notices_addon ON notices (addon);
-- The retention sweep.
CREATE INDEX IF NOT EXISTS notices_created ON notices (created_at);
