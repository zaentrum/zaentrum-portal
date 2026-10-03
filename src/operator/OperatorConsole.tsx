import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Card,
  Table,
  Badge,
  Button,
  IconButton,
  Select,
  Spinner,
  Text,
  Heading,
} from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { Minus, Plus, RotateCw, RefreshCw, Lock, ArrowUpCircle, ShieldCheck } from 'lucide-react';
import {
  usePortalApi,
  type OperatorInfo,
  type OperatorState,
  type Instance,
  type InstalledAddon,
  type OperatorController,
  type OperatorVerification,
  type VerifyRequest,
} from '../lib/api';
import { containersSummary, hasComponentGroups, phaseTone } from '../lib/addons';
import {
  applyUpdateBody,
  canScaleTo,
  channelConfirmation,
  restartConfirmation,
  scaleConfirmation,
  updateConfirmation,
  updateModeConfirmation,
  type Confirmation,
} from '../lib/confirm';
import { ConfirmDialog } from '../components/ConfirmDialog';
import {
  controllerNote,
  controllerPath,
  controllerUpdate,
  controllerUpdateLabel,
  controllerVersion,
  hasController,
  isVersionLike,
  notReportedNote,
} from '../lib/controller';
import {
  RESULT_RUNNING,
  isAnswered,
  since,
  verificationAttention,
  verificationBusy,
  verificationCondition,
  verificationCounts,
  verificationFinished,
  verificationLabel,
  verificationNote,
  verificationOtherCounts,
  verificationTone,
  verificationTrigger,
} from '../lib/verification';
import './operator.css';

const REFRESH_MS = 5000;
// Installed addons change on an admin's action, not per second.
const ADDONS_REFRESH_MS = 30000;

// Row is a live instance, or a placeholder for a component an installed addon
// declares that is not running here.
type Row = Instance & { placeholder?: string; componentName?: string; role?: string };

interface Grouped {
  platform: Row[];
  addons: { addon: InstalledAddon; rows: Row[] }[] | null; // null: installed addons unknown
  undeclared: Row[];
  unclaimed: Row[];
}

// groupInstances places every workload exactly once. Platform ownership wins;
// then each installed addon claims the workloads it declares (live rows, or a
// "not deployed" placeholder); addon-labelled workloads no installed addon
// declares, and everything else, follow.
function groupInstances(instances: Instance[], addons: InstalledAddon[] | null): Grouped {
  const byName = new Map(instances.map((i) => [i.name, i]));
  const claimed = new Set<string>();
  // A chart addon that is only planned has no workloads to place yet.
  const sections =
    addons?.filter((addon) => addon.registered !== false || addon.components.length > 0).map((addon) => ({
      addon,
      rows: addon.components.map((c): Row => {
        const live = byName.get(c.workload);
        if (live && live.group !== 'platform') {
          claimed.add(live.name);
          return { ...live, componentName: c.name, role: c.role };
        }
        return {
          name: c.workload,
          image: '',
          desiredReplicas: 0,
          readyReplicas: 0,
          updatedReplicas: 0,
          availableReplicas: 0,
          restarts: 0,
          phase: '',
          protected: true,
          operatorManaged: false,
          group: 'addon',
          reason: '',
          alwaysPull: false,
          placeholder: live ? 'a platform workload' : 'not deployed',
          componentName: c.name,
          role: c.role,
        };
      }),
    })) ?? null;
  const unclaimedBy = (g: string) => instances.filter((i) => i.group === g && !claimed.has(i.name));
  return {
    platform: instances.filter((i) => i.group === 'platform'),
    addons: sections,
    undeclared: unclaimedBy('addon'),
    unclaimed: unclaimedBy('other'),
  };
}

// shorten ghcr.io/zaentrum/chino-api:latest -> chino-api:latest
// ghcr.io/zaentrum/chino-api:latest              -> chino-api:latest
// ghcr.io/zaentrum/analyzer@sha256:56268318f11a…  -> analyzer@5626831…
//
// The operator pins images by digest, so a live row carries a 71-character
// hash. Rendered whole it tells you nothing you can act on and it blew the
// column width apart — the three tables were meant to line up as one grid and
// did not, because the widths are minimums that long content overrides. Twelve
// hex characters is enough to tell two builds apart by eye.
const shortImage = (img: string) => {
  const tail = img.replace(/^.*\//, '') || img;
  return tail.replace(/@sha256:([0-9a-f]{12})[0-9a-f]+$/, '@$1…');
};

// The operator / instances console: monitor the running services, scale them,
// and update the platform — from inside the portal. Works against a plain-manifest
// deployment (the demo, and any all-in-one appliance) by acting on Deployments,
// and surfaces the zaentrum-operator (Zaentrum CR) when one is present. Admin-only.
export function OperatorConsole() {
  const api = usePortalApi();
  const [state, setState] = useState<OperatorState | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const [addons, setAddons] = useState<InstalledAddon[] | null>(null);
  // following: the request token of a verification this console asked for,
  // until the run that answers it has ended.
  const [following, setFollowing] = useState<string | null>(null);
  // pending: a change waiting for the admin to confirm it — every write that
  // changes what runs asks first, and says what it will do.
  const [pending, setPending] = useState<{ confirmation: Confirmation; run: () => Promise<unknown> } | null>(null);
  const inflight = useRef(false);
  const seq = useRef(0);

  const load = useCallback(
    (quiet = false) => {
      // Don't pile up auto-ticks while a request is outstanding (a namespace-wide
      // read can exceed the poll interval); and drop out-of-order responses so a
      // slow reply never clobbers fresher state.
      if (quiet && inflight.current) return;
      if (!quiet) setErr(null);
      inflight.current = true;
      const mySeq = ++seq.current;
      api<OperatorState>('/operator')
        .then((s) => {
          if (mySeq === seq.current) setState(s);
        })
        .catch((e) => {
          if (mySeq === seq.current && !quiet) setErr(e instanceof Error ? e.message : String(e));
        })
        .finally(() => {
          inflight.current = false;
        });
    },
    [api],
  );

  useEffect(() => {
    load();
    const t = setInterval(() => load(true), REFRESH_MS);
    return () => clearInterval(t);
  }, [load]);

  // Installed addons decide the sections below. Without them the console
  // falls back to one "addons" section by label — when the call fails, and
  // when an older portal-api answers it with rows that carry no components.
  const loadAddons = useCallback(() => {
    api<unknown>('/addons')
      .then((rows) => setAddons(hasComponentGroups(rows) ? rows : null))
      .catch(() => setAddons(null));
  }, [api]);

  useEffect(() => {
    loadAddons();
    const t = setInterval(loadAddons, ADDONS_REFRESH_MS);
    return () => clearInterval(t);
  }, [loadAddons]);

  async function act(key: string, fn: () => Promise<unknown>, label: string) {
    try {
      await run(key, fn, label);
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    }
  }
  // run makes a write: busy while it runs, the toolbar says what it did, and
  // a refusal is thrown back — to the dialog that asked, which shows it.
  async function run(key: string, fn: () => Promise<unknown>, label: string) {
    setBusy(key);
    setMsg(null);
    try {
      await fn();
      setMsg(`${label} ${key}`);
      load(true);
    } finally {
      setBusy(null);
    }
  }
  const ask = (confirmation: Confirmation, key: string, fn: () => Promise<unknown>, label: string) =>
    setPending({ confirmation, run: () => run(key, fn, label) });

  const scale = (i: Instance, n: number) =>
    ask(scaleConfirmation(i, n), i.name, async () => {
      await api(`/operator/instances/${i.name}/scale`, { method: 'POST', body: JSON.stringify({ replicas: n }) });
      // Reflect the new desired count at once (the poll then confirms), so the
      // next step is computed from the intended value.
      setState((prev) =>
        prev ? { ...prev, instances: prev.instances.map((x) => (x.name === i.name ? { ...x, desiredReplicas: n } : x)) } : prev,
      );
    }, 'scaled');
  const restart = (i: Instance) =>
    ask(restartConfirmation(i), i.name, () => api(`/operator/instances/${i.name}/restart`, { method: 'POST' }), 'restarted');
  const patchOperator = (confirmation: Confirmation, patch: Record<string, string>) =>
    ask(confirmation, 'operator', () => api('/operator', { method: 'PATCH', body: JSON.stringify(patch) }), 'updated');
  // The update applied is the one shown when the dialog opened: the server
  // refuses it (409) if the operator has found another since.
  const applyUpdate = (info: OperatorInfo, managed: number) => {
    const shown = info.availableUpdate ?? '';
    ask(
      updateConfirmation(info, managed),
      'operator',
      () => api('/operator/apply-update', { method: 'POST', body: JSON.stringify(applyUpdateBody(shown)) }),
      'update triggered',
    );
  };
  // The operator runs the checks; the answer is the token its run will carry.
  const verifyNow = () =>
    act(
      'verification',
      async () => {
        const out = await api<VerifyRequest>('/operator/verify', { method: 'POST' });
        if (out?.request) setFollowing(out.request);
      },
      'requested',
    );

  const op = state?.operator;
  const verification = op?.verification;
  const managed = (state?.instances ?? []).filter((i) => i.operatorManaged).length;

  // A run this console asked for is followed through the page's own polling:
  // once the document carries its token with a result that is no longer
  // Running, the toolbar says how it went.
  useEffect(() => {
    if (following && verification && isAnswered(verification, following)) {
      setMsg(verificationFinished(verification));
      setFollowing(null);
    }
  }, [verification, following]);

  // The server classifies each workload (owner refs for platform, the addon
  // labels for addons); installed addons claim the workloads they declare.
  const grouped = groupInstances(state?.instances ?? [], addons);

  const columns: TableColumn<Row>[] = [
    {
      key: 'name',
      header: 'service',
      width: 260,
      render: (i) => (
        <span className="op__svc">
          <b>{i.name}</b>
          {i.componentName && (
            <Text variant="dim">
              {i.componentName !== i.name ? `${i.componentName} · ` : ''}
              {i.role}
            </Text>
          )}
          {i.protected && !i.placeholder && (
            <span className="op__lock" title="protected — not scalable here">
              <Lock size={12} />
            </span>
          )}
        </span>
      ),
    },
    {
      key: 'image',
      header: 'image',
      width: 260,
      render: (i) => (i.placeholder ? '—' : <span className="op__mono">{shortImage(i.image)}</span>),
    },
    {
      key: 'readyReplicas',
      header: 'replicas',
      width: 130,
      render: (i) =>
        i.placeholder ? (
          '—'
        ) : (
          <span className="op__scale">
            {!i.protected && (
              <IconButton
                label={`scale ${i.name} down`}
                size="sm"
                variant="ghost"
                disabled={busy === i.name || !canScaleTo(i, i.desiredReplicas - 1)}
                title={
                  i.adminStack && i.desiredReplicas <= 1
                    ? `the console runs on ${i.name}: it keeps one replica`
                    : undefined
                }
                onClick={() => scale(i, i.desiredReplicas - 1)}
              >
                <Minus size={13} />
              </IconButton>
            )}
            <span className="op__count">
              {i.readyReplicas}/{i.desiredReplicas}
            </span>
            {!i.protected && (
              <IconButton
                label={`scale ${i.name} up`}
                size="sm"
                variant="ghost"
                disabled={busy === i.name || !canScaleTo(i, i.desiredReplicas + 1)}
                onClick={() => scale(i, i.desiredReplicas + 1)}
              >
                <Plus size={13} />
              </IconButton>
            )}
          </span>
        ),
    },
    {
      key: 'phase',
      header: 'status',
      width: 260,
      // The reason sits next to the phase, not in its own column: "degraded"
      // on its own is not actionable, and the two are one fact.
      render: (i) =>
        i.placeholder ? (
          <Badge tone="neutral">{i.placeholder}</Badge>
        ) : (
          <span className="op__status">
            <Badge tone={phaseTone(i.phase)} dot>
              {i.phase}
            </Badge>
            {i.reason && <Text variant="dim">{i.reason}</Text>}
          </span>
        ),
    },
    {
      key: 'restarts',
      header: 'restarts',
      align: 'right',
      width: 90,
      render: (i) => (i.restarts > 0 ? i.restarts : '—'),
    },
    {
      key: 'updatedReplicas',
      header: '',
      align: 'right',
      width: 120,
      render: (i) =>
        i.placeholder ? null : i.protected ? (
          <Badge tone="neutral">protected</Badge>
        ) : (
          <Button
            variant="ghost"
            size="sm"
            leading={<RotateCw size={13} />}
            loading={busy === i.name}
            onClick={() => restart(i)}
            title={i.alwaysPull ? 'restart — re-pulls the latest image' : 'restart'}
          >
            restart
          </Button>
        ),
    },
  ];

  return (
    <div className="op">
      <div className="op__head">
        <Heading level={1} chevron>
          operator
        </Heading>
        <span className="op__sub">running instances — monitor, scale &amp; update</span>
      </div>

      <div className="op__toolbar">
        <Button
          variant="ghost"
          size="sm"
          leading={<RefreshCw size={14} />}
          onClick={() => {
            load();
            loadAddons();
          }}
        >
          refresh
        </Button>
        {msg && <span className="op__msg">{msg}</span>}
      </div>

      {err && <div className="op__err">error: {err}</div>}

      {pending && (
        <ConfirmDialog confirmation={pending.confirmation} onConfirm={pending.run} onClose={() => setPending(null)} />
      )}

      {/* operator (desired state) panel — only when in-cluster (else the
          instances section below shows the single "unavailable" note) */}
      {op && state?.available && (
        <Card
          header={<span className="op__card-title">platform{op.present ? '' : ' · direct mode'}</span>}
          headerAside={
            op.present && op.availableUpdate ? (
              <Button size="sm" leading={<ArrowUpCircle size={14} />} loading={busy === 'operator'} onClick={() => applyUpdate(op, managed)}>
                update to {op.availableUpdate}
              </Button>
            ) : undefined
          }
        >
          {op.present ? (
            <div className="op__grid">
              <Field label="version">
                <span className="op__mono">{op.currentVersion || op.version || 'latest'}</span>
              </Field>
              <Field label="phase">
                <Badge tone={phaseTone((op.phase || '').toLowerCase())} dot>{op.phase || '—'}</Badge>
              </Field>
              <Field label="channel">
                {/* Controlled by what the operator reports: it changes only
                    once the dialog is confirmed and the next read says so. */}
                <Select
                  value={op.channel || 'stable'}
                  disabled={busy === 'operator'}
                  onChange={(e) => {
                    const next = e.target.value;
                    if (next !== (op.channel || 'stable')) patchOperator(channelConfirmation(op, next), { channel: next });
                  }}
                  options={[{ label: 'stable', value: 'stable' }, { label: 'edge', value: 'edge' }]}
                />
              </Field>
              <Field label="updates">
                <Select
                  value={op.updateMode || 'manual'}
                  disabled={busy === 'operator'}
                  onChange={(e) => {
                    const next = e.target.value;
                    if (next !== (op.updateMode || 'manual')) patchOperator(updateModeConfirmation(op, next), { updateMode: next });
                  }}
                  options={[{ label: 'manual', value: 'manual' }, { label: 'auto', value: 'auto' }]}
                />
              </Field>
            </div>
          ) : (
            <Text variant="muted">{op.note || 'no operator detected — changes act on Deployments directly.'}</Text>
          )}
        </Card>
      )}

      {/* the platform checking itself — absent against an older portal-api */}
      {op?.present && state?.available && verification && (
        <VerificationCard
          v={verification}
          requesting={busy === 'verification' || !!following}
          onVerify={verifyNow}
        />
      )}

      {/* the operator's own controller — read-only on purpose */}
      {op?.present && state?.available && <ControllerCard controller={op.controller} />}

      {/* instances (observed state) */}
      {!state && !err && (
        <div className="op__state">
          <Spinner /> <Text variant="muted">loading instances…</Text>
        </div>
      )}
      {state && !state.available ? (
        <Text variant="muted">
          instance management is unavailable — the portal-api is not running in a cluster.
        </Text>
      ) : (
        state && (
          <>
            <InstanceGroup
              title="platform services"
              hint="rendered by the operator from the platform chart — these upgrade with it"
              rows={grouped.platform}
              columns={columns}
            />
            {grouped.addons?.map(({ addon, rows }) => (
              <InstanceGroup
                key={addon.key}
                title={addon.title || addon.key}
                hint={`addon ${addon.key}${addon.version ? ` ${addon.version}` : ''} — ${containersSummary(addon.components).text}; ${
                  addon.chart
                    ? `the operator runs its containers from the chart ${addon.chart.ref}${addon.chart.lastApplied?.version ? ` ${addon.chart.lastApplied.version}` : ''}`
                    : "its containers run through the addon's own deployment channel"
                }`}
                rows={rows}
                columns={columns}
                emptyText="the addon declares no containers."
              />
            ))}
            <InstanceGroup
              title={grouped.addons === null ? 'addons' : 'undeclared addon workloads'}
              hint={
                grouped.addons === null
                  ? 'installed alongside the platform, each with its own lifecycle and version'
                  : 'labelled as addon workloads, but no installed addon declares them'
              }
              rows={grouped.undeclared}
              columns={columns}
              emptyText={grouped.addons === null || grouped.addons.length === 0 ? 'no addons are running.' : ''}
            />
            <InstanceGroup
              title="unclaimed"
              hint="running in this namespace but claimed by neither the platform chart nor an addon"
              rows={grouped.unclaimed}
              columns={columns}
              emptyText=""
            />
          </>
        )
      )}
    </div>
  );
}

// VerificationCard is the platform checking itself: how the last run went,
// after which update, and what failed — and "verify now" to ask for another.
//
// The operator runs the checks, after every update and on request; the card
// shows its record and asks, and decides nothing itself. The button waits
// while a run is in progress or a request waits for one, because asking again
// then changes nothing; a platform with verification switched off gets no
// button at all, since the run it would ask for never comes.
function VerificationCard({
  v,
  requesting,
  onVerify,
}: {
  v: OperatorVerification;
  requesting: boolean;
  onVerify: () => void;
}) {
  const now = Date.now();
  const running = v.result === RESULT_RUNNING;
  const busy = requesting || verificationBusy(v);
  const when = running || !v.finishedAt ? v.startedAt : v.finishedAt;
  const otherCounts = verificationOtherCounts(v);
  const attention = verificationAttention(v);
  const note = verificationNote(v);
  return (
    <Card
      header={<span className="op__card-title">verification</span>}
      headerAside={
        v.enabled ? (
          <Button
            size="sm"
            leading={<ShieldCheck size={14} />}
            loading={busy}
            onClick={onVerify}
            title={busy ? (running ? 'a run is in progress' : 'a run is requested') : 'run the platform checks now'}
          >
            verify now
          </Button>
        ) : (
          <Badge tone="neutral" title="spec.verification.enabled is false">
            off
          </Badge>
        )
      }
    >
      <div className="op__grid">
        <Field label="result">
          <span className="op__status">
            <Badge tone={verificationTone(v.result)} dot title={verificationCondition(v) || undefined}>
              {verificationLabel(v)}
            </Badge>
            {v.pendingRequest && <Text variant="dim">requested</Text>}
          </span>
        </Field>
        {v.result && (
          <>
            <Field label="checks">
              <span className="op__mono">{verificationCounts(v) || '—'}</span>
              {otherCounts && <span className="op__counts-more">{otherCounts}</span>}
            </Field>
            <Field label={running || !v.finishedAt ? 'started' : 'finished'}>
              <Text variant="dim" title={when}>
                {since(when, now) || '—'}
              </Text>
            </Field>
            <Field label="after">
              <span className="op__mono">{verificationTrigger(v) || '—'}</span>
            </Field>
            {v.job && (
              <Field label="job">
                <span className="op__mono op__wrap">{v.job}</span>
              </Field>
            )}
          </>
        )}
      </div>
      {attention.length > 0 && (
        <ul className="op__checks">
          {attention.map((c, i) => (
            <li key={`${i}-${c.name}`} className="op__check">
              <Badge tone="amber" dot={c.status === 'fail'}>
                {c.status}
              </Badge>
              <span className="op__mono">{c.name}</span>
              {c.detail && <Text variant="dim">{c.detail}</Text>}
            </li>
          ))}
        </ul>
      )}
      {note && (
        <Text variant="muted" className="op__card-note">
          {note}
        </Text>
      )}
    </Card>
  );
}

// ControllerCard is the operator's own controller: what is in charge, how it
// got here, and whether something newer exists.
//
// It is read-only, and deliberately so. The controller runs in its own
// namespace, outside the portal's permissions, and is installed and upgraded
// outside the product — by OLM, by applying its install manifest, or with the
// appliance. A button here could only ever fail, or worse, look like it
// worked; naming the path is the honest thing the console can do. Every
// operator older than the field reports nothing, and that says so too rather
// than rendering a row of dashes.
function ControllerCard({ controller }: { controller?: OperatorController }) {
  const update = controllerUpdate(controller);
  const label = controllerUpdateLabel(controller);
  const moving = !!update && !isVersionLike(update);
  const { installed } = controllerPath(controller?.source);
  return (
    <Card
      header={<span className="op__card-title">operator controller</span>}
      headerAside={
        label ? (
          <Badge
            tone="blue"
            title={
              moving
                ? // A moving tag is not a version you can be on: the channel this
                  // install follows points somewhere else now, which is a fact
                  // about the channel and not a fault.
                  `the ${update} tag this install follows now points at a different image — the controller is updated outside the platform`
                : 'the controller is updated outside the platform'
            }
          >
            {label}
          </Badge>
        ) : undefined
      }
    >
      {hasController(controller) ? (
        <>
          <div className="op__grid">
            <Field label="version">
              <span className="op__mono">{controllerVersion(controller)}</span>
            </Field>
            <Field label="installed by">
              <Text>{installed}</Text>
            </Field>
            <Field label="image">
              <span className="op__mono op__wrap">{controller?.image || '—'}</span>
            </Field>
            {controller?.observedAt && (
              <Field label="observed">
                <Text variant="dim">{controller.observedAt}</Text>
              </Field>
            )}
          </div>
          <Text variant="muted" className="op__ctl-note">
            {controllerNote(controller?.source)}
          </Text>
        </>
      ) : (
        <Text variant="muted">{notReportedNote}</Text>
      )}
    </Card>
  );
}

// tiny inline field (label + control) reused in the operator panel grid.
function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="op__field">
      <span className="op__field-label">{label}</span>
      {children}
    </label>
  );
}

// InstanceGroup renders one section. A group with nothing in it is HIDDEN when
// emptyText is empty (the "unclaimed" case is normal and does not need a row
// saying so) but SHOWN when it is set — "no addons are running" is information,
// whereas an empty unclaimed list is just tidy.
function InstanceGroup({
  title,
  hint,
  rows,
  columns,
  emptyText,
}: {
  title: string;
  hint: string;
  rows: Row[];
  columns: TableColumn<Row>[];
  emptyText?: string;
}) {
  if (rows.length === 0 && !emptyText) return null;
  return (
    <section className="op__group">
      <Heading level={2}>{title}</Heading>
      <Text variant="dim">{hint}</Text>
      {rows.length > 0 ? (
        <Table columns={columns} rows={rows} rowKey={(i) => i.name} dense />
      ) : (
        <Text variant="muted">{emptyText}</Text>
      )}
    </section>
  );
}
