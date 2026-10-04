import type { BadgeTone } from '@nalet/design-system';
import type { Confirmation } from './confirm';

// The People page and the invite page, worded.
//
// One account per person. An admin adds people, gives a child a rating cap,
// and sends each an invite link, on which the person chooses their own
// password. portal-api does the work (GET/POST/PATCH/DELETE /api/portal/people,
// /api/portal/invites/{token}) and keeps the rules — never the last admin,
// never your own role or switch, never an administrator of Keycloak itself.
// What is here is the reading: chips, sentences, what each change asks first,
// and the checks the forms make before anything is sent. Pure, so npm test
// runs it under node.

// ─── the documents (mirror server/internal/api/people.go) ───────────────────

export type Role = 'admin' | 'user';
export type InviteStatus = 'pending' | 'expired' | 'used' | 'revoked';

export interface InviteState {
  status: InviteStatus;
  createdAt: string;
  expiresAt: string;
  usedAt?: string;
}

export interface Person {
  id: string;
  username: string;
  displayName: string;
  email?: string;
  enabled: boolean;
  role: Role;
  // maxRating: the rating cap, an age; null for none.
  maxRating: number | null;
  // createdAt: null for an account Keycloak keeps no time of.
  createdAt: string | null;
  // managed: "keycloak" for an administrator of Keycloak itself, which is
  // changed in Keycloak's admin console.
  managed?: string;
  invite: InviteState | null;
  self: boolean;
}

export type PeopleMode = 'bundled' | 'external' | 'unavailable';

export interface PeopleDoc {
  mode: PeopleMode;
  manageUrl?: string;
  note?: string;
  people: Person[];
  // deletesData: deleting a person takes chino's data of them too.
  deletesData: boolean;
  inviteDays: number;
}

// InviteLink is a new invite, shown once.
export interface InviteLink {
  url: string;
  path: string;
  token: string;
  expiresAt: string;
}

export interface PasswordPolicy {
  minLength: number;
  maxLength?: number;
  digits?: number;
  lowerCase?: number;
  upperCase?: number;
  specialChars?: number;
  notUsername?: boolean;
  notEmail?: boolean;
  hints: string[];
}

// Invite is GET /api/portal/invites/{token} while the link works.
export interface Invite {
  valid: true;
  username: string;
  displayName: string;
  expiresAt: string;
  passwordPolicy: PasswordPolicy;
}

// ─── reading ─────────────────────────────────────────────────────────────────

export const MAX_RATING = 21;
// Common caps, offered as suggestions: ages most rating systems use.
export const RATING_PRESETS = [0, 6, 12, 16, 18];

export const roleLabel = (r: Role) => (r === 'admin' ? 'Admin' : 'User');

export function roleChip(p: Pick<Person, 'role'>): { label: string; tone: BadgeTone } {
  return p.role === 'admin' ? { label: 'Admin', tone: 'blue' } : { label: 'User', tone: 'neutral' };
}

// ratingLabel is the cap as the table says it.
export function ratingLabel(maxRating: number | null): string {
  return maxRating === null ? 'No Cap' : `Up to ${maxRating}`;
}

const DAY = 24 * 60 * 60 * 1000;

// daysLeft is how many days until at, rounded up; 0 once it passed.
export function daysLeft(at: string, now: number): number {
  const t = Date.parse(at);
  if (Number.isNaN(t) || t <= now) return 0;
  return Math.ceil((t - now) / DAY);
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

// inviteChip is the invite column: what became of the person's newest invite
// at now. null when there is none to speak of — never sent from here.
export function inviteChip(p: Pick<Person, 'invite' | 'managed'>, now: number): { label: string; tone: BadgeTone; hint: string } | null {
  const i = p.invite;
  if (!i || p.managed === 'keycloak') return null;
  const status = i.status === 'pending' && daysLeft(i.expiresAt, now) === 0 ? 'expired' : i.status;
  switch (status) {
    case 'pending': {
      const left = daysLeft(i.expiresAt, now);
      return { label: 'Invited', tone: 'amber', hint: `The link works for ${plural(left, 'more day', 'more days')}.` };
    }
    case 'used':
      return { label: 'Joined', tone: 'green', hint: 'They chose their password with the link.' };
    case 'revoked':
      return { label: 'Link Revoked', tone: 'neutral', hint: 'The link stopped working: send a new one.' };
    default:
      return { label: 'Link Expired', tone: 'neutral', hint: 'The link expired before it was used: send a new one.' };
  }
}

// expiryText says until when a new link works, in days and as a date.
export function expiryText(expiresAt: string, now: number): string {
  const d = new Date(expiresAt);
  const date = Number.isNaN(d.getTime())
    ? expiresAt
    : d.toLocaleString('en-GB', { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
  const left = daysLeft(expiresAt, now);
  return `Works once, for ${plural(left, 'day', 'days')} — until ${date}.`;
}

// inviteUrl is the link to share: portal-api's, else this origin's.
export function inviteUrl(link: Pick<InviteLink, 'url' | 'path'>, origin: string): string {
  return link.url || `${origin}${link.path}`;
}

// ─── what a change may be ────────────────────────────────────────────────────

export type Action = 'edit' | 'invite' | 'switch' | 'delete';

// blockedReason says why an action is not offered for a person, or null.
export function blockedReason(p: Person, action: Action): string | null {
  if (p.managed === 'keycloak') return "Administers Keycloak itself: changed in Keycloak's admin console.";
  if (!p.self) {
    if (action === 'invite' && !p.enabled) return 'Switched off: switch them on first.';
    return null;
  }
  switch (action) {
    case 'invite':
      return 'You are signed in: change your password in your account settings.';
    case 'switch':
      return 'You cannot switch yourself off: another admin can.';
    case 'delete':
      return 'You cannot delete yourself here: another admin can, or you delete your account from the apps.';
    default:
      return null;
  }
}

// lastAdmin: p is the only enabled admin.
export function lastAdmin(people: Person[], p: Person): boolean {
  return p.role === 'admin' && p.enabled && !people.some((o) => o.id !== p.id && o.role === 'admin' && o.enabled);
}

// ─── the forms ───────────────────────────────────────────────────────────────

export interface Draft {
  username: string;
  displayName: string;
  role: Role;
  // maxRating as typed: '' for no cap.
  maxRating: string;
}

export const emptyDraft = (): Draft => ({ username: '', displayName: '', role: 'user', maxRating: '' });

export function draftOf(p: Person): Draft {
  return { username: p.username, displayName: p.displayName, role: p.role, maxRating: p.maxRating === null ? '' : String(p.maxRating) };
}

// usernameProblem is why a username is not one portal-api takes, or null:
// letters a to z, digits, and . _ - between them, 3 to 64.
export function usernameProblem(raw: string): string | null {
  const u = raw.trim().toLowerCase();
  if (!u) return 'A username is needed: what they sign in with.';
  if (u.length < 3 || u.length > 64) return 'A username has 3 to 64 characters.';
  if (!/^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$/.test(u)) return 'Letters a to z, digits, and . _ - between them.';
  return null;
}

const NAME_PROHIBITED = /[<>&"$%!#?§;*~/\\|^=[\]{}()\p{Cc}]/u;

// nameProblem is why a display name is not taken, or null.
export function nameProblem(raw: string): string | null {
  const n = raw.trim();
  if (!n) return 'A name is needed: what the apps call them.';
  if ([...n].length > 100) return 'A name has at most 100 characters.';
  if (NAME_PROHIBITED.test(n)) return 'A name may not hold < > & " $ % ! # ? § ; * ~ / \\ | ^ = [ ] { } ( ).';
  return null;
}

// ratingOf reads a cap as typed: null for none, a number, or the problem.
export function ratingOf(raw: string): { cap: number | null; problem: string | null } {
  const s = raw.trim();
  if (!s) return { cap: null, problem: null };
  if (!/^\d+$/.test(s)) return { cap: null, problem: `An age from 0 to ${MAX_RATING}, or empty for no cap.` };
  const n = Number(s);
  if (n > MAX_RATING) return { cap: null, problem: `An age from 0 to ${MAX_RATING}, or empty for no cap.` };
  return { cap: n, problem: null };
}

// draftProblem is the first thing a draft lacks, or null. A cap is for a
// user: an admin can change every cap, their own too.
export function draftProblem(d: Draft, isNew: boolean): string | null {
  if (isNew) {
    const u = usernameProblem(d.username);
    if (u) return u;
  }
  const n = nameProblem(d.displayName);
  if (n) return n;
  const r = ratingOf(d.maxRating);
  if (r.problem) return r.problem;
  if (d.role === 'admin' && r.cap !== null) return 'An admin can change every rating cap, their own too: a cap is for a user.';
  return null;
}

// createBody is POST /people's body.
export function createBody(d: Draft): { username: string; displayName: string; role: Role; maxRating?: number } {
  const cap = ratingOf(d.maxRating).cap;
  return {
    username: d.username.trim().toLowerCase(),
    displayName: d.displayName.trim(),
    role: d.role,
    ...(cap === null ? {} : { maxRating: cap }),
  };
}

export interface Patch {
  displayName?: string;
  role?: Role;
  maxRating?: number | null;
  enabled?: boolean;
}

// changes is PATCH /people/{id}'s body: what the draft changes, nothing else;
// empty when it changes nothing.
export function changes(p: Person, d: Draft): Patch {
  const out: Patch = {};
  const name = d.displayName.trim();
  if (name !== p.displayName) out.displayName = name;
  if (d.role !== p.role) out.role = d.role;
  const cap = ratingOf(d.maxRating).cap;
  if (cap !== p.maxRating) out.maxRating = cap;
  return out;
}

// ─── what each change asks first ─────────────────────────────────────────────

const capLine = (cap: number | null, name: string) =>
  cap === null
    ? `${name} sees every title: no rating cap.`
    : `${name} sees titles rated up to ${cap} — on every app, from their next sign-in or within five minutes.`;

export function addConfirmation(d: Draft, inviteDays: number): Confirmation {
  const b = createBody(d);
  const cap = b.maxRating ?? null;
  return {
    title: `Add ${b.displayName}?`,
    lines: [
      `an account ${b.username}, ${b.role === 'admin' ? 'an admin: they manage the server and its people' : 'a user'}.`,
      capLine(cap, b.displayName),
      `you get an invite link to send them: it works once, for ${plural(inviteDays, 'day', 'days')}, and they choose their own password with it.`,
    ],
    confirm: 'Add',
  };
}

export function editConfirmation(p: Person, patch: Patch): Confirmation | null {
  const lines: string[] = [];
  if (patch.displayName !== undefined) lines.push(`the apps call them ${patch.displayName}.`);
  if (patch.role === 'admin') lines.push('they become an admin: they manage the server and its people.');
  if (patch.role === 'user') lines.push('they become a user: the consoles and the People page close to them.');
  if (patch.maxRating !== undefined) lines.push(capLine(patch.maxRating, patch.displayName ?? p.displayName));
  if (!lines.length) return null;
  return { title: `Change ${p.displayName}?`, lines, confirm: 'Save' };
}

export function switchConfirmation(p: Person, enabled: boolean): Confirmation {
  if (enabled) {
    return {
      title: `Switch ${p.displayName} On?`,
      lines: ['they can sign in again, with the password they had.', 'an invite link sent before they were switched off stays revoked: send a new one if they never chose a password.'],
      confirm: 'Switch On',
    };
  }
  return {
    title: `Switch ${p.displayName} Off?`,
    lines: [
      'they cannot sign in any more; where they are signed in, the apps stop within five minutes.',
      'their account, history and lists stay: switch them on again any time.',
      ...(p.invite?.status === 'pending' ? ['their open invite link stops working.'] : []),
    ],
    confirm: 'Switch Off',
    danger: true,
  };
}

export function deleteConfirmation(p: Person, deletesData: boolean): Confirmation {
  return {
    title: `Delete ${p.displayName}?`,
    lines: [
      `the account ${p.username} goes: they cannot sign in any more.`,
      deletesData
        ? 'their watch history, progress, lists and likes go with it.'
        : 'their watch history and lists stay on the server: chino-api is not set up to remove them.',
      'this cannot be undone; switching them off can.',
    ],
    confirm: 'Delete',
    danger: true,
  };
}

export function inviteConfirmation(p: Person, inviteDays: number): Confirmation {
  const open = p.invite?.status === 'pending';
  return {
    title: `New Invite Link for ${p.displayName}?`,
    lines: [
      `a new link that works once, for ${plural(inviteDays, 'day', 'days')}: with it they choose a new password.`,
      open ? 'the link you sent before stops working.' : 'any link sent before stays revoked.',
      'their current password works until they use the new link.',
    ],
    confirm: 'Make Link',
  };
}

// ─── the invite page ─────────────────────────────────────────────────────────

// passwordProblem is why the password typed is not one to send, or null:
// the policy portal-api reads for the realm, and the two entries alike.
export function passwordProblem(pw: string, repeat: string, policy: PasswordPolicy, username: string): string | null {
  const chars = [...pw];
  if (!pw) return 'Choose a password.';
  if (chars.length < policy.minLength) return `At least ${policy.minLength} characters.`;
  if (policy.maxLength && chars.length > policy.maxLength) return `At most ${policy.maxLength} characters.`;
  const count = (re: RegExp) => chars.filter((c) => re.test(c)).length;
  if (policy.digits && count(/\p{Nd}/u) < policy.digits) return `At least ${plural(policy.digits, 'digit', 'digits')}.`;
  if (policy.upperCase && count(/\p{Lu}/u) < policy.upperCase) return `At least ${plural(policy.upperCase, 'capital letter', 'capital letters')}.`;
  if (policy.lowerCase && count(/\p{Ll}/u) < policy.lowerCase) return `At least ${plural(policy.lowerCase, 'small letter', 'small letters')}.`;
  if (policy.specialChars && count(/[^\p{L}\p{Nd}\s]/u) < policy.specialChars) {
    return `At least ${plural(policy.specialChars, 'symbol', 'symbols')}.`;
  }
  if (policy.notUsername && pw.toLowerCase() === username.toLowerCase()) return 'Not your username.';
  if (pw !== repeat) return 'The two passwords differ.';
  return null;
}

// tokenOfPath is the invite token of a path under the router's base
// (/invite/<token>), or null.
export function tokenOfPath(pathname: string): string | null {
  const m = /^\/invite\/([A-Za-z0-9_-]{43})\/?$/.exec(pathname);
  return m ? m[1] : null;
}

// isInvitePath: the path is the invite page's, which needs no sign-in.
export function isInvitePath(pathname: string): boolean {
  return pathname === '/invite' || pathname.startsWith('/invite/');
}

// ─── the setup checklist's step ──────────────────────────────────────────────

export interface PeopleStep {
  state: 'info';
  mode?: PeopleMode;
  manageUrl?: string;
  note?: string;
}

// peopleStepText is the setup step's sentence.
export function peopleStepText(s: PeopleStep): string {
  switch (s.mode) {
    case 'bundled':
      return 'Everyone who uses the server gets an account of their own on the People page, and a child a rating cap. They choose their password with an invite link.';
    case 'external':
      return 'People sign in with your identity provider: add them and give them roles there.';
    case 'unavailable':
      return s.note || 'The People page is not set up.';
    default:
      return "Accounts for the people who use this server are made in Keycloak's admin console, which is not on the public host.";
  }
}
