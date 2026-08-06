// Serves the portal-api surface the operator console reads, shaped exactly like
// live zaentrum-beta (14 platform, 5 addons) plus one deliberately unclaimed
// workload so all three groups render.
import { createServer } from 'node:http';

const platform = [
  'analyzer','chino-api','chino-stream','chino-web','katalog-api','katalog-ingest',
  'katalog-manage-ui','katalog-manager-api','katalog-manager-ui','packager',
  'portal-api','transcoder','valkey','zaentrum-portal',
];
const addons = ['acquire','download-gateway','nzbget','prowlarr','qbittorrent'];

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

createServer((req, res) => {
  res.setHeader('Access-Control-Allow-Origin', '*');
  if (req.url.startsWith('/api/portal/operator')) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    return res.end(body);
  }
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end('[]');
}).listen(8791, () => console.log('mock portal-api on :8791'));
