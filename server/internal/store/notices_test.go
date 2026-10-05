package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/db"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// noticeStore is a migrated store with the addons sample and other
// installed.
func noticeStore(t *testing.T) *Store {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // every boot re-applies every migration
		if err := st.Migrate(ctx, db.Migrations, "zaentrum-admin"); err != nil {
			t.Fatal(err)
		}
	}
	installNoticeAddon(t, st, "sample", "Sample Addon")
	installNoticeAddon(t, st, "other", "Other Addon")
	return st
}

func installNoticeAddon(t *testing.T, st *Store, key, title string) {
	t.Helper()
	if err := st.InstallAddon(context.Background(), AddonInstall{
		App:   model.App{Key: key, Title: title, Icon: "puzzle", Kind: "tool", Enabled: true, BaseURL: "/portal/app/" + key, ProxyURL: "http://" + key},
		Addon: model.Addon{Key: key, Address: "http://" + key, Version: "1.0.0"},
	}); err != nil {
		t.Fatalf("install %s: %v", key, err)
	}
}

func post(t *testing.T, st *Store, sub, addon, title string) model.Notice {
	t.Helper()
	n, _, err := st.PostNotice(context.Background(), NewNotice{Sub: sub, Addon: addon, Title: title, Body: "body of " + title})
	if err != nil {
		t.Fatalf("post %q for %s: %v", title, sub, err)
	}
	return n
}

func titles(ns []model.Notice) string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Title)
	}
	return strings.Join(out, ",")
}

// A person reads their own notices, newest first, and nobody else's; what
// they mark read or delete is theirs, and a notice of someone else's is as
// one there is not — and stays as it was.
func TestNoticesAreTheirPersonsOwn(t *testing.T) {
	st := noticeStore(t)
	ctx := context.Background()
	first := post(t, st, "user-mia", "sample", "first")
	second := post(t, st, "user-mia", "other", "second")
	leos := post(t, st, "user-leo", "sample", "leo's")

	if first.AddonTitle != "Sample Addon" || first.AddonIcon != "puzzle" || first.ReadAt != nil || first.CreatedAt.IsZero() ||
		!noticeID.MatchString(first.ID) || first.Body != "body of first" {
		t.Errorf("posted %+v", first)
	}
	// Served in UTC, whatever zone portal-api runs in.
	if first.CreatedAt.Location() != time.UTC {
		t.Errorf("created at %v, in %v", first.CreatedAt, first.CreatedAt.Location())
	}
	mine, unread, err := st.Notices(ctx, "user-mia", 0)
	if err != nil || titles(mine) != "second,first" || unread != 2 {
		t.Fatalf("mia's = %q, %d unread, %v", titles(mine), unread, err)
	}
	if mine[0].AddonTitle != "Other Addon" {
		t.Errorf("from %q", mine[0].AddonTitle)
	}

	// Leo, with mia's ids: nothing of hers changes.
	if _, err := st.ReadNotice(ctx, "user-leo", first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("leo reads mia's: %v", err)
	}
	if err := st.DeleteNotice(ctx, "user-leo", first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("leo deletes mia's: %v", err)
	}
	if n, err := st.ReadAllNotices(ctx, "user-leo"); err != nil || n != 1 {
		t.Errorf("leo reads all of his: %d, %v", n, err)
	}
	if _, unread, _ := st.Notices(ctx, "user-mia", 0); unread != 2 {
		t.Fatalf("leo's reads changed mia's: %d unread", unread)
	}
	if theirs, _, _ := st.Notices(ctx, "user-leo", 0); len(theirs) != 1 || theirs[0].ID != leos.ID || theirs[0].ReadAt == nil {
		t.Errorf("leo's = %+v", theirs)
	}

	// Mia reads hers: once — reading again keeps when it was read first.
	if n, err := st.ReadNotice(ctx, "user-mia", first.ID); err != nil || n != 1 {
		t.Fatalf("read: %d unread, %v", n, err)
	}
	read, _, _ := st.Notices(ctx, "user-mia", 0)
	at := read[1].ReadAt
	if at == nil {
		t.Fatal("not read")
	}
	if at.Location() != time.UTC {
		t.Errorf("read at %v, in %v", at, at.Location())
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := st.ReadNotice(ctx, "user-mia", first.ID); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := st.Notices(ctx, "user-mia", 0); !again[1].ReadAt.Equal(*at) {
		t.Errorf("read again: %v, first read %v", again[1].ReadAt, at)
	}
	if n, err := st.ReadAllNotices(ctx, "user-mia"); err != nil || n != 1 {
		t.Errorf("read all: %d, %v", n, err)
	}
	if err := st.DeleteNotice(ctx, "user-mia", second.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNotice(ctx, "user-mia", second.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted twice: %v", err)
	}
	if left, unread, _ := st.Notices(ctx, "user-mia", 0); titles(left) != "first" || unread != 0 {
		t.Errorf("left = %q, %d unread", titles(left), unread)
	}

	// An id that is no notice's never reaches the database as one.
	for _, id := range []string{"", "1", "not-a-uuid", strings.ToUpper(first.ID), first.ID + "'; DROP TABLE notices; --"} {
		if _, err := st.ReadNotice(ctx, "user-mia", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("read %q: %v", id, err)
		}
		if err := st.DeleteNotice(ctx, "user-mia", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete %q: %v", id, err)
		}
	}
	if limited, _, _ := st.Notices(ctx, "user-leo", 1); len(limited) != 1 {
		t.Errorf("limit 1: %d", len(limited))
	}
}

// A person keeps their newest hundred: each one beyond goes as a new one
// comes — only theirs.
func TestAPersonKeepsTheirNewestNotices(t *testing.T) {
	st := noticeStore(t)
	ctx := context.Background()
	for i := 0; i < NoticesKept; i++ {
		post(t, st, "user-mia", "sample", fmt.Sprint("n", i))
	}
	post(t, st, "user-leo", "sample", "leo's")
	for i := NoticesKept; i < NoticesKept+5; i++ {
		_, pruned, err := st.PostNotice(ctx, NewNotice{Sub: "user-mia", Addon: "sample", Title: fmt.Sprint("n", i), Body: "b"})
		if err != nil || pruned != 1 {
			t.Fatalf("post %d: pruned %d, %v", i, pruned, err)
		}
	}
	mine, unread, err := st.Notices(ctx, "user-mia", 0)
	if err != nil || len(mine) != NoticesKept || unread != NoticesKept {
		t.Fatalf("mia keeps %d (%d unread), %v", len(mine), unread, err)
	}
	if mine[0].Title != "n104" || mine[len(mine)-1].Title != "n5" {
		t.Errorf("kept from %s to %s, want n104 to n5", mine[0].Title, mine[len(mine)-1].Title)
	}
	if theirs, _, _ := st.Notices(ctx, "user-leo", 0); titles(theirs) != "leo's" {
		t.Errorf("leo's = %q", titles(theirs))
	}
}

// Notices arriving at once keep the cap between them.
func TestNoticesArrivingAtOnceKeepTheCap(t *testing.T) {
	st := noticeStore(t)
	ctx := context.Background()
	for i := 0; i < NoticesKept-5; i++ {
		post(t, st, "user-mia", "sample", fmt.Sprint("n", i))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, _, err := st.PostNotice(ctx, NewNotice{Sub: "user-mia", Addon: "other", Title: fmt.Sprint("at once ", i), Body: "b"}); err != nil {
				t.Errorf("post: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM notices WHERE sub = 'user-mia'`).Scan(&n); err != nil || n != NoticesKept {
		t.Errorf("mia has %d, %v — want %d", n, err, NoticesKept)
	}
}

// Removing an addon removes its notices — a refresh keeps them — and
// deleting a person deletes theirs, nobody else's.
func TestNoticesGoWithTheirAddonAndTheirPerson(t *testing.T) {
	st := noticeStore(t)
	ctx := context.Background()
	post(t, st, "user-mia", "sample", "from sample")
	post(t, st, "user-mia", "other", "from other")
	post(t, st, "user-leo", "sample", "leo's from sample")
	post(t, st, "user-leo", "other", "leo's from other")

	installNoticeAddon(t, st, "sample", "Sample Addon, refreshed")
	if mine, _, _ := st.Notices(ctx, "user-mia", 0); len(mine) != 2 || mine[1].AddonTitle != "Sample Addon, refreshed" {
		t.Fatalf("after a refresh: %+v", mine)
	}
	if _, err := st.RemoveAddon(ctx, "sample", ""); err != nil {
		t.Fatal(err)
	}
	if mine, _, _ := st.Notices(ctx, "user-mia", 0); titles(mine) != "from other" {
		t.Errorf("mia's after sample is removed: %q", titles(mine))
	}
	if n, err := st.DeletePersonNotices(ctx, "user-mia"); err != nil || n != 1 {
		t.Errorf("mia deleted: %d, %v", n, err)
	}
	if mine, unread, _ := st.Notices(ctx, "user-mia", 0); len(mine) != 0 || unread != 0 {
		t.Errorf("mia's after her deletion: %q", titles(mine))
	}
	if theirs, _, _ := st.Notices(ctx, "user-leo", 0); titles(theirs) != "leo's from other" {
		t.Errorf("leo's = %q", titles(theirs))
	}
}

// A notice names an installed addon; and the table keeps the lengths the
// API checks, whoever writes to it.
func TestNoticesKeepTheirRules(t *testing.T) {
	st := noticeStore(t)
	ctx := context.Background()
	if _, _, err := st.PostNotice(ctx, NewNotice{Sub: "user-mia", Addon: "ghost", Title: "t", Body: "b"}); !errors.Is(err, ErrNoAddon) {
		t.Errorf("an addon not installed: %v", err)
	}
	for name, n := range map[string]NewNotice{
		"no person":          {Addon: "sample", Title: "t", Body: "b"},
		"no title":           {Sub: "user-mia", Addon: "sample", Body: "b"},
		"no body":            {Sub: "user-mia", Addon: "sample", Title: "t"},
		"a title of 81":      {Sub: "user-mia", Addon: "sample", Title: strings.Repeat("é", 81), Body: "b"},
		"a body of 281":      {Sub: "user-mia", Addon: "sample", Title: "t", Body: strings.Repeat("é", 281)},
		"an item id of 129":  {Sub: "user-mia", Addon: "sample", Title: "t", Body: "b", ItemID: strings.Repeat("a", 129)},
		"a subject over 255": {Sub: strings.Repeat("u", 256), Addon: "sample", Title: "t", Body: "b"},
	} {
		if _, _, err := st.PostNotice(ctx, n); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	// Characters, not bytes: 80 of them fit.
	if _, _, err := st.PostNotice(ctx, NewNotice{Sub: "user-mia", Addon: "sample", Title: strings.Repeat("é", 80), Body: strings.Repeat("é", 280)}); err != nil {
		t.Errorf("80 and 280 characters: %v", err)
	}
}

// The sweep deletes what is older than the retention, and nothing newer.
func TestOldNoticesAreSwept(t *testing.T) {
	st := noticeStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, age := range []time.Duration{100 * 24 * time.Hour, 91 * 24 * time.Hour, 89 * 24 * time.Hour, time.Hour} {
		exec(t, st, `INSERT INTO notices (sub, addon, title, body, created_at) VALUES ('user-mia', 'sample', $1, 'b', $2)`,
			age.String(), now.Add(-age))
	}
	n, err := st.SweepNotices(ctx, now.Add(-90*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("swept %d, %v — want 2", n, err)
	}
	if left, _, _ := st.Notices(ctx, "user-mia", 0); titles(left) != "1h0m0s,2136h0m0s" {
		t.Errorf("left = %q", titles(left))
	}
}

// The counts say how many, unread, for how many people and the newest —
// per addon, or for the one asked about.
func TestNoticeCounts(t *testing.T) {
	st := noticeStore(t)
	ctx := context.Background()
	post(t, st, "user-mia", "sample", "a")
	read := post(t, st, "user-mia", "sample", "b")
	post(t, st, "user-leo", "sample", "c")
	latest := post(t, st, "user-leo", "other", "d")
	if _, err := st.ReadNotice(ctx, "user-mia", read.ID); err != nil {
		t.Fatal(err)
	}
	all, err := st.NoticeCounts(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("counts = %+v, %v", all, err)
	}
	if c := all[1]; c.Addon != "sample" || c.Notices != 3 || c.Unread != 2 || c.People != 2 || c.Latest == nil {
		t.Errorf("sample = %+v", c)
	}
	if c := all[0]; c.Addon != "other" || c.Notices != 1 || c.Unread != 1 || c.People != 1 || !c.Latest.Equal(latest.CreatedAt) || c.Latest.Location() != time.UTC {
		t.Errorf("other = %+v (latest %v)", c, latest.CreatedAt)
	}
	if one, err := st.NoticeCounts(ctx, "other"); err != nil || len(one) != 1 || one[0].Addon != "other" {
		t.Errorf("other's = %+v, %v", one, err)
	}
	if none, err := st.NoticeCounts(ctx, "ghost"); err != nil || len(none) != 0 {
		t.Errorf("ghost's = %+v, %v", none, err)
	}
}
