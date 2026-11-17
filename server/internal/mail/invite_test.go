package mail

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"slices"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
)

// R.C4: the invite's two mails have the words of the section Words, the
// subject and every part of the text, each part a paragraph, and the date in
// UTC, saying so.
func TestRC4InviteMails(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	i := Invite{
		To: "mia@example.org", Name: "Mia Zoë", Username: "mia", Admin: "Anna", Host: "zaentrum.example.org",
		Link:    "https://zaentrum.example.org/portal/invite/aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5bC7dE9f",
		Expires: time.Date(2026, 11, 22, 15, 2, 37, 0, cet),
	}
	t.Run("R.C4 the invite of a person added", func(t *testing.T) {
		m := InviteMail(i)
		want := "Hello Mia Zoë,\n\n" +
			"Anna made an account for you on zaentrum.example.org. Your username is mia.\n\n" +
			"Open this link to choose your password:\n\n" +
			"https://zaentrum.example.org/portal/invite/aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5bC7dE9f\n\n" +
			"The link works once, until 22 November 2026, 14:02 UTC. After that, ask Anna for a new one.\n\n" +
			"If you did not expect this mail, ignore it.\n"
		if m.Kind != KindInvite || m.To != "mia@example.org" || m.Subject != "Your account on zaentrum.example.org" {
			t.Errorf("kind %q, to %q, subject %q", m.Kind, m.To, m.Subject)
		}
		if m.Text != want {
			t.Errorf("text:\n%s\nwant:\n%s", m.Text, want)
		}
	})
	t.Run("R.C4 the mail of a new link", func(t *testing.T) {
		m := NewLinkMail(i)
		want := "Hello Mia Zoë,\n\n" +
			"Anna made a new link for your account mia on zaentrum.example.org.\n\n" +
			"Open it to choose a new password:\n\n" +
			"https://zaentrum.example.org/portal/invite/aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5bC7dE9f\n\n" +
			"The link works once, until 22 November 2026, 14:02 UTC. Your password stays as it is until you use the link.\n\n" +
			"If you did not expect this mail, ignore it.\n"
		if m.Kind != KindNewLink || m.To != "mia@example.org" || m.Subject != "A new link for your account on zaentrum.example.org" {
			t.Errorf("kind %q, to %q, subject %q", m.Kind, m.To, m.Subject)
		}
		if m.Text != want {
			t.Errorf("text:\n%s\nwant:\n%s", m.Text, want)
		}
	})
	t.Run("R.C4 a time in a mail", func(t *testing.T) {
		for at, want := range map[time.Time]string{
			time.Date(2026, 11, 15, 14, 2, 0, 0, time.UTC): "15 November 2026, 14:02 UTC",
			time.Date(2026, 12, 5, 0, 30, 59, 0, cet):      "4 December 2026, 23:30 UTC",
		} {
			if got := When(at); got != want {
				t.Errorf("When(%v) = %q, want %q", at, got, want)
			}
		}
	})
	t.Run("R.C4 sent as it is written", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{})
		m := InviteMail(i)
		if err := sender(srv, None, "", "").Send(context.Background(), m); err != nil {
			t.Fatalf("send = %v", err)
		}
		mails := srv.Mails()
		if len(mails) != 1 || !slices.Equal(mails[0].To, []string{"mia@example.org"}) {
			t.Fatalf("mails = %+v", mails)
		}
		msg, err := mail.ReadMessage(bytes.NewReader(mails[0].Data))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
		if err != nil || subject != m.Subject || msg.Header.Get("Auto-Submitted") != "auto-generated" {
			t.Errorf("subject %q (%v), headers %v", subject, err, msg.Header)
		}
		text, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
		if err != nil || string(text) != m.Text {
			t.Errorf("text as sent (%v):\n%s", err, text)
		}
	})
}
