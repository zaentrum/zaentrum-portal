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
import { Minus, Plus, RotateCw, RefreshCw, Lock, ArrowUpCircle } from 'lucide-react';
import { usePortalApi, type OperatorState, type Instance } from '../lib/api';
import './operator.css';

const REFRESH_MS = 5000;

type BadgeTone = 'neutral' | 'green' | 'amber' | 'blue';

function phaseTone(phase: string): BadgeTone {
  switch (phase) {
    case 'ready':
      return 'green';
    case 'progressing':
      return 'blue';
    case 'degraded':
      return 'amber';
    default:
      return 'neutral';
  }
}

// shorten ghcr.io/zaentrum/chino-api:latest -> chino-api:latest
const shortImage = (img: string) => img.replace(/^.*\//, '') || img;

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

  async function act(key: string, fn: () => Promise<unknown>, label: string) {
    setBusy(key);
    setMsg(null);
    try {
      await fn();
      setMsg(`${label} ${key}`);
      load(true);
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }
  const scale = (i: Instance, n: number) =>
    act(
      i.name,
      async () => {
        await api(`/operator/instances/${i.name}/scale`, { method: 'POST', body: JSON.stringify({ replicas: n }) });
        // Optimistically reflect the new desired count so a fast follow-up click
        // computes from the intended value (the poll then confirms).
        setState((prev) =>
          prev
            ? { ...prev, instances: prev.instances.map((x) => (x.name === i.name ? { ...x, desiredReplicas: n } : x)) }
            : prev,
        );
      },
      'scaled',
    );
  const restart = (i: Instance) =>
    act(i.name, () => api(`/operator/instances/${i.name}/restart`, { method: 'POST' }), 'restarted');
  const patchOperator = (patch: Record<string, string>) =>
    act('operator', () => api('/operator', { method: 'PATCH', body: JSON.stringify(patch) }), 'updated');
  const applyUpdate = () => act('operator', () => api('/operator/apply-update', { method: 'POST' }), 'update triggered');

  const op = state?.operator;

  const columns: TableColumn<Instance>[] = [
    {
      key: 'name',
      header: 'service',
      render: (i) => (
        <span className="op__svc">
          <b>{i.name}</b>
          {i.operatorManaged && <Badge tone="blue">operator</Badge>}
          {i.protected && (
            <span className="op__lock" title="protected — not scalable here">
              <Lock size={12} />
            </span>
          )}
        </span>
      ),
    },
    { key: 'image', header: 'image', render: (i) => <span className="op__mono">{shortImage(i.image)}</span> },
    {
      key: 'readyReplicas',
      header: 'replicas',
      render: (i) => (
        <span className="op__scale">
          {!i.protected && (
            <IconButton
              label={`scale ${i.name} down`}
              size="sm"
              variant="ghost"
              disabled={busy === i.name || i.desiredReplicas <= 0}
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
              disabled={busy === i.name || i.desiredReplicas >= 20}
              onClick={() => scale(i, i.desiredReplicas + 1)}
            >
              <Plus size={13} />
            </IconButton>
          )}
        </span>
      ),
    },
    { key: 'phase', header: 'status', render: (i) => <Badge tone={phaseTone(i.phase)} dot>{i.phase}</Badge> },
    { key: 'restarts', header: 'restarts', align: 'right', render: (i) => (i.restarts > 0 ? i.restarts : '—') },
    {
      key: 'updatedReplicas',
      header: '',
      align: 'right',
      render: (i) =>
        i.protected ? (
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
        <Button variant="ghost" size="sm" leading={<RefreshCw size={14} />} onClick={() => load()}>
          refresh
        </Button>
        {msg && <span className="op__msg">{msg}</span>}
      </div>

      {err && <div className="op__err">error: {err}</div>}

      {/* operator (desired state) panel — only when in-cluster (else the
          instances section below shows the single "unavailable" note) */}
      {op && state?.available && (
        <Card
          header={<span className="op__card-title">platform{op.present ? '' : ' · direct mode'}</span>}
          headerAside={
            op.present && op.availableUpdate ? (
              <Button size="sm" leading={<ArrowUpCircle size={14} />} loading={busy === 'operator'} onClick={applyUpdate}>
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
                <Select
                  value={op.channel || 'stable'}
                  onChange={(e) => patchOperator({ channel: e.target.value })}
                  options={[{ label: 'stable', value: 'stable' }, { label: 'edge', value: 'edge' }]}
                />
              </Field>
              <Field label="updates">
                <Select
                  value={op.updateMode || 'manual'}
                  onChange={(e) => patchOperator({ updateMode: e.target.value })}
                  options={[{ label: 'manual', value: 'manual' }, { label: 'auto', value: 'auto' }]}
                />
              </Field>
            </div>
          ) : (
            <Text variant="muted">{op.note || 'no operator detected — changes act on Deployments directly.'}</Text>
          )}
        </Card>
      )}

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
          <Table
            columns={columns}
            rows={state.instances}
            rowKey={(i) => i.name}
            dense
            empty={<Text variant="muted">no instances.</Text>}
          />
        )
      )}
    </div>
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
