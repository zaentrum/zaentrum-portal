// Package keycloak is as much of Keycloak's admin REST API as the People page
// uses, called as the realm's people client: a confidential client whose
// service account holds view-users, query-users and manage-users of
// realm-management and nothing else (the platform chart's realm Job keeps it
// so). It lists, makes, changes and deletes the realm's users, maps the
// realm's roles to them and sets a password — and can do nothing more, which
// is the point: not read a role, a client or the realm's settings, not grant
// one of Keycloak's own administrator roles.
//
// It never answers with what Keycloak said. Every failure is one of the
// errors below, which the API words for the person who asked; Keycloak's own
// message goes to the log at most.
package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// callTimeout bounds one call to Keycloak.
const callTimeout = 10 * time.Second

// pageSize is how many users one list call asks for.
const pageSize = 100

// MaxUsers is the most users a listing reads: a household has a handful, and
// a realm with more than this is not one the People page is for.
const MaxUsers = 2000

var (
	// ErrUnreachable: Keycloak did not answer, or not as Keycloak.
	ErrUnreachable = errors.New("keycloak did not answer")
	// ErrRefused: Keycloak refused the people client — its secret, or a role
	// its service account lacks.
	ErrRefused = errors.New("keycloak refused the people client")
	// ErrNotFound: no such user.
	ErrNotFound = errors.New("no such user")
	// ErrConflict: a user of that username exists.
	ErrConflict = errors.New("a user of that username exists")
)

// Invalid is Keycloak declining a value: a user profile attribute it does not
// take (Field, the attribute) or a password its policy refuses (Field
// "password"). Code is Keycloak's message key, e.g. error-number-out-of-range
// or invalidPasswordMinLengthMessage; Params are its parameters.
type Invalid struct {
	Field  string
	Code   string
	Params []string
	// Description is Keycloak's own wording of a password refusal.
	Description string
}

func (e *Invalid) Error() string {
	return fmt.Sprintf("keycloak declined %s: %s", e.Field, e.Code)
}

// Failed is any other answer Keycloak gave: its status, and its error key
// when it named one.
type Failed struct {
	Status int
	Code   string
}

func (e *Failed) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("keycloak answered %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("keycloak answered %d", e.Status)
}

// Config says where Keycloak is and how the people client signs in.
type Config struct {
	// URL is Keycloak's base, with its relative path: http://keycloak:80/auth.
	URL          string
	Realm        string
	ClientID     string
	ClientSecret string
}

// Client calls the admin API as the people client.
type Client struct {
	cfg  Config
	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New is a client for cfg. Every call fails with ErrRefused while the
// client's secret is empty: the people client exists, its credential does
// not (yet).
func New(cfg Config) *Client {
	cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	return &Client{cfg: cfg, http: &http.Client{
		Timeout: callTimeout,
		// An answer that redirects is no answer of the admin API's.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Configured reports whether the client has a secret to sign in with.
func (c *Client) Configured() bool { return c != nil && c.cfg.ClientSecret != "" }

// ─── users ───────────────────────────────────────────────────────────────────

// User is a user of the realm, as the admin API represents one. The
// representation is kept whole: Keycloak replaces a user on an update, so a
// change is written back over everything it read.
type User struct {
	rep map[string]any
}

func (u *User) str(k string) string {
	s, _ := u.rep[k].(string)
	return s
}

// ID is the user's id, which is also the subject of their tokens.
func (u *User) ID() string       { return u.str("id") }
func (u *User) Username() string { return u.str("username") }
func (u *User) FirstName() string {
	return u.str("firstName")
}
func (u *User) LastName() string { return u.str("lastName") }
func (u *User) Email() string    { return u.str("email") }

// Enabled reports whether the user may sign in.
func (u *User) Enabled() bool {
	b, _ := u.rep["enabled"].(bool)
	return b
}

// Created is when the user was made; zero for one an import made, which
// Keycloak gives no time.
func (u *User) Created() time.Time {
	switch v := u.rep["createdTimestamp"].(type) {
	case float64:
		return time.UnixMilli(int64(v)).UTC()
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return time.UnixMilli(n).UTC()
		}
	}
	return time.Time{}
}

// Attribute is the user's first value of a user attribute, and whether it has
// one.
func (u *User) Attribute(name string) (string, bool) {
	attrs, _ := u.rep["attributes"].(map[string]any)
	vals, _ := attrs[name].([]any)
	if len(vals) == 0 {
		return "", false
	}
	s, ok := vals[0].(string)
	return s, ok
}

// SetAttribute sets a user attribute to one value, or removes it for "".
func (u *User) SetAttribute(name, value string) {
	attrs, _ := u.rep["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	if value == "" {
		delete(attrs, name)
	} else {
		attrs[name] = []any{value}
	}
	u.rep["attributes"] = attrs
}

// SetName sets the user's first name to name and clears the last: a person
// here has one name, the one the People page shows.
func (u *User) SetName(name string) {
	u.rep["firstName"] = name
	u.rep["lastName"] = ""
}

// SetEnabled lets the user sign in, or not.
func (u *User) SetEnabled(on bool) { u.rep["enabled"] = on }

// ClearRequiredActions drops whatever the user would be asked to do at their
// next sign-in.
func (u *User) ClearRequiredActions() { u.rep["requiredActions"] = []any{} }

// NewUser is a user to make: enabled, with no password and nothing to do at
// the first sign-in. Attributes holds one value each.
type NewUser struct {
	Username   string
	FirstName  string
	Attributes map[string]string
}

// Users lists the realm's users, service accounts aside (the admin API leaves
// them out), at most MaxUsers.
func (c *Client) Users(ctx context.Context) ([]*User, error) {
	var out []*User
	for first := 0; first < MaxUsers; first += pageSize {
		var page []map[string]any
		q := url.Values{"briefRepresentation": {"false"}, "first": {strconv.Itoa(first)}, "max": {strconv.Itoa(pageSize)}}
		if err := c.call(ctx, http.MethodGet, "/users?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, rep := range page {
			out = append(out, &User{rep: rep})
		}
		if len(page) < pageSize {
			break
		}
	}
	return out, nil
}

// User reads one user.
func (c *Client) User(ctx context.Context, id string) (*User, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	var rep map[string]any
	if err := c.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id), nil, &rep); err != nil {
		return nil, err
	}
	return &User{rep: rep}, nil
}

// Create makes a user and answers its id.
func (c *Client) Create(ctx context.Context, nu NewUser) (string, error) {
	attrs := map[string]any{}
	for k, v := range nu.Attributes {
		attrs[k] = []string{v}
	}
	rep := map[string]any{
		"username": nu.Username, "enabled": true, "firstName": nu.FirstName,
		"attributes": attrs, "requiredActions": []string{},
	}
	loc, err := c.callLocation(ctx, http.MethodPost, "/users", rep)
	if err != nil {
		return "", err
	}
	id := loc[strings.LastIndex(loc, "/")+1:]
	if !validID(id) {
		return "", &Failed{Status: http.StatusCreated, Code: "no-user-id"}
	}
	return id, nil
}

// Update writes u back, whole.
func (c *Client) Update(ctx context.Context, u *User) error {
	return c.call(ctx, http.MethodPut, "/users/"+url.PathEscape(u.ID()), u.rep, nil)
}

// Delete deletes a user. One that is gone already is ErrNotFound.
func (c *Client) Delete(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	return c.call(ctx, http.MethodDelete, "/users/"+url.PathEscape(id), nil, nil)
}

// SetPassword gives a user a password they keep (not temporary). A password
// the realm's policy refuses is an *Invalid of Field "password".
func (c *Client) SetPassword(ctx context.Context, id, password string) error {
	body := map[string]any{"type": "password", "temporary": false, "value": password}
	err := c.call(ctx, http.MethodPut, "/users/"+url.PathEscape(id)+"/reset-password", body, nil)
	var inv *Invalid
	if errors.As(err, &inv) {
		inv.Field = "password"
	}
	return err
}

// ClearLockout ends a brute-force lockout of the user, if there is one.
func (c *Client) ClearLockout(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/attack-detection/brute-force/users/"+url.PathEscape(id), nil, nil)
}

// ─── roles ───────────────────────────────────────────────────────────────────

// Role is a realm role as a role mapping names it.
type Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Mappings is what a user holds directly: realm roles, and the roles of each
// client by its clientId.
type Mappings struct {
	Realm   []Role
	Clients map[string][]string
}

// Mappings reads a user's direct role mappings.
func (c *Client) Mappings(ctx context.Context, id string) (Mappings, error) {
	var raw struct {
		RealmMappings  []Role `json:"realmMappings"`
		ClientMappings map[string]struct {
			Mappings []Role `json:"mappings"`
		} `json:"clientMappings"`
	}
	if err := c.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id)+"/role-mappings", nil, &raw); err != nil {
		return Mappings{}, err
	}
	out := Mappings{Realm: raw.RealmMappings, Clients: map[string][]string{}}
	for client, m := range raw.ClientMappings {
		for _, r := range m.Mappings {
			out.Clients[client] = append(out.Clients[client], r.Name)
		}
	}
	return out, nil
}

// EffectiveRealmRoles are the realm roles a user holds, through composites
// and groups too: what their tokens carry.
func (c *Client) EffectiveRealmRoles(ctx context.Context, id string) ([]string, error) {
	var roles []Role
	if err := c.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id)+"/role-mappings/realm/composite", nil, &roles); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	return out, nil
}

// AvailableRealmRoles are the realm roles the people client may map to the
// user and the user does not hold. (It may not read a role by name: this is
// how it learns a role's id.)
func (c *Client) AvailableRealmRoles(ctx context.Context, id string) ([]Role, error) {
	var roles []Role
	err := c.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id)+"/role-mappings/realm/available", nil, &roles)
	return roles, err
}

// AddRealmRoles maps realm roles to a user.
func (c *Client) AddRealmRoles(ctx context.Context, id string, roles []Role) error {
	if len(roles) == 0 {
		return nil
	}
	return c.call(ctx, http.MethodPost, "/users/"+url.PathEscape(id)+"/role-mappings/realm", roles, nil)
}

// RemoveRealmRoles unmaps realm roles from a user.
func (c *Client) RemoveRealmRoles(ctx context.Context, id string, roles []Role) error {
	if len(roles) == 0 {
		return nil
	}
	return c.call(ctx, http.MethodDelete, "/users/"+url.PathEscape(id)+"/role-mappings/realm", roles, nil)
}

// ─── the wire ────────────────────────────────────────────────────────────────

// validID is what a Keycloak user id looks like (a UUID, or a federated
// user's id): nothing that changes the path it is put into.
func validID(id string) bool {
	if id == "" || len(id) > 255 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == ':' || r == '.') {
			return false
		}
	}
	return id != "." && id != ".."
}

func (c *Client) adminURL(path string) string {
	return c.cfg.URL + "/admin/realms/" + url.PathEscape(c.cfg.Realm) + path
}

// call sends one admin API request with the people client's token, decoding
// the answer into out when there is one. A token the admin API no longer
// takes is replaced once.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	_, err := c.send(ctx, method, path, body, out)
	return err
}

// callLocation is call for a create: it answers the Location of what was
// made.
func (c *Client) callLocation(ctx context.Context, method, path string, body any) (string, error) {
	h, err := c.send(ctx, method, path, body, nil)
	if err != nil {
		return "", err
	}
	return h.Get("Location"), nil
}

func (c *Client) send(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		var rdr io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			rdr = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.adminURL(path), rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.forgetToken()
			continue
		}
		if resp.StatusCode/100 != 2 {
			return nil, answerError(resp.StatusCode, raw)
		}
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return nil, fmt.Errorf("%w: an answer that is no JSON", ErrUnreachable)
			}
		}
		return resp.Header, nil
	}
}

// answerError reads an admin API refusal: the errors a user profile or a
// password policy gives, and Keycloak's error keys.
func answerError(status int, raw []byte) error {
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		ErrorMessage     string `json:"errorMessage"`
		Field            string `json:"field"`
		Params           []any  `json:"params"`
		Errors           []struct {
			Field        string `json:"field"`
			ErrorMessage string `json:"errorMessage"`
			Params       []any  `json:"params"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &e)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrRefused
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	case http.StatusBadRequest:
		switch {
		case len(e.Errors) > 0:
			return &Invalid{Field: e.Errors[0].Field, Code: e.Errors[0].ErrorMessage, Params: params(e.Errors[0].Params)}
		case e.Field != "" || e.ErrorMessage != "":
			return &Invalid{Field: e.Field, Code: e.ErrorMessage, Params: params(e.Params)}
		case strings.HasPrefix(e.Error, "invalidPassword"):
			return &Invalid{Field: "password", Code: e.Error, Description: e.ErrorDescription}
		}
	}
	code := e.Error
	if code == "" {
		code = e.ErrorMessage
	}
	if len(code) > 80 {
		code = code[:80]
	}
	return &Failed{Status: status, Code: code}
}

func params(in []any) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, fmt.Sprint(p))
	}
	return out
}

// ─── the people client's token ───────────────────────────────────────────────

// accessToken is a token of the people client from the client credentials
// grant, kept until shortly before it expires.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	if !c.Configured() {
		return "", ErrRefused
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.cfg.ClientID}, "client_secret": {c.cfg.ClientSecret}}
	endpoint := c.cfg.URL + "/realms/" + url.PathEscape(c.cfg.Realm) + "/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		// invalid_client: a secret Keycloak does not take, or no such client.
		return "", ErrRefused
	case resp.StatusCode/100 != 2:
		return "", &Failed{Status: resp.StatusCode, Code: "token"}
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("%w: a token answer without a token", ErrUnreachable)
	}
	life := time.Duration(tok.ExpiresIn) * time.Second
	if life <= 0 {
		life = time.Minute
	}
	// Renewed a little before Keycloak would refuse it.
	c.token, c.expires = tok.AccessToken, time.Now().Add(life*3/4)
	return c.token, nil
}

func (c *Client) forgetToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}
