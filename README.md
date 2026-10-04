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
