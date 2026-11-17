// Package mailtest is an SMTP server for tests: a goroutine on 127.0.0.1
// that speaks as much of SMTP as portal-api's mail client uses — EHLO,
// STARTTLS, AUTH PLAIN and LOGIN, MAIL, RCPT, DATA and QUIT — with a
// certificate of its own, and records every command it was sent and whether
// the connection was encrypted when the command came.
package mailtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options say how the server behaves. The zero value greets, takes EHLO,
// MAIL, RCPT and DATA, and offers neither STARTTLS nor AUTH.
type Options struct {
	// TLS: the connection is encrypted from the start.
	TLS bool
	// StartTLS: EHLO offers STARTTLS.
	StartTLS bool
	// Auth are the mechanisms EHLO offers: PLAIN, LOGIN. With StartTLS they
	// are offered once the connection is encrypted, as most servers do.
	Auth []string
	// User and Password are the sign-in the server takes.
	User, Password string
	// Greeting replaces the greeting ("220 mailtest ESMTP").
	Greeting string
	// Replies replace the reply to a command, by its verb ("RCPT": "550 5.1.1
	// no such user"); "." replaces the reply to the end of a mail's data.
	Replies map[string]string
	// Silent: the server takes the connection and never says a word.
	Silent bool
}

// Command is one line a client sent.
type Command struct {
	// Verb is the command, in upper case: EHLO, STARTTLS, AUTH, MAIL…
	Verb string
	// Arg is the rest of the line. For AUTH it is the mechanism alone: the
	// credentials are not kept.
	Arg string
	// TLS: the connection was encrypted when the line came.
	TLS bool
}

// Mail is a mail the server took.
type Mail struct {
	From string   // the address of MAIL FROM
	To   []string // the addresses of RCPT TO
	Data []byte   // what DATA carried, dot-unstuffed, with \n line ends
	TLS  bool     // the connection was encrypted
	User string   // whom the client signed in as; "" for nobody
}

// Server is an SMTP server on 127.0.0.1, for the test that started it.
type Server struct {
	Host string
	Port int
	// CA trusts the server's certificate. A client given it passes the
	// check; one with the system's authorities does not.
	CA *x509.CertPool

	o   Options
	ln  net.Listener
	tls *tls.Config

	mu       sync.Mutex
	sessions [][]Command
	mails    []Mail
	conns    []net.Conn
	wg       sync.WaitGroup
}

// Start starts a server that stops with the test.
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	cert, pool := certificate(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mailtest: %v", err)
	}
	s := &Server{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, CA: pool, o: o, ln: ln,
		tls: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

// ClosedPort is a port on 127.0.0.1 that nothing listens on.
func ClosedPort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mailtest: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// Sessions are the commands of every connection, in the order they came.
func (s *Server) Sessions() [][]Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]Command, len(s.sessions))
	for i, cmds := range s.sessions {
		out[i] = slices.Clone(cmds)
	}
	return out
}

// Mails are the mails the server took.
func (s *Server) Mails() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.mails)
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.sessions = append(s.sessions, nil)
		session := len(s.sessions) - 1
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.serve(conn, session)
		}()
	}
}

func (s *Server) record(session int, c Command) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session] = append(s.sessions[session], c)
}

func (s *Server) reply(verb string) (string, bool) {
	r, ok := s.o.Replies[verb]
	return r, ok
}

func (s *Server) serve(conn net.Conn, session int) {
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	if s.o.Silent {
		_, _ = io.Copy(io.Discard, conn) // until the client gives up
		return
	}
	encrypted := false
	if s.o.TLS {
		tc := tls.Server(conn, s.tls)
		if tc.Handshake() != nil {
			return
		}
		conn, encrypted = tc, true
	}
	tp := textproto.NewConn(conn)
	greeting := s.o.Greeting
	if greeting == "" {
		greeting = "220 mailtest ESMTP"
	}
	if tp.PrintfLine("%s", greeting) != nil || !strings.HasPrefix(greeting, "220") {
		return
	}
	var m *Mail
	user := ""
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		cmd := Command{Verb: verb, Arg: arg, TLS: encrypted}
		if verb == "AUTH" {
			cmd.Arg, _, _ = strings.Cut(arg, " ")
		}
		s.record(session, cmd)
		if r, ok := s.reply(verb); ok {
			_ = tp.PrintfLine("%s", r)
			if verb == "QUIT" {
				return
			}
			continue
		}
		switch verb {
		case "EHLO":
			lines := []string{"mailtest"}
			if s.o.StartTLS && !encrypted {
				lines = append(lines, "STARTTLS")
			}
			if len(s.o.Auth) > 0 && (encrypted || !s.o.StartTLS) {
				lines = append(lines, "AUTH "+strings.Join(s.o.Auth, " "))
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, l)
			}
		case "HELO":
			_ = tp.PrintfLine("250 mailtest")
		case "STARTTLS":
			if !s.o.StartTLS || encrypted {
				_ = tp.PrintfLine("502 5.5.1 STARTTLS is not offered")
				continue
			}
			_ = tp.PrintfLine("220 2.0.0 ready to start TLS")
			tc := tls.Server(conn, s.tls)
			if tc.Handshake() != nil {
				return
			}
			conn, encrypted = tc, true
			tp = textproto.NewConn(conn)
			// What was said before is forgotten (RFC 3207).
			m, user = nil, ""
		case "AUTH":
			who, ok := s.auth(tp, arg)
			if !ok {
				_ = tp.PrintfLine("535 5.7.8 authentication credentials invalid")
				continue
			}
			user = who
			_ = tp.PrintfLine("235 2.7.0 authentication successful")
		case "MAIL":
			m = &Mail{From: path(arg, "FROM:"), TLS: encrypted, User: user}
			_ = tp.PrintfLine("250 2.1.0 sender ok")
		case "RCPT":
			if m == nil {
				_ = tp.PrintfLine("503 5.5.1 MAIL first")
				continue
			}
			m.To = append(m.To, path(arg, "TO:"))
			_ = tp.PrintfLine("250 2.1.5 recipient ok")
		case "DATA":
			if m == nil || len(m.To) == 0 {
				_ = tp.PrintfLine("503 5.5.1 RCPT first")
				continue
			}
			_ = tp.PrintfLine("354 end data with <CR><LF>.<CR><LF>")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			if r, ok := s.reply("."); ok {
				_ = tp.PrintfLine("%s", r)
				m = nil
				continue
			}
			m.Data = data
			s.mu.Lock()
			s.mails = append(s.mails, *m)
			s.mu.Unlock()
			m = nil
			_ = tp.PrintfLine("250 2.0.0 queued")
		case "RSET", "NOOP":
			_ = tp.PrintfLine("250 2.0.0 ok")
		case "QUIT":
			_ = tp.PrintfLine("221 2.0.0 bye")
			return
		default:
			_ = tp.PrintfLine("502 5.5.2 command not recognised")
		}
	}
}

// auth takes AUTH PLAIN or LOGIN of a mechanism the server offers, and
// answers whom it signed in and whether the sign-in is the one it takes.
func (s *Server) auth(tp *textproto.Conn, arg string) (string, bool) {
	mech, initial, _ := strings.Cut(arg, " ")
	mech = strings.ToUpper(mech)
	if !slices.Contains(s.o.Auth, mech) {
		return "", false
	}
	ask := func(prompt string) (string, bool) {
		if tp.PrintfLine("334 %s", base64.StdEncoding.EncodeToString([]byte(prompt))) != nil {
			return "", false
		}
		line, err := tp.ReadLine()
		if err != nil {
			return "", false
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		return string(raw), err == nil
	}
	var user, password string
	switch mech {
	case "PLAIN":
		var response string
		if initial != "" {
			raw, err := base64.StdEncoding.DecodeString(initial)
			if err != nil {
				return "", false
			}
			response = string(raw)
		} else {
			var ok bool
			if response, ok = ask(""); !ok {
				return "", false
			}
		}
		parts := strings.Split(response, "\x00")
		if len(parts) != 3 {
			return "", false
		}
		user, password = parts[1], parts[2]
	case "LOGIN":
		var ok bool
		if user, ok = ask("Username:"); !ok {
			return "", false
		}
		if password, ok = ask("Password:"); !ok {
			return "", false
		}
	}
	return user, s.o.User != "" && user == s.o.User && password == s.o.Password
}

// path is the address of "FROM:<a@example.org> …".
func path(arg, prefix string) string {
	arg = strings.TrimSpace(arg)
	if len(arg) >= len(prefix) && strings.EqualFold(arg[:len(prefix)], prefix) {
		arg = arg[len(prefix):]
	}
	arg, _, _ = strings.Cut(strings.TrimSpace(arg), " ")
	return strings.TrimSuffix(strings.TrimPrefix(arg, "<"), ">")
}

// certificate is a certificate of the server's own for 127.0.0.1 and
// localhost, and a pool that trusts it.
func certificate(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mailtest: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mailtest"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("mailtest: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("mailtest: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}
