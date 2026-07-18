import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Card, Badge, Button, Select, Spinner, Text, Heading, Input } from '@nalet/design-system';
import { RefreshCw, Download, Play, Pause } from 'lucide-react';
import { usePortalApi, usePortalText, type DebugPod } from '../lib/api';
import './logs.css';

const LIVE_MS = 4000;
const TAIL_DEFAULT = 500;

type BadgeTone = 'neutral' | 'green' | 'amber' | 'blue';

function phaseTone(phase: string): BadgeTone {
  switch (phase) {
    case 'Running':
      return 'green';
    case 'Pending':
      return 'blue';
    case 'Failed':
      return 'amber';
    default:
      return 'neutral';
  }
}

// The Logs console: every container's logs from inside the portal, with a
// pod/container selector, a client-side filter, live tail, and download. Secrets
// are redacted server-side. Admin-only (RequireAdmin + the API gate).
export function LogsConsole() {
  const api = usePortalApi();
  const text = usePortalText();

  const [pods, setPods] = useState<DebugPod[]>([]);
  const [pod, setPod] = useState('');
  const [container, setContainer] = useState('');
  const [tail, setTail] = useState(TAIL_DEFAULT);
  const [filter, setFilter] = useState('');
  const [logs, setLogs] = useState('');
  const [live, setLive] = useState(false);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const inflight = useRef(false);
  const bodyRef = useRef<HTMLPreElement>(null);

  // Load the pod list once.
  useEffect(() => {
    api<DebugPod[]>('/debug/pods')
      .then((ps) => {
        setPods(ps);
        if (ps.length && !pod) {
          setPod(ps[0].pod);
          setContainer(ps[0].containers[0] ?? '');
        }
      })
      .catch((e) => setErr(String(e.message ?? e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const containers = useMemo(
    () => pods.find((p) => p.pod === pod)?.containers ?? [],
    [pods, pod],
  );

  const load = useCallback(
    async (quiet = false) => {
      if (!pod) return;
      if (quiet && inflight.current) return;
      inflight.current = true;
      if (!quiet) {
        setLoading(true);
        setErr(null);
      }
      try {
        const q = new URLSearchParams({ pod, tail: String(tail) });
        if (container) q.set('container', container);
        const body = await text(`/debug/logs?${q.toString()}`);
        setLogs(body);
        // Keep the view pinned to the newest lines.
        requestAnimationFrame(() => {
          if (bodyRef.current) bodyRef.current.scrollTop = bodyRef.current.scrollHeight;
        });
      } catch (e) {
        if (!quiet) setErr(String((e as Error).message ?? e));
      } finally {
        inflight.current = false;
        setLoading(false);
      }
    },
    [pod, container, tail, text],
  );

  // Load on selection change.
  useEffect(() => {
    if (pod) load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pod, container, tail]);

  // Live tail.
  useEffect(() => {
    if (!live) return;
    const id = setInterval(() => load(true), LIVE_MS);
    return () => clearInterval(id);
  }, [live, load]);

  const shown = useMemo(() => {
    if (!filter.trim()) return logs;
    const needle = filter.toLowerCase();
    return logs
      .split('\n')
      .filter((l) => l.toLowerCase().includes(needle))
      .join('\n');
  }, [logs, filter]);

  function download() {
    const blob = new Blob([logs], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `${pod}${container ? '.' + container : ''}.log`;
    a.click();
    URL.revokeObjectURL(url);
  }

  return (
    <div className="logs">
      <div className="logs__head">
        <Heading level={1} chevron>
          logs
        </Heading>
        <Text variant="muted">container logs · secrets redacted</Text>
      </div>

      <Card>
        <div className="logs__bar">
          <label className="logs__field">
            <Text variant="ui">pod</Text>
            <Select
              value={pod}
              onChange={(e) => {
                const next = e.target.value;
                setPod(next);
                const c = pods.find((p) => p.pod === next)?.containers ?? [];
                setContainer(c[0] ?? '');
              }}
            >
              {pods.map((p) => (
                <option key={p.pod} value={p.pod}>
                  {p.pod}
                </option>
              ))}
            </Select>
          </label>

          <label className="logs__field">
            <Text variant="ui">container</Text>
            <Select value={container} onChange={(e) => setContainer(e.target.value)}>
              {containers.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </Select>
          </label>

          <label className="logs__field logs__field--sm">
            <Text variant="ui">tail</Text>
            <Select value={String(tail)} onChange={(e) => setTail(Number(e.target.value))}>
              {[200, 500, 1000, 2000, 5000].map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </Select>
          </label>

          <label className="logs__field logs__field--grow">
            <Text variant="ui">filter</Text>
            <Input
              placeholder="substring…"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </label>

          <div className="logs__actions">
            <Button variant="default" size="sm" onClick={() => load()} loading={loading}>
              <RefreshCw size={14} /> refresh
            </Button>
            <Button
              variant={live ? 'primary' : 'default'}
              size="sm"
              onClick={() => setLive((v) => !v)}
            >
              {live ? <Pause size={14} /> : <Play size={14} />} {live ? 'live' : 'live'}
            </Button>
            <Button variant="default" size="sm" onClick={download} disabled={!logs}>
              <Download size={14} /> download
            </Button>
          </div>
        </div>

        <div className="logs__meta">
          {pods.length === 0 && !err ? (
            <span className="logs__spin">
              <Spinner /> <Text variant="muted">loading pods…</Text>
            </span>
          ) : (
            <>
              {pods.find((p) => p.pod === pod) && (
                <Badge tone={phaseTone(pods.find((p) => p.pod === pod)!.phase)}>
                  {pods.find((p) => p.pod === pod)!.phase}
                </Badge>
              )}
              {filter.trim() && (
                <Text variant="dim">
                  {shown.split('\n').filter(Boolean).length} matching lines
                </Text>
              )}
              {live && <Badge tone="green">live</Badge>}
            </>
          )}
        </div>

        {err && (
          <Text variant="muted" className="logs__err">
            {err}
          </Text>
        )}

        <pre ref={bodyRef} className="logs__body">
          {shown || (loading ? '' : 'no output')}
        </pre>
      </Card>
    </div>
  );
}
