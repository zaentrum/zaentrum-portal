// node --test (type stripping, Node >= 22.18): the People page's and the
// invite page's reading of what portal-api answers. Excluded from the app's
// tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  addConfirmation,
  blockedReason,
  changes,
  createBody,
  daysLeft,
  deleteConfirmation,
  draftOf,
  draftProblem,
  editConfirmation,
  emptyDraft,
  expiryText,
  inviteChip,
  inviteConfirmation,
  inviteUrl,
  isInvitePath,
  lastAdmin,
  nameProblem,
  passwordProblem,
  peopleStepText,
  ratingLabel,
  ratingOf,
  roleChip,
  switchConfirmation,
  tokenOfPath,
  usernameProblem,
  type InviteStatus,
  type PasswordPolicy,
  type Person,
} from './people.ts';

const now = Date.parse('2026-10-04T08:00:00Z');
const inDays = (d: number) => new Date(now + d * 24 * 60 * 60 * 1000).toISOString();

const person = (over: Partial<Person> = {}): Person => ({
  id: 'u-mia', username: 'mia', displayName: 'Mia', enabled: true, role: 'user', maxRating: 12,
  createdAt: '2026-10-01T08:00:00Z', invite: null, self: false, ...over,
});

test('roles and caps read as the table says them', () => {
  assert.deepEqual(roleChip({ role: 'admin' }), { label: 'Admin', tone: 'blue' });
  assert.deepEqual(roleChip({ role: 'user' }), { label: 'User', tone: 'neutral' });
  assert.equal(ratingLabel(null), 'No Cap');
  assert.equal(ratingLabel(12), 'Up to 12');
  assert.equal(ratingLabel(0), 'Up to 0');
});

test('an invite reads as what became of it', () => {
  const inv = (status: InviteStatus, expiresAt: string) => ({ status, createdAt: inDays(-1), expiresAt });
  assert.equal(inviteChip(person({ invite: inv('pending', inDays(6.5)) }), now)?.label, 'Invited');
  assert.match(inviteChip(person({ invite: inv('pending', inDays(6.5)) }), now)!.hint, /7 more days/);
  assert.match(inviteChip(person({ invite: inv('pending', inDays(0.5)) }), now)!.hint, /1 more day\./);
  // Pending in the store, past its time on this clock: expired.
  assert.equal(inviteChip(person({ invite: inv('pending', inDays(-0.1)) }), now)?.label, 'Link Expired');
  assert.equal(inviteChip(person({ invite: inv('expired', inDays(-1)) }), now)?.label, 'Link Expired');
  assert.equal(inviteChip(person({ invite: inv('used', inDays(3)) }), now)?.label, 'Joined');
  assert.equal(inviteChip(person({ invite: inv('revoked', inDays(3)) }), now)?.label, 'Link Revoked');
  assert.equal(inviteChip(person(), now), null, 'never invited from here');
  assert.equal(inviteChip(person({ managed: 'keycloak', invite: inv('used', inDays(1)) }), now), null);
  assert.equal(daysLeft('not a date', now), 0);
  assert.match(expiryText(inDays(7), now), /^Works once, for 7 days — until /);
});

test('the link to share is portal-api’s, else this origin’s', () => {
  assert.equal(inviteUrl({ url: 'https://media.example.org/portal/invite/t', path: '/portal/invite/t' }, 'https://x.example'), 'https://media.example.org/portal/invite/t');
  assert.equal(inviteUrl({ url: '', path: '/portal/invite/t' }, 'https://x.example'), 'https://x.example/portal/invite/t');
});

test('what is not offered says why', () => {
  const owner = person({ id: 'u-owner', role: 'admin', maxRating: null, managed: 'keycloak' });
  for (const a of ['edit', 'invite', 'switch', 'delete'] as const) {
    assert.match(blockedReason(owner, a) ?? '', /Keycloak's admin console/);
  }
  const me = person({ id: 'u-me', role: 'admin', maxRating: null, self: true });
  assert.equal(blockedReason(me, 'edit'), null, 'your own name and cap are yours to change');
  assert.ok(blockedReason(me, 'switch'));
  assert.ok(blockedReason(me, 'delete'));
  assert.ok(blockedReason(me, 'invite'));
  assert.equal(blockedReason(person({ enabled: false }), 'invite'), 'Switched off: switch them on first.');
  assert.equal(blockedReason(person(), 'delete'), null);
});

test('the last enabled admin is the only one', () => {
  const a = person({ id: 'a', role: 'admin', maxRating: null });
  const b = person({ id: 'b', role: 'admin', maxRating: null });
  const off = person({ id: 'c', role: 'admin', maxRating: null, enabled: false });
  assert.equal(lastAdmin([a, b], a), false);
  assert.equal(lastAdmin([a, off], a), true, 'one switched off does not count');
  assert.equal(lastAdmin([a, person()], person()), false, 'a user is no admin');
});

test('usernames, names and caps are checked as portal-api checks them', () => {
  for (const ok of ['mia', ' Mia ', 'anna-lena', 'leo.k', 'j_2']) assert.equal(usernameProblem(ok), null, ok);
  for (const bad of ['', 'ab', '-mia', 'mia.', 'mi a', 'mïa', 'a/b', 'x'.repeat(65)]) assert.ok(usernameProblem(bad), bad);
  for (const ok of ['Mia', 'Anna-Lena Müller', "O'Brien"]) assert.equal(nameProblem(ok), null, ok);
  for (const bad of ['', '  ', '<b>', 'a{b}', 'tab\tname', 'x'.repeat(101)]) assert.ok(nameProblem(bad), bad);
  assert.deepEqual(ratingOf(''), { cap: null, problem: null });
  assert.deepEqual(ratingOf(' 12 '), { cap: 12, problem: null });
  assert.deepEqual(ratingOf('0'), { cap: 0, problem: null });
  for (const bad of ['22', '-1', '12.5', 'twelve']) assert.ok(ratingOf(bad).problem, bad);
  assert.equal(draftProblem({ ...emptyDraft(), username: 'mia', displayName: 'Mia', maxRating: '12' }, true), null);
  assert.match(draftProblem({ username: 'boss', displayName: 'Boss', role: 'admin', maxRating: '16' }, true) ?? '', /cap is for a user/);
  assert.equal(draftProblem({ username: '', displayName: 'Mia', role: 'user', maxRating: '' }, false), null, 'an edit keeps the username');
});

test('a form sends what it says, an edit only what changed', () => {
  assert.deepEqual(createBody({ username: ' Mia ', displayName: ' Mia ', role: 'user', maxRating: '12' }),
    { username: 'mia', displayName: 'Mia', role: 'user', maxRating: 12 });
  assert.deepEqual(createBody({ username: 'leo', displayName: 'Leo', role: 'admin', maxRating: '' }),
    { username: 'leo', displayName: 'Leo', role: 'admin' });
  const mia = person();
  assert.deepEqual(changes(mia, draftOf(mia)), {});
  assert.deepEqual(changes(mia, { ...draftOf(mia), maxRating: '' }), { maxRating: null }, 'cleared is null: no cap');
  assert.deepEqual(changes(mia, { ...draftOf(mia), displayName: 'Mia Sophie', maxRating: '16' }), { displayName: 'Mia Sophie', maxRating: 16 });
  assert.deepEqual(changes(mia, { ...draftOf(mia), role: 'admin', maxRating: '' }), { role: 'admin', maxRating: null });
});

test('every change asks first, and says what it does', () => {
  const add = addConfirmation({ username: 'mia', displayName: 'Mia', role: 'user', maxRating: '12' }, 7);
  assert.equal(add.title, 'Add Mia?');
  assert.ok(add.lines.some((l) => l.includes('rated up to 12')));
  assert.ok(add.lines.some((l) => l.includes('7 days')));
  assert.equal(add.danger, undefined);
  const mia = person({ invite: { status: 'pending', createdAt: inDays(-1), expiresAt: inDays(6) } });
  assert.equal(editConfirmation(mia, {}), null, 'nothing changed, nothing to ask');
  assert.ok(editConfirmation(mia, { maxRating: null })!.lines.some((l) => l.includes('no rating cap')));
  const off = switchConfirmation(mia, false);
  assert.equal(off.danger, true);
  assert.ok(off.lines.some((l) => l.includes('open invite link stops working')));
  assert.equal(switchConfirmation(mia, true).danger, undefined);
  const del = deleteConfirmation(mia, true);
  assert.equal(del.danger, true);
  assert.ok(del.lines.some((l) => l.includes('watch history, progress, lists and likes go')));
  assert.ok(deleteConfirmation(mia, false).lines.some((l) => l.includes('stay on the server')));
  assert.ok(inviteConfirmation(mia, 7).lines.some((l) => l.includes('the link you sent before stops working')));
});

test('the invite page checks a password as the realm’s policy does', () => {
  const policy: PasswordPolicy = { minLength: 8, notUsername: true, notEmail: true, hints: ['At least 8 characters.', 'Not your username.'] };
  assert.equal(passwordProblem('', '', policy, 'mia'), 'Choose a password.');
  assert.equal(passwordProblem('short', 'short', policy, 'mia'), 'At least 8 characters.');
  assert.equal(passwordProblem('miamiamia', 'miamiamia', policy, 'MiaMiaMia'), 'Not your username.');
  assert.equal(passwordProblem('sternschnuppe', 'sternschnupe', policy, 'mia'), 'The two passwords differ.');
  assert.equal(passwordProblem('sternschnuppe', 'sternschnuppe', policy, 'mia'), null);
  const strict: PasswordPolicy = { minLength: 10, digits: 2, upperCase: 1, specialChars: 1, hints: [] };
  assert.equal(passwordProblem('Sternschnuppe', 'Sternschnuppe', strict, 'mia'), 'At least 2 digits.');
  assert.equal(passwordProblem('sternschnuppe42!', 'sternschnuppe42!', strict, 'mia'), 'At least 1 capital letter.');
  assert.equal(passwordProblem('Sternschnuppe42', 'Sternschnuppe42', strict, 'mia'), 'At least 1 symbol.');
  assert.equal(passwordProblem('Sternschnuppe42!', 'Sternschnuppe42!', strict, 'mia'), null);
});

test('the invite page is found by its path, which needs no sign-in', () => {
  const token = 'A'.repeat(43);
  assert.equal(tokenOfPath(`/invite/${token}`), token);
  assert.equal(tokenOfPath(`/invite/${token}/`), token);
  for (const bad of ['/invite/', '/invite/short', `/invite/${token}x`, `/people/${token}`, '/invite/' + 'A'.repeat(42) + '!']) {
    assert.equal(tokenOfPath(bad), null, bad);
  }
  assert.equal(isInvitePath(`/invite/${token}`), true);
  assert.equal(isInvitePath('/invite/garbage'), true, 'a broken link still shows the invite page, which says so');
  assert.equal(isInvitePath('/'), false);
  assert.equal(isInvitePath('/inviter'), false);
});

test('the setup step says where people get their accounts', () => {
  assert.match(peopleStepText({ state: 'info', mode: 'bundled' }), /People page/);
  assert.match(peopleStepText({ state: 'info', mode: 'external' }), /identity provider/);
  assert.equal(peopleStepText({ state: 'info', mode: 'unavailable', note: 'It needs Secret x.' }), 'It needs Secret x.');
  assert.match(peopleStepText({ state: 'info' }), /Keycloak's admin console/, 'an older portal-api names no mode');
});
