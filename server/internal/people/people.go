// Package people is the realm's people as the People page manages them: one
// account per person, a role — admin or user — and, for a child, a rating
// cap. It works through the realm's people client (package keycloak), and
// keeps the rules that make the page safe to hand to more than one admin:
//
//   - The last enabled admin is never deleted, disabled or made a user: the
//     platform would have nobody left to administer it.
//   - Nobody changes their own role or switches themselves off, or deletes
//     themselves here (an account is deleted by its owner from the apps).
//   - An account that holds a role of realm-management — the first admin, or
//     anyone given one in Keycloak's admin console — is Keycloak's to change.
//     The people client could reset such an account's password or delete it
//     (manage-users reaches every user of the realm); the page does not, so
//     an admin of the platform cannot make themselves one of Keycloak's.
//   - Accounts of the platform's own (the self-test's) are not people and
//     are neither listed nor changed.
//
// A rating cap is the user attribute max_rating, an age from 0 to 21, which
// the viewers' access tokens carry as the claim max_rating (no attribute, no
// claim, no cap). Keycloak drops an attribute its user profile does not
// declare without saying so; every write that sets one reads it back.
package people

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak"
)

// The realm roles a person holds: everyone the user role, an admin the admin
// role besides.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	// RatingAttribute is the user attribute a rating cap is kept in, and
	// the access token's claim.
	RatingAttribute = "max_rating"
	// MaxRating is the highest cap: an age.
	MaxRating = 21

	// realmManagement is the client whose roles make a Keycloak administrator.
	realmManagement = "realm-management"
)

// Person is one account as the People page shows it.
type Person struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
	Enabled     bool   `json:"enabled"`
	// Role is admin or user.
	Role string `json:"role"`
	// MaxRating is the rating cap, an age; null for none.
	MaxRating *int `json:"maxRating"`
	// CreatedAt is when the account was made; null for one Keycloak keeps no
	// time of (an imported one).
	CreatedAt *time.Time `json:"createdAt"`
	// Managed says where the account is changed: "" on the People page,
	// "keycloak" in Keycloak's admin console (it holds a realm-management
	// role).
	Managed string `json:"managed,omitempty"`
}

// Rules, broken.
var (
	ErrNotFound     = errors.New("no such person")
	ErrUsernameUsed = errors.New("that username is taken")
)

// Refused is a change the rules do not allow, in words for the admin.
type Refused struct{ Message string }

func (e *Refused) Error() string { return e.Message }

func refused(format string, args ...any) error {
	return &Refused{Message: fmt.Sprintf(format, args...)}
}

// Invalid is a value the page does not take, in words for the admin.
type Invalid struct{ Message string }

func (e *Invalid) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &Invalid{Message: fmt.Sprintf(format, args...)}
}

// Service is the realm's people.
type Service struct {
	kc *keycloak.Client
	// adminRole and userRole are the realm roles behind admin and user.
	adminRole, userRole string
	// hidden are usernames of the platform's own accounts.
	hidden []string
}

// New is the realm's people through kc. hidden names the platform's own
// accounts, which are not people.
func New(kc *keycloak.Client, adminRole, userRole string, hidden []string) *Service {
	return &Service{kc: kc, adminRole: adminRole, userRole: userRole, hidden: hidden}
}

// Configured reports whether the people client has its credential.
func (s *Service) Configured() bool { return s != nil && s.kc.Configured() }

// Keycloak is the client the service works through.
func (s *Service) Keycloak() *keycloak.Client { return s.kc }

// account is a user and what decides their role.
type account struct {
	user      *keycloak.User
	effective []string
	mappings  keycloak.Mappings
}

func (a account) admin(role string) bool { return slices.Contains(a.effective, role) }

// keycloakAdmin: the account holds a role of realm-management directly.
func (a account) keycloakAdmin() bool { return len(a.mappings.Clients[realmManagement]) > 0 }

func (s *Service) isHidden(u *keycloak.User) bool {
	return slices.Contains(s.hidden, u.Username())
}

// person is how an account reads on the page.
func (s *Service) person(a account) Person {
	u := a.user
	p := Person{
		ID: u.ID(), Username: u.Username(), DisplayName: displayName(u), Email: u.Email(),
		Enabled: u.Enabled(), Role: RoleUser,
	}
	if a.admin(s.adminRole) {
		p.Role = RoleAdmin
	}
	if v, ok := u.Attribute(RatingAttribute); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			p.MaxRating = &n
		}
	}
	if t := u.Created(); !t.IsZero() {
		p.CreatedAt = &t
	}
	if a.keycloakAdmin() {
		p.Managed = "keycloak"
	}
	return p
}

// displayName is a person's name as the page shows it: first and last name,
// else the username.
func displayName(u *keycloak.User) string {
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName()) + " " + strings.TrimSpace(u.LastName()))
	if name == "" {
		return u.Username()
	}
	return name
}

// load reads one account: the user and their roles.
func (s *Service) load(ctx context.Context, u *keycloak.User) (account, error) {
	a := account{user: u}
	var wg sync.WaitGroup
	var errEff, errMap error
	wg.Add(2)
	go func() { defer wg.Done(); a.effective, errEff = s.kc.EffectiveRealmRoles(ctx, u.ID()) }()
	go func() { defer wg.Done(); a.mappings, errMap = s.kc.Mappings(ctx, u.ID()) }()
	wg.Wait()
	if errEff != nil {
		return a, errEff
	}
	return a, errMap
}

// accounts reads every person's account, a few at a time.
func (s *Service) accounts(ctx context.Context) ([]account, error) {
	users, err := s.kc.Users(ctx)
	if err != nil {
		return nil, err
	}
	var kept []*keycloak.User
	for _, u := range users {
		if !s.isHidden(u) {
			kept = append(kept, u)
		}
	}
	out := make([]account, len(kept))
	errs := make([]error, len(kept))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, u := range kept {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i], errs[i] = s.load(ctx, u)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// one reads a person's account by id; a hidden or missing one is ErrNotFound.
func (s *Service) one(ctx context.Context, id string) (account, error) {
	u, err := s.kc.User(ctx, id)
	if errors.Is(err, keycloak.ErrNotFound) {
		return account{}, ErrNotFound
	}
	if err != nil {
		return account{}, err
	}
	if s.isHidden(u) {
		return account{}, ErrNotFound
	}
	return s.load(ctx, u)
}

// List is everyone, by display name.
func (s *Service) List(ctx context.Context) ([]Person, error) {
	accts, err := s.accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Person, 0, len(accts))
	for _, a := range accts {
		out = append(out, s.person(a))
	}
	slices.SortFunc(out, func(a, b Person) int {
		if c := strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName)); c != 0 {
			return c
		}
		return strings.Compare(a.Username, b.Username)
	})
	return out, nil
}

// Get is one person.
func (s *Service) Get(ctx context.Context, id string) (Person, error) {
	a, err := s.one(ctx, id)
	if err != nil {
		return Person{}, err
	}
	return s.person(a), nil
}

// ─── validation ──────────────────────────────────────────────────────────────

// Usernames: lower-case letters, digits, '.', '_' and '-', 3 to 64 of them,
// starting and ending with a letter or a digit. What a person types on a TV.
const (
	minUsername = 3
	maxUsername = 64
	maxName     = 100
)

// CleanUsername reads a username as typed: trimmed, lower-cased.
func CleanUsername(raw string) (string, error) {
	u := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case u == "":
		return "", invalid("A username is needed: what the person signs in with.")
	case len(u) < minUsername || len(u) > maxUsername:
		return "", invalid("A username has %d to %d characters.", minUsername, maxUsername)
	}
	for i, r := range u {
		letter := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if !letter && (i == 0 || i == len(u)-1 || !strings.ContainsRune("._-", r)) {
			return "", invalid("A username has letters a to z, digits, and . _ - between them.")
		}
	}
	return u, nil
}

// nameProhibited is what Keycloak's person-name validator refuses in a name.
const nameProhibited = `<>&"$%!#?§;*~/\|^=[]{}()`

// CleanName reads a display name: trimmed, 1 to 100 characters, none that
// Keycloak refuses in a name.
func CleanName(raw string) (string, error) {
	n := strings.TrimSpace(raw)
	switch {
	case n == "":
		return "", invalid("A name is needed: what the page and the apps call the person.")
	case utf8.RuneCountInString(n) > maxName:
		return "", invalid("A name has at most %d characters.", maxName)
	}
	for _, r := range n {
		if unicode.IsControl(r) || strings.ContainsRune(nameProhibited, r) {
			return "", invalid("A name may not hold any of %s.", nameProhibited)
		}
	}
	return n, nil
}

// CheckRole is admin or user.
func CheckRole(role string) error {
	if role != RoleAdmin && role != RoleUser {
		return invalid(`A role is "admin" or "user".`)
	}
	return nil
}

// CheckRating is a cap from 0 to MaxRating.
func CheckRating(n int) error {
	if n < 0 || n > MaxRating {
		return invalid("A rating cap is an age from 0 to %d.", MaxRating)
	}
	return nil
}

// ─── changes ─────────────────────────────────────────────────────────────────

// NewPerson is a person to add.
type NewPerson struct {
	Username    string
	DisplayName string
	Role        string
	MaxRating   *int
}

// Create adds a person: enabled, without a password — the invite sets it —
// holding the user role, and the admin role for an admin.
func (s *Service) Create(ctx context.Context, np NewPerson) (Person, error) {
	username, err := CleanUsername(np.Username)
	if err != nil {
		return Person{}, err
	}
	name, err := CleanName(np.DisplayName)
	if err != nil {
		return Person{}, err
	}
	if err := CheckRole(np.Role); err != nil {
		return Person{}, err
	}
	attrs := map[string]string{}
	if np.MaxRating != nil {
		if err := CheckRating(*np.MaxRating); err != nil {
			return Person{}, err
		}
		if np.Role == RoleAdmin {
			return Person{}, adminCapped()
		}
		attrs[RatingAttribute] = strconv.Itoa(*np.MaxRating)
	}
	if slices.Contains(s.hidden, username) {
		return Person{}, ErrUsernameUsed
	}
	id, err := s.kc.Create(ctx, keycloak.NewUser{Username: username, FirstName: name, Attributes: attrs})
	if errors.Is(err, keycloak.ErrConflict) {
		return Person{}, ErrUsernameUsed
	}
	if err != nil {
		return Person{}, err
	}
	// A person half made — no role, or a cap Keycloak did not keep — is not
	// left behind.
	undo := func(cause error) (Person, error) {
		if derr := s.kc.Delete(context.WithoutCancel(ctx), id); derr != nil && !errors.Is(derr, keycloak.ErrNotFound) {
			return Person{}, fmt.Errorf("%w (and %s could not be removed again: %v)", cause, username, derr)
		}
		return Person{}, cause
	}
	want := []string{s.userRole}
	if np.Role == RoleAdmin {
		want = append(want, s.adminRole)
	}
	if err := s.grant(ctx, id, want); err != nil {
		return undo(err)
	}
	a, err := s.one(ctx, id)
	if err != nil {
		return undo(err)
	}
	if err := keptRating(a.user, np.MaxRating); err != nil {
		return undo(err)
	}
	return s.person(a), nil
}

// grant maps the named realm roles to a user, those they lack.
func (s *Service) grant(ctx context.Context, id string, names []string) error {
	available, err := s.kc.AvailableRealmRoles(ctx, id)
	if err != nil {
		return err
	}
	var add []keycloak.Role
	for _, name := range names {
		i := slices.IndexFunc(available, func(r keycloak.Role) bool { return r.Name == name })
		if i < 0 {
			continue // held already, or not a role of this realm (checked below)
		}
		add = append(add, available[i])
	}
	if err := s.kc.AddRealmRoles(ctx, id, add); err != nil {
		return err
	}
	effective, err := s.kc.EffectiveRealmRoles(ctx, id)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !slices.Contains(effective, name) {
			return fmt.Errorf("the realm has no role %s to grant", name)
		}
	}
	return nil
}

// ErrRatingNotKept: Keycloak dropped the cap — its user profile does not
// declare max_rating (the realm Job declares it).
var ErrRatingNotKept = errors.New("keycloak did not keep the rating cap")

func keptRating(u *keycloak.User, want *int) error {
	v, ok := u.Attribute(RatingAttribute)
	switch {
	case want == nil && !ok:
		return nil
	case want != nil && ok && v == strconv.Itoa(*want):
		return nil
	}
	return ErrRatingNotKept
}

func adminCapped() error {
	return invalid("An admin can change every rating cap, their own too: a cap is for a user.")
}

// Change is what a PATCH changes; nil leaves it as it is. ClearRating removes
// the cap.
type Change struct {
	DisplayName *string
	Role        *string
	MaxRating   *int
	ClearRating bool
	Enabled     *bool
}

// Update changes a person, as caller (the subject of the admin's token).
func (s *Service) Update(ctx context.Context, caller, id string, ch Change) (Person, error) {
	a, err := s.one(ctx, id)
	if err != nil {
		return Person{}, err
	}
	p := s.person(a)
	if a.keycloakAdmin() {
		return Person{}, keycloakManaged(p)
	}
	name := p.DisplayName
	if ch.DisplayName != nil {
		if name, err = CleanName(*ch.DisplayName); err != nil {
			return Person{}, err
		}
	}
	role := p.Role
	if ch.Role != nil {
		if err := CheckRole(*ch.Role); err != nil {
			return Person{}, err
		}
		role = *ch.Role
	}
	enabled := p.Enabled
	if ch.Enabled != nil {
		enabled = *ch.Enabled
	}
	rating := p.MaxRating
	switch {
	case ch.ClearRating:
		rating = nil
	case ch.MaxRating != nil:
		if err := CheckRating(*ch.MaxRating); err != nil {
			return Person{}, err
		}
		rating = ch.MaxRating
	}
	if role == RoleAdmin && rating != nil {
		return Person{}, adminCapped()
	}
	if id == caller && (role != p.Role || enabled != p.Enabled) {
		return Person{}, refused("You cannot change your own role or switch yourself off: another admin can.")
	}
	if p.Role == RoleAdmin && p.Enabled && (role != RoleAdmin || !enabled) {
		if err := s.notLastAdmin(ctx, id, p); err != nil {
			return Person{}, err
		}
	}

	u := a.user
	if name != p.DisplayName {
		u.SetName(name)
	}
	if enabled != p.Enabled {
		u.SetEnabled(enabled)
	}
	if !sameRating(rating, p.MaxRating) {
		v := ""
		if rating != nil {
			v = strconv.Itoa(*rating)
		}
		u.SetAttribute(RatingAttribute, v)
	}
	if name != p.DisplayName || enabled != p.Enabled || !sameRating(rating, p.MaxRating) {
		if err := s.kc.Update(ctx, u); err != nil {
			return Person{}, err
		}
	}
	if role != p.Role {
		if err := s.setRole(ctx, a, role); err != nil {
			return Person{}, err
		}
	}
	after, err := s.one(ctx, id)
	if err != nil {
		return Person{}, err
	}
	if err := keptRating(after.user, rating); err != nil {
		return Person{}, err
	}
	np := s.person(after)
	if np.Role != role {
		// The admin role through a group or a composite stays, whatever is
		// unmapped here.
		return np, refused("%s holds the admin role through a group or a composite role in Keycloak: change it in Keycloak's admin console.", np.DisplayName)
	}
	return np, nil
}

func sameRating(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// setRole maps the admin role to an account, or unmaps it; the user role
// either way.
func (s *Service) setRole(ctx context.Context, a account, role string) error {
	if role == RoleAdmin {
		return s.grant(ctx, a.user.ID(), []string{s.userRole, s.adminRole})
	}
	var drop []keycloak.Role
	for _, r := range a.mappings.Realm {
		if r.Name == s.adminRole {
			drop = append(drop, r)
		}
	}
	if err := s.kc.RemoveRealmRoles(ctx, a.user.ID(), drop); err != nil {
		return err
	}
	return s.grant(ctx, a.user.ID(), []string{s.userRole})
}

// notLastAdmin refuses a change that would leave no enabled admin but p.
func (s *Service) notLastAdmin(ctx context.Context, id string, p Person) error {
	accts, err := s.accounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range accts {
		if a.user.ID() != id && a.user.Enabled() && a.admin(s.adminRole) {
			return nil
		}
	}
	return refused("%s is the last admin: make someone else an admin first.", p.DisplayName)
}

func keycloakManaged(p Person) error {
	return refused("%s administers Keycloak itself: change this account in Keycloak's admin console.", p.DisplayName)
}

// CanDelete says whether caller may delete the person id, and answers them.
func (s *Service) CanDelete(ctx context.Context, caller, id string) (Person, error) {
	a, err := s.one(ctx, id)
	if err != nil {
		return Person{}, err
	}
	p := s.person(a)
	switch {
	case a.keycloakAdmin():
		return p, keycloakManaged(p)
	case id == caller:
		return p, refused("You cannot delete your own account here: another admin can, or you delete it from the apps.")
	case p.Role == RoleAdmin && p.Enabled:
		if err := s.notLastAdmin(ctx, id, p); err != nil {
			return p, err
		}
	}
	return p, nil
}

// Delete deletes a person's account. Gone already is no error.
func (s *Service) Delete(ctx context.Context, id string) error {
	err := s.kc.Delete(ctx, id)
	if errors.Is(err, keycloak.ErrNotFound) {
		return nil
	}
	return err
}

// CanDeleteOwn says whether the person id may delete their own account: not
// the last admin, not a Keycloak administrator, not one of the platform's own
// accounts. A person already gone is ErrNotFound.
func (s *Service) CanDeleteOwn(ctx context.Context, id string) (Person, error) {
	a, err := s.one(ctx, id)
	if err != nil {
		return Person{}, err
	}
	p := s.person(a)
	switch {
	case a.keycloakAdmin():
		return p, refused("Your account administers Keycloak itself: it is removed in Keycloak's admin console.")
	case p.Role == RoleAdmin && p.Enabled:
		if err := s.notLastAdmin(ctx, id, p); err != nil {
			return p, refused("You are the last admin: make someone else an admin first.")
		}
	}
	return p, nil
}

// CanInvite says whether caller may send person id an invite.
func (s *Service) CanInvite(ctx context.Context, caller, id string) (Person, error) {
	a, err := s.one(ctx, id)
	if err != nil {
		return Person{}, err
	}
	p := s.person(a)
	switch {
	case a.keycloakAdmin():
		return p, keycloakManaged(p)
	case id == caller:
		return p, refused("You are signed in already: change your own password in your account settings.")
	case !p.Enabled:
		return p, refused("%s is switched off: switch them on first, or the invite leads nowhere.", p.DisplayName)
	}
	return p, nil
}

// Invitee is the account an invite is for, as its page shows it; ErrNotFound
// for one that is gone, switched off or not a person's.
func (s *Service) Invitee(ctx context.Context, id string) (Person, error) {
	a, err := s.one(ctx, id)
	if err != nil {
		return Person{}, err
	}
	p := s.person(a)
	if !p.Enabled || a.keycloakAdmin() {
		return Person{}, ErrNotFound
	}
	return p, nil
}

// Accept sets the password an invited person chose, and clears whatever
// Keycloak would ask them at their first sign-in (and a lockout of failed
// attempts before it).
func (s *Service) Accept(ctx context.Context, id, password string) error {
	if err := s.kc.SetPassword(ctx, id, password); err != nil {
		return err
	}
	u, err := s.kc.User(ctx, id)
	if err != nil {
		return err
	}
	u.ClearRequiredActions()
	if err := s.kc.Update(ctx, u); err != nil {
		return err
	}
	_ = s.kc.ClearLockout(ctx, id)
	return nil
}
