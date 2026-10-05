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
page (`/portal/people`), gives a child a rating cap, and sends each an invite link; the
person opens it — no sign-in, they have nothing to sign in with yet — chooses their own
password, and signs in on every app with their username. portal-api does it through the
platform realm's `zaentrum-people` client, which may view, query and manage the realm's
users and nothing else (the platform chart's realm Job keeps it so). with an external
identity provider people live there: the page says so and links to it.

| route | who | what |
|---|---|---|
| `GET /api/portal/people` | admin | everyone (the platform's own accounts aside): role, rating cap, enabled, created, their newest invite's state — never its token; `mode` bundled, external (`manageUrl`) or unavailable (`note`) |
| `POST /api/portal/people` | admin | `{username, displayName, role, maxRating}`: an enabled account with no password, `zaentrum-user` (+ `zaentrum-admin`); answers the person and their invite link, once |
| `PATCH /api/portal/people/{id}` | admin | `displayName`, `role`, `maxRating` (`null`: no cap), `enabled`; switched off, the open invite is revoked |
| `DELETE /api/portal/people/{id}` | admin | chino-api deletes the person's data first, then the account goes, then their invites |
| `POST /api/portal/people/{id}/invite` | admin | a new link; older ones stop working |
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
