# UI harness

Renders a portal view in a real browser with **no cluster, no Keycloak and no
portal-api** — against data shaped like the live environment.

It exists because the portal has no other way to be verified: every view sits
behind OIDC and a namespace-scoped API, so the only previous way to look at a
change was to deploy it. When the registry credential died (2026-08-05) that
became impossible for 36 hours, and several UI changes piled up unseen.

    node harness/mock-server.mjs &          # fake portal-api on :8791
    npx vite --config harness/vite.config.ts # the real component on :8792

`oidc-stub.tsx` stands in for `react-oidc-context` via a Vite alias, so
`usePortalApi` still runs its real fetch path — only the token is fabricated.
The component, its CSS and the design system are all the real ones.

Two bugs were caught here that reading the code did not surface: a per-row
group badge repeated on all 14 platform rows (redundant with the section
heading it sat under, and it made the service column ragged), and three
independently auto-sized tables whose columns did not line up.

`mock-server.mjs` mirrors live `zaentrum-beta` — 14 platform services, 5
addons, one deliberately unclaimed workload so every group renders, and the
ImagePullBackOff state the estate was actually in.
