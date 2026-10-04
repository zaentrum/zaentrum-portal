import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { Button, Card, Field, Input, Spinner, Text } from '@nalet/design-system';
import { Check, KeyRound, LogIn } from 'lucide-react';
import { useLocation } from 'react-router-dom';
import { ZaentrumLockup } from '../glyphs';
import { PORTAL_API } from '../lib/api';
import { passwordProblem, tokenOfPath, type Invite } from '../lib/people';
import './invite.css';

// What the page shows: the invite read, the password set, or a link that
// does not work — portal-api answers every such link the same way, and so
// does the page.
type View =
  | { kind: 'loading' }
  | { kind: 'open'; invite: Invite }
  | { kind: 'done'; username: string; signIn: string }
  | { kind: 'closed'; message: string };

const CLOSED = 'This invite link does not work: it was used, it expired, or a newer one replaced it. Ask whoever invited you for a new link.';
const BUSY = 'Too many tries: wait a moment, then try again.';
const DOWN = 'Accounts cannot be set up right now. Try again in a few minutes.';

async function readMessage(res: Response, fallback: string): Promise<string> {
  try {
    const body: unknown = await res.clone().json();
    const m = (body as { message?: unknown }).message;
    if (typeof m === 'string' && m) return m;
  } catch {
    const t = (await res.text()).trim();
    if (t) return t;
  }
  return fallback;
}

// InvitePage is the page an invite link opens, at /portal/invite/<token>:
// no sign-in — the person has nothing to sign in with yet. They choose their
// password, twice, under the rules the realm's policy sets; then they sign in
// on the portal, and with the same username and password on the TV and phone
// apps.
export function InvitePage() {
  const { pathname } = useLocation();
  const token = tokenOfPath(pathname);
  const [view, setView] = useState<View>({ kind: 'loading' });

  useEffect(() => {
    if (!token) {
      setView({ kind: 'closed', message: CLOSED });
      return;
    }
    let live = true;
    fetch(`${PORTAL_API}/invites/${token}`, { headers: { Accept: 'application/json' } })
      .then(async (res) => {
        if (!live) return;
        if (res.ok) setView({ kind: 'open', invite: (await res.json()) as Invite });
        else if (res.status === 429) setView({ kind: 'closed', message: BUSY });
        else if (res.status === 404) setView({ kind: 'closed', message: await readMessage(res, CLOSED) });
        else setView({ kind: 'closed', message: DOWN });
      })
      .catch(() => live && setView({ kind: 'closed', message: DOWN }));
    return () => {
      live = false;
    };
  }, [token]);

  return (
    <div className="inv">
      <div className="inv__brand">
        <ZaentrumLockup height={28} />
      </div>
      <Card className="inv__card" header={<span className="inv__title">Set Up Your Account</span>}>
        {view.kind === 'loading' && (
          <div className="inv__state">
            <Spinner /> <Text variant="muted">Reading your invite…</Text>
          </div>
        )}
        {view.kind === 'closed' && (
          <Text as="p" className="inv__text" role="alert">
            {view.message}
          </Text>
        )}
        {view.kind === 'open' && token && (
          <ChoosePassword invite={view.invite} token={token} onDone={(username, signIn) => setView({ kind: 'done', username, signIn })}
            onClosed={(message) => setView({ kind: 'closed', message })} />
        )}
        {view.kind === 'done' && (
          <div className="inv__done">
            <Text as="p" className="inv__text">
              <Check size={16} aria-hidden className="inv__ok" /> Your password is set.
            </Text>
            <Text as="p" variant="muted" className="inv__text">
              Sign in as <b className="inv__mono">{view.username}</b> — here, and on the TV and phone apps with the same password.
            </Text>
            <div className="inv__signin">
              <Button variant="primary" leading={<LogIn size={15} />} onClick={() => window.location.assign(view.signIn)}>
                Sign In
              </Button>
            </div>
          </div>
        )}
      </Card>
    </div>
  );
}

function ChoosePassword({
  invite,
  token,
  onDone,
  onClosed,
}: {
  invite: Invite;
  token: string;
  onDone: (username: string, signIn: string) => void;
  onClosed: (message: string) => void;
}) {
  const [pw, setPw] = useState('');
  const [repeat, setRepeat] = useState('');
  const [tried, setTried] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const policy = invite.passwordPolicy;
  const problem = passwordProblem(pw, repeat, policy, invite.username);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setTried(true);
    setErr(null);
    if (problem) return;
    setBusy(true);
    try {
      const res = await fetch(`${PORTAL_API}/invites/${token}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify({ password: pw }),
      });
      if (res.ok) {
        const body = (await res.json()) as { username: string; signIn: string };
        // The link is used: it leaves the address bar and the history.
        window.history.replaceState(null, '', '/portal/invite');
        onDone(body.username, body.signIn || '/portal/');
        return;
      }
      if (res.status === 404) onClosed(await readMessage(res, CLOSED));
      else if (res.status === 429) setErr(BUSY);
      else if (res.status === 400) setErr(await readMessage(res, 'Choose another password.'));
      else setErr(DOWN);
    } catch {
      setErr(DOWN);
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="inv__form" onSubmit={submit}>
      <Text as="p" className="inv__text">
        Welcome, {invite.displayName}. Choose the password you sign in with as <b className="inv__mono">{invite.username}</b>.
      </Text>
      {/* The username, for the browser's password manager. */}
      <input type="text" name="username" autoComplete="username" value={invite.username} readOnly hidden />
      <Field label="Password">
        <Input type="password" autoComplete="new-password" autoFocus value={pw} invalid={tried && !!problem && problem !== 'The two passwords differ.'}
          onChange={(e) => setPw(e.target.value)} />
      </Field>
      <Field label="Password Again">
        <Input type="password" autoComplete="new-password" value={repeat} invalid={tried && problem === 'The two passwords differ.'}
          onChange={(e) => setRepeat(e.target.value)} />
      </Field>
      <ul className="inv__rules" aria-label="Password rules">
        {policy.hints.map((h) => (
          <li key={h}>{h}</li>
        ))}
      </ul>
      {tried && problem && (
        <span className="inv__err" role="alert">
          {problem}
        </span>
      )}
      {err && (
        <span className="inv__err" role="alert">
          {err}
        </span>
      )}
      <Button type="submit" variant="primary" leading={<KeyRound size={15} />} loading={busy}>
        Set Password
      </Button>
    </form>
  );
}
