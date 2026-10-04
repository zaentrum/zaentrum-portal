import { useCallback, useEffect, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { Badge, Button, Card, Input, Spinner, Text } from '@nalet/design-system';
import { Check, ExternalLink, KeyRound, Power, ScanSearch, Server } from 'lucide-react';
import { usePortalApi } from '../lib/api';
import type { Confirmation } from '../lib/confirm';
import {
  DOCS,
  DOCS_ADMIN_CONSOLE,
  DOCS_FIRST_RUN,
  STEPS,
  TMDB_KEYS,
  completeConfirmation,
  devicesText,
  gpuText,
  libraryText,
  metadataText,
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
  tmdbKeyProblem,
  type SetupCompletion,
  type SetupDoc,
  type SetupLibrary,
  type SetupMetadata,
  type SetupProcessing,
  type StepKey,
} from '../lib/setup';
import { peopleStepText } from '../lib/people';
import { ConfirmDialog } from '../components/ConfirmDialog';
import './setup.css';

// How often a step under way — a scan, the pipeline starting — is read again.
const POLL_MS = 2500;

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

// SetupCard is the first-run checklist the launchpad shows an admin until one
// marks setup done: the TMDB key, the library, the media pipeline, https for
// phones and TVs, the people who use the server — and where they get their
// accounts: the People page, or the identity provider. portal-api reads each step
// live, from where it is configured; the card says what that means, offers
// the one thing to do next, and reads again while something is under way.
// Every change that reaches past the catalog asks first, as the operator
// console's do.
export function SetupCard({ isAdmin }: { isAdmin: boolean }) {
  const api = usePortalApi();
  // The record says whether to show the card at all: null is open, a record
  // is done, undefined is a portal-api without setup (or one that refused).
  const [record, setRecord] = useState<SetupCompletion | null | undefined>(undefined);
  const [loaded, setLoaded] = useState(false);
  const [doc, setDoc] = useState<SetupDoc | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<StepKey | 'done' | null>(null);
  const [pending, setPending] = useState<{ confirmation: Confirmation; run: () => Promise<unknown> } | null>(null);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!isAdmin) return;
    let live = true;
    api<{ completed: SetupCompletion | null }>('/setup/complete')
      .then((r) => live && setRecord(r.completed ?? null))
      .catch(() => live && setRecord(undefined))
      .finally(() => live && setLoaded(true));
    return () => {
      live = false;
    };
  }, [api, isAdmin]);

  const read = useCallback(async () => {
    try {
      setDoc(await api<SetupDoc>('/setup'));
      setErr(null);
    } catch (e) {
      setErr(errText(e));
    } finally {
      setNow(Date.now());
    }
  }, [api]);

  const open = showChecklist(isAdmin, record, loaded);
  useEffect(() => {
    if (open) void read();
  }, [open, read]);

  // A step under way is read again until it is not.
  useEffect(() => {
    if (!open || !polls(doc)) return;
    const t = setTimeout(() => void read(), POLL_MS);
    return () => clearTimeout(t);
  }, [open, doc, read]);

  if (!open) return null;

  // write runs a change and takes the checklist it answers with — or reads
  // it again, for a write that answers with something else.
  async function write(step: StepKey | 'done', fn: () => Promise<SetupDoc | void>) {
    setBusy(step);
    setErr(null);
    try {
      const next = await fn();
      if (next) {
        setDoc(next);
        setNow(Date.now());
      }
    } finally {
      setBusy(null);
    }
  }
  const post = (path: string, body?: unknown) =>
    api<SetupDoc>(path, { method: 'POST', ...(body === undefined ? {} : { body: JSON.stringify(body) }) });

  const saveKey = (key: string) => write('metadata', () => post('/setup/metadata', { tmdbKey: key.trim() }));
  const scan = () =>
    write('library', () => post('/setup/library/scan')).catch((e) => setErr(errText(e)));
  const switchPipeline = (p: SetupProcessing, on: boolean) =>
    setPending({
      confirmation: pipelineConfirmation(p, on),
      run: () =>
        write('processing', async () => {
          await api('/operator', { method: 'PATCH', body: JSON.stringify({ pipeline: on }) });
          return api<SetupDoc>('/setup');
        }),
    });
  const complete = async () => {
    const r = await api<{ completed: SetupCompletion }>('/setup/complete', { method: 'POST' });
    setRecord(r.completed);
  };
  const done = () => {
    if (!doc) return;
    const c = completeConfirmation(doc);
    if (c) setPending({ confirmation: c, run: () => write('done', complete) });
    else write('done', complete).catch((e) => setErr(errText(e)));
  };

  const p = doc ? progress(doc) : null;
  return (
    <Card
      className="setup"
      aria-label="Set Up Your Server"
      header={
        <span className="setup__title">
          <Server size={14} aria-hidden /> Set Up Your Server
        </span>
      }
      headerAside={p && <Badge tone={p.done === p.total ? 'green' : 'neutral'}>{`${p.done} of ${p.total} Done`}</Badge>}
      footer={
        <div className="setup__foot">
          <Text variant="dim">The launchpad shows this until you mark it done; settings shows it again.</Text>
          <Button size="sm" variant="primary" leading={<Check size={14} />} loading={busy === 'done'} disabled={!doc} onClick={done}>
            Done
          </Button>
        </div>
      }
    >
      {err && (
        <span className="setup__err" role="alert">
          {err}
        </span>
      )}
      {!doc && !err && (
        <div className="setup__state">
          <Spinner /> <Text variant="muted">Reading your server…</Text>
        </div>
      )}
      {doc && (
        <ol className="setup__steps">
          {STEPS.map(({ key, title }) => {
            const chip = stepChip(key, doc[key]);
            return (
              <li key={key} className="setup__step" data-step={key} data-state={doc[key].state}>
                <div className="setup__label">
                  <span className="setup__name">{title}</span>
                  <Badge tone={chip.tone} dot={doc[key].state === 'done'}>
                    {chip.label}
                  </Badge>
                </div>
                <div className="setup__body">
                  {key === 'metadata' && (
                    <MetadataStep m={doc.metadata} now={now} busy={busy === 'metadata'} onSave={saveKey} />
                  )}
                  {key === 'library' && <LibraryStep l={doc.library} now={now} busy={busy === 'library'} onScan={scan} />}
                  {key === 'processing' && (
                    <ProcessingStep p={doc.processing} busy={busy === 'processing'} onSwitch={(on) => switchPipeline(doc.processing, on)} />
                  )}
                  {key === 'devices' && (
                    <>
                      <Text as="p" variant="muted" className="setup__text">
                        {devicesText(doc.devices)}
                      </Text>
                      {doc.devices.state === 'unknown' && (
                        <Text as="p" variant="dim" className="setup__text setup__text--small">
                          {stepNote(doc.devices)}
                        </Text>
                      )}
                      <div className="setup__actions">
                        <DocLink href={DOCS}>Read How</DocLink>
                      </div>
                    </>
                  )}
                  {key === 'people' && (
                    <>
                      <Text as="p" variant="muted" className="setup__text">
                        {peopleStepText(doc.people)}
                      </Text>
                      <div className="setup__actions">
                        {doc.people.mode === 'bundled' ? (
                          <Link className="setup__link" to="/people">
                            People
                          </Link>
                        ) : doc.people.mode === 'external' && doc.people.manageUrl ? (
                          <DocLink href={doc.people.manageUrl}>Identity Provider</DocLink>
                        ) : (
                          <DocLink href={DOCS_ADMIN_CONSOLE}>Read How</DocLink>
                        )}
                      </div>
                    </>
                  )}
                </div>
              </li>
            );
          })}
        </ol>
      )}
      {pending && <ConfirmDialog confirmation={pending.confirmation} onConfirm={pending.run} onClose={() => setPending(null)} />}
    </Card>
  );
}

function DocLink({ href, children }: { href: string; children: ReactNode }) {
  return (
    <a className="setup__link" href={href} target="_blank" rel="noreferrer">
      {children} <ExternalLink size={12} aria-hidden />
    </a>
  );
}

// MetadataStep: the TMDB key. The input is for a new key only — the catalog
// never answers with the one it holds — and is cleared once it is saved.
function MetadataStep({
  m,
  now,
  busy,
  onSave,
}: {
  m: SetupMetadata;
  now: number;
  busy: boolean;
  onSave: (key: string) => Promise<void>;
}) {
  const [replacing, setReplacing] = useState(false);
  const [key, setKey] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const problem = key ? tmdbKeyProblem(key) : null;
  const asking = m.state !== 'unknown' && (m.key === 'none' || replacing);

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (tmdbKeyProblem(key)) return;
    setErr(null);
    try {
      await onSave(key);
      setKey('');
      setReplacing(false);
    } catch (ex) {
      setErr(errText(ex));
    }
  }

  return (
    <>
      <Text as="p" variant="muted" className="setup__text">
        {metadataText(m, now)}
      </Text>
      {m.state === 'unknown' && (
        <Text as="p" variant="dim" className="setup__text setup__text--small">
          {stepNote(m)}
        </Text>
      )}
      {asking ? (
        <form className="setup__key" onSubmit={submit}>
          <Input
            type="password"
            autoComplete="off"
            spellCheck={false}
            inputSize="sm"
            placeholder="TMDB API read access token"
            aria-label="TMDB API read access token"
            value={key}
            invalid={!!problem}
            onChange={(e) => setKey(e.target.value)}
          />
          <Button type="submit" size="sm" leading={<KeyRound size={14} />} loading={busy} disabled={!key || !!problem}>
            Save Key
          </Button>
          {m.key !== 'none' && (
            <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={() => {
                setReplacing(false);
                setKey('');
              }}>
              Cancel
            </Button>
          )}
          <DocLink href={TMDB_KEYS}>Get a Key</DocLink>
        </form>
      ) : (
        m.state !== 'unknown' && (
          <div className="setup__actions">
            <Button variant="ghost" size="sm" leading={<KeyRound size={14} />} onClick={() => setReplacing(true)}>
              Replace Key
            </Button>
          </div>
        )
      )}
      {problem && <span className="setup__hint">{problem}</span>}
      {err && (
        <span className="setup__err" role="alert">
          {err}
        </span>
      )}
    </>
  );
}

// LibraryStep: what the catalog holds, where files go, and a scan.
function LibraryStep({ l, now, busy, onScan }: { l: SetupLibrary; now: number; busy: boolean; onScan: () => void }) {
  const known = l.state !== 'unknown';
  const running = scanRunning(l.scan, now);
  return (
    <>
      <Text as="p" variant="muted" className="setup__text">
        {libraryText(l)}
      </Text>
      <Text as="p" variant="dim" className="setup__text setup__text--small">
        {known ? scanText(l, now) : stepNote(l)}
      </Text>
      <div className="setup__actions">
        {known && (
          <Button size="sm" leading={<ScanSearch size={14} />} loading={busy || running} disabled={busy || running} onClick={onScan}>
            {scanLabel(l, now)}
          </Button>
        )}
        {l.appliance && <DocLink href={DOCS_FIRST_RUN}>Copying Files to the Appliance</DocLink>}
      </div>
    </>
  );
}

// ProcessingStep: the media pipeline, and its switch — which asks first.
function ProcessingStep({ p, busy, onSwitch }: { p: SetupProcessing; busy: boolean; onSwitch: (on: boolean) => void }) {
  const on = p.pipeline === true;
  return (
    <>
      <Text as="p" variant="muted" className="setup__text">
        {processingText(p)}
      </Text>
      <Text as="p" variant="dim" className="setup__text setup__text--small" title={p.gpuNote || undefined}>
        {p.state === 'unknown' ? stepNote(p) : gpuText(p)}
      </Text>
      <div className="setup__actions">
        {p.switchable && p.pipeline !== null && (
          <Button
            size="sm"
            variant={on ? 'ghost' : 'default'}
            leading={<Power size={14} />}
            loading={busy}
            onClick={() => onSwitch(!on)}
          >
            {on ? 'Turn Off' : 'Turn On'}
          </Button>
        )}
        <Link className="setup__link" to="/operator">
          Operator Console
        </Link>
      </div>
    </>
  );
}
