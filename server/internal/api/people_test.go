package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth/authtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/chino"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak/kctest"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/people"
	"github.com/zaentrum/zaentrum-portal/server/internal/ratelimit"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeInvites is the invite store in memory, with the store's rules: a hash
// per invite, a new one revokes the person's open ones, a use is once and
// all or nothing (the lock is the mutex).
type fakeInvites struct {
	mu   sync.Mutex
	rows []model.Invite
}

func (f *fakeInvites) CreateInvite(_ context.Context, userID, createdBy string, hash []byte, expires time.Time) (model.Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for i := range f.rows {
		if f.rows[i].UserID == userID && f.rows[i].UsedAt == nil && f.rows[i].RevokedAt == nil {
			f.rows[i].RevokedAt = &now
		}
	}
	inv := model.Invite{ID: int64(len(f.rows) + 1), TokenHash: append([]byte(nil), hash...), UserID: userID, CreatedBy: createdBy,
		CreatedAt: now, ExpiresAt: expires}
	f.rows = append(f.rows, inv)
	return inv, nil
}

func (f *fakeInvites) LatestInvites(context.Context) (map[string]model.Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]model.Invite{}
	for _, r := range f.rows {
		out[r.UserID] = r
	}
	return out, nil
}

func (f *fakeInvites) find(hash []byte) int {
	for i := range f.rows {
		if bytes.Equal(f.rows[i].TokenHash, hash) {
			return i
		}
	}
	return -1
}

func (f *fakeInvites) OpenInvite(_ context.Context, hash []byte, now time.Time) (model.Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(hash)
	if i < 0 || !f.rows[i].Opens(hash, now) {
		return model.Invite{}, store.ErrInviteClosed
	}
	return f.rows[i], nil
}

func (f *fakeInvites) UseInvite(_ context.Context, hash []byte, now time.Time, use func(model.Invite) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(hash)
	if i < 0 || !f.rows[i].Opens(hash, now) {
		return store.ErrInviteClosed
	}
	if err := use(f.rows[i]); err != nil {
		return err
	}
	f.rows[i].UsedAt = &now
	return nil
}

func (f *fakeInvites) RevokeInvites(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for i := range f.rows {
		if f.rows[i].UserID == userID && f.rows[i].UsedAt == nil && f.rows[i].RevokedAt == nil {
			f.rows[i].RevokedAt = &now
		}
	}
	return nil
}

func (f *fakeInvites) DeleteInvites(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = slices.DeleteFunc(f.rows, func(r model.Invite) bool { return r.UserID == userID })
	return nil
}

// fakeChino is chino-api's data deletion: it records each call, and answers
// what status says.
type fakeChino struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	calls  []*http.Request
}

func newFakeChino(t *testing.T) *fakeChino {
	t.Helper()
	f := &fakeChino{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Clone(context.Background()))
		w.WriteHeader(f.status)
	}))
	t.Cleanup(f.Close)
	return f
}

const deletionToken = "deletion-token-for-tests-0123456789abcdefghijk"

// peopleEnv is portal-api behind the test issuer, with the realm's people in
// a fake Keycloak, the invites in memory and chino-api faked.
type peopleEnv struct {
	*tokenEnv
	kc      *kctest.Server
	invites *fakeInvites
	chino   *fakeChino
	// owner is the first admin, a Keycloak administrator; admin the
	// platform's admin who signs in (adminPortal's subject).
	owner, admin string
}

func newPeopleEnv(t *testing.T) *peopleEnv {
	t.Helper()
	e := newTokenEnv(t)
	kc := kctest.New(t)
	ch := newFakeChino(t)
	inv := &fakeInvites{}
	e.api.cfg.PeopleClientID = kc.ClientID
	e.api.cfg.PasswordPolicy = "length(8) and notUsername and notEmail"
	e.api.cfg.InviteTTL = 7 * 24 * time.Hour
	e.api.cfg.AccountDeletionToken = deletionToken
	e.api.people = people.New(keycloak.New(keycloak.Config{URL: kc.Base(), Realm: kc.Realm, ClientID: kc.ClientID, ClientSecret: kc.ClientSecret}),
		"zaentrum-admin", "zaentrum-user", []string{"zaentrum-verify"})
	e.api.invites = inv
	e.api.inviteIP, e.api.inviteToken = newInviteLimits()
	e.api.chino = chino.New(ch.URL, deletionToken)
	owner := kc.Add(kctest.Seed{ID: "user-owner", Username: "owner", FirstName: "Zaentrum", LastName: "Administrator", Email: "owner@example.org",
		Roles: []string{kctest.AdminRole, kctest.UserRole}, RealmManagement: []string{"realm-admin"}, Imported: true})
	admin := kc.Add(kctest.Seed{ID: "user-admin", Username: "admin", FirstName: "Anna", Roles: []string{kctest.AdminRole, kctest.UserRole}})
	kc.Add(kctest.Seed{Username: "zaentrum-verify", FirstName: "Zaentrum", LastName: "Verification", Roles: []string{kctest.UserRole}})
	return &peopleEnv{tokenEnv: e, kc: kc, invites: inv, chino: ch, owner: owner, admin: admin}
}

// public sends a request with no bearer, from the client address ip.
func (e *peopleEnv) public(ip, method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func decodeAs[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %v %s", err, rec.Body)
	}
	return v
}

type created struct {
	Person personView `json:"person"`
	Invite inviteLink `json:"invite"`
}

// add makes a person as the admin and answers what POST /people did.
func (e *peopleEnv) add(body map[string]any) created {
	e.t.Helper()
	rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", body)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("POST /people %v = %d %s", body, rec.Code, rec.Body)
	}
	return decodeAs[created](e.t, rec)
}

func hashToken(t *testing.T, token string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return sum[:]
}

// ─── access ──────────────────────────────────────────────────────────────────

// The People routes take the admin role on a token of the portal's own
// clients — the admin rule of every console; the invite routes take no
// bearer at all; DELETE /me takes the account deletion token and a person's
// bearer.
func TestPeopleRoutesAreTheAdminsAndInvitesArePublic(t *testing.T) {
	e := newPeopleEnv(t)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/portal/people"},
		{http.MethodPost, "/api/portal/people"},
		{http.MethodPatch, "/api/portal/people/user-owner"},
		{http.MethodDelete, "/api/portal/people/user-owner"},
		{http.MethodPost, "/api/portal/people/user-owner/invite"},
	}
	for _, rt := range routes {
		for name, c := range map[string]struct {
			claims map[string]any
			code   int
		}{
			"no bearer":                      {nil, http.StatusUnauthorized},
			"a viewer":                       {viewerPortal, http.StatusForbidden},
			"an admin through the media app": {adminMedia, http.StatusForbidden},
			"an addon's service account":     {addonSample, http.StatusForbidden},
		} {
			if rec := e.do(c.claims, rt.method, rt.path, map[string]any{}); rec.Code != c.code {
				t.Errorf("%s %s as %s: %d, want %d", rt.method, rt.path, name, rec.Code, c.code)
			}
		}
	}
	if rec := e.do(adminPortal, http.MethodGet, "/api/portal/people", nil); rec.Code != http.StatusOK {
		t.Errorf("the admin: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(adminCLI, http.MethodGet, "/api/portal/people", nil); rec.Code != http.StatusOK {
		t.Errorf("the admin through the CLI: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.kc.CallsMatching("PUT")) + len(e.kc.CallsMatching("POST")) + len(e.kc.CallsMatching("DELETE")); n != 0 {
		t.Errorf("a refused caller changed Keycloak: %d writes", n)
	}
	// The invite routes answer without a bearer.
	if rec := e.public("203.0.113.9", http.MethodGet, "/api/portal/invites/"+strings.Repeat("A", 43), nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /invites without a bearer: %d", rec.Code)
	}
}

// ─── the list ────────────────────────────────────────────────────────────────

// Everyone but the platform's own accounts, by name: role, cap, whether they
// are on, when made, their invite's state — never its token — and which one
// is the caller and which Keycloak's to change.
func TestPeopleAreListed(t *testing.T) {
	e := newPeopleEnv(t)
	mia := e.add(map[string]any{"username": "mia", "displayName": "Mia", "role": "user", "maxRating": 12})
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/people", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), mia.Invite.Token) {
		t.Error("the listing carries an invite's token")
	}
	doc := decodeAs[peopleDoc](t, rec)
	if doc.Mode != peopleBundled || !doc.DeletesData || doc.InviteDays != 7 {
		t.Errorf("doc %+v", doc)
	}
	var names []string
	for _, p := range doc.People {
		names = append(names, p.Username)
	}
	if !slices.Equal(names, []string{"admin", "mia", "owner"}) { // Anna, Mia, Zaentrum Administrator
		t.Fatalf("listed %v: the verification account is no person", names)
	}
	anna, kid, owner := doc.People[0], doc.People[1], doc.People[2]
	if !anna.Self || anna.Role != people.RoleAdmin || anna.Managed != "" || anna.Invite != nil {
		t.Errorf("the caller: %+v", anna)
	}
	if kid.Role != people.RoleUser || kid.MaxRating == nil || *kid.MaxRating != 12 || kid.Invite == nil || kid.Invite.Status != model.InvitePending {
		t.Errorf("mia: %+v %+v", kid, kid.Invite)
	}
	if owner.Managed != "keycloak" || owner.CreatedAt != nil || owner.Email != "owner@example.org" || owner.DisplayName != "Zaentrum Administrator" {
		t.Errorf("the first admin: %+v", owner)
	}
}

// With an external provider people live there: the page links to it and
// nothing here changes anyone. Without the people client's secret the page
// says what it needs.
func TestPeopleWithAnExternalProviderOrNoSecret(t *testing.T) {
	e := newPeopleEnv(t)
	e.api.people = nil
	e.api.cfg.OIDCIssuer = "https://sso.example.org/realms/household"
	doc := decodeAs[peopleDoc](t, e.do(adminPortal, http.MethodGet, "/api/portal/people", nil))
	if doc.Mode != peopleExternal || doc.ManageURL != "https://sso.example.org/admin/household/console/" || len(doc.People) != 0 {
		t.Errorf("external: %+v", doc)
	}
	e.api.cfg.PeopleManageURL = "https://idp.example.org/users"
	if doc := decodeAs[peopleDoc](t, e.do(adminPortal, http.MethodGet, "/api/portal/people", nil)); doc.ManageURL != "https://idp.example.org/users" {
		t.Errorf("a configured link: %+v", doc)
	}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "mia", "displayName": "Mia"}); rec.Code != http.StatusConflict {
		t.Errorf("adding with an external provider: %d", rec.Code)
	}
	if rec := e.public("203.0.113.1", http.MethodGet, "/api/portal/invites/"+strings.Repeat("A", 43), nil); rec.Code != http.StatusNotFound {
		t.Errorf("an invite with an external provider: %d", rec.Code)
	}

	e = newPeopleEnv(t)
	e.api.people = people.New(keycloak.New(keycloak.Config{URL: e.kc.Base(), Realm: "zaentrum", ClientID: "zaentrum-people"}), "zaentrum-admin", "zaentrum-user", nil)
	doc = decodeAs[peopleDoc](t, e.do(adminPortal, http.MethodGet, "/api/portal/people", nil))
	if doc.Mode != peopleUnavailable || !strings.Contains(doc.Note, "Secret zaentrum-people") {
		t.Errorf("no secret: %+v", doc)
	}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "mia", "displayName": "Mia"}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("adding without the secret: %d", rec.Code)
	}
}

// ─── adding ──────────────────────────────────────────────────────────────────

// A person is added enabled, without a password, with their role and cap, and
// the answer carries their invite link once: a token of 32 random bytes,
// stored as its SHA-256.
func TestAddingAPerson(t *testing.T) {
	e := newPeopleEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/people", strings.NewReader(`{"username":"Mia","displayName":" Mia ","role":"user","maxRating":12}`))
	req.Header.Set("Authorization", "Bearer "+e.iss.Token(t, adminPortal))
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "media.example.org")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	c := decodeAs[created](t, rec)
	p := c.Person
	if p.Username != "mia" || p.DisplayName != "Mia" || p.Role != people.RoleUser || *p.MaxRating != 12 || !p.Enabled {
		t.Errorf("person %+v", p)
	}
	id := e.kc.UserByName("mia")
	if id == "" || id != p.ID || e.kc.Password(id) != "" {
		t.Fatalf("in Keycloak: %q, password set: %v", id, e.kc.Password(id) != "")
	}
	if roles := e.kc.RealmRoles(id); !slices.Contains(roles, "zaentrum-user") || slices.Contains(roles, "zaentrum-admin") {
		t.Errorf("mia's roles %v", roles)
	}
	inv := c.Invite
	if len(inv.Token) != 43 || inv.Path != "/portal/invite/"+inv.Token || inv.URL != "https://media.example.org/portal/invite/"+inv.Token {
		t.Errorf("invite %+v", inv)
	}
	if got := inv.ExpiresAt.Sub(time.Now()); got < 7*24*time.Hour-time.Minute || got > 7*24*time.Hour {
		t.Errorf("expires in %v", got)
	}
	rows := e.invites.rows
	if len(rows) != 1 || !bytes.Equal(rows[0].TokenHash, hashToken(t, inv.Token)) || rows[0].UserID != id || rows[0].CreatedBy != "admin" {
		t.Errorf("stored %+v", rows)
	}
	if bytes.Contains(rows[0].TokenHash, []byte(inv.Token)) {
		t.Error("the token is stored")
	}

	// What the page does not take.
	for name, body := range map[string]map[string]any{
		"a username taken":     {"username": "mia", "displayName": "Mia Two", "role": "user"},
		"no username":          {"username": "", "displayName": "X", "role": "user"},
		"a username with a /":  {"username": "a/b", "displayName": "X", "role": "user"},
		"a name with markup":   {"username": "kim", "displayName": "<b>Kim</b>", "role": "user"},
		"a cap past 21":        {"username": "kim", "displayName": "Kim", "role": "user", "maxRating": 22},
		"a role of no kind":    {"username": "kim", "displayName": "Kim", "role": "owner"},
		"an admin with a cap":  {"username": "kim", "displayName": "Kim", "role": "admin", "maxRating": 16},
		"a field of no person": {"username": "kim", "displayName": "Kim", "role": "user", "realmRoles": []string{"realm-admin"}},
	} {
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if id := e.kc.UserByName("kim"); id != "" {
		t.Error("a refused person was made")
	}
}

// A cap Keycloak does not keep — its user profile does not declare the
// attribute — fails the add, says why, and leaves no uncapped child behind.
func TestACapKeycloakDropsFailsTheAdd(t *testing.T) {
	e := newPeopleEnv(t)
	e.kc.DeclareRating = false
	rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "leo", "displayName": "Leo", "role": "user", "maxRating": 6})
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "max_rating") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if e.kc.UserByName("leo") != "" {
		t.Error("leo stayed, without his cap")
	}
}

// ─── changing ────────────────────────────────────────────────────────────────

func (e *peopleEnv) patch(claims map[string]any, id string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(claims, http.MethodPatch, "/api/portal/people/"+id, body)
}

// A name, a role, a cap, on or off: each changes alone, null removes the cap,
// and the answer is the person as Keycloak holds them now.
func TestChangingAPerson(t *testing.T) {
	e := newPeopleEnv(t)
	id := e.add(map[string]any{"username": "mia", "displayName": "Mia", "role": "user", "maxRating": 12}).Person.ID

	p := decodeAs[personView](t, e.patch(adminPortal, id, map[string]any{"displayName": "Mia Sophie", "maxRating": 16}))
	if p.DisplayName != "Mia Sophie" || *p.MaxRating != 16 {
		t.Errorf("renamed, capped at 16: %+v", p)
	}
	if u := e.kc.User(id); u["firstName"] != "Mia Sophie" || u["lastName"] != "" {
		t.Errorf("in Keycloak: %v", u)
	}
	p = decodeAs[personView](t, e.patch(adminPortal, id, map[string]any{"maxRating": nil}))
	if p.MaxRating != nil || p.DisplayName != "Mia Sophie" {
		t.Errorf("uncapped: %+v", p)
	}
	if rec := e.patch(adminPortal, id, map[string]any{"maxRating": "twelve"}); rec.Code != http.StatusBadRequest {
		t.Errorf("a cap that is no number: %d", rec.Code)
	}
	p = decodeAs[personView](t, e.patch(adminPortal, id, map[string]any{"role": "admin"}))
	if p.Role != people.RoleAdmin || !slices.Contains(e.kc.RealmRoles(id), "zaentrum-admin") {
		t.Errorf("made an admin: %+v %v", p, e.kc.RealmRoles(id))
	}
	if rec := e.patch(adminPortal, id, map[string]any{"maxRating": 12}); rec.Code != http.StatusBadRequest {
		t.Errorf("capping an admin: %d %s", rec.Code, rec.Body)
	}
	p = decodeAs[personView](t, e.patch(adminPortal, id, map[string]any{"role": "user", "maxRating": 12}))
	if p.Role != people.RoleUser || *p.MaxRating != 12 || slices.Contains(e.kc.RealmRoles(id), "zaentrum-admin") {
		t.Errorf("a user again, capped: %+v %v", p, e.kc.RealmRoles(id))
	}

	// Switched off, the person's open invite stops working.
	link := decodeAs[inviteLink](t, e.do(adminPortal, http.MethodPost, "/api/portal/people/"+id+"/invite", nil))
	p = decodeAs[personView](t, e.patch(adminPortal, id, map[string]any{"enabled": false}))
	if p.Enabled || p.Invite == nil || p.Invite.Status != model.InviteRevoked {
		t.Errorf("switched off: %+v %+v", p, p.Invite)
	}
	if rec := e.public("203.0.113.5", http.MethodGet, "/api/portal/invites/"+link.Token, nil); rec.Code != http.StatusNotFound {
		t.Errorf("the invite of a person switched off: %d", rec.Code)
	}
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/people/"+id+"/invite", nil); rec.Code != http.StatusConflict {
		t.Errorf("inviting a person switched off: %d", rec.Code)
	}
	if rec := e.patch(adminPortal, "no-such-id", map[string]any{"displayName": "X"}); rec.Code != http.StatusNotFound {
		t.Errorf("no such person: %d", rec.Code)
	}
}

// The rules, through the API: nobody changes their own role or switches
// themselves off, the last enabled admin stays one, the first admin —
// Keycloak's administrator — is changed in Keycloak, and the platform's own
// account is no one's to change.
func TestTheRulesHold(t *testing.T) {
	e := newPeopleEnv(t)
	for name, c := range map[string]struct {
		id   string
		body map[string]any
	}{
		"demoting yourself":       {e.admin, map[string]any{"role": "user"}},
		"switching yourself off":  {e.admin, map[string]any{"enabled": false}},
		"switching the owner off": {e.owner, map[string]any{"enabled": false}},
		"renaming the owner":      {e.owner, map[string]any{"displayName": "Owner"}},
		"demoting the owner":      {e.owner, map[string]any{"role": "user"}},
	} {
		if rec := e.patch(adminPortal, c.id, c.body); rec.Code != http.StatusConflict {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	verify := e.kc.UserByName("zaentrum-verify")
	for _, rec := range []*httptest.ResponseRecorder{
		e.patch(adminPortal, verify, map[string]any{"enabled": false}),
		e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+verify, nil),
		e.do(adminPortal, http.MethodPost, "/api/portal/people/"+verify+"/invite", nil),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("the verification account: %d %s", rec.Code, rec.Body)
		}
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"deleting yourself":                     e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+e.admin, nil),
		"deleting the owner":                    e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+e.owner, nil),
		"inviting the owner (a password reset)": e.do(adminPortal, http.MethodPost, "/api/portal/people/"+e.owner+"/invite", nil),
		"inviting yourself":                     e.do(adminPortal, http.MethodPost, "/api/portal/people/"+e.admin+"/invite", nil),
	} {
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if pw := e.kc.Password(e.owner); pw != "" {
		t.Error("the owner's password was touched")
	}

	// Lukas is the other admin; once the owner is gone (in Keycloak), the
	// admin of the portal and Lukas are the only ones: neither may leave
	// the other with none.
	lukas := e.add(map[string]any{"username": "lukas", "displayName": "Lukas", "role": "admin"}).Person.ID
	if err := e.api.people.Delete(context.Background(), e.owner); err != nil {
		t.Fatal(err)
	}
	if rec := e.patch(adminPortal, lukas, map[string]any{"role": "user"}); rec.Code != http.StatusOK {
		t.Fatalf("demoting lukas while anna is an admin: %d %s", rec.Code, rec.Body)
	}
	lukasClaims := authtest.Person("zaentrum-web", "lukas", "zaentrum-admin", "zaentrum-user")
	lukasClaims["sub"] = lukas
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"demoting the last admin":      e.patch(lukasClaims, e.admin, map[string]any{"role": "user"}),
		"switching the last admin off": e.patch(lukasClaims, e.admin, map[string]any{"enabled": false}),
		"deleting the last admin":      e.do(lukasClaims, http.MethodDelete, "/api/portal/people/"+e.admin, nil),
	} {
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "last admin") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if !slices.Contains(e.kc.RealmRoles(e.admin), "zaentrum-admin") || e.kc.User(e.admin)["enabled"] != true {
		t.Error("the last admin was changed")
	}
}

// ─── deleting ────────────────────────────────────────────────────────────────

// chino's data of the person goes first — asked with the admin's bearer and
// the deletion token — then the account, then their invites. chino-api
// refusing deletes nothing.
func TestDeletingAPerson(t *testing.T) {
	e := newPeopleEnv(t)
	c := e.add(map[string]any{"username": "mia", "displayName": "Mia", "role": "user", "maxRating": 12})
	id := c.Person.ID

	e.chino.status = http.StatusBadGateway
	if rec := e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+id, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("chino-api refusing: %d %s", rec.Code, rec.Body)
	}
	if e.kc.User(id) == nil {
		t.Fatal("the account went though chino's data stayed")
	}
	e.chino.status = http.StatusOK
	rec := e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+id, nil)
	if rec.Code != http.StatusOK || decodeAs[map[string]any](t, rec)["data"] != "deleted" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.kc.User(id) != nil {
		t.Error("the account stays")
	}
	if len(e.chino.calls) != 2 {
		t.Fatalf("%d calls to chino-api", len(e.chino.calls))
	}
	call := e.chino.calls[1]
	if call.Method != http.MethodDelete || call.URL.Path != "/api/v1/admin/accounts/"+id+"/data" ||
		call.Header.Get(chino.DeletionHeader) != deletionToken || !strings.HasPrefix(call.Header.Get("Authorization"), "Bearer ") {
		t.Errorf("chino-api was asked %s %s with %v", call.Method, call.URL, call.Header)
	}
	if rec := e.public("203.0.113.7", http.MethodGet, "/api/portal/invites/"+c.Invite.Token, nil); rec.Code != http.StatusNotFound {
		t.Errorf("a deleted person's invite: %d", rec.Code)
	}
	if rows, _ := e.invites.LatestInvites(context.Background()); len(rows) != 0 {
		t.Errorf("invites stay: %v", rows)
	}
	if rec := e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+id, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleted twice: %d", rec.Code)
	}

	// Without chino-api to ask, the account goes and the answer says the
	// data stays.
	e.api.chino = nil
	id = e.add(map[string]any{"username": "leo", "displayName": "Leo", "role": "user"}).Person.ID
	rec = e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+id, nil)
	if rec.Code != http.StatusOK || decodeAs[map[string]any](t, rec)["data"] != "kept" {
		t.Errorf("without chino-api: %d %s", rec.Code, rec.Body)
	}
}

// Deleting a person deletes their notices — an admin's delete on the People
// page, and their own from the apps — and nobody else's.
func TestDeletingAPersonDeletesTheirNotices(t *testing.T) {
	e := newPeopleEnv(t)
	e.store.apps["sample"] = model.App{Key: "sample", Title: "Sample Addon"}
	e.store.addons["sample"] = model.Addon{Key: "sample"}
	lena := e.add(map[string]any{"username": "lena", "displayName": "Lena", "role": "user"}).Person.ID
	noah := e.add(map[string]any{"username": "noah", "displayName": "Noah", "role": "user"}).Person.ID
	for _, sub := range []string{lena, noah, e.admin} {
		if rec := e.do(addonSample, http.MethodPost, "/api/portal/notices", notice(sub)); rec.Code != http.StatusCreated {
			t.Fatalf("post to %s: %d %s", sub, rec.Code, rec.Body)
		}
	}
	if rec := e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+lena, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete lena: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.notices.of(lena)); n != 0 {
		t.Errorf("lena's %d notices stay", n)
	}
	if len(e.notices.of(noah)) != 1 || len(e.notices.of(e.admin)) != 1 {
		t.Errorf("deleting lena took others': %+v", e.notices.all())
	}

	noahs := authtest.Person("chino-tv", "noah", "zaentrum-user")
	noahs["sub"] = noah
	req := httptest.NewRequest(http.MethodDelete, "/api/portal/me", nil)
	req.Header.Set("Authorization", "Bearer "+e.iss.Token(t, noahs))
	req.Header.Set(chino.DeletionHeader, deletionToken)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("noah deletes his account: %d %s", rec.Code, rec.Body)
	}
	if len(e.notices.of(noah)) != 0 || len(e.notices.of(e.admin)) != 1 {
		t.Errorf("after noah: %+v", e.notices.all())
	}
	// A refused deletion deletes no notice.
	e.chino.status = http.StatusBadGateway
	if rec := e.do(adminPortal, http.MethodDelete, "/api/portal/people/"+e.admin, nil); rec.Code == http.StatusOK {
		t.Fatalf("deleting oneself: %d", rec.Code)
	}
	if len(e.notices.of(e.admin)) != 1 {
		t.Error("a refused deletion took the notices")
	}
}

// ─── invites ─────────────────────────────────────────────────────────────────

// The invite page reads whose invite it is and the password's rules; the
// person chooses a password, which Keycloak keeps — not temporary — with
// nothing left to do at the first sign-in, and the link is used.
func TestAnInviteSetsThePasswordOnce(t *testing.T) {
	e := newPeopleEnv(t)
	c := e.add(map[string]any{"username": "mia", "displayName": "Mia", "role": "user", "maxRating": 12})
	id := c.Person.ID
	e.kc.SetRequiredActions(id, "UPDATE_PASSWORD", "VERIFY_EMAIL")
	e.kc.Lock(id)
	path := "/api/portal/invites/" + c.Invite.Token

	rec := e.public("203.0.113.10", http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %d %s", rec.Code, rec.Body)
	}
	got := decodeAs[map[string]any](t, rec)
	if got["valid"] != true || got["username"] != "mia" || got["displayName"] != "Mia" {
		t.Errorf("GET %v", got)
	}
	if pol, _ := got["passwordPolicy"].(map[string]any); pol["minLength"] != float64(8) {
		t.Errorf("policy %v", got["passwordPolicy"])
	}
	for k := range got {
		if !slices.Contains([]string{"valid", "username", "displayName", "expiresAt", "passwordPolicy"}, k) {
			t.Errorf("GET says %s", k)
		}
	}

	// Refused passwords leave the invite open: the policy portal-api knows,
	// then a rule only Keycloak's policy holds.
	for _, pw := range []string{"short", "mia", ""} {
		rec := e.public("203.0.113.10", http.MethodPost, path, map[string]any{"password": pw})
		if rec.Code != http.StatusBadRequest || decodeAs[map[string]any](t, rec)["error"] != "password" {
			t.Errorf("password %q: %d %s", pw, rec.Code, rec.Body)
		}
	}
	e.kc.ExtraDigits = 2
	rec = e.public("203.0.113.10", http.MethodPost, path, map[string]any{"password": "sternschnuppe"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at least 2 digit") {
		t.Errorf("Keycloak's own rule: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "Invalid password") {
		t.Errorf("Keycloak's words: %s", rec.Body)
	}
	if e.kc.Password(id) != "" {
		t.Fatal("a refused password was set")
	}
	rec = e.public("203.0.113.10", http.MethodPost, path, map[string]any{"password": "sternschnuppe42"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %d %s", rec.Code, rec.Body)
	}
	if got := decodeAs[map[string]any](t, rec); got["username"] != "mia" || got["signIn"] != "/portal/" {
		t.Errorf("POST %v", got)
	}
	if e.kc.Password(id) != "sternschnuppe42" {
		t.Error("the password was not set")
	}
	if u := e.kc.User(id); len(u["requiredActions"].([]any)) != 0 || u["firstName"] != "Mia" {
		t.Errorf("after the invite: %v", u)
	}
	if e.kc.Locked(id) {
		t.Error("the lockout stays")
	}
	if calls := e.kc.CallsMatching("PUT /users/" + id + "/reset-password"); len(calls) != 2 {
		t.Errorf("reset-password calls %v", calls)
	}

	// Used: the same link, read or posted again, does not work.
	if rec := e.public("203.0.113.10", http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET after use: %d", rec.Code)
	}
	if rec := e.public("203.0.113.10", http.MethodPost, path, map[string]any{"password": "another-password-9"}); rec.Code != http.StatusNotFound {
		t.Errorf("POST after use: %d", rec.Code)
	}
	if e.kc.Password(id) != "sternschnuppe42" {
		t.Error("a used invite set the password again")
	}
	doc := decodeAs[peopleDoc](t, e.do(adminPortal, http.MethodGet, "/api/portal/people", nil))
	for _, p := range doc.People {
		if p.ID == id && (p.Invite == nil || p.Invite.Status != model.InviteUsed || p.Invite.UsedAt == nil) {
			t.Errorf("listed after use: %+v", p.Invite)
		}
	}
}

// Every link that does not work answers alike: a token of no invite, one
// malformed, used, expired, replaced by a newer one, or of an account gone or
// switched off — so a reply tells nobody which.
func TestInvitesThatDoNotWorkAnswerAlike(t *testing.T) {
	e := newPeopleEnv(t)
	start := time.Now()
	clock := start
	e.api.now = func() time.Time { return clock }
	newLink := func(name string) (string, string) {
		c := e.add(map[string]any{"username": name, "displayName": strings.ToUpper(name[:1]) + name[1:], "role": "user"})
		return c.Person.ID, c.Invite.Token
	}
	_, used := newLink("used")
	if rec := e.public("203.0.113.20", http.MethodPost, "/api/portal/invites/"+used, map[string]any{"password": "a-good-password"}); rec.Code != http.StatusOK {
		t.Fatalf("use: %d %s", rec.Code, rec.Body)
	}
	repl, replaced := newLink("replaced")
	if rec := e.do(adminPortal, http.MethodPost, "/api/portal/people/"+repl+"/invite", nil); rec.Code != http.StatusCreated {
		t.Fatalf("re-invite: %d", rec.Code)
	}
	goneID, gone := newLink("gone")
	if err := e.api.people.Delete(context.Background(), goneID); err != nil {
		t.Fatal(err)
	}
	offID, off := newLink("off")
	u, _ := e.api.people.Keycloak().User(context.Background(), offID)
	u.SetEnabled(false) // switched off in Keycloak, not on the page
	_ = e.api.people.Keycloak().Update(context.Background(), u)
	_, expired := newLink("expired")

	var want string
	closed := func(name, token string) {
		t.Helper()
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := e.public("203.0.113.21", method, "/api/portal/invites/"+token, map[string]any{"password": "a-good-password"})
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d %s", method, name, rec.Code, rec.Body)
				continue
			}
			if want == "" {
				want = rec.Body.String()
			}
			if rec.Body.String() != want {
				t.Errorf("%s %s answers differently: %s, want %s", method, name, rec.Body, want)
			}
		}
	}
	// Each while the others still work: none of them is merely expired.
	for name, token := range map[string]string{
		"unknown": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "malformed": "not-a-token",
		"too long": strings.Repeat("A", 44), "used": used, "replaced": replaced, "gone": gone, "off": off,
	} {
		closed(name, token)
	}
	if rec := e.public("203.0.113.21", http.MethodGet, "/api/portal/invites/"+expired, nil); rec.Code != http.StatusOK {
		t.Fatalf("the invite that expires next still works now: %d %s", rec.Code, rec.Body)
	}
	clock = start.Add(7*24*time.Hour + time.Second)
	closed("expired", expired)
	if !strings.Contains(want, `"valid":false`) {
		t.Errorf("the refusal: %s", want)
	}
	if e.kc.Password(offID) != "" {
		t.Error("a switched-off account's invite set its password")
	}
}

// Two posts of one link at once: one sets the password, the other finds the
// link used.
func TestAnInviteUsedTwiceAtOnce(t *testing.T) {
	e := newPeopleEnv(t)
	c := e.add(map[string]any{"username": "mia", "displayName": "Mia", "role": "user"})
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.public(fmt.Sprintf("203.0.113.%d", 30+i), http.MethodPost, "/api/portal/invites/"+c.Invite.Token,
				map[string]any{"password": fmt.Sprintf("password-number-%d", i)}).Code
		}()
	}
	wg.Wait()
	ok, closed := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusNotFound:
			closed++
		}
	}
	if ok != 1 || closed != 5 {
		t.Errorf("answers %v: want one 200 and five 404", codes)
	}
}

// A client address gets 20 tries in a burst, a token 10 — from any address —
// then 429 with Retry-After. The address is the last one X-Forwarded-For
// names: the one the proxy in front of portal-api added, never what a client
// writes before it.
func TestInvitesAreRateLimited(t *testing.T) {
	e := newPeopleEnv(t)
	frozen := time.Unix(1_800_000_000, 0)
	e.api.inviteIP = ratelimit.New(inviteIPBurst, inviteIPEvery).WithClock(func() time.Time { return frozen })
	e.api.inviteToken = ratelimit.New(inviteTokenBurst, inviteTokenEvery).WithClock(func() time.Time { return frozen })
	unknown := func(i int) string {
		return "/api/portal/invites/" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(i)}, 32))
	}
	for i := 0; i < inviteIPBurst; i++ {
		if rec := e.public("198.51.100.1", http.MethodGet, unknown(i), nil); rec.Code != http.StatusNotFound {
			t.Fatalf("try %d: %d", i+1, rec.Code)
		}
	}
	rec := e.public("198.51.100.1", http.MethodGet, unknown(99), nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("past the burst: %d %v", rec.Code, rec.Header())
	}
	// Another address written before the proxy's does not get around it.
	req := httptest.NewRequest(http.MethodGet, unknown(98), nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.77, 198.51.100.1")
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("a client-written address: %d", rr.Code)
	}
	if rec := e.public("198.51.100.2", http.MethodGet, unknown(97), nil); rec.Code != http.StatusNotFound {
		t.Errorf("another address: %d", rec.Code)
	}

	// One token, tried from many addresses.
	c := e.add(map[string]any{"username": "mia", "displayName": "Mia", "role": "user"})
	path := "/api/portal/invites/" + c.Invite.Token
	for i := 0; i < inviteTokenBurst; i++ {
		if rec := e.public(fmt.Sprintf("203.0.113.%d", 100+i), http.MethodPost, path, map[string]any{"password": "x"}); rec.Code != http.StatusBadRequest {
			t.Fatalf("try %d on the token: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if rec := e.public("203.0.113.200", http.MethodPost, path, map[string]any{"password": "a-good-password"}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("past the token's burst: %d", rec.Code)
	}
	if e.kc.Password(c.Person.ID) != "" {
		t.Error("a limited try set the password")
	}
}

// ─── deleting your own account ───────────────────────────────────────────────

// chino-api deletes a person's account with the deletion token and the
// person's own bearer: the account of the token's subject, and only that.
// Anything else is refused — the last admin and a Keycloak administrator too.
func TestDeletingYourOwnAccount(t *testing.T) {
	e := newPeopleEnv(t)
	c := e.add(map[string]any{"username": "mia", "displayName": "Mia", "role": "user", "maxRating": 12})
	mia := authtest.Person("chino-tv", "mia", "zaentrum-user")
	mia["sub"] = c.Person.ID
	del := func(claims map[string]any, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/portal/me", nil)
		if claims != nil {
			req.Header.Set("Authorization", "Bearer "+e.iss.Token(t, claims))
		}
		if token != "" {
			req.Header.Set(chino.DeletionHeader, token)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	for name, c := range map[string]struct {
		claims map[string]any
		token  string
		code   int
	}{
		"no token":                    {mia, "", http.StatusForbidden},
		"a wrong token":               {mia, deletionToken[:len(deletionToken)-1] + "x", http.StatusForbidden},
		"a token one char short":      {mia, deletionToken[:len(deletionToken)-1], http.StatusForbidden},
		"no bearer":                   {nil, deletionToken, http.StatusUnauthorized},
		"a service account":           {authtest.ServiceAccount("chino-api"), deletionToken, http.StatusForbidden},
		"the owner, Keycloak's admin": {func() map[string]any { o := authtest.Person("chino-web", "owner"); o["sub"] = e.owner; return o }(), deletionToken, http.StatusConflict},
	} {
		if rec := del(c.claims, c.token); rec.Code != c.code {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, c.code)
		}
	}
	if e.kc.User(c.Person.ID) == nil || e.kc.User(e.owner) == nil {
		t.Fatal("a refused deletion deleted")
	}
	if rec := del(mia, deletionToken); rec.Code != http.StatusNoContent {
		t.Fatalf("mia: %d %s", rec.Code, rec.Body)
	}
	if e.kc.User(c.Person.ID) != nil || e.kc.User(e.admin) == nil {
		t.Error("deleted the wrong account")
	}
	if rows, _ := e.invites.LatestInvites(context.Background()); len(rows) != 0 {
		t.Errorf("mia's invites stay: %v", rows)
	}
	if rec := del(mia, deletionToken); rec.Code != http.StatusNotFound {
		t.Errorf("deleted twice: %d", rec.Code)
	}

	// The last admin of the platform is refused; with another admin, not.
	if err := e.api.people.Delete(context.Background(), e.owner); err != nil {
		t.Fatal(err)
	}
	if rec := del(adminMedia, deletionToken); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "last admin") {
		t.Errorf("the last admin: %d %s", rec.Code, rec.Body)
	}
	e.add(map[string]any{"username": "lukas", "displayName": "Lukas", "role": "admin"})
	if rec := del(adminMedia, deletionToken); rec.Code != http.StatusNoContent {
		t.Errorf("an admin with another one: %d %s", rec.Code, rec.Body)
	}

	// Not set up: no token configured.
	e.api.cfg.AccountDeletionToken = ""
	if rec := del(mia, ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("not set up: %d", rec.Code)
	}
}

// ─── never Keycloak's words ──────────────────────────────────────────────────

// A Keycloak that fails, refuses the people client or does not answer is
// said in portal-api's words; what Keycloak said goes nowhere near the
// answer.
func TestKeycloakFailuresAreWorded(t *testing.T) {
	e := newPeopleEnv(t)
	e.kc.Fail = http.StatusInternalServerError
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/people", nil)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "java") {
		t.Errorf("failing: %d %s", rec.Code, rec.Body)
	}
	e.kc.Fail = http.StatusForbidden
	rec = e.do(adminPortal, http.MethodGet, "/api/portal/people", nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "RealmConfigured") {
		t.Errorf("refusing the client: %d %s", rec.Code, rec.Body)
	}
	e.kc.Fail = 0
	e.kc.Close()
	rec = e.do(adminPortal, http.MethodGet, "/api/portal/people", nil)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("gone: %d %s", rec.Code, rec.Body)
	}
}

// Tokens are compared in constant time, wherever one is: the invite's hash
// (model.Invite.Opens) and the account deletion token (deleteMe).
func TestTokensAreComparedInConstantTime(t *testing.T) {
	for file, want := range map[string]string{
		"../model/model.go": "subtle.ConstantTimeCompare(i.TokenHash, hash)",
		"people.go":         "subtle.ConstantTimeCompare([]byte(got), []byte(want))",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s compares a token without %s", file, want)
		}
	}
}
