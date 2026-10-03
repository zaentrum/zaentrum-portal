// Serves the portal-api surface the operator and settings consoles read,
// shaped like live zaentrum-beta (14 platform services) plus one installed
// addon made of three containers, a few addon workloads no installed addon
// declares, and one deliberately unclaimed workload so every group renders.
import { createServer } from 'node:http';

const platform = [
  'analyzer','chino-api','chino-stream','chino-web','katalog-api','katalog-ingest',
  'katalog-manage-ui','katalog-manager-api','katalog-manager-ui','packager',
  'portal-api','transcoder','valkey','zaentrum-portal',
];
// Names are deliberately generic: the harness exercises the GROUPING, not
// any particular addon, and this repo is public.
const undeclared = ['addon-alpha','addon-beta','addon-gamma'];

// 13 of the 14 platform services are in ImagePullBackOff right now; valkey is
// the one still up. Reproduced faithfully so the reason column is exercised.
const inst = (name, group, broken, labels = {}) => ({
  name,
  // Platform images are digest-pinned by the operator; addons run tags.
  // Mirroring that is the point — a fixture where everything is a short
  // tag hides the column-width problem that digests cause.
  image:
    group === 'platform'
      ? `ghcr.io/zaentrum/${name}@sha256:56268318f11a2083bc3f03da3b7720ead6e1bdfb0f86da7de5746b1bcad3a7dd`
      : `ghcr.io/zaentrum/${name}:latest`,
  desiredReplicas: 1,
  // status.replicas is the total across every ReplicaSet: a broken workload is
  // mid-surge here, one new pod beside the old one, which is the state the
  // other counters describe wrongly.
  replicas: broken ? 2 : 1,
  readyReplicas: broken ? 0 : 1,
  updatedReplicas: 1,
  availableReplicas: broken ? 0 : 1,
  restarts: broken ? 3 : 0,
  phase: broken ? 'degraded' : 'ready',
  protected: name === 'valkey',
  // The console runs on these: the platform keeps one replica of each.
  adminStack: ['portal-api', 'zaentrum-portal', 'katalog-manager-api'].includes(name),
  // As the demo runs them: the stateful and single-writer workloads recreate
  // their pods, everything else rolls.
  strategy: ['analyzer', 'katalog-ingest', 'transcoder'].includes(name) ? 'Recreate' : 'RollingUpdate',
  operatorManaged: group === 'platform',
  alwaysPull: true,
  group,
  reason: broken ? 'ImagePullBackOff' : '',
  // What a client waits on: the spec generation, the one the Deployment
  // controller has acted on, and the rollout-restart stamp. A broken workload
  // is mid-rollout here — observed one behind — which is exactly the state
  // the replica counters describe wrongly.
  generation: broken ? 4 : 3,
  observedGeneration: 3,
  restartedAt: '2026-09-21T08:00:00Z',
  ...labels,
});

// The installed "example" addon: its primary and worker run (the worker is
// crash-looping), its optional sync container is not deployed at all.
const example = { addon: 'example' };
const instances = [
  ...platform.map((n) => inst(n, 'platform', n !== 'valkey')),
  inst('example', 'addon', false, { ...example, component: 'example' }),
  { ...inst('example-worker', 'addon', true, { ...example, component: 'worker' }), reason: 'CrashLoopBackOff' },
  ...undeclared.map((n) => inst(n, 'addon', false)),
  { ...inst('leftover-job-runner', 'other', false), image: 'docker.io/library/busybox:latest' },
];

// The operator's own controller, as status.controller reports it. The source
// decides what the console tells an administrator to go and do, so the harness
// can play each one — and `none` plays every operator older than the field,
// which reports no controller at all.
const controllers = {
  olm: { image: 'ghcr.io/zaentrum/operator:v0.4.1', version: 'v0.4.1', source: 'olm',
    availableUpdate: 'v0.5.0', observedAt: new Date().toISOString() },
  manifest: { image: 'ghcr.io/zaentrum/operator@sha256:56268318f11a2083bc3f03da3b7720ead6e1bdfb0f86da7de5746b1bcad3a7dd',
    version: 'sha256:56268318f11a', source: 'manifest', availableUpdate: '', observedAt: new Date().toISOString() },
  appliance: { image: 'ghcr.io/zaentrum/operator:v0.4.1', version: 'v0.4.1', source: 'appliance',
    availableUpdate: '', observedAt: new Date().toISOString() },
  unknown: { image: 'ghcr.io/zaentrum/operator:v0.4.1', version: 'v0.4.1', source: 'unknown',
    availableUpdate: '', observedAt: '' },
  // The live shape on demo and beta: pinned to a commit image, following a
  // moving channel tag. `latest` is not a version anyone can be "on", so the
  // card says the channel moved rather than naming it as an update.
  moving: { image: 'ghcr.io/zaentrum/operator:sha-19ea431', version: 'sha-19ea431', source: 'manifest',
    availableUpdate: 'latest', observedAt: new Date().toISOString() },
};
const controllerMode = (req) => (req.headers.cookie ?? '').match(/(?:^|;\s*)mock-controller=([a-z]*)/)?.[1] ?? '';

// ─── verification ────────────────────────────────────────────────────────────
//
// A stand-in for the operator verifying the platform. ?verification= picks the
// record the console starts from, and "verify now" plays the whole round trip:
// the request waits VERIFY_WAIT_MS for the operator, the run takes
// VERIFY_RUN_MS, and it ends the way the mode's record did — failed in the
// default mode, which matches the degraded estate above. A request made while
// a run is in progress waits for it; one made while another waits joins it.
const VERIFY_WAIT_MS = 1500;
const VERIFY_RUN_MS = 4000;
const minutesAgo = (m) => new Date(Date.now() - m * 60_000).toISOString();

const checksFor = (result) => [
  { name: 'workloads ready', status: result === 'Failed' ? 'fail' : 'ok',
    detail: result === 'Failed' ? '1/14 platform workloads ready — 13 in ImagePullBackOff' : '' },
  { name: 'portal launchpad', status: 'ok', detail: '' },
  { name: 'issuer discovery', status: 'warn', detail: 'answered in 1.8 s, over 1 s' },
  { name: 'katalog-api health', status: result === 'Failed' ? 'fail' : 'ok',
    detail: result === 'Failed' ? 'GET /actuator/health/readiness: connection refused' : '' },
  { name: 'event bus', status: 'skip', detail: 'no bus is configured' },
  ...['chino-api health', 'chino-stream health', 'packager health', 'transcoder health', 'database', 'cache',
    'tls certificates', 'routes', 'addon registry'].map((name) => ({ name, status: 'ok', detail: '' })),
];

// ended is a finished record: the counts are the checks', as the operator's are.
const ended = (result, base) => {
  const checks = checksFor(result);
  const count = (s) => checks.filter((c) => c.status === s).length;
  const failed = count('fail');
  return {
    ...base, result, finishedAt: base.finishedAt || new Date().toISOString(),
    passed: count('ok'), failed, warned: count('warn'), skipped: count('skip'), checks,
    message: failed ? `${failed} of ${checks.length} checks failed` : `all ${checks.length - count('skip')} checks passed`,
  };
};
const runBase = (trigger, request, startedAt) => ({
  trigger, request, fingerprint: 'sha256:5626831', version: 'v0.3.0', startedAt, finishedAt: '',
  job: `zaentrum-verify-${Math.random().toString(36).slice(2, 7)}`,
});
const running = (trigger, request) => ({
  ...runBase(trigger, request, new Date().toISOString()),
  result: 'Running', passed: 0, failed: 0, warned: 0, skipped: 0, checks: [], message: '',
});

// The record each mode starts from: the run, the annotation, and how a run
// asked for in this mode ends.
const seedVerification = (mode) => {
  const update = (result, m) => ended(result, { ...runBase('update', '', minutesAgo(m + 2)), finishedAt: minutesAgo(m) });
  switch (mode) {
    case 'passed': return { record: update('Passed', 3), annotation: '', ends: 'Passed' };
    case 'running': return { record: running('update', ''), annotation: '', ends: 'Passed' };
    case 'requested': return { record: update('Passed', 50), annotation: 'harness-seed', ends: 'Passed' };
    case 'never': return { record: null, annotation: '', ends: 'Passed' };
    case 'off': return { record: update('Passed', 3 * 24 * 60), annotation: '', ends: 'Passed', off: true };
    case 'unreadable': return { record: null, annotation: '', ends: 'Passed', unreadable: true };
    default: return { record: update('Failed', 12), annotation: '', ends: 'Failed' };
  }
};

let verify = null;
const verificationMode = (req) => (req.headers.cookie ?? '').match(/(?:^|;\s*)mock-verification=([a-z]*)/)?.[1] ?? '';

// verifyState is the stand-in operator, evaluated whenever it is read.
const verifyState = (mode) => {
  if (!verify || verify.mode !== mode) verify = { mode, requestedAt: Date.now(), ...seedVerification(mode) };
  const v = verify;
  const now = Date.now();
  if (v.record?.result === 'Running' && now - Date.parse(v.record.startedAt) >= VERIFY_RUN_MS) {
    v.record = ended(v.ends, { ...v.record, finishedAt: new Date().toISOString() });
  }
  const waiting = v.annotation && v.annotation !== v.record?.request;
  if (waiting && !v.off && v.record?.result !== 'Running' && now - v.requestedAt >= VERIFY_WAIT_MS) {
    v.record = running('request', v.annotation);
  }
  return v;
};

// verificationView is operator.verification as portal-api answers it.
const verificationView = (mode) => {
  const v = verifyState(mode);
  if (v.unreadable) {
    return { enabled: true, result: null,
      note: "the operator's verification record cannot be read: json: cannot unmarshal string into Go struct field verificationRecord.passed of type int" };
  }
  const out = { enabled: !v.off, result: v.record ? v.record.result : null };
  if (v.record) {
    const { result, ...run } = v.record;
    Object.assign(out, run);
    if (result !== 'Running') {
      out.condition = { status: result === 'Passed' ? 'True' : 'False',
        reason: result === 'Passed' ? 'ChecksPassed' : 'ChecksFailed', lastTransitionTime: v.record.finishedAt };
    }
  }
  if (v.annotation && v.annotation !== v.record?.request) out.pendingRequest = v.annotation;
  return out;
};

// requestVerification answers POST /operator/verify as portal-api does.
const requestVerification = (req, res) => {
  const mode = verificationMode(req);
  if (mode === 'old') return text(res, 404, '404 page not found');
  const v = verifyState(mode);
  if (v.off) {
    return text(res, 409, 'verification is disabled: spec.verification.enabled is false on zaentrum, so the operator runs no checks — enable it to verify the platform');
  }
  const waiting = v.annotation && v.annotation !== v.record?.request;
  if (!waiting) {
    v.annotation = `harness-${Math.random().toString(36).slice(2, 10)}`;
    v.requestedAt = Date.now();
  }
  console.log(`mock: verification requested — ${waiting ? 'joins' : 'writes'} request ${v.annotation}`);
  return json(res, 202, { request: v.annotation });
};

// The platform's spec as the console changes it: channel and update mode are
// written by PATCH, and "update to" pins the version on offer.
const spec = { channel: 'stable', updateMode: 'manual', version: 'v0.3.0', availableUpdate: 'v0.4.0', generation: 7 };

const operatorState = (req) => {
  const mode = controllerMode(req);
  const controller = mode === 'none' ? null : (controllers[mode] ?? controllers.olm);
  const vmode = verificationMode(req);
  return {
    available: true,
    operator: {
      present: true, name: 'zaentrum', channel: spec.channel, updateMode: spec.updateMode,
      version: spec.version, currentVersion: 'v0.3.0', availableUpdate: spec.availableUpdate, phase: 'Degraded',
      components: [], generation: spec.generation, observedGeneration: spec.generation,
      // Absent, not empty: an operator that predates the field sends no key.
      ...(controller ? { controller } : {}),
      // `old` is a portal-api older than verification, which sends no key.
      ...(vmode === 'old' ? {} : { verification: verificationView(vmode) }),
    },
    instances,
  };
};

// The registry, as seeded by migrations 002/003/004 — note every seeded app has
// an EMPTY proxyUrl, which is the state that made the "embeddable" column worth
// adding: nothing in a fresh install can be hosted inside the portal shell.
const apps = [
  // core: the platform stands on it — edited and disabled, never deleted.
  { key: 'chino', title: 'chino', description: 'films & series', baseUrl: 'https://chino.example.com',
    kind: 'product', healthUrl: '', icon: 'film', enabled: true, proxyUrl: '', core: true },
  { key: 'katalog', title: 'katalog', description: 'browse the catalog', baseUrl: '/katalog',
    kind: 'admin', healthUrl: '', icon: 'library', enabled: true, proxyUrl: '' },
  { key: 'katalog-manage', title: 'katalog-manage', description: 'manage the catalog', baseUrl: '/katalog-manage',
    kind: 'admin', healthUrl: '', icon: 'settings', enabled: true, proxyUrl: '' },
  // One app WITH a proxyUrl, so the embeddable column shows both states.
  { key: 'example', title: 'Example', description: 'the example addon', baseUrl: '/portal/app/example',
    kind: 'tool', healthUrl: '', icon: 'puzzle', enabled: true, proxyUrl: 'http://example' },
];
const spaces = [
  { key: 'apps', title: 'apps', order: 10, audience: [], core: true },
  { key: 'manage', title: 'manage', order: 20, audience: [], core: true },
  { key: 'ops', title: 'ops', order: 30, audience: ['ops'], core: false },
];
// The tiles as the seed leaves them: the catalog for admins, the products for
// everyone signed in; plus one only operations see.
const tile = (key, appKey, spaceKey, title, order, audience, extra = {}) => ({
  key, appKey, spaceKey, title, description: '', icon: '', target: '', order, badge: '', badgeTone: '',
  status: 'online', external: false, open: 'inline', enabled: true, audience, ...extra,
});
const tiles = [
  tile('chino.open', 'chino', 'apps', 'chino', 10, [], { open: 'newtab' }),
  tile('katalog.catalog', 'katalog', 'manage', 'Catalog', 10, ['zaentrum-admin'], { badge: 'admin', badgeTone: 'info' }),
  tile('katalog-manage.open', 'katalog-manage', 'manage', 'Catalog Management', 20, ['zaentrum-admin'], { badge: 'admin', badgeTone: 'info' }),
  tile('example.ops', 'example', 'ops', 'example queue', 10, []),
];
const registry = { apps, spaces, tiles };

// serveRegistry answers the registry console's CRUD the way portal-api does:
// a core entry is refused 409, a write that names no audience keeps the
// stored one, and core is never written.
async function serveRegistry(req, res, pathname) {
  const m = pathname.match(/^\/api\/portal\/(apps|spaces|tiles)(?:\/([^/]+))?$/);
  if (!m) return false;
  const [, kind, key] = m;
  const rows = registry[kind];
  const one = kind.slice(0, -1);
  if (req.method === 'GET' && !key) return json(res, 200, rows), true;
  if (req.method === 'DELETE' && key) {
    const i = rows.findIndex((r) => r.key === key);
    if (i < 0) return text(res, 404, 'not found'), true;
    if (rows[i].core) return text(res, 409, `${one} "${key}" is a core entry of the platform and cannot be deleted`), true;
    rows.splice(i, 1);
    if (kind !== 'tiles') {
      const field = kind === 'apps' ? 'appKey' : 'spaceKey';
      for (let t = tiles.length - 1; t >= 0; t--) if (tiles[t][field] === key) tiles.splice(t, 1);
    }
    console.log(`mock: deleted ${one} ${key}`);
    res.writeHead(204);
    res.end();
    return true;
  }
  if ((req.method === 'POST' && !key) || (req.method === 'PATCH' && key)) {
    const body = await readBody(req);
    if (!body) return text(res, 400, 'invalid json'), true;
    const k = key ?? body.key;
    const i = rows.findIndex((r) => r.key === k);
    const old = i >= 0 ? rows[i] : undefined;
    const row = { ...body, key: k, core: old?.core ?? false };
    if (kind !== 'apps' && row.audience == null) row.audience = old?.audience ?? [];
    if (i >= 0) rows[i] = row;
    else rows.push(row);
    console.log(`mock: wrote ${one} ${k}${row.audience ? ` visible to [${row.audience}]` : ''}`);
    return json(res, 200, row), true;
  }
  return false;
}

// What GET /api/portal/addons answers: component state is null when a
// workload is not deployed.
const component = (name, workload, role, summary) => {
  const live = instances.find((i) => i.name === workload);
  return {
    name, workload, role, summary,
    phase: live ? live.phase : null,
    ready: live ? live.readyReplicas : null,
    desired: live ? live.desiredReplicas : null,
    restarts: live ? live.restarts : null,
    reason: live ? live.reason : null,
  };
};
const setup = {
  path: '/api/setup',
  sections: [
    { key: 'sources', title: 'sources', description: 'where items come from', required: true, target: '#/sources', ord: 10 },
    { key: 'workers', title: 'workers', description: 'what processes the queue', required: true, target: '#/workers', ord: 20 },
    { key: 'policy', title: 'policy', required: false, target: '#/settings', ord: 30 },
  ],
};
const addons = [
  {
    key: 'example', title: 'Example', proxyUrl: 'http://example', version: '2.0.0',
    installedAt: '2026-09-10T08:00:00Z', refreshedAt: '2026-09-12T08:00:00Z', tiles: 3, slots: 1,
    components: [
      component('example', 'example', 'primary', 'serves the manifest and the console'),
      component('worker', 'example-worker', 'required', 'processes the queue'),
      component('sync', 'example-sync', 'optional', 'mirrors items elsewhere'),
    ],
    setup,
    refreshAvailable: true,
  },
  // Installed before the addons table existed: no manifest, one implicit
  // primary whose workload is not running here.
  {
    key: 'legacy', title: 'legacy', proxyUrl: 'http://legacy', version: '',
    installedAt: '2026-08-01T08:00:00Z', refreshedAt: '2026-08-01T08:00:00Z', tiles: 1, slots: 0,
    components: [component('legacy', 'legacy', 'primary', '')],
    setup: null,
    refreshAvailable: false,
  },
];

// The addon's own answer at its setup path. Summaries are plain text — one
// carries markup on purpose, to show it is rendered as text.
const setupStatus = {
  state: 'needs-setup',
  sections: [
    { key: 'sources', state: 'ready', summary: '2 sources enabled' },
    { key: 'workers', state: 'needs-setup', summary: 'no worker is configured <b>yet</b>' },
    { key: 'policy', state: 'ready', summary: 'defaults' },
  ],
};

const json = (res, status, body) => {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
};

const text = (res, status, body) => {
  res.writeHead(status, { 'Content-Type': 'text/plain' });
  res.end(body);
};

const readBody = (req) =>
  new Promise((resolve) => {
    let b = '';
    req.on('data', (c) => (b += c));
    req.on('end', () => {
      try {
        resolve(b ? JSON.parse(b) : {});
      } catch {
        resolve(null);
      }
    });
  });

// ─── chart addons ────────────────────────────────────────────────────────────
//
// An in-memory ZaentrumAddon per addon, and a stand-in operator: it plans a
// changed generation after PLAN_MS, brings an installed addon's workloads up
// one by one, and the addon counts as registered REGISTER_MS after it is ready.
// Any chart reference works; the plan is generated from the chart's name. A
// reference containing "privileged" plans with a violation, so the refusal
// renders. The harness page's ?charts=off makes this an older portal-api (no
// /addon-charts), ?charts=nocrd a cluster without the resource type.
const PLAN_MS = 1200;
const WORKLOAD_MS = 1500;
const REGISTER_MS = 1000;

const valuesSchema = JSON.stringify({
  $schema: 'http://json-schema.org/draft-07/schema#',
  type: 'object',
  required: ['config'],
  properties: {
    replicas: { type: 'integer', title: 'worker replicas', default: 1, minimum: 1, description: 'how many workers process the queue' },
    logLevel: { type: 'string', title: 'log level', enum: ['info', 'debug', 'warn'], default: 'info' },
    metrics: {
      type: 'object',
      title: 'metrics',
      properties: { enabled: { type: 'boolean', title: 'expose metrics', default: false } },
    },
    config: {
      type: 'object',
      title: 'configuration',
      required: ['password', 'key'],
      properties: {
        password: { type: 'string', writeOnly: true, title: 'database password' },
        key: { type: 'string', writeOnly: true, title: 'encryption key', 'x-zaentrum-generate': 'random-base64-32' },
        url: { type: 'string', title: 'public url', description: 'where the console is reached, if not through the portal' },
      },
    },
    tolerations: { type: 'array', title: 'tolerations', description: 'scheduling tolerations for every workload' },
  },
});

// defaultName mirrors the server: the last path segment, without tag,
// version or archive extension.
const defaultName = (ref) => {
  const path = ref.replace(/^[a-z]+:\/\//i, '').replace(/[?#].*$/, '');
  const last = path.slice(path.lastIndexOf('/') + 1).replace(/[:@].*$/, '').replace(/(\.tar\.gz|\.tgz)$/i, '');
  return last.replace(/-v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$/, '').toLowerCase();
};
const archiveVersion = (ref) => (ref.match(/-v?([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*)\.(tgz|tar\.gz)$/i) ?? [])[1] ?? '';
const shownVersion = (chart) => chart.version || archiveVersion(chart.ref) || '0.0.0';

// normaliseChart: what POST and PATCH accept, as the server does.
const normaliseChart = (ref, version, digest) => {
  ref = (ref ?? '').trim();
  version = (version ?? '').trim();
  if (!ref) return { error: 'chart is required: an oci:// reference or an https:// link to a chart archive' };
  if (/^oci:\/\//i.test(ref)) {
    const m = ref.match(/^(oci:\/\/.+\/[^/:@]+)(?::([^/@]+))?$/i);
    if (!m) return { error: `chart "${ref}": an oci:// reference is a registry and a repository` };
    if (m[2] && version && m[2] !== version) return { error: `version "${version}" does not match the tag "${m[2]}" in the chart reference` };
    version = version || m[2] || '';
    if (!version) return { error: 'version is required for an oci:// chart' };
    return { chart: { ref: m[1].replace(/^oci/i, 'oci'), version, ...(digest ? { digest } : {}) } };
  }
  if (/^https:\/\//i.test(ref)) return { chart: { ref, ...(digest ? { digest } : {}) } };
  return { error: 'chart must be an oci:// reference or an https:// link to a chart archive' };
};

const chartAddons = new Map();

const planFor = (a) => {
  const version = shownVersion(a.chart);
  const title = a.name.charAt(0).toUpperCase() + a.name.slice(1);
  const img = (w, v) => `ghcr.io/example/${w}:${v}`;
  const plan = {
    chart: {
      name: a.name, version, appVersion: '2.0.0', description: 'an addon made of a console and a worker',
      digest: a.chart.digest ?? '',
      annotations: { 'zaentrum.io/addon': 'true', 'zaentrum.io/primary': a.name, 'zaentrum.io/title': title },
    },
    valuesSchema,
    valuesErrors: a.inputs.has('config.password') ? [] : ['config.password: required — the database password is not set'],
    violations: a.chart.ref.includes('privileged') ? [`Deployment/${a.name}-worker: privileged containers are not allowed`] : [],
    objects: [
      { kind: 'ServiceAccount', name: a.name },
      { kind: 'ConfigMap', name: `${a.name}-config` },
      { kind: 'PersistentVolumeClaim', name: `${a.name}-data` },
      { kind: 'Service', name: a.name },
      { kind: 'Deployment', name: a.name },
      { kind: 'Deployment', name: `${a.name}-worker` },
    ],
    workloads: [
      { kind: 'Deployment', name: a.name, images: [img(a.name, version)], ports: [{ name: 'http', containerPort: 8080, protocol: 'TCP' }] },
      { kind: 'Deployment', name: `${a.name}-worker`, images: [img(`${a.name}-worker`, version)], ports: [] },
    ],
    changes: null,
  };
  const old = a.lastAppliedChart && shownVersion(a.lastAppliedChart);
  if (old && old !== version) {
    plan.changes = {
      added: [],
      removed: [],
      images: [`Deployment/${a.name}: ${img(a.name, old)} → ${img(a.name, version)}`, `Deployment/${a.name}-worker: ${img(`${a.name}-worker`, old)} → ${img(`${a.name}-worker`, version)}`],
    };
  }
  return plan;
};

// tick is the operator, evaluated whenever the addon is read.
const tick = (a) => {
  const now = Date.now();
  if (a.observedGeneration !== a.generation && now - a.changedAt >= PLAN_MS) {
    a.observedGeneration = a.generation;
    a.plan = planFor(a);
    const refused = a.plan.violations.length + a.plan.valuesErrors.length > 0;
    if (a.suspend) {
      // An applied addon keeps reporting its running workloads while a new
      // plan waits; one never applied is planned, or failed to plan.
      if (!a.lastAppliedChart || refused) a.phase = refused ? 'PlanFailed' : 'Planned';
      else if (a.phase === 'PlanFailed' || a.phase === 'Failed') a.phase = 'Ready';
      a.message = refused ? 'the plan has problems; nothing is applied' : 'planned; nothing is applied while suspended';
    } else if (refused) {
      a.phase = 'Failed';
      a.message = 'the plan has problems; the running release is left as it is';
    } else {
      const sameChart = a.lastAppliedChart && shownVersion(a.lastAppliedChart) === shownVersion(a.chart) && a.lastAppliedChart.ref === a.chart.ref;
      a.lastAppliedChart = { ...a.chart };
      if (!sameChart || !a.installStartedAt) {
        a.installStartedAt = now;
        a.readyAt = 0;
      }
      a.phase = 'Installing'; // settles to Ready below once the workloads are
      a.message = '';
    }
  }
  if (!a.suspend && a.installStartedAt && a.phase !== 'Failed') {
    const elapsed = now - a.installStartedAt;
    a.components = [
      { name: a.name, kind: 'Deployment', ready: elapsed >= WORKLOAD_MS ? 1 : 0, desired: 1, reason: '' },
      { name: `${a.name}-worker`, kind: 'Deployment', ready: elapsed >= 2 * WORKLOAD_MS ? 1 : 0, desired: 1, reason: '' },
    ];
    const ready = a.components.every((c) => c.ready >= c.desired);
    if (ready && !a.readyAt) a.readyAt = now;
    if (a.observedGeneration === a.generation) a.phase = ready ? 'Ready' : 'Installing';
    if (ready && now - a.readyAt >= REGISTER_MS) a.registered = true;
  }
};

const chartView = (a) => {
  tick(a);
  return {
    name: a.name, chart: a.chart, suspended: a.suspend, phase: a.phase, message: a.message,
    generation: a.generation, observedGeneration: a.observedGeneration, plan: a.plan,
    components: a.components, lastAppliedChart: a.lastAppliedChart, values: a.values,
    secretKeys: [...a.inputs.keys()].sort(), secretRefs: Object.fromEntries(a.inputs), registered: a.registered,
  };
};

// The addons list row of a chart addon, as GET /addons merges it.
const chartRow = (a) => {
  tick(a);
  const primary = a.plan?.chart.annotations['zaentrum.io/primary'];
  return {
    key: a.name,
    title: a.plan?.chart.annotations['zaentrum.io/title'] || a.name,
    proxyUrl: a.registered ? `http://${a.name}` : '',
    version: a.registered ? '2.0.0' : '',
    installedAt: '2026-09-15T08:00:00Z', refreshedAt: '2026-09-15T08:00:00Z',
    tiles: a.registered ? 1 : 0, slots: 0,
    components: a.components.map((c) => ({
      name: c.name, workload: c.name, role: c.name === primary ? 'primary' : 'required', summary: '',
      phase: c.desired === 0 ? 'stopped' : c.ready >= c.desired ? 'ready' : c.ready === 0 ? 'degraded' : 'progressing',
      ready: c.ready, desired: c.desired, restarts: 0, reason: c.reason,
    })),
    setup: null,
    refreshAvailable: false,
    chart: { ...a.chart, lastApplied: a.lastAppliedChart },
    phase: a.phase, suspended: a.suspend, registered: a.registered,
  };
};

// writeInputs is what portal-api does with secret inputs: the values of one
// write go to a new Secret, references point at Secrets that exist.
const writeInputs = (a, body) => {
  const set = Object.keys(body.secretValues ?? {});
  const secret = `zaentrum-addon-${a.name}-values-${Math.random().toString(36).slice(2, 7)}`;
  set.forEach((path) => a.inputs.set(path, { name: secret, key: path }));
  Object.entries(body.secretRefs ?? {}).forEach(([path, ref]) => a.inputs.set(path, { name: ref.name, key: ref.key || path }));
  (body.clearSecrets ?? []).forEach((path) => a.inputs.delete(path));
  return set.length > 0 || Object.keys(body.secretRefs ?? {}).length > 0 || (body.clearSecrets ?? []).length > 0;
};

const newChartAddon = (name, chart, values) => ({
  name, chart, values, inputs: new Map(), suspend: true,
  generation: 1, observedGeneration: 0, changedAt: Date.now(), phase: 'Pending', message: '',
  plan: null, components: [], lastAppliedChart: null, registered: false, installStartedAt: 0, readyAt: 0,
});

// Seeded: "sample", installed from its chart and registered, one version behind.
{
  const sample = newChartAddon('sample', { ref: 'oci://registry.example.org/charts/sample', version: '1.0.0' }, { replicas: 2 });
  sample.inputs.set('config.password', { name: 'zaentrum-addon-sample-values-7kq2x', key: 'config.password' });
  sample.suspend = false;
  sample.observedGeneration = 1;
  sample.plan = planFor(sample);
  sample.phase = 'Ready';
  sample.lastAppliedChart = { ...sample.chart };
  sample.installStartedAt = Date.now() - 60_000;
  sample.readyAt = Date.now() - 50_000;
  sample.registered = true;
  chartAddons.set('sample', sample);
}

// accepted is how portal-api answers a write: the generation it made, the one
// planned so far, and the chart the addon names.
const accepted = (a) => ({ name: a.name, generation: a.generation, observedGeneration: a.observedGeneration, chart: a.chart });

const chartsMode = (req) => (req.headers.cookie ?? '').match(/(?:^|;\s*)mock-charts=([a-z]*)/)?.[1] ?? '';

// serveCharts answers /api/portal/addon-charts…; false when the request is not one.
async function serveCharts(req, res, pathname, query) {
  if (!pathname.startsWith('/api/portal/addon-charts')) return false;
  const mode = chartsMode(req);
  if (mode === 'off') return text(res, 404, '404 page not found'), true;
  const rest = pathname.slice('/api/portal/addon-charts'.length).split('/').filter(Boolean);
  if (rest.length === 0 && req.method === 'GET') {
    return json(res, 200, mode === 'nocrd'
      ? { available: false, note: 'this cluster serves no ZaentrumAddon resource — update the zaentrum-operator to install addons from charts' }
      : { available: true, note: '' }), true;
  }
  if (mode === 'nocrd') return text(res, 503, 'this cluster serves no ZaentrumAddon resource — update the zaentrum-operator to install addons from charts'), true;

  if (rest.length === 0 && req.method === 'POST') {
    const body = await readBody(req);
    if (!body) return text(res, 400, 'invalid json'), true;
    const { chart, error } = normaliseChart(body.chart, body.version, body.digest);
    if (error) return text(res, 400, error), true;
    const name = (body.name ?? '').trim() || defaultName(chart.ref);
    if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name) || name.length > 40) {
      return text(res, 400, `name "${name}" must be a DNS-1123 label of at most 40 characters: lower-case letters, digits and '-'`), true;
    }
    if (body.values && (typeof body.values !== 'object' || Array.isArray(body.values))) return text(res, 400, 'values must be a JSON object'), true;
    const taken = addons.find((a) => a.key === name);
    if (taken && !chartAddons.has(name)) {
      return text(res, 409, `an addon "${name}" is already installed from ${taken.proxyUrl} — remove it first, or choose another name`), true;
    }
    const existing = chartAddons.get(name);
    if (existing) {
      Object.assign(existing, { chart, values: body.values ?? null, suspend: true, changedAt: Date.now() });
      writeInputs(existing, body);
      existing.generation++;
    } else {
      const a = newChartAddon(name, chart, body.values ?? null);
      writeInputs(a, body);
      chartAddons.set(name, a);
    }
    return json(res, 202, accepted(chartAddons.get(name))), true;
  }

  const a = chartAddons.get(rest[0]);
  if (!a) return text(res, 404, `no addon "${rest[0]}" is installed from a chart`), true;

  if (rest.length === 1 && req.method === 'GET') return json(res, 200, chartView(a)), true;

  if (rest.length === 2 && rest[1] === 'install' && req.method === 'POST') {
    tick(a);
    const current = a.plan && a.observedGeneration === a.generation;
    if (!current) return text(res, 409, 'the addon has no plan for its current configuration yet — wait for the operator to plan it'), true;
    const blockers = [...a.plan.violations, ...a.plan.valuesErrors];
    if (blockers.length) return text(res, 409, `the plan refuses the chart: ${blockers.join('; ')}`), true;
    if (a.suspend) {
      a.suspend = false;
      a.generation++;
      a.changedAt = Date.now();
      a.phase = 'Installing';
      a.message = '';
    }
    return json(res, 202, accepted(a)), true;
  }

  if (rest.length === 1 && req.method === 'PATCH') {
    const body = await readBody(req);
    if (!body) return text(res, 400, 'invalid json'), true;
    const clear = body.clearSecrets ?? [];
    if (clear.some((k) => k in (body.secretValues ?? {}))) return text(res, 400, 'a secret input is both set and cleared'), true;
    let changed = false;
    if (body.chart !== undefined || body.version !== undefined || body.digest !== undefined) {
      const ref = body.chart ?? a.chart.ref;
      const version = body.version !== undefined ? body.version : body.chart !== undefined && /:[^/]+$/.test(body.chart.replace(/^oci:\/\//, '')) ? '' : a.chart.version;
      const { chart, error } = normaliseChart(ref, version, body.digest !== undefined ? body.digest : undefined);
      if (error) return text(res, 400, error), true;
      const applied = a.lastAppliedChart;
      const runs = applied && applied.ref === chart.ref && (applied.version ?? '') === (chart.version ?? '') && (!chart.digest || chart.digest === applied.digest);
      changed = !runs || chart.ref !== a.chart.ref || (chart.version ?? '') !== (a.chart.version ?? '') || (chart.digest ?? '') !== (a.chart.digest ?? '');
      a.chart = chart;
    }
    if ('values' in body) a.values = body.values;
    writeInputs(a, body);
    if (body.suspend !== undefined) a.suspend = body.suspend;
    else if (changed) a.suspend = true;
    a.generation++;
    a.changedAt = Date.now();
    return json(res, 202, accepted(a)), true;
  }

  if (rest.length === 1 && req.method === 'DELETE') {
    const keep = query.get('keepValues');
    if (keep && keep !== 'true' && keep !== 'false') return text(res, 400, 'keepValues must be true or false'), true;
    chartAddons.delete(a.name);
    console.log(`mock: removed chart addon ${a.name}${keep === 'true' ? ' (values Secrets kept)' : ''}`);
    return json(res, 200, { name: a.name, resource: true, keptValues: keep === 'true', warnings: [] }), true;
  }
  return text(res, 405, 'method not allowed'), true;
}

createServer(async (req, res) => {
  res.setHeader('Access-Control-Allow-Origin', '*');
  const url = req.url;
  const { pathname, searchParams } = new URL(url, 'http://mock');
  if (await serveCharts(req, res, pathname, searchParams)) return;
  if (await serveRegistry(req, res, pathname)) return;
  if (pathname === '/api/portal/me') {
    return json(res, 200, { username: 'harness', roles: ['zaentrum-admin', 'zaentrum-user'], isAdmin: true, adminRole: 'zaentrum-admin', client: 'zaentrum-web' });
  }
  // Through the app proxy: the example addon's setup endpoint.
  if (url.startsWith('/api/portal/apps/example/api/setup')) return json(res, 200, setupStatus);
  if (url.startsWith('/api/portal/apps/')) return json(res, 502, { error: 'app unreachable' });
  if (url.startsWith('/api/portal/apps')) return json(res, 200, apps);
  if (url.startsWith('/api/portal/spaces')) return json(res, 200, spaces);
  if (pathname === '/api/portal/operator/verify' && req.method === 'POST') return requestVerification(req, res);
  if (pathname === '/api/portal/operator' && req.method === 'PATCH') {
    const body = await readBody(req);
    if (!body) return text(res, 400, 'invalid json');
    if (body.channel) spec.channel = body.channel;
    if (body.updateMode) spec.updateMode = body.updateMode;
    spec.generation++;
    console.log(`mock: platform now follows ${spec.channel}, updates ${spec.updateMode}`);
    return json(res, 200, { version: spec.version, generation: spec.generation });
  }
  // apply-update names the update it applies, as the server requires; one the
  // operator replaced meanwhile is refused 409. ?update= is not needed: send a
  // stale version by hand to see the refusal.
  if (pathname === '/api/portal/operator/apply-update' && req.method === 'POST') {
    const body = await readBody(req);
    if (!body?.version) return text(res, 400, 'apply-update names the update it applies: {"version": "<operator.availableUpdate>"}');
    if (body.version !== spec.availableUpdate) {
      return text(res, 409, `the available update changed: ${spec.availableUpdate} is on the ${spec.channel} channel now, not ${body.version}`);
    }
    spec.version = body.version;
    spec.generation++;
    console.log(`mock: platform pinned to ${body.version}`);
    return json(res, 200, { version: body.version, generation: spec.generation });
  }
  const write = pathname.match(/^\/api\/portal\/operator\/instances\/([^/]+)\/(scale|restart)$/);
  if (write && req.method === 'POST') {
    const i = instances.find((x) => x.name === write[1]);
    if (!i) return text(res, 404, `no such workload: ${write[1]}`);
    if (i.protected) return text(res, 400, `"${i.name}" is a protected (stateful) service and cannot be ${write[2] === 'scale' ? 'scaled' : 'restarted'} from here`);
    if (write[2] === 'scale') {
      const body = await readBody(req);
      if (i.adminStack && body?.replicas === 0) {
        return text(res, 400, `"${i.name}" keeps at least one replica here — the admin console runs on it, and scaling it to 0 would leave nothing to scale it back up from`);
      }
      i.desiredReplicas = body?.replicas ?? i.desiredReplicas;
      return json(res, 200, { name: i.name, generation: ++i.generation });
    }
    if (i.adminStack && i.strategy === 'Recreate') return text(res, 400, `"${i.name}" is not restarted from here — its Deployment recreates its pods`);
    i.restartedAt = new Date().toISOString();
    return json(res, 200, { name: i.name, generation: ++i.generation, restartedAt: i.restartedAt });
  }
  if (url.startsWith('/api/portal/operator')) return json(res, 200, operatorState(req));
  if (url.startsWith('/api/portal/addons') && req.method === 'GET') {
    // An older portal-api lists what the registry holds; this one merges the
    // chart addons, registered or not.
    const charted = chartsMode(req) === '' ? [...chartAddons.values()].map(chartRow) : [];
    return json(res, 200, [...addons, ...charted]);
  }
  if (url.startsWith('/api/portal/addons') && req.method === 'POST') {
    const body = await readBody(req);
    const a = addons[0];
    // Every address serves example's manifest here, so any other address is a
    // move: check reports it, install refuses it until it is confirmed.
    const moved = body.proxyUrl !== a.proxyUrl;
    if (moved && !body.dryRun && !body.replaceAddress) {
      res.writeHead(409, { 'Content-Type': 'text/plain' });
      return res.end(`addon "example" is installed from ${a.proxyUrl} — installing it from ${body.proxyUrl} moves it there; check shows the move, and installing must confirm it with replaceAddress`);
    }
    return json(res, 200, {
      key: a.key, app: { ...apps[3], proxyUrl: body.proxyUrl }, space: null, tiles: a.tiles, slots: a.slots, commands: 2, checks: 1,
      version: a.version, components: a.components, setup, refresh: true, adopt: false,
      previousAddress: moved ? a.proxyUrl : '', dryRun: !!body.dryRun,
    });
  }
  if (url.startsWith('/api/portal/addons/') && req.method === 'DELETE') {
    const key = pathname.split('/').pop();
    if (chartAddons.has(key)) {
      return text(res, 409, `addon "${key}" is installed from a chart — remove it as a chart addon (DELETE /api/portal/addon-charts/${key})`);
    }
    return json(res, 200, { removed: { tiles: 3, rows: 1, space: '' }, remainingWorkloads: ['example', 'example-worker'] });
  }
  json(res, 200, []);
}).listen(8791, () => console.log('mock portal-api on :8791'));
