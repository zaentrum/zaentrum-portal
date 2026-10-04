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
    http://localhost:8792/?view=launchpad    # launchpad, with the setup checklist

Ports taken? `MOCK_PORT` moves the mock, and the dev proxy with it;
`HARNESS_PORT` moves the page:

    MOCK_PORT=8795 node harness/mock-server.mjs &
    MOCK_PORT=8795 HARNESS_PORT=8796 npx vite --config harness/vite.config.ts

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

    http://localhost:8792/?controller=olm        # a subscription, with a VERSION on the channel
    http://localhost:8792/?controller=manifest   # digest-pinned, applied from the install manifest
    http://localhost:8792/?controller=appliance  # the appliance carries it
    http://localhost:8792/?controller=unknown    # installed somehow; all three paths named
    http://localhost:8792/?controller=moving     # pinned to a commit, following a moving channel tag
    http://localhost:8792/?controller=none       # an older operator: nothing reported

`moving` is the live shape on the demo: `availableUpdate` is a channel tag
(`latest`), not a version, so the badge says the channel serves a different
image instead of offering "latest" as if it were a release.

Nothing in that card is a control, whichever mode is on: the controller is
updated outside the platform, and a button here could only look like it worked.

The verification card renders from `operator.verification`, served by a
stand-in for the operator verifying the platform. Each mode is a record the
console starts from, and **verify now** plays the whole round trip in any of
them: the request waits about a second and a half for the operator, the run
takes four, and the toolbar says how it ended. A request made while a run is
in progress waits for it, and one made while another waits joins it, as the
server does:

    http://localhost:8792/                          # failed after the last update (the degraded estate)
    http://localhost:8792/?verification=passed      # passed, one warning
    http://localhost:8792/?verification=running     # a run in progress; it ends a few seconds later
    http://localhost:8792/?verification=requested   # a request waiting for the operator
    http://localhost:8792/?verification=never       # no run reported, as every older operator
    http://localhost:8792/?verification=off         # switched off: no button, and the last record
    http://localhost:8792/?verification=unreadable  # a record portal-api could not read
    http://localhost:8792/?verification=old         # a portal-api older than the field: no card

Every change the operator console makes asks first, and the mock answers it
the way portal-api does: **update to** sends the version shown (one the
operator replaced is refused 409), the admin stack — portal-api, the portal,
the catalog manager — keeps one replica (its step to 0 is disabled), and a
restart of a workload that recreates its pods (analyzer, katalog-ingest,
transcoder) says it goes down until the new pod is ready. In settings, a core
entry (chino, the apps and manage spaces) has no delete, every other delete
says what goes with it, tiles and spaces carry who sees them (the ops space is
for the ops role only), and a chart addon's **values** are planned first: the
dialog shows the plan and asks, and cancelling puts the values back.

The launchpad view shows the first-run setup checklist an admin sees until
setup is marked done. The mock stands in for portal-api's setup endpoints
and for what they read — the catalog manager and the operator — and plays
every action: a key saved is set (one that is the v3 API key is refused
before it is sent), **scan now** runs four seconds and finds 14 files, the
pipeline switched on (it asks first) brings its workers up within three —
the transcoder stays Unschedulable, as on a box with no GPU node — and
**done** asks while steps are open. Settings then says who marked it done,
and **show setup again** reopens it:

    http://localhost:8792/?view=launchpad                     # a fresh appliance
    http://localhost:8792/?view=launchpad&setup=scanning      # a scan under way
    http://localhost:8792/?view=launchpad&setup=ready&gpu=1   # every step done
    http://localhost:8792/?view=launchpad&setup=unknown       # a catalog manager that does not answer
    http://localhost:8792/?view=launchpad&setup=old           # a portal-api without setup: no card
    http://localhost:8792/?view=settings&setup=done           # marked done: show setup again

`setup=filled` has a key and a library, `setup=https` a public host over
https; `gpu=1` gives the cluster a GPU node, so the transcoder runs.

The mock keeps its state in memory; restart it for the seeded state.

The registry fixture mirrors what migrations 002/003/004 actually seed — every
seeded app has an **empty** `proxyUrl`, which is the state that made the
`embeddable` column worth adding: nothing in a fresh install can be hosted
inside the portal shell, and before this the console could not even show that,
let alone set it. One app carries a `proxyUrl` so both states render.

Add a view by importing it in `main.tsx` and giving it a `?view=` key.
