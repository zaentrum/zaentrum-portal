import { useState } from 'react';
import {
  Table,
  Badge,
  Button,
  Heading,
  Text,
  Tabs,
  Spinner,
  Divider,
} from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { ArrowLeft, Sparkles, Package, CheckCircle2, Film } from 'lucide-react';
import { useNavigate, useParams } from 'react-router-dom';
import { useQuery } from '../lib/useQuery';
import { useGql } from '../lib/gql';
import { statusTone } from './status';

interface Step {
  step: string;
  status: string;
  attempts: number | null;
  error: string | null;
  finishedAt: string | null;
}
interface Asset {
  path: string;
  kind: string | null;
  codec: string | null;
  resolution: string | null;
  sizeMB: number | null;
  audioCodec: string | null;
  audioChannels: number | null;
}
interface Segment {
  kind: string;
  startMs: number;
  endMs: number;
  source: string;
  confidence: number | null;
}
interface Chapter {
  ordinal: number | null;
  title: string | null;
  startMs: number;
  endMs: number;
}
interface Trailer {
  site: string | null;
  title: string | null;
  url: string;
  downloadedAt: string | null;
}
interface Person {
  role: string;
  person: { name: string };
}
interface Item {
  id: string;
  type: string;
  title: string;
  year: number | null;
  tagline: string | null;
  rating: number | null;
  description: string | null;
  posterUrl: string | null;
  runtimeMin: number | null;
  isPackaged: boolean;
  overallStatus: { overallStatus: string | null; doneCount: number | null; totalSteps: number | null } | null;
  processingSteps: Step[];
  assets: Asset[];
  segments: Segment[];
  chapters: Chapter[];
  trailerLinks: Trailer[];
  genres: { name: string }[];
  people: Person[];
  tags: string[];
  externalIds: { source: string; externalId: string }[];
  diagnostics: { sourcePath: string | null; sourceSize: number | null; notes: string | null } | null;
}

const ITEM_Q = `query Item($id: ID!) {
  item(id: $id) {
    id type title year tagline rating description posterUrl runtimeMin isPackaged
    overallStatus { overallStatus doneCount totalSteps }
    processingSteps { step status attempts error finishedAt }
    assets { path kind codec resolution sizeMB audioCodec audioChannels }
    segments { kind startMs endMs source confidence }
    chapters { ordinal title startMs endMs }
    trailerLinks { site title url downloadedAt }
    genres { name }
    people { role person { name } }
    tags
    externalIds { source externalId }
    diagnostics { sourcePath sourceSize notes }
  }
}`;

function ms(t: number): string {
  const s = Math.floor(t / 1000);
  const m = Math.floor(s / 60);
  return `${m}:${String(s % 60).padStart(2, '0')}`;
}

export function ItemDetail() {
  const { id = '' } = useParams();
  const nav = useNavigate();
  const gql = useGql();
  const { data, loading, error, refetch } = useQuery<{ item: Item | null }>(ITEM_Q, { id }, [id]);
  const [tab, setTab] = useState('overview');
  const [busy, setBusy] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);

  const item = data?.item;

  async function run(label: string, mutation: string, pick: (d: Record<string, unknown>) => string) {
    setBusy(label);
    setMsg(null);
    try {
      const d = await gql<Record<string, unknown>>(mutation, { id });
      setMsg(pick(d));
      refetch();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  if (loading && !item) {
    return (
      <div className="kat__state">
        <Spinner /> <Text variant="muted">loading item…</Text>
      </div>
    );
  }
  if (error) return <div className="kat__err">error: {error}</div>;
  if (!item) return <Text variant="muted">item not found.</Text>;

  const TABS = [
    { value: 'overview', label: 'overview' },
    { value: 'steps', label: `steps (${item.processingSteps.length})` },
    { value: 'assets', label: `assets (${item.assets.length})` },
    { value: 'media', label: `segments (${item.segments.length})` },
    { value: 'chapters', label: `chapters (${item.chapters.length})` },
    { value: 'trailers', label: `trailers (${item.trailerLinks.length})` },
    { value: 'cast', label: `cast (${item.people.length})` },
    { value: 'diagnostics', label: 'diagnostics' },
  ];

  return (
    <div>
      <Button variant="ghost" size="sm" leading={<ArrowLeft size={14} />} onClick={() => nav('/katalog')}>
        catalog
      </Button>

      <div className="kat__obj-head">
        {item.posterUrl && (
          <img
            className="kat__obj-poster"
            src={item.posterUrl}
            alt=""
            onError={(e) => ((e.target as HTMLImageElement).style.visibility = 'hidden')}
          />
        )}
        <div className="kat__obj-meta">
          <Heading level={1} chevron>
            {item.title}
          </Heading>
          <div className="kat__obj-actions">
            <Badge tone="blue">{item.type}</Badge>
            {item.year != null && <Badge tone="neutral">{item.year}</Badge>}
            {item.rating != null && <Badge tone="neutral">★ {item.rating}</Badge>}
            {item.isPackaged && (
              <Badge tone="green" dot>
                packaged
              </Badge>
            )}
            {item.overallStatus?.overallStatus && (
              <Badge tone={statusTone(item.overallStatus.overallStatus)}>
                {item.overallStatus.overallStatus} ({item.overallStatus.doneCount ?? 0}/{item.overallStatus.totalSteps ?? 0})
              </Badge>
            )}
          </div>
          {item.tagline && <Text variant="muted">{item.tagline}</Text>}
          <div className="kat__obj-actions">
            <Button
              size="sm"
              leading={<Sparkles size={14} />}
              loading={busy === 'enrich'}
              onClick={() => run('enrich', `mutation($id:ID!){ enrichOne(id:$id){ status message } }`, (d) => {
                const r = d.enrichOne as { status: string; message?: string };
                return `enrich: ${r.status}${r.message ? ` — ${r.message}` : ''}`;
              })}
            >
              enrich
            </Button>
            <Button
              size="sm"
              leading={<Package size={14} />}
              loading={busy === 'package'}
              onClick={() => run('package', `mutation($id:ID!){ packageItem(id:$id){ status message alreadyActive episodesEnqueued } }`, (d) => {
                const r = d.packageItem as { status?: string; message?: string; episodesEnqueued?: number };
                return `package: ${r.status ?? ''}${r.episodesEnqueued != null ? ` (${r.episodesEnqueued} episodes)` : ''}${r.message ? ` — ${r.message}` : ''}`;
              })}
            >
              package
            </Button>
            <Button
              size="sm"
              variant="default"
              leading={<CheckCircle2 size={14} />}
              loading={busy === 'validate'}
              onClick={() => run('validate', `mutation($id:ID!){ validateItem(id:$id){ code message findings{code} } }`, (d) => {
                const r = d.validateItem as { code: string; message: string };
                return `validate: ${r.code} — ${r.message}`;
              })}
            >
              validate
            </Button>
            <Button
              size="sm"
              variant="default"
              leading={<Film size={14} />}
              loading={busy === 'trailers'}
              onClick={() => run('trailers', `mutation($id:ID!){ fetchTrailers(id:$id){ enqueued message } }`, (d) => {
                const r = d.fetchTrailers as { enqueued: number; message?: string };
                return `trailers: enqueued ${r.enqueued}${r.message ? ` — ${r.message}` : ''}`;
              })}
            >
              fetch trailers
            </Button>
          </div>
          {msg && <div className="kat__ok kat__mono">{msg}</div>}
        </div>
      </div>

      <Divider />
      <Tabs items={TABS} value={tab} onChange={setTab} />

      <div className="kat__facet">
        {tab === 'overview' && <Overview item={item} />}
        {tab === 'steps' && <StepsTab steps={item.processingSteps} />}
        {tab === 'assets' && <AssetsTab assets={item.assets} />}
        {tab === 'media' && <SegmentsTab segments={item.segments} />}
        {tab === 'chapters' && <ChaptersTab chapters={item.chapters} />}
        {tab === 'trailers' && <TrailersTab trailers={item.trailerLinks} />}
        {tab === 'cast' && <CastTab people={item.people} />}
        {tab === 'diagnostics' && <DiagnosticsTab item={item} />}
      </div>
    </div>
  );
}

function Overview({ item }: { item: Item }) {
  return (
    <dl className="kat__kv">
      <dt>description</dt>
      <dd>{item.description || <span className="kat__muted">—</span>}</dd>
      <dt>runtime</dt>
      <dd>{item.runtimeMin != null ? `${item.runtimeMin} min` : '—'}</dd>
      <dt>genres</dt>
      <dd>{item.genres.length ? item.genres.map((g) => g.name).join(', ') : '—'}</dd>
      <dt>tags</dt>
      <dd>{item.tags.length ? item.tags.join(', ') : '—'}</dd>
      <dt>external ids</dt>
      <dd className="kat__mono">
        {item.externalIds.length ? item.externalIds.map((x) => `${x.source}:${x.externalId}`).join('  ') : '—'}
      </dd>
    </dl>
  );
}

function StepsTab({ steps }: { steps: Step[] }) {
  const cols: TableColumn<Step>[] = [
    { key: 'step', header: 'step', render: (r) => <span className="kat__mono">{r.step}</span> },
    { key: 'status', header: 'status', render: (r) => <Badge tone={statusTone(r.status)}>{r.status}</Badge> },
    { key: 'attempts', header: 'tries', align: 'right', render: (r) => r.attempts ?? 0 },
    { key: 'error', header: 'error', render: (r) => r.error || <span className="kat__muted">—</span> },
  ];
  return <Table columns={cols} rows={steps} rowKey={(r) => r.step} dense empty={<Text variant="muted">no steps.</Text>} />;
}

function AssetsTab({ assets }: { assets: Asset[] }) {
  const cols: TableColumn<Asset>[] = [
    { key: 'kind', header: 'kind', render: (r) => <span className="kat__mono">{r.kind ?? 'primary'}</span> },
    { key: 'path', header: 'path', render: (r) => <span className="kat__mono kat__muted">{r.path}</span> },
    { key: 'codec', header: 'codec', render: (r) => r.codec ?? '—' },
    { key: 'resolution', header: 'res', render: (r) => r.resolution ?? '—' },
    { key: 'sizeMB', header: 'mb', align: 'right', render: (r) => r.sizeMB ?? '—' },
    { key: 'audioCodec', header: 'audio', render: (r) => (r.audioCodec ? `${r.audioCodec} ${r.audioChannels ?? ''}ch` : '—') },
  ];
  return <Table columns={cols} rows={assets} rowKey={(r) => r.path} dense empty={<Text variant="muted">no assets.</Text>} />;
}

function SegmentsTab({ segments }: { segments: Segment[] }) {
  const cols: TableColumn<Segment>[] = [
    { key: 'kind', header: 'kind', render: (r) => <Badge tone="blue">{r.kind}</Badge> },
    { key: 'startMs', header: 'start', align: 'right', render: (r) => ms(r.startMs) },
    { key: 'endMs', header: 'end', align: 'right', render: (r) => ms(r.endMs) },
    { key: 'source', header: 'source', render: (r) => <span className="kat__mono">{r.source}</span> },
    { key: 'confidence', header: 'conf', align: 'right', render: (r) => (r.confidence != null ? r.confidence.toFixed(2) : '—') },
  ];
  return <Table columns={cols} rows={segments} rowKey={(_, i) => i} dense empty={<Text variant="muted">no segments.</Text>} />;
}

function ChaptersTab({ chapters }: { chapters: Chapter[] }) {
  const cols: TableColumn<Chapter>[] = [
    { key: 'ordinal', header: '#', align: 'right', render: (r) => r.ordinal ?? '—' },
    { key: 'title', header: 'title', render: (r) => r.title || <span className="kat__muted">—</span> },
    { key: 'startMs', header: 'start', align: 'right', render: (r) => ms(r.startMs) },
    { key: 'endMs', header: 'end', align: 'right', render: (r) => ms(r.endMs) },
  ];
  return <Table columns={cols} rows={chapters} rowKey={(_, i) => i} dense empty={<Text variant="muted">no chapters.</Text>} />;
}

function TrailersTab({ trailers }: { trailers: Trailer[] }) {
  const cols: TableColumn<Trailer>[] = [
    { key: 'title', header: 'title', render: (r) => r.title || <span className="kat__muted">—</span> },
    { key: 'site', header: 'site', render: (r) => r.site ?? '—' },
    {
      key: 'url',
      header: 'url',
      render: (r) => (
        <a className="kat__rowlink kat__mono" href={r.url} target="_blank" rel="noreferrer">
          open
        </a>
      ),
    },
    {
      key: 'downloadedAt',
      header: 'local',
      render: (r) => (r.downloadedAt ? <Badge tone="green" dot>downloaded</Badge> : <Badge tone="neutral">remote</Badge>),
    },
  ];
  return <Table columns={cols} rows={trailers} rowKey={(_, i) => i} dense empty={<Text variant="muted">no trailers.</Text>} />;
}

function CastTab({ people }: { people: Person[] }) {
  const cols: TableColumn<Person>[] = [
    { key: 'role', header: 'role', render: (r) => <span className="kat__mono">{r.role}</span> },
    { key: 'person', header: 'name', render: (r) => r.person.name },
  ];
  return <Table columns={cols} rows={people} rowKey={(_, i) => i} dense empty={<Text variant="muted">no cast.</Text>} />;
}

function DiagnosticsTab({ item }: { item: Item }) {
  const d = item.diagnostics;
  if (!d) return <Text variant="muted">no diagnostics captured.</Text>;
  return (
    <dl className="kat__kv">
      <dt>source path</dt>
      <dd className="kat__mono kat__muted">{d.sourcePath ?? '—'}</dd>
      <dt>source size</dt>
      <dd>{d.sourceSize != null ? `${Math.round(d.sourceSize / 1048576)} mb` : '—'}</dd>
      <dt>notes</dt>
      <dd>{d.notes ?? '—'}</dd>
    </dl>
  );
}
