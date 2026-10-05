// Notices: what addons tell the signed-in person, as the shell's bell reads
// them (GET /api/portal/me/notices) and changes them (POST …/{id}/read,
// POST …/read-all, DELETE …/{id}). What a notice says is the addon's: plain
// text, shown as text and never as markup, and a link is followed only while
// it stays on this instance — portal-api checks both when a notice is
// posted, and the bell checks again before it follows one. Pure, so npm test
// runs it under node.

// ─── the documents (mirror server/internal/model.Notice) ────────────────────

export interface Notice {
  id: string;
  addon: string;
  // addonTitle and addonIcon: the addon's app, which the notice is from.
  addonTitle: string;
  addonIcon: string;
  title: string;
  body: string;
  // link and itemId: '' for none.
  link: string;
  itemId: string;
  createdAt: string;
  // readAt: null while unread.
  readAt: string | null;
}

export interface NoticeList {
  notices: Notice[];
  unread: number;
}

// How often the bell asks again while the page is shown.
export const NOTICE_POLL_MS = 60_000;

const str = (v: unknown): string => (typeof v === 'string' ? v : '');

// noticeListOf reads GET /me/notices as it came: what is no notice — no id,
// no title — is left out, and the unread count is never below the unread
// notices listed.
export function noticeListOf(raw: unknown): NoticeList {
  const doc = (raw && typeof raw === 'object' ? raw : {}) as { notices?: unknown; unread?: unknown };
  const notices: Notice[] = [];
  if (Array.isArray(doc.notices)) {
    for (const n of doc.notices as Record<string, unknown>[]) {
      if (!n || typeof n !== 'object' || !str(n.id) || !str(n.title)) continue;
      notices.push({
        id: str(n.id),
        addon: str(n.addon),
        addonTitle: str(n.addonTitle),
        addonIcon: str(n.addonIcon),
        title: str(n.title),
        body: str(n.body),
        link: str(n.link),
        itemId: str(n.itemId),
        createdAt: str(n.createdAt),
        readAt: typeof n.readAt === 'string' && n.readAt ? n.readAt : null,
      });
    }
  }
  const listed = notices.filter((n) => n.readAt === null).length;
  const unread = typeof doc.unread === 'number' && Number.isInteger(doc.unread) && doc.unread >= 0 ? doc.unread : listed;
  return { notices, unread: Math.max(unread, listed) };
}

// ─── reading ─────────────────────────────────────────────────────────────────

// badgeText is the unread count on the bell: nothing at none, 99+ past 99.
export function badgeText(unread: number): string {
  if (!(unread > 0)) return '';
  return unread > 99 ? '99+' : String(Math.floor(unread));
}

// bellLabel is what the bell is called to a screen reader.
export function bellLabel(unread: number): string {
  return unread > 0 ? `Notices, ${unread} unread` : 'Notices';
}

// fromText is whom a notice is from: its addon's title, else its key.
export function fromText(n: Pick<Notice, 'addon' | 'addonTitle'>): string {
  return n.addonTitle.trim() || n.addon || 'An addon';
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

// ageText says when a notice came, short: now, 5m, 3h, 2d — and a date from
// a week on.
export function ageText(createdAt: string, now: number): string {
  const t = Date.parse(createdAt);
  if (Number.isNaN(t)) return '';
  const ago = now - t;
  if (ago < MINUTE) return 'now';
  if (ago < HOUR) return `${Math.floor(ago / MINUTE)}m`;
  if (ago < DAY) return `${Math.floor(ago / HOUR)}h`;
  if (ago < 7 * DAY) return `${Math.floor(ago / DAY)}d`;
  return new Date(t).toLocaleDateString('en-GB', { day: 'numeric', month: 'short' });
}

// noticeHref is where a notice's link may lead: an http(s) page on the
// origin of the page the bell is on — given as a path or absolute — and
// nothing else: no javascript: or data: URL, no other host, no "//host", no
// credentials. null when it leads nowhere it may.
export function noticeHref(link: string, pageUrl: string): string | null {
  const raw = link.trim();
  // A path, or an absolute http(s) URL: nothing relative to the page, and no
  // protocol-relative "//host" (which a backslash spells too).
  if (!/^(\/(?![/\\])|https?:\/\/)/i.test(raw)) return null;
  let page: URL;
  let u: URL;
  try {
    page = new URL(pageUrl);
    u = new URL(raw, page);
  } catch {
    return null;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
  if (u.origin !== page.origin || u.username || u.password) return null;
  return u.href;
}

// shellRoute is the route inside the shell an href on this origin opens —
// /app/example for /portal/app/example — so it opens without a reload; null
// for a page the shell does not route (another app, the API).
export function shellRoute(href: string, pageUrl: string, base = '/portal'): string | null {
  let u: URL;
  let page: URL;
  try {
    page = new URL(pageUrl);
    u = new URL(href, page);
  } catch {
    return null;
  }
  if (u.origin !== page.origin) return null;
  const root = base.replace(/\/+$/, '');
  if (u.pathname !== root && !u.pathname.startsWith(root + '/')) return null;
  return (u.pathname.slice(root.length) || '/') + u.search + u.hash;
}

// ─── changing ────────────────────────────────────────────────────────────────

// markRead is the list once a notice is read, before portal-api answers.
export function markRead(list: NoticeList, id: string, at: string): NoticeList {
  let changed = 0;
  const notices = list.notices.map((n) => {
    if (n.id !== id || n.readAt !== null) return n;
    changed++;
    return { ...n, readAt: at };
  });
  return { notices, unread: Math.max(0, list.unread - changed) };
}

// markAllRead is the list once every notice is read.
export function markAllRead(list: NoticeList, at: string): NoticeList {
  return { notices: list.notices.map((n) => (n.readAt === null ? { ...n, readAt: at } : n)), unread: 0 };
}

// removeNotice is the list without a notice.
export function removeNotice(list: NoticeList, id: string): NoticeList {
  const gone = list.notices.find((n) => n.id === id);
  return {
    notices: list.notices.filter((n) => n.id !== id),
    unread: Math.max(0, list.unread - (gone && gone.readAt === null ? 1 : 0)),
  };
}
