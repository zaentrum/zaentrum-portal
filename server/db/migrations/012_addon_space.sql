-- The space an addon was installed into.
--
-- An addon's tiles go to the space the admin chose when adding it, unless its
-- manifest brings a space of its own. A refresh — the addon added again by its
-- address, a chart addon registered again after an upgrade — names no space,
-- and put them into whichever space came first: out of the one they were
-- installed into. The install records its space here, and a refresh reuses it
-- while that space exists.
--
-- An addon installed before this column gets the space its tiles are in, when
-- its manifest brings none of its own and its tiles are all in one space; any
-- other stays '' and a refresh puts its tiles in the first space, as before.
-- Idempotent: applied on every boot, and only an addon still without a space
-- is looked at.

ALTER TABLE addons ADD COLUMN IF NOT EXISTS space text NOT NULL DEFAULT '';

UPDATE addons ad SET space = owned.space_key
FROM (
  SELECT a.key, min(t.space_key) AS space_key
  FROM addons a
  JOIN tiles t ON t.key = 'addon.' || a.key OR left(t.key, length(a.key) + 7) = 'addon.' || a.key || '.'
  GROUP BY a.key
  HAVING count(DISTINCT t.space_key) = 1
) owned
WHERE ad.key = owned.key
  AND ad.space = ''
  AND btrim(coalesce(ad.manifest #>> '{ui,space,key}', '')) = '';
