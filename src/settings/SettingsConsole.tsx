import { useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import {
  Tabs,
  Table,
  Button,
  Badge,
  Modal,
  Field,
  Input,
  Select,
  Switch,
  Spinner,
  Text,
  Heading,
} from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { Plus, Pencil, Trash2 } from 'lucide-react';
import { usePortalApi, type App, type Space, type Tile, type Extension } from '../lib/api';
import { ICON_CHOICES } from '../lib/icons';
import { useResource } from './useResource';
import './settings.css';

const KINDS = ['product', 'manage', 'tool', 'external'];
const BADGE_TONES = ['', 'info', 'success', 'warning', 'danger', 'neutral'];
const STATUSES = ['', 'online', 'offline', 'degraded', 'unknown'];
const EXT_KINDS = ['link', 'action'];

function opts(values: string[], noneLabel?: string) {
  return values.map((v) => ({ label: v === '' ? (noneLabel ?? '(none)') : v, value: v }));
}

// SettingsConsole — the registry admin (the one "unconfigured" tile). The portal
// shell configuring itself: register apps, spaces and the tiles ("semantic
// objects") that place them on the launchpad. Admin-only (gated by App).
export function SettingsConsole() {
  const [tab, setTab] = useState('apps');
  return (
    <div className="set">
      <div className="set__head">
        <Heading level={1} chevron>
          settings
        </Heading>
        <span className="set__sub">app registry — register apps, spaces &amp; tiles</span>
      </div>
      <Tabs
        items={[
          { value: 'apps', label: 'apps' },
          { value: 'spaces', label: 'spaces' },
          { value: 'tiles', label: 'tiles' },
          { value: 'extensions', label: 'extensions' },
        ]}
        value={tab}
        onChange={setTab}
      />
      <div className="set__body">
        {tab === 'apps' && <AppsPanel />}
        {tab === 'spaces' && <SpacesPanel />}
        {tab === 'tiles' && <TilesPanel />}
        {tab === 'extensions' && <ExtensionsPanel />}
      </div>
    </div>
  );
}

// ─── generic CRUD panel ──────────────────────────────────────────────────────

function CrudPanel<T extends { key: string }>({
  singular,
  path,
  columns,
  empty,
  keyHint,
  renderForm,
}: {
  singular: string;
  path: string;
  columns: TableColumn<T>[];
  empty: () => T;
  keyHint?: string;
  renderForm: (draft: T, patch: (p: Partial<T>) => void) => ReactNode;
}) {
  const { items, loading, error, save, remove } = useResource<T>(path);
  const [draft, setDraft] = useState<T | null>(null);
  const [isNew, setIsNew] = useState(false);
  const [busy, setBusy] = useState(false);
  const [formErr, setFormErr] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);

  const patch = (p: Partial<T>) => setDraft((d) => (d ? { ...d, ...p } : d));

  function openNew() {
    setDraft(empty());
    setIsNew(true);
    setFormErr(null);
  }
  function openEdit(row: T) {
    setDraft({ ...row });
    setIsNew(false);
    setFormErr(null);
  }

  async function submit() {
    if (!draft) return;
    if (!draft.key.trim()) {
      setFormErr('key is required');
      return;
    }
    setBusy(true);
    setFormErr(null);
    try {
      await save(draft, isNew);
      setMsg(`${isNew ? 'created' : 'updated'} ${draft.key}`);
      setDraft(null);
    } catch (e) {
      setFormErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function del(row: T) {
    if (!confirm(`delete ${singular} "${row.key}"?`)) return;
    setMsg(null);
    try {
      await remove(row.key);
      setMsg(`deleted ${row.key}`);
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    }
  }

  // The actions column has no backing field; `key` is only used as a React key
  // here (a render is provided), so cast past the keyof-Row constraint.
  const actionsCol = {
    key: '__act',
    header: '',
    align: 'right',
    render: (r: T) => (
      <span style={{ display: 'inline-flex', gap: 4 }}>
        <Button variant="ghost" size="sm" leading={<Pencil size={13} />} onClick={() => openEdit(r)}>
          edit
        </Button>
        <Button variant="ghost" size="sm" leading={<Trash2 size={13} />} onClick={() => del(r)}>
          del
        </Button>
      </span>
    ),
  } as unknown as TableColumn<T>;
  const cols: TableColumn<T>[] = [...columns, actionsCol];

  return (
    <div>
      <div className="set__toolbar">
        <Button leading={<Plus size={15} />} onClick={openNew}>
          new {singular}
        </Button>
        {msg && <span className="set__ok">{msg}</span>}
      </div>
      {error && <span className="set__err">error: {error}</span>}
      {loading && !items.length ? (
        <div className="set__state">
          <Spinner /> <Text variant="muted">loading…</Text>
        </div>
      ) : (
        <Table
          columns={cols}
          rows={items}
          rowKey={(r) => r.key}
          dense
          empty={<Text variant="muted">nothing here yet.</Text>}
        />
      )}

      {draft && (
        <Modal
          open
          onClose={() => setDraft(null)}
          title={isNew ? `new ${singular}` : `edit ${draft.key}`}
          footer={
            <>
              <Button variant="ghost" size="sm" onClick={() => setDraft(null)}>
                cancel
              </Button>
              <Button size="sm" loading={busy} onClick={submit}>
                save
              </Button>
            </>
          }
        >
          <div className="set__form">
            <Field label="key" hint={isNew ? keyHint : 'identifier — read-only after create'}>
              <Input
                value={draft.key}
                disabled={!isNew}
                onChange={(e) => patch({ key: e.target.value } as Partial<T>)}
              />
            </Field>
            {renderForm(draft, patch)}
            {formErr && <span className="set__err">{formErr}</span>}
          </div>
        </Modal>
      )}
    </div>
  );
}

// ─── apps ────────────────────────────────────────────────────────────────────

function AppsPanel() {
  return (
    <CrudPanel<App>
      singular="app"
      path="/apps"
      keyHint="e.g. katalog, chino, my-tool"
      empty={() => ({ key: '', title: '', description: '', baseUrl: '', kind: 'tool', healthUrl: '', icon: '', enabled: true })}
      columns={[
        { key: 'title', header: 'title', render: (r) => <b>{r.title}</b> },
        { key: 'key', header: 'key', render: (r) => <span className="set__mono">{r.key}</span> },
        { key: 'kind', header: 'kind', render: (r) => <Badge tone="neutral">{r.kind}</Badge> },
        { key: 'baseUrl', header: 'base', render: (r) => <span className="set__mono">{r.baseUrl || '—'}</span> },
        {
          key: 'enabled',
          header: 'enabled',
          render: (r) => (r.enabled ? <Badge tone="green" dot>on</Badge> : <Badge tone="neutral">off</Badge>),
        },
      ]}
      renderForm={(d, patch) => (
        <>
          <Field label="title">
            <Input value={d.title} onChange={(e) => patch({ title: e.target.value })} />
          </Field>
          <Field label="description">
            <Input value={d.description} onChange={(e) => patch({ description: e.target.value })} />
          </Field>
          <Field label="kind">
            <Select value={d.kind} onChange={(e) => patch({ kind: e.target.value })} options={opts(KINDS)} />
          </Field>
          <Field label="base url" hint="e.g. /katalog/ or https://tool.example/">
            <Input value={d.baseUrl} onChange={(e) => patch({ baseUrl: e.target.value })} />
          </Field>
          <Field label="icon">
            <Select value={d.icon} onChange={(e) => patch({ icon: e.target.value })} options={opts(['', ...ICON_CHOICES])} />
          </Field>
          <Field label="health url" hint="optional">
            <Input value={d.healthUrl} onChange={(e) => patch({ healthUrl: e.target.value })} />
          </Field>
          <Field label="enabled">
            <Switch checked={d.enabled} onChange={(e) => patch({ enabled: e.target.checked })} />
          </Field>
        </>
      )}
    />
  );
}

// ─── extensions ──────────────────────────────────────────────────────────────

// ExtensionsPanel — the addon UI seam. Rows are usually written by an addon's
// service account on install (e.g. laedeli/acquire adds a "request" button to
// chino's search-empty slot); admins can view/toggle/remove them here.
function ExtensionsPanel() {
  return (
    <CrudPanel<Extension>
      singular="extension"
      path="/extensions"
      keyHint="e.g. acquire.search-request"
      empty={() => ({
        key: '', addon: '', slot: 'search.empty', kind: 'link', label: '', icon: '',
        url: '', method: 'POST', statusUrl: '', ord: 0, enabled: true,
      })}
      columns={[
        { key: 'label', header: 'label', render: (r) => <b>{r.label || '—'}</b> },
        { key: 'slot', header: 'slot', render: (r) => <span className="set__mono">{r.slot}</span> },
        { key: 'kind', header: 'kind', render: (r) => <Badge tone="neutral">{r.kind}</Badge> },
        { key: 'addon', header: 'addon', render: (r) => <span className="set__mono">{r.addon || '—'}</span> },
        {
          key: 'enabled',
          header: 'enabled',
          render: (r) => (r.enabled ? <Badge tone="green" dot>on</Badge> : <Badge tone="neutral">off</Badge>),
        },
      ]}
      renderForm={(d, patch) => (
        <>
          <Field label="label">
            <Input value={d.label} onChange={(e) => patch({ label: e.target.value })} />
          </Field>
          <Field label="slot" hint="e.g. search.empty, item.detail.actions">
            <Input value={d.slot} onChange={(e) => patch({ slot: e.target.value })} />
          </Field>
          <Field label="kind">
            <Select value={d.kind} onChange={(e) => patch({ kind: e.target.value })} options={opts(EXT_KINDS)} />
          </Field>
          <Field label="url" hint="{q} is replaced with the current query">
            <Input value={d.url} onChange={(e) => patch({ url: e.target.value })} />
          </Field>
          <Field label="method" hint="for kind=action">
            <Input value={d.method} onChange={(e) => patch({ method: e.target.value })} />
          </Field>
          <Field label="status url" hint="optional live-status feed">
            <Input value={d.statusUrl} onChange={(e) => patch({ statusUrl: e.target.value })} />
          </Field>
          <Field label="icon">
            <Select value={d.icon} onChange={(e) => patch({ icon: e.target.value })} options={opts(['', ...ICON_CHOICES])} />
          </Field>
          <Field label="addon" hint="owning addon id (for bulk uninstall)">
            <Input value={d.addon} onChange={(e) => patch({ addon: e.target.value })} />
          </Field>
          <Field label="order" hint="lower shows first">
            <Input
              type="number"
              value={String(d.ord)}
              onChange={(e) => patch({ ord: Number(e.target.value) || 0 })}
            />
          </Field>
          <Field label="enabled">
            <Switch checked={d.enabled} onChange={(e) => patch({ enabled: e.target.checked })} />
          </Field>
        </>
      )}
    />
  );
}

// ─── spaces ──────────────────────────────────────────────────────────────────

function SpacesPanel() {
  return (
    <CrudPanel<Space>
      singular="space"
      path="/spaces"
      keyHint="e.g. apps, manage, ops"
      empty={() => ({ key: '', title: '', order: 0 })}
      columns={[
        { key: 'title', header: 'title', render: (r) => <b>{r.title}</b> },
        { key: 'key', header: 'key', render: (r) => <span className="set__mono">{r.key}</span> },
        { key: 'order', header: 'order', align: 'right', render: (r) => r.order },
      ]}
      renderForm={(d, patch) => (
        <>
          <Field label="title">
            <Input value={d.title} onChange={(e) => patch({ title: e.target.value })} />
          </Field>
          <Field label="order" hint="lower shows first">
            <Input
              type="number"
              value={String(d.order)}
              onChange={(e) => patch({ order: parseInt(e.target.value, 10) || 0 })}
            />
          </Field>
        </>
      )}
    />
  );
}

// ─── tiles ───────────────────────────────────────────────────────────────────

function TilesPanel() {
  const api = usePortalApi();
  const [apps, setApps] = useState<App[]>([]);
  const [spaces, setSpaces] = useState<Space[]>([]);
  useEffect(() => {
    let live = true;
    api<App[]>('/apps').then((a) => live && setApps(a)).catch(() => {});
    api<Space[]>('/spaces').then((s) => live && setSpaces(s)).catch(() => {});
    return () => {
      live = false;
    };
  }, [api]);

  return (
    <CrudPanel<Tile>
      singular="tile"
      path="/tiles"
      keyHint="e.g. chino.open (app.action)"
      empty={() => ({
        key: '',
        appKey: apps[0]?.key ?? '',
        spaceKey: spaces[0]?.key ?? '',
        title: '',
        description: '',
        icon: '',
        target: '',
        order: 0,
        badge: '',
        badgeTone: '',
        status: '',
        external: false,
        open: 'inline',
        enabled: true,
      })}
      columns={[
        { key: 'title', header: 'title', render: (r) => <b>{r.title}</b> },
        { key: 'key', header: 'key', render: (r) => <span className="set__mono">{r.key}</span> },
        { key: 'appKey', header: 'app', render: (r) => <Badge tone="blue">{r.appKey}</Badge> },
        { key: 'spaceKey', header: 'space', render: (r) => <Badge tone="neutral">{r.spaceKey}</Badge> },
        { key: 'target', header: 'target', render: (r) => <span className="set__mono">{r.target || '—'}</span> },
        {
          key: 'open',
          header: 'open',
          render: (r) =>
            (r.open || (r.external ? 'newtab' : 'inline')) === 'newtab' ? (
              <Badge tone="blue">new tab</Badge>
            ) : (
              <Badge tone="neutral">inline</Badge>
            ),
        },
        {
          key: 'enabled',
          header: 'enabled',
          render: (r) => (r.enabled ? <Badge tone="green" dot>on</Badge> : <Badge tone="neutral">off</Badge>),
        },
      ]}
      renderForm={(d, patch) => (
        <>
          <Field label="title">
            <Input value={d.title} onChange={(e) => patch({ title: e.target.value })} />
          </Field>
          <Field label="app">
            <Select value={d.appKey} onChange={(e) => patch({ appKey: e.target.value })} options={opts(apps.map((a) => a.key))} />
          </Field>
          <Field label="space">
            <Select value={d.spaceKey} onChange={(e) => patch({ spaceKey: e.target.value })} options={opts(spaces.map((s) => s.key))} />
          </Field>
          <Field label="target" hint="path within the app (e.g. scan), or an absolute url">
            <Input value={d.target} onChange={(e) => patch({ target: e.target.value })} />
          </Field>
          <Field label="description">
            <Input value={d.description} onChange={(e) => patch({ description: e.target.value })} />
          </Field>
          <Field label="icon">
            <Select value={d.icon} onChange={(e) => patch({ icon: e.target.value })} options={opts(['', ...ICON_CHOICES])} />
          </Field>
          <Field label="badge" hint="optional label, e.g. ready / soon">
            <Input value={d.badge} onChange={(e) => patch({ badge: e.target.value })} />
          </Field>
          <Field label="badge tone">
            <Select value={d.badgeTone} onChange={(e) => patch({ badgeTone: e.target.value })} options={opts(BADGE_TONES)} />
          </Field>
          <Field label="status">
            <Select value={d.status} onChange={(e) => patch({ status: e.target.value })} options={opts(STATUSES)} />
          </Field>
          <Field label="order" hint="lower shows first">
            <Input type="number" value={String(d.order)} onChange={(e) => patch({ order: parseInt(e.target.value, 10) || 0 })} />
          </Field>
          <Field label="open" hint="inline replaces the portal; new tab keeps it open">
            <Select
              value={d.open || (d.external ? 'newtab' : 'inline')}
              onChange={(e) => patch({ open: e.target.value })}
              options={[
                { label: 'inline (same tab)', value: 'inline' },
                { label: 'new tab', value: 'newtab' },
              ]}
            />
          </Field>
          <Field label="external" hint="external tool (shows the ↗ glyph; target used verbatim)">
            <Switch checked={d.external} onChange={(e) => patch({ external: e.target.checked })} />
          </Field>
          <Field label="enabled">
            <Switch checked={d.enabled} onChange={(e) => patch({ enabled: e.target.checked })} />
          </Field>
        </>
      )}
    />
  );
}
