// Serves the portal-api surface the operator console reads, shaped exactly like
// live zaentrum-beta (14 platform, 5 addons) plus one deliberately unclaimed
// workload so all three groups render.
import { createServer } from 'node:http';

const platform = [
  'analyzer','chino-api','chino-stream','chino-web','katalog-api','katalog-ingest',
  'katalog-manage-ui','katalog-manager-api','katalog-manager-ui','packager',
  'portal-api','transcoder','valkey','zaentrum-portal',
];
// Names are deliberately generic: the harness exercises the GROUPING, not
// any particular addon, and this repo is public.
const addons = ['addon-alpha','addon-beta','addon-gamma','addon-delta','addon-epsilon'];

// 13 of the 14 platform services are in ImagePullBackOff right now; valkey is
// the one still up. Reproduced faithfully so the reason column is exercised.
const inst = (name, group, broken) => ({
  name,
  image: `ghcr.io/zaentrum/${name}:latest`,
  desiredReplicas: 1,
  readyReplicas: broken ? 0 : 1,
  updatedReplicas: 1,
  availableReplicas: broken ? 0 : 1,
  restarts: 0,
  phase: broken ? 'degraded' : 'ready',
  protected: name === 'valkey',
  operatorManaged: group === 'platform',
  alwaysPull: true,
  group,
  reason: broken ? 'ImagePullBackOff' : '',
});

const instances = [
  ...platform.map((n) => inst(n, 'platform', n !== 'valkey')),
  ...addons.map((n) => inst(n, 'addon', false)),
  { ...inst('leftover-job-runner', 'other', false), image: 'docker.io/library/busybox:latest' },
];

const body = JSON.stringify({
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
  { key: 'acquire', title: 'acquire', description: 'requests & acquisition', baseUrl: '/acquire',
    kind: 'admin', healthUrl: 'http://acquire/readyz', icon: 'download', enabled: true,
    proxyUrl: 'http://acquire' },
];
const spaces = [{ key: 'default', title: 'default', apps: apps.map((a) => a.key) }];

createServer((req, res) => {
  res.setHeader('Access-Control-Allow-Origin', '*');
  if (req.url.startsWith('/api/portal/apps')) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    return res.end(JSON.stringify(apps));
  }
  if (req.url.startsWith('/api/portal/spaces')) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    return res.end(JSON.stringify(spaces));
  }
  if (req.url.startsWith('/api/portal/operator')) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    return res.end(body);
  }
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end('[]');
}).listen(8791, () => console.log('mock portal-api on :8791'));
