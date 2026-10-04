package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/db"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

func inviteStore(t *testing.T) *Store {
	t.Helper()
	st := testStore(t)
	for i := 0; i < 2; i++ { // every boot re-applies every migration
		if err := st.Migrate(context.Background(), db.Migrations, "zaentrum-admin"); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func hashOf(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// An invite opens for its token while it is neither used, revoked nor
// expired — and for nothing else, with the same error whatever the reason.
func TestInviteOpensOnlyWhileOpen(t *testing.T) {
	st := inviteStore(t)
	ctx := context.Background()
	now := time.Now()
	inv, err := st.CreateInvite(ctx, "user-mia", "admin", hashOf("mia-1"), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if inv.UserID != "user-mia" || inv.CreatedBy != "admin" || inv.Status(now) != model.InvitePending {
		t.Fatalf("made %+v", inv)
	}
	got, err := st.OpenInvite(ctx, hashOf("mia-1"), now)
	if err != nil || got.ID != inv.ID {
		t.Fatalf("open: %+v, %v", got, err)
	}
	for name, at := range map[string]time.Time{"after it expired": now.Add(time.Hour), "long after": now.Add(48 * time.Hour)} {
		if _, err := st.OpenInvite(ctx, hashOf("mia-1"), at); !errors.Is(err, ErrInviteClosed) {
			t.Errorf("%s: %v, want ErrInviteClosed", name, err)
		}
	}
	for _, other := range []string{"mia-2", "", "MIA-1"} {
		if _, err := st.OpenInvite(ctx, hashOf(other), now); !errors.Is(err, ErrInviteClosed) {
			t.Errorf("a token of no invite %q: %v", other, err)
		}
	}
	if _, err := st.OpenInvite(ctx, []byte("short"), now); !errors.Is(err, ErrInviteClosed) {
		t.Errorf("a hash that is no SHA-256: %v", err)
	}
}

// The store keeps the token's SHA-256, never the token: nothing in the table
// is the token, and a row that is no SHA-256 is refused.
func TestInvitesKeepNoToken(t *testing.T) {
	st := inviteStore(t)
	ctx := context.Background()
	token := "the-token-itself-never-stored"
	if _, err := st.CreateInvite(ctx, "user-mia", "admin", hashOf(token), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var found bool
	err := st.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM invites i WHERE row_to_json(i)::text LIKE '%' || $1 || '%')`, token).Scan(&found)
	if err != nil || found {
		t.Errorf("the token is in the table: %v, %v", found, err)
	}
	if _, err := st.CreateInvite(ctx, "user-mia", "admin", []byte(token), time.Now().Add(time.Hour)); err == nil {
		t.Error("a hash that is no SHA-256 was taken")
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO invites (token_hash, user_id, expires_at) VALUES ($1, 'u', now())`, []byte("x")); err == nil {
		t.Error("the table took a token_hash of 1 byte")
	}
}

// Used once: the password is set and the invite marked used together; a use
// that fails leaves it open; a second use finds it closed.
func TestAnInviteIsUsedOnce(t *testing.T) {
	st := inviteStore(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := st.CreateInvite(ctx, "user-mia", "admin", hashOf("t"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("the realm's policy refused the password")
	if err := st.UseInvite(ctx, hashOf("t"), now, func(model.Invite) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("a failed use: %v", err)
	}
	if _, err := st.OpenInvite(ctx, hashOf("t"), now); err != nil {
		t.Fatalf("a failed use closed the invite: %v", err)
	}
	var who string
	if err := st.UseInvite(ctx, hashOf("t"), now, func(inv model.Invite) error { who = inv.UserID; return nil }); err != nil || who != "user-mia" {
		t.Fatalf("use: %v (%s)", err, who)
	}
	called := false
	err := st.UseInvite(ctx, hashOf("t"), now, func(model.Invite) error { called = true; return nil })
	if !errors.Is(err, ErrInviteClosed) || called {
		t.Errorf("a second use: %v, called %v", err, called)
	}
	latest, err := st.LatestInvites(ctx)
	if err != nil || latest["user-mia"].Status(now) != model.InviteUsed || latest["user-mia"].UsedAt == nil {
		t.Errorf("latest: %+v, %v", latest, err)
	}
	// Expired, unused: no use.
	if _, err := st.CreateInvite(ctx, "user-leo", "admin", hashOf("old"), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.UseInvite(ctx, hashOf("old"), now, func(model.Invite) error { return nil }); !errors.Is(err, ErrInviteClosed) {
		t.Errorf("an expired invite was used: %v", err)
	}
}

// Two requests with the same token at once: one uses it, the other waits for
// it and finds it used.
func TestAnInviteUsedTwiceAtOnceIsUsedOnce(t *testing.T) {
	st := inviteStore(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := st.CreateInvite(ctx, "user-mia", "admin", hashOf("race"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	uses, closed := 0, 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := st.UseInvite(ctx, hashOf("race"), now, func(model.Invite) error {
				time.Sleep(30 * time.Millisecond) // the password being set
				return nil
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				uses++
			case errors.Is(err, ErrInviteClosed):
				closed++
			default:
				t.Errorf("use: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if uses != 1 || closed != 7 {
		t.Errorf("%d uses, %d closed — want 1 and 7", uses, closed)
	}
}

// A new invite revokes the person's open ones — only theirs; switching a
// person off revokes theirs; deleting a person removes their rows.
func TestANewInviteRevokesTheOldOnes(t *testing.T) {
	st := inviteStore(t)
	ctx := context.Background()
	now := time.Now()
	for i, token := range []string{"a1", "a2", "a3"} {
		if _, err := st.CreateInvite(ctx, "user-anna", "admin", hashOf(token), now.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateInvite(ctx, "user-leo", "admin", hashOf("l1"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"a1", "a2"} {
		if _, err := st.OpenInvite(ctx, hashOf(token), now); !errors.Is(err, ErrInviteClosed) {
			t.Errorf("%s still opens after a newer invite: %v", token, err)
		}
	}
	for _, token := range []string{"a3", "l1"} {
		if _, err := st.OpenInvite(ctx, hashOf(token), now); err != nil {
			t.Errorf("%s: %v", token, err)
		}
	}
	latest, _ := st.LatestInvites(ctx)
	if got := latest["user-anna"]; got.Status(now) != model.InvitePending || !got.ExpiresAt.Equal(now.Add(3*time.Hour).Truncate(time.Microsecond)) {
		t.Errorf("anna's latest: %+v", got)
	}
	if err := st.RevokeInvites(ctx, "user-leo"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenInvite(ctx, hashOf("l1"), now); !errors.Is(err, ErrInviteClosed) {
		t.Errorf("revoked, it opens: %v", err)
	}
	latest, _ = st.LatestInvites(ctx)
	if latest["user-leo"].Status(now) != model.InviteRevoked {
		t.Errorf("leo's latest: %s", latest["user-leo"].Status(now))
	}
	if err := st.DeleteInvites(ctx, "user-anna"); err != nil {
		t.Fatal(err)
	}
	latest, _ = st.LatestInvites(ctx)
	if _, ok := latest["user-anna"]; ok {
		t.Error("a deleted person's invites stay")
	}
	if _, ok := latest["user-leo"]; !ok {
		t.Error("deleting anna's invites took leo's")
	}
}

// Closed invites older than three months go when the next one is made; open
// and recent ones stay.
func TestOldInvitesArePruned(t *testing.T) {
	st := inviteStore(t)
	ctx := context.Background()
	now := time.Now()
	for i, age := range []time.Duration{100 * 24 * time.Hour, 10 * 24 * time.Hour} {
		if _, err := st.pool.Exec(ctx, `INSERT INTO invites (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			hashOf(fmt.Sprint("old", i)), fmt.Sprint("user-", i), now.Add(-age), now.Add(-age).Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateInvite(ctx, "user-new", "admin", hashOf("new"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	latest, _ := st.LatestInvites(ctx)
	if _, ok := latest["user-0"]; ok {
		t.Error("an invite closed 100 days ago stays")
	}
	if _, ok := latest["user-1"]; !ok {
		t.Error("an invite closed 10 days ago went")
	}
}
