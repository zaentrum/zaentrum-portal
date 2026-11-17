// Package kctest is a Keycloak for tests: the token endpoint and the part of
// the admin REST API the People page calls, in memory, answering as Keycloak
// 26 answers its people client — a confidential client whose service account
// holds view-users, query-users and manage-users of realm-management — and
// refusing what that client may not do, as Keycloak refuses it: reading a
// role by name or its members (403), listing clients (an empty list),
// granting a role of realm-management (403).
//
// It keeps the behaviour the client depends on: an update replaces the
// user's profile with what it sends; an attribute the user profile does not
// declare is dropped without a word; the rating cap is an integer from 0 to
// 21; the password policy refuses a short password; usernames are unique and
// lower case; the realm's default roles come with every new user; an address
// is kept in lower case, refused by the user profile's email validator when it
// is none, and refused when another user has it, as a realm that lets no two
// users have one address refuses it (the bundled realm's
// duplicateEmailsAllowed false); an address written empty is gone, its key
// with it. Its answers are Keycloak 26.0.7's, in their words: a declined
// address is 400 error-invalid-email; another user's address is 409 "User
// exists with same email", on a create and a change, and on a create that
// would take another user's username too; another user's username is 409
// "User exists with same username".
package kctest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Realm roles of the fake realm.
const (
	AdminRole   = "zaentrum-admin"
	UserRole    = "zaentrum-user"
	defaultRole = "default-roles-zaentrum"
)

// Server is a running fake Keycloak.
type Server struct {
	*httptest.Server
	t *testing.T

	Realm        string
	ClientID     string
	ClientSecret string

	mu sync.Mutex
	// DeclareRating: the user profile declares max_rating. Off, Keycloak
	// drops it as it drops every attribute it does not declare.
	DeclareRating bool
	// MinPassword is the realm's password policy: length(n). ExtraDigits
	// asks for digits(n) besides, a rule portal-api does not know about.
	MinPassword int
	ExtraDigits int
	// DuplicateEmails: the realm lets two users have one address
	// (duplicateEmailsAllowed). Off, as the bundled realm has it, a user made
	// or changed with another user's address is refused.
	DuplicateEmails bool
	// Fail answers every admin call with this status, 0 for none.
	Fail int
	// Race is another write that lands at the moment the next create or
	// change of a user reaches the fake: the next such write takes it, after
	// every listing before it.
	Race *Race
	// Calls is every admin call, "METHOD /path" (the realm's prefix cut).
	Calls []string

	users     map[string]map[string]any
	roles     map[string]string              // realm role name -> id
	mappings  map[string][]string            // user id -> direct realm roles
	clients   map[string]map[string][]string // user id -> clientId -> roles
	groupRole map[string][]string            // user id -> realm roles through a group
	passwords map[string]string
	tokens    map[string]time.Time
	lockouts  map[string]bool
}

// New starts a fake Keycloak for realm zaentrum and the people client; it
// stops with the test.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		t: t, Realm: "zaentrum", ClientID: "zaentrum-people", ClientSecret: "people-secret-for-tests",
		DeclareRating: true, MinPassword: 8,
		users: map[string]map[string]any{}, mappings: map[string][]string{}, clients: map[string]map[string][]string{},
		groupRole: map[string][]string{}, passwords: map[string]string{}, tokens: map[string]time.Time{}, lockouts: map[string]bool{},
		roles: map[string]string{},
	}
	for _, r := range []string{AdminRole, UserRole, "offline_access", "uma_authorization", "zaentrum-addon", defaultRole} {
		s.roles[r] = newID()
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// URL is the base the client is configured with: Keycloak's /auth.
func (s *Server) Base() string { return s.URL + "/auth" }

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ─── seeding and reading, for tests ──────────────────────────────────────────

// Race is a write that lands at the same moment as another. Seed is the
// account it makes, added to the realm before the other write is checked, so
// that the write meets it where a listing before did not; nil for none. Text,
// where it is set, is the 409 the other write is refused with at once, in
// words of the path two writes at once take, in place of the fake's own
// checks. Body, where it is set, is that 409's whole body instead, as
// Keycloak 26.0.7 answers the loser of two creates at once most often:
// {"error":"unknown_error","error_description":"Duplicate resource error"},
// with no errorMessage at all.
type Race struct {
	Seed *Seed
	Text string
	Body map[string]any
}

// race runs the Race of a write, if one waits: false when it answered the
// write.
func (s *Server) race(w http.ResponseWriter) bool {
	r := s.Race
	if r == nil {
		return true
	}
	s.Race = nil
	if r.Seed != nil {
		s.add(*r.Seed)
	}
	switch {
	case r.Body != nil:
		writeJSON(w, http.StatusConflict, r.Body)
		return false
	case r.Text != "":
		writeJSON(w, http.StatusConflict, map[string]any{"errorMessage": r.Text})
		return false
	}
	return true
}

// Seed is an account the realm already holds.
type Seed struct {
	ID, Username, FirstName, LastName, Email string
	Disabled                                 bool
	Roles                                    []string // realm roles, mapped directly
	GroupRoles                               []string // realm roles through a group
	RealmManagement                          []string // roles of realm-management
	MaxRating                                string   // the attribute, "" for none
	Imported                                 bool     // no createdTimestamp, as an import leaves it
}

// Add seeds an account and answers its id.
func (s *Server) Add(u Seed) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(u)
}

func (s *Server) add(u Seed) string {
	id := u.ID
	if id == "" {
		id = newID()
	}
	rep := map[string]any{
		"id": id, "username": u.Username, "enabled": !u.Disabled, "firstName": u.FirstName, "lastName": u.LastName,
		"emailVerified": u.Email != "", "attributes": map[string]any{}, "requiredActions": []any{},
	}
	if u.Email != "" {
		rep["email"] = u.Email
	}
	if !u.Imported {
		rep["createdTimestamp"] = float64(time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC).UnixMilli())
	}
	if u.MaxRating != "" {
		rep["attributes"] = map[string]any{"max_rating": []any{u.MaxRating}}
	}
	s.users[id] = rep
	s.mappings[id] = append([]string{defaultRole}, u.Roles...)
	s.groupRole[id] = u.GroupRoles
	if len(u.RealmManagement) > 0 {
		s.clients[id] = map[string][]string{"realm-management": u.RealmManagement}
	}
	return id
}

// User is a copy of an account's representation, nil when gone.
func (s *Server) User(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep, ok := s.users[id]
	if !ok {
		return nil
	}
	raw, _ := json.Marshal(rep)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// UserByName is the id of the account of that username, "" for none.
func (s *Server) UserByName(username string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rep := range s.users {
		if rep["username"] == username {
			return id
		}
	}
	return ""
}

// Password is the account's password, "" for none.
func (s *Server) Password(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.passwords[id]
}

// Lock locks an account out, as failed sign-ins do; Locked says whether it is.
func (s *Server) Lock(id string) { s.mu.Lock(); s.lockouts[id] = true; s.mu.Unlock() }
func (s *Server) Locked(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockouts[id]
}

// RealmRoles are the account's direct realm roles.
func (s *Server) RealmRoles(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.mappings[id])
}

// SetRequiredActions sets what the account is asked at the next sign-in.
func (s *Server) SetRequiredActions(id string, actions ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []any{}
	for _, a := range actions {
		list = append(list, a)
	}
	s.users[id]["requiredActions"] = list
}

// CallsMatching are the admin calls that start with prefix.
func (s *Server) CallsMatching(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.Calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// ─── the wire ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	tokenPath := "/auth/realms/" + s.Realm + "/protocol/openid-connect/token"
	adminPrefix := "/auth/admin/realms/" + s.Realm
	switch {
	case r.URL.Path == tokenPath:
		s.token(w, r)
	case strings.HasPrefix(r.URL.Path, adminPrefix):
		s.admin(w, r, strings.TrimPrefix(r.URL.Path, adminPrefix))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Method != http.MethodPost {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_id") != s.ClientID ||
		r.PostForm.Get("client_secret") != s.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized_client", "error_description": "Invalid client or Invalid client credentials"})
		return
	}
	tok := "kc-" + newID()
	s.mu.Lock()
	s.tokens[tok] = time.Now().Add(5 * time.Minute)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "expires_in": 300, "token_type": "Bearer"})
}

// Expire makes every token the fake handed out stop working, as a restarted
// Keycloak would.
func (s *Server) Expire() {
	s.mu.Lock()
	s.tokens = map[string]time.Time{}
	s.mu.Unlock()
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, r.Method+" "+path)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if exp, ok := s.tokens[tok]; !ok || time.Now().After(exp) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "HTTP 401 Unauthorized"})
		return
	}
	if s.Fail != 0 {
		writeJSON(w, s.Fail, map[string]any{"error": "unknown_error", "error_description": "something broke at java.lang.Thread.run(Thread.java:1583)"})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	forbidden := func() { writeJSON(w, http.StatusForbidden, map[string]any{"error": "HTTP 403 Forbidden"}) }
	switch {
	case len(parts) >= 2 && parts[0] == "roles":
		// GET /roles/{name}, /roles/{name}/users: view-realm, which the people
		// client does not hold.
		forbidden()
	case parts[0] == "clients":
		writeJSON(w, http.StatusOK, []any{})
	case parts[0] == "events":
		forbidden()
	case parts[0] == "attack-detection" && len(parts) == 4 && r.Method == http.MethodDelete:
		delete(s.lockouts, parts[3])
		w.WriteHeader(http.StatusNoContent)
	case parts[0] == "users" && len(parts) == 1:
		s.listOrCreate(w, r)
	case parts[0] == "users" && len(parts) >= 2:
		s.user(w, r, parts[1], parts[2:])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) listOrCreate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		first, _ := strconv.Atoi(q.Get("first"))
		max, err := strconv.Atoi(q.Get("max"))
		if err != nil {
			max = 100
		}
		var ids []string
		for id := range s.users {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			return s.users[ids[i]]["username"].(string) < s.users[ids[j]]["username"].(string)
		})
		out := []any{}
		for i := first; i < len(ids) && i < first+max; i++ {
			out = append(out, s.users[ids[i]])
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var rep map[string]any
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unable to read contents from stream"})
			return
		}
		if !s.race(w) {
			return
		}
		// Another user's address is answered before another user's username,
		// as Keycloak answers a create that would take both.
		if address, _ := rep["email"].(string); !s.DuplicateEmails && s.emailTaken(address, "") {
			writeJSON(w, http.StatusConflict, map[string]any{"errorMessage": "User exists with same email"})
			return
		}
		name := strings.ToLower(fmt.Sprint(rep["username"]))
		for _, u := range s.users {
			if u["username"] == name {
				writeJSON(w, http.StatusConflict, map[string]any{"errorMessage": "User exists with same username"})
				return
			}
		}
		id := newID()
		stored := map[string]any{"id": id, "username": name, "createdTimestamp": float64(time.Now().UnixMilli())}
		if !s.apply(w, stored, rep) {
			return
		}
		s.users[id] = stored
		s.mappings[id] = []string{defaultRole}
		w.Header().Set("Location", s.URL+"/auth/admin/realms/"+s.Realm+"/users/"+id)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// apply writes rep over stored as Keycloak's user profile does: the profile's
// attributes are what rep says, absent ones cleared; an attribute the profile
// does not declare is dropped; max_rating is checked.
func (s *Server) apply(w http.ResponseWriter, stored, rep map[string]any) bool {
	for _, k := range []string{"firstName", "lastName", "email"} {
		v, _ := rep[k].(string)
		stored[k] = v
	}
	if !s.email(w, stored) {
		return false
	}
	if v, ok := rep["enabled"].(bool); ok {
		stored["enabled"] = v
	} else if _, had := stored["enabled"]; !had {
		stored["enabled"] = false
	}
	// requiredActions are no attribute of the profile: one the update
	// leaves out stays.
	if list, ok := rep["requiredActions"].([]any); ok {
		stored["requiredActions"] = list
	} else if _, had := stored["requiredActions"]; !had {
		stored["requiredActions"] = []any{}
	}
	attrs := map[string]any{}
	in, _ := rep["attributes"].(map[string]any)
	for k, v := range in {
		if k != "max_rating" || !s.DeclareRating {
			continue // not declared: dropped, without a word
		}
		vals, _ := v.([]any)
		if len(vals) == 0 {
			continue
		}
		n, err := strconv.Atoi(fmt.Sprint(vals[0]))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"field": "max_rating", "errorMessage": "error-invalid-number", "params": []any{"max_rating"}})
			return false
		}
		if n < 0 || n > 21 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"field": "max_rating", "errorMessage": "error-number-out-of-range", "params": []any{"max_rating", 0, 21}})
			return false
		}
		attrs[k] = []any{strconv.Itoa(n)}
	}
	stored["attributes"] = attrs
	return true
}

// email checks the address apply wrote, as Keycloak 26 does with the bundled
// realm: kept in lower case; gone, its key with it, where it is empty;
// refused by the user profile's email validator when it is not one @ between
// a local part of 1 to 64 characters and a domain, neither ending in a dot;
// and, unless DuplicateEmails, refused when another user has it.
func (s *Server) email(w http.ResponseWriter, stored map[string]any) bool {
	address, _ := stored["email"].(string)
	if strings.TrimSpace(address) == "" {
		delete(stored, "email")
		return true
	}
	typed := address
	address = strings.ToLower(address)
	stored["email"] = address
	local, domain, _ := strings.Cut(address, "@")
	if local == "" || len(local) > 64 || domain == "" || strings.Contains(domain, "@") ||
		strings.HasSuffix(local, ".") || strings.HasSuffix(domain, ".") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"field": "email", "errorMessage": "error-invalid-email", "params": []any{"email", typed}})
		return false
	}
	if !s.DuplicateEmails && s.emailTaken(address, fmt.Sprint(stored["id"])) {
		writeJSON(w, http.StatusConflict, map[string]any{"errorMessage": "User exists with same email"})
		return false
	}
	return true
}

// emailTaken: a user other than except has the address.
func (s *Server) emailTaken(address, except string) bool {
	if strings.TrimSpace(address) == "" {
		return false
	}
	for id, u := range s.users {
		if other, _ := u["email"].(string); id != except && strings.EqualFold(other, strings.TrimSpace(address)) {
			return true
		}
	}
	return false
}

func (s *Server) user(w http.ResponseWriter, r *http.Request, id string, rest []string) {
	stored, ok := s.users[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "User not found"})
		return
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		out := map[string]any{}
		for k, v := range stored {
			out[k] = v
		}
		out["access"] = map[string]any{"manage": true, "view": true, "mapRoles": true, "impersonate": false}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 0 && r.Method == http.MethodPut:
		var rep map[string]any
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unable to read contents from stream"})
			return
		}
		if u, _ := rep["username"].(string); u != "" && u != stored["username"] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"errorMessage": "error-user-attribute-read-only", "field": "username"})
			return
		}
		if !s.race(w) {
			return
		}
		next := map[string]any{"id": id, "username": stored["username"], "enabled": stored["enabled"], "requiredActions": stored["requiredActions"]}
		if ct, ok := stored["createdTimestamp"]; ok {
			next["createdTimestamp"] = ct
		}
		if !s.apply(w, next, rep) {
			return
		}
		s.users[id] = next
		w.WriteHeader(http.StatusNoContent)
	case len(rest) == 0 && r.Method == http.MethodDelete:
		delete(s.users, id)
		delete(s.mappings, id)
		delete(s.clients, id)
		delete(s.passwords, id)
		w.WriteHeader(http.StatusNoContent)
	case len(rest) == 1 && rest[0] == "reset-password" && r.Method == http.MethodPut:
		var body struct {
			Type      string `json:"type"`
			Value     string `json:"value"`
			Temporary bool   `json:"temporary"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Type != "password" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid"})
			return
		}
		if len([]rune(body.Value)) < s.MinPassword {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalidPasswordMinLengthMessage",
				"error_description": fmt.Sprintf("Invalid password: minimum length %d.", s.MinPassword)})
			return
		}
		digits := 0
		for _, c := range body.Value {
			if c >= '0' && c <= '9' {
				digits++
			}
		}
		if digits < s.ExtraDigits {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalidPasswordMinDigitsMessage",
				"error_description": fmt.Sprintf("Invalid password: must contain at least %d numerical digits.", s.ExtraDigits)})
			return
		}
		if body.Temporary {
			stored["requiredActions"] = append(stored["requiredActions"].([]any), "UPDATE_PASSWORD")
		}
		s.passwords[id] = body.Value
		w.WriteHeader(http.StatusNoContent)
	case len(rest) == 1 && rest[0] == "role-mappings" && r.Method == http.MethodGet:
		realm := []any{}
		for _, name := range s.mappings[id] {
			realm = append(realm, map[string]any{"id": s.roles[name], "name": name})
		}
		clients := map[string]any{}
		for client, names := range s.clients[id] {
			list := []any{}
			for _, n := range names {
				list = append(list, map[string]any{"id": "rm-" + n, "name": n})
			}
			clients[client] = map[string]any{"id": "client-" + client, "client": client, "mappings": list}
		}
		out := map[string]any{"realmMappings": realm}
		if len(clients) > 0 {
			out["clientMappings"] = clients
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 3 && rest[0] == "role-mappings" && rest[1] == "realm" && rest[2] == "composite":
		seen := map[string]bool{}
		out := []any{}
		add := func(name string) {
			if !seen[name] {
				seen[name] = true
				out = append(out, map[string]any{"id": s.roles[name], "name": name})
			}
		}
		for _, name := range append(slices.Clone(s.mappings[id]), s.groupRole[id]...) {
			add(name)
			if name == defaultRole {
				add("offline_access")
				add("uma_authorization")
			}
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 3 && rest[0] == "role-mappings" && rest[1] == "realm" && rest[2] == "available":
		out := []any{}
		for name, rid := range s.roles {
			if !slices.Contains(s.mappings[id], name) {
				out = append(out, map[string]any{"id": rid, "name": name})
			}
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 2 && rest[0] == "role-mappings" && rest[1] == "realm" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
		var roles []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&roles); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid"})
			return
		}
		for _, role := range roles {
			if s.roles[role.Name] != role.ID || role.ID == "" {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "Role not found"})
				return
			}
		}
		for _, role := range roles {
			if r.Method == http.MethodPost {
				if !slices.Contains(s.mappings[id], role.Name) {
					s.mappings[id] = append(s.mappings[id], role.Name)
				}
			} else {
				s.mappings[id] = slices.DeleteFunc(s.mappings[id], func(n string) bool { return n == role.Name })
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case len(rest) == 3 && rest[0] == "role-mappings" && rest[1] == "clients":
		// A role of realm-management: one the people client does not hold
		// itself, which Keycloak refuses to let it grant.
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "HTTP 403 Forbidden"})
	default:
		http.NotFound(w, r)
	}
}
