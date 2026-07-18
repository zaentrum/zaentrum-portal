import { useCallback, useEffect, useMemo, useState } from 'react';
import { Card, Badge, Button, Select, Spinner, Text, Heading, Input } from '@nalet/design-system';
import { RefreshCw, ChevronLeft, ChevronRight, Database } from 'lucide-react';
import { usePortalApi, type DbTable, type DbPage } from '../lib/api';
import './db.css';

// The curated DB browser: a read-only window onto a whitelist of platform tables
// (catalog items, pipeline status/steps, playback assets, portal registry). No
// arbitrary SQL — the server runs fixed SELECTs inside read-only transactions and
// masks secret-shaped values. Admin-only (RequireAdmin + the API gate).
export function DbConsole() {
  const api = usePortalApi();

  const [tables, setTables] = useState<DbTable[]>([]);
  const [table, setTable] = useState('');
  const [limit, setLimit] = useState(100);
  const [offset, setOffset] = useState(0);
  const [page, setPage] = useState<DbPage | null>(null);
  const [filter, setFilter] = useState('');
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Load the curated table list once.
  useEffect(() => {
    api<DbTable[]>('/debug/db/tables')
      .then((ts) => {
        setTables(ts);
        if (ts.length && !table) setTable(ts[0].key);
      })
      .catch((e) => setErr(String(e.message ?? e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const load = useCallback(async () => {
    if (!table) return;
    setLoading(true);
    setErr(null);
    try {
      const q = new URLSearchParams({ table, limit: String(limit), offset: String(offset) });
      setPage(await api<DbPage>(`/debug/db/rows?${q.toString()}`));
    } catch (e) {
      setErr(String((e as Error).message ?? e));
      setPage(null);
    } finally {
      setLoading(false);
    }
  }, [api, table, limit, offset]);

  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [table, limit, offset]);

  // Reset to the first page when the selected table changes.
  useEffect(() => {
    setOffset(0);
    setFilter('');
  }, [table]);

  const meta = useMemo(() => tables.find((t) => t.key === table), [tables, table]);

  const shownRows = useMemo(() => {
    if (!page) return [];
    if (!filter.trim()) return page.rows;
    const needle = filter.toLowerCase();
    return page.rows.filter((r) => r.some((c) => c.toLowerCase().includes(needle)));
  }, [page, filter]);

  const total = page?.total ?? 0;
  const from = total === 0 ? 0 : offset + 1;
  const to = Math.min(offset + limit, total);
  const canPrev = offset > 0;
  const canNext = offset + limit < total;

  return (
    <div className="db">
      <div className="db__head">
        <Heading level={1} chevron>
          database
        </Heading>
        <Text variant="muted">curated · read-only · secrets masked</Text>
      </div>

      <Card>
        <div className="db__bar">
          <label className="db__field db__field--grow">
            <Text variant="ui">table</Text>
            <Select value={table} onChange={(e) => setTable(e.target.value)}>
              {tables.map((t) => (
                <option key={t.key} value={t.key}>
                  {t.label} ({t.rows < 0 ? '?' : t.rows})
                </option>
              ))}
            </Select>
          </label>

          <label className="db__field db__field--sm">
            <Text variant="ui">page size</Text>
            <Select
              value={String(limit)}
              onChange={(e) => {
                setLimit(Number(e.target.value));
                setOffset(0);
              }}
            >
              {[25, 50, 100, 250, 500].map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </Select>
          </label>

          <label className="db__field db__field--grow">
            <Text variant="ui">filter this page</Text>
            <Input placeholder="substring…" value={filter} onChange={(e) => setFilter(e.target.value)} />
          </label>

          <div className="db__actions">
            <Button variant="default" size="sm" onClick={() => load()} loading={loading}>
              <RefreshCw size={14} /> refresh
            </Button>
          </div>
        </div>

        <div className="db__meta">
          {meta && (
            <>
              <Badge tone="neutral">{meta.db}</Badge>
              <Text variant="dim">{meta.description}</Text>
            </>
          )}
          <span className="db__pager">
            <Text variant="dim">
              {from}–{to} of {total}
              {filter.trim() ? ` · ${shownRows.length} match` : ''}
            </Text>
            <Button variant="default" size="sm" disabled={!canPrev} onClick={() => setOffset(Math.max(0, offset - limit))}>
              <ChevronLeft size={14} />
            </Button>
            <Button variant="default" size="sm" disabled={!canNext} onClick={() => setOffset(offset + limit)}>
              <ChevronRight size={14} />
            </Button>
          </span>
        </div>

        {err && (
          <Text variant="muted" className="db__err">
            {err}
          </Text>
        )}

        <div className="db__tablewrap">
          {loading && !page ? (
            <div className="db__empty">
              <Spinner /> <Text variant="muted">loading…</Text>
            </div>
          ) : page && page.columns.length ? (
            <table className="db__table">
              <thead>
                <tr>
                  <th className="db__num">#</th>
                  {page.columns.map((c) => (
                    <th key={c}>{c}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {shownRows.map((r, i) => (
                  <tr key={i}>
                    <td className="db__num">{offset + i + 1}</td>
                    {r.map((cell, j) => (
                      <td key={j} title={cell}>
                        {cell === '' ? <span className="db__null">∅</span> : cell}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <div className="db__empty">
              <Database size={16} /> <Text variant="dim">no rows</Text>
            </div>
          )}
        </div>
      </Card>
    </div>
  );
}
