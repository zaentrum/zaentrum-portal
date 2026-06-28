import { useState } from 'react';
import { Table, Badge, Button, Text, Spinner } from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { ScanLine } from 'lucide-react';
import { useQuery } from '../lib/useQuery';
import { useGql } from '../lib/gql';
import { statusTone, fmtTime } from './status';

interface ScanJob {
  id: string;
  source: string;
  status: string;
  startedAt: string | null;
  finishedAt: string | null;
  filesSeen: number | null;
  itemsInserted: number | null;
  itemsUpdated: number | null;
}

const JOBS_Q = `{ scanJobs(limit: 25) {
  id source status startedAt finishedAt filesSeen itemsInserted itemsUpdated
} }`;

export function ScanView() {
  const gql = useGql();
  const { data, loading, error, refetch } = useQuery<{ scanJobs: ScanJob[] }>(JOBS_Q);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  async function trigger() {
    setBusy(true);
    setMsg(null);
    try {
      const d = await gql<{ triggerScan: { id: string; status: string } }>(
        `mutation { triggerScan(source: "nfs") { id status } }`,
      );
      setMsg(`scan ${d.triggerScan.status} (${d.triggerScan.id.slice(0, 8)})`);
      refetch();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  const cols: TableColumn<ScanJob>[] = [
    { key: 'source', header: 'source', render: (r) => <span className="kat__mono">{r.source}</span> },
    { key: 'status', header: 'status', render: (r) => <Badge tone={statusTone(r.status)}>{r.status}</Badge> },
    { key: 'startedAt', header: 'started', render: (r) => fmtTime(r.startedAt) },
    { key: 'filesSeen', header: 'files', align: 'right', render: (r) => r.filesSeen ?? 0 },
    { key: 'itemsInserted', header: 'inserted', align: 'right', render: (r) => r.itemsInserted ?? 0 },
    { key: 'itemsUpdated', header: 'updated', align: 'right', render: (r) => r.itemsUpdated ?? 0 },
  ];

  return (
    <div>
      <div className="kat__toolbar">
        <Button leading={<ScanLine size={15} />} loading={busy} onClick={trigger}>
          trigger scan
        </Button>
        <Button variant="ghost" size="sm" onClick={refetch}>
          refresh
        </Button>
        {msg && <span className="kat__ok kat__mono">{msg}</span>}
      </div>
      {error && <div className="kat__err">error: {error}</div>}
      {loading && !data ? (
        <div className="kat__state">
          <Spinner /> <Text variant="muted">loading jobs…</Text>
        </div>
      ) : (
        <Table
          columns={cols}
          rows={data?.scanJobs ?? []}
          rowKey={(r) => r.id}
          dense
          empty={<Text variant="muted">no scan jobs yet.</Text>}
        />
      )}
    </div>
  );
}
