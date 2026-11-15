# zaentrum-portal

The portal of zaentrum: the shell a person signs in to, the launchpad of spaces and tiles
the apps open from, and the admin's consoles (settings, operator, People, logs, events,
database, export). Two components, each its own image: the front end (React, Vite,
TypeScript), served by nginx under `/portal/`, and portal-api in `server/` (Go), the registry
of apps, spaces and tiles behind `/api/portal`, which also keeps invites and the notices
addons post. The front end reads the sign-in service from the platform's `GET /api/config`
(chino-api answers it) and calls portal-api; portal-api uses Postgres, the sign-in service,
the cluster's API, Kafka, chino-api and katalog-manager.
How a change is proposed and what a pull request has to pass is in the
[guide to contributing](https://github.com/zaentrum/.github/blob/main/CONTRIBUTING.md);
what may be built is in [building from the specification](https://github.com/zaentrum/zaentrum/blob/main/AGENTS.md).

## Build

```sh
npm ci
npm run build                        # tsc -b && vite build -> dist/
(cd server && go build ./...)
docker build -t zaentrum-portal .    # the front end: nginx-unprivileged on 8080, under /portal/
docker build -t portal-api server    # portal-api: distroless, on 8080
```

The front end's tests need Node 22.18 or later (the workflow uses 22; the image builds on
`node:20-alpine`). portal-api needs Go 1.24 (`server/go.mod`). The design system comes from
the tarball in `vendor/`, not from a registry.

## Test

As `.github/workflows/build.yml` runs them, in its jobs `build` and `server`:

```sh
npm ci --no-audit --no-fund && npm run build && npm test   # node --test src/lib/*.test.ts
cd server && go build ./... && go vet ./... && go test ./...
```

The store's tests (`server/internal/store`) run the migrations and statements against a
disposable Postgres named by `PORTAL_TEST_DATABASE_URL`, and drop the registry's tables
first. Without it they skip, unless `CI` is set: then they fail. The workflow gives them
`postgres:16-alpine`:

```sh
PORTAL_TEST_DATABASE_URL='postgres://portal@localhost:5432/portal_test?sslmode=disable' go test ./...
```

## Run it

The harness renders the real views against recorded answers, with no cluster, no sign-in
service and no portal-api:

```sh
node harness/mock-server.mjs &             # a stand-in portal-api on :8791
npx vite --config harness/vite.config.ts   # http://localhost:8792/
```

`?view=` picks the view: none for the operator console, `settings`, `launchpad`, `people`,
`shell`, or `invite&token=<token>` (the mock logs an open invite's token at start).
`MOCK_PORT` moves the mock and the proxy to it, `HARNESS_PORT` the page.
[harness/README.md](harness/README.md) lists every state the mock plays.

portal-api runs on its own against any Postgres, on `SERVER_PORT` or `PORT` (default 8080),
and applies its migrations at start:

```sh
cd server && PG_URL='postgres://localhost:5432/portal' go run ./cmd/server
```

Without `OIDC_ISSUER` (or with `AUTH_DISABLED=true`) it checks no token: every caller is an
admin. Outside a cluster the operator console reports unavailable, without `KAFKA_BROKERS`
the events console. From the root, `MOCK_PORT=8080 npx vite --config harness/vite.config.ts`
shows the harness's views against it. `server/internal/config/config.go` lists every variable
but `PORTAL_NAMESPACE`, which `server/internal/k8s/client.go` reads where the pod's own
namespace cannot be read, as outside a cluster.

`npm run dev` serves the shell at `http://localhost:5173/portal/` with no proxy, so it stops
at "sign-in failed": the sign-in service comes from `/api/config`, else `VITE_OIDC_AUTHORITY`,
else `/auth/realms/zaentrum` on the page's origin. On the platform all of it is one origin,
as in the reference demo ([how it is deployed](https://github.com/zaentrum/zaentrum/blob/main/docs/reference-demo.md)).

## What the tests leave unchecked

- The views: the front end's tests cover the functions in `src/lib`; the settings and operator
  consoles, the launchpad, the shell, the People page and the invite page are looked at in
  the harness, by hand; the logs, events, database and export consoles (`src/debug/`) have
  no harness view.
- Sign-in: no test in the workflow meets a real sign-in service. `TestAgainstARealKeycloak`
  (`server/internal/people`) runs only with `PORTAL_TEST_KEYCLOAK_URL` and
  `PORTAL_TEST_KEYCLOAK_SECRET`; the workflow runs its rules against the fake in
  `server/internal/keycloak/kctest`.
- The cluster and Kafka: the operator console runs against a fake
  (`server/internal/k8s/k8sfake`), the event tap without a broker.
- `server/internal/chino` (the client for chino-api) and `server/cmd/server` have no tests.
- A pull request builds the front end's image only; `image-api.yml` builds portal-api's on
  main when `server/` changes, and on a release tag. Neither image is started.
- `npm run lint` (oxlint) is not in the workflow; `preview.html` and `preview/` are in
  neither the build nor the type check.

## Before a pull request

The design system is vendored: `package.json` installs the tarball in `vendor/` by `file:`,
and `package-lock.json` holds its sha512. A new tarball goes in with both files updated by
`npm install`. `npm ci` on a fresh runner fails with `EINTEGRITY` when they differ; a local
npm cache can hide that by installing the copy it already holds.
