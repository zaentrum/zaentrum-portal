import type { BadgeTone } from '@nalet/design-system';
import type { Confirmation } from './confirm';
import type { PeopleStep } from './people.ts';
import { since } from './verification.ts';

// The setup checklist, worded for the launchpad.
//
// portal-api reads every step live — the catalog with the admin's own bearer,
// the operator's resource, the cluster — and says each one's state (GET
// /api/portal/setup). What is here is the reading: what a state means for
// that step, in a short label and a sentence, and what to offer next. Pure,
// so npm test runs it under node.

// ─── the document (mirror server/internal/api/setup.go) ─────────────────────

export type StepState = 'done' | 'todo' | 'working' | 'optional' | 'unknown' | 'info';

export interface SetupStep {
  state: StepState;
  // note: why the state is unknown.
  note?: string;
}

export interface SetupCompletion {
  at: string;
  by: string;
}

export interface ScanJob {
  id: string;
  status: string; // running | done | failed
  startedAt: string | null;
  finishedAt: string | null;
  error?: string;
  filesSeen: number;
  itemsInserted: number;
  itemsUpdated: number;
}

export interface SetupMetadata extends SetupStep {
  // key: where the TMDB key in effect comes from — never the key.
  key: 'setting' | 'environment' | 'none';
  updatedAt?: string;
}

export interface SetupLibrary extends SetupStep {
  titles: number;
  titlesMore: boolean;
  path: string;
  volume: string;
  folder: string;
  appliance: boolean;
  scan: ScanJob | null;
}

export interface SetupWorker {
  name: string;
  phase: string; // ready | progressing | degraded | stopped | absent
  reason: string;
  ready: number;
  desired: number;
}

export interface SetupProcessing extends SetupStep {
  pipeline: boolean | null;
  gpu: boolean | null;
  gpuNodes: number | null;
  gpuNote?: string;
  workers: SetupWorker[];
  switchable: boolean;
}

export interface SetupDevices extends SetupStep {
  origin: string;
  https: boolean;
  issuer: string;
  issuerHttps: boolean;
  localOnly: boolean;
  source: string; // operator | request
}

export interface SetupDoc {
  completed: SetupCompletion | null;
  metadata: SetupMetadata;
  library: SetupLibrary;
  processing: SetupProcessing;
  devices: SetupDevices;
  // people: where accounts are made — the People page (mode bundled), an
  // identity provider (external, manageUrl), or not set up (unavailable,
  // note). No mode from an older portal-api.
  people: PeopleStep;
}

export type StepKey = 'metadata' | 'library' | 'processing' | 'devices' | 'people';

// The steps in the order the card lists them, with their labels.
export const STEPS: { key: StepKey; title: string }[] = [
  { key: 'metadata', title: 'Metadata' },
  { key: 'library', title: 'Library' },
  { key: 'processing', title: 'Processing' },
  { key: 'devices', title: 'Devices' },
  { key: 'people', title: 'People' },
];

// Where to read more. The self-hosting guide is the one the docs keep.
export const DOCS = 'https://github.com/zaentrum/zaentrum/blob/main/docs/self-hosting.md';
export const DOCS_FIRST_RUN = `${DOCS}#first-run`;
export const DOCS_ADMIN_CONSOLE = `${DOCS}#the-admin-console`;
export const TMDB_KEYS = 'https://www.themoviedb.org/settings/api';

// ─── the card ────────────────────────────────────────────────────────────────

// showChecklist: the launchpad shows the card to an admin until setup is
// marked done. Nothing to show while the record is still being read (null),
// nor against an older portal-api, which answers the record with a 404
// (undefined here).
export function showChecklist(isAdmin: boolean, completed: SetupCompletion | null | undefined, loaded: boolean): boolean {
  return isAdmin && loaded && completed === null;
}

// stepChip is a step's badge: a short label and its tone.
export function stepChip(key: StepKey, s: SetupStep): { label: string; tone: BadgeTone } {
  switch (s.state) {
    case 'done':
      return { label: 'Done', tone: 'green' };
    case 'todo':
      return { label: 'To Do', tone: 'amber' };
    case 'working':
      return { label: key === 'library' ? 'Scanning' : 'Starting', tone: 'blue' };
    case 'optional':
      return { label: 'Optional', tone: 'neutral' };
    case 'info':
      return { label: 'Manual', tone: 'neutral' };
    default:
      return { label: 'Unknown', tone: 'neutral' };
  }
}

// The steps that count towards "done": people is nothing the platform can
// check. An optional step — the pipeline off — counts as done.
const COUNTED: StepKey[] = ['metadata', 'library', 'processing', 'devices'];

const finished = (s: SetupStep) => s.state === 'done' || s.state === 'optional';

// progress is how many of the counted steps are done: "2 of 4".
export function progress(doc: SetupDoc): { done: number; total: number } {
  return { done: COUNTED.filter((k) => finished(doc[k])).length, total: COUNTED.length };
}

// openSteps are the counted steps not done yet, in the card's order.
export function openSteps(doc: SetupDoc): StepKey[] {
  return COUNTED.filter((k) => !finished(doc[k]));
}

// polls: a step under way is read again every few seconds — a scan running,
// the pipeline's workers starting — and only then.
export function polls(doc: SetupDoc | null): boolean {
  return !!doc && (doc.library.state === 'working' || doc.processing.state === 'working');
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

// ─── metadata ────────────────────────────────────────────────────────────────

// tmdbKeyProblem is why what was pasted cannot be the token katalog-manager
// signs in to TMDB with, or null. The catalog sends it as a bearer, which is
// TMDB's API read access token (v4, a JWT: eyJ…); the shorter API key (v3)
// is refused there, and enrichment would fail without a word.
export function tmdbKeyProblem(raw: string): string | null {
  const key = raw.trim();
  if (!key) return 'Paste the token first.';
  if (/\s/.test(key)) return 'A TMDB token has no spaces or line breaks — paste it again.';
  if (/^[0-9a-f]{32}$/i.test(key)) {
    return 'That is the API key (v3). The catalog needs the API read access token (v4) from the same page — it starts with eyJ.';
  }
  return null;
}

// metadataText is the step's sentence.
export function metadataText(m: SetupMetadata, now: number): string {
  if (m.state === 'unknown') return 'Cannot tell whether a TMDB key is set.';
  switch (m.key) {
    case 'setting': {
      const when = since(m.updatedAt, now);
      return `A TMDB key is set${when ? `, ${when}` : ''}. Titles get their posters, plots and cast from TMDB.`;
    }
    case 'environment':
      return "The catalog uses the server's own TMDB key. A key saved here takes its place.";
    default:
      return 'Titles, posters and plots come from TMDB. Paste a TMDB API read access token: it is saved to the catalog and never shown again.';
  }
}

// ─── library ─────────────────────────────────────────────────────────────────

// stepNote is why a step's state is unknown, as portal-api says it, for the
// line under the step's sentence; '' for every other state.
export function stepNote(s: SetupStep): string {
  if (s.state !== 'unknown') return '';
  const note = (s.note ?? '').trim();
  return note ? `${note.charAt(0).toUpperCase()}${note.slice(1)}${/[.!?]$/.test(note) ? '' : '.'}` : 'No answer.';
}

// libraryText is the step's sentence: what the catalog holds — or that it
// cannot be told — and where files go.
export function libraryText(l: SetupLibrary): string {
  return l.state === 'unknown' ? `Cannot tell what the catalog holds. ${filesText(l)}` : `${titlesText(l)} ${filesText(l)}`;
}

// titlesText says what the catalog holds: "No titles yet", "214 titles",
// "400+ titles" when the count is a floor.
export function titlesText(l: Pick<SetupLibrary, 'titles' | 'titlesMore'>): string {
  if (l.titles === 0) return 'No titles yet.';
  return l.titlesMore ? `${l.titles}+ titles.` : `${plural(l.titles, 'title', 'titles')}.`;
}

// filesText says where files go: the folder of the volume, and the path the
// catalog reads it as — the first files, or more of them.
export function filesText(l: Pick<SetupLibrary, 'path' | 'volume' | 'folder' | 'titles'>): string {
  const more = l.titles > 0;
  if (!l.volume) {
    return more
      ? `More files go to ${l.path}, where the catalog reads them; scan again after copying.`
      : `Copy your files to ${l.path}, where the catalog reads them, then scan.`;
  }
  const folder = l.folder ? `the ${l.folder}/ folder of the ${l.volume} volume` : `the ${l.volume} volume`;
  return more
    ? `More files go into ${folder} (${l.path} to the catalog); scan again after copying.`
    : `Copy your files into ${folder} — the catalog reads it as ${l.path} — then scan.`;
}

// STALE_SCAN_MS: a scan that has run this long without ending has probably
// stopped — its catalog manager restarted under it — and may be started
// again.
export const STALE_SCAN_MS = 30 * 60 * 1000;

// scanRunning: a scan is under way, and not one that has stopped.
export function scanRunning(scan: ScanJob | null, now: number): boolean {
  if (!scan || scan.status !== 'running') return false;
  const started = scan.startedAt ? Date.parse(scan.startedAt) : NaN;
  return Number.isNaN(started) || now - started < STALE_SCAN_MS;
}

// scanLabel is the scan button's word.
export function scanLabel(l: Pick<SetupLibrary, 'scan'>, now: number): string {
  if (scanRunning(l.scan, now)) return 'Scanning';
  return l.scan ? 'Scan Again' : 'Scan Now';
}

// scanText says how the latest scan went.
export function scanText(l: Pick<SetupLibrary, 'scan' | 'path'>, now: number): string {
  const s = l.scan;
  if (!s) return 'No scan has run yet.';
  const started = since(s.startedAt ?? undefined, now);
  if (s.status === 'running') {
    return scanRunning(s, now)
      ? `Scanning, started ${started || 'just now'} — new titles appear as it finds them.`
      : `A scan started ${started} has not ended; it may have stopped. Scan again.`;
  }
  const ended = since(s.finishedAt ?? s.startedAt ?? undefined, now);
  if (s.status === 'failed') return `The last scan failed${ended ? `, ${ended}` : ''}: ${s.error || 'no reason given'}.`;
  if (s.filesSeen === 0) return `The last scan${ended ? `, ${ended},` : ''} found no files in ${l.path}.`;
  const counts = [plural(s.filesSeen, 'file', 'files'), `${s.itemsInserted} new`];
  if (s.itemsUpdated) counts.push(`${s.itemsUpdated} updated`);
  return `Last scan${ended ? ` ${ended}` : ''}: ${counts.join(', ')}.`;
}

// ─── processing ──────────────────────────────────────────────────────────────

// workerName is a pipeline workload as people say it.
const workerName = (name: string) => (name === 'katalog-ingest' ? 'catalog ingest' : name);

const list = (names: string[]) =>
  names.length <= 1 ? names.join('') : `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;

// reasonText words a fault the cluster names.
function reasonText(w: SetupWorker): string {
  switch (w.reason) {
    case 'Unschedulable':
      return w.name === 'transcoder' ? 'no node offers the NVIDIA GPU it asks for (nvidia.com/gpu)' : 'no node can take it';
    case 'ImagePullBackOff':
    case 'ErrImagePull':
      return 'its image cannot be pulled';
    case 'CrashLoopBackOff':
      return 'it keeps crashing';
    default:
      return w.reason;
  }
}

// gpuText says whether a node offers a GPU, or that it cannot be told.
export function gpuText(p: Pick<SetupProcessing, 'gpuNodes'>): string {
  if (p.gpuNodes === null) return 'Whether a node here offers a GPU cannot be told from the portal.';
  if (p.gpuNodes === 0) return 'No node offers a GPU, which the transcoder needs.';
  return `${plural(p.gpuNodes, 'node offers', 'nodes offer')} a GPU.`;
}

// processingText is the step's sentence.
export function processingText(p: SetupProcessing): string {
  switch (p.state) {
    case 'unknown':
      return 'Cannot tell whether the media pipeline runs.';
    case 'optional':
      return 'Files stream as they are. The media pipeline analyzes, transcodes and packages them for adaptive streaming; its transcoder needs an NVIDIA GPU.';
    case 'working': {
      const starting = p.workers.filter((w) => w.phase !== 'ready').map((w) => workerName(w.name));
      return `The media pipeline is starting: ${list(starting)} ${starting.length === 1 ? 'is' : 'are'} not ready yet.`;
    }
    case 'todo': {
      const faulty = p.workers.filter((w) => w.reason);
      if (faulty.length === 1) {
        return `The media pipeline is on, but the ${workerName(faulty[0].name)} cannot run: ${reasonText(faulty[0])}.`;
      }
      const faults = faulty.map((w) => `${workerName(w.name)}: ${reasonText(w)}`);
      return `The media pipeline is on, but ${list(faulty.map((w) => workerName(w.name)))} cannot run — ${faults.join('; ')}.`;
    }
    default:
      return `The media pipeline runs: ${list(p.workers.map((w) => workerName(w.name)))} ${p.workers.length === 1 ? 'is' : 'are'} ready.`;
  }
}

// pipelineConfirmation asks before the pipeline is switched: what starts or
// stops, and what the transcoder needs.
export function pipelineConfirmation(p: SetupProcessing, on: boolean): Confirmation {
  if (on) {
    const lines = [
      'the operator starts the analyzer, the transcoder, the packager and the catalog ingest.',
      'each title is analyzed, transcoded and packaged for adaptive streaming once it is in the catalog.',
    ];
    lines.push(
      p.gpuNodes === 0
        ? 'the transcoder needs an NVIDIA GPU, and no node offers one: it waits, and titles go no further than analysis.'
        : p.gpuNodes === null
          ? 'the transcoder needs an NVIDIA GPU (nvidia.com/gpu); without a node that offers one it waits, and titles go no further than analysis.'
          : 'the transcoder runs on a node that offers an NVIDIA GPU.',
    );
    return { title: 'Turn On the Media Pipeline?', lines, confirm: 'Turn On' };
  }
  return {
    title: 'Turn Off the Media Pipeline?',
    lines: [
      'the operator stops the analyzer, the transcoder, the packager and the catalog ingest.',
      'what is packaged stays playable; new titles stream as they are.',
    ],
    confirm: 'Turn Off',
    danger: true,
  };
}

// ─── devices ─────────────────────────────────────────────────────────────────

const hostOf = (origin: string) => {
  try {
    return new URL(origin).host;
  } catch {
    return origin;
  }
};

// devicesText is the step's sentence.
export function devicesText(d: SetupDevices): string {
  if (d.state === 'unknown') return 'Cannot tell how the platform is reached.';
  if (d.state === 'done') return `Phones and TVs can sign in: ${hostOf(d.origin)} answers over https.`;
  if (d.localOnly) {
    return `${hostOf(d.origin)} answers on this machine only. Phones and TVs need a name they reach, served over https.`;
  }
  if (!d.https) {
    return `Phones and TVs sign in over https only, and ${hostOf(d.origin)} answers over http. Put TLS in front of the platform and set identity.issuerScheme to https.`;
  }
  return `Sign-in (${hostOf(d.issuer) || 'the issuer'}) answers over http; phones and TVs sign in over https only.`;
}

// ─── people ──────────────────────────────────────────────────────────────────
//
// The step's sentence is people.ts's peopleStepText: the People page with
// the platform's own realm, the identity provider with an external one.

// ─── done ────────────────────────────────────────────────────────────────────

const OPEN: Record<StepKey, string> = {
  metadata: 'metadata: without a TMDB key, titles keep their file names and get no posters or plots.',
  library: 'library: the catalog holds no titles yet.',
  processing: 'processing: the media pipeline is not running as it should.',
  devices: 'devices: phones and TVs cannot sign in yet.',
  people: '',
};

// completeConfirmation asks before setup is marked done while steps are
// open; null when every counted step is done, which needs no question.
export function completeConfirmation(doc: SetupDoc): Confirmation | null {
  const open = openSteps(doc);
  if (open.length === 0) return null;
  return {
    title: 'Mark Setup Done?',
    lines: [
      ...open.map((k) => OPEN[k]),
      'the checklist leaves the launchpad; settings shows it again.',
    ],
    confirm: 'Mark Done',
  };
}

// completedText says who marked setup done, and when.
export function completedText(c: SetupCompletion, now: number): string {
  const when = since(c.at, now);
  return `Setup was marked done${c.by ? ` by ${c.by}` : ''}${when ? `, ${when}` : ''}.`;
}
