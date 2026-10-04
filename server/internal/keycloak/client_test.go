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
