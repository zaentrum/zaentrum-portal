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
  readyReplicas: broken ? 0 : 1,
  updatedReplicas: 1,
  availableReplicas: broken ? 0 : 1,
  restarts: broken ? 3 : 0,
  phase: broken ? 'degraded' : 'ready',
  protected: name === 'valkey',
  operatorManaged: group === 'platform',
  alwaysPull: true,
  group,
  reason: broken ? 'ImagePullBackOff' : '',
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

const operator = JSON.stringify({
  available: true,
  operator: { present: true, name: 'zaentrum', channel: 'stable', version: 'v0.3.0', phase: 'Degraded', components: [] },
  instances,
});

// The registry, as seeded by migrations 002/003/004 — note every seeded app has
// an EMPTY proxyUrl, which is the state that made the "embeddable" column worth
// adding: nothing in a fresh install can be hosted inside the portal shell.
const apps = [
  { key: 'chino', title: 'chino', description: 'films & series', baseUrl: 'https://chino.example.com',
    kind: 'product', healthUrl: '', icon: 'film', enabled: true, proxyUrl: '' },
  { key: 'katalog', title: 'katalog', description: 'browse the catalog', baseUrl: '/katalog',
    kind: 'admin', healthUrl: '', icon: 'library', enabled: true, proxyUrl: '' },
  { key: 'katalog-manage', title: 'katalog-manage', description: 'manage the catalog', baseUrl: '/katalog-manage',
    kind: 'admin', healthUrl: '', icon: 'settings', enabled: true, proxyUrl: '' },
  // One app WITH a proxyUrl, so the embeddable column shows both states.
  { key: 'example', title: 'Example', description: 'the example addon', baseUrl: '/portal/app/example',
    kind: 'tool', healthUrl: '', icon: 'puzzle', enabled: true, proxyUrl: 'http://example' },
];
const spaces = [{ key: 'apps', title: 'apps', order: 10 }, { key: 'manage', title: 'manage', order: 20 }];

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

const readBody = (req) =>
  new Promise((resolve) => {
    let b = '';
    req.on('data', (c) => (b += c));
    req.on('end', () => resolve(b ? JSON.parse(b) : {}));
  });

createServer(async (req, res) => {
  res.setHeader('Access-Control-Allow-Origin', '*');
  const url = req.url;
  // Through the app proxy: the example addon's setup endpoint.
  if (url.startsWith('/api/portal/apps/example/api/setup')) return json(res, 200, setupStatus);
  if (url.startsWith('/api/portal/apps/')) return json(res, 502, { error: 'app unreachable' });
  if (url.startsWith('/api/portal/apps')) return json(res, 200, apps);
  if (url.startsWith('/api/portal/spaces')) return json(res, 200, spaces);
  if (url.startsWith('/api/portal/operator')) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    return res.end(operator);
  }
  if (url.startsWith('/api/portal/addons') && req.method === 'GET') return json(res, 200, addons);
  if (url.startsWith('/api/portal/addons') && req.method === 'POST') {
    const body = await readBody(req);
    const a = addons[0];
    return json(res, 200, {
      key: a.key, app: apps[3], space: null, tiles: a.tiles, slots: a.slots, commands: 2, checks: 1,
      version: a.version, components: a.components, setup, refresh: body.proxyUrl === a.proxyUrl, dryRun: !!body.dryRun,
    });
  }
  if (url.startsWith('/api/portal/addons/') && req.method === 'DELETE') {
    return json(res, 200, { removed: { tiles: 3, rows: 1, space: '' }, remainingWorkloads: ['example', 'example-worker'] });
  }
  json(res, 200, []);
}).listen(8791, () => console.log('mock portal-api on :8791'));
