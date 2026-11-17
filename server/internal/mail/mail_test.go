package mail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
)

const (
	testFrom = "zaentrum@example.org"
	testTo   = "mia@example.org"
	testHost = "zaentrum.example.org"
)

// sender is a Sender for srv that trusts srv's own certificate, with a
// short timeout for the tests.
func sender(srv *mailtest.Server, encryption, user, password string) *Sender {
	s := New(Settings{Host: srv.Host, Port: srv.Port, Encryption: encryption, From: testFrom, Username: user, Password: password}, testHost)
	s.roots = srv.CA
	s.total = 5 * time.Second
	return s
}

func send(t *testing.T, s *Sender) error {
	t.Helper()
	return s.Send(context.Background(), TestMail(testHost, testTo))
}

func verbs(cmds []mailtest.Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Verb)
	}
	return out
}

// failure is err as a *Failure, failing the test when it is none.
func failure(t *testing.T, err error) *Failure {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("want a *Failure, got %v", err)
	}
	return f
}

// R.C2: with starttls the server has to offer STARTTLS after its greeting;
// everything after it — the sign-in, the sender, the mail — is encrypted, and
// a server that does not offer it is refused and told nothing in the clear
// but the EHLO that asked.
func TestRC2StartTLS(t *testing.T) {
	t.Run("R.C2 offered: the sign-in and the mail go encrypted", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{StartTLS: true, Auth: []string{"PLAIN", "LOGIN"}, User: "zaentrum", Password: "password-for-tests"})
		if err := send(t, sender(srv, StartTLS, "zaentrum", "password-for-tests")); err != nil {
			t.Fatalf("send = %v", err)
		}
		mails := srv.Mails()
		if len(mails) != 1 || !mails[0].TLS || mails[0].User != "zaentrum" {
			t.Fatalf("mails = %+v", mails)
		}
		cmds := srv.Sessions()[0]
		if got := verbs(cmds); !slices.Equal(got[:3], []string{"EHLO", "STARTTLS", "EHLO"}) {
			t.Fatalf("commands = %v", got)
		}
		for _, c := range cmds[2:] {
			if !c.TLS {
				t.Errorf("%s came in the clear", c.Verb)
			}
		}
	})
	t.Run("R.C2 not offered: refused, and nothing but EHLO is said", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{Auth: []string{"PLAIN"}, User: "zaentrum", Password: "password-for-tests"})
		f := failure(t, send(t, sender(srv, StartTLS, "zaentrum", "password-for-tests")))
		if f.Reason != NoEncryption || f.Sentence() != "The mail server doesn't offer this encryption. Choose another." {
			t.Errorf("failure = %+v %q", f, f.Sentence())
		}
		if got := verbs(srv.Sessions()[0]); !slices.Equal(got, []string{"EHLO"}) {
			t.Errorf("the client said %v to a server that offers no encryption", got)
		}
		if len(srv.Mails()) != 0 {
			t.Error("a mail went out in the clear")
		}
	})
	t.Run("R.C2 offered and then refused", func(t *testing.T) {
		srv := mailtest.Start(t, mailtest.Options{StartTLS: true, Replies: map[string]string{"STARTTLS": "454 4.7.0 TLS not available"}})
		f := failure(t, send(t, sender(srv, StartTLS, "", "")))
		if f.Reason != NoEncryption {
			t.Errorf("failure = %+v", f)
		}
		if got := verbs(srv.Sessions()[0]); !slices.Equal(got, []string{"EHLO", "STARTTLS"}) {
			t.Errorf("commands = %v", got)
		}
	})
}

// R.C2: with tls the connection is encrypted from the start; a server that
// speaks plain SMTP there does not offer that encryption.
func TestRC2TLSFromTheStart(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{TLS: true, Auth: []string{"PLAIN"}, User: "zaentrum", Password: "password-for-tests"})
	if err := send(t, sender(srv, TLS, "zaentrum", "password-for-tests")); err != nil {
		t.Fatalf("send = %v", err)
	}
	cmds := srv.Sessions()[0]
	if len(cmds) == 0 || !cmds[0].TLS || len(srv.Mails()) != 1 {
		t.Fatalf("commands %+v, mails %d", cmds, len(srv.Mails()))
	}

	plain := mailtest.Start(t, mailtest.Options{})
	f := failure(t, send(t, sender(plain, TLS, "", "")))
	if f.Reason != NoEncryption || len(plain.Mails()) != 0 {
		t.Errorf("tls to a plain server: %+v", f)
	}
}

// R.C2: with none nothing is encrypted — the sign-in included, as the admin
// chose (the mail page warns).
func TestRC2NoneIsInTheClear(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{Auth: []string{"PLAIN"}, User: "zaentrum", Password: "password-for-tests"})
	if err := send(t, sender(srv, None, "zaentrum", "password-for-tests")); err != nil {
		t.Fatalf("send = %v", err)
	}
	m := srv.Mails()
	if len(m) != 1 || m[0].TLS || m[0].User != "zaentrum" {
		t.Fatalf("mails = %+v", m)
	}
	if got := verbs(srv.Sessions()[0]); slices.Contains(got, "STARTTLS") {
		t.Errorf("none asked for encryption: %v", got)
	}
}

// R.C2: the server's certificate is checked against the authorities the image
// trusts — a server's own certificate is not one of theirs — and the session
// stops there, before the sign-in.
func TestRC2CertificateIsChecked(t *testing.T) {
	for _, enc := range []string{StartTLS, TLS} {
		t.Run("R.C2 "+enc, func(t *testing.T) {
			srv := mailtest.Start(t, mailtest.Options{TLS: enc == TLS, StartTLS: enc == StartTLS, Auth: []string{"PLAIN"}, User: "zaentrum", Password: "password-for-tests"})
			s := sender(srv, enc, "zaentrum", "password-for-tests")
			s.roots = nil // the system's
			f := failure(t, send(t, s))
			if f.Reason != Certificate || f.Sentence() != "The mail server's certificate could not be checked." {
				t.Errorf("failure = %+v", f)
			}
			for _, sess := range srv.Sessions() {
				for _, c := range sess {
					if c.Verb == "AUTH" || c.Verb == "MAIL" {
						t.Errorf("%s was sent after a certificate that did not pass", c.Verb)
					}
				}
			}
		})
	}
}

// R.C2: portal-api signs in when a user is set, and only then; a server that
// offers only LOGIN is signed in to with LOGIN.
func TestRC2SignsInOnlyWithAUser(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{Auth: []string{"PLAIN", "LOGIN"}, User: "zaentrum", Password: "password-for-tests"})
	if err := send(t, sender(srv, None, "", "")); err != nil {
		t.Fatalf("send = %v", err)
	}
	if got := verbs(srv.Sessions()[0]); slices.Contains(got, "AUTH") {
		t.Errorf("signed in without a user: %v", got)
	}

	login := mailtest.Start(t, mailtest.Options{Auth: []string{"LOGIN"}, User: "zaentrum", Password: "password-for-tests"})
	if err := send(t, sender(login, None, "zaentrum", "password-for-tests")); err != nil {
		t.Fatalf("send with LOGIN = %v", err)
	}
	if m := login.Mails(); len(m) != 1 || m[0].User != "zaentrum" {
		t.Errorf("mails = %+v", m)
	}
}

// R.C2: a mail that is not sent says why, in the words of the mail page — and
// what goes into the log says the reason alone: never the address, never the
// server's words.
func TestRC2RefusalsSayWhy(t *testing.T) {
	for _, c := range []struct {
		name     string
		opts     mailtest.Options
		user     string
		closed   bool
		reason   Reason
		sentence string
	}{
		{name: "nothing listens", closed: true, reason: Unreachable,
			sentence: "Can't reach the mail server. Check its name and its port."},
		{name: "the password is wrong", opts: mailtest.Options{Auth: []string{"PLAIN"}, User: "zaentrum", Password: "another"}, user: "zaentrum",
			reason: SignIn, sentence: "The mail server refused the user or the password."},
		{name: "no sign-in is offered", opts: mailtest.Options{}, user: "zaentrum",
			reason: SignIn, sentence: "The mail server refused the user or the password."},
		{name: "the sender is refused", opts: mailtest.Options{Replies: map[string]string{"MAIL": "553 5.7.1 sender address rejected"}},
			reason: SenderRefused, sentence: "The mail server refused the sender's address."},
		{name: "the address is refused", opts: mailtest.Options{Replies: map[string]string{"RCPT": "550 5.1.1 <mia@example.org>: no such user"}},
			reason: RecipientRefused, sentence: "The mail server refused the address mia@example.org."},
		{name: "a relay is refused", opts: mailtest.Options{Replies: map[string]string{"RCPT": "554 5.7.1 <mia@example.org>: relay access denied"}},
			reason: Answered, sentence: "The mail server answered: 5.7.1 <mia@example.org>: relay access denied"},
		{name: "the greeting refuses", opts: mailtest.Options{Greeting: "554 5.3.2 no service here"},
			reason: Answered, sentence: "The mail server answered: 5.3.2 no service here"},
		{name: "the mail is refused", opts: mailtest.Options{Replies: map[string]string{".": "552 5.3.4 message too big"}},
			reason: Answered, sentence: "The mail server answered: 5.3.4 message too big"},
		{name: "the server says nothing", opts: mailtest.Options{Silent: true},
			reason: TimedOut, sentence: "The mail server didn't answer in time."},
	} {
		t.Run("R.C2 "+c.name, func(t *testing.T) {
			var s *Sender
			if c.closed {
				s = New(Settings{Host: "127.0.0.1", Port: mailtest.ClosedPort(t), Encryption: None, From: testFrom}, testHost)
			} else {
				srv := mailtest.Start(t, c.opts)
				s = sender(srv, None, c.user, "password-for-tests")
				s.total = 500 * time.Millisecond
			}
			f := failure(t, send(t, s))
			if f.Reason != c.reason || f.Sentence() != c.sentence {
				t.Errorf("failure = %s %q, want %s %q", f.Reason, f.Sentence(), c.reason, c.sentence)
			}
			if strings.Contains(f.Error(), "example.org") || strings.Contains(f.Error(), "5.") {
				t.Errorf("Error() carries an address or the server's words: %q", f.Error())
			}
		})
	}
}

// R.C2: every mail is plain text in UTF-8, to one address, with the headers
// From, To, Subject, Date, Message-ID and Auto-Submitted: auto-generated, and
// what makes it plain text in UTF-8 — nothing else; the test mail has the
// words of the section Words.
func TestRC2TheMailIsPlainTextToOneAddress(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{})
	s := New(Settings{Host: srv.Host, Port: srv.Port, Encryption: None, From: testFrom, FromName: "Zäntrum"}, testHost)
	s.now = func() time.Time { return time.Date(2026, 11, 15, 14, 2, 0, 0, time.UTC) }
	if err := send(t, s); err != nil {
		t.Fatalf("send = %v", err)
	}
	mails := srv.Mails()
	if len(mails) != 1 || mails[0].From != testFrom || !slices.Equal(mails[0].To, []string{testTo}) {
		t.Fatalf("mails = %+v", mails)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(mails[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for k := range msg.Header {
		names = append(names, k)
	}
	slices.Sort(names)
	want := []string{"Auto-Submitted", "Content-Transfer-Encoding", "Content-Type", "Date", "From", "Message-Id", "Mime-Version", "Subject", "To"}
	if !slices.Equal(names, want) {
		t.Errorf("headers = %v, want %v", names, want)
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Name != "Zäntrum" || from.Address != testFrom {
		t.Errorf("From = %q (%v)", msg.Header.Get("From"), err)
	}
	to, err := mail.ParseAddressList(msg.Header.Get("To"))
	if err != nil || len(to) != 1 || to[0].Address != testTo {
		t.Errorf("To = %q (%v)", msg.Header.Get("To"), err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != "Test mail from zaentrum.example.org" {
		t.Errorf("Subject = %q (%v)", subject, err)
	}
	if d, err := msg.Header.Date(); err != nil || !d.Equal(time.Date(2026, 11, 15, 14, 2, 0, 0, time.UTC)) {
		t.Errorf("Date = %q (%v)", msg.Header.Get("Date"), err)
	}
	if id := msg.Header.Get("Message-Id"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.org>") {
		t.Errorf("Message-ID = %q", id)
	}
	if got := msg.Header.Get("Auto-Submitted"); got != "auto-generated" {
		t.Errorf("Auto-Submitted = %q", got)
	}
	if got := msg.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimRight(string(body), "\r\n"); got != "This mail was sent from the mail page of zaentrum.example.org, to check that mail works. Nothing else needs doing." {
		t.Errorf("text = %q", got)
	}
}

// R.C2: one connection for each mail, and the timeouts: 10 seconds to
// connect, 30 seconds in all.
func TestRC2OneConnectionForEachMail(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{})
	s := sender(srv, None, "", "")
	for range 2 {
		if err := send(t, s); err != nil {
			t.Fatal(err)
		}
	}
	sessions := srv.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("%d connections for two mails", len(sessions))
	}
	for _, sess := range sessions {
		if n := strings.Count(strings.Join(verbs(sess), " "), "MAIL"); n != 1 {
			t.Errorf("a connection carried %d mails", n)
		}
	}
	if d := New(Settings{}, ""); d.connect != 10*time.Second || d.total != 30*time.Second {
		t.Errorf("timeouts = %s to connect, %s in all", d.connect, d.total)
	}
}

// R.C2: the settings' defaults — the port by the encryption, STARTTLS, the
// sender's name — and what mail being set up takes: a server and a sender.
func TestRC2SettingsDefaults(t *testing.T) {
	for enc, port := range map[string]int{"": 587, StartTLS: 587, TLS: 465, None: 25} {
		if got := (Settings{Encryption: enc}).Effective(); got.Port != port || got.FromName != DefaultFromName {
			t.Errorf("encryption %q: port %d, name %q", enc, got.Port, got.FromName)
		}
	}
	if got := (Settings{Port: 2525, Encryption: " TLS "}).Effective(); got.Port != 2525 || got.Encryption != TLS {
		t.Errorf("set: %+v", got)
	}
	for _, s := range []Settings{{}, {Host: "smtp.example.org"}, {From: testFrom}, {Host: " ", From: testFrom}} {
		if s.SetUp() {
			t.Errorf("%+v reads as set up", s)
		}
	}
	if !(Settings{Host: "smtp.example.org", From: testFrom}).SetUp() {
		t.Error("a server and a sender are mail set up")
	}
	// An encryption that is none of the three sends nothing, never in the clear.
	srv := mailtest.Start(t, mailtest.Options{})
	if f := failure(t, send(t, sender(srv, "ssl", "", ""))); f.Reason != NoEncryption || len(srv.Sessions()) != 0 {
		t.Errorf("an unknown encryption: %+v, %d connections", f, len(srv.Sessions()))
	}
	if err := New(Settings{Host: "smtp.example.org"}, "").Send(context.Background(), TestMail(testHost, testTo)); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("without a sender: %v", err)
	}
}

// R.C2: an address is one bare address of at most 254 characters — no name,
// no angle brackets, no line break — whose domain is a host name of at least
// two parts, in ASCII letters; one that would be an address but for accented
// or non-Latin letters is refused for its letters. The name portal-api gives
// itself in EHLO is the platform's host.
func TestRC2Addresses(t *testing.T) {
	long := strings.Repeat("a", 64) + "@" + strings.Repeat(strings.Repeat("b", 62)+".", 3) + "example.org"
	for raw, want := range map[string]error{
		"mia@example.org": nil, " mia@example.org ": nil, "mia.k+alerts@mail.example.org": nil,
		"o'hara!#$%&*/=?^_`{|}~-@example.org": nil, "mia@xn--zntrum-cua.example.org": nil, "mia@a-b.example.org": nil,
		"mia@1.example.org": nil, strings.Repeat("a", 64) + "@" + strings.Repeat("b", 63) + ".example.org": nil,
		"": ErrNotAnAddress, "mia": ErrNotAnAddress, "Mia <mia@example.org>": ErrNotAnAddress, "<mia@example.org>": ErrNotAnAddress,
		"mia@example.org, tom@example.org": ErrNotAnAddress, "mia@example.org\r\nBcc: tom@example.org": ErrNotAnAddress,
		strings.Repeat("a", 250) + "@example.org": ErrNotAnAddress, long: ErrNotAnAddress,
		// The domain is a host name of at least two parts, and no IP address.
		"a@b": ErrNotAnAddress, "a@[127.0.0.1]": ErrNotAnAddress, "a@-example.org": ErrNotAnAddress,
		"a@example-.org": ErrNotAnAddress, "a@example.org.": ErrNotAnAddress, "a@.example.org": ErrNotAnAddress,
		"a@example..org": ErrNotAnAddress, "a@exa_mple.org": ErrNotAnAddress, "a@127.0.0.1": ErrNotAnAddress,
		"a@example.123": ErrNotAnAddress, "a@" + strings.Repeat("b", 64) + ".org": ErrNotAnAddress,
		// The local part: atoms between single dots, nothing quoted.
		"a..b@example.org": ErrNotAnAddress, ".a@example.org": ErrNotAnAddress, "a.@example.org": ErrNotAnAddress,
		`"mia k"@example.org`: ErrNotAnAddress, "mia k@example.org": ErrNotAnAddress, "mia(x)@example.org": ErrNotAnAddress,
		"mia@@example.org": ErrNotAnAddress, "@example.org": ErrNotAnAddress, "mia@": ErrNotAnAddress,
		"mia\x00@example.org": ErrNotAnAddress, "mia@example.org\x00": ErrNotAnAddress, "mia\t@example.org": ErrNotAnAddress,
		"mia@exa\nmple.org": ErrNotAnAddress, "mia\u00a0@example.org": ErrNotAnAddress, "mia\u200b@example.org": ErrNotAnAddress,
		"mia\U0001F600@example.org": ErrNotAnAddress, "mïa@example": ErrNotAnAddress, "\xffmia@example.org": ErrNotAnAddress,
		// Accented and non-Latin letters.
		"mïa@example.org": ErrAddressLetters, "mia@zäntrum.example.org": ErrAddressLetters, "δοκιμή@example.org": ErrAddressLetters,
		"用户@example.org": ErrAddressLetters, "jose\u0301@example.org": ErrAddressLetters,
	} {
		got, err := ReadAddress(raw)
		if !errors.Is(err, want) || err == nil && got != strings.TrimSpace(raw) {
			t.Errorf("ReadAddress(%q) = %q, %v; want %v", raw, got, err, want)
		}
		if _, ok := Address(raw); ok != (want == nil) {
			t.Errorf("Address(%q) = %v, want %v", raw, ok, want == nil)
		}
	}
	for name, want := range map[string]string{
		"zaentrum.example.org": "zaentrum.example.org", "": "localhost", "127.0.0.1": "[127.0.0.1]", "::1": "[IPv6:::1]",
	} {
		if got := ehloName(name); got != want {
			t.Errorf("ehloName(%q) = %q, want %q", name, got, want)
		}
	}
	srv := mailtest.Start(t, mailtest.Options{})
	if err := send(t, sender(srv, None, "", "")); err != nil {
		t.Fatal(err)
	}
	if c := srv.Sessions()[0][0]; c.Verb != "EHLO" || c.Arg != testHost {
		t.Errorf("first command = %+v", c)
	}
}

// R.C2: each reason a mail was not sent has a code of its own, which says the
// case and not its sentence, and the values its sentence names: the address
// the server refused, or the server's own words.
func TestRC2EachReasonHasItsCode(t *testing.T) {
	seen := map[string]Reason{}
	for _, c := range []struct {
		f      Failure
		code   string
		values map[string]any
	}{
		{Failure{Reason: Unreachable}, "mail_server_unreachable", nil},
		{Failure{Reason: NoEncryption}, "mail_encryption_not_offered", nil},
		{Failure{Reason: Certificate}, "mail_certificate_unverified", nil},
		{Failure{Reason: SignIn}, "mail_sign_in_refused", nil},
		{Failure{Reason: SenderRefused}, "mail_sender_refused", nil},
		{Failure{Reason: RecipientRefused, Address: "mia@example.org"}, "mail_recipient_refused", map[string]any{"address": "mia@example.org"}},
		{Failure{Reason: TimedOut}, "mail_server_timeout", nil},
		{Failure{Reason: Answered, Words: "452 4.3.1 mailbox full"}, "mail_server_answered", map[string]any{"words": "452 4.3.1 mailbox full"}},
	} {
		if got := c.f.Code(); got != c.code {
			t.Errorf("%s: code %q, want %q", c.f.Reason, got, c.code)
		}
		if r, dup := seen[c.code]; dup {
			t.Errorf("%s and %s share the code %s", r, c.f.Reason, c.code)
		}
		seen[c.code] = c.f.Reason
		if got := c.f.Values(); !reflect.DeepEqual(got, c.values) {
			t.Errorf("%s: values %v, want %v", c.f.Reason, got, c.values)
		}
	}
}

// headerLines are a composed mail's header lines, as they are sent.
func headerLines(t *testing.T, raw []byte) []string {
	t.Helper()
	head, _, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("no end of the header in %q", raw)
	}
	return strings.Split(string(head), "\r\n")
}

// composed is a mail from name, with subject, as it is sent, and as a mail
// program reads it.
func composed(t *testing.T, name, subject string) ([]string, *mail.Message) {
	t.Helper()
	raw := compose(Message{To: testTo, Subject: subject, Text: "text\n"}, testFrom, name, testTo, time.Now(), "id@example.org")
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("name %q, subject %q: %v\n%s", name, subject, err, raw)
	}
	return headerLines(t, raw), msg
}

// readBack is the sender's name and the subject as a mail program reads them
// from the headers.
func readBack(t *testing.T, msg *mail.Message) (string, string) {
	t.Helper()
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Address != testFrom {
		t.Errorf("From %q: %+v, %v", msg.Header.Get("From"), from, err)
		return "", ""
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Errorf("Subject %q: %v", msg.Header.Get("Subject"), err)
	}
	return from.Name, subject
}

// R.C2: a header is ASCII. The sender's name and the subject are written so
// that a mail program reads back exactly what was given, whatever it holds —
// letters beyond ASCII, quotes, a backslash, a comma, what looks like an
// encoded word: a name in a quoted string or in encoded words, never as it is
// typed, and a subject in encoded words where it is not plain ASCII.
func TestRC2TheSenderAndTheSubjectAreWrittenAsHeaders(t *testing.T) {
	for _, c := range []struct{ name, subject string }{
		{"Zäntrum", "Grüezi from zäntrum.example.org"},
		{`Our "home" server`, "Test mail from zaentrum.example.org"},
		{`back\slash`, "日本語の件名"},
		{"Mia, the server <ops>", "=?utf-8?q?not_an_encoded_word?="},
		{"=?utf-8?q?Mia?=", "two  spaces"},
		{"Ελληνικά (home)", " leading and trailing "},
		{"plain", "under_score and = and ?"},
	} {
		lines, msg := composed(t, c.name, c.subject)
		for _, l := range lines {
			for i := 0; i < len(l); i++ {
				if l[i] < ' ' && l[i] != '\t' || l[i] > '~' {
					t.Errorf("name %q, subject %q: a header line holds %q: %q", c.name, c.subject, l[i], l)
					break
				}
			}
		}
		if name, subject := readBack(t, msg); name != c.name || subject != c.subject {
			t.Errorf("read back as name %q, subject %q; want %q, %q", name, subject, c.name, c.subject)
		}
	}
}

// R.C2: a header is folded between its words: no line is longer than 78
// characters where folding can keep it so, none longer than 998 (RFC 5322),
// and the sender's name and the subject read back as they were given — a
// subject of 80 four-byte characters among them, which unfolded was one line
// of 1176 octets.
func TestRC2HeadersAreFolded(t *testing.T) {
	check := func(name, subject string) {
		t.Helper()
		lines, msg := composed(t, name, subject)
		for _, l := range lines {
			if len(l) > 78 && !strings.HasPrefix(strings.TrimSpace(l), "<") {
				t.Errorf("a line of %d characters: %q", len(l), l)
			}
			if len(l) > 998 {
				t.Errorf("a line of %d octets", len(l))
			}
		}
		if n, s := readBack(t, msg); n != name || s != subject {
			t.Errorf("read back as name %q, subject %q; want %q, %q", n, s, name, subject)
		}
	}
	check(strings.Repeat("\U0001F600", 60), strings.Repeat("\U0001F600", 80))
	check(strings.Repeat(`"`, 60), strings.Repeat("Ü", 300))
	check("Our server", "A new link for your account on "+strings.Repeat("long-name.", 24)+"example.org")
	check("Our server", strings.Repeat("word ", 60)+"end")

	// Whatever a name and a subject hold.
	rnd := rand.New(rand.NewPCG(1, 2))
	parts := []string{"a", "Z", "0", " ", " ", "  ", "_", "=", "?", "=?", "?=", `"`, `\`, ",", "<", "(", "ä", "日", "\U0001F600", "́", "word", "utf-8"}
	random := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(parts[rnd.IntN(len(parts))])
		}
		return b.String()
	}
	for range 300 {
		name := strings.TrimSpace(random(1 + rnd.IntN(20)))
		subject := random(1 + rnd.IntN(120))
		if name == "" || strings.ContainsAny(name, " ") && strings.Contains(name, "  ") {
			// A mail program reads a name's runs of spaces as one.
			name = strings.ReplaceAll(name, " ", "_")
		}
		check(name, subject)
	}
}

// R.C2: a connection has 10 seconds to open, apart from the 30 the whole mail
// has: a server that does not answer the connection is given up when the
// connection's time is out, not the mail's.
func TestRC2TheConnectionHasATimeoutOfItsOwn(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{})
	s := sender(srv, None, "", "")
	s.connect, s.total = 200*time.Millisecond, 5*time.Second
	s.control = func(ctx context.Context, _, _ string, _ syscall.RawConn) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	f := failure(t, send(t, s))
	if took := time.Since(start); f.Reason != Unreachable || took > 2*time.Second {
		t.Errorf("a connection that does not open: %s after %s, want %s after about %s", f.Reason, took.Round(time.Millisecond), Unreachable, s.connect)
	}
	if len(srv.Sessions()) != 0 {
		t.Error("the server was spoken to")
	}
}

// R.C2: of a server's answer only its words are repeated, on one line and
// without a control character.
func TestRC2TheServersWordsHoldNoControlCharacter(t *testing.T) {
	for msg, want := range map[string]string{
		"5.7.1 relay\x00 denied":                 "5.7.1 relay denied",
		"5.7.1 \x1b[31mred\x1b[0m\r\nBcc: x\x7f": "5.7.1 [31mred [0m Bcc: x",
		"5.7.1 \u0085next\u009bline":             "5.7.1 next line",
		"\x00\x01":                               "554",
	} {
		if got := serverWords(&textproto.Error{Code: 554, Msg: msg}); got != want {
			t.Errorf("serverWords(%q) = %q, want %q", msg, got, want)
		}
	}
}
