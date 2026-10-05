package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth/authtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/ratelimit"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// ─── the store, in memory ────────────────────────────────────────────────────

// fakeNotices is the notices in memory, with the store's rules: every read
// and change is of the subject's own, a person keeps their newest hundred,
// and a notice names an installed addon — the addons and their apps' titles
// are reg's.
type fakeNotices struct {
	mu   sync.Mutex
	reg  *fakeAddonStore
	rows []fakeNotice
	seq  int
}

type fakeNotice struct {
	model.Notice
	sub string
}

func newFakeNotices(reg *fakeAddonStore) *fakeNotices { return &fakeNotices{reg: reg} }

var noticeEpoch = time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)

func (f *fakeNotices) PostNotice(_ context.Context, in store.NewNotice) (model.Notice, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.reg.addons[in.Addon]; !ok {
		return model.Notice{}, 0, store.ErrNoAddon
	}
	f.seq++
	app := f.reg.apps[in.Addon]
	n := model.Notice{
		ID: fmt.Sprintf("%08x-0000-4000-8000-%012x", f.seq, f.seq), Addon: in.Addon, AddonTitle: app.Title, AddonIcon: app.Icon,
		Title: in.Title, Body: in.Body, Link: in.Link, ItemID: in.ItemID, CreatedAt: noticeEpoch.Add(time.Duration(f.seq) * time.Second),
	}
	f.rows = append(f.rows, fakeNotice{Notice: n, sub: in.Sub})
	// Keep the person's newest hundred.
	mine := f.of(in.Sub)
	pruned := 0
	if len(mine) > store.NoticesKept {
		drop := map[string]bool{}
		for _, old := range mine[store.NoticesKept:] {
			drop[old.ID] = true
		}
		kept := f.rows[:0]
		for _, r := range f.rows {
			if drop[r.ID] {
				pruned++
				continue
			}
			kept = append(kept, r)
		}
		f.rows = kept
	}
	return n, pruned, nil
}

// of is sub's notices, newest first.
func (f *fakeNotices) of(sub string) []model.Notice {
	var out []model.Notice
	for _, r := range f.rows {
		if r.sub == sub {
			out = append(out, r.Notice)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (f *fakeNotices) unread(sub string) int {
	n := 0
	for _, r := range f.rows {
		if r.sub == sub && r.ReadAt == nil {
			n++
		}
	}
	return n
}

func (f *fakeNotices) Notices(_ context.Context, sub string, limit int) ([]model.Notice, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.of(sub)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, f.unread(sub), nil
}

func (f *fakeNotices) ReadNotice(_ context.Context, sub, id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].sub == sub && f.rows[i].ID == id {
			if f.rows[i].ReadAt == nil {
				at := noticeEpoch.Add(time.Hour)
				f.rows[i].ReadAt = &at
			}
			return f.unread(sub), nil
		}
	}
	return 0, store.ErrNotFound
}

func (f *fakeNotices) ReadAllNotices(_ context.Context, sub string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i := range f.rows {
		if f.rows[i].sub == sub && f.rows[i].ReadAt == nil {
			at := noticeEpoch.Add(time.Hour)
			f.rows[i].ReadAt = &at
			n++
		}
	}
	return n, nil
}

func (f *fakeNotices) DeleteNotice(_ context.Context, sub, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.rows {
		if r.sub == sub && r.ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeNotices) DeletePersonNotices(_ context.Context, sub string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept, n := f.rows[:0], 0
	for _, r := range f.rows {
		if r.sub == sub {
			n++
			continue
		}
		kept = append(kept, r)
	}
	f.rows = kept
	return n, nil
}

func (f *fakeNotices) SweepNotices(_ context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept, n := f.rows[:0], 0
	for _, r := range f.rows {
		if r.CreatedAt.Before(cutoff) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	f.rows = kept
	return n, nil
}

func (f *fakeNotices) NoticeCounts(_ context.Context, addon string) ([]model.NoticeCount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	by := map[string]*model.NoticeCount{}
	people := map[string]map[string]bool{}
	for _, r := range f.rows {
		if addon != "" && r.Addon != addon {
			continue
		}
		c := by[r.Addon]
		if c == nil {
			c = &model.NoticeCount{Addon: r.Addon}
			by[r.Addon], people[r.Addon] = c, map[string]bool{}
		}
		c.Notices++
		if r.ReadAt == nil {
			c.Unread++
		}
		people[r.Addon][r.sub] = true
		c.People = len(people[r.Addon])
		if at := r.CreatedAt; c.Latest == nil || at.After(*c.Latest) {
			c.Latest = &at
		}
	}
	var out []model.NoticeCount
	for _, c := range by {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addon < out[j].Addon })
	return out, nil
}

// all is every notice stored, of everyone.
func (f *fakeNotices) all() []fakeNotice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeNotice(nil), f.rows...)
}

// ─── the routes ──────────────────────────────────────────────────────────────

// The people the notice tests post to and read as: their tokens' subjects
// are user-<name>.
var (
	mia        = authtest.Person("chino-web", "mia", "zaentrum-user")
	leo        = authtest.Person("chino-tv", "leo", "zaentrum-user")
	miaShell   = authtest.Person("zaentrum-web", "mia", "zaentrum-user")
	addonOther = authtest.ServiceAccount("other", "zaentrum-addon")
)

// noticeEnv is the token env with the addons sample and other installed —
// and the platform's hostname, media.example.org, its public origin.
func noticeEnv(t *testing.T) *tokenEnv {
	t.Helper()
	e := newTokenEnv(t)
	for key, title := range map[string]string{"sample": "Sample Addon", "other": "Other Addon"} {
		e.store.apps[key] = model.App{Key: key, Title: title, Icon: "puzzle", Enabled: true}
		e.store.addons[key] = model.Addon{Key: key}
	}
	e.kube.Put("zaentrums", map[string]any{"metadata": map[string]any{"name": "zaentrum"}, "spec": map[string]any{"hostname": "media.example.org"}})
	return e
}

func notice(sub string, kv ...any) map[string]any {
	m := map[string]any{"sub": sub, "title": "Your title is ready", "body": "It is in your library now."}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func ownNotice(addon string, kv ...any) map[string]any {
	m := map[string]any{"addon": addon, "title": "Hello", "body": "From the console."}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func (e *tokenEnv) listOf(claims map[string]any) noticeList {
	e.t.Helper()
	rec := e.do(claims, http.MethodGet, "/api/portal/me/notices", nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /me/notices = %d %s", rec.Code, rec.Body)
	}
	return decodeAs[noticeList](e.t, rec)
}

// Each route takes its callers: an installed addon's service account posts,
// a person reads theirs and posts to themselves, an admin counts.
func TestNoticeRoutesTakeTheirCallers(t *testing.T) {
	e := noticeEnv(t)
	personWithAddonRole := authtest.Person("chino-web", "alice", "zaentrum-addon")
	notInstalled := authtest.ServiceAccount("ghost", "zaentrum-addon")
	for _, c := range []struct {
		name         string
		claims       map[string]any
		method, path string
		body         any
		want         int
	}{
		{"an addon posts", addonSample, http.MethodPost, "/api/portal/notices", notice("user-mia"), http.StatusCreated},
		{"an addon not installed here", notInstalled, http.MethodPost, "/api/portal/notices", notice("user-mia"), http.StatusForbidden},
		{"an admin posts as an addon", adminPortal, http.MethodPost, "/api/portal/notices", notice("user-mia"), http.StatusForbidden},
		{"a person posts as an addon", mia, http.MethodPost, "/api/portal/notices", notice("user-leo"), http.StatusForbidden},
		{"a person given the addon role", personWithAddonRole, http.MethodPost, "/api/portal/notices", notice("user-leo"), http.StatusForbidden},
		{"no bearer posts", nil, http.MethodPost, "/api/portal/notices", notice("user-mia"), http.StatusUnauthorized},

		{"a person posts to themselves", miaShell, http.MethodPost, "/api/portal/me/notices", ownNotice("sample"), http.StatusCreated},
		{"an addon posts to itself", addonSample, http.MethodPost, "/api/portal/me/notices", ownNotice("sample"), http.StatusForbidden},
		{"no bearer posts to themselves", nil, http.MethodPost, "/api/portal/me/notices", ownNotice("sample"), http.StatusUnauthorized},

		{"a person reads", mia, http.MethodGet, "/api/portal/me/notices", nil, http.StatusOK},
		{"a person reads all", leo, http.MethodPost, "/api/portal/me/notices/read-all", nil, http.StatusOK},
		{"no bearer reads", nil, http.MethodGet, "/api/portal/me/notices", nil, http.StatusUnauthorized},
		{"no bearer reads all", nil, http.MethodPost, "/api/portal/me/notices/read-all", nil, http.StatusUnauthorized},

		{"an admin counts", adminPortal, http.MethodGet, "/api/portal/notices", nil, http.StatusOK},
		{"an admin through the CLI counts", adminCLI, http.MethodGet, "/api/portal/notices", nil, http.StatusOK},
		{"an admin through the media app counts", adminMedia, http.MethodGet, "/api/portal/notices", nil, http.StatusForbidden},
		{"a person counts", mia, http.MethodGet, "/api/portal/notices", nil, http.StatusForbidden},
		{"an addon counts", addonSample, http.MethodGet, "/api/portal/notices", nil, http.StatusForbidden},
	} {
		if rec := e.do(c.claims, c.method, c.path, c.body); rec.Code != c.want {
			t.Errorf("%s: %s %s = %d %s, want %d", c.name, c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
	// Two posted: the addon's to mia, and mia's to herself.
	if got := e.notices.all(); len(got) != 2 {
		t.Errorf("stored %d notices: %+v", len(got), got)
	}
}

// An addon posts as itself and no other: its token says which addon, the
// body cannot — naming one is refused, and nothing is stored.
func TestAnAddonPostsAsItselfOnly(t *testing.T) {
	e := noticeEnv(t)
	for name, claims := range map[string]map[string]any{
		"named after its addon": addonSample,
		"bound by the zaentrum_addon claim": func() map[string]any {
			c := authtest.ServiceAccount("demo-sample-svc", "zaentrum-addon")
			c["zaentrum_addon"] = "sample"
			return c
		}(),
	} {
		rec := e.do(claims, http.MethodPost, "/api/portal/notices", notice("user-mia"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if got := decodeAs[model.Notice](t, rec); got.Addon != "sample" || got.AddonTitle != "Sample Addon" {
			t.Errorf("%s: posted %+v", name, got)
		}
	}
	for name, body := range map[string]map[string]any{
		"another addon named":     notice("user-mia", "addon", "other"),
		"its own addon named":     notice("user-mia", "addon", "sample"),
		"a second person named":   notice("user-mia", "subs", []string{"user-leo"}),
		"from someone else named": notice("user-mia", "from", "other"),
	} {
		if rec := e.do(addonSample, http.MethodPost, "/api/portal/notices", body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown field") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := e.do(addonOther, http.MethodPost, "/api/portal/notices", notice("user-leo")); rec.Code != http.StatusCreated {
		t.Fatalf("other: %d %s", rec.Code, rec.Body)
	}
	byAddon := map[string]string{}
	for _, n := range e.notices.all() {
		byAddon[n.sub+"/"+n.Addon] = n.Title
	}
	if len(byAddon) != 2 || byAddon["user-mia/sample"] == "" || byAddon["user-leo/other"] == "" {
		t.Errorf("stored %v", byAddon)
	}
	// Each is its person's: mia sees sample's two, leo other's one.
	if got := e.listOf(mia); len(got.Notices) != 2 || got.Unread != 2 || got.Notices[0].Addon != "sample" {
		t.Errorf("mia's = %+v", got)
	}
	if got := e.listOf(leo); len(got.Notices) != 1 || got.Notices[0].Addon != "other" {
		t.Errorf("leo's = %+v", got)
	}
}

// A person reads, marks and deletes their own notices; someone else's id is
// as one there is not, and nothing of theirs changes.
func TestAPersonReadsOnlyTheirOwn(t *testing.T) {
	e := noticeEnv(t)
	post := func(sub, title string) string {
		rec := e.do(addonSample, http.MethodPost, "/api/portal/notices", notice(sub, "title", title))
		if rec.Code != http.StatusCreated {
			t.Fatalf("post: %d %s", rec.Code, rec.Body)
		}
		return decodeAs[model.Notice](t, rec).ID
	}
	miaFirst, miaSecond := post("user-mia", "first"), post("user-mia", "second")
	leos := post("user-leo", "leo's")

	got := e.listOf(mia)
	if len(got.Notices) != 2 || got.Notices[0].Title != "second" || got.Notices[1].Title != "first" || got.Unread != 2 {
		t.Fatalf("mia's = %+v", got)
	}
	// Leo, with mia's ids.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/portal/me/notices/" + miaFirst + "/read"},
		{http.MethodDelete, "/api/portal/me/notices/" + miaFirst},
	} {
		if rec := e.do(leo, c.method, c.path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("leo %s %s = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if rec := e.do(leo, http.MethodPost, "/api/portal/me/notices/read-all", nil); rec.Code != http.StatusOK || decodeAs[map[string]int](t, rec)["read"] != 1 {
		t.Errorf("leo reads all = %d %s", rec.Code, rec.Body)
	}
	if got := e.listOf(mia); len(got.Notices) != 2 || got.Unread != 2 {
		t.Fatalf("leo changed mia's: %+v", got)
	}
	if got := e.listOf(leo); len(got.Notices) != 1 || got.Notices[0].ID != leos || got.Notices[0].ReadAt == nil {
		t.Errorf("leo's = %+v", got)
	}

	// Mia, with hers — through any client: the portal's or a product app's.
	rec := e.do(miaShell, http.MethodPost, "/api/portal/me/notices/"+miaFirst+"/read", nil)
	if rec.Code != http.StatusOK || decodeAs[map[string]int](t, rec)["unread"] != 1 {
		t.Fatalf("mia reads first = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(mia, http.MethodDelete, "/api/portal/me/notices/"+miaSecond, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("mia deletes second = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(mia, http.MethodDelete, "/api/portal/me/notices/"+miaSecond, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleted twice = %d", rec.Code)
	}
	if rec := e.do(mia, http.MethodPost, "/api/portal/me/notices/no-such-notice/read", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an id of none = %d", rec.Code)
	}
	if got := e.listOf(mia); len(got.Notices) != 1 || got.Notices[0].ID != miaFirst || got.Notices[0].ReadAt == nil || got.Unread != 0 {
		t.Errorf("mia's after = %+v", got)
	}
	if got := e.listOf(leo); len(got.Notices) != 1 {
		t.Errorf("leo's after mia's changes = %+v", got)
	}
}

// A person posts a notice to themselves only: the body names no person, and
// one that does is refused; the addon it is from must be installed here.
func TestAPersonPostsOnlyToThemselves(t *testing.T) {
	e := noticeEnv(t)
	rec := e.do(miaShell, http.MethodPost, "/api/portal/me/notices", ownNotice("sample", "link", "/portal/app/sample"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mia to herself = %d %s", rec.Code, rec.Body)
	}
	if got := decodeAs[model.Notice](t, rec); got.Addon != "sample" || got.Link != own+"/portal/app/sample" {
		t.Errorf("posted %+v", got)
	}
	for name, c := range map[string]struct {
		body    map[string]any
		mention string
	}{
		"to someone else":         {ownNotice("sample", "sub", "user-leo"), "unknown field"},
		"to someone else by name": {ownNotice("sample", "to", "leo"), "unknown field"},
		"from an addon not here":  {ownNotice("ghost"), `no addon "ghost" is installed`},
		"from no addon":           {ownNotice(""), "the key of an installed addon"},
		"from a key that is none": {ownNotice("Sample Addon"), "the key of an installed addon"},
		"from a key reaching out": {ownNotice("sample/../other"), "the key of an installed addon"},
	} {
		if rec := e.do(miaShell, http.MethodPost, "/api/portal/me/notices", c.body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.mention) {
			t.Errorf("%s: %d %s, want 400 mentioning %q", name, rec.Code, rec.Body, c.mention)
		}
	}
	all := e.notices.all()
	if len(all) != 1 || all[0].sub != "user-mia" {
		t.Errorf("stored %+v", all)
	}
	if got := e.listOf(leo); len(got.Notices) != 0 {
		t.Errorf("leo got %+v", got)
	}
}

// What a notice says is plain text of a length a client can show, for a
// person a token can name; an item is an id.
func TestNoticeTextRules(t *testing.T) {
	e := noticeEnv(t)
	for name, c := range map[string]struct {
		body    any
		mention string
	}{
		"no title":                      {notice("user-mia", "title", "  "), "title is required"},
		"no body":                       {notice("user-mia", "body", ""), "body is required"},
		"a title of 81 characters":      {notice("user-mia", "title", strings.Repeat("ä", 81)), "title is at most 80 characters; this one has 81"},
		"a body of 281 characters":      {notice("user-mia", "body", strings.Repeat("ä", 281)), "body is at most 280 characters; this one has 281"},
		"a title of two lines":          {notice("user-mia", "title", "one\ntwo"), "no control characters or line breaks"},
		"a body with a carriage return": {notice("user-mia", "body", "one\r\ntwo"), "no control characters but line breaks"},
		"a body with a bell":            {notice("user-mia", "body", "ding\a"), "no control characters"},
		"a title turned around":         {notice("user-mia", "title", "abc‮dcba"), "no control characters"},
		"a title isolated":              {notice("user-mia", "title", "abc⁧x⁩"), "no control characters"},
		"no person":                     {notice(""), "sub is required"},
		"a person of spaces":            {notice("user mia"), "no token subject"},
		"a person over 255 bytes":       {notice(strings.Repeat("u", 256)), "at most 255"},
		"an item that is a path":        {notice("user-mia", "itemId", "../items/1"), "no catalog item's id"},
		"an item over 128":              {notice("user-mia", "itemId", strings.Repeat("a", 129)), "no catalog item's id"},
		"an item with a space":          {notice("user-mia", "itemId", "item 1"), "no catalog item's id"},
		"a title that is no string":     {map[string]any{"sub": "user-mia", "title": 7, "body": "b"}, "invalid json"},
		"two objects":                   {`{"sub":"user-mia","title":"t","body":"b"}{"sub":"user-leo"}`, ""},
		"a body too big to be one":      {notice("user-mia", "body", strings.Repeat("a", 9000)), "invalid json"},
	} {
		rec := e.do(addonSample, http.MethodPost, "/api/portal/notices", c.body)
		if name == "two objects" {
			// One object is read; what follows is not a second notice.
			if rec.Code != http.StatusCreated || len(e.notices.of("user-leo")) != 0 {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.mention) {
			t.Errorf("%s: %d %s, want 400 mentioning %q", name, rec.Code, rec.Body, c.mention)
		}
	}
	// What fits: 80 and 280 characters, a body of lines, an item id, the
	// text trimmed; a link is optional.
	rec := e.do(addonSample, http.MethodPost, "/api/portal/notices", notice("user-mia",
		"title", " "+strings.Repeat("ä", 80)+" ", "body", strings.Repeat("ä", 140)+"\n"+strings.Repeat("ö", 139), "itemId", "3f2b9c1e-0b7a-4c55-9d1e-2a4f6b8c0d1e"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("what fits: %d %s", rec.Code, rec.Body)
	}
	got := decodeAs[model.Notice](t, rec)
	if got.Title != strings.Repeat("ä", 80) || !strings.Contains(got.Body, "\n") || got.ItemID != "3f2b9c1e-0b7a-4c55-9d1e-2a4f6b8c0d1e" || got.Link != "" {
		t.Errorf("stored %+v", got)
	}
}

// One rule decides where a notice may lead and where a slot row may: every
// case of the slot URL rule is taken or refused alike by both write APIs,
// with the same words.
func TestNoticeLinksAreSlotLinks(t *testing.T) {
	e := noticeEnv(t)
	slot := func(url string) *httptest.ResponseRecorder {
		return e.do(addonSample, http.MethodPost, "/api/portal/extensions",
			map[string]any{"key": "sample.link", "slot": "search.empty", "label": "x", "kind": "link", "url": url})
	}
	link := func(url string) *httptest.ResponseRecorder {
		return e.do(addonSample, http.MethodPost, "/api/portal/notices", notice("user-mia", "link", url))
	}
	for _, url := range slotURLsTaken {
		s, n := slot(url), link(url)
		if s.Code != http.StatusOK || n.Code != http.StatusCreated {
			t.Errorf("%q: slot %d %s, notice %d %s — want both taken", url, s.Code, s.Body, n.Code, n.Body)
			continue
		}
		// A path is made absolute on the instance's public origin; an
		// absolute URL is kept as it is.
		stored := decodeAs[model.Notice](t, n).Link
		if want := strings.TrimSpace(url); strings.HasPrefix(want, "/") && stored != own+want || !strings.HasPrefix(want, "/") && stored != want {
			t.Errorf("%q: stored as %q", url, stored)
		}
	}
	for _, c := range slotURLsRefused {
		if c.url == "" {
			continue // a notice without a link has none; a slot row must have one
		}
		s, n := slot(c.url), link(c.url)
		if s.Code != http.StatusBadRequest || n.Code != http.StatusBadRequest {
			t.Errorf("%q: slot %d, notice %d — want both refused", c.url, s.Code, n.Code)
			continue
		}
		if !strings.Contains(s.Body.String(), c.mention) || !strings.Contains(n.Body.String(), c.mention) {
			t.Errorf("%q: slot %q, notice %q — want both to mention %q", c.url, s.Body, n.Body, c.mention)
		}
	}
	if len(e.notices.all()) != len(slotURLsTaken) {
		t.Errorf("stored %d notices for %d links taken", len(e.notices.all()), len(slotURLsTaken))
	}

	// An instance that does not know its origin takes paths only — and keeps
	// them as paths.
	e.kube.Remove("zaentrums", "zaentrum")
	if rec := link("https://media.example.org/portal/app/sample"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "knows no origin") {
		t.Errorf("absolute without an origin = %d %s", rec.Code, rec.Body)
	}
	if rec := link("/portal/app/sample"); rec.Code != http.StatusCreated || decodeAs[model.Notice](t, rec).Link != "/portal/app/sample" {
		t.Errorf("a path without an origin = %d %s", rec.Code, rec.Body)
	}
}

// A stored link or item that breaks the rules is not served: what a client
// follows is checked on the way out as on the way in.
func TestNoticeReadsServeOnlyLinksThatObeyTheRules(t *testing.T) {
	e := noticeEnv(t)
	for _, n := range []store.NewNotice{
		{Sub: "user-mia", Addon: "sample", Title: "javascript", Body: "b", Link: "javascript:alert(document.cookie)"},
		{Sub: "user-mia", Addon: "sample", Title: "protocol-relative", Body: "b", Link: "//elsewhere.example/x"},
		{Sub: "user-mia", Addon: "sample", Title: "a path item", Body: "b", ItemID: "../../etc"},
		{Sub: "user-mia", Addon: "sample", Title: "fine", Body: "b", Link: "https://media.example.org/portal/app/sample", ItemID: "item-1"},
	} {
		if _, _, err := e.notices.PostNotice(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]model.Notice{}
	for _, n := range e.listOf(mia).Notices {
		got[n.Title] = n
	}
	if got["javascript"].Link != "" || got["protocol-relative"].Link != "" || got["a path item"].ItemID != "" {
		t.Errorf("served %+v", got)
	}
	if f := got["fine"]; f.Link != "https://media.example.org/portal/app/sample" || f.ItemID != "item-1" || f.AddonTitle != "Sample Addon" || f.AddonIcon != "puzzle" {
		t.Errorf("fine = %+v", f)
	}
}

// An addon posts a hundred at once and then one a second; a person posting
// to themselves ten. One addon's limit is not another's, one person's not
// another's.
func TestNoticesAreLimited(t *testing.T) {
	e := noticeEnv(t)
	at := noticeEpoch
	clock := func() time.Time { return at }
	e.api.noticeAddon = ratelimit.New(noticeAddonBurst, noticeAddonEvery).WithClock(clock)
	e.api.noticePerson = ratelimit.New(noticePersonBurst, noticePersonEvery).WithClock(clock)
	for i := 0; i < noticeAddonBurst; i++ {
		if rec := e.do(addonSample, http.MethodPost, "/api/portal/notices", notice("user-mia")); rec.Code != http.StatusCreated {
			t.Fatalf("post %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := e.do(addonSample, http.MethodPost, "/api/portal/notices", notice("user-mia"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("one more = %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := e.do(addonOther, http.MethodPost, "/api/portal/notices", notice("user-mia")); rec.Code != http.StatusCreated {
		t.Errorf("another addon = %d", rec.Code)
	}
	at = at.Add(noticeAddonEvery)
	if rec := e.do(addonSample, http.MethodPost, "/api/portal/notices", notice("user-mia")); rec.Code != http.StatusCreated {
		t.Errorf("a second later = %d", rec.Code)
	}

	for i := 0; i < noticePersonBurst; i++ {
		if rec := e.do(miaShell, http.MethodPost, "/api/portal/me/notices", ownNotice("sample")); rec.Code != http.StatusCreated {
			t.Fatalf("mia's %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := e.do(mia, http.MethodPost, "/api/portal/me/notices", ownNotice("sample")); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "6" {
		t.Errorf("mia's one more = %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := e.do(leo, http.MethodPost, "/api/portal/me/notices", ownNotice("sample")); rec.Code != http.StatusCreated {
		t.Errorf("leo = %d", rec.Code)
	}
	// Mia keeps her newest hundred.
	if got := e.listOf(mia); len(got.Notices) != store.NoticesKept || got.Notices[0].Body != "From the console." {
		t.Errorf("mia keeps %d, newest %+v", len(got.Notices), got.Notices[0])
	}
}

// The counts say how many, unread, for how many people and the newest — per
// addon, never what a notice says nor whom it is for.
func TestNoticeCountsSayNoText(t *testing.T) {
	e := noticeEnv(t)
	for _, c := range []struct {
		claims map[string]any
		sub    string
	}{{addonSample, "user-mia"}, {addonSample, "user-mia"}, {addonSample, "user-leo"}, {addonOther, "user-mia"}} {
		if rec := e.do(c.claims, http.MethodPost, "/api/portal/notices", notice(c.sub, "title", "secret title", "body", "secret body")); rec.Code != http.StatusCreated {
			t.Fatal(rec.Body)
		}
	}
	e.do(leo, http.MethodPost, "/api/portal/me/notices/read-all", nil)
	type counts struct {
		Addons         []model.NoticeCount `json:"addons"`
		Kept           int                 `json:"kept"`
		RetentionHours int                 `json:"retentionHours"`
	}
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/notices", nil)
	got := decodeAs[counts](t, rec)
	if len(got.Addons) != 2 || got.Kept != store.NoticesKept || got.RetentionHours != 90*24 {
		t.Fatalf("counts = %s", rec.Body)
	}
	if s := got.Addons[1]; s.Addon != "sample" || s.Notices != 3 || s.Unread != 2 || s.People != 2 || s.Latest == nil {
		t.Errorf("sample = %+v", s)
	}
	for _, leak := range []string{"secret", "user-mia", "user-leo"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the counts carry %q: %s", leak, rec.Body)
		}
	}
	if got := decodeAs[counts](t, e.do(adminPortal, http.MethodGet, "/api/portal/notices?addon=other", nil)); len(got.Addons) != 1 || got.Addons[0].Notices != 1 {
		t.Errorf("other's = %+v", got)
	}
	if got := decodeAs[counts](t, e.do(adminPortal, http.MethodGet, "/api/portal/notices?addon=ghost", nil)); len(got.Addons) != 1 || got.Addons[0].Addon != "ghost" || got.Addons[0].Notices != 0 {
		t.Errorf("ghost's = %+v", got)
	}
	if rec := e.do(adminPortal, http.MethodGet, "/api/portal/notices?addon=Not%20One", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a key that is none = %d", rec.Code)
	}
}

// Every write is logged — who, which notice — and what a notice says never
// is.
func TestNoticeWritesAreLoggedWithoutWhatTheySay(t *testing.T) {
	e := noticeEnv(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	rec := e.do(addonSample, http.MethodPost, "/api/portal/notices",
		notice("user-mia", "title", "Wombat Title", "body", "Platypus body", "link", "/portal/app/sample?x=quokka", "itemId", "numbat-1"))
	posted := decodeAs[model.Notice](t, rec).ID
	rec = e.do(miaShell, http.MethodPost, "/api/portal/me/notices", ownNotice("sample", "title", "Echidna Title", "body", "Bilby body"))
	own := decodeAs[model.Notice](t, rec).ID
	e.do(mia, http.MethodPost, "/api/portal/me/notices/"+posted+"/read", nil)
	e.do(mia, http.MethodPost, "/api/portal/me/notices/read-all", nil)
	e.do(mia, http.MethodDelete, "/api/portal/me/notices/"+own, nil)

	logged := buf.String()
	for _, line := range []string{
		"notices: addon sample posted " + posted + " to user-mia",
		"notices: mia posted " + own + " to themselves, from addon sample",
		"notices: mia read " + posted,
		"notices: mia read all their notices (1)",
		"notices: mia deleted " + own,
	} {
		if !strings.Contains(logged, line) {
			t.Errorf("no %q in the log:\n%s", line, logged)
		}
	}
	for _, said := range []string{"Wombat", "Platypus", "quokka", "numbat", "Echidna", "Bilby"} {
		if strings.Contains(logged, said) {
			t.Errorf("the log carries what a notice says (%q):\n%s", said, logged)
		}
	}
}

// The sweep deletes what is past the retention — 90 days unless configured —
// and stops with its context.
func TestTheSweepKeepsTheRetention(t *testing.T) {
	e := noticeEnv(t)
	for i := 0; i < 3; i++ {
		e.do(addonSample, http.MethodPost, "/api/portal/notices", notice("user-mia"))
	}
	// The fake dates the notices a second apart from noticeEpoch.
	e.api.now = func() time.Time { return noticeEpoch.Add(90*24*time.Hour + 2*time.Second + time.Millisecond) }
	e.api.sweepNotices(context.Background())
	if left := e.notices.of("user-mia"); len(left) != 1 {
		t.Errorf("past 90 days: %d left, want 1", len(left))
	}
	e.api.cfg.NoticeRetention = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { e.api.RunNoticeSweep(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep does not stop with its context")
	}
	if left := e.notices.of("user-mia"); len(left) != 0 {
		t.Errorf("past an hour: %d left", len(left))
	}
}

// What a person reads carries where it is from — the addon's app — and
// nothing of whom it is for.
func TestServedNoticesSayWhereTheyAreFrom(t *testing.T) {
	e := noticeEnv(t)
	e.do(addonSample, http.MethodPost, "/api/portal/notices", notice("user-mia", "itemId", "item-7", "link", "/portal/app/sample#/ready"))
	rec := e.do(mia, http.MethodGet, "/api/portal/me/notices", nil)
	var raw struct {
		Notices []map[string]any `json:"notices"`
		Unread  int              `json:"unread"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw.Notices) != 1 || raw.Unread != 1 {
		t.Fatalf("%s", rec.Body)
	}
	n := raw.Notices[0]
	for k, want := range map[string]any{
		"addon": "sample", "addonTitle": "Sample Addon", "addonIcon": "puzzle", "title": "Your title is ready",
		"body": "It is in your library now.", "itemId": "item-7", "link": own + "/portal/app/sample#/ready", "readAt": nil,
	} {
		if n[k] != want {
			t.Errorf("%s = %v, want %v", k, n[k], want)
		}
	}
	if _, ok := n["sub"]; ok || strings.Contains(rec.Body.String(), "user-mia") {
		t.Errorf("a notice says whom it is for: %s", rec.Body)
	}
	if empty := e.do(leo, http.MethodGet, "/api/portal/me/notices", nil); strings.TrimSpace(empty.Body.String()) != `{"notices":[],"unread":0}` {
		t.Errorf("none = %s", empty.Body)
	}
}
