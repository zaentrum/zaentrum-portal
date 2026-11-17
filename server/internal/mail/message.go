package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"mime/quotedprintable"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The kinds of mail, as the log names them. The log says that a mail of a
// kind was sent or refused — never an address, never the text.
const (
	KindTest = "test mail"
)

// Message is one mail to one address, in plain text.
type Message struct {
	// Kind names the mail in the log.
	Kind    string
	To      string
	Subject string
	Text    string
}

// TestMail is the mail the mail page sends to check that mail works: from
// the platform at host, to the address the admin names.
func TestMail(host, to string) Message {
	return Message{
		Kind:    KindTest,
		To:      to,
		Subject: "Test mail from " + host,
		Text:    "This mail was sent from the mail page of " + host + ", to check that mail works. Nothing else needs doing.\n",
	}
}

// ─── addresses ───────────────────────────────────────────────────────────────

// MaxAddress is the most characters an address has: the longest path SMTP
// carries (RFC 5321), less its angle brackets.
const MaxAddress = 254

// Why a text is not an address mail is sent to.
var (
	// ErrNotAnAddress: the text is not one bare address.
	ErrNotAnAddress = errors.New("not an e-mail address")
	// ErrAddressLetters: the text is an address with accented or non-Latin
	// letters, which not every mail server carries.
	ErrAddressLetters = errors.New("an address with accented or non-Latin letters")
)

// ReadAddress reads one e-mail address as it is typed, trimmed. An address
// is one bare address of at most MaxAddress characters: a local part of
// RFC 5322's atoms between single dots — no name, no angle brackets, no line
// break, no quoted part — an @, and a domain that is a host name of at least
// two parts, no IP address. Its letters are ASCII: one that would be an
// address but for accented or non-Latin letters is ErrAddressLetters, and
// anything else that is no address ErrNotAnAddress.
func ReadAddress(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxAddress || strings.Count(s, "@") != 1 {
		return "", ErrNotAnAddress
	}
	local, domain, _ := strings.Cut(s, "@")
	if !dotAtom(local) || !hostName(domain) {
		return "", ErrNotAnAddress
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return "", ErrAddressLetters
		}
	}
	return s, nil
}

// Address is the address ReadAddress reads, and whether raw is one.
func Address(raw string) (string, bool) {
	a, err := ReadAddress(raw)
	return a, err == nil
}

// otherLetter: a letter, a mark or a digit beyond ASCII. An address with one
// is told apart from no address at all: it is refused for its letters.
func otherLetter(r rune) bool {
	return r >= utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r))
}

// dotAtom: atoms of RFC 5322's atext between single dots.
func dotAtom(s string) bool {
	if s == "" {
		return false
	}
	for _, atom := range strings.Split(s, ".") {
		if atom == "" {
			return false
		}
		for _, r := range atom {
			if !(asciiLetterOrDigit(r) || r < utf8.RuneSelf && strings.ContainsRune("!#$%&'*+-/=?^_`{|}~", r) || otherLetter(r)) {
				return false
			}
		}
	}
	return true
}

// hostName: at least two labels of letters, digits and inner hyphens, 1 to
// 63 characters each and 253 in all, the last not all digits — an IP address
// is no host name.
func hostName(s string) bool {
	labels := strings.Split(s, ".")
	if len(labels) < 2 || utf8.RuneCountInString(s) > 253 {
		return false
	}
	for _, l := range labels {
		if n := utf8.RuneCountInString(l); n == 0 || n > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(asciiLetterOrDigit(r) || r == '-' || otherLetter(r)) {
				return false
			}
		}
	}
	return strings.Trim(labels[len(labels)-1], "0123456789") != ""
}

func asciiLetterOrDigit(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// ─── the mail as it is sent ──────────────────────────────────────────────────

// compose writes a mail as it is sent: the headers, then the text in UTF-8,
// quoted-printable, so that a server that takes 7 bits only carries it whole.
// It carries no picture and nothing that reports whether it was opened.
//
// A header is ASCII: a sender's name or a subject that is not printable ASCII
// goes in RFC 2047's encoded words, and a header is folded between its words,
// so that no line is longer than 78 characters where folding can keep it so
// — an address cannot be folded — and none is longer than 998 (RFC 5322).
func compose(m Message, from, fromName, to string, now time.Time, id string) []byte {
	var b bytes.Buffer
	writeHeader(&b, "From", append(phraseWords(fromName, foldAt-len("From: ")), "<"+from+">"))
	writeHeader(&b, "To", []string{"<" + to + ">"})
	writeHeader(&b, "Subject", textWords(m.Subject, foldAt-len("Subject: ")))
	writeHeader(&b, "Date", []string{now.UTC().Format(time.RFC1123Z)})
	writeHeader(&b, "Message-ID", []string{"<" + id + ">"})
	writeHeader(&b, "Auto-Submitted", []string{"auto-generated"})
	// What makes it plain text in UTF-8.
	writeHeader(&b, "MIME-Version", []string{"1.0"})
	writeHeader(&b, "Content-Type", []string{"text/plain; charset=utf-8"})
	writeHeader(&b, "Content-Transfer-Encoding", []string{"quoted-printable"})
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(m.Text))
	_ = qp.Close()
	return b.Bytes()
}

// The lengths of a header's lines (RFC 5322, 2.1.1): at most foldAt
// characters where folding can keep a line so short, never more than
// lineMax. An encoded word has at most wordMax (RFC 2047).
const (
	foldAt  = 78
	lineMax = 998
	wordMax = 75
)

// writeHeader writes a header field, its words joined by single spaces and
// folded — a line break before the space — before a word that would make its
// line longer than foldAt. A word is never split.
func writeHeader(b *bytes.Buffer, name string, words []string) {
	b.WriteString(name)
	b.WriteString(":")
	line := len(name) + 1
	for i, w := range words {
		if i > 0 && line+1+len(w) > foldAt {
			b.WriteString("\r\n")
			line = 0
		}
		b.WriteString(" ")
		b.WriteString(w)
		line += 1 + len(w)
	}
	b.WriteString("\r\n")
}

// textWords are a subject's words as its header carries them, the first in
// first characters at most: as typed where the subject is printable ASCII
// between single spaces, with no word too long for a line and nothing a
// reader would take for an encoded word; else encoded words (Q).
func textWords(s string, first int) []string {
	if plain(s) {
		words := strings.Split(s, " ")
		fits := len(words[0]) <= first
		for _, w := range words[1:] {
			fits = fits && len(w) < foldAt
		}
		if fits {
			return words
		}
	}
	return encodedWords(s, false, first)
}

// phraseWords are a sender's name as the From header carries it, before the
// address: a quoted string where the name is printable ASCII, holds nothing a
// reader would take for an encoded word and fits in first characters, else
// encoded words (B, which a name may carry whatever its characters, as Q may
// not).
func phraseWords(name string, first int) []string {
	if name == "" {
		return nil
	}
	if printable(name) && !strings.Contains(name, "=?") {
		quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
		if len(quoted) <= first {
			return []string{quoted}
		}
	}
	return encodedWords(name, true, first)
}

// printable: s is printable ASCII.
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// plain: s can stand in a header as it is — printable ASCII, words between
// single spaces, none of RFC 2047's "=?" that would start an encoded word.
func plain(s string) bool {
	return s != "" && printable(s) && !strings.Contains(s, "=?") && !strings.Contains(s, "  ") &&
		!strings.HasPrefix(s, " ") && !strings.HasSuffix(s, " ")
}

// encodedWords writes s as RFC 2047 encoded words of UTF-8, B (base64) or Q,
// the first at most first characters long and every other wordMax, so that
// each has a line of its own when the header is folded. A character is never
// split across two words.
func encodedWords(s string, b bool, first int) []string {
	s = strings.ToValidUTF8(s, "�")
	prefix := "=?utf-8?q?"
	encode, size := qEncode, qSize
	if b {
		prefix = "=?utf-8?b?"
		encode = func(raw string) string { return base64.StdEncoding.EncodeToString([]byte(raw)) }
		size = func(raw string) int { return base64.StdEncoding.EncodedLen(len(raw)) }
	}
	room := min(first, wordMax) - len(prefix) - len("?=")
	var words []string
	start := 0
	for i, r := range s {
		next := i + utf8.RuneLen(r)
		if i > start && size(s[start:next]) > room {
			words = append(words, prefix+encode(s[start:i])+"?=")
			start, room = i, wordMax-len(prefix)-len("?=")
		}
	}
	if start < len(s) {
		words = append(words, prefix+encode(s[start:])+"?=")
	}
	return words
}

// qEncode is RFC 2047's Q encoding: printable ASCII as it is but "=", "?"
// and "_", a space as "_", any other byte as =XX.
func qEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == ' ':
			b.WriteByte('_')
		case c > ' ' && c <= '~' && c != '=' && c != '?' && c != '_':
			b.WriteByte(c)
		default:
			b.WriteByte('=')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// qSize is the length of qEncode(s).
func qSize(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= ' ' && c <= '~' && c != '=' && c != '?' && c != '_' {
			n++
		} else {
			n += 3
		}
	}
	return n
}

// messageID is a new Message-ID: 16 random bytes at the sender's domain, or
// at localhost when the domain is not plain ASCII.
func messageID(from string) string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	domain := "localhost"
	if i := strings.LastIndexByte(from, '@'); i >= 0 && asciiDomain(from[i+1:]) {
		domain = from[i+1:]
	}
	return hex.EncodeToString(raw) + "@" + domain
}

func asciiDomain(d string) bool {
	if d == "" {
		return false
	}
	for _, r := range d {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}
