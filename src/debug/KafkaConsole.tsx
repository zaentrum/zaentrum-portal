import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Card, Badge, Button, Select, Spinner, Text, Heading, Input } from '@nalet/design-system';
import { RefreshCw, Play, Pause, ChevronRight } from 'lucide-react';
import {
  usePortalApi,
  type KafkaTopology,
  type KafkaEvent,
} from '../lib/api';
import './kafka.css';

const LIVE_MS = 4000;

// Short label for a topic: drop the platform prefix so rows stay scannable.
function shortTopic(t: string): string {
  return t.replace(/^stube\./, '');
}

function relTime(iso?: string): string {
  if (!iso) return '—';
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '—';
  const s = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  return `${Math.round(s / 3600)}h ago`;
}

function hhmmss(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toISOString().slice(11, 19);
}

// The Kafka event console: the platform's live bus, from inside the portal. Top
// card is the topology (topics → partitions, consumer groups, observed activity);
// below is the live event tail (newest first, secrets redacted server-side).
// Admin-only (RequireAdmin + the API gate). Read-only — the tap never produces.
export function KafkaConsole() {
  const api = usePortalApi();

  const [topo, setTopo] = useState<KafkaTopology | null>(null);
  const [events, setEvents] = useState<KafkaEvent[]>([]);
  const [topic, setTopic] = useState('');
  const [limit, setLimit] = useState(250);
  const [filter, setFilter] = useState('');
  const [live, setLive] = useState(false);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<number | null>(null);
  const inflight = useRef(false);

  const loadTopology = useCallback(() => {
    api<KafkaTopology>('/debug/kafka/topology')
      .then(setTopo)
      .catch((e) => setErr(String(e.message ?? e)));
  }, [api]);

  const loadEvents = useCallback(
    async (quiet = false) => {
      if (quiet && inflight.current) return;
      inflight.current = true;
      if (!quiet) {
        setLoading(true);
        setErr(null);
      }
      try {
        const q = new URLSearchParams({ limit: String(limit) });
        if (topic) q.set('topic', topic);
        const ev = await api<KafkaEvent[]>(`/debug/kafka/events?${q.toString()}`);
        setEvents(ev);
      } catch (e) {
        if (!quiet) setErr(String((e as Error).message ?? e));
      } finally {
        inflight.current = false;
        setLoading(false);
      }
    },
    [api, topic, limit],
  );

  useEffect(() => {
    loadTopology();
  }, [loadTopology]);

  useEffect(() => {
    loadEvents();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [topic, limit]);

  useEffect(() => {
    if (!live) return;
    const id = setInterval(() => {
      loadEvents(true);
      loadTopology();
    }, LIVE_MS);
    return () => clearInterval(id);
  }, [live, loadEvents, loadTopology]);

  const shown = useMemo(() => {
    if (!filter.trim()) return events;
    const needle = filter.toLowerCase();
    return events.filter((e) =>
      [e.topic, e.type, e.itemId, e.key, e.payload]
        .filter(Boolean)
        .some((s) => s!.toLowerCase().includes(needle)),
    );
  }, [events, filter]);

  const unavailable = topo && !topo.available;

  return (
    <div className="kaf">
      <div className="kaf__head">
        <Heading level={1} chevron>
          events
        </Heading>
        <Text variant="muted">kafka topology · live tail · secrets redacted</Text>
      </div>

      {unavailable && (
        <Card>
          <div className="kaf__empty">
            <Text variant="muted">{topo?.note || 'Kafka introspection is unavailable.'}</Text>
          </div>
        </Card>
      )}

      {/* ─── topology ─── */}
      <Card>
        <div className="kaf__section">
          <Text variant="ui">topology</Text>
          {topo?.brokers?.length ? (
            <Text variant="dim">
              {topo.brokers.join(', ')} · {topo.topics?.length ?? 0} topics ·{' '}
              {topo.groups?.length ?? 0} groups
            </Text>
          ) : (
            <span className="kaf__spin">
              <Spinner /> <Text variant="muted">loading…</Text>
            </span>
          )}
        </div>

        {topo?.topics?.length ? (
          <div className="kaf__topics">
            <div className="kaf__trow kaf__trow--head">
              <span>topic</span>
              <span>part.</span>
              <span>consumers</span>
              <span>seen</span>
              <span>last</span>
            </div>
            {topo.topics.map((t) => (
              <div className="kaf__trow" key={t.topic}>
                <span className="kaf__topic" title={t.topic}>
                  {shortTopic(t.topic)}
                </span>
                <span>{t.partitions}</span>
                <span className="kaf__consumers">
                  {t.consumers?.length ? (
                    t.consumers.map((c) => (
                      <Badge key={c} tone="blue">
                        {c}
                      </Badge>
                    ))
                  ) : (
                    <Text variant="dim">—</Text>
                  )}
                </span>
                <span>{t.seen > 0 ? <Badge tone="green">{t.seen}</Badge> : <Text variant="dim">0</Text>}</span>
                <span className="kaf__rel">{relTime(t.lastEvent)}</span>
              </div>
            ))}
          </div>
        ) : (
          topo?.available && (
            <div className="kaf__empty">
              <Text variant="dim">no topics yet — trigger a scan to see the pipeline light up</Text>
            </div>
          )
        )}
      </Card>

      {/* ─── live events ─── */}
      <Card>
        <div className="kaf__bar">
          <label className="kaf__field">
            <Text variant="ui">topic</Text>
            <Select value={topic} onChange={(e) => setTopic(e.target.value)}>
              <option value="">all topics</option>
              {topo?.topics?.map((t) => (
                <option key={t.topic} value={t.topic}>
                  {shortTopic(t.topic)}
                </option>
              ))}
            </Select>
          </label>

          <label className="kaf__field kaf__field--sm">
            <Text variant="ui">limit</Text>
            <Select value={String(limit)} onChange={(e) => setLimit(Number(e.target.value))}>
              {[100, 250, 500].map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </Select>
          </label>

          <label className="kaf__field kaf__field--grow">
            <Text variant="ui">filter</Text>
            <Input
              placeholder="topic · type · itemId · payload…"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </label>

          <div className="kaf__actions">
            <Button variant="default" size="sm" onClick={() => loadEvents()} loading={loading}>
              <RefreshCw size={14} /> refresh
            </Button>
            <Button
              variant={live ? 'primary' : 'default'}
              size="sm"
              onClick={() => setLive((v) => !v)}
            >
              {live ? <Pause size={14} /> : <Play size={14} />} live
            </Button>
          </div>
        </div>

        <div className="kaf__meta">
          <Text variant="dim">
            {shown.length} event{shown.length === 1 ? '' : 's'}
            {filter.trim() ? ` (of ${events.length})` : ''}
          </Text>
          {live && <Badge tone="green">live</Badge>}
        </div>

        {err && (
          <Text variant="muted" className="kaf__err">
            {err}
          </Text>
        )}

        <div className="kaf__events">
          {shown.length === 0 && !loading ? (
            <div className="kaf__empty">
              <Text variant="dim">no events buffered — the tap tails live; trigger activity to populate</Text>
            </div>
          ) : (
            shown.map((e) => {
              const open = expanded === e.seq;
              return (
                <div className="kaf__ev" key={`${e.topic}-${e.partition}-${e.offset}-${e.seq}`}>
                  <button
                    className="kaf__evhead"
                    onClick={() => setExpanded(open ? null : e.seq)}
                    aria-expanded={open}
                  >
                    <ChevronRight size={13} className={open ? 'kaf__chev kaf__chev--open' : 'kaf__chev'} />
                    <span className="kaf__time">{hhmmss(e.time)}</span>
                    <span className="kaf__evtopic">{shortTopic(e.topic)}</span>
                    {e.type ? <Badge tone="blue">{e.type}</Badge> : null}
                    {e.itemId ? <span className="kaf__item">{e.itemId}</span> : null}
                    <span className="kaf__off">
                      p{e.partition}·{e.offset}
                    </span>
                  </button>
                  {open && <pre className="kaf__payload">{e.payload}</pre>}
                </div>
              );
            })
          )}
        </div>
      </Card>
    </div>
  );
}
