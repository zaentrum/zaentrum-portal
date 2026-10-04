package people

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak"
)

// The People page against a real Keycloak: the platform chart's realm, with
// its people client and the secret the realm Job set. Opt in with
//
//	PORTAL_TEST_KEYCLOAK_URL=http://127.0.0.1:8080/auth
//	PORTAL_TEST_KEYCLOAK_SECRET=<Secret zaentrum-people, client-secret>
//
// It adds, changes and deletes a person of its own (it-mia), and leaves the
// realm as it found it. Without the variables it is skipped: CI runs the
// same rules against kctest.
func realKeycloak(t *testing.T) *Service {
	t.Helper()
	base, secret := os.Getenv("PORTAL_TEST_KEYCLOAK_URL"), os.Getenv("PORTAL_TEST_KEYCLOAK_SECRET")
	if base == "" || secret == "" {
		t.Skip("PORTAL_TEST_KEYCLOAK_URL / PORTAL_TEST_KEYCLOAK_SECRET not set — skipping the test against a real Keycloak")
	}
	kc := keycloak.New(keycloak.Config{URL: base, Realm: "zaentrum", ClientID: "zaentrum-people", ClientSecret: secret})
	return New(kc, "zaentrum-admin", "zaentrum-user", []string{"zaentrum-verify"})
}

func TestAgainstARealKeycloak(t *testing.T) {
	s := realKeycloak(t)
	ctx := context.Background()
	cleanup := func() {
		list, _ := s.List(ctx)
		for _, p := range list {
			if strings.HasPrefix(p.Username, "it-") {
				_ = s.Delete(ctx, p.ID)
			}
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var owner *Person
	for i := range list {
		if list[i].Username == "admin" {
			owner = &list[i]
		}
	}
	if owner == nil || owner.Role != RoleAdmin || owner.Managed != "keycloak" {
		t.Fatalf("the realm's first admin, as listed: %+v (all: %+v)", owner, list)
	}

	twelve := 12
	mia, err := s.Create(ctx, NewPerson{Username: "it-mia", DisplayName: "Mia", Role: RoleUser, MaxRating: &twelve})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if mia.MaxRating == nil || *mia.MaxRating != 12 || mia.Role != RoleUser || mia.Managed != "" || mia.CreatedAt == nil {
		t.Errorf("made %+v", mia)
	}
	if _, err := s.Create(ctx, NewPerson{Username: "it-mia", DisplayName: "Mia", Role: RoleUser}); !errors.Is(err, ErrUsernameUsed) {
		t.Errorf("twice: %v", err)
	}
	sixteen := 16
	p, err := s.Update(ctx, owner.ID, mia.ID, Change{DisplayName: ptr("Mia Sophie"), MaxRating: &sixteen})
	if err != nil || p.DisplayName != "Mia Sophie" || *p.MaxRating != 16 {
		t.Fatalf("update: %+v, %v", p, err)
	}
	if p, err = s.Update(ctx, owner.ID, mia.ID, Change{Role: ptr(RoleAdmin), ClearRating: true}); err != nil || p.Role != RoleAdmin || p.MaxRating != nil {
		t.Fatalf("made an admin: %+v, %v", p, err)
	}
	if p, err = s.Update(ctx, owner.ID, mia.ID, Change{Role: ptr(RoleUser), MaxRating: &twelve}); err != nil || p.Role != RoleUser || *p.MaxRating != 12 {
		t.Fatalf("a user again: %+v, %v", p, err)
	}
	var refused *PasswordRefused
	for _, pw := range []string{"short", "it-mia"} {
		if err := PasswordError(s.Accept(ctx, mia.ID, pw)); !errors.As(err, &refused) {
			t.Errorf("password %q: %v", pw, err)
		}
	}
	if err := s.Accept(ctx, mia.ID, "Sternschnuppe-42"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := s.CanInvite(ctx, mia.ID, owner.ID); err == nil {
		t.Error("the people client would re-invite the realm's administrator")
	}
	effective, err := s.kc.EffectiveRealmRoles(ctx, mia.ID)
	if err != nil || !slices.Contains(effective, "zaentrum-user") || slices.Contains(effective, "zaentrum-admin") {
		t.Errorf("roles %v, %v", effective, err)
	}
	if err := s.Delete(ctx, mia.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, mia.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}
