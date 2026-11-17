package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak/kctest"
	zmail "github.com/zaentrum/zaentrum-portal/server/internal/mail"
	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// newInviteEnv is mailEnv with mail ready: the realm's people in the fake
// Keycloak, and an SMTP server on 127.0.0.1 that takes the sign-in of the
// settings, named by the platform's resource, which the operator says is
// Configured, and by the settings portal-api runs with.
func newInviteEnv(t *testing.T, o mailtest.Options) *mailEnv {
	t.Helper()
	o.Auth, o.User, o.Password = []string{"PLAIN"}, "zaentrum@example.org", "password-for-tests"
	e := newMailEnv(t, true, o)
	e.ready(zmail.None)
	return e
}

// fieldsOf is an answer's fields as they came.
func fieldsOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("not JSON: %v %s", err, rec.Body)
	}
	return raw
}

// keys are an answer's field names, in order.
func keys(raw map[string]json.RawMessage) []string {
	out := make([]string, 0, len(raw))
	for k := range raw {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// mailOutcome is what an answer says of the mail: mail, "" when it is not
// there, and mailError, a refusal with no code when it is not there.
func mailOutcome(t *testing.T, raw map[string]json.RawMessage) (string, refusal) {
	t.Helper()
	var outcome string
	var why refusal
	if v, ok := raw["mail"]; ok {
		if err := json.Unmarshal(v, &outcome); err != nil || outcome == "" {
			t.Errorf("mail = %s", v)
		}
	}
	if v, ok := raw["mailError"]; ok {
		if err := json.Unmarshal(v, &why); err != nil || why.Error == "" || why.Message == "" {
			t.Errorf("mailError = %s", v)
		}
	}
	return outcome, why
}

// readMail is a mail the server took: its subject, its headers, and its text
// as it was written.
func readMail(t *testing.T, m mailtest.Mail) (string, mail.Header, string) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(m.Data))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	text, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	return subject, msg.Header, string(text)
}

// addPerson is POST /people as the admin, Anna; it fails the test unless the
// person was added.
func addPerson(e *mailEnv, body map[string]any) (created, map[string]json.RawMessage) {
	e.t.Helper()
	rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", body)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("POST /people %v = %d %s", body, rec.Code, rec.Body)
	}
	return decodeAs[created](e.t, rec), fieldsOf(e.t, rec)
}

// invitesMade is how many invites were ever made.
func (e *mailEnv) invitesMade() int {
	inv := e.api.invites.(*fakeInvites)
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return len(inv.rows)
}

// opens: the invite link of token works.
func (e *mailEnv) opens(token string) bool {
	return e.do(nil, http.MethodGet, "/api/portal/invites/"+token, nil).Code == http.StatusOK
}

// The time the tests' invites are made at, and the words their mails give
// for when the link stops working: seven days later, in UTC.
var (
	inviteMadeAt = time.Date(2026, 11, 15, 14, 2, 5, 0, time.UTC)
	inviteUntil  = "22 November 2026, 14:02 UTC"
)

// R.C4: POST /people takes an address, optional; PATCH /people/{id} changes
// it, and an empty one removes it. The answers are as before: the person, now
// with their address as the identity provider keeps it.
func TestRC4AnAddressOnCreateAndOnChange(t *testing.T) {
	e := newPeopleEnv(t)
	var mia string
	t.Run("R.C4 added with an address", func(t *testing.T) {
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "mia", "displayName": "Mia", "email": " Mia@Example.org ", "role": "user"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /people = %d %s", rec.Code, rec.Body)
		}
		c := decodeAs[created](t, rec)
		mia = c.Person.ID
		if c.Person.Email != "mia@example.org" || e.kc.User(mia)["email"] != "mia@example.org" || c.Invite.Token == "" {
			t.Errorf("added: %+v, in Keycloak %v", c, e.kc.User(mia))
		}
		if got := keys(fieldsOf(t, rec)); !slices.Equal(got, []string{"invite", "person"}) {
			t.Errorf("the answer's fields = %v, as before", got)
		}
		doc := decodeAs[peopleDoc](t, e.do(adminPortal, http.MethodGet, "/api/portal/people", nil))
		if i := slices.IndexFunc(doc.People, func(p personView) bool { return p.ID == mia }); i < 0 || doc.People[i].Email != "mia@example.org" {
			t.Errorf("listed: %+v", doc.People)
		}
	})
	t.Run("R.C4 added without one, as before", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"username": "leo", "displayName": "Leo", "role": "user"},
			{"username": "noah", "displayName": "Noah", "role": "user", "email": ""},
			{"username": "lena", "displayName": "Lena", "role": "user", "email": "   "},
		} {
			c := e.add(body)
			if c.Person.Email != "" || e.kc.User(c.Person.ID)["email"] != nil {
				t.Errorf("%v: %+v", body, c.Person)
			}
		}
	})
	t.Run("R.C4 changed", func(t *testing.T) {
		p := decodeAs[personView](t, e.patch(adminPortal, mia, map[string]any{"email": "mia.k@example.org"}))
		if p.Email != "mia.k@example.org" || p.DisplayName != "Mia" || p.Role != "user" || e.kc.User(mia)["email"] != "mia.k@example.org" {
			t.Errorf("changed: %+v, in Keycloak %v", p, e.kc.User(mia))
		}
		// Another field changes alone, and the address stays.
		p = decodeAs[personView](t, e.patch(adminPortal, mia, map[string]any{"displayName": "Mia K", "maxRating": 12}))
		if p.Email != "mia.k@example.org" || p.DisplayName != "Mia K" || p.MaxRating == nil || *p.MaxRating != 12 {
			t.Errorf("renamed and capped: %+v", p)
		}
		// The same address in other letters changes nothing.
		puts := len(e.kc.CallsMatching("PUT /users/" + mia))
		p = decodeAs[personView](t, e.patch(adminPortal, mia, map[string]any{"email": "MIA.K@example.org"}))
		if n := len(e.kc.CallsMatching("PUT /users/" + mia)); p.Email != "mia.k@example.org" || n != puts {
			t.Errorf("the same in other letters: %+v, %d writes", p, n-puts)
		}
	})
	t.Run("R.C4 an empty one removes it", func(t *testing.T) {
		for _, empty := range []string{"", "  "} {
			if rec := e.patch(adminPortal, mia, map[string]any{"email": "mia@example.org"}); rec.Code != http.StatusOK {
				t.Fatalf("set again: %d %s", rec.Code, rec.Body)
			}
			rec := e.patch(adminPortal, mia, map[string]any{"email": empty})
			if rec.Code != http.StatusOK || decodeAs[personView](t, rec).Email != "" || e.kc.User(mia)["email"] != nil {
				t.Errorf("email %q: %d %s, in Keycloak %v", empty, rec.Code, rec.Body, e.kc.User(mia))
			}
			if strings.Contains(rec.Body.String(), `"email"`) {
				t.Errorf("the person without an address: %s", rec.Body)
			}
		}
	})
}

// R.C4: portal-api refuses an address another account has — also in a realm
// that would let two accounts have it — and one that is not an address, with
// the words of the section Words, and changes nothing. An account that administers the identity
// provider is changed in the provider's console, as before, its address
// included.
func TestRC4AddressesRefused(t *testing.T) {
	e := newPeopleEnv(t)
	mia := e.add(map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org", "role": "user"}).Person.ID
	ctx := context.Background()
	verify, _ := e.api.people.Keycloak().User(ctx, e.kc.UserByName("zaentrum-verify"))
	verify.SetEmail("verify@example.org")
	if err := e.api.people.Keycloak().Update(ctx, verify); err != nil {
		t.Fatal(err)
	}
	unchanged := func(t *testing.T) {
		t.Helper()
		if e.kc.UserByName("kim") != "" || e.kc.User(mia)["email"] != "mia@example.org" {
			t.Errorf("a refused address was kept: kim %q, mia %v", e.kc.UserByName("kim"), e.kc.User(mia)["email"])
		}
	}

	t.Run("R.C4 an address another account has", func(t *testing.T) {
		// The realm lets two accounts have one address: portal-api refuses
		// it by itself.
		e.kc.DuplicateEmails = true
		defer func() { e.kc.DuplicateEmails = false }()
		// The first admin's, whose account is Keycloak's to change, the
		// platform's own account's, and a person's in other letters.
		for _, taken := range []string{"owner@example.org", "verify@example.org", "MIA@example.org"} {
			rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "kim", "displayName": "Kim", "email": taken, "sendInvite": true})
			refused(t, rec, http.StatusConflict, "address_taken", "Another account already has this address.")
		}
		rec := e.patch(adminPortal, mia, map[string]any{"email": "owner@example.org"})
		refused(t, rec, http.StatusConflict, "address_taken", "Another account already has this address.")
		unchanged(t)
		if n := len(e.invites.rows); n != 1 {
			t.Errorf("%d invites made, want mia's alone", n)
		}
	})
	t.Run("R.C4 one that is not an address", func(t *testing.T) {
		for _, bad := range []string{"mia", "mia@", "@example.org", "Mia <mia@example.org>", "<mia@example.org>",
			"mia@example.org, leo@example.org", "mia@example.org\nBcc: leo@example.org", strings.Repeat("m", 250) + "@example.org"} {
			rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "kim", "displayName": "Kim", "email": bad})
			refused(t, rec, http.StatusBadRequest, "address_invalid", "This is not an e-mail address.")
			rec = e.patch(adminPortal, mia, map[string]any{"email": bad})
			refused(t, rec, http.StatusBadRequest, "address_invalid", "This is not an e-mail address.")
		}
		// One the identity provider's validator does not take, though it reads
		// as an address: a local part of more than 64 characters.
		long := strings.Repeat("m", 65) + "@example.org"
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "kim", "displayName": "Kim", "email": long})
		refused(t, rec, http.StatusBadRequest, "address_invalid", "This is not an e-mail address.")
		refused(t, e.patch(adminPortal, mia, map[string]any{"email": long}), http.StatusBadRequest, "address_invalid", "This is not an e-mail address.")
		unchanged(t)
	})
	t.Run("R.C4 the identity provider's administrator is changed in its console, the address too", func(t *testing.T) {
		for _, address := range []string{"new@example.org", ""} {
			rec := e.patch(adminPortal, e.owner, map[string]any{"email": address})
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "administers Keycloak itself") {
				t.Errorf("the owner's address to %q: %d %s", address, rec.Code, rec.Body)
			}
		}
		if e.kc.User(e.owner)["email"] != "owner@example.org" {
			t.Errorf("the owner's address: %v", e.kc.User(e.owner)["email"])
		}
	})
}

// R.C4: with sendInvite, POST /people mails the person their invite link
// while the admin waits, with the words of the section Words, and answers the
// link as before and mail: sent, not-set-up, no-address or failed, with
// mailError, the reason's code and sentence, when it failed. The log says that an invite was sent
// or refused, never the address and never the text.
func TestRC4SendInvite(t *testing.T) {
	mia := map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org", "role": "user", "sendInvite": true}
	t.Run("R.C4 mail ready: sent, with the words of the section Words", func(t *testing.T) {
		e := newInviteEnv(t, mailtest.Options{})
		e.api.now = func() time.Time { return inviteMadeAt }
		logged := captureLog(t)
		// The admin reaches the portal through a proxy on another name than
		// the platform's public address.
		body, _ := json.Marshal(mia)
		req := httptest.NewRequest(http.MethodPost, "/api/portal/people", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+e.iss.Token(t, adminPortal))
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "media.example.org")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /people = %d %s", rec.Code, rec.Body)
		}
		c, raw := decodeAs[created](t, rec), fieldsOf(t, rec)
		if outcome, why := mailOutcome(t, raw); outcome != "sent" || why.Error != "" {
			t.Errorf("mail %q, mailError %+v", outcome, why)
		}
		if got := keys(raw); !slices.Equal(got, []string{"invite", "mail", "person"}) {
			t.Errorf("the answer's fields = %v", got)
		}
		// The link as before: shown once, on the address the admin reached the
		// portal on, and it works. The mail's starts with PORTAL_PUBLIC_URL.
		token := c.Invite.Token
		if len(token) != 43 || c.Invite.URL != "https://media.example.org/portal/invite/"+token || !e.opens(token) {
			t.Errorf("the link: %+v", c.Invite)
		}
		mails := e.smtp.Mails()
		if len(mails) != 1 || !slices.Equal(mails[0].To, []string{"mia@example.org"}) || mails[0].From != "zaentrum@example.org" {
			t.Fatalf("mails = %+v", mails)
		}
		subject, header, text := readMail(t, mails[0])
		want := "Hello Mia,\n\n" +
			"Anna made an account for you on zaentrum.example.org. Your username is mia.\n\n" +
			"Open this link to choose your password:\n\n" +
			"https://zaentrum.example.org/portal/invite/" + token + "\n\n" +
			"The link works once, until " + inviteUntil + ". After that, ask Anna for a new one.\n\n" +
			"If you did not expect this mail, ignore it.\n"
		if subject != "Your account on zaentrum.example.org" || text != want {
			t.Errorf("subject %q, text:\n%s\nwant:\n%s", subject, text, want)
		}
		if header.Get("To") != "<mia@example.org>" || !strings.Contains(header.Get("From"), "zaentrum@example.org") ||
			header.Get("Auto-Submitted") != "auto-generated" {
			t.Errorf("headers %v", header)
		}
		if !strings.Contains(logged.String(), "mail: an invite was sent") {
			t.Errorf("log = %q", logged)
		}
		for _, secret := range []string{"mia@example.org", "Hello", "made an account", token} {
			if strings.Contains(logged.String(), secret) {
				t.Errorf("the log holds %q: %q", secret, logged)
			}
		}
	})
	t.Run("R.C4 left out or false: nothing is sent, and the answer is as before", func(t *testing.T) {
		e := newInviteEnv(t, mailtest.Options{})
		for i, send := range []any{nil, false} {
			body := map[string]any{"username": "mia" + string(rune('a'+i)), "displayName": "Mia", "email": "mia" + string(rune('a'+i)) + "@example.org"}
			if send != nil {
				body["sendInvite"] = send
			}
			c, raw := addPerson(e, body)
			if got := keys(raw); !slices.Equal(got, []string{"invite", "person"}) || c.Invite.Token == "" {
				t.Errorf("sendInvite %v: the answer's fields = %v", send, got)
			}
		}
		if n := len(e.smtp.Sessions()); n != 0 {
			t.Errorf("%d connections to the mail server", n)
		}
	})
	t.Run("R.C4 mail not set up: not-set-up, and nothing is sent", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.putPlatform(nil, operator.MailNotSetUp)
		e.api.cfg.Mail = e.settings(zmail.None, "", "") // the resource says no more, whatever portal-api still runs with
		c, raw := addPerson(e, mia)
		if outcome, why := mailOutcome(t, raw); outcome != "not-set-up" || why.Error != "" {
			t.Errorf("mail %q, mailError %+v", outcome, why)
		}
		if c.Invite.Token == "" || !e.opens(c.Invite.Token) || len(e.smtp.Sessions()) != 0 {
			t.Errorf("the link %+v; %d connections", c.Invite, len(e.smtp.Sessions()))
		}
	})
	t.Run("R.C4 no address: no-address", func(t *testing.T) {
		e := newInviteEnv(t, mailtest.Options{})
		for _, body := range []map[string]any{
			{"username": "leo", "displayName": "Leo", "sendInvite": true},
			{"username": "kim", "displayName": "Kim", "email": "", "sendInvite": true},
		} {
			c, raw := addPerson(e, body)
			if outcome, why := mailOutcome(t, raw); outcome != "no-address" || why.Error != "" || c.Invite.Token == "" {
				t.Errorf("%v: mail %q, mailError %+v, link %+v", body, outcome, why, c.Invite)
			}
		}
		if n := len(e.smtp.Sessions()); n != 0 {
			t.Errorf("%d connections to the mail server", n)
		}
	})
	t.Run("R.C4 a failed send: failed, with the reason's code and sentence; the link stays", func(t *testing.T) {
		for _, c := range []struct {
			name, verb, reply, kind, code, why string
			values                             map[string]any
		}{
			{"the address refused", "RCPT", "550 5.1.1 <mia@example.org>: no such user", "recipient", "mail_recipient_refused",
				"The mail server refused the address mia@example.org.", map[string]any{"address": "mia@example.org"}},
			{"in the server's own words", "MAIL", "451 4.3.0 try again later", "answered", "mail_server_answered",
				"The mail server answered: 4.3.0 try again later", map[string]any{"words": "4.3.0 try again later"}},
		} {
			t.Run("R.C4 "+c.name, func(t *testing.T) {
				e := newInviteEnv(t, mailtest.Options{Replies: map[string]string{c.verb: c.reply}})
				logged := captureLog(t)
				added, raw := addPerson(e, mia)
				if outcome, why := mailOutcome(t, raw); outcome != "failed" || why.Error != c.code || why.Message != c.why ||
					!reflect.DeepEqual(why.Values, c.values) {
					t.Errorf("mail %q, mailError %+v, want %s %q %v", outcome, why, c.code, c.why, c.values)
				}
				if added.Invite.Token == "" || !e.opens(added.Invite.Token) || len(e.smtp.Mails()) != 0 {
					t.Errorf("the link %+v; mails %+v", added.Invite, e.smtp.Mails())
				}
				if !strings.Contains(logged.String(), "mail: an invite was refused ("+c.kind+")") || strings.Contains(logged.String(), "mia@example.org") {
					t.Errorf("log = %q", logged)
				}
			})
		}
	})
	t.Run("R.C4 set up, not ready yet: failed, with where mail stands", func(t *testing.T) {
		for name, c := range map[string]struct {
			unready   func(e *mailEnv)
			code, why string
		}{
			"the Secret is missing": {func(e *mailEnv) { e.setCondition(operator.MailSecretMissing, 1) },
				"mail_password_missing", "The mail server's password is missing."},
			"portal-api runs with older settings": {func(e *mailEnv) { e.api.cfg.Mail.Host = "smtp.example.org" },
				"mail_not_ready", "The mail settings are still being applied."},
		} {
			e := newInviteEnv(t, mailtest.Options{})
			c.unready(e)
			added, raw := addPerson(e, mia)
			if outcome, why := mailOutcome(t, raw); outcome != "failed" || why.Error != c.code || why.Message != c.why || added.Invite.Token == "" {
				t.Errorf("%s: mail %q, mailError %+v, link %+v", name, outcome, why, added.Invite)
			}
			if n := len(e.smtp.Sessions()); n != 0 {
				t.Errorf("%s: %d connections to the mail server", name, n)
			}
		}
	})
}

// R.C4: with send, POST /people/{id}/invite mails the new link while the
// admin waits, with the words of the section Words, and answers the link as
// before, with mail and mailError as POST /people does. The body stays
// optional.
func TestRC4SendNewLink(t *testing.T) {
	newLink := func(e *mailEnv, id string, body any) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
		e.t.Helper()
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/people/"+id+"/invite", body)
		if rec.Code != http.StatusCreated {
			return rec, nil
		}
		return rec, fieldsOf(e.t, rec)
	}
	t.Run("R.C4 mail ready: the new link is sent, with the words of the section Words", func(t *testing.T) {
		e := newInviteEnv(t, mailtest.Options{})
		e.api.now = func() time.Time { return inviteMadeAt }
		first, _ := addPerson(e, map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org"})
		logged := captureLog(t)
		rec, raw := newLink(e, first.Person.ID, map[string]any{"send": true})
		if raw == nil {
			t.Fatalf("POST invite = %d %s", rec.Code, rec.Body)
		}
		if outcome, why := mailOutcome(t, raw); outcome != "sent" || why.Error != "" {
			t.Errorf("mail %q, mailError %+v", outcome, why)
		}
		if got := keys(raw); !slices.Equal(got, []string{"expiresAt", "mail", "path", "token", "url"}) {
			t.Errorf("the answer's fields = %v", got)
		}
		link := decodeAs[inviteLink](t, rec)
		if link.Token == first.Invite.Token || !e.opens(link.Token) || e.opens(first.Invite.Token) {
			t.Errorf("the new link works, the older one does not: %+v", link)
		}
		mails := e.smtp.Mails()
		if len(mails) != 1 || !slices.Equal(mails[0].To, []string{"mia@example.org"}) {
			t.Fatalf("mails = %+v", mails)
		}
		subject, _, text := readMail(t, mails[0])
		want := "Hello Mia,\n\n" +
			"Anna made a new link for your account mia on zaentrum.example.org.\n\n" +
			"Open it to choose a new password:\n\n" +
			"https://zaentrum.example.org/portal/invite/" + link.Token + "\n\n" +
			"The link works once, until " + inviteUntil + ". Your password stays as it is until you use the link.\n\n" +
			"If you did not expect this mail, ignore it.\n"
		if subject != "A new link for your account on zaentrum.example.org" || text != want {
			t.Errorf("subject %q, text:\n%s\nwant:\n%s", subject, text, want)
		}
		if !strings.Contains(logged.String(), "mail: a new invite link was sent") || strings.Contains(logged.String(), "mia@example.org") {
			t.Errorf("log = %q", logged)
		}
	})
	t.Run("R.C4 without send: nothing is sent, and the answer is as before", func(t *testing.T) {
		e := newInviteEnv(t, mailtest.Options{})
		first, _ := addPerson(e, map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org"})
		for _, body := range []any{nil, map[string]any{}, map[string]any{"send": false}} {
			rec, raw := newLink(e, first.Person.ID, body)
			if raw == nil {
				t.Fatalf("body %v: %d %s", body, rec.Code, rec.Body)
			}
			if got := keys(raw); !slices.Equal(got, []string{"expiresAt", "path", "token", "url"}) {
				t.Errorf("body %v: the answer's fields = %v", body, got)
			}
		}
		if n := len(e.smtp.Sessions()); n != 0 {
			t.Errorf("%d connections to the mail server", n)
		}
	})
	t.Run("R.C4 not set up, no address, failed", func(t *testing.T) {
		for name, c := range map[string]struct {
			o       mailtest.Options
			email   string
			prepare func(e *mailEnv)
			outcome string
			code    string
			why     string
		}{
			"mail is not set up": {email: "mia@example.org", prepare: func(e *mailEnv) { e.putPlatform(nil, operator.MailNotSetUp) }, outcome: "not-set-up"},
			"no address":         {outcome: "no-address"},
			// The import's placeholder, which the people page never writes.
			"the placeholder": {email: "mia@zaentrum.local", outcome: "no-address"},
			"the server refuses": {o: mailtest.Options{Replies: map[string]string{"RCPT": "550 5.1.1 no such user"}}, email: "mia@example.org",
				outcome: "failed", code: "mail_recipient_refused", why: "The mail server refused the address mia@example.org."},
		} {
			e := newInviteEnv(t, c.o)
			id := e.kc.Add(kctest.Seed{Username: "mia", FirstName: "Mia", Email: c.email, Roles: []string{kctest.UserRole}})
			if c.prepare != nil {
				c.prepare(e)
			}
			rec, raw := newLink(e, id, map[string]any{"send": true})
			if raw == nil {
				t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
			}
			if outcome, why := mailOutcome(t, raw); outcome != c.outcome || why.Error != c.code || why.Message != c.why {
				t.Errorf("%s: mail %q, mailError %+v", name, outcome, why)
			}
			if link := decodeAs[inviteLink](t, rec); !e.opens(link.Token) || len(e.smtp.Mails()) != 0 {
				t.Errorf("%s: the link %+v; mails %+v", name, link, e.smtp.Mails())
			}
			if c.email == "mia@zaentrum.local" && len(e.smtp.Sessions()) != 0 {
				t.Errorf("%s: a connection to the mail server", name)
			}
		}
	})
	t.Run("R.C4 a body it does not take makes no link", func(t *testing.T) {
		e := newInviteEnv(t, mailtest.Options{})
		first, _ := addPerson(e, map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org"})
		for _, body := range []any{map[string]any{"send": "yes"}, map[string]any{"sent": true}, "not json"} {
			if rec := e.do(adminPortal, http.MethodPost, "/api/portal/people/"+first.Person.ID+"/invite", body); rec.Code != http.StatusBadRequest {
				t.Errorf("body %v: %d %s", body, rec.Code, rec.Body)
			}
		}
		if e.invitesMade() != 1 || !e.opens(first.Invite.Token) || len(e.smtp.Sessions()) != 0 {
			t.Errorf("%d invites made; the first works: %v", e.invitesMade(), e.opens(first.Invite.Token))
		}
	})
}

// R.C4: where the operator does not run the platform, the settings portal-api
// runs with send the invite, as they send the test mail. Where the platform's
// resource cannot be read, no mail is sent: mail is failed, mailError the
// refusal the test mail answers then, the cluster's words in detail, and the
// link is in the answer.
func TestRC4InviteMailWithoutTheResource(t *testing.T) {
	mia := map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org", "sendInvite": true}
	t.Run("R.C4 not run by the operator: sent with the chart's values", func(t *testing.T) {
		e := newMailEnv(t, true, mailtest.Options{})
		e.api.cfg.Mail = e.settings(zmail.None, "", "")
		if _, raw := addPerson(e, mia); len(e.smtp.Mails()) != 1 {
			t.Errorf("answer %v; mails %+v", raw, e.smtp.Mails())
		} else if outcome, _ := mailOutcome(t, raw); outcome != "sent" {
			t.Errorf("mail %q", outcome)
		}
	})
	for _, c := range []struct {
		name      string
		status    int
		code, why string
	}{
		{"R.C4 the resource cannot be read: failed, with the cluster's refusal", http.StatusInternalServerError,
			"cluster_unexpected_answer", "The cluster did not answer as expected."},
		{"R.C4 the resource may not be read: failed, with the cluster's refusal", http.StatusForbidden,
			"platform_read_forbidden", "The portal is not allowed to read the platform's settings. Update the operator, which gives the portal that right."},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newInviteEnv(t, mailtest.Options{})
			e.kube.Fail = func(call k8sfake.Call) (int, string, bool) {
				if call.Method == http.MethodGet && strings.Contains(call.Path, "/zaentrums") {
					return c.status, "the apiserver's own words", true
				}
				return 0, "", false
			}
			added, raw := addPerson(e, mia)
			outcome, why := mailOutcome(t, raw)
			if outcome != "failed" || why.Error != c.code || why.Message != c.why || !strings.Contains(why.Detail, "the apiserver's own words") {
				t.Errorf("POST /people: mail %q, mailError %+v", outcome, why)
			}
			if added.Invite.Token == "" || !e.opens(added.Invite.Token) {
				t.Errorf("the link: %+v", added.Invite)
			}
			rec := e.do(adminPortal, http.MethodPost, "/api/portal/people/"+added.Person.ID+"/invite", map[string]any{"send": true})
			if outcome, why := mailOutcome(t, fieldsOf(t, rec)); rec.Code != http.StatusCreated || outcome != "failed" || why.Error != c.code {
				t.Errorf("POST invite: %d, mail %q, mailError %+v", rec.Code, outcome, why)
			}
			if n := len(e.smtp.Sessions()); n != 0 {
				t.Errorf("%d connections to the mail server", n)
			}
		})
	}
}

// R.C4: the link in an invite mail comes from the public address alone,
// never from the request's link: a portal-api without a full public address
// — PORTAL_PUBLIC_URL unset, blank, or without a scheme — sends no invite.
// Where the operator runs the platform mail is not ready; where it does not,
// mail is not set up. The link the admin hands over is in the answer.
func TestRC4TheLinkComesFromThePublicAddressAlone(t *testing.T) {
	mia := map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org", "sendInvite": true}
	for _, public := range []string{"", "   ", "zaentrum.example.org"} {
		for _, byOperator := range []bool{true, false} {
			t.Run(fmt.Sprintf("R.C4 PORTAL_PUBLIC_URL %q, run by the operator %v", public, byOperator), func(t *testing.T) {
				e := newInviteEnv(t, mailtest.Options{})
				want, code := "failed", "mail_not_ready"
				if !byOperator {
					e.kube.Remove("zaentrums", "zaentrum")
					want, code = "not-set-up", ""
				}
				e.api.cfg.PublicURL = public
				added, raw := addPerson(e, mia)
				if outcome, why := mailOutcome(t, raw); outcome != want || why.Error != code {
					t.Errorf("POST /people: mail %q, mailError %+v; want %q %q", outcome, why, want, code)
				}
				if added.Invite.URL == "" || !e.opens(added.Invite.Token) {
					t.Errorf("the link the admin hands over: %+v", added.Invite)
				}
				rec := e.do(adminPortal, http.MethodPost, "/api/portal/people/"+added.Person.ID+"/invite", map[string]any{"send": true})
				if outcome, why := mailOutcome(t, fieldsOf(t, rec)); outcome != want || why.Error != code {
					t.Errorf("POST invite: mail %q, mailError %+v", outcome, why)
				}
				if n := len(e.smtp.Sessions()); n != 0 {
					t.Errorf("%d connections to the mail server", n)
				}
			})
		}
	}
}

// R.C4: the link a mail carries is on the public address alone: without a
// full one there is none, never the link the request came with.
func TestRC4TheMailsLinkIsOnThePublicAddress(t *testing.T) {
	e := newInviteEnv(t, mailtest.Options{})
	link := inviteLink{URL: "https://elsewhere.example.org/portal/invite/token", Path: "/portal/invite/token", Token: "token"}
	for public, want := range map[string]string{
		"": "", "   ": "", "zaentrum.example.org": "", "//zaentrum.example.org": "", "ftp://zaentrum.example.org": "",
		"https://zaentrum.example.org":    "https://zaentrum.example.org/portal/invite/token",
		" https://zaentrum.example.org/ ": "https://zaentrum.example.org/portal/invite/token",
	} {
		e.api.cfg.PublicURL = public
		if got, ok := e.api.mailLink(link); got != want || ok != (want != "") {
			t.Errorf("PORTAL_PUBLIC_URL %q: %q, %v; want %q", public, got, ok, want)
		}
	}
}

// R.C4: an address typed in the form that is a placeholder is refused as the
// test mail refuses it, and one with accented or non-Latin letters as the mail
// page does; the people routes answer an account whose address is at the
// placeholder domain without one.
func TestRC4APlaceholderAndLetters(t *testing.T) {
	e := newPeopleEnv(t)
	mia := e.add(map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org", "role": "user"}).Person.ID
	for _, c := range []struct {
		address, code, say string
	}{
		{"kim@zaentrum.local", "address_placeholder", "This address is a placeholder: nothing is sent to it. Type another."},
		{"Kim@Zaentrum.Local", "address_placeholder", "This address is a placeholder: nothing is sent to it. Type another."},
		{"kïm@example.org", "address_unsupported", "Addresses with accented or non-Latin letters are not supported."},
		{"ким@example.org", "address_unsupported", "Addresses with accented or non-Latin letters are not supported."},
		{"kim@b", "address_invalid", "This is not an e-mail address."},
	} {
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "kim", "displayName": "Kim", "email": c.address, "sendInvite": true})
		refused(t, rec, http.StatusBadRequest, c.code, c.say)
		refused(t, e.patch(adminPortal, mia, map[string]any{"email": c.address}), http.StatusBadRequest, c.code, c.say)
	}
	if e.kc.UserByName("kim") != "" || e.kc.User(mia)["email"] != "mia@example.org" || len(e.invites.rows) != 1 {
		t.Errorf("a refused address was kept: kim %q, mia %v, %d invites", e.kc.UserByName("kim"), e.kc.User(mia)["email"], len(e.invites.rows))
	}

	// An account the import, or the provider's console, gave a placeholder.
	lena := e.kc.Add(kctest.Seed{Username: "lena", FirstName: "Lena", Email: "lena@zaentrum.local", Roles: []string{kctest.UserRole}})
	doc := e.do(adminPortal, http.MethodGet, "/api/portal/people", nil)
	var listed struct {
		People []map[string]json.RawMessage `json:"people"`
	}
	if err := json.Unmarshal(doc.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	for _, p := range listed.People {
		var id string
		_ = json.Unmarshal(p["id"], &id)
		if _, has := p["email"]; id == lena && has {
			t.Errorf("GET /people answers the placeholder: %s", doc.Body)
		}
		if _, has := p["email"]; id == mia && !has {
			t.Errorf("GET /people lost mia's address: %s", doc.Body)
		}
	}
	rec := e.patch(adminPortal, lena, map[string]any{"displayName": "Lena K"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"email"`) || strings.Contains(rec.Body.String(), "zaentrum.local") {
		t.Errorf("PATCH answers the placeholder: %d %s", rec.Code, rec.Body)
	}
	if e.kc.User(lena)["email"] != "lena@zaentrum.local" {
		t.Errorf("the placeholder in the realm changed: %v", e.kc.User(lena)["email"])
	}
}

// R.C4: nothing holds an address between the look at the realm and the write,
// so the realm's own refusal at the write is the guard: where the listing
// shows the address free and another write takes it at that moment, adding a
// person and changing one are refused as another account's address, and
// nothing is kept.
func TestRC4TheRealmsRefusalAtTheWrite(t *testing.T) {
	e := newPeopleEnv(t)
	mia := e.add(map[string]any{"username": "mia", "displayName": "Mia", "role": "user"}).Person.ID
	race := func(username, address string) *kctest.Race {
		return &kctest.Race{Seed: &kctest.Seed{Username: username, FirstName: "Other", Email: address, Roles: []string{kctest.UserRole}}}
	}
	t.Run("R.C4 adding a person", func(t *testing.T) {
		e.kc.Race = race("other", "kim@example.org")
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/people", map[string]any{"username": "kim", "displayName": "Kim", "email": "kim@example.org", "sendInvite": true})
		refused(t, rec, http.StatusConflict, "address_taken", "Another account already has this address.")
		if e.kc.UserByName("kim") != "" || len(e.invites.rows) != 1 {
			t.Errorf("kim %q, %d invites", e.kc.UserByName("kim"), len(e.invites.rows))
		}
	})
	t.Run("R.C4 changing a person", func(t *testing.T) {
		e.kc.Race = race("another", "mia.k@example.org")
		refused(t, e.patch(adminPortal, mia, map[string]any{"email": "mia.k@example.org"}), http.StatusConflict,
			"address_taken", "Another account already has this address.")
		if e.kc.User(mia)["email"] != nil {
			t.Errorf("mia's address: %v", e.kc.User(mia)["email"])
		}
	})
	// The listing right before each refused write found the address free: the
	// refusal was the realm's.
	calls := e.kc.CallsMatching("")
	for _, write := range []string{"POST /users", "PUT /users/" + mia} {
		last := -1
		for i, c := range calls {
			if c == write {
				last = i
			}
		}
		if last < 1 || calls[last-1] != "GET /users" {
			t.Errorf("%s did not follow a listing: %v", write, calls)
		}
	}
}

// R.C4: with an external identity provider the platform keeps no address and
// makes no invite: invites and password resets are the provider's own. The
// routes are refused as before, an address and sendInvite or send with them,
// and nothing is mailed.
func TestRC4AnExternalIdentityProvider(t *testing.T) {
	e := newMailEnv(t, false, mailtest.Options{})
	e.ready(zmail.None)
	for _, rt := range []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodPost, "/api/portal/people", map[string]any{"username": "mia", "displayName": "Mia", "email": "mia@example.org", "sendInvite": true}},
		{http.MethodPatch, "/api/portal/people/user-mia", map[string]any{"email": "mia@example.org"}},
		{http.MethodPost, "/api/portal/people/user-mia/invite", map[string]any{"send": true}},
	} {
		rec := e.do(adminPortal, rt.method, rt.path, rt.body)
		if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "People live in your identity provider: add and change them there." {
			t.Errorf("%s %s = %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
	if n := len(e.smtp.Sessions()); n != 0 {
		t.Errorf("%d connections to the mail server", n)
	}
}
