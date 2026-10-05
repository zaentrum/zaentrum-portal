package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/chino"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/people"
	"github.com/zaentrum/zaentrum-portal/server/internal/ratelimit"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// ─── people ──────────────────────────────────────────────────────────────────
//
// One account per person. An admin adds people on the People page, gives a
// child a rating cap, and sends each an invite link; the person opens it —
// without signing in, there is nothing to sign in with yet — and chooses their
// own password. portal-api does it through the platform realm's people client
// (package people, package keycloak), which may view, query and manage the
// realm's users and nothing else, and keeps the invites (store, migration
// 014): their tokens' SHA-256, never the tokens.
//
//	GET    /people               admin   everyone, with their invite's state
//	POST   /people               admin   add a person; answers their invite link
//	PATCH  /people/{id}          admin   name, role, rating cap, switched on or off
//	DELETE /people/{id}          admin   delete a person, chino's data of them first, their notices after
//	POST   /people/{id}/invite   admin   a new invite link; older ones stop working
//	GET    /invites/{token}      public  whose invite it is, while it is open
//	POST   /invites/{token}      public  set the password, once
//	DELETE /me                   chino-api, for the signed-in person: their account
//
// With an external identity provider people live there: the page says so and
// links there, and nothing here changes anyone.

// inviteStore is the invites as the People page uses them. *store.Store
// implements it; tests substitute an in-memory one.
type inviteStore interface {
	CreateInvite(ctx context.Context, userID, createdBy string, hash []byte, expires time.Time) (model.Invite, error)
	LatestInvites(ctx context.Context) (map[string]model.Invite, error)
	OpenInvite(ctx context.Context, hash []byte, now time.Time) (model.Invite, error)
	UseInvite(ctx context.Context, hash []byte, now time.Time, use func(model.Invite) error) error
	RevokeInvites(ctx context.Context, userID string) error
	DeleteInvites(ctx context.Context, userID string) error
}

// The public invite endpoints' limits: per client address, and per token. A
// person opens their link and sets a password in a request or three; a guess
// at a token is hopeless — 32 random bytes — and the limits keep a flood of
// them off the database and Keycloak.
const (
	inviteIPBurst    = 20
	inviteIPEvery    = 15 * time.Second
	inviteTokenBurst = 10
	inviteTokenEvery = time.Minute
	// maxInviteBody bounds the one field a password POST sends.
	maxInviteBody = 4 << 10
)

// newInviteLimits are the two limiters of the public invite endpoints.
func newInviteLimits() (ip, token *ratelimit.Limiter) {
	return ratelimit.New(inviteIPBurst, inviteIPEvery), ratelimit.New(inviteTokenBurst, inviteTokenEvery)
}

// People modes, as GET /people says them.
const (
	peopleBundled     = "bundled"     // managed here
	peopleExternal    = "external"    // in an identity provider the platform does not manage
	peopleUnavailable = "unavailable" // managed here, but the people client has no secret
)

// notSetUp says what the People page needs, when its client has no secret.
const notSetUp = "The People page is not set up: it needs Secret zaentrum-people (key client-secret). The operator makes it; where Secrets are made by hand, make it with a random value, and the realm Job sets it in the realm."

func (a *API) peopleMode() string {
	switch {
	case a.people == nil:
		return peopleExternal
	case !a.people.Configured():
		return peopleUnavailable
	}
	return peopleBundled
}

// peopleReady guards a change: only where people are managed here.
func (a *API) peopleReady(w http.ResponseWriter) bool {
	switch a.peopleMode() {
	case peopleExternal:
		http.Error(w, "People live in your identity provider: add and change them there.", http.StatusConflict)
		return false
	case peopleUnavailable:
		http.Error(w, notSetUp, http.StatusServiceUnavailable)
		return false
	}
	return true
}

// manageURL is where people are managed with an external provider: the one
// configured, else — for an issuer that is a Keycloak realm — that realm's
// admin console, else the issuer's origin.
func manageURL(configured, issuer string) string {
	if configured != "" {
		return configured
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return ""
	}
	if i := strings.LastIndex(u.Path, "/realms/"); i >= 0 {
		realm := strings.Trim(u.Path[i+len("/realms/"):], "/")
		if realm != "" && !strings.Contains(realm, "/") {
			return u.Scheme + "://" + u.Host + u.Path[:i] + "/admin/" + realm + "/console/"
		}
	}
	return u.Scheme + "://" + u.Host + "/"
}

// peopleDoc is GET /people.
type peopleDoc struct {
	// Mode: bundled (managed here), external (manageUrl says where), or
	// unavailable (note says why).
	Mode      string       `json:"mode"`
	ManageURL string       `json:"manageUrl,omitempty"`
	Note      string       `json:"note,omitempty"`
	People    []personView `json:"people"`
	// DeletesData: deleting a person also deletes chino's data of them.
	DeletesData bool `json:"deletesData"`
	// InviteDays is how many days an invite link holds.
	InviteDays int `json:"inviteDays"`
}

// personView is a person as the page lists them.
type personView struct {
	people.Person
	// Invite is the person's newest invite, never its token; null when none
	// was sent from here.
	Invite *inviteView `json:"invite"`
	// Self: the person is the caller.
	Self bool `json:"self"`
}

type inviteView struct {
	Status    string     `json:"status"` // pending | expired | used | revoked
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
}

func viewInvite(i model.Invite, now time.Time) *inviteView {
	return &inviteView{Status: i.Status(now), CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt, UsedAt: i.UsedAt}
}

// inviteLink is a new invite, as it is shown once: the link and its token
// are in no other answer, and nowhere in portal-api's store.
type inviteLink struct {
	URL       string    `json:"url"`
	Path      string    `json:"path"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (a *API) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// caller is the subject of the request's token: the person's Keycloak id.
func caller(r *http.Request) string {
	if p, _ := auth.PrincipalFrom(r.Context()); p != nil {
		return p.Subject
	}
	return ""
}

// listPeople handles GET /people.
func (a *API) listPeople(w http.ResponseWriter, r *http.Request) {
	doc := peopleDoc{Mode: a.peopleMode(), People: []personView{}, DeletesData: a.chino != nil,
		InviteDays: int(a.cfg.InviteTTL.Round(24*time.Hour) / (24 * time.Hour))}
	switch doc.Mode {
	case peopleExternal:
		doc.ManageURL = manageURL(a.cfg.PeopleManageURL, a.cfg.OIDCIssuer)
		writeJSON(w, http.StatusOK, doc)
		return
	case peopleUnavailable:
		doc.Note = notSetUp
		writeJSON(w, http.StatusOK, doc)
		return
	}
	list, err := a.people.List(r.Context())
	if err != nil {
		peopleFailure(w, err)
		return
	}
	invites, err := a.invites.LatestInvites(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	now, me := a.clock(), caller(r)
	for _, p := range list {
		v := personView{Person: p, Self: p.ID == me}
		if inv, ok := invites[p.ID]; ok {
			v.Invite = viewInvite(inv, now)
		}
		doc.People = append(doc.People, v)
	}
	writeJSON(w, http.StatusOK, doc)
}

// createPerson handles POST /people {username, displayName, role, maxRating}:
// the person, enabled and without a password, and their invite link.
func (a *API) createPerson(w http.ResponseWriter, r *http.Request) {
	if !a.peopleReady(w) {
		return
	}
	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Role        string `json:"role"`
		MaxRating   *int   `json:"maxRating"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Role == "" {
		body.Role = people.RoleUser
	}
	p, err := a.people.Create(r.Context(), people.NewPerson{
		Username: body.Username, DisplayName: body.DisplayName, Role: body.Role, MaxRating: body.MaxRating,
	})
	if err != nil {
		peopleFailure(w, err)
		return
	}
	log.Printf("people: %s added %s (%s%s)", requester(r), p.Username, p.Role, ratingNote(p.MaxRating))
	out := map[string]any{"person": personView{Person: p}, "invite": nil}
	link, err := a.issueInvite(r, p.ID)
	if err != nil {
		log.Printf("people: no invite for %s: %v", p.Username, err)
		out["note"] = "The person is added, but no invite could be made: send one from the list."
	} else {
		out["invite"] = link
		out["person"] = personView{Person: p, Invite: &inviteView{Status: model.InvitePending, CreatedAt: a.clock(), ExpiresAt: link.ExpiresAt}}
	}
	writeJSON(w, http.StatusCreated, out)
}

func ratingNote(n *int) string {
	if n == nil {
		return ""
	}
	return ", rating cap " + strconv.Itoa(*n)
}

// patchPerson handles PATCH /people/{id} {displayName, role, maxRating,
// enabled}: what is sent changes, maxRating null removes the cap.
func (a *API) patchPerson(w http.ResponseWriter, r *http.Request) {
	if !a.peopleReady(w) {
		return
	}
	var body struct {
		DisplayName *string         `json:"displayName"`
		Role        *string         `json:"role"`
		MaxRating   json.RawMessage `json:"maxRating"`
		Enabled     *bool           `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	ch := people.Change{DisplayName: body.DisplayName, Role: body.Role, Enabled: body.Enabled}
	switch raw := strings.TrimSpace(string(body.MaxRating)); {
	case raw == "":
	case raw == "null":
		ch.ClearRating = true
	default:
		var n int
		if err := json.Unmarshal(body.MaxRating, &n); err != nil {
			badRequest(w, "maxRating is an age from 0 to 21, or null for no cap")
			return
		}
		ch.MaxRating = &n
	}
	id := chi.URLParam(r, "id")
	p, err := a.people.Update(r.Context(), caller(r), id, ch)
	if err != nil {
		peopleFailure(w, err)
		return
	}
	if ch.Enabled != nil && !*ch.Enabled {
		// Switched off, the person's open invite leads nowhere.
		if err := a.invites.RevokeInvites(r.Context(), id); err != nil {
			log.Printf("people: the invites of %s stay open: %v", p.Username, err)
		}
	}
	log.Printf("people: %s changed %s", requester(r), p.Username)
	v := personView{Person: p, Self: p.ID == caller(r)}
	if invites, err := a.invites.LatestInvites(r.Context()); err == nil {
		if inv, ok := invites[p.ID]; ok {
			v.Invite = viewInvite(inv, a.clock())
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// deletePerson handles DELETE /people/{id}: chino's data of the person first
// (with the admin's bearer), then their account, then their invites and the
// notices addons left them. A refusal of chino-api's deletes nothing.
func (a *API) deletePerson(w http.ResponseWriter, r *http.Request) {
	if !a.peopleReady(w) {
		return
	}
	id := chi.URLParam(r, "id")
	p, err := a.people.CanDelete(r.Context(), caller(r), id)
	if err != nil {
		peopleFailure(w, err)
		return
	}
	data := "kept"
	if a.chino != nil {
		if err := a.chino.DeleteData(r.Context(), r.Header.Get("Authorization"), id); err != nil {
			log.Printf("people: chino-api did not delete the data of %s: %v", p.Username, err)
			http.Error(w, fmt.Sprintf("chino-api did not delete %s's watch history and lists, so nothing was deleted. Try again.", p.DisplayName),
				http.StatusBadGateway)
			return
		}
		data = "deleted"
	}
	if err := a.people.Delete(r.Context(), id); err != nil {
		peopleFailure(w, err)
		return
	}
	if err := a.invites.DeleteInvites(r.Context(), id); err != nil {
		log.Printf("people: the invites of %s stay: %v", p.Username, err)
	}
	a.dropNotices(r.Context(), id, p.Username)
	log.Printf("people: %s deleted %s (chino's data: %s)", requester(r), p.Username, data)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "data": data})
}

// invitePerson handles POST /people/{id}/invite: a new invite link; every
// older one of the person's stops working.
func (a *API) invitePerson(w http.ResponseWriter, r *http.Request) {
	if !a.peopleReady(w) {
		return
	}
	id := chi.URLParam(r, "id")
	p, err := a.people.CanInvite(r.Context(), caller(r), id)
	if err != nil {
		peopleFailure(w, err)
		return
	}
	link, err := a.issueInvite(r, id)
	if err != nil {
		serverError(w, err)
		return
	}
	log.Printf("people: %s invited %s again", requester(r), p.Username)
	writeJSON(w, http.StatusCreated, link)
}

// issueInvite makes an invite for the account id: 32 random bytes as the
// token, its SHA-256 stored.
func (a *API) issueInvite(r *http.Request, id string) (inviteLink, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return inviteLink{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256(raw)
	expires := a.clock().Add(a.cfg.InviteTTL).UTC().Truncate(time.Second)
	if _, err := a.invites.CreateInvite(r.Context(), id, callerName(r), sum[:], expires); err != nil {
		return inviteLink{}, err
	}
	path := "/portal/invite/" + token
	link := inviteLink{Path: path, Token: token, ExpiresAt: expires}
	if origin := requestOrigin(r); origin != "" {
		link.URL = origin + path
	}
	return link, nil
}

// inviteHash is the SHA-256 of a token as a link carries it: 32 bytes,
// base64url without padding. Anything else is no token.
func inviteHash(token string) ([]byte, bool) {
	if len(token) != 43 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// closedInvite answers every invite that does not open — a token of none, a
// used, revoked or expired one, one whose account is gone or switched off —
// the same way.
func closedInvite(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"valid":   false,
		"message": "This invite link does not work: it was used, it expired, or a newer one replaced it. Ask whoever invited you for a new link.",
	})
}

// clientIP is the address a request came from as the nearest proxy saw it:
// the last entry of X-Forwarded-For, which a client cannot write — what it
// sends comes before — else the connection's peer.
func clientIP(r *http.Request) string {
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		last := xff[len(xff)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if ip := strings.TrimSpace(last); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// inviteAllowed takes a request from the client address's bucket and, for a
// token of the right form, from the token's; false once it answered 429.
func (a *API) inviteAllowed(w http.ResponseWriter, r *http.Request, hash []byte) bool {
	ok, wait := a.inviteIP.Allow(clientIP(r))
	if ok && hash != nil {
		ok, wait = a.inviteToken.Allow(string(hash))
	}
	if ok {
		return true
	}
	secs := int(wait.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, "Too many tries: wait a moment, then try again.", http.StatusTooManyRequests)
	return false
}

// unavailableInvite answers an invite endpoint that cannot be served now.
func unavailableInvite(w http.ResponseWriter, err error) {
	log.Printf("people: an invite could not be served: %v", err)
	http.Error(w, "Accounts cannot be set up right now. Try again in a few minutes.", http.StatusServiceUnavailable)
}

// invitesServed: invites are made here. With an external provider there are
// none, and a link is one that does not work; without the people client's
// secret none can be served now.
func (a *API) invitesServed(w http.ResponseWriter) bool {
	switch a.peopleMode() {
	case peopleExternal:
		closedInvite(w)
		return false
	case peopleUnavailable:
		unavailableInvite(w, errors.New("the people client has no secret"))
		return false
	}
	return true
}

// getInvite handles GET /invites/{token}: whose invite it is and the
// password rules, while it is open; else closedInvite.
func (a *API) getInvite(w http.ResponseWriter, r *http.Request) {
	hash, ok := inviteHash(chi.URLParam(r, "token"))
	if !a.inviteAllowed(w, r, hash) {
		return
	}
	if !ok {
		closedInvite(w)
		return
	}
	if !a.invitesServed(w) {
		return
	}
	inv, err := a.invites.OpenInvite(r.Context(), hash, a.clock())
	if errors.Is(err, store.ErrInviteClosed) {
		closedInvite(w)
		return
	}
	if err != nil {
		unavailableInvite(w, err)
		return
	}
	p, err := a.people.Invitee(r.Context(), inv.UserID)
	if errors.Is(err, people.ErrNotFound) {
		closedInvite(w)
		return
	}
	if err != nil {
		unavailableInvite(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true, "username": p.Username, "displayName": p.DisplayName, "expiresAt": inv.ExpiresAt,
		"passwordPolicy": people.ParsePolicy(a.cfg.PasswordPolicy),
	})
}

// acceptInvite handles POST /invites/{token} {password}: the password is set
// — never temporary — what Keycloak would ask at the first sign-in is
// cleared, and the invite is used, all or nothing. The answer says where to
// sign in.
func (a *API) acceptInvite(w http.ResponseWriter, r *http.Request) {
	hash, ok := inviteHash(chi.URLParam(r, "token"))
	if !a.inviteAllowed(w, r, hash) {
		return
	}
	if !ok {
		closedInvite(w)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxInviteBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		badRequest(w, `send {"password": "…"}`)
		return
	}
	if !a.invitesServed(w) {
		return
	}
	policy := people.ParsePolicy(a.cfg.PasswordPolicy)
	var who people.Person
	err := a.invites.UseInvite(r.Context(), hash, a.clock(), func(inv model.Invite) error {
		p, err := a.people.Invitee(r.Context(), inv.UserID)
		if errors.Is(err, people.ErrNotFound) {
			return store.ErrInviteClosed
		}
		if err != nil {
			return err
		}
		if err := policy.Check(body.Password, p); err != nil {
			return err
		}
		if err := a.people.Accept(r.Context(), inv.UserID, body.Password); err != nil {
			return people.PasswordError(err)
		}
		who = p
		return nil
	})
	var refused *people.PasswordRefused
	switch {
	case errors.Is(err, store.ErrInviteClosed):
		closedInvite(w)
	case errors.As(err, &refused):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "password", "message": refused.Message})
	case err != nil:
		unavailableInvite(w, err)
	default:
		log.Printf("people: %s chose a password with their invite", who.Username)
		writeJSON(w, http.StatusOK, map[string]any{"username": who.Username, "signIn": "/portal/"})
	}
}

// deleteMe handles DELETE /me, which chino-api calls for the signed-in
// person once it deleted their data: the account deletion token
// (chino.DeletionHeader) and the person's own bearer. The account deleted is
// the token's subject, never one the request names; the last admin and a
// Keycloak administrator are refused. Their invites and notices go with it.
func (a *API) deleteMe(w http.ResponseWriter, r *http.Request) {
	want := a.cfg.AccountDeletionToken
	if want == "" {
		http.Error(w, "account deletion is not set up here", http.StatusServiceUnavailable)
		return
	}
	got := r.Header.Get(chino.DeletionHeader)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		log.Printf("people: an account deletion without the deletion token was refused (%s)", requester(r))
		http.Error(w, "forbidden: an account is deleted through chino-api's DELETE /api/v1/me, which deletes its data first",
			http.StatusForbidden)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if p == nil || p.Subject == "" || auth.IsServiceAccount(p) {
		http.Error(w, "forbidden: a person deletes their own account", http.StatusForbidden)
		return
	}
	if !a.peopleReady(w) {
		return
	}
	who, err := a.people.CanDeleteOwn(r.Context(), p.Subject)
	if errors.Is(err, people.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"account": "gone"})
		return
	}
	if err != nil {
		peopleFailure(w, err)
		return
	}
	if err := a.people.Delete(r.Context(), p.Subject); err != nil {
		peopleFailure(w, err)
		return
	}
	if err := a.invites.DeleteInvites(r.Context(), p.Subject); err != nil {
		log.Printf("people: the invites of %s stay: %v", who.Username, err)
	}
	a.dropNotices(r.Context(), p.Subject, who.Username)
	log.Printf("people: %s deleted their own account", who.Username)
	w.WriteHeader(http.StatusNoContent)
}

// peopleFailure answers a change that did not happen, in words for the admin
// — never Keycloak's.
func peopleFailure(w http.ResponseWriter, err error) {
	var refused *people.Refused
	var inv *people.Invalid
	var kcInvalid *keycloak.Invalid
	var kcFailed *keycloak.Failed
	switch {
	case errors.Is(err, people.ErrNotFound) || errors.Is(err, keycloak.ErrNotFound):
		http.Error(w, "No such person.", http.StatusNotFound)
	case errors.Is(err, people.ErrUsernameUsed):
		http.Error(w, "That username is taken: choose another.", http.StatusConflict)
	case errors.As(err, &refused):
		http.Error(w, refused.Message, http.StatusConflict)
	case errors.As(err, &inv):
		badRequest(w, inv.Message)
	case errors.Is(err, people.ErrRatingNotKept):
		log.Printf("people: %v", err)
		http.Error(w, "Keycloak did not keep the rating cap: the realm's user profile does not declare max_rating yet. The realm Job declares it (see the RealmConfigured condition); then try again.",
			http.StatusBadGateway)
	case errors.As(err, &kcInvalid):
		log.Printf("people: keycloak declined %s (%s)", kcInvalid.Field, kcInvalid.Code)
		badRequest(w, invalidWords(kcInvalid))
	case errors.Is(err, keycloak.ErrRefused):
		log.Printf("people: %v", err)
		http.Error(w, "Keycloak refused the People page's client: its secret, or the roles the realm Job gives it. See the RealmConfigured condition of the platform.",
			http.StatusServiceUnavailable)
	case errors.Is(err, keycloak.ErrUnreachable):
		log.Printf("people: %v", err)
		http.Error(w, "Keycloak did not answer. Try again in a moment.", http.StatusBadGateway)
	case errors.As(err, &kcFailed):
		log.Printf("people: %v", err)
		http.Error(w, fmt.Sprintf("Keycloak refused the change (HTTP %d).", kcFailed.Status), http.StatusBadGateway)
	default:
		log.Printf("people: %v", err)
		http.Error(w, "The change failed; the log of portal-api says why.", http.StatusInternalServerError)
	}
}

// invalidWords says which value Keycloak declined.
func invalidWords(e *keycloak.Invalid) string {
	switch e.Field {
	case people.RatingAttribute:
		return fmt.Sprintf("A rating cap is an age from 0 to %d.", people.MaxRating)
	case "username":
		return "Keycloak does not take that username: use letters a to z, digits, and . _ -"
	case "firstName", "lastName":
		return "Keycloak does not take that name."
	}
	return "Keycloak does not take one of the values."
}
