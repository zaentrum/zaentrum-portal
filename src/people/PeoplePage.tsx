import { useCallback, useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { Badge, Button, Card, Field, Heading, IconButton, Input, Modal, Select, Spinner, Table, Text, Tooltip } from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { Copy, ExternalLink, Info, Link2, Pencil, Plus, Power, PowerOff, Trash2, UserRound } from 'lucide-react';
import { usePortalApi } from '../lib/api';
import type { Confirmation } from '../lib/confirm';
import {
  MAX_RATING,
  RATING_PRESETS,
  addConfirmation,
  blockedReason,
  changes,
  createBody,
  deleteConfirmation,
  draftOf,
  draftProblem,
  editConfirmation,
  emptyDraft,
  expiryText,
  inviteChip,
  inviteConfirmation,
  inviteUrl,
  lastAdmin,
  ratingLabel,
  roleChip,
  switchConfirmation,
  type Action,
  type Draft,
  type InviteLink,
  type PeopleDoc,
  type Person,
} from '../lib/people';
import { ConfirmDialog } from '../components/ConfirmDialog';
import './people.css';

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

// The form open: a person to add (person null) or one to change.
type FormState = { draft: Draft; person: Person | null };
// A change waiting for its confirmation.
type Pending = { confirmation: Confirmation; run: () => Promise<unknown> };
// An invite link just made, shown once.
type Shown = { name: string; link: InviteLink };

// PeoplePage is where an admin manages the people who use the server: one
// account each, a role, a child's rating cap, and the invite link each one
// chooses their own password with. Every change asks first, in the operator
// console's way, and portal-api keeps the rules either way. With an external
// identity provider people live there, and the page says so.
export function PeoplePage() {
  const api = usePortalApi();
  const [doc, setDoc] = useState<PeopleDoc | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const [form, setForm] = useState<FormState | null>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [shown, setShown] = useState<Shown | null>(null);
  const [now, setNow] = useState(() => Date.now());

  const load = useCallback(async () => {
    try {
      setDoc(await api<PeopleDoc>('/people'));
      setErr(null);
    } catch (e) {
      setErr(errText(e));
    } finally {
      setNow(Date.now());
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  const people = doc?.people ?? [];
  const inviteDays = doc?.inviteDays || 7;

  // ─── the changes, each asked first ─────────────────────────────────────────

  const ask = (confirmation: Confirmation, run: () => Promise<unknown>) => setPending({ confirmation, run });

  function submitForm(f: FormState) {
    const { draft, person } = f;
    if (!person) {
      ask(addConfirmation(draft, inviteDays), async () => {
        const res = await api<{ person: Person; invite: InviteLink | null; note?: string }>('/people', {
          method: 'POST',
          body: JSON.stringify(createBody(draft)),
        });
        setForm(null);
        if (res.invite) setShown({ name: res.person.displayName, link: res.invite });
        setMsg(res.note ?? `Added ${res.person.displayName}.`);
        await load();
      });
      return;
    }
    const patch = changes(person, draft);
    const c = editConfirmation(person, patch);
    if (!c) {
      setForm(null);
      return;
    }
    ask(c, async () => {
      await api(`/people/${encodeURIComponent(person.id)}`, { method: 'PATCH', body: JSON.stringify(patch) });
      setForm(null);
      setMsg(`Changed ${person.displayName}.`);
      await load();
    });
  }

  function switchPerson(p: Person) {
    const enabled = !p.enabled;
    const c = switchConfirmation(p, enabled);
    if (!enabled && lastAdmin(people, p)) {
      c.blocked = `${p.displayName} is the last admin: make someone else an admin first.`;
    }
    ask(c, async () => {
      await api(`/people/${encodeURIComponent(p.id)}`, { method: 'PATCH', body: JSON.stringify({ enabled }) });
      setMsg(`Switched ${p.displayName} ${enabled ? 'on' : 'off'}.`);
      await load();
    });
  }

  function deletePerson(p: Person) {
    const c = deleteConfirmation(p, !!doc?.deletesData);
    if (lastAdmin(people, p)) c.blocked = `${p.displayName} is the last admin: make someone else an admin first.`;
    ask(c, async () => {
      await api(`/people/${encodeURIComponent(p.id)}`, { method: 'DELETE' });
      setMsg(`Deleted ${p.displayName}.`);
      await load();
    });
  }

  function invitePerson(p: Person) {
    ask(inviteConfirmation(p, inviteDays), async () => {
      const link = await api<InviteLink>(`/people/${encodeURIComponent(p.id)}/invite`, { method: 'POST' });
      setShown({ name: p.displayName, link });
      await load();
    });
  }

  // ─── the page ──────────────────────────────────────────────────────────────

  return (
    <div className="ppl">
      <div className="ppl__head">
        <Heading level={1} chevron>
          People
        </Heading>
        <span className="ppl__sub">One account per person: their role, a child&apos;s rating cap, their invite link.</span>
      </div>

      {err && (
        <span className="ppl__err" role="alert">
          {err}
        </span>
      )}
      {!doc && !err && (
        <div className="ppl__state">
          <Spinner /> <Text variant="muted">Reading the people…</Text>
        </div>
      )}

      {doc?.mode === 'external' && (
        <Card className="ppl__note">
          <Text as="p">People sign in with your identity provider: add them, change them and give them their roles there.</Text>
          {doc.manageUrl && (
            <a className="ppl__link" href={doc.manageUrl} target="_blank" rel="noreferrer">
              Open Identity Provider <ExternalLink size={12} aria-hidden />
            </a>
          )}
        </Card>
      )}
      {doc?.mode === 'unavailable' && (
        <Card className="ppl__note">
          <Text as="p">{doc.note || 'The People page is not set up.'}</Text>
        </Card>
      )}

      {doc?.mode === 'bundled' && (
        <>
          <div className="ppl__toolbar">
            <Button variant="primary" leading={<Plus size={15} />} onClick={() => setForm({ draft: emptyDraft(), person: null })}>
              Add Person
            </Button>
            {msg && <span className="ppl__ok">{msg}</span>}
          </div>
          <PeopleTable people={people} now={now} onEdit={(p) => setForm({ draft: draftOf(p), person: p })}
            onInvite={invitePerson} onSwitch={switchPerson} onDelete={deletePerson} />
          <PeopleCards people={people} now={now} onEdit={(p) => setForm({ draft: draftOf(p), person: p })}
            onInvite={invitePerson} onSwitch={switchPerson} onDelete={deletePerson} />
        </>
      )}

      {form && <PersonForm state={form} onClose={() => setForm(null)} onSubmit={submitForm} />}
      {pending && <ConfirmDialog confirmation={pending.confirmation} onConfirm={pending.run} onClose={() => setPending(null)} />}
      {shown && <InviteShown shown={shown} now={now} onClose={() => setShown(null)} />}
    </div>
  );
}

// ─── the list ────────────────────────────────────────────────────────────────

interface ListProps {
  people: Person[];
  now: number;
  onEdit: (p: Person) => void;
  onInvite: (p: Person) => void;
  onSwitch: (p: Person) => void;
  onDelete: (p: Person) => void;
}

// Hint is an ⓘ with the text behind it.
function Hint({ text }: { text: string }) {
  return (
    <Tooltip content={text}>
      <span className="ppl__hint" tabIndex={0} aria-label={text}>
        <Info size={13} aria-hidden />
      </span>
    </Tooltip>
  );
}

function NameCell({ p }: { p: Person }) {
  return (
    <span className="ppl__name">
      <span className="ppl__who">
        <b>{p.displayName}</b>
        {p.self && <Badge tone="blue">You</Badge>}
      </span>
      {p.email && <span className="ppl__email">{p.email}</span>}
    </span>
  );
}

function RoleCell({ p }: { p: Person }) {
  const chip = roleChip(p);
  return (
    <span className="ppl__role">
      <Badge tone={chip.tone}>{chip.label}</Badge>
      {p.managed === 'keycloak' && (
        <>
          <Badge tone="neutral">Keycloak</Badge>
          <Hint text="Administers Keycloak itself: changed in Keycloak's admin console." />
        </>
      )}
    </span>
  );
}

function InviteCell({ p, now }: { p: Person; now: number }) {
  const chip = inviteChip(p, now);
  if (!chip) return <Text variant="dim">—</Text>;
  return (
    <span className="ppl__invite">
      <Badge tone={chip.tone} dot={chip.label === 'Joined'}>
        {chip.label}
      </Badge>
      <Hint text={chip.hint} />
    </span>
  );
}

function StatusCell({ p }: { p: Person }) {
  return p.enabled ? (
    <Badge tone="green" dot>
      Enabled
    </Badge>
  ) : (
    <Badge tone="neutral">Disabled</Badge>
  );
}

// Actions are the row's buttons; one the rules do not offer is shown off,
// and says why.
function Actions({ p, onEdit, onInvite, onSwitch, onDelete }: { p: Person } & Omit<ListProps, 'people' | 'now'>) {
  const btn = (action: Action, label: string, icon: ReactNode, onClick: () => void, danger = false) => {
    const why = blockedReason(p, action);
    return (
      <IconButton
        key={action}
        label={why ? `${label}: ${why}` : `${label} ${p.displayName}`}
        title={why ?? label}
        size="sm"
        variant={danger ? 'danger' : 'ghost'}
        disabled={!!why}
        onClick={onClick}
      >
        {icon}
      </IconButton>
    );
  };
  return (
    <span className="ppl__actions">
      {btn('edit', 'Edit', <Pencil size={14} />, () => onEdit(p))}
      {btn('invite', 'New Invite Link', <Link2 size={14} />, () => onInvite(p))}
      {btn('switch', p.enabled ? 'Disable' : 'Enable', p.enabled ? <PowerOff size={14} /> : <Power size={14} />, () => onSwitch(p))}
      {btn('delete', 'Delete', <Trash2 size={14} />, () => onDelete(p))}
    </span>
  );
}

function PeopleTable(props: ListProps) {
  const { people, now } = props;
  const columns: TableColumn<Person>[] = [
    { key: 'displayName', header: 'Name', render: (p) => <NameCell p={p} /> },
    { key: 'username', header: 'Username', render: (p) => <span className="ppl__mono">{p.username}</span> },
    { key: 'role', header: 'Role', render: (p) => <RoleCell p={p} /> },
    {
      key: 'maxRating',
      header: 'Rating Cap',
      render: (p) => <span className={p.maxRating === null ? 'ppl__dim' : undefined}>{ratingLabel(p.maxRating)}</span>,
    },
    { key: 'enabled', header: 'Status', render: (p) => <StatusCell p={p} /> },
    { key: 'invite', header: 'Invite', render: (p) => <InviteCell p={p} now={now} /> },
    { key: 'id', header: '', align: 'right', render: (p) => <Actions p={p} {...props} /> },
  ];
  return (
    <div className="ppl__table">
      <Table columns={columns} rows={people} rowKey={(p) => p.id} empty={<Text variant="muted">Nobody yet: add the first person.</Text>} />
    </div>
  );
}

// PeopleCards is the list on a phone: a card per person, the same facts and
// buttons.
function PeopleCards(props: ListProps) {
  const { people, now } = props;
  return (
    <ul className="ppl__cards">
      {people.map((p) => (
        <li key={p.id} className="ppl__card">
          <div className="ppl__card-head">
            <UserRound size={16} aria-hidden />
            <NameCell p={p} />
          </div>
          <dl className="ppl__facts">
            <dt>Username</dt>
            <dd className="ppl__mono">{p.username}</dd>
            <dt>Role</dt>
            <dd>
              <RoleCell p={p} />
            </dd>
            <dt>Rating Cap</dt>
            <dd>{ratingLabel(p.maxRating)}</dd>
            <dt>Status</dt>
            <dd>
              <StatusCell p={p} />
            </dd>
            <dt>Invite</dt>
            <dd>
              <InviteCell p={p} now={now} />
            </dd>
          </dl>
          <Actions p={p} {...props} />
        </li>
      ))}
    </ul>
  );
}

// ─── the form ────────────────────────────────────────────────────────────────

function PersonForm({ state, onClose, onSubmit }: { state: FormState; onClose: () => void; onSubmit: (f: FormState) => void }) {
  const [draft, setDraft] = useState<Draft>(state.draft);
  const [tried, setTried] = useState(false);
  const p = state.person;
  const isNew = !p;
  const problem = draftProblem(draft, isNew);
  const patch = (d: Partial<Draft>) => setDraft((cur) => ({ ...cur, ...d }));
  const roleLocked = !!p && (p.self || p.managed === 'keycloak');

  function submit() {
    setTried(true);
    if (problem) return;
    onSubmit({ draft, person: p });
  }

  return (
    <Modal
      open
      width={520}
      onClose={onClose}
      title={isNew ? 'Add Person' : `Edit ${p.displayName}`}
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onClose}>
            Cancel
          </Button>
          <Button size="sm" variant="primary" onClick={submit}>
            {isNew ? 'Add' : 'Save'}
          </Button>
        </>
      }
    >
      <form
        className="ppl__form"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <Field label="Name">
          <Input value={draft.displayName} autoFocus placeholder="Mia" onChange={(e) => patch({ displayName: e.target.value })} />
        </Field>
        <Field label={<FieldLabel text="Username" hint="What they sign in with, on every app: letters a to z, digits, and . _ - between them. It cannot change later." />}>
          <Input
            value={draft.username}
            disabled={!isNew}
            spellCheck={false}
            autoCapitalize="none"
            autoComplete="off"
            placeholder="mia"
            onChange={(e) => patch({ username: e.target.value })}
          />
        </Field>
        <Field label="Role">
          <Select
            value={draft.role}
            disabled={roleLocked}
            onChange={(e) => {
              const role = e.target.value as Draft['role'];
              patch(role === 'admin' ? { role, maxRating: '' } : { role });
            }}
            options={[
              { label: 'User — watches', value: 'user' },
              { label: 'Admin — manages the server and its people', value: 'admin' },
            ]}
          />
        </Field>
        <Field
          label={<FieldLabel text="Rating Cap" hint={`An age from 0 to ${MAX_RATING}: titles rated above it are hidden from them on every app. Empty: no cap. An admin has none.`} />}
        >
          <Input
            type="number"
            inputMode="numeric"
            min={0}
            max={MAX_RATING}
            list="ppl-ratings"
            placeholder="No Cap"
            disabled={draft.role === 'admin'}
            value={draft.maxRating}
            onChange={(e) => patch({ maxRating: e.target.value })}
          />
        </Field>
        <datalist id="ppl-ratings">
          {RATING_PRESETS.map((n) => (
            <option key={n} value={n} />
          ))}
        </datalist>
        {tried && problem && (
          <span className="ppl__err" role="alert">
            {problem}
          </span>
        )}
        {/* Enter submits, as the buttons above do. */}
        <button type="submit" hidden />
      </form>
    </Modal>
  );
}

function FieldLabel({ text, hint }: { text: string; hint: string }) {
  return (
    <span className="ppl__label">
      {text} <Hint text={hint} />
    </span>
  );
}

// ─── the link, shown once ────────────────────────────────────────────────────

function InviteShown({ shown, now, onClose }: { shown: Shown; now: number; onClose: () => void }) {
  const url = inviteUrl(shown.link, window.location.origin);
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
    } catch {
      // No clipboard (an http page): the link is selected, to copy by hand.
      const input = document.getElementById('ppl-link') as HTMLInputElement | null;
      input?.select();
    }
  }

  return (
    <Modal
      open
      width={560}
      onClose={onClose}
      title={`Invite Link for ${shown.name}`}
      footer={
        <Button size="sm" onClick={onClose}>
          Done
        </Button>
      }
    >
      <div className="ppl__shown">
        <Text as="p">Send {shown.name} this link: with it they choose their own password, then sign in on any app with their username.</Text>
        <div className="ppl__copy">
          <Input id="ppl-link" readOnly value={url} spellCheck={false} onFocus={(e) => e.currentTarget.select()} />
          <Button variant="primary" leading={<Copy size={14} />} onClick={() => void copy()}>
            {copied ? 'Copied' : 'Copy Invite Link'}
          </Button>
        </div>
        <Text as="p" variant="dim" className="ppl__small">
          {expiryText(shown.link.expiresAt, now)} It is shown only now; a new one replaces it.
        </Text>
      </div>
    </Modal>
  );
}
