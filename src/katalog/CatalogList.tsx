import { useState } from 'react';
import { Table, Badge, Input, Select, Field, Spinner, Text } from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { Search } from 'lucide-react';
import { useNavigate } from 'react-router-dom';
import { useQuery } from '../lib/useQuery';
import { statusTone } from './status';

interface Item {
  id: string;
  type: string;
  title: string;
  year: number | null;
  posterUrl: string | null;
  isPackaged: boolean;
  overallStatus: { overallStatus: string | null } | null;
}

const LIST_Q = `query Catalog($type: String, $genre: String, $year: Int, $search: String) {
  items(type: $type, genre: $genre, year: $year, search: $search, limit: 200) {
    id type title year posterUrl isPackaged overallStatus { overallStatus }
  }
}`;

const GENRES_Q = `{ genres { name } }`;

export function CatalogList() {
  const nav = useNavigate();
  const [search, setSearch] = useState('');
  const [type, setType] = useState('');
  const [genre, setGenre] = useState('');
  const [year, setYear] = useState('');

  const vars = {
    search: search || null,
    type: type || null,
    genre: genre || null,
    year: year ? parseInt(year, 10) : null,
  };
  const { data, loading, error } = useQuery<{ items: Item[] }>(LIST_Q, vars, [search, type, genre, year]);
  const genresData = useQuery<{ genres: { name: string }[] }>(GENRES_Q);

  const columns: TableColumn<Item>[] = [
    {
      key: 'posterUrl',
      header: '',
      width: 40,
      render: (r) =>
        r.posterUrl ? (
          <img
            className="kat__poster"
            src={r.posterUrl}
            alt=""
            onError={(e) => ((e.target as HTMLImageElement).style.visibility = 'hidden')}
          />
        ) : (
          <div className="kat__poster" />
        ),
    },
    {
      key: 'title',
      header: 'title',
      render: (r) => (
        <span className="kat__rowlink" onClick={() => nav(`/katalog/item/${r.id}`)}>
          {r.title}
        </span>
      ),
    },
    { key: 'type', header: 'type', render: (r) => <span className="kat__mono">{r.type}</span> },
    { key: 'year', header: 'year', align: 'right', render: (r) => r.year ?? '—' },
    {
      key: 'isPackaged',
      header: 'packaged',
      render: (r) =>
        r.isPackaged ? (
          <Badge tone="green" dot>
            ready
          </Badge>
        ) : (
          <Badge tone="neutral">—</Badge>
        ),
    },
    {
      key: 'id',
      header: 'status',
      render: (r) => {
        const s = r.overallStatus?.overallStatus ?? 'unknown';
        return <Badge tone={statusTone(s)}>{s}</Badge>;
      },
    },
  ];

  return (
    <div>
      <div className="kat__filters">
        <Field label="search">
          <Input
            placeholder="title…"
            value={search}
            leading={<Search size={14} />}
            onChange={(e) => setSearch(e.target.value)}
          />
        </Field>
        <Field label="type">
          <Select
            value={type}
            onChange={(e) => setType(e.target.value)}
            options={[
              { label: 'all', value: '' },
              { label: 'movie', value: 'movie' },
              { label: 'series', value: 'series' },
              { label: 'episode', value: 'episode' },
              { label: 'album', value: 'album' },
            ]}
          />
        </Field>
        <Field label="genre">
          <Select
            value={genre}
            onChange={(e) => setGenre(e.target.value)}
            options={[
              { label: 'all', value: '' },
              ...(genresData.data?.genres ?? []).map((g) => ({ label: g.name, value: g.name })),
            ]}
          />
        </Field>
        <Field label="year">
          <Input type="number" placeholder="any" value={year} onChange={(e) => setYear(e.target.value)} />
        </Field>
        <div className="kat__spacer" />
        {!loading && data && <Text variant="dim">{data.items.length} items</Text>}
      </div>

      {error && <div className="kat__err">error: {error}</div>}
      {loading && !data ? (
        <div className="kat__state">
          <Spinner /> <Text variant="muted">loading catalog…</Text>
        </div>
      ) : (
        <Table
          columns={columns}
          rows={data?.items ?? []}
          rowKey={(r) => r.id}
          dense
          empty={<Text variant="muted">no items match.</Text>}
        />
      )}
    </div>
  );
}
