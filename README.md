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
