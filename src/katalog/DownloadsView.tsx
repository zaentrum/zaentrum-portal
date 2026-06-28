import { useMemo, useState } from 'react';
import { Table, Badge, Button, Text, Spinner, Modal, Field, Input, Select } from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { Plus, X } from 'lucide-react';
import { useQuery } from '../lib/useQuery';
import { useGql } from '../lib/gql';
import { statusTone, fmtTime } from './status';

interface DownloadJob {
  id: string;
  adapter: string;
  clientJobId: string;
  title: string | null;
  state: string;
  progressPct: number | null;
  sizeBytes: number | null;
  lastEventAt: string | null;
}

const JOBS_Q = `{
  downloadJobs(limit: 50) { id adapter clientJobId title state progressPct sizeBytes lastEventAt }
  downloadClients
}`;

function mb(n: number | null): string {
  return n != null ? `${Math.round(n / 1048576)} mb` : '—';
}

export function DownloadsView() {
  const gql = useGql();
  const { data, loading, error, refetch } = useQuery<{ downloadJobs: DownloadJob[]; downloadClients: string }>(JOBS_Q);
  const [adding, setAdding] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  const clients = useMemo<string[]>(() => {
    try {
      const arr = JSON.parse(data?.downloadClients ?? '[]') as unknown;
      return Array.isArray(arr) ? (arr as string[]) : [];
    } catch {
      return [];
    }
  }, [data?.downloadClients]);

  async function cancel(j: DownloadJob) {
    setMsg(null);
    try {
      const d = await gql<{ cancelDownload: { ok: boolean; message?: string } }>(
        `mutation($a:String!,$c:String!){ cancelDownload(adapter:$a, clientJobId:$c){ ok message } }`,
        { a: j.adapter, c: j.clientJobId },
      );
      setMsg(`cancel: ${d.cancelDownload.ok ? 'ok' : 'failed'}${d.cancelDownload.message ? ` — ${d.cancelDownload.message}` : ''}`);
      refetch();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    }
  }

  const cols: TableColumn<DownloadJob>[] = [
    { key: 'title', header: 'title', render: (r) => r.title || <span className="kat__muted">—</span> },
    { key: 'adapter', header: 'adapter', render: (r) => <span className="kat__mono">{r.adapter}</span> },
    { key: 'state', header: 'state', render: (r) => <Badge tone={statusTone(r.state)}>{r.state}</Badge> },
    {
      key: 'progressPct',
      header: 'progress',
      align: 'right',
      render: (r) => (r.progressPct != null ? `${r.progressPct.toFixed(0)}%` : '—'),
    },
    { key: 'sizeBytes', header: 'size', align: 'right', render: (r) => mb(r.sizeBytes) },
    { key: 'lastEventAt', header: 'updated', render: (r) => fmtTime(r.lastEventAt) },
    {
      key: 'id',
      header: '',
      align: 'right',
      render: (r) =>
        r.state === 'downloading' || r.state === 'queued' ? (
          <Button variant="ghost" size="sm" leading={<X size={13} />} onClick={() => cancel(r)}>
            cancel
          </Button>
        ) : null,
    },
  ];

  return (
    <div>
      <div className="kat__toolbar">
        <Button leading={<Plus size={15} />} onClick={() => setAdding(true)}>
          add download
        </Button>
        <Button variant="ghost" size="sm" onClick={refetch}>
          refresh
        </Button>
        {msg && <span className="kat__ok kat__mono">{msg}</span>}
      </div>
      {error && <div className="kat__err">error: {error}</div>}
      {loading && !data ? (
        <div className="kat__state">
          <Spinner /> <Text variant="muted">loading downloads…</Text>
        </div>
      ) : (
        <Table
          columns={cols}
          rows={data?.downloadJobs ?? []}
          rowKey={(r) => r.id}
          dense
          empty={<Text variant="muted">no downloads.</Text>}
        />
      )}

      {adding && (
        <AddDownload
          clients={clients}
          onClose={() => setAdding(false)}
          onDone={(m) => {
            setMsg(m);
            setAdding(false);
            refetch();
          }}
        />
      )}
    </div>
  );
}

function AddDownload({
  clients,
  onClose,
  onDone,
}: {
  clients: string[];
  onClose: () => void;
  onDone: (msg: string) => void;
}) {
  const gql = useGql();
  const [adapter, setAdapter] = useState(clients[0] ?? 'odownloader');
  const [source, setSource] = useState('');
  const [title, setTitle] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function submit() {
    if (!source.trim()) {
      setErr('source url/magnet is required');
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      const d = await gql<{ addDownload: { ok: boolean; clientJobId?: string; message?: string } }>(
        `mutation($a:String!,$s:String!,$t:String){ addDownload(adapter:$a, source:$s, title:$t){ ok clientJobId message } }`,
        { a: adapter, s: source, t: title || null },
      );
      const r = d.addDownload;
      onDone(`add: ${r.ok ? 'queued' : 'failed'}${r.message ? ` — ${r.message}` : ''}`);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  const adapterOpts = (clients.length ? clients : ['odownloader', 'qbittorrent', 'nzbget']).map((c) => ({
    label: c,
    value: c,
  }));

  return (
    <Modal
      open
      onClose={onClose}
      title="add download"
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onClose}>
            cancel
          </Button>
          <Button size="sm" loading={busy} onClick={submit}>
            add
          </Button>
        </>
      }
    >
      <div className="kat__form">
        <Field label="adapter">
          <Select value={adapter} onChange={(e) => setAdapter(e.target.value)} options={adapterOpts} />
        </Field>
        <Field label="source" error={err ?? undefined}>
          <Input placeholder="magnet:… or url" value={source} onChange={(e) => setSource(e.target.value)} />
        </Field>
        <Field label="title (optional)">
          <Input placeholder="display title" value={title} onChange={(e) => setTitle(e.target.value)} />
        </Field>
      </div>
    </Modal>
  );
}
