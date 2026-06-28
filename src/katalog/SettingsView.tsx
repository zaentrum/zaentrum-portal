import { useState } from 'react';
import { Table, Button, Text, Spinner, Modal, Field, Input, Select, Textarea, Badge } from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { Plus, Pencil, Trash2 } from 'lucide-react';
import { useQuery } from '../lib/useQuery';
import { useGql } from '../lib/gql';

interface Setting {
  id: string;
  key: string;
  valueText: string;
  valueType: string;
  description: string | null;
}

const Q = `{ settings { id key valueText valueType description } }`;
const TYPES = ['string', 'list_csv', 'bool', 'int', 'float'];

export function SettingsView() {
  const gql = useGql();
  const { data, loading, error, refetch } = useQuery<{ settings: Setting[] }>(Q);
  const [editing, setEditing] = useState<Setting | 'new' | null>(null);
  const [msg, setMsg] = useState<string | null>(null);

  async function remove(s: Setting) {
    if (!confirm(`delete setting "${s.key}"?`)) return;
    setMsg(null);
    try {
      await gql(`mutation($id:ID!){ deleteSetting(id:$id) }`, { id: s.id });
      setMsg(`deleted ${s.key}`);
      refetch();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    }
  }

  const cols: TableColumn<Setting>[] = [
    { key: 'key', header: 'key', render: (r) => <span className="kat__mono">{r.key}</span> },
    { key: 'valueText', header: 'value', render: (r) => <span className="kat__mono">{r.valueText || '—'}</span> },
    { key: 'valueType', header: 'type', render: (r) => <Badge tone="neutral">{r.valueType}</Badge> },
    { key: 'description', header: 'description', render: (r) => r.description || <span className="kat__muted">—</span> },
    {
      key: 'id',
      header: '',
      align: 'right',
      render: (r) => (
        <span style={{ display: 'inline-flex', gap: 4 }}>
          <Button variant="ghost" size="sm" leading={<Pencil size={13} />} onClick={() => setEditing(r)}>
            edit
          </Button>
          <Button variant="ghost" size="sm" leading={<Trash2 size={13} />} onClick={() => remove(r)}>
            del
          </Button>
        </span>
      ),
    },
  ];

  return (
    <div>
      <div className="kat__toolbar">
        <Button leading={<Plus size={15} />} onClick={() => setEditing('new')}>
          new setting
        </Button>
        <Button variant="ghost" size="sm" onClick={refetch}>
          refresh
        </Button>
        {msg && <span className="kat__ok kat__mono">{msg}</span>}
      </div>
      {error && <div className="kat__err">error: {error}</div>}
      {loading && !data ? (
        <div className="kat__state">
          <Spinner /> <Text variant="muted">loading settings…</Text>
        </div>
      ) : (
        <Table
          columns={cols}
          rows={data?.settings ?? []}
          rowKey={(r) => r.id}
          dense
          empty={<Text variant="muted">no settings.</Text>}
        />
      )}

      {editing && (
        <EditSetting
          setting={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
          onDone={(m) => {
            setMsg(m);
            setEditing(null);
            refetch();
          }}
        />
      )}
    </div>
  );
}

function EditSetting({
  setting,
  onClose,
  onDone,
}: {
  setting: Setting | null;
  onClose: () => void;
  onDone: (msg: string) => void;
}) {
  const gql = useGql();
  const isNew = setting === null;
  const [key, setKey] = useState(setting?.key ?? '');
  const [valueText, setValueText] = useState(setting?.valueText ?? '');
  const [valueType, setValueType] = useState(setting?.valueType ?? 'string');
  const [description, setDescription] = useState(setting?.description ?? '');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function submit() {
    if (isNew && !key.trim()) {
      setErr('key is required');
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      if (isNew) {
        await gql(
          `mutation($k:String!,$v:String!,$t:String,$d:String){ createSetting(key:$k, valueText:$v, valueType:$t, description:$d){ id } }`,
          { k: key, v: valueText, t: valueType, d: description || null },
        );
      } else {
        await gql(
          `mutation($id:ID!,$v:String,$t:String,$d:String){ updateSetting(id:$id, valueText:$v, valueType:$t, description:$d){ id } }`,
          { id: setting.id, v: valueText, t: valueType, d: description || null },
        );
      }
      onDone(isNew ? `created ${key}` : `updated ${setting.key}`);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={isNew ? 'new setting' : `edit ${setting.key}`}
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onClose}>
            cancel
          </Button>
          <Button size="sm" loading={busy} onClick={submit}>
            save
          </Button>
        </>
      }
    >
      <div className="kat__form">
        <Field label="key" hint={isNew ? undefined : 'read-only after create'} error={err && isNew ? err : undefined}>
          <Input value={key} disabled={!isNew} onChange={(e) => setKey(e.target.value)} />
        </Field>
        <Field label="value">
          <Input value={valueText} onChange={(e) => setValueText(e.target.value)} />
        </Field>
        <Field label="type">
          <Select value={valueType} onChange={(e) => setValueType(e.target.value)} options={TYPES.map((t) => ({ label: t, value: t }))} />
        </Field>
        <Field label="description">
          <Textarea value={description} rows={2} onChange={(e) => setDescription(e.target.value)} />
        </Field>
      </div>
    </Modal>
  );
}
