// node --test (type stripping, Node >= 22.18): the shell's bell reading what
// portal-api answers for the signed-in person's notices, and the list after
// each change. Excluded from the app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  ageText,
  badgeText,
  bellLabel,
  fromText,
  markAllRead,
  markRead,
  noticeHref,
  noticeListOf,
  removeNotice,
  shellRoute,
  type Notice,
  type NoticeList,
} from './notices.ts';

const page = 'https://media.example.org/portal/';
const now = Date.parse('2026-10-05T08:00:00Z');

const notice = (over: Partial<Notice> = {}): Notice => ({
  id: '00000001-0000-4000-8000-000000000001',
  addon: 'example',
  addonTitle: 'Example',
  addonIcon: 'puzzle',
  title: 'Your title is ready',
  body: 'It is in your library now.',
  link: '',
  itemId: '',
  createdAt: '2026-10-05T07:58:00Z',
  readAt: null,
  ...over,
});

// What portal-api answers is read as it came: a notice without an id or a
// title is none, a missing field is empty, and the unread count holds the
// unread notices listed.
test('a list is read defensively', () => {
  const got = noticeListOf({
    notices: [
      notice(),
      { id: 'x' },
      { title: 'no id' },
      null,
      'a string',
      { id: 'y', title: 'bare', readAt: '2026-10-05T07:59:00Z', link: 7 },
    ],
    unread: 1,
  });
  assert.equal(got.notices.length, 2);
  assert.equal(got.notices[1].link, '');
  assert.equal(got.notices[1].readAt, '2026-10-05T07:59:00Z');
  assert.equal(got.notices[1].addonTitle, '');
  assert.equal(got.unread, 1);
  assert.deepEqual(noticeListOf(null), { notices: [], unread: 0 });
  assert.deepEqual(noticeListOf({ notices: 'no' }), { notices: [], unread: 0 });
  // An unread count below what is listed unread is not believed.
  assert.equal(noticeListOf({ notices: [notice(), notice({ id: 'b' })], unread: 0 }).unread, 2);
  assert.equal(noticeListOf({ notices: [notice()], unread: -3 }).unread, 1);
  assert.equal(noticeListOf({ notices: [], unread: 7 }).unread, 7);
});

test('the bell says how many are unread', () => {
  assert.equal(badgeText(0), '');
  assert.equal(badgeText(-1), '');
  assert.equal(badgeText(Number.NaN), '');
  assert.equal(badgeText(1), '1');
  assert.equal(badgeText(99), '99');
  assert.equal(badgeText(100), '99+');
  assert.equal(bellLabel(0), 'Notices');
  assert.equal(bellLabel(3), 'Notices, 3 unread');
});

test('a notice says whom it is from and when', () => {
  assert.equal(fromText(notice()), 'Example');
  assert.equal(fromText(notice({ addonTitle: '  ' })), 'example');
  assert.equal(fromText(notice({ addonTitle: '', addon: '' })), 'An addon');
  assert.equal(ageText('2026-10-05T07:59:30Z', now), 'now');
  assert.equal(ageText('2026-10-05T07:55:00Z', now), '5m');
  assert.equal(ageText('2026-10-05T05:00:00Z', now), '3h');
  assert.equal(ageText('2026-10-03T08:00:00Z', now), '2d');
  // ICU spells September's short form Sep or Sept, by version.
  assert.match(ageText('2026-09-12T08:00:00Z', now), /^12 Sept?$/);
  assert.equal(ageText('not a time', now), '');
});

// A link is followed only to an http(s) page of this origin: what the
// server's rule takes, and nothing it refuses — checked again here, since
// the bell is where it is clicked.
test('a link leads only to this instance', () => {
  for (const [link, want] of [
    ['/portal/app/example', 'https://media.example.org/portal/app/example'],
    ['/portal/app/example?q=x#/ready', 'https://media.example.org/portal/app/example?q=x#/ready'],
    ['https://media.example.org/portal/app/example', 'https://media.example.org/portal/app/example'],
    ['HTTPS://MEDIA.EXAMPLE.ORG/x', 'https://media.example.org/x'],
    ['  /x  ', 'https://media.example.org/x'],
  ] as const) {
    assert.equal(noticeHref(link, page), want, link);
  }
  for (const link of [
    '',
    'javascript:alert(document.cookie)',
    'JavaScript:alert(1)',
    'data:text/html,<script>alert(1)</script>',
    'https://elsewhere.example/x',
    'http://media.example.org/x', // another scheme is another origin
    'https://media.example.org.elsewhere.example/x',
    '//elsewhere.example/x',
    '//media.example.org/x',
    '/\\elsewhere.example/x',
    'https://user:pass@media.example.org/x',
    'portal/app/example',
    '?q=x',
    '#/x',
    'mailto:someone@example.org',
  ]) {
    assert.equal(noticeHref(link, page), null, link);
  }
});

test('a page of the shell opens as a route, anything else as a page', () => {
  assert.equal(shellRoute('https://media.example.org/portal/app/example?q=1#/x', page), '/app/example?q=1#/x');
  assert.equal(shellRoute('https://media.example.org/portal', page), '/');
  assert.equal(shellRoute('https://media.example.org/portal/', page), '/');
  assert.equal(shellRoute('https://media.example.org/chino/item/1', page), null);
  assert.equal(shellRoute('https://media.example.org/portalx/app', page), null);
  assert.equal(shellRoute('https://elsewhere.example/portal/app/x', page), null);
});

test('reading, reading all and deleting keep the count', () => {
  const list: NoticeList = {
    notices: [notice({ id: 'a' }), notice({ id: 'b' }), notice({ id: 'c', readAt: '2026-10-05T07:00:00Z' })],
    unread: 2,
  };
  const read = markRead(list, 'a', '2026-10-05T08:00:00Z');
  assert.equal(read.unread, 1);
  assert.equal(read.notices[0].readAt, '2026-10-05T08:00:00Z');
  assert.equal(list.notices[0].readAt, null, 'the list it was given stays as it was');
  assert.equal(markRead(read, 'a', 'later').unread, 1, 'reading again changes nothing');
  assert.equal(markRead(read, 'c', 'later').notices[2].readAt, '2026-10-05T07:00:00Z');
  assert.equal(markRead(list, 'none', 'later').unread, 2);
  const all = markAllRead(list, 'now');
  assert.equal(all.unread, 0);
  assert.ok(all.notices.every((n) => n.readAt !== null));
  assert.equal(all.notices[2].readAt, '2026-10-05T07:00:00Z');
  assert.deepEqual(removeNotice(list, 'b').notices.map((n) => n.id), ['a', 'c']);
  assert.equal(removeNotice(list, 'b').unread, 1);
  assert.equal(removeNotice(list, 'c').unread, 2, 'a read one takes nothing off the count');
  assert.equal(removeNotice({ notices: [notice({ id: 'a' })], unread: 0 }, 'a').unread, 0);
});
