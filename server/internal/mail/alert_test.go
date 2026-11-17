package mail

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
)

// R.C8: an alert's two mails have the words of the section Words, The mails:
// the start mail its title and text, where to see it and why the reader gets
// it; the end mail "Ended:" and its last title, the sentence of its end, when
// it started and ended, in UTC and saying so, and why the reader gets it.
func TestRC8AlertMails(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	why := "You get this mail because you are an admin of zaentrum.example.org, or your address is in its alert settings.\n"
	t.Run("R.C8 the mail of an alert that started", func(t *testing.T) {
		m := AlertMail(AlertStart{
			To: "anna@example.org", Host: "zaentrum.example.org", Link: "https://zaentrum.example.org/portal/operator",
			Title: "A part is not ready: katalog-api", Text: "katalog-api has not been ready for more than 10 minutes. Open operator to see why.",
		})
		want := "katalog-api has not been ready for more than 10 minutes. Open operator to see why.\n\n" +
			"See it: https://zaentrum.example.org/portal/operator\n\n" + why
		if m.Kind != KindAlert || m.To != "anna@example.org" || m.Subject != "A part is not ready: katalog-api" || m.Text != want {
			t.Errorf("kind %q, to %q, subject %q, text:\n%s", m.Kind, m.To, m.Subject, m.Text)
		}
	})
	t.Run("R.C8 the mail of an alert that ended", func(t *testing.T) {
		m := AlertEndMail(AlertEnd{
			To: "anna@example.org", Host: "zaentrum.example.org", Title: "A part is not ready: katalog-api", Sentence: "katalog-api is ready again.",
			Started: time.Date(2026, 11, 16, 13, 2, 0, 0, cet), Ended: time.Date(2026, 11, 16, 14, 30, 0, 0, time.UTC),
		})
		want := "katalog-api is ready again.\n\n" +
			"It started on 16 November 2026, 12:02 UTC and ended on 16 November 2026, 14:30 UTC.\n\n" + why
		if m.Kind != KindAlertEnd || m.Subject != "Ended: A part is not ready: katalog-api" || m.Text != want {
			t.Errorf("kind %q, subject %q, text:\n%s", m.Kind, m.Subject, m.Text)
		}
	})
	t.Run("R.C8 sent as it is written, a long subject folded", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{})
		m := AlertEndMail(AlertEnd{
			To: "anna@example.org", Host: "zaentrum.example.org", Title: "A volume is almost full: library, databases, backups and event broker",
			Sentence: "The disk of library, databases, backups and event broker is at 80 %.", Started: time.Now(), Ended: time.Now(),
		})
		if err := sender(srv, None, "", "").Send(context.Background(), m); err != nil {
			t.Fatalf("send = %v", err)
		}
		data := srv.Mails()[0].Data
		head, _, _ := bytes.Cut(data, []byte("\n\n"))
		for _, line := range strings.Split(string(head), "\n") {
			if len(line) > 78 {
				t.Errorf("a header line of %d characters: %q", len(line), line)
			}
		}
		msg, err := mail.ReadMessage(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
		if err != nil || subject != m.Subject || msg.Header.Get("Auto-Submitted") != "auto-generated" {
			t.Errorf("subject %q (%v)", subject, err)
		}
		text, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
		if err != nil || string(text) != m.Text {
			t.Errorf("text as sent (%v):\n%s", err, text)
		}
	})
}
