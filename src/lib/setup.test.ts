// node --test (type stripping, Node >= 22.18): the setup checklist's reading
// of what portal-api reports. Excluded from the app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  STALE_SCAN_MS,
  completeConfirmation,
  completedText,
  devicesText,
  filesText,
  gpuText,
  metadataText,
  openSteps,
  pipelineConfirmation,
  polls,
  processingText,
  progress,
  scanLabel,
  scanRunning,
  scanText,
  showChecklist,
  stepChip,
  stepNote,
  libraryText,
  titlesText,
  tmdbKeyProblem,
  type SetupDoc,
  type SetupProcessing,
} from './setup.ts';

const now = Date.parse('2026-10-04T08:00:00Z');
const ago = (min: number) => new Date(now - min * 60_000).toISOString();

// A fresh appliance, as portal-api reads it.
const fresh = (): SetupDoc => ({
  completed: null,
  metadata: { state: 'todo', key: 'none' },
  library: { state: 'todo', titles: 0, titlesMore: false, path: '/var/lib/katalog/media', volume: 'media', folder: 'media', appliance: true, scan: null },
  processing: { state: 'optional', pipeline: false, gpu: false, gpuNodes: null, gpuNote: 'no node reads', workers: [], switchable: true },
  devices: { state: 'todo', origin: 'http://zaentrum.localhost', https: false, issuer: 'http://zaentrum.localhost/auth/realms/zaentrum', issuerHttps: false, localOnly: true, source: 'operator' },
  people: { state: 'info' },
});

test('the card is an admin’s, until setup is marked done', () => {
  assert.equal(showChecklist(true, null, true), true);
  assert.equal(showChecklist(false, null, true), false); // not an admin
  assert.equal(showChecklist(true, { at: ago(1), by: 'admin' }, true), false); // marked done
  assert.equal(showChecklist(true, null, false), false); // still reading the record
  assert.equal(showChecklist(true, undefined, true), false); // an older portal-api: no record at all
});

test('each state has its chip', () => {
  const d = fresh();
  assert.deepEqual(stepChip('metadata', d.metadata), { label: 'To Do', tone: 'amber' });
  assert.deepEqual(stepChip('library', { state: 'working' }), { label: 'Scanning', tone: 'blue' });
  assert.deepEqual(stepChip('processing', { state: 'working' }), { label: 'Starting', tone: 'blue' });
  assert.deepEqual(stepChip('processing', d.processing), { label: 'Optional', tone: 'neutral' });
  assert.deepEqual(stepChip('devices', { state: 'done' }), { label: 'Done', tone: 'green' });
  assert.deepEqual(stepChip('people', d.people), { label: 'Manual', tone: 'neutral' });
  assert.deepEqual(stepChip('library', { state: 'unknown', note: 'x' }), { label: 'Unknown', tone: 'neutral' });
});

test('progress counts four steps, the pipeline off among the done ones', () => {
  const d = fresh();
  assert.deepEqual(progress(d), { done: 1, total: 4 }); // processing: optional
  assert.deepEqual(openSteps(d), ['metadata', 'library', 'devices']);
  d.metadata.state = 'done';
  d.library.state = 'working'; // a scan running is not done
  assert.deepEqual(progress(d), { done: 2, total: 4 });
  d.processing.state = 'unknown'; // cannot tell is not done
  assert.deepEqual(openSteps(d), ['library', 'processing', 'devices']);
});

test('a step under way is read again, and only then', () => {
  const d = fresh();
  assert.equal(polls(d), false);
  assert.equal(polls(null), false);
  d.library.state = 'working';
  assert.equal(polls(d), true);
  d.library.state = 'done';
  d.processing.state = 'working';
  assert.equal(polls(d), true);
});

test('a TMDB token is the v4 read access token, pasted whole', () => {
  assert.equal(tmdbKeyProblem('eyJhbGciOiJIUzI1NiJ9.eyJhdWQiOiJ4In0.c2ln'), null);
  assert.equal(tmdbKeyProblem('  eyJhbGciOiJIUzI1NiJ9.a.b\n'), null); // trimmed, as the server trims it
  assert.match(tmdbKeyProblem('') ?? '', /Paste/);
  assert.match(tmdbKeyProblem('eyJ abc') ?? '', /no spaces/);
  // The v3 API key is refused where the catalog sends it, as a bearer.
  assert.match(tmdbKeyProblem('0123456789abcdef0123456789ABCDEF') ?? '', /API key \(v3\)/);
});

test('metadata says where the key comes from, never what it is', () => {
  assert.match(metadataText({ state: 'todo', key: 'none' }, now), /Paste a TMDB API read access token/);
  assert.equal(metadataText({ state: 'done', key: 'setting', updatedAt: ago(3) }, now),
    'A TMDB key is set, 3 min ago. Titles get their posters, plots and cast from TMDB.');
  assert.match(metadataText({ state: 'done', key: 'environment' }, now), /server's own TMDB key/);
  assert.equal(metadataText({ state: 'unknown', key: 'none', note: 'the catalog manager did not answer' }, now),
    'Cannot tell whether a TMDB key is set.');
});

test('an unknown step says why on a line of its own', () => {
  assert.equal(stepNote({ state: 'unknown', note: 'the catalog manager did not answer: dial tcp: no such host' }),
    'The catalog manager did not answer: dial tcp: no such host.');
  assert.equal(stepNote({ state: 'unknown', note: 'no operator detected — managing deployments directly.' }),
    'No operator detected — managing deployments directly.');
  assert.equal(stepNote({ state: 'unknown' }), 'No answer.');
  assert.equal(stepNote({ state: 'todo', note: 'ignored' }), '');
  assert.equal(libraryText({ ...fresh().library, state: 'unknown', note: 'x' }),
    'Cannot tell what the catalog holds. Copy your files into the media/ folder of the media volume — the catalog reads it as /var/lib/katalog/media — then scan.');
  assert.equal(libraryText({ ...fresh().library, titles: 3, state: 'done' }),
    '3 titles. More files go into the media/ folder of the media volume (/var/lib/katalog/media to the catalog); scan again after copying.');
});

test('the library says what it holds and where files go', () => {
  assert.equal(titlesText({ titles: 0, titlesMore: false }), 'No titles yet.');
  assert.equal(titlesText({ titles: 1, titlesMore: false }), '1 title.');
  assert.equal(titlesText({ titles: 214, titlesMore: false }), '214 titles.');
  assert.equal(titlesText({ titles: 400, titlesMore: true }), '400+ titles.');
  assert.equal(filesText(fresh().library),
    'Copy your files into the media/ folder of the media volume — the catalog reads it as /var/lib/katalog/media — then scan.');
  assert.equal(filesText({ path: '/srv/media', volume: '', folder: '', titles: 0 }), 'Copy your files to /srv/media, where the catalog reads them, then scan.');
  assert.equal(filesText({ path: '/var/lib/katalog', volume: 'nas', folder: '', titles: 0 }), 'Copy your files into the nas volume — the catalog reads it as /var/lib/katalog — then scan.');
  // Once there are titles, the next files are more of them.
  assert.equal(filesText({ ...fresh().library, titles: 12 }),
    'More files go into the media/ folder of the media volume (/var/lib/katalog/media to the catalog); scan again after copying.');
  assert.equal(filesText({ path: '/srv/media', volume: '', folder: '', titles: 3 }), 'More files go to /srv/media, where the catalog reads them; scan again after copying.');
});

test('a scan runs, ends, fails — or has stopped and may start again', () => {
  const l = fresh().library;
  assert.equal(scanText(l, now), 'No scan has run yet.');
  assert.equal(scanLabel(l, now), 'Scan Now');

  const running = { id: 'j1', status: 'running', startedAt: ago(2), finishedAt: null, filesSeen: 0, itemsInserted: 0, itemsUpdated: 0 };
  assert.equal(scanRunning(running, now), true);
  assert.equal(scanLabel({ scan: running }, now), 'Scanning');
  assert.match(scanText({ ...l, scan: running }, now), /^Scanning, started 2 min ago/);

  // Running for longer than any scan takes: it stopped under a restart.
  const stuck = { ...running, startedAt: new Date(now - STALE_SCAN_MS - 60_000).toISOString() };
  assert.equal(scanRunning(stuck, now), false);
  assert.equal(scanLabel({ scan: stuck }, now), 'Scan Again');
  assert.match(scanText({ ...l, scan: stuck }, now), /may have stopped/);

  const done = { ...running, status: 'done', finishedAt: ago(1), filesSeen: 14, itemsInserted: 12, itemsUpdated: 2 };
  assert.equal(scanText({ ...l, scan: done }, now), 'Last scan 1 min ago: 14 files, 12 new, 2 updated.');
  assert.equal(scanText({ ...l, scan: { ...done, itemsUpdated: 0, filesSeen: 1, itemsInserted: 1 } }, now), 'Last scan 1 min ago: 1 file, 1 new.');
  assert.equal(scanText({ ...l, scan: { ...done, filesSeen: 0, itemsInserted: 0, itemsUpdated: 0 } }, now),
    'The last scan, 1 min ago, found no files in /var/lib/katalog/media.');
  assert.equal(scanText({ ...l, scan: { ...done, status: 'failed', error: 'permission denied' } }, now),
    'The last scan failed, 1 min ago: permission denied.');
  assert.equal(scanLabel({ scan: done }, now), 'Scan Again');
});

const pipeline = (state: SetupProcessing['state'], workers: SetupProcessing['workers'], gpuNodes: number | null = null): SetupProcessing => ({
  state, pipeline: true, gpu: false, gpuNodes, workers, switchable: true,
});
const worker = (name: string, phase = 'ready', reason = '') => ({ name, phase, reason, ready: phase === 'ready' ? 1 : 0, desired: 1 });

test('processing says what runs, and what keeps a worker from running', () => {
  assert.match(processingText(fresh().processing), /^Files stream as they are/);
  assert.equal(
    processingText(pipeline('working', [worker('analyzer'), worker('katalog-ingest', 'absent'), worker('packager', 'progressing'), worker('transcoder')])),
    'The media pipeline is starting: catalog ingest and packager are not ready yet.',
  );
  assert.equal(
    processingText(pipeline('todo', [worker('analyzer'), worker('transcoder', 'degraded', 'Unschedulable')])),
    'The media pipeline is on, but the transcoder cannot run: no node offers the NVIDIA GPU it asks for (nvidia.com/gpu).',
  );
  assert.equal(
    processingText(pipeline('todo', [worker('analyzer', 'degraded', 'CrashLoopBackOff'), worker('packager', 'degraded', 'ImagePullBackOff')])),
    'The media pipeline is on, but analyzer and packager cannot run — analyzer: it keeps crashing; packager: its image cannot be pulled.',
  );
  assert.equal(
    processingText(pipeline('done', [worker('analyzer'), worker('katalog-ingest'), worker('packager'), worker('transcoder')])),
    'The media pipeline runs: analyzer, catalog ingest, packager and transcoder are ready.',
  );
  assert.equal(processingText({ ...fresh().processing, state: 'unknown', note: 'no operator detected' }), 'Cannot tell whether the media pipeline runs.');
  assert.equal(gpuText({ gpuNodes: null }), 'Whether a node here offers a GPU cannot be told from the portal.');
  assert.equal(gpuText({ gpuNodes: 0 }), 'No node offers a GPU, which the transcoder needs.');
  assert.equal(gpuText({ gpuNodes: 2 }), '2 nodes offer a GPU.');
});

test('switching the pipeline asks first, and says what the transcoder needs', () => {
  const on = pipelineConfirmation(fresh().processing, true);
  assert.equal(on.confirm, 'Turn On');
  assert.equal(on.danger, undefined);
  assert.match(on.lines.join(' '), /analyzer, the transcoder, the packager and the catalog ingest/);
  assert.match(on.lines.join(' '), /without a node that offers one it waits/); // GPU nodes unknown
  assert.match(pipelineConfirmation({ ...fresh().processing, gpuNodes: 0 }, true).lines.join(' '), /no node offers one/);
  assert.match(pipelineConfirmation({ ...fresh().processing, gpuNodes: 1 }, true).lines.join(' '), /runs on a node that offers/);
  const off = pipelineConfirmation(pipeline('done', []), false);
  assert.equal(off.confirm, 'Turn Off');
  assert.equal(off.danger, true);
  assert.match(off.lines.join(' '), /stays playable/);
});

test('devices say why phones and TVs cannot sign in', () => {
  const d = fresh().devices;
  assert.equal(devicesText(d), 'zaentrum.localhost answers on this machine only. Phones and TVs need a name they reach, served over https.');
  assert.match(devicesText({ ...d, origin: 'http://media.example.com', localOnly: false }), /media\.example\.com answers over http/);
  assert.equal(devicesText({ ...d, state: 'done', origin: 'https://media.example.com', https: true, issuerHttps: true, localOnly: false }),
    'Phones and TVs can sign in: media.example.com answers over https.');
  assert.match(devicesText({ ...d, origin: 'https://media.example.com', https: true, localOnly: false, issuer: 'http://sso.example.com/realms/x' }),
    /Sign-in \(sso\.example\.com\) answers over http/);
});

test('marking done asks first while steps are open, and says which', () => {
  const d = fresh();
  const c = completeConfirmation(d);
  assert.ok(c);
  assert.equal(c.confirm, 'Mark Done');
  assert.deepEqual(c.lines.slice(0, 3).map((l) => l.split(':')[0]), ['metadata', 'library', 'devices']);
  assert.match(c.lines[c.lines.length - 1], /settings shows it again/);
  d.metadata.state = 'done';
  d.library.state = 'done';
  d.devices.state = 'done';
  assert.equal(completeConfirmation(d), null); // nothing open: nothing to ask
  assert.equal(completedText({ at: ago(5), by: 'admin' }, now), 'Setup was marked done by admin, 5 min ago.');
});
