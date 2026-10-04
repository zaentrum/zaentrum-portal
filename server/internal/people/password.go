package people

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak"
)

// The password an invited person chooses. Keycloak decides — its realm's
// password policy is the rule — and portal-api says beforehand what it can:
// the policy the platform gave the realm (PORTAL_PASSWORD_POLICY, the realm
// import's), read for the invite page's hint and checked before Keycloak is
// asked, so that the common refusals cost no round trip. A policy Keycloak
// holds beyond it still decides, and its refusal is worded the same way.

// MaxPassword bounds a password: more is no password a person types.
const MaxPassword = 256

// Policy is a Keycloak password policy as portal-api reads it.
type Policy struct {
	MinLength   int  `json:"minLength"`
	MaxLength   int  `json:"maxLength,omitempty"`
	Digits      int  `json:"digits,omitempty"`
	Lower       int  `json:"lowerCase,omitempty"`
	Upper       int  `json:"upperCase,omitempty"`
	Special     int  `json:"specialChars,omitempty"`
	NotUsername bool `json:"notUsername,omitempty"`
	NotEmail    bool `json:"notEmail,omitempty"`
	// Hints are the rules, one short sentence each, for the invite page.
	Hints []string `json:"hints"`
}

// ParsePolicy reads Keycloak's policy syntax, "length(8) and notUsername":
// the rules it knows; one it does not is Keycloak's alone. Empty is
// Keycloak's default minimum of 8.
func ParsePolicy(s string) Policy {
	p := Policy{MinLength: 8}
	for _, part := range strings.Split(s, " and ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, arg := part, ""
		if i := strings.IndexByte(part, '('); i > 0 && strings.HasSuffix(part, ")") {
			name, arg = part[:i], strings.TrimSpace(part[i+1:len(part)-1])
		}
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 {
			n = 1
		}
		switch name {
		case "length":
			p.MinLength = n
		case "maxLength":
			p.MaxLength = n
		case "digits":
			p.Digits = n
		case "lowerCase":
			p.Lower = n
		case "upperCase":
			p.Upper = n
		case "specialChars":
			p.Special = n
		case "notUsername":
			p.NotUsername = true
		case "notEmail":
			p.NotEmail = true
		}
	}
	plural := func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return strconv.Itoa(n) + " " + many
	}
	p.Hints = append(p.Hints, fmt.Sprintf("At least %d characters.", p.MinLength))
	if p.Digits > 0 {
		p.Hints = append(p.Hints, "At least "+plural(p.Digits, "one digit", "digits")+".")
	}
	if p.Upper > 0 {
		p.Hints = append(p.Hints, "At least "+plural(p.Upper, "one capital letter", "capital letters")+".")
	}
	if p.Lower > 0 {
		p.Hints = append(p.Hints, "At least "+plural(p.Lower, "one small letter", "small letters")+".")
	}
	if p.Special > 0 {
		p.Hints = append(p.Hints, "At least "+plural(p.Special, "one symbol", "symbols")+", such as ! or #.")
	}
	if p.NotUsername {
		p.Hints = append(p.Hints, "Not your username.")
	}
	return p
}

// PasswordRefused is a password Keycloak, or the policy before it, does not
// take, in words for the person choosing it.
type PasswordRefused struct{ Message string }

func (e *PasswordRefused) Error() string { return e.Message }

// Check says why p refuses password for the person, or nil.
func (p Policy) Check(password string, person Person) error {
	n := utf8.RuneCountInString(password)
	var digits, lower, upper, special int
	for _, r := range password {
		switch {
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLower(r):
			lower++
		case unicode.IsUpper(r):
			upper++
		case !unicode.IsLetter(r) && !unicode.IsSpace(r):
			special++
		}
	}
	refuse := func(format string, args ...any) error {
		return &PasswordRefused{Message: fmt.Sprintf(format, args...)}
	}
	switch {
	case password == "":
		return refuse("Choose a password.")
	case len(password) > MaxPassword:
		return refuse("A password has at most %d characters.", MaxPassword)
	case n < p.MinLength:
		return refuse("The password needs at least %d characters.", p.MinLength)
	case p.MaxLength > 0 && n > p.MaxLength:
		return refuse("The password may have at most %d characters.", p.MaxLength)
	case digits < p.Digits:
		return refuse("The password needs at least %d digit(s).", p.Digits)
	case upper < p.Upper:
		return refuse("The password needs at least %d capital letter(s).", p.Upper)
	case lower < p.Lower:
		return refuse("The password needs at least %d small letter(s).", p.Lower)
	case special < p.Special:
		return refuse("The password needs at least %d symbol(s).", p.Special)
	case p.NotUsername && strings.EqualFold(password, person.Username):
		return refuse("The password may not be your username.")
	case p.NotEmail && person.Email != "" && strings.EqualFold(password, person.Email):
		return refuse("The password may not be your email address.")
	}
	return nil
}

// PasswordError words Keycloak's refusal of a password: its policy's rule,
// with the rule's number. Anything else is not a refusal of the password.
func PasswordError(err error) error {
	var inv *keycloak.Invalid
	if !errors.As(err, &inv) || inv.Field != "password" {
		return err
	}
	first := func() string {
		if len(inv.Params) > 0 {
			return inv.Params[0]
		}
		// Keycloak puts the number in its description: "minimum length 8."
		f := strings.Fields(strings.TrimSuffix(inv.Description, "."))
		for i := len(f) - 1; i >= 0; i-- {
			if _, err := strconv.Atoi(f[i]); err == nil {
				return f[i]
			}
		}
		return "more"
	}
	words := map[string]string{
		"invalidPasswordMinLengthMessage":           "The password needs at least %s characters.",
		"invalidPasswordMaxLengthMessage":           "The password may have at most %s characters.",
		"invalidPasswordMinDigitsMessage":           "The password needs at least %s digit(s).",
		"invalidPasswordMinLowerCaseCharsMessage":   "The password needs at least %s small letter(s).",
		"invalidPasswordMinUpperCaseCharsMessage":   "The password needs at least %s capital letter(s).",
		"invalidPasswordMinSpecialCharsMessage":     "The password needs at least %s symbol(s).",
		"invalidPasswordHistoryMessage":             "The password must differ from the last %s used.",
		"invalidPasswordNotUsernameMessage":         "The password may not be your username.",
		"invalidPasswordNotContainsUsernameMessage": "The password may not contain your username.",
		"invalidPasswordNotEmailMessage":            "The password may not be your email address.",
		"invalidPasswordBlacklistedMessage":         "That password is too common: choose another.",
		"invalidPasswordRegexPatternMessage":        "The password does not have the form this server asks for.",
	}
	if w, ok := words[inv.Code]; ok {
		if strings.Contains(w, "%s") {
			return &PasswordRefused{Message: fmt.Sprintf(w, first())}
		}
		return &PasswordRefused{Message: w}
	}
	return &PasswordRefused{Message: "This server's password policy does not take that password: choose another."}
}
