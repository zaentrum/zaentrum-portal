import { Button, IconButton, Spinner } from '@nalet/design-system';
import { Bell, CheckCheck, X } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { ApiError, usePortalApi } from '../lib/api';
import {
  NOTICE_POLL_MS,
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
} from '../lib/notices';
import './notices.css';

// The bell in the shell's header: what addons told the signed-in person —
// an unread count, the list, read one or all, delete one. The portal is
// where people land, so it is on every page of the shell. A notice is the
// addon's plain text and is shown as text; its link is followed only while
// it stays on this instance.
//
// Best effort: a portal-api without notices (404) shows no bell at all, one
// that does not answer keeps the last list and says so in the panel.
export function NoticesBell() {
  const api = usePortalApi();
  const navigate = useNavigate();
  const [list, setList] = useState<NoticeList | null>(null);
  const [absent, setAbsent] = useState(false);
  const [failed, setFailed] = useState(false);
  const [open, setOpen] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const root = useRef<HTMLDivElement>(null);

  const load = useCallback(async () => {
    try {
      setList(noticeListOf(await api<unknown>('/me/notices')));
      setFailed(false);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) setAbsent(true);
      else setFailed(true);
    }
    setNow(Date.now());
  }, [api]);

  // Now, every minute while the page is shown, and when it is shown again.
  useEffect(() => {
    void load();
    const shown = () => document.visibilityState === 'visible';
    const t = window.setInterval(() => shown() && void load(), NOTICE_POLL_MS);
    const onShow = () => shown() && void load();
    document.addEventListener('visibilitychange', onShow);
    return () => {
      window.clearInterval(t);
      document.removeEventListener('visibilitychange', onShow);
    };
  }, [load]);

  // The panel closes on a click outside it and on Escape.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (root.current && !root.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false);
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDown);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  if (absent) return null;
  const unread = list?.unread ?? 0;
  const stamp = () => new Date().toISOString();

  // Each change shows at once and is sent; one portal-api refuses reloads
  // the list as it is.
  const send = (path: string, init: RequestInit) => api<unknown>(path, init).catch(() => load());

  const read = (n: Notice) => {
    if (n.readAt !== null) return;
    setList((l) => (l ? markRead(l, n.id, stamp()) : l));
    void send(`/me/notices/${encodeURIComponent(n.id)}/read`, { method: 'POST' });
  };
  const readAll = () => {
    setList((l) => (l ? markAllRead(l, stamp()) : l));
    void send('/me/notices/read-all', { method: 'POST' });
  };
  const remove = (n: Notice) => {
    setList((l) => (l ? removeNotice(l, n.id) : l));
    void send(`/me/notices/${encodeURIComponent(n.id)}`, { method: 'DELETE' });
  };
  // Opening a notice reads it, and follows its link — inside the shell as a
  // route, elsewhere on this instance as a page.
  const follow = (n: Notice) => {
    read(n);
    const href = noticeHref(n.link, window.location.href);
    if (!href) return;
    setOpen(false);
    const route = shellRoute(href, window.location.href);
    if (route) navigate(route);
    else window.location.assign(href);
  };

  const toggle = () => {
    if (!open) void load();
    setOpen(!open);
  };

  return (
    <div className="nb" ref={root}>
      <IconButton label={bellLabel(unread)} variant="ghost" size="sm" aria-expanded={open} aria-haspopup="dialog" onClick={toggle}>
        <Bell size={16} strokeWidth={1.75} />
      </IconButton>
      {unread > 0 && (
        <span className="nb__count" aria-hidden>
          {badgeText(unread)}
        </span>
      )}
      {open && (
        <div className="nb__panel" role="dialog" aria-label="Notices">
          <div className="nb__head">
            <span className="nb__title">Notices</span>
            {unread > 0 && (
              <Button variant="ghost" size="sm" leading={<CheckCheck size={14} />} onClick={readAll}>
                Mark All Read
              </Button>
            )}
          </div>
          {failed && <p className="nb__note">Notices could not be loaded just now.</p>}
          {list === null ? (
            !failed && (
              <div className="nb__note">
                <Spinner />
              </div>
            )
          ) : list.notices.length === 0 ? (
            <p className="nb__note">No notices. An addon you use can tell you something here.</p>
          ) : (
            <ul className="nb__list">
              {list.notices.map((n) => {
                const href = noticeHref(n.link, window.location.href);
                return (
                  <li key={n.id} className={n.readAt === null ? 'nb__row nb__row--unread' : 'nb__row'}>
                    <button type="button" className="nb__item" onClick={() => follow(n)}>
                      <span className="nb__meta">
                        <span className="nb__from">{fromText(n)}</span>
                        <span className="nb__age">{ageText(n.createdAt, now)}</span>
                      </span>
                      <span className="nb__ntitle">{n.title}</span>
                      <span className="nb__body">{n.body}</span>
                      {href && <span className="nb__open">Open</span>}
                    </button>
                    <IconButton label={`Delete notice: ${n.title}`} variant="ghost" size="sm" onClick={() => remove(n)}>
                      <X size={14} />
                    </IconButton>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
