package keycloak_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak/kctest"
)

func client(kc *kctest.Server, secret string) *keycloak.Client {
	return keycloak.New(keycloak.Config{URL: kc.Base(), Realm: kc.Realm, ClientID: kc.ClientID, ClientSecret: secret})
}

// The people client signs in with the client credentials grant, keeps its
// token for the calls after, and takes a new one when Keycloak no longer
// takes the old — a restarted Keycloak — once.
func TestTheClientSignsInAsThePeopleClient(t *testing.T) {
	kc := kctest.New(t)
	kc.Add(kctest.Seed{Username: "mia", FirstName: "Mia"})
	c := client(kc, kc.ClientSecret)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.Users(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(kc.CallsMatching("GET /users")); n != 3 {
		t.Errorf("%d listings, want 3", n)
	}
	kc.Expire()
	users, err := c.Users(ctx)
	if err != nil || len(users) != 1 || users[0].Username() != "mia" {
		t.Fatalf("after Keycloak forgot the token: %v, %v", users, err)
	}

	// A secret Keycloak does not take, or none at all, is a refusal, and no
	// admin call goes out with it.
	for _, secret := range []string{"wrong", ""} {
		before := len(kc.CallsMatching(""))
		_, err := client(kc, secret).Users(ctx)
		if !errors.Is(err, keycloak.ErrRefused) {
			t.Errorf("secret %q: %v, want ErrRefused", secret, err)
		}
		if len(kc.CallsMatching("")) != before {
			t.Errorf("secret %q: an admin call went out", secret)
		}
	}
	if client(kc, "").Configured() {
		t.Error("a client without a secret says it is configured")
	}
}

// Every refusal is one of the package's errors — never Keycloak's own words,
// which can carry its internals.
func TestRefusalsAreThePackagesErrors(t *testing.T) {
	kc := kctest.New(t)
	c := client(kc, kc.ClientSecret)
	ctx := context.Background()
	mia := kc.Add(kctest.Seed{Username: "mia", FirstName: "Mia"})

	if _, err := c.Create(ctx, keycloak.NewUser{Username: "mia", FirstName: "Mia"}); !errors.Is(err, keycloak.ErrConflict) {
		t.Errorf("a username taken: %v, want ErrConflict", err)
	}
	if _, err := c.User(ctx, "no-such-id"); !errors.Is(err, keycloak.ErrNotFound) {
		t.Errorf("no such user: %v", err)
	}
	for _, id := range []string{"../../clients", "a/b", "", ".."} {
		if _, err := c.User(ctx, id); !errors.Is(err, keycloak.ErrNotFound) {
			t.Errorf("an id %q that would change the path: %v", id, err)
		}
	}
	var inv *keycloak.Invalid
	_, err := c.Create(ctx, keycloak.NewUser{Username: "kid", FirstName: "Kid", Attributes: map[string]string{"max_rating": "22"}})
	if !errors.As(err, &inv) || inv.Field != "max_rating" || inv.Code != "error-number-out-of-range" {
		t.Errorf("a cap out of range: %v", err)
	}
	err = c.SetPassword(ctx, mia, "short")
	if !errors.As(err, &inv) || inv.Field != "password" || inv.Code != "invalidPasswordMinLengthMessage" {
		t.Errorf("a password the policy refuses: %#v", err)
	}
	kc.Fail = http.StatusInternalServerError
	_, err = c.Users(ctx)
	var failed *keycloak.Failed
	if !errors.As(err, &failed) || failed.Status != 500 {
		t.Fatalf("a Keycloak that fails: %v", err)
	}
	if strings.Contains(err.Error(), "java") || strings.Contains(fmt.Sprintf("%+v", err), "Thread") {
		t.Errorf("the error carries Keycloak's internals: %v", err)
	}
	kc.Fail = http.StatusForbidden
	if _, err := c.Users(ctx); !errors.Is(err, keycloak.ErrRefused) {
		t.Errorf("403: %v, want ErrRefused", err)
	}
	kc.Fail = 0

	// What the people client may not do, Keycloak refuses it.
	if err := c.AddRealmRoles(ctx, mia, []keycloak.Role{{ID: "made-up", Name: "zaentrum-admin"}}); !errors.Is(err, keycloak.ErrNotFound) {
		t.Errorf("a role of a made-up id: %v", err)
	}
}

// An update writes the user back whole: Keycloak replaces what it is sent,
// so a field the client did not read would be lost.
func TestAnUpdateKeepsWhatItDoesNotChange(t *testing.T) {
	kc := kctest.New(t)
	c := client(kc, kc.ClientSecret)
	ctx := context.Background()
	id := kc.Add(kctest.Seed{Username: "anna", FirstName: "Anna", LastName: "Muster", Email: "anna@example.org", MaxRating: "12"})
	u, err := c.User(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	u.SetEnabled(false)
	if err := c.Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	after := kc.User(id)
	if after["enabled"] != false || after["firstName"] != "Anna" || after["lastName"] != "Muster" || after["email"] != "anna@example.org" {
		t.Errorf("after switching off: %v", after)
	}
	if v, ok := (&keycloakUser{after}).rating(); !ok || v != "12" {
		t.Errorf("the rating cap went: %v", after["attributes"])
	}
}

type keycloakUser struct{ rep map[string]any }

func (u *keycloakUser) rating() (string, bool) {
	attrs, _ := u.rep["attributes"].(map[string]any)
	vals, _ := attrs["max_rating"].([]any)
	if len(vals) == 0 {
		return "", false
	}
	return fmt.Sprint(vals[0]), true
}

// A listing reads every page.
func TestUsersReadsEveryPage(t *testing.T) {
	kc := kctest.New(t)
	for i := 0; i < 230; i++ {
		kc.Add(kctest.Seed{Username: fmt.Sprintf("u%03d", i)})
	}
	users, err := client(kc, kc.ClientSecret).Users(context.Background())
	if err != nil || len(users) != 230 {
		t.Fatalf("%d users, %v", len(users), err)
	}
	if n := len(kc.CallsMatching("GET /users")); n != 3 {
		t.Errorf("%d pages read, want 3", n)
	}
}

// R.C4: a user is made with an address, or none; an update sets it or removes
// it; Keycloak's refusals of an address are the package's errors: another
// user's, in a realm that lets no two users have one, and one its user
// profile's validator does not take.
func TestRC4AnAddress(t *testing.T) {
	kc := kctest.New(t)
	c := client(kc, kc.ClientSecret)
	ctx := context.Background()
	kc.Add(kctest.Seed{Username: "anna", FirstName: "Anna", Email: "anna@example.org"})

	t.Run("R.C4 made with an address, or none", func(t *testing.T) {
		mia, err := c.Create(ctx, keycloak.NewUser{Username: "mia", FirstName: "Mia", Email: "mia@example.org"})
		if err != nil || kc.User(mia)["email"] != "mia@example.org" {
			t.Fatalf("made %v: %v", kc.User(mia), err)
		}
		leo, err := c.Create(ctx, keycloak.NewUser{Username: "leo", FirstName: "Leo"})
		if err != nil || kc.User(leo)["email"] != nil {
			t.Errorf("made without one: %v, %v", kc.User(leo), err)
		}
	})
	t.Run("R.C4 an update sets it, and removes it", func(t *testing.T) {
		id := kc.UserByName("leo")
		u, err := c.User(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		u.SetEmail("leo@example.org")
		if err := c.Update(ctx, u); err != nil || kc.User(id)["email"] != "leo@example.org" || kc.User(id)["firstName"] != "Leo" {
			t.Fatalf("set: %v, %v", kc.User(id), err)
		}
		// An empty address removes it: Keycloak keeps no key for it then.
		u.SetEmail("")
		if err := c.Update(ctx, u); err != nil || kc.User(id)["email"] != nil {
			t.Errorf("removed: %v, %v", kc.User(id), err)
		}
	})
	t.Run("R.C4 another user's address", func(t *testing.T) {
		if _, err := c.Create(ctx, keycloak.NewUser{Username: "kim", FirstName: "Kim", Email: "ANNA@example.org"}); !errors.Is(err, keycloak.ErrEmailConflict) {
			t.Errorf("made with anna's: %v, want ErrEmailConflict", err)
		}
		u, _ := c.User(ctx, kc.UserByName("mia"))
		u.SetEmail("anna@example.org")
		if err := c.Update(ctx, u); !errors.Is(err, keycloak.ErrEmailConflict) {
			t.Errorf("changed to anna's: %v, want ErrEmailConflict", err)
		}
		// A username taken stays ErrConflict.
		if _, err := c.Create(ctx, keycloak.NewUser{Username: "anna", FirstName: "Anna", Email: "other@example.org"}); !errors.Is(err, keycloak.ErrConflict) {
			t.Errorf("a username taken: %v, want ErrConflict", err)
		}
	})
	t.Run("R.C4 an address the user profile does not take", func(t *testing.T) {
		var inv *keycloak.Invalid
		_, err := c.Create(ctx, keycloak.NewUser{Username: "kim", FirstName: "Kim", Email: strings.Repeat("k", 65) + "@example.org"})
		if !errors.As(err, &inv) || inv.Field != "email" || inv.Code != "error-invalid-email" || len(inv.Params) != 2 || inv.Params[0] != "email" {
			t.Errorf("a local part of 65 characters: %v", err)
		}
		if kc.UserByName("kim") != "" {
			t.Error("kim was made")
		}
	})
}

// R.C4: Keycloak 26.0.7 answers a write that would take what another user has
// with a 409 in one of two texts: "User exists with same email" — on a create
// and a change alike, and on a create that would take both — and "User exists
// with same username". A 409 in any other words, as two writes at once may be
// answered, is looked up: the address where another user has it, else the
// username, else the refusal as it came.
func TestRC4TheProvidersRefusalOfATakenAddress(t *testing.T) {
	kc := kctest.New(t)
	c := client(kc, kc.ClientSecret)
	ctx := context.Background()
	kc.Add(kctest.Seed{Username: "anna", FirstName: "Anna", Email: "anna@example.org"})
	mia, err := c.Create(ctx, keycloak.NewUser{Username: "mia", FirstName: "Mia", Email: "mia@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	changeTo := func(address string) error {
		u, err := c.User(ctx, mia)
		if err != nil {
			t.Fatal(err)
		}
		u.SetEmail(address)
		return c.Update(ctx, u)
	}
	t.Run("R.C4 Keycloak's own two texts", func(t *testing.T) {
		for name, w := range map[string]struct {
			err  error
			want error
		}{
			"an address taken at a create": {errOf(c.Create(ctx, keycloak.NewUser{Username: "kim", FirstName: "Kim", Email: "anna@example.org"})), keycloak.ErrEmailConflict},
			"a username taken":             {errOf(c.Create(ctx, keycloak.NewUser{Username: "anna", FirstName: "Other", Email: "other@example.org"})), keycloak.ErrConflict},
			"both taken: the address":      {errOf(c.Create(ctx, keycloak.NewUser{Username: "anna", FirstName: "Other", Email: "anna@example.org"})), keycloak.ErrEmailConflict},
			"an address taken at a change": {changeTo("anna@example.org"), keycloak.ErrEmailConflict},
		} {
			if !errors.Is(w.err, w.want) {
				t.Errorf("%s: %v, want %v", name, w.err, w.want)
			}
		}
		if kc.UserByName("kim") != "" || kc.User(mia)["email"] != "mia@example.org" {
			t.Errorf("a refused write was kept: kim %q, mia %v", kc.UserByName("kim"), kc.User(mia)["email"])
		}
	})
	t.Run("R.C4 a 409 in other words is looked up", func(t *testing.T) {
		const other = "User exists with same username or email"
		// Another write takes the address, or the username, at the moment
		// of this one, and Keycloak refuses this one in other words.
		kc.Race = &kctest.Race{Seed: &kctest.Seed{Username: "lena", FirstName: "Lena", Email: "kim@example.org"}, Text: other}
		if _, err := c.Create(ctx, keycloak.NewUser{Username: "kim", FirstName: "Kim", Email: "kim@example.org"}); !errors.Is(err, keycloak.ErrEmailConflict) {
			t.Errorf("the address taken at once: %v, want ErrEmailConflict", err)
		}
		kc.Race = &kctest.Race{Seed: &kctest.Seed{Username: "noah", FirstName: "Noah", Email: "noah@example.org"}, Text: other}
		if _, err := c.Create(ctx, keycloak.NewUser{Username: "noah", FirstName: "Noah K", Email: "noah.k@example.org"}); !errors.Is(err, keycloak.ErrConflict) {
			t.Errorf("the username taken at once: %v, want ErrConflict", err)
		}
		kc.Race = &kctest.Race{Seed: &kctest.Seed{Username: "omar", FirstName: "Omar", Email: "omar@example.org"}, Text: other}
		if err := changeTo("omar@example.org"); !errors.Is(err, keycloak.ErrEmailConflict) {
			t.Errorf("a change, the address taken at once: %v, want ErrEmailConflict", err)
		}
		// The loser of two creates at once, as Keycloak 26.0.7 answered it in
		// 256 of 300 tries: no errorMessage at all.
		kc.Race = &kctest.Race{Seed: &kctest.Seed{Username: "rosa", FirstName: "Rosa", Email: "ida@example.org"},
			Body: map[string]any{"error": "unknown_error", "error_description": "Duplicate resource error"}}
		if _, err := c.Create(ctx, keycloak.NewUser{Username: "ida", FirstName: "Ida", Email: "ida@example.org"}); !errors.Is(err, keycloak.ErrEmailConflict) {
			t.Errorf("Keycloak's own answer to the loser of two creates at once: %v, want ErrEmailConflict", err)
		}
		// Neither is another user's: the refusal stays Keycloak's, no taken one.
		kc.Race = &kctest.Race{Text: "Could not create user"}
		_, err := c.Create(ctx, keycloak.NewUser{Username: "pia", FirstName: "Pia", Email: "pia@example.org"})
		var failed *keycloak.Failed
		if !errors.As(err, &failed) || failed.Status != 409 || errors.Is(err, keycloak.ErrConflict) || errors.Is(err, keycloak.ErrEmailConflict) {
			t.Errorf("a 409 that names nothing taken: %v", err)
		}
		if kc.UserByName("kim") != "" || kc.UserByName("pia") != "" || kc.UserByName("ida") != "" || kc.User(mia)["email"] != "mia@example.org" {
			t.Error("a refused write was kept")
		}
	})
}

// errOf is the error of a create.
func errOf(_ string, err error) error { return err }
