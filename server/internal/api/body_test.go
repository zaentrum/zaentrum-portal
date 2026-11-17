package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
)

// R.C8: a body that is no JSON object, or that holds a field the route does
// not take, is answered 400 with a plain line of detail and no code, in no
// decoder's words.
func TestRC8ABodyThatIsNoObject(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	send := func(body string) (int, string) {
		req := httptest.NewRequest(http.MethodPatch, "/api/portal/alerts/settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+e.iss.Token(t, adminPortal))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, body := range []string{`[]`, `"mail"`, `5`, `null`, `true`, `not json`, `{"mail": tru`, ``} {
		if code, got := send(body); code != http.StatusBadRequest || got != "the body is not an object\n" {
			t.Errorf("%q = %d %q", body, code, got)
		}
	}
	if code, got := send(`{"mail": true, "colour": "red"}`); code != http.StatusBadRequest || got != "the field colour is not one this route takes\n" {
		t.Errorf("a field it does not take = %d %q", code, got)
	}
	if code, got := send(`{"mail": true, "x": "` + strings.Repeat("a", maxAlertsBody) + `"}`); code != http.StatusBadRequest || got != "the body is larger than this route takes\n" {
		t.Errorf("a body too large = %d %q", code, got)
	}
	if n := len(e.writes()); n != 0 {
		t.Errorf("%d writes", n)
	}
}

// R.C8: save on the mail page refuses a placeholder as the sender's address
// or a further one, with the test mail's sentence, and writes nothing.
func TestRC8SaveRefusesAPlaceholder(t *testing.T) {
	e := newMailEnv(t, true, mailtest.Options{})
	e.putPlatform(nil, "")
	for _, body := range []map[string]any{
		{"host": "smtp.example.org", "from": "zaentrum@zaentrum.local"},
		{"host": "smtp.example.org", "from": "zaentrum@example.org", "alertsTo": []any{"ops@example.org", "night@Zaentrum.Local"}},
	} {
		rec := e.do(adminPortal, http.MethodPut, "/api/portal/mail", body)
		refused(t, rec, http.StatusBadRequest, "address_placeholder", "This address is a placeholder: nothing is sent to it. Type another.")
	}
	if n := len(e.writes()); n != 0 {
		t.Errorf("%d writes", n)
	}
}
