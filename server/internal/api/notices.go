package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/ratelimit"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// ─── notices ─────────────────────────────────────────────────────────────────
//
// An addon tells one person something — "your title is ready" — and every
// client that person uses shows it: the portal's bell, and the product apps
// through chino-api (GET /api/v1/notices). The core does not know what a
// notice says. The text is the addon's, plain text; the core keeps it for its
// person, shows it to them and to nobody else, and forgets it (migration 015).
//
//	POST   /notices               an addon's service account  a notice for one person, from that addon and no other
//	POST   /me/notices            a person                    a notice for themselves, from an installed addon they name
//	GET    /me/notices            a person                    their own, newest first, and how many are unread
//	POST   /me/notices/{id}/read  a person                    one of theirs, read
//	POST   /me/notices/read-all   a person                    all of theirs, read
//	DELETE /me/notices/{id}       a person                    one of theirs, deleted
//	GET    /notices?addon=        admin                       each addon's notices counted, never what they say
//
// A notice to oneself is for an addon that holds no credential, as the
// reference addon holds none: its console, inside the shell, posts with the
// bearer the shell hands it. It reaches nobody but the bearer's own subject,
// so it tells no one anything the software they already trust with their
// session could not show them anyway — and it is no proof of which addon sent
// it, since whoever holds a person's bearer may name any installed addon. A
// notice for someone else, or for later, takes the addon's service account,
// whose token binds its addon.
//
// A notice's link is held to the rule a slot row's link is — slotURL decides
// both — and a path is made absolute on the instance's public origin when
// portal-api knows it, as an install does for a slot row. Every write is
// logged; what a notice says never is.

// noticeStore is the notices as these routes keep them. *store.Store
// implements it; tests substitute an in-memory one.
type noticeStore interface {
	PostNotice(ctx context.Context, in store.NewNotice) (model.Notice, int, error)
	Notices(ctx context.Context, sub string, limit int) ([]model.Notice, int, error)
	ReadNotice(ctx context.Context, sub, id string) (int, error)
	ReadAllNotices(ctx context.Context, sub string) (int, error)
	DeleteNotice(ctx context.Context, sub, id string) error
	DeletePersonNotices(ctx context.Context, sub string) (int, error)
	SweepNotices(ctx context.Context, cutoff time.Time) (int, error)
	NoticeCounts(ctx context.Context, addon string) ([]model.NoticeCount, error)
}

const (
	maxNoticeTitle = 80
	maxNoticeBody  = 280
	maxNoticeSub   = 255
	// maxNoticeBytes bounds what a post sends: the four fields, generously.
	maxNoticeBytes = 8 << 10

	// The limits of posting, per replica: an addon posts a hundred at once
	// — a batch of titles ready together — and one a second after that; a
	// person posting to themselves, ten and one every six seconds.
	noticeAddonBurst  = 100
	noticeAddonEvery  = time.Second
	noticePersonBurst = 10
	noticePersonEvery = 6 * time.Second

	// noticeSweepEvery is how often the notices past their retention go;
	// defaultNoticeRetention how long they are kept when nothing says.
	noticeSweepEvery       = time.Hour
	defaultNoticeRetention = 90 * 24 * time.Hour
)

// newNoticeLimits are the limits of posting notices: per addon, and per
// person posting to themselves.
func newNoticeLimits() (perAddon, perPerson *ratelimit.Limiter) {
	return ratelimit.New(noticeAddonBurst, noticeAddonEvery), ratelimit.New(noticePersonBurst, noticePersonEvery)
}

// noticeItem is what a catalog item's id may be: short, and nothing a path or
// a URL would read as more than an id.
var noticeItem = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// noticeText is what a notice says and where it leads, as it is posted.
type noticeText struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Link   string `json:"link"`
	ItemID string `json:"itemId"`
}

// noticeList is GET /me/notices.
type noticeList struct {
	Notices []model.Notice `json:"notices"`
	Unread  int            `json:"unread"`
}

// postNotice handles POST /notices {sub, title, body, link?, itemId?}: an
// addon's service account tells one person something, as that addon. The
// addon is the token's — the body names none, and one that does is refused
// as an unknown field.
func (a *API) postNotice(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if p == nil || p.Addon == "" { // RequireAddon refused it already
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	addon := p.Addon
	var body struct {
		Sub string `json:"sub"`
		noticeText
	}
	if !decodeNotice(w, r, &body) {
		return
	}
	sub, err := noticeSub(body.Sub)
	if err != nil {
		badRequest(w, "notice "+err.Error())
		return
	}
	in, err := a.checkNotice(r, addon, sub, body.noticeText)
	if err != nil {
		badRequest(w, "notice "+err.Error())
		return
	}
	if !noticeAllowed(w, a.noticeAddon, addon) {
		return
	}
	switch ok, err := a.addonInstalled(r.Context(), addon); {
	case err != nil:
		serverError(w, err)
		return
	case !ok:
		http.Error(w, fmt.Sprintf("forbidden: addon %q is not installed on this instance — an admin adds it in settings → addons; its service account then posts its notices", addon),
			http.StatusForbidden)
		return
	}
	n, ok := a.storeNotice(r.Context(), w, in)
	if !ok {
		return
	}
	log.Printf("notices: addon %s posted %s to %s", addon, n.ID, sub)
	writeJSON(w, http.StatusCreated, servableNotice(n))
}

// postOwnNotice handles POST /me/notices {addon, title, body, link?,
// itemId?}: a person posts a notice to themselves — never to anyone else, the
// body names no person — from an installed addon the body names.
func (a *API) postOwnNotice(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if p == nil || p.Subject == "" || auth.IsServiceAccount(p) {
		http.Error(w, "forbidden: a person posts a notice to themselves here — an addon's service account posts with POST /api/portal/notices",
			http.StatusForbidden)
		return
	}
	var body struct {
		Addon string `json:"addon"`
		noticeText
	}
	if !decodeNotice(w, r, &body) {
		return
	}
	addon := strings.TrimSpace(body.Addon)
	if !isDNSLabel(addon) {
		badRequest(w, "notice addon: the key of an installed addon, e.g. sample")
		return
	}
	in, err := a.checkNotice(r, addon, p.Subject, body.noticeText)
	if err != nil {
		badRequest(w, "notice "+err.Error())
		return
	}
	if !noticeAllowed(w, a.noticePerson, p.Subject) {
		return
	}
	switch ok, err := a.addonInstalled(r.Context(), addon); {
	case err != nil:
		serverError(w, err)
		return
	case !ok:
		badRequest(w, fmt.Sprintf("notice addon: no addon %q is installed here", addon))
		return
	}
	n, ok := a.storeNotice(r.Context(), w, in)
	if !ok {
		return
	}
	log.Printf("notices: %s posted %s to themselves, from addon %s", requester(r), n.ID, addon)
	writeJSON(w, http.StatusCreated, servableNotice(n))
}

// storeNotice stores a checked notice; false once it answered.
func (a *API) storeNotice(ctx context.Context, w http.ResponseWriter, in store.NewNotice) (model.Notice, bool) {
	n, pruned, err := a.notices.PostNotice(ctx, in)
	switch {
	case errors.Is(err, store.ErrNoAddon):
		// Removed since it was looked up.
		http.Error(w, fmt.Sprintf("addon %q is not installed on this instance", in.Addon), http.StatusConflict)
		return n, false
	case err != nil:
		serverError(w, err)
		return n, false
	}
	if pruned > 0 {
		log.Printf("notices: %d older notices of %s went: a person keeps their newest %d", pruned, in.Sub, store.NoticesKept)
	}
	return n, true
}

// myNotices handles GET /me/notices: the caller's notices, newest first, and
// how many of theirs are unread.
func (a *API) myNotices(w http.ResponseWriter, r *http.Request) {
	sub := caller(r)
	out := noticeList{Notices: []model.Notice{}}
	if sub == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	list, unread, err := a.notices.Notices(r.Context(), sub, store.NoticesKept)
	if err != nil {
		serverError(w, err)
		return
	}
	for _, n := range list {
		out.Notices = append(out.Notices, servableNotice(n))
	}
	out.Unread = unread
	writeJSON(w, http.StatusOK, out)
}

// readNotice handles POST /me/notices/{id}/read: one of the caller's notices,
// read, and how many of theirs are unread still. Someone else's is 404, as
// one there is not.
func (a *API) readNotice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	unread, err := a.notices.ReadNotice(r.Context(), caller(r), id)
	if !noticeFound(w, err) {
		return
	}
	log.Printf("notices: %s read %s", requester(r), id)
	writeJSON(w, http.StatusOK, map[string]any{"unread": unread})
}

// readAllNotices handles POST /me/notices/read-all: every notice of the
// caller's, read.
func (a *API) readAllNotices(w http.ResponseWriter, r *http.Request) {
	read, err := a.notices.ReadAllNotices(r.Context(), caller(r))
	if err != nil {
		serverError(w, err)
		return
	}
	if read > 0 {
		log.Printf("notices: %s read all their notices (%d)", requester(r), read)
	}
	writeJSON(w, http.StatusOK, map[string]any{"read": read, "unread": 0})
}

// deleteNotice handles DELETE /me/notices/{id}: one of the caller's notices,
// deleted. Someone else's is 404, as one there is not.
func (a *API) deleteNotice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !noticeFound(w, a.notices.DeleteNotice(r.Context(), caller(r), id)) {
		return
	}
	log.Printf("notices: %s deleted %s", requester(r), id)
	w.WriteHeader(http.StatusNoContent)
}

// noticeCounts handles GET /notices?addon=: each addon's notices counted —
// how many, how many unread, for how many people, the newest — for the
// admin consoles; never what one says, nor whom it is for. An addon named
// that has none is counted as none.
func (a *API) noticeCounts(w http.ResponseWriter, r *http.Request) {
	addon := strings.TrimSpace(r.URL.Query().Get("addon"))
	if addon != "" && !isDNSLabel(addon) {
		badRequest(w, "addon: the key of an addon, e.g. sample")
		return
	}
	counts, err := a.notices.NoticeCounts(r.Context(), addon)
	if err != nil {
		serverError(w, err)
		return
	}
	if addon != "" && len(counts) == 0 {
		counts = []model.NoticeCount{{Addon: addon}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"addons":         nonNil(counts),
		"kept":           store.NoticesKept,
		"retentionHours": int(a.noticeRetention() / time.Hour),
	})
}

// noticeFound answers a change of a notice that is not the caller's — or not
// at all — with 404; false once it answered.
func noticeFound(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such notice", http.StatusNotFound)
		return false
	case err != nil:
		serverError(w, err)
		return false
	}
	return true
}

// decodeNotice reads a notice's body: one JSON object of known fields, of a
// size a notice can be.
func decodeNotice(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxNoticeBytes)
	return decode(w, r, dst)
}

// checkNotice checks what a notice for sub from addon says and where it
// leads, and answers it as it is stored: trimmed, and a path link made
// absolute on the instance's public origin when portal-api knows it.
func (a *API) checkNotice(r *http.Request, addon, sub string, in noticeText) (store.NewNotice, error) {
	title, err := plainText("title", in.Title, maxNoticeTitle, false)
	if err != nil {
		return store.NewNotice{}, err
	}
	body, err := plainText("body", in.Body, maxNoticeBody, true)
	if err != nil {
		return store.NewNotice{}, err
	}
	link := ""
	if strings.TrimSpace(in.Link) != "" {
		public := a.publicOrigin(r.Context())
		if link, err = slotURL(in.Link, originsOf(r, public)); err != nil {
			return store.NewNotice{}, fmt.Errorf("link: %v", err)
		}
		if strings.HasPrefix(link, "/") && public != "" {
			link = public + link
		}
	}
	item := strings.TrimSpace(in.ItemID)
	if item != "" && !noticeItem.MatchString(item) {
		return store.NewNotice{}, fmt.Errorf("itemId %q is no catalog item's id: letters, digits and . _ : -, at most 128", in.ItemID)
	}
	return store.NewNotice{Sub: sub, Addon: addon, Title: title, Body: body, Link: link, ItemID: item}, nil
}

// plainText checks one of a notice's texts: given, at most max characters,
// and plain — no control characters (but, where lines is set, line breaks)
// and none of the formatting characters that turn the text around.
func plainText(field, raw string, max int, lines bool) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if n := utf8.RuneCountInString(s); n > max {
		return "", fmt.Errorf("%s is at most %d characters; this one has %d", field, max, n)
	}
	for _, c := range s {
		switch {
		case c == '\n' && lines:
		case unicode.IsControl(c), c >= 0x202a && c <= 0x202e, c >= 0x2066 && c <= 0x2069:
			if lines {
				return "", fmt.Errorf("%s is plain text: no control characters but line breaks", field)
			}
			return "", fmt.Errorf("%s is plain text: no control characters or line breaks", field)
		}
	}
	return s, nil
}

// noticeSub checks whom a notice is for: a token subject — given, at most 255
// characters, no spaces or control characters.
func noticeSub(raw string) (string, error) {
	sub := strings.TrimSpace(raw)
	switch {
	case sub == "":
		return "", errors.New("sub is required: the subject of the person's token")
	case len(sub) > maxNoticeSub:
		return "", fmt.Errorf("sub is at most %d bytes", maxNoticeSub)
	case hasControl(sub) || strings.ContainsAny(sub, "  "):
		return "", fmt.Errorf("sub %q is no token subject", raw)
	}
	return sub, nil
}

// servableNotice is a stored notice as it is served: a link that does not
// keep the link rule's shape, or an item id that is none, is left out — what
// a client follows is checked on the way out as on the way in.
func servableNotice(n model.Notice) model.Notice {
	if n.Link != "" {
		if info, err := parseSlotURL(n.Link); err == nil {
			n.Link = info.url
		} else {
			n.Link = ""
		}
	}
	if n.ItemID != "" && !noticeItem.MatchString(n.ItemID) {
		n.ItemID = ""
	}
	return n
}

// addonInstalled reports whether key is an installed addon.
func (a *API) addonInstalled(ctx context.Context, key string) (bool, error) {
	_, err := a.addons.GetAddon(ctx, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// noticeAllowed takes a post from key's bucket of limit; false once it
// answered 429.
func noticeAllowed(w http.ResponseWriter, limit *ratelimit.Limiter, key string) bool {
	if limit == nil {
		return true
	}
	ok, wait := limit.Allow(key)
	if ok {
		return true
	}
	secs := int(wait.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, "Too many notices: wait a moment, then try again.", http.StatusTooManyRequests)
	return false
}

// noticeRetention is how long a notice is kept.
func (a *API) noticeRetention() time.Duration {
	if a.cfg.NoticeRetention > 0 {
		return a.cfg.NoticeRetention
	}
	return defaultNoticeRetention
}

// RunNoticeSweep deletes the notices past their retention at once, and then
// every hour, until ctx ends.
func (a *API) RunNoticeSweep(ctx context.Context) {
	if a.notices == nil {
		return
	}
	a.sweepNotices(ctx)
	t := time.NewTicker(noticeSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sweepNotices(ctx)
		}
	}
}

// sweepNotices deletes the notices past their retention, once.
func (a *API) sweepNotices(ctx context.Context) {
	retention := a.noticeRetention()
	n, err := a.notices.SweepNotices(ctx, a.clock().Add(-retention))
	switch {
	case err != nil && ctx.Err() == nil:
		log.Printf("notices: the notices past their retention stay for now: %v", err)
	case n > 0:
		log.Printf("notices: %d older than %s swept", n, retention)
	}
}

// dropNotices deletes the notices of a person whose account went.
func (a *API) dropNotices(ctx context.Context, sub, who string) {
	if a.notices == nil {
		return
	}
	n, err := a.notices.DeletePersonNotices(ctx, sub)
	switch {
	case err != nil:
		log.Printf("notices: the notices of %s stay until their retention: %v", who, err)
	case n > 0:
		log.Printf("notices: the %d notices of %s went with their account", n, who)
	}
}
