// Package mail sends the platform's mail through the SMTP server the admin
// names: spec.mail of the platform's resource, which the operator gives
// portal-api as its environment. Every mail is plain text in UTF-8, to one
// address, and goes over a connection of its own: 10 seconds to connect, 30
// seconds in all.
//
// The encryption is the admin's:
//
//   - starttls: the server has to offer STARTTLS after its greeting. One that
//     does not is refused, and nothing is said to it in the clear but the EHLO
//     that asks: no sign-in, no sender, no mail.
//   - tls: the connection is encrypted from the start.
//   - none: nothing is encrypted; the mail page warns.
//
// A server's certificate is checked against the authorities the image trusts,
// for the server's name; nothing here skips the check. portal-api signs in
// when a user is set, and only then.
//
// A mail that is not sent fails with a *Failure, which says why in the words
// of the mail page (Sentence). Its Error holds no address and no text, so a
// log line written from it holds neither.
package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// The encryptions a mail server is spoken to with.
const (
	// StartTLS: plain at first; the server has to offer STARTTLS after its
	// greeting, and everything after it is encrypted.
	StartTLS = "starttls"
	// TLS: encrypted from the start.
	TLS = "tls"
	// None: nothing is encrypted.
	None = "none"
)

const (
	// DefaultFromName is the sender's name when none is set.
	DefaultFromName = "zaentrum"
	// MaxFromName is the most characters a sender's name has.
	MaxFromName = 60
	// MaxAlertsTo is the most further addresses that get every alert mail.
	MaxAlertsTo = 10
	// ConnectTimeout is how long a connection to the server may take to
	// open; Timeout is how long one mail may take in all.
	ConnectTimeout = 10 * time.Second
	Timeout        = 30 * time.Second
)

// KnownEncryption answers whether e is one of the three encryptions.
func KnownEncryption(e string) bool { return e == StartTLS || e == TLS || e == None }

// DefaultPort is an encryption's port: 587 with starttls, 465 with tls and
// 25 with none.
func DefaultPort(encryption string) int {
	switch encryption {
	case TLS:
		return 465
	case None:
		return 25
	}
	return 587
}

// Settings are a mail server and the sender, as portal-api is given them.
type Settings struct {
	Host string
	// Port is 0 for the encryption's default.
	Port int
	// Encryption is starttls, tls or none; "" is starttls.
	Encryption string
	// From is the sender's address, and FromName the name a mail program
	// shows for it; "" is DefaultFromName.
	From     string
	FromName string
	// AlertsTo are further addresses that get every alert mail.
	AlertsTo []string
	// Username and Password are the sign-in; without a user there is none.
	Username string
	Password string
}

// SetUp answers whether a server and a sender are named: without both, no
// mail is sent.
func (s Settings) SetUp() bool {
	return strings.TrimSpace(s.Host) != "" && strings.TrimSpace(s.From) != ""
}

// Effective is s as a mail is sent with it: trimmed, and the defaults filled
// in. An encryption that is none of the three stays as it is, and nothing is
// sent with it.
func (s Settings) Effective() Settings {
	s.Host = strings.TrimSpace(s.Host)
	s.From = strings.TrimSpace(s.From)
	s.Encryption = strings.ToLower(strings.TrimSpace(s.Encryption))
	if s.Encryption == "" {
		s.Encryption = StartTLS
	}
	if s.Port < 1 || s.Port > 65535 {
		s.Port = DefaultPort(s.Encryption)
	}
	s.FromName = strings.TrimSpace(s.FromName)
	if s.FromName == "" {
		s.FromName = DefaultFromName
	}
	s.Username = strings.TrimSpace(s.Username)
	return s
}

// ErrNotSetUp: no server or no sender is named, so nothing is sent.
var ErrNotSetUp = errors.New("mail is not set up: no server or no sender is named")

// Sender sends mail through one server. It is safe for concurrent use: each
// mail opens its own connection.
type Sender struct {
	settings  Settings
	localName string
	// roots are the authorities a certificate is checked against; nil is
	// the system's, which is what the image trusts. Tests trust their own.
	roots *x509.CertPool
	// connect and total are ConnectTimeout and Timeout; tests shorten them.
	connect, total time.Duration
	now            func() time.Time
	// control, when a test sets it, runs inside each connection's dial,
	// before the connection is opened, under the dial's deadline: it stands
	// in for a server that does not answer the connection.
	control func(ctx context.Context, network, address string, c syscall.RawConn) error
}

// New is a Sender for the settings. localName is the name portal-api gives
// itself in EHLO: the platform's host, else localhost.
func New(s Settings, localName string) *Sender {
	return &Sender{
		settings: s.Effective(), localName: ehloName(localName),
		connect: ConnectTimeout, total: Timeout, now: time.Now,
	}
}

// ehloName is a name for EHLO: a host name as it is, an IP address as an
// address literal, localhost for none.
func ehloName(name string) string {
	name = strings.Trim(strings.TrimSpace(name), "[]")
	if name == "" || strings.ContainsAny(name, " \r\n\t") {
		return "localhost"
	}
	if ip := net.ParseIP(name); ip != nil {
		if ip.To4() != nil {
			return "[" + ip.String() + "]"
		}
		return "[IPv6:" + ip.String() + "]"
	}
	return name
}

// Send sends m, while the caller waits: nil once the server took it, else a
// *Failure that says why it did not (or ErrNotSetUp).
func (s *Sender) Send(ctx context.Context, m Message) error {
	st := s.settings
	if !st.SetUp() {
		return ErrNotSetUp
	}
	if !KnownEncryption(st.Encryption) {
		return &Failure{Reason: NoEncryption}
	}
	from, ok := Address(st.From)
	if !ok {
		return &Failure{Reason: SenderRefused}
	}
	to, ok := Address(m.To)
	if !ok {
		return &Failure{Reason: RecipientRefused, Address: m.To}
	}
	ctx, cancel := context.WithTimeout(ctx, s.total)
	defer cancel()
	return s.send(ctx, from, to, m)
}

// The steps of a session, which say what a server's refusal means.
type step int

const (
	stepTLS step = iota // the handshake of a connection encrypted from the start
	stepGreeting
	stepHello
	stepStartTLS
	stepAuth
	stepSender
	stepRecipient
	stepData
)

func (s *Sender) send(ctx context.Context, from, to string, m Message) error {
	st := s.settings
	dialer := net.Dialer{Timeout: s.connect, ControlContext: s.control}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(st.Host, strconv.Itoa(st.Port)))
	if err != nil {
		return &Failure{Reason: Unreachable}
	}
	defer raw.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	// A caller that goes away takes its mail with it.
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Now()) })
	defer stop()

	conn := raw
	tlsConfig := &tls.Config{ServerName: st.Host, RootCAs: s.roots, MinVersion: tls.VersionTLS12}
	if st.Encryption == TLS {
		tc := tls.Client(conn, tlsConfig)
		if err := tc.HandshakeContext(ctx); err != nil {
			return failed(ctx, stepTLS, to, err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, st.Host)
	if err != nil {
		return failed(ctx, stepGreeting, to, err)
	}
	// Closing without QUIT: a session that stops here has nothing more to
	// say, least of all in the clear.
	defer c.Close()
	if err := c.Hello(s.localName); err != nil {
		return failed(ctx, stepHello, to, err)
	}
	if st.Encryption == StartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &Failure{Reason: NoEncryption}
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return failed(ctx, stepStartTLS, to, err)
		}
	}
	if st.Username != "" {
		auth, ok := signIn(c, st.Username, st.Password)
		if !ok {
			return &Failure{Reason: SignIn}
		}
		if err := c.Auth(auth); err != nil {
			return failed(ctx, stepAuth, to, err)
		}
	}
	if err := c.Mail(from); err != nil {
		return failed(ctx, stepSender, to, err)
	}
	if err := c.Rcpt(to); err != nil {
		return failed(ctx, stepRecipient, to, err)
	}
	w, err := c.Data()
	if err != nil {
		return failed(ctx, stepData, to, err)
	}
	if _, err := w.Write(compose(m, from, st.FromName, to, s.now(), messageID(from))); err != nil {
		_ = w.Close()
		return failed(ctx, stepData, to, err)
	}
	if err := w.Close(); err != nil {
		return failed(ctx, stepData, to, err)
	}
	// The server took the mail; a QUIT it does not answer changes nothing.
	_ = c.Quit()
	return nil
}

// ─── signing in ──────────────────────────────────────────────────────────────

// signIn picks a way to sign in that the server offers: PLAIN, else LOGIN,
// else CRAM-MD5. false when it offers none of them, or no AUTH at all.
//
// PLAIN and LOGIN are this package's own: net/smtp's PLAIN refuses to send a
// password over a connection that is not encrypted, and with the encryption
// none that is what the admin chose (the mail page warns).
func signIn(c *smtp.Client, user, password string) (smtp.Auth, bool) {
	ok, params := c.Extension("AUTH")
	if !ok {
		return nil, false
	}
	offered := strings.Fields(strings.ToUpper(params))
	switch {
	case slices.Contains(offered, "PLAIN"):
		return plainAuth{user: user, password: password}, true
	case slices.Contains(offered, "LOGIN"):
		return &loginAuth{user: user, password: password}, true
	case slices.Contains(offered, "CRAM-MD5"):
		return smtp.CRAMMD5Auth(user, password), true
	}
	return nil, false
}

// plainAuth is AUTH PLAIN (RFC 4616): the user and the password at once.
type plainAuth struct{ user, password string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.password), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, &Failure{Reason: SignIn}
	}
	return nil, nil
}

// loginAuth is AUTH LOGIN: the user, then the password, each asked for.
type loginAuth struct {
	user, password string
	asked          int
}

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	a.asked = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.asked++
	switch a.asked {
	case 1:
		return []byte(a.user), nil
	case 2:
		return []byte(a.password), nil
	}
	return nil, &Failure{Reason: SignIn}
}

// ─── why a mail was not sent ─────────────────────────────────────────────────

// Reason is why a mail was not sent. Each has its sentence on the mail page.
type Reason string

const (
	// Unreachable: no connection to the server, or it broke.
	Unreachable Reason = "unreachable"
	// NoEncryption: the server does not speak the encryption chosen.
	NoEncryption Reason = "encryption"
	// Certificate: the server's certificate did not pass the check.
	Certificate Reason = "certificate"
	// SignIn: the server refused the user or the password.
	SignIn Reason = "sign-in"
	// SenderRefused: the server refused the sender's address.
	SenderRefused Reason = "sender"
	// RecipientRefused: the server refused the address the mail is for.
	RecipientRefused Reason = "recipient"
	// TimedOut: the server did not answer in time.
	TimedOut Reason = "timeout"
	// Answered: the server refused the mail in its own words.
	Answered Reason = "answered"
)

// Failure is a mail that was not sent, and why.
type Failure struct {
	Reason Reason
	// Address is the address the server refused, for RecipientRefused.
	Address string
	// Words are the server's own, for Answered.
	Words string
}

// Error names the reason and nothing else: no address, no words of the
// server's, which may quote one.
func (f *Failure) Error() string { return "the mail was not sent: " + string(f.Reason) }

// Sentence says why, as the mail page says it.
func (f *Failure) Sentence() string {
	switch f.Reason {
	case Unreachable:
		return "Can't reach the mail server. Check its name and its port."
	case NoEncryption:
		return "The mail server doesn't offer this encryption. Choose another."
	case Certificate:
		return "The mail server's certificate could not be checked."
	case SignIn:
		return "The mail server refused the user or the password."
	case SenderRefused:
		return "The mail server refused the sender's address."
	case RecipientRefused:
		return "The mail server refused the address " + f.Address + "."
	case TimedOut:
		return "The mail server didn't answer in time."
	}
	return "The mail server answered: " + f.Words
}

// Code names the reason as an answer does (ADR-0023, part 4): the case, not
// its sentence.
func (f *Failure) Code() string {
	switch f.Reason {
	case Unreachable:
		return "mail_server_unreachable"
	case NoEncryption:
		return "mail_encryption_not_offered"
	case Certificate:
		return "mail_certificate_unverified"
	case SignIn:
		return "mail_sign_in_refused"
	case SenderRefused:
		return "mail_sender_refused"
	case RecipientRefused:
		return "mail_recipient_refused"
	case TimedOut:
		return "mail_server_timeout"
	}
	return "mail_server_answered"
}

// Values are what the sentence names, numbers and names as they are (ADR-0023,
// part 4): the address the server refused, or the server's own words.
func (f *Failure) Values() map[string]any {
	switch f.Reason {
	case RecipientRefused:
		return map[string]any{"address": f.Address}
	case Answered:
		return map[string]any{"words": f.Words}
	}
	return nil
}

// failed says why a step failed.
func failed(ctx context.Context, at step, to string, err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	if timedOut(ctx, err) {
		return &Failure{Reason: TimedOut}
	}
	if certificateRefused(err) {
		return &Failure{Reason: Certificate}
	}
	var reply *textproto.Error
	if errors.As(err, &reply) {
		switch {
		case at == stepStartTLS:
			return &Failure{Reason: NoEncryption}
		case at == stepAuth && reply.Code >= 500:
			return &Failure{Reason: SignIn}
		case at == stepSender && addressRefused(reply.Code):
			return &Failure{Reason: SenderRefused}
		case at == stepRecipient && addressRefused(reply.Code):
			return &Failure{Reason: RecipientRefused, Address: to}
		}
		return &Failure{Reason: Answered, Words: serverWords(reply)}
	}
	if at == stepTLS || at == stepStartTLS {
		// A handshake that failed for another reason than the certificate:
		// the server speaks no TLS there, or none this client takes.
		return &Failure{Reason: NoEncryption}
	}
	return &Failure{Reason: Unreachable}
}

// addressRefused: the codes a server refuses an address with — syntax (501),
// no such mailbox (550, 551) or a name it does not take (553).
func addressRefused(code int) bool {
	return code == 501 || code == 550 || code == 551 || code == 553
}

func timedOut(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func certificateRefused(err error) bool {
	var (
		verification *tls.CertificateVerificationError
		authority    x509.UnknownAuthorityError
		hostname     x509.HostnameError
		invalid      x509.CertificateInvalidError
	)
	return errors.As(err, &verification) || errors.As(err, &authority) ||
		errors.As(err, &hostname) || errors.As(err, &invalid)
}

// maxServerWords bounds the server's words in a sentence.
const maxServerWords = 300

// serverWords are a refusal's text on one line, without a control character
// — its code when it has none — cut to maxServerWords characters.
func serverWords(reply *textproto.Error) string {
	text := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(reply.Msg, ""))
	words := strings.Join(strings.Fields(text), " ")
	if words == "" {
		return strconv.Itoa(reply.Code)
	}
	if utf8.RuneCountInString(words) > maxServerWords {
		words = string([]rune(words)[:maxServerWords]) + "…"
	}
	return words
}
