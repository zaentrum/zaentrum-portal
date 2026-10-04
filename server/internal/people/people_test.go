package people

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak/kctest"
)

func service(t *testing.T) (*Service, *kctest.Server) {
	t.Helper()
	kc := kctest.New(t)
	c := keycloak.New(keycloak.Config{URL: kc.Base(), Realm: kc.Realm, ClientID: kc.ClientID, ClientSecret: kc.ClientSecret})
	return New(c, kctest.AdminRole, kctest.UserRole, []string{"zaentrum-verify"}), kc
}

// What a person types on a TV: letters, digits and . _ - between them, in
// lower case, 3 to 64 of them.
func TestUsernames(t *testing.T) {
	for raw, want := range map[string]string{
		"mia": "mia", "  Mia  ": "mia", "anna-lena": "anna-lena", "leo.k": "leo.k", "j_2": "j_2",
	} {
		if got, err := CleanUsername(raw); err != nil || got != want {
			t.Errorf("%q: %q, %v, want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"", "ab", strings.Repeat("a", 65), "-mia", "mia.", "mi a", "mïa", "mia@home", "../x", "a/b"} {
		var inv *Invalid
		if _, err := CleanUsername(bad); !errors.As(err, &inv) {
			t.Errorf("%q: %v, want Invalid", bad, err)
		}
	}
}

// A name is what the page and the apps call a person: anything but what
// Keycloak refuses in a name.
func TestNames(t *testing.T) {
	for _, ok := range []string{"Mia", "Anna-Lena Müller", "O'Brien", "李雷"} {
		if got, err := CleanName("  " + ok + " "); err != nil || got != ok {
			t.Errorf("%q: %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "   ", "<script>", "a{b}", "tab\tname", strings.Repeat("x", 101)} {
		if _, err := CleanName(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

func TestRatings(t *testing.T) {
	for _, n := range []int{0, 6, 12, 16, 18, 21} {
		if err := CheckRating(n); err != nil {
			t.Errorf("%d: %v", n, err)
		}
	}
	for _, n := range []int{-1, 22, 99} {
		if err := CheckRating(n); err == nil {
			t.Errorf("%d was taken", n)
		}
	}
}

// The policy as portal-api reads Keycloak's: its rules, in a hint each, and
// a check before Keycloak is asked.
func TestPasswordPolicy(t *testing.T) {
	p := ParsePolicy("length(8) and notUsername and notEmail")
	if p.MinLength != 8 || !p.NotUsername || !p.NotEmail {
		t.Fatalf("%+v", p)
	}
	if !slices.Equal(p.Hints, []string{"At least 8 characters.", "Not your username."}) {
		t.Errorf("hints %q", p.Hints)
	}
	mia := Person{Username: "mia", Email: "mia@example.org"}
	for pw, ok := range map[string]bool{
		"": false, "short": false, "mia": false, "Mia": false, "mia@example.org": false,
		"long enough": true, "Sternschnuppe7": true, strings.Repeat("x", MaxPassword+1): false,
	} {
		if err := p.Check(pw, mia); (err == nil) != ok {
			t.Errorf("%q: %v", pw, err)
		}
	}
	strict := ParsePolicy("length(10) and digits(2) and upperCase(1) and specialChars(1) and maxLength(20)")
	if len(strict.Hints) != 4 {
		t.Errorf("hints %q", strict.Hints)
	}
	for pw, ok := range map[string]bool{"Abcdefghij12!": true, "abcdefghij12!": false, "Abcdefghij1!": false, "Abcdefghij12": false,
		"Abcdefghijklmnopqrst12!": false} {
		if err := strict.Check(pw, mia); (err == nil) != ok {
			t.Errorf("strict %q: %v", pw, err)
		}
	}
	if d := ParsePolicy(""); d.MinLength != 8 {
		t.Errorf("no policy: %+v", d)
	}
}

// Keycloak's refusal of a password, in words: the rule and its number.
func TestPasswordErrors(t *testing.T) {
	for _, c := range []struct {
		inv  keycloak.Invalid
		want string
	}{
		{keycloak.Invalid{Field: "password", Code: "invalidPasswordMinLengthMessage", Description: "Invalid password: minimum length 12."},
			"The password needs at least 12 characters."},
		{keycloak.Invalid{Field: "password", Code: "invalidPasswordHistoryMessage", Description: "Invalid password: must not be equal to any of last 3 passwords."},
			"The password must differ from the last 3 used."},
		{keycloak.Invalid{Field: "password", Code: "invalidPasswordNotUsernameMessage"}, "The password may not be your username."},
		{keycloak.Invalid{Field: "password", Code: "somethingNewMessage", Description: "Invalid password: at java.lang.Thread"},
			"This server's password policy does not take that password: choose another."},
	} {
		var refused *PasswordRefused
		inv := c.inv
		if err := PasswordError(&inv); !errors.As(err, &refused) || refused.Message != c.want {
			t.Errorf("%s: %v, want %q", c.inv.Code, err, c.want)
		}
	}
	other := errors.New("keycloak did not answer")
	if PasswordError(other) != other {
		t.Error("an error that is no refusal of the password was reworded")
	}
}

// The rules of the page, against the realm: the last admin stays an enabled
// admin, nobody changes their own role or switches themselves off, a
// Keycloak administrator is Keycloak's to change, and the platform's own
// accounts are no people.
func TestTheRules(t *testing.T) {
	s, kc := service(t)
	ctx := context.Background()
	owner := kc.Add(kctest.Seed{ID: "user-admin", Username: "admin", FirstName: "Zaentrum", LastName: "Administrator",
		Roles: []string{kctest.AdminRole, kctest.UserRole}, RealmManagement: []string{"realm-admin"}, Imported: true})
	lena := kc.Add(kctest.Seed{ID: "user-lena", Username: "lena", FirstName: "Lena", Roles: []string{kctest.AdminRole, kctest.UserRole}})
	kc.Add(kctest.Seed{Username: "zaentrum-verify", FirstName: "Zaentrum", LastName: "Verification", Roles: []string{kctest.UserRole}})

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Username != "lena" || list[1].Username != "admin" {
		t.Fatalf("people %+v: the verification account is no person", list)
	}
	if list[1].Managed != "keycloak" || list[1].Role != RoleAdmin || list[1].CreatedAt != nil || list[1].DisplayName != "Zaentrum Administrator" {
		t.Errorf("the first admin: %+v", list[1])
	}

	// Lena is an admin besides the owner — but the owner is Keycloak's.
	var refused *Refused
	if _, err := s.Update(ctx, "user-lena", owner, Change{Enabled: ptr(false)}); !errors.As(err, &refused) {
		t.Errorf("switching off a Keycloak administrator: %v", err)
	}
	if _, err := s.CanInvite(ctx, "user-lena", owner); !errors.As(err, &refused) {
		t.Errorf("inviting a Keycloak administrator (a password reset): %v", err)
	}
	if _, err := s.CanDelete(ctx, "user-lena", owner); !errors.As(err, &refused) {
		t.Errorf("deleting a Keycloak administrator: %v", err)
	}
	// Lena cannot demote herself; the owner can.
	if _, err := s.Update(ctx, "user-lena", lena, Change{Role: ptr(RoleUser)}); !errors.As(err, &refused) {
		t.Errorf("demoting yourself: %v", err)
	}
	if p, err := s.Update(ctx, owner, lena, Change{Role: ptr(RoleUser)}); err != nil || p.Role != RoleUser {
		t.Fatalf("demoting lena: %+v, %v", p, err)
	}
	if slices.Contains(kc.RealmRoles(lena), kctest.AdminRole) || !slices.Contains(kc.RealmRoles(lena), kctest.UserRole) {
		t.Errorf("lena's roles: %v", kc.RealmRoles(lena))
	}
	// The owner is the last enabled admin now; a second admin switched off
	// does not count.
	off := kc.Add(kctest.Seed{Username: "off", FirstName: "Off", Disabled: true, Roles: []string{kctest.AdminRole}})
	plain := kc.Add(kctest.Seed{Username: "plain", FirstName: "Plain", Roles: []string{kctest.AdminRole, kctest.UserRole}})
	if _, err := s.Update(ctx, owner, plain, Change{Role: ptr(RoleUser)}); err != nil {
		t.Fatalf("demoting plain while the owner is an admin: %v", err)
	}
	if _, err := s.Update(ctx, owner, plain, Change{Role: ptr(RoleAdmin)}); err != nil {
		t.Fatal(err)
	}
	_ = off
	// Without the owner, plain is the last enabled admin.
	if err := s.Delete(ctx, owner); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]Change{"demoted": {Role: ptr(RoleUser)}, "switched off": {Enabled: ptr(false)}} {
		if _, err := s.Update(ctx, "user-lena", plain, ch); !errors.As(err, &refused) || !strings.Contains(refused.Message, "last admin") {
			t.Errorf("the last admin %s: %v", name, err)
		}
	}
	if _, err := s.CanDelete(ctx, "user-lena", plain); !errors.As(err, &refused) || !strings.Contains(refused.Message, "last admin") {
		t.Errorf("deleting the last admin: %v", err)
	}
	if _, err := s.CanDeleteOwn(ctx, plain); !errors.As(err, &refused) {
		t.Errorf("the last admin deleting their own account: %v", err)
	}

	// The platform's own account: not found, whatever is tried.
	verify := kc.UserByName("zaentrum-verify")
	if _, err := s.Get(ctx, verify); !errors.Is(err, ErrNotFound) {
		t.Errorf("the verification account: %v", err)
	}
	if _, err := s.Create(ctx, NewPerson{Username: "zaentrum-verify", DisplayName: "X", Role: RoleUser}); !errors.Is(err, ErrUsernameUsed) {
		t.Errorf("a person named as the platform's own account: %v", err)
	}
}

// The admin role through a group stays, whatever the page unmaps: it says so.
func TestAnAdminThroughAGroup(t *testing.T) {
	s, kc := service(t)
	ctx := context.Background()
	kc.Add(kctest.Seed{ID: "user-owner", Username: "owner", FirstName: "Owner", Roles: []string{kctest.AdminRole, kctest.UserRole}})
	grp := kc.Add(kctest.Seed{Username: "grp", FirstName: "Grp", Roles: []string{kctest.UserRole}, GroupRoles: []string{kctest.AdminRole}})
	var refused *Refused
	if _, err := s.Update(ctx, "user-owner", grp, Change{Role: ptr(RoleUser)}); !errors.As(err, &refused) || !strings.Contains(refused.Message, "group") {
		t.Errorf("demoting an admin through a group: %v", err)
	}
}

// A person is made whole or not at all: with their roles and their cap, and
// a cap Keycloak dropped — its user profile does not declare the attribute —
// removes the person again.
func TestCreate(t *testing.T) {
	s, kc := service(t)
	ctx := context.Background()
	twelve := 12
	p, err := s.Create(ctx, NewPerson{Username: "Mia", DisplayName: "Mia", Role: RoleUser, MaxRating: &twelve})
	if err != nil {
		t.Fatal(err)
	}
	if p.Username != "mia" || p.Role != RoleUser || p.MaxRating == nil || *p.MaxRating != 12 || !p.Enabled || p.CreatedAt == nil {
		t.Errorf("made %+v", p)
	}
	if kc.Password(p.ID) != "" {
		t.Error("a person is made with a password")
	}
	admin, err := s.Create(ctx, NewPerson{Username: "lukas", DisplayName: "Lukas", Role: RoleAdmin})
	if err != nil || admin.Role != RoleAdmin || !slices.Contains(kc.RealmRoles(admin.ID), kctest.UserRole) {
		t.Fatalf("an admin: %+v, %v, %v", admin, err, kc.RealmRoles(admin.ID))
	}
	var inv *Invalid
	if _, err := s.Create(ctx, NewPerson{Username: "boss", DisplayName: "Boss", Role: RoleAdmin, MaxRating: &twelve}); !errors.As(err, &inv) {
		t.Errorf("an admin with a cap: %v", err)
	}
	if _, err := s.Create(ctx, NewPerson{Username: "mia", DisplayName: "Other Mia", Role: RoleUser}); !errors.Is(err, ErrUsernameUsed) {
		t.Errorf("a username taken: %v", err)
	}
	kc.DeclareRating = false
	if _, err := s.Create(ctx, NewPerson{Username: "leo", DisplayName: "Leo", Role: RoleUser, MaxRating: &twelve}); !errors.Is(err, ErrRatingNotKept) {
		t.Errorf("a cap Keycloak dropped: %v", err)
	}
	if id := kc.UserByName("leo"); id != "" {
		t.Error("the person whose cap was dropped stayed, uncapped")
	}
}

func ptr[T any](v T) *T { return &v }
