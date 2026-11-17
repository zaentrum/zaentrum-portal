# zaentrum-portal

the launchpad shell for **zaentrum**, a neutral self-hosted media platform. you log in
once and land on a portal of spaces and tiles; each product (chino for video; tv and musig
planned) is an app you launch from a tile. bring your own server.

built with react + vite + [`@nalet/design-system`](https://github.com/nalet/design-system)
(square, dark, terminal aesthetic). single-page app, served under a configurable base path,
authenticating against any OIDC issuer it discovers at runtime.

## how it works

- **auth** — on load the app fetches `GET /api/config` for the OIDC issuer + web client id,
  then runs an authorization-code + PKCE flow against that issuer (`src/auth/oidc.ts`).
  point it at your own keycloak / identity provider; nothing is baked in at build time.
- **base path** — the SPA is built with a Vite `base` (default `/portal/`). the bundled
  nginx serves it there and 302s the bare host to the launchpad.
- **tiles** — products are siblings on the same origin. launching a tile full-page-navigates
  to that app, which re-uses the shared session (SSO) — no second login.

## people

one account per person. an admin adds the people who use the server on the **People**
page (`/portal/people`), gives a child a rating cap, and sends each an invite link — by
hand, or by mail to the person's address where mail is ready; the person opens it — no
sign-in, they have nothing to sign in with yet — chooses their own password, and signs in
on every app with their username. portal-api does it through the
platform realm's `zaentrum-people` client, which may view, query and manage the realm's
users and nothing else (the platform chart's realm Job keeps it so). with an external
identity provider people live there: the page says so and links to it.

| route | who | what |
|---|---|---|
| `GET /api/portal/people` | admin | everyone (the platform's own accounts aside): `email` (none for an address at the placeholder domain `zaentrum.local`), role, rating cap, enabled, created, their newest invite's state — never its token; `mode` bundled, external (`manageUrl`) or unavailable (`note`) |
| `POST /api/portal/people` | admin | `{username, displayName, email, role, maxRating, sendInvite}`: an enabled account with no password and the address, if one is given, `zaentrum-user` (+ `zaentrum-admin`); answers the person and their invite link, once, and with `sendInvite` true `mail`: `sent`, `not-set-up`, `no-address` or `failed`, with `mailError` when it failed |
| `PATCH /api/portal/people/{id}` | admin | `displayName`, `email` (empty: the address removed), `role`, `maxRating` (`null`: no cap), `enabled`; switched off, the open invite is revoked |
| `DELETE /api/portal/people/{id}` | admin | chino-api deletes the person's data first, then the account goes, then their invites |
| `POST /api/portal/people/{id}/invite` | admin | `{send}`, the body optional: a new link, and older ones stop working; with `send` true, `mail` and `mailError` as for `POST /api/portal/people` |
| `GET /api/portal/invites/{token}` | anyone with the link | `{valid, username, displayName, expiresAt, passwordPolicy}` while it works |
| `POST /api/portal/invites/{token}` | anyone with the link | `{password}`: set (not temporary), required actions cleared, the invite used — once; answers where to sign in |
| `DELETE /api/portal/me` | chino-api | the signed-in person's own account (see below) |

"admin" is the rule of every console: the admin role on a token of the portal's own
clients. the rules hold whoever asks: the last enabled admin is never demoted, switched
off or deleted; nobody changes their own role or switches themselves off; an account
that holds a `realm-management` role — the first admin — is changed in Keycloak's admin
console, not here (the people client could reset its password; the page does not); a
rating cap is for a user. a rating cap is the user attribute `max_rating`, an age from 0
to 21, which the viewers' access tokens carry as the integer claim `max_rating`; no
claim, no cap.

**addresses.** a person's address is their account's in the identity provider, optional,
and one address belongs to one account: portal-api refuses one that another account of the
realm has, the platform's own accounts' too, by looking at the realm before it writes.
nothing holds the address between that look and the write; there the realm's own refusal
is the guard, where the realm lets no two users have one address, as the bundled realm
does. Keycloak's refusal of another user's address or username is read by its words, and a
refusal in other words by looking again at who has which. an address at `zaentrum.local`,
the placeholder the realm's import gives the first admin, is no address: it is refused
where it is typed, the routes answer its account without one, and nothing is mailed to
it. an account that administers Keycloak gets its address in Keycloak's console.

**the invite by mail.** with `sendInvite` or `send`, portal-api mails the person their
link while the admin waits, through the mail settings (see mail); the answer still carries
the link. the link in the mail starts with the public address, `PORTAL_PUBLIC_URL`, alone,
never the address the admin reached the portal on. `mail` is `not-set-up` while mail is
not set up, `no-address` for a person without one, and `failed` with `mailError` — the
reason as a refusal of the mail routes: `mail_not_ready`, `mail_password_missing`, a mail
server's code, or, where the platform's resource cannot be read, `platform_read_forbidden`
or `cluster_unexpected_answer` with the cluster's words in `detail`.

| code | status | route | the case |
|---|---|---|---|
| `address_invalid` | 400 | `POST`, `PATCH` | `email` is not one address, or is one the identity provider declines |
| `address_unsupported` | 400 | `POST`, `PATCH` | `email` has accented or non-Latin letters |
| `address_placeholder` | 400 | `POST`, `PATCH` | `email` is at the placeholder domain `zaentrum.local` |
| `address_taken` | 409 | `POST`, `PATCH` | another account has the address |

a refusal with a code is as the mail routes' are; the people routes' other refusals are
their sentence as plain text.

**invites.** a token is 32 random bytes (base64url); portal-api stores only its SHA-256
(migration 014), with whose account it is for, who made it, until when it holds (7 days,
`PORTAL_INVITE_TTL`) and when it was used. it works once: the password is set and the
invite marked used in one transaction. every link that does not work — unknown, used,
expired, replaced, of an account gone or switched off — gets the same answer. the two
public routes are limited per client address and per token (`429`, `Retry-After`). a
password the realm's policy refuses is answered in words, and the link keeps working.

**deleting an account.** a person deletes their own from the apps: chino-api's
`DELETE /api/v1/me` deletes their data and calls `DELETE /api/portal/me` with the
person's bearer and the account deletion token (`X-Account-Deletion-Token`,
`PORTAL_ACCOUNT_DELETION_TOKEN`), and only commits once the account is gone. the account
deleted is the bearer's own; a service account, the last admin and a Keycloak
administrator are refused. an admin's delete on the People page asks chino-api
(`PORTAL_CHINO_API_URL`) for the person's data first, with the same token.

configuration: `PORTAL_PEOPLE_KEYCLOAK_URL` (in-cluster, e.g. `http://keycloak:80/auth`),
`PORTAL_PEOPLE_REALM`, `PORTAL_PEOPLE_CLIENT_ID`, `PORTAL_PEOPLE_CLIENT_SECRET`,
`PORTAL_PEOPLE_HIDDEN` (the platform's own accounts), `PORTAL_PEOPLE_MANAGE_URL` (external),
`PORTAL_USER_ROLE`, `PORTAL_PASSWORD_POLICY`, `PORTAL_INVITE_TTL`,
`PORTAL_ACCOUNT_DELETION_TOKEN`, `PORTAL_CHINO_API_URL`. the platform chart sets them with
bundled identity.

## notices

an addon can tell one person something — "your title is ready" — and every client that
person uses shows it: the bell in the portal's header, and the product apps through
chino-api (`GET /api/v1/notices`). the core does not know what a notice says: the text is
the addon's, plain text, and portal-api keeps it for its person (migration 015), shows it to
them and to nobody else, and forgets it.

| route | who | what |
|---|---|---|
| `POST /api/portal/notices` | an installed addon's service account | `{sub, title, body, link?, itemId?}`: a notice for the person whose token subject is `sub`, from that addon — the token binds the addon, and a body that names one is refused |
| `POST /api/portal/me/notices` | a person | `{addon, title, body, link?, itemId?}`: a notice for themselves, from an installed addon they name — for an addon that holds no credential; it reaches nobody else |
| `GET /api/portal/me/notices` | a person | `{notices, unread}`: their own, newest first, each with the addon's `addonTitle` and `addonIcon` — never whom it is for |
| `POST /api/portal/me/notices/{id}/read` | a person | one of theirs, read: `{unread}` |
| `POST /api/portal/me/notices/read-all` | a person | all of theirs, read: `{read, unread}` |
| `DELETE /api/portal/me/notices/{id}` | a person | one of theirs, deleted (`204`) |
| `GET /api/portal/notices?addon=` | admin | each addon's notices counted — `notices`, `unread`, `people`, `latest` — with `kept` and `retentionHours`; never what one says |

the rules hold whoever asks. a title is at most 80 characters and a body at most 280 (line
breaks allowed there), plain text without control or bidirectional formatting characters;
an item id is a short id (letters, digits and `. _ : -`, at most 128). the link is held to
the rule a slot row's link is — a path on this instance or an absolute http(s) URL on its
own origin, never `javascript:`, another host or `//host` — and a path is made absolute on
the instance's public origin when portal-api knows it. someone else's notice is `404`, as
one there is not. posting is limited per addon (100 at once, then one a second) and per
person posting to themselves (10, then one every six seconds): `429` with `Retry-After`. a
person keeps their newest 100, and every notice goes after `PORTAL_NOTICE_RETENTION`
(default `2160h`, 90 days), swept at boot and every hour. removing an addon removes its
notices; deleting a person removes theirs. every write is logged — who, and which notice —
and what a notice says never is.

a notice to oneself proves no addon sent it: whoever holds a person's bearer may name any
installed addon, and it reaches that person only. an addon that tells someone something
later — when their title is ready — posts with its service account (`zaentrum-addon`, the
client named after the addon), whose token binds the addon.

## mail

portal-api sends the platform's own mails, the test mail so far, through the SMTP server
an admin sets with the routes below. the settings are the platform resource's
`spec.mail`, which the operator gives portal-api as `PORTAL_MAIL_*`; a new
password goes alone into a new Secret, which portal-api writes and never reads. a link in
a mail and the platform's name in it come from the public address, `PORTAL_PUBLIC_URL`,
alone: without a full one, http or https and a host, no mail is sent, and with a user none
is sent while portal-api runs without its password. a mail is plain text in UTF-8 to one
address, over STARTTLS, TLS or none, the certificate checked; its headers are ASCII,
folded at 78 characters where they can be. portal-api signs in only with a user. an
address is one bare address of at most 254 characters, its domain a host name of at least
two parts, in ASCII letters. no setting takes a control character. its log says that a
mail of a kind was sent or refused, never an address and never the text.

| route | who | what |
|---|---|---|
| `GET /api/portal/mail` | admin | the settings without the password, `passwordSet`, `setUp`, `ready`, `secretMissing` (the condition `Mail` says the password's Secret is missing), `runByOperator` (false where the operator does not run the platform), `identity` (bundled or external), `alertMail`, and `recipients`: each admin's name and address, and the further addresses |
| `PUT /api/portal/mail` | admin | `{host, port, encryption, from, fromName, user, password, alertsTo}` into `spec.mail`, a password left out keeping the one kept and a port left out the encryption's default; checked first in a dry run, and written, with strict field validation (`fieldValidation=Strict`), so that a cluster that does not know `spec.mail` refuses it; answers `{generation}` |
| `DELETE /api/portal/mail` | admin | `spec.mail` removed; answers `{generation}` |
| `POST /api/portal/mail/test` | admin | `{to}`: the test mail, sent while the admin waits; answers `{to, message}` |

a refusal is `{"error": code, "values": {…}, "message": "…", "detail": "…"}`, as ADR-0023
part 4 has it: `error` names the case, and a code has one sentence; `values` are what its
sentence names, numbers and names as they are; `message` is the sentence a page shows until
the portal takes it from its message catalogue, and `detail` what the cluster said, for a
page's details and never its message. `values` and `detail` are there only where there is
something in them. a code the mail page words its own way starts with `mail_`; one worded
the same on every page is shared.

| code | status | route | the case |
|---|---|---|---|
| `mail_server_missing` | 400 | `PUT` | no server |
| `mail_server_invalid` | 400 | `PUT` | a server that is no host name or IP address, or not text |
| `mail_port_invalid` | 400 | `PUT` | a port that is no whole number from 1 to 65535: 0, a fraction, a number as text |
| `mail_encryption_invalid` | 400 | `PUT` | an encryption other than `starttls`, `tls` or `none` |
| `mail_sender_missing` | 400 | `PUT` | no sender |
| `mail_sender_invalid` | 400 | `PUT` | a sender that is not one address |
| `mail_sender_name_too_long` | 400 | `PUT` | a sender's name over 60 characters, or not text |
| `mail_further_address_invalid` | 400 | `PUT` | a further address that is no address, or `alertsTo` that is no list of text |
| `mail_further_address_limit` | 400 | `PUT` | more than 10 further addresses |
| `mail_user_needs_password` | 400 | `PUT` | a user, and no password typed or kept |
| `mail_value_invalid` | 400 | `PUT` | a value of the wrong kind in a field with no code of its own, `user` or `password`, in `values.field` |
| `mail_control_character` | 400 | `PUT` | a control character in the server, the sender, the sender's name, the user, the password or a further address |
| `address_unsupported` | 400 | `PUT`, test | an address with accented or non-Latin letters: the sender, a further address, `to` |
| `mail_not_run_by_operator` | 409 | `PUT`, `DELETE` | the platform has no resource |
| `mail_settings_unknown` | 503 | `PUT` | the cluster does not know `spec.mail`, as under an operator from before mail settings: nothing is written and no Secret made |
| `platform_read_forbidden` | 503 | all four | the cluster does not let portal-api read the resource |
| `mail_write_forbidden` | 503 | `PUT`, `DELETE` | the cluster does not let portal-api change it |
| `mail_write_refused` | 422 | `PUT`, `DELETE` | the cluster refuses the settings, a field of `spec.mail` its schema does not know among them |
| `cluster_unexpected_answer` | 502 | all four | any other answer of the cluster |
| `address_invalid` | 400 | test | `to` is no address |
| `address_placeholder` | 400 | `PUT`, test | a placeholder address, at `zaentrum.local`: the sender, a further address, `to` |
| `mail_not_set_up` | 409 | test | mail is not set up |
| `mail_not_ready` | 409 | test | set up, and not ready yet: the settings are being applied, or portal-api has no full public address |
| `mail_password_missing` | 409 | test | the operator says the password's Secret is missing, or portal-api runs with a user and no password |
| `mail_server_unreachable` | 502 | test | no connection to the mail server, or it broke |
| `mail_encryption_not_offered` | 502 | test | the server does not offer the encryption chosen |
| `mail_certificate_unverified` | 502 | test | the server's certificate did not pass the check |
| `mail_sign_in_refused` | 502 | test | the server refused the user or the password |
| `mail_sender_refused` | 502 | test | the server refused the sender's address |
| `mail_recipient_refused` | 502 | test | the server refused the address, in `values.address` |
| `mail_server_timeout` | 502 | test | the server did not answer in time |
| `mail_server_answered` | 502 | test | the server refused the mail in its own words, in `values.words` |

the five with the cluster in them carry `detail`. "test" is `POST /api/portal/mail/test`. a
field of the wrong kind is refused with its field's code. a body that is no JSON object is
answered `400` with the line `the body is not an object`, and one that holds a field the
route does not take with `the field {name} is not one this route takes`: a plain line of
detail with no code, never in the decoder's words, as on every route that reads its body
the same way — all of portal-api's but the chart addons' two. on a route without codes of
its own, a field of the wrong kind is `the field {name} has a value of the wrong kind`, and
a body larger than the route reads `the body is larger than this route takes`.

configuration: `PORTAL_MAIL_HOST`, `PORTAL_MAIL_PORT`, `PORTAL_MAIL_ENCRYPTION`,
`PORTAL_MAIL_FROM`, `PORTAL_MAIL_FROM_NAME`, `PORTAL_MAIL_ALERTS_TO` (comma-separated),
`PORTAL_MAIL_USERNAME`, `PORTAL_MAIL_PASSWORD`, `PORTAL_MAIL_SECRET` (the name of the
password's Secret) and `PORTAL_PUBLIC_URL`. the operator sets them from `spec.mail`.

## alerts

portal-api reads the status of the platform's resource every 60 seconds and raises five
alerts for the admins: a part that is not ready, a volume that is almost full, the
platform's certificate that ends soon, no good backup, and an update that failed. it keeps
them in a table of its own (migration 016) until 90 days after they ended, swept once an
hour a batch at a time, and which admin has seen which in another. one alert of a kind is
active for one subject; where portal-api runs more than once, the write that starts an
alert, and the one that ends it, decide which of them mails it. a check writes every start
and every end before it sends a mail, so a mail server that does not answer delays no
alert; a check has 50 seconds, and each call of the portal's database in it 10; a check
that fails is said in the log, and the next one runs. while the phase is `Restoring`, no
alert about a part or about the update starts or ends. a resource younger than 1 hour
raises no alert, and a platform the operator does not run raises none. the updater's
report of a one-machine install is not read yet: that is I.C17's.

an alert is mailed when it starts and when it ends, plain text to each address: every
admin whose account is enabled and has an address, and each address of
`spec.mail.alertsTo` that is no placeholder, once each — the end to the addresses the start
reached. one start mail is sent for an alert about one subject in 24 hours, counted from
when the mail server first took the mail of the last start for an address. a mail the
server does not take, one held back while mail is set up and not ready, and one whose
mail settings could not be read, with the cluster's refusal as its reason, is tried again
after 5 minutes, three times in all for each address, by whichever portal-api runs then:
mail that is set up and not ready — the settings still being applied, the password
missing — holds a mail back at each try, and after the third it could not be sent. an
address the start mail has not reached when the alert ends gets neither mail: no try of
the start mail is made after the end, and a portal-api that carries it on learns of an
end written by another with its next write. nothing is mailed while mail is not set up,
while `spec.alerts.mail` is false, or where nobody has an address: an alert that starts
then is never mailed, not later either, and neither is its end, and a mail that meets one
of them at a try again goes no further.

an address gets a start mail once and an end mail once, however many portal-apis run: one
holds an alert's mail at a time, for 2 minutes by the database's clock, right before it
goes; each write of what became of it names its holder, and is made after each address,
which keeps the hold. a portal-api told to stop finishes the address it is on, writes what
became of it, and lets the rest go to whichever portal-api runs next, at once; it is given
25 of the 30 seconds a pod has to stop. the addresses are kept until the alert's end mail
is done, and then their number, how many took it, and one reason with no address in it.

the thresholds are `spec.alerts`: `partNotReadyMinutes` (default 10, 1 to 1440),
`volumeFullPercent` (90, 50 to 99), `certificateDays` (14, 1 to 90) and `backupAgeDays` (2,
1 to 30), with `mail` (true). the resource holds no defaults: portal-api applies them.

| route | who | what |
|---|---|---|
| `GET /api/portal/alerts` | admin | `{available, alerts, more, unseen, watched}`: every active alert, the newest first, and then the newest 500 that ended in the last 90 days, the newest end first; `more`, true, where older ones that ended are kept; `unseen`, how many of those the bell shows — the active ones and those that ended within 7 days — the admin has not seen since they last changed; `watched`, the five. where the operator does not run the platform, `{available: false, reason: "no-operator"}` |
| `PATCH /api/portal/alerts/settings` | admin | any of `partNotReadyMinutes`, `volumeFullPercent`, `certificateDays`, `backupAgeDays` and `mail` into `spec.alerts`, the ranges checked first, written with strict field validation (`fieldValidation=Strict`); answers `{generation}` |
| `POST /api/portal/alerts/{id}/read` | admin | the alert seen by the admin who asks: `{unseen}`; `404` for one there is not |
| `POST /api/portal/alerts/read-all` | admin | every alert the admin who asks had not seen since it last changed, seen by them, and no other: `{seen, unseen}`, `seen` how many |

an alert is `{id, kind, about, title, text, values, link, startedAt, endedAt, mail,
seen}`. `kind` is `part-not-ready`, `volume-full`, `certificate-ends`, `no-good-backup` or
`update-failed`; `about` the part's name, the disk's first volume as the status names it
(`media`, `postgres-data`, `backups` or `kafka`), `certificate`, `backups` or `update`;
`link` a path of the portal; `values` what filled in the title and the text. a disk is the
volumes that report one filesystem id at a check, and its alert is about its first volume
in the order library, databases, backups, event broker: the id only says at each check
which volumes are one disk, so an id that changes neither ends an alert nor starts one. a
part whose own rollout has given up (`status.components[].stalled`) raises no alert of its
own: an update failed names it. {minutes} is the setting as it was when the alert started.
a percentage is compared in whole numbers, used × 100 against the setting × size. `mail`
is `{state, addresses, taken, error}`: `state` is `sent`, `sending`, `not-set-up`, `off`,
`no-address`, `sent-recently`, `failed` or `ended-before-mail` (the alert ended before its
start mail got a single try, and neither mail went), of the start mail while the alert is
active and of the end mail once it ended (of the start mail where no end mail was due);
`addresses` how many it is for; `taken`, where some took it and others did not or still
wait, how many did — once the server took it for an address it is sent, whatever changes
afterwards; `error`, with `failed` and always there with it, the reason as a refusal: the
test mail's codes, `mail_not_ready`, `mail_password_missing`, and, where the mail settings
could not be read, `platform_read_forbidden` or `cluster_unexpected_answer`; where a mail
went no further before it was tried for some addresses, `mail_not_set_up` (“Mail is not
set up.”) or `alerts_mail_off` (“Alert mail is off.”) for them. once the end mail is done,
a reason that named an address is `mail_recipient_refused` with no values and the sentence
“The mail server refused an address.”. `watched` is each of the five as `{kind, setting,
least, most, now}`, `now` a `{state, values}`: `parts-ready` and `parts-not-ready` (`n`),
`fullest` (`n`), `not-measured`, `certificate-ends` (`date`), `no-certificate`,
`certificate-unreadable`, `newest-backup` (`date`), `no-backup-yet`, `backups-off`,
`databases-external`, `databases-without-volume`, `version-runs` (`version`), and under an
operator older than a fact `update-operator-parts`, `update-operator-volumes` and
`update-operator-certificate`. whether the operator says since when a part is not ready is
asked of the cluster in a dry run (`dryRun=All`) that writes nothing: an operator whose
schema has alert settings says it.

| code | status | route | the case |
|---|---|---|---|
| `alerts_threshold_out_of_range` | 400 | `PATCH` | a threshold outside its range, or no whole number: `values.field`, `values.least`, `values.most` |
| `alerts_not_run_by_operator` | 409 | `PATCH` | the platform has no resource |
| `alerts_settings_unknown` | 503 | `PATCH` | the cluster does not know `spec.alerts`, as under an operator from before alert settings: nothing is written |
| `platform_read_forbidden` | 503 | `GET`, `PATCH` | the cluster does not let portal-api read the resource |
| `alerts_write_forbidden` | 503 | `PATCH` | the cluster does not let portal-api change it |
| `alerts_write_refused` | 422 | `PATCH` | the cluster refuses the settings |
| `cluster_unexpected_answer` | 502 | `GET`, `PATCH` | any other answer of the cluster |
| `alerts_mail_off` | 200, in `mail.error` | `GET` | alert mail was switched off before a mail was tried for some of its addresses: “Alert mail is off.” |

the five with the cluster in them carry `detail`. `mail` that is not true or false, a body
that names none of the five, one that is no JSON object (`the body is not an object`) or
holds a field the route does not take (`the field {name} is not one this route takes`):
`400` with a line of detail and no code, as on the mail routes.

portal-api's log says that an alert started or ended, `alerts: {kind} about {about}
started`, where {about} is the alert's subject: the part's name, the disk's first volume
(`media`, `postgres-data`, `backups` or `kafka`), `certificate`, `backups` or `update`; that
a mail of an alert was sent, refused or given up; and what of the portal's database or the
cluster failed. no line holds an address, a title or a text, and a control character in a
value from the status, a line break among them, is written as a space.

## develop

```bash
npm install          # builds the vendored @nalet/design-system tarball
npm run dev          # vite dev server
npm run build        # production bundle into dist/
```

## build the container

```bash
docker build -t zaentrum-portal .
docker run -p 8080:8080 zaentrum-portal      # http://localhost:8080 -> /portal/
```

the image is a static nginx serving the built SPA on port 8080 (non-root). configure the
OIDC issuer through your platform's `/api/config` endpoint.

## deploy

deployment manifests and the GitOps wiring for the reference instance live separately
(GitLab, deploy-only). this repository is the application source.

## license

[MPL-2.0](LICENSE).
