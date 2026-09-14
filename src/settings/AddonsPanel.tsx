import { Fragment, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Table,
  Thead,
  Tbody,
  Tr,
  Th,
  Td,
  Button,
  IconButton,
  Badge,
  Modal,
  Field,
  Input,
  Select,
  Spinner,
  Text,
} from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { ChevronDown, ChevronRight, ExternalLink, Puzzle, RefreshCw, Search, Trash2 } from 'lucide-react';
import {
  usePortalApi,
  type AddonComponent,
  type AddonSetup,
  type InstallResult,
  type InstalledAddon,
  type RemoveResult,
  type SetupStatus,
  type Space,
} from '../lib/api';
import {
  UNKNOWN_SETUP,
  appRoute,
  componentPhase,
  containersSummary,
  parseSetupStatus,
  phaseTone,
  setupTone,
} from '../lib/addons';
import { useResource } from './useResource';

// What an install created (or a check would create), in the order it matters
// to the admin.
function summarise(r: InstallResult) {
  const n = (count: number, one: string, many: string) => `${count} ${count === 1 ? one : many}`;
  const parts = [];
  if (r.space) parts.push(`space "${r.space.title || r.space.key}"`);
  if (r.tiles) parts.push(n(r.tiles, 'tile', 'tiles'));
  if (r.slots) parts.push(n(r.slots, 'slot row', 'slot rows'));
  if (r.commands) parts.push(n(r.commands, 'CLI command', 'CLI commands'));
  if (r.components?.length) parts.push(n(r.components.length, 'container', 'containers'));
  return parts.length ? parts.join(', ') : 'nothing to show — the addon declares no UI';
}

// The design system's Table aligns its own headers; the primitives used for
// the expandable table do not.
const LEFT = { textAlign: 'left' } as const;

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

// AddonsPanel is the install path and the addon overview. The admin types the
// addon's in-cluster address; portal-api pulls the addon's manifest and creates
// its app, tiles and slot rows, owned by the addon key, and records the
// containers the addon consists of. The portal shows their state and the
// addon's own setup checklist, and links to where the addon is configured —
// it never holds a configuration value, and it never deploys a container.
export function AddonsPanel() {
  const api = usePortalApi();
  const navigate = useNavigate();
  const [spaces, setSpaces] = useState<Space[]>([]);
  const { items, loading, error, reload } = useResource<InstalledAddon>('/addons');
  const [url, setUrl] = useState('');
  const [space, setSpace] = useState('');
  const [busy, setBusy] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [preview, setPreview] = useState<InstallResult | null>(null);
  const [removing, setRemoving] = useState<InstalledAddon | null>(null);
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [setup, setSetup] = useState<Record<string, SetupStatus | undefined>>({});

  useEffect(() => {
    let live = true;
    api<Space[]>('/spaces')
      .then((s) => live && setSpaces(s))
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [api]);

  // The browser asks each addon for its own setup state, through the app
  // proxy and with the admin's token: the portal-api never sees the answer,
  // and a failure only ever reads as "unknown".
  useEffect(() => {
    let live = true;
    setSetup({});
    for (const a of items) {
      if (!a.setup) continue;
      api<unknown>(`/apps/${encodeURIComponent(a.key)}${a.setup.path}`)
        .then((raw) => live && setSetup((s) => ({ ...s, [a.key]: parseSetupStatus(raw) })))
        .catch(() => live && setSetup((s) => ({ ...s, [a.key]: UNKNOWN_SETUP })));
    }
    return () => {
      live = false;
    };
  }, [api, items]);

  const post = (proxyUrl: string, extra: Record<string, unknown>) =>
    api<InstallResult>('/addons', {
      method: 'POST',
      body: JSON.stringify({ proxyUrl, ...(space ? { space } : {}), ...extra }),
    });

  async function check() {
    const proxyUrl = url.trim();
    if (!proxyUrl) {
      setErr('the addon address is required');
      return;
    }
    setBusy('check');
    setErr(null);
    setMsg(null);
    try {
      setPreview(await post(proxyUrl, { dryRun: true }));
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(null);
    }
  }

  async function install() {
    const proxyUrl = url.trim();
    if (!proxyUrl) {
      setErr('the addon address is required');
      return;
    }
    setBusy('install');
    setErr(null);
    setMsg(null);
    try {
      const r = await post(proxyUrl, {});
      setMsg(`${r.refresh ? 'refreshed' : 'installed'} ${r.key}: ${summarise(r)}`);
      setUrl('');
      setPreview(null);
      reload();
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(null);
    }
  }

  async function refresh(row: InstalledAddon) {
    // Re-pull the manifest: the addon shipped a new version and declares
    // different contributions. Same call as install; rows are replaced.
    setBusy(row.key);
    setMsg(null);
    setErr(null);
    try {
      const r = await api<InstallResult>('/addons', {
        method: 'POST',
        body: JSON.stringify({ proxyUrl: row.proxyUrl }),
      });
      setMsg(`refreshed ${r.key}: ${summarise(r)}`);
      reload();
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(null);
    }
  }

  async function remove(row: InstalledAddon) {
    setBusy(row.key);
    setMsg(null);
    setErr(null);
    try {
      // 200 with what was removed; an older portal-api answers 204.
      const r = await api<RemoveResult | undefined>(`/addons/${encodeURIComponent(row.key)}`, { method: 'DELETE' });
      const left = r?.remainingWorkloads ?? [];
      setMsg(
        left.length
          ? `removed ${row.key} — still running, remove through your deployment channel: ${left.join(', ')}`
          : `removed ${row.key}`,
      );
      setRemoving(null);
      reload();
    } catch (e) {
      setErr(errText(e));
      setRemoving(null);
    } finally {
      setBusy(null);
    }
  }

  const toggle = (key: string) => setOpen((o) => ({ ...o, [key]: !o[key] }));
  const COLS = 7;

  return (
    <div>
      <div className="set__form" style={{ maxWidth: 640, marginBottom: 16 }}>
        <Field
          label="add an addon"
          hint="its in-cluster address — the Service name of the addon's primary container, e.g. http://example. The platform reads the addon's manifest and creates what it declares. Check first to see what that is."
        >
          <Input
            value={url}
            placeholder="http://example"
            onChange={(e) => setUrl(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && check()}
          />
        </Field>
        {spaces.length > 0 && (
          <Field
            label="space"
            hint="where tiles are placed, unless the addon brings its own section; default is the first space"
          >
            <Select
              value={space}
              onChange={(e) => setSpace(e.target.value)}
              options={[{ label: '(default)', value: '' }, ...spaces.map((s) => ({ label: s.title || s.key, value: s.key }))]}
            />
          </Field>
        )}
        <div className="set__toolbar">
          <Button variant="default" leading={<Search size={15} />} loading={busy === 'check'} onClick={check}>
            check
          </Button>
          <Button leading={<Puzzle size={15} />} loading={busy === 'install'} onClick={install}>
            install
          </Button>
          {msg && <span className="set__ok">{msg}</span>}
        </div>
        {err && <span className="set__err">{err}</span>}
      </div>

      {error && <span className="set__err">error: {error}</span>}
      {loading && !items.length ? (
        <div className="set__state">
          <Spinner /> <Text variant="muted">loading…</Text>
        </div>
      ) : (
        <div className="nc-table__scroll">
          <table className="nc-table nc-table--dense">
            <Thead>
              <tr>
                <Th style={{ width: 36 }} aria-label="expand" />
                <Th style={LEFT}>addon</Th>
                <Th style={LEFT}>key</Th>
                <Th style={LEFT}>version</Th>
                <Th style={LEFT}>containers</Th>
                <Th style={LEFT}>setup</Th>
                <Th />
              </tr>
            </Thead>
            <Tbody>
              {items.length === 0 && (
                <tr>
                  <td className="nc-table__empty" colSpan={COLS}>
                    no addons installed. Deploy one next to the platform, then add it by its address.
                  </td>
                </tr>
              )}
              {items.map((a) => {
                const expanded = !!open[a.key];
                const containers = containersSummary(a.components);
                return (
                  <Fragment key={a.key}>
                    <Tr>
                      <Td>
                        <IconButton
                          label={expanded ? `collapse ${a.key}` : `expand ${a.key}`}
                          size="sm"
                          variant="ghost"
                          aria-expanded={expanded}
                          onClick={() => toggle(a.key)}
                        >
                          {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                        </IconButton>
                      </Td>
                      <Td>
                        <span className="set__addon">
                          <b>{a.title || a.key}</b>
                          {a.refreshAvailable && (
                            <Badge tone="blue" title="the addon now serves a different manifest — refresh to apply it">
                              refresh available
                            </Badge>
                          )}
                        </span>
                      </Td>
                      <Td>
                        <span className="set__mono">{a.key}</span>
                      </Td>
                      <Td>
                        <span className="set__mono">{a.version || '—'}</span>
                      </Td>
                      <Td>
                        <Badge tone={containers.tone} dot>
                          {containers.text}
                        </Badge>
                      </Td>
                      <Td>
                        <SetupBadge setup={a.setup} status={setup[a.key]} />
                      </Td>
                      <Td style={{ textAlign: 'right' }}>
                        <span style={{ display: 'inline-flex', gap: 4 }}>
                          <Button
                            variant="ghost"
                            size="sm"
                            leading={<RefreshCw size={13} />}
                            loading={busy === a.key}
                            onClick={() => refresh(a)}
                          >
                            refresh
                          </Button>
                          <Button variant="ghost" size="sm" leading={<Trash2 size={13} />} onClick={() => setRemoving(a)}>
                            remove
                          </Button>
                        </span>
                      </Td>
                    </Tr>
                    {expanded && (
                      <tr className="set__detail">
                        <td className="nc-table__td" colSpan={COLS}>
                          <AddonDetail
                            addon={a}
                            status={setup[a.key]}
                            onConfigure={(target) => navigate(appRoute(a.key, target))}
                          />
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </Tbody>
          </table>
        </div>
      )}

      {preview && (
        <Modal
          open
          width={760}
          onClose={() => setPreview(null)}
          title={`check ${preview.key}`}
          footer={
            <>
              <Button variant="ghost" size="sm" onClick={() => setPreview(null)}>
                cancel
              </Button>
              <Button size="sm" loading={busy === 'install'} onClick={install}>
                {preview.refresh ? 'refresh' : 'install'}
              </Button>
            </>
          }
        >
          <div className="set__form">
            <Text as="p">
              <b>{preview.app?.title || preview.key}</b>
              {preview.version ? ` ${preview.version}` : ''} —{' '}
              {preview.refresh ? 'already installed; installing again refreshes it.' : 'not installed yet.'}
            </Text>
            <Text variant="muted" as="p">
              creates: {summarise(preview)}
            </Text>
            <ComponentTable components={preview.components ?? []} />
            {preview.setup && <SetupChecklist setup={preview.setup} />}
          </div>
        </Modal>
      )}

      {removing && (
        <Modal
          open
          onClose={() => setRemoving(null)}
          title={`remove ${removing.title || removing.key}`}
          footer={
            <>
              <Button variant="ghost" size="sm" onClick={() => setRemoving(null)}>
                cancel
              </Button>
              <Button variant="danger" size="sm" loading={busy === removing.key} onClick={() => remove(removing)}>
                remove
              </Button>
            </>
          }
        >
          <div className="set__form">
            <Text as="p">
              Its app, tiles, slot rows and any space it brought are deleted from the portal.
            </Text>
            {removing.components.length > 0 ? (
              <>
                <Text variant="muted" as="p">
                  The platform does not delete containers. Whatever of these is deployed keeps running until you
                  remove it through your deployment channel:
                </Text>
                <ul className="set__list">
                  {removing.components.map((c) => (
                    <li key={c.name}>
                      <span className="set__mono">{c.workload}</span>{' '}
                      <Text variant="dim">
                        {c.name} · {c.role} · {componentPhase(c)}
                      </Text>
                    </li>
                  ))}
                </ul>
              </>
            ) : (
              <Text variant="muted" as="p">
                The platform does not delete containers; the addon&apos;s workload is yours to remove.
              </Text>
            )}
          </div>
        </Modal>
      )}
    </div>
  );
}

function SetupBadge({ setup, status }: { setup: AddonSetup | null; status: SetupStatus | undefined }) {
  if (!setup) return <Text variant="dim">—</Text>;
  if (!status) return <Text variant="dim">checking…</Text>;
  return (
    <Badge tone={setupTone(status.state)} dot>
      {status.state}
    </Badge>
  );
}

// AddonDetail is the expanded row: what the addon runs, and what it still
// needs before it works.
function AddonDetail({
  addon,
  status,
  onConfigure,
}: {
  addon: InstalledAddon;
  status: SetupStatus | undefined;
  onConfigure: (target?: string) => void;
}) {
  return (
    <div className="set__detail-body">
      <Text variant="dim" as="p">
        installed from <span className="set__mono">{addon.proxyUrl || '—'}</span> · {addon.tiles}{' '}
        {addon.tiles === 1 ? 'tile' : 'tiles'} · {addon.slots} {addon.slots === 1 ? 'slot row' : 'slot rows'}
      </Text>
      <ComponentTable components={addon.components} />
      {addon.setup && <SetupChecklist setup={addon.setup} status={status} onConfigure={onConfigure} />}
    </div>
  );
}

function ComponentTable({ components }: { components: AddonComponent[] }) {
  const cols: TableColumn<AddonComponent>[] = [
    {
      key: 'name',
      header: 'container',
      render: (c) => (
        <span className="set__addon">
          <b>{c.name}</b>
          <Badge tone={c.role === 'primary' ? 'blue' : 'neutral'}>{c.role}</Badge>
        </span>
      ),
    },
    { key: 'workload', header: 'workload', render: (c) => <span className="set__mono">{c.workload}</span> },
    {
      key: 'phase',
      header: 'status',
      render: (c) => (
        <span className="set__addon">
          <Badge tone={phaseTone(c.phase)} dot>
            {componentPhase(c)}
          </Badge>
          {c.reason && <Text variant="dim">{c.reason}</Text>}
        </span>
      ),
    },
    {
      key: 'ready',
      header: 'replicas',
      align: 'right',
      render: (c) => (c.ready === null || c.desired === null ? '—' : `${c.ready}/${c.desired}`),
    },
    {
      key: 'restarts',
      header: 'restarts',
      align: 'right',
      render: (c) => (c.restarts ? c.restarts : '—'),
    },
    { key: 'summary', header: 'does', render: (c) => <Text variant="dim">{c.summary || '—'}</Text> },
  ];
  return (
    <Table columns={cols} rows={components} rowKey={(c) => c.name} dense empty={<Text variant="muted">no containers declared.</Text>} />
  );
}

// SetupChecklist renders the addon's declared sections with the state the
// addon reported for each. Summaries are addon-authored plain text.
function SetupChecklist({
  setup,
  status,
  onConfigure,
}: {
  setup: AddonSetup;
  status?: SetupStatus;
  onConfigure?: (target?: string) => void;
}) {
  const sections = setup.sections ?? [];
  const reported = new Map((status?.sections ?? []).map((s) => [s.key, s]));
  return (
    <div className="set__checklist">
      <div className="set__checklist-head">
        <b>setup</b>
        {status && (
          <Badge tone={setupTone(status.state)} dot>
            {status.state}
          </Badge>
        )}
        {status?.state === 'unknown' && (
          <Text variant="dim">the addon did not answer its setup check at {setup.path}</Text>
        )}
      </div>
      {sections.length === 0 && <Text variant="muted">the addon declares no setup sections.</Text>}
      <ul className="set__list">
        {sections.map((sec) => {
          const r = reported.get(sec.key);
          return (
            <li key={sec.key} className="set__check">
              <span className="set__check-state">
                {status ? (
                  <Badge tone={setupTone(r?.state ?? 'unknown')} dot>
                    {r?.state ?? 'unknown'}
                  </Badge>
                ) : (
                  <Badge tone="neutral">{sec.required ? 'required' : 'optional'}</Badge>
                )}
              </span>
              <span className="set__check-text">
                <span>
                  <b>{sec.title || sec.key}</b>{' '}
                  {status && <Text variant="dim">{sec.required ? 'required' : 'optional'}</Text>}
                </span>
                {sec.description && <Text variant="dim">{sec.description}</Text>}
                {r?.summary && <Text variant="muted">{r.summary}</Text>}
              </span>
              {onConfigure && sec.target && (
                <Button variant="ghost" size="sm" leading={<ExternalLink size={13} />} onClick={() => onConfigure(sec.target)}>
                  configure
                </Button>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
