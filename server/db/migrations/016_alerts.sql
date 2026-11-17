-- Alerts: what is wrong with the platform, for its admins (R.C8).
--
-- portal-api reads the platform's resource every 60 seconds and keeps each of
-- the five alerts here: its kind, what it is about, its title and its text as
-- the status last wrote them, when it started and when it ended, and what was
-- mailed. One alert of a kind is active for one subject. The partial unique
-- index alerts_active holds that, so that where portal-api runs more than once
-- the write that starts an alert, and the one that ends it, decide which of
-- them mails it.
--
-- An alert is kept until 90 days after it ended; an active one is kept. The
-- table alert_seen holds which admin has seen which: an alert is unseen by an
-- admin until they have seen it since it last changed, when it started or
-- when it ended.
--
-- Idempotent: applied on every boot.

CREATE TABLE IF NOT EXISTS alerts (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind            text NOT NULL CHECK (kind IN ('part-not-ready', 'volume-full', 'certificate-ends', 'no-good-backup', 'update-failed')),
  about           text NOT NULL CHECK (about <> '' AND char_length(about) <= 255),
  title           text NOT NULL CHECK (title <> '' AND char_length(title) <= 80),
  body            text NOT NULL CHECK (body <> '' AND char_length(body) <= 280),
  -- What filled in the title and the text, as the routes answer it.
  vals            jsonb NOT NULL DEFAULT '{}'::jsonb,
  -- A path of the portal: the page that explains the alert.
  link            text NOT NULL DEFAULT '',
  -- What the rules keep of the alert between two checks; never answered.
  memo            jsonb NOT NULL DEFAULT '{}'::jsonb,
  started_at      timestamptz NOT NULL,
  ended_at        timestamptz,
  -- The sentence of its end, and what filled it in.
  end_text        text CHECK (end_text IS NULL OR (end_text <> '' AND char_length(end_text) <= 280)),
  end_vals        jsonb,
  -- The start mail and the end mail: each address, and what became of it.
  -- mail_next is when a try is due. mail_holder is the portal-api that holds
  -- the tries, until mail_lease: both are read and written by the database's
  -- clock, and every write of the mail names its holder.
  mail            jsonb NOT NULL DEFAULT '{}'::jsonb,
  mail_next       timestamptz,
  mail_holder     text,
  mail_lease      timestamptz,
  -- When the mail server first took the start mail for an address: the 24
  -- hours in which one start mail is sent count from it.
  start_mailed_at timestamptz,
  CHECK (ended_at IS NULL OR ended_at >= started_at),
  CHECK ((ended_at IS NULL) = (end_text IS NULL))
);

-- One active alert of a kind for one subject.
CREATE UNIQUE INDEX IF NOT EXISTS alerts_active ON alerts (kind, about) WHERE ended_at IS NULL;
-- The last start whose mail the mail server took, for a kind and a subject.
CREATE INDEX IF NOT EXISTS alerts_start_mailed ON alerts (kind, about, start_mailed_at) WHERE start_mailed_at IS NOT NULL;
-- The tries that are due.
CREATE INDEX IF NOT EXISTS alerts_mail_due ON alerts (mail_next) WHERE mail_next IS NOT NULL;
-- The sweep.
CREATE INDEX IF NOT EXISTS alerts_ended ON alerts (ended_at) WHERE ended_at IS NOT NULL;

-- Which admin has seen which alert, and when. The admin is the subject of
-- their token, as a notice's person is (015).
CREATE TABLE IF NOT EXISTS alert_seen (
  alert_id uuid NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  sub      text NOT NULL CHECK (length(sub) <= 255),
  seen_at  timestamptz NOT NULL,
  PRIMARY KEY (alert_id, sub)
);
