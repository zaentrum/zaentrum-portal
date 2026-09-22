# UI harness

Renders a portal view in a real browser with **no cluster, no Keycloak and no
portal-api** — against data shaped like the live environment.

It exists because the portal has no other way to be verified: every view sits
behind OIDC and a namespace-scoped API, so the only previous way to look at a
change was to deploy it. When the registry credential died (2026-08-05) that
became impossible for 36 hours, and several UI changes piled up unseen.

    node harness/mock-server.mjs &           # fake portal-api on :8791
    npx vite --config harness/vite.config.ts # the real component on :8792

    http://localhost:8792/                   # operator console
    http://localhost:8792/?view=settings     # app registry console

`oidc-stub.tsx` stands in for `react-oidc-context` via a Vite alias, so
`usePortalApi` still runs its real fetch path — only the token is fabricated.
The component, its CSS and the design system are all the real ones.

Two bugs were caught here that reading the code did not surface: a per-row
group badge repeated on all 14 platform rows (redundant with the section
heading it sat under, and it made the service column ragged), and three
independently auto-sized tables whose columns did not line up.

`mock-server.mjs` mirrors live `zaentrum-beta` — 14 platform services and
the ImagePullBackOff state the estate was actually in — plus one installed
addon, `example`, made of three containers (one crash-looping, one not
deployed), addon workloads no installed addon declares, and one deliberately
unclaimed workload, so every section of the operator console renders. The
settings console's addons tab reads the same fixture: a refresh-available
badge, the containers column, and a setup checklist whose status the browser
fetches through the app proxy (one summary carries markup on purpose, to show
it renders as text).

Chart addons are served by an in-memory stand-in for the operator: settings →
addons → **add from a chart** walks the whole wizard. Any chart reference plans
(e.g. `oci://registry.example.org/charts/notes` with version `1.2.0`, or
`https://charts.example.org/notes-1.2.0.tgz`); the plan is generated from the
chart's name and carries a values schema with a secret input, a generated one,
an enum, a checkbox, a nested group and a JSON field. Until the database
password is set the plan has a values error — paste it among the values
(`{"config":{"password":"…"}}`) and the plan offers to move it to the secret
inputs; a reference containing
`privileged` plans with a violation, so a refusal renders. A plan turns current
about a second after each change, an install brings its two workloads up one
after the other, and the addon reads as registered a second after it is ready.
`sample` is seeded installed and registered from chart 1.0.0, for the row
actions: upgrade (plan, changes, apply or cancel), values, remove with "keep
values". An address that collides with an addon added by address (`example`) is
refused like the server refuses it.

    http://localhost:8792/?view=settings               # chart addons available
    http://localhost:8792/?view=settings&charts=off    # an older portal-api: no "+"
    http://localhost:8792/?view=settings&charts=nocrd  # a cluster without the resource type

The operator console's controller card renders from `status.controller`, and
every install source sends the reader somewhere different, so each one can be
looked at — including the operator that reports nothing, which is every
operator older than the field:

    http://localhost:8792/?controller=olm        # a subscription, with an update on the channel
    http://localhost:8792/?controller=manifest   # digest-pinned, applied from the install manifest
    http://localhost:8792/?controller=appliance  # the appliance carries it
    http://localhost:8792/?controller=unknown    # installed somehow; all three paths named
    http://localhost:8792/?controller=none       # an older operator: nothing reported

Nothing in that card is a control, whichever mode is on: the controller is
updated outside the platform, and a button here could only look like it worked.

The mock keeps its state in memory; restart it for the seeded state.

The registry fixture mirrors what migrations 002/003/004 actually seed — every
seeded app has an **empty** `proxyUrl`, which is the state that made the
`embeddable` column worth adding: nothing in a fresh install can be hosted
inside the portal shell, and before this the console could not even show that,
let alone set it. One app carries a `proxyUrl` so both states render.

Add a view by importing it in `main.tsx` and giving it a `?view=` key.
