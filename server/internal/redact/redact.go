// Package redact removes credential-shaped values from text so none of the admin
// debug surfaces — the live log viewer, the Kafka event tap, the curated DB
// browser, or the support-bundle export — ever surface passwords/tokens.
//
// Best-effort by design: it targets the common credential shapes rather than
// claiming to catch everything, and is layered on top of admin-only access — not
// instead of it. One implementation, reused everywhere, so every surface redacts
// identically.
package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// secretWord is what names a credential in a key: the one list both the text
// rule (Secrets) and the document rule (Document) read keys by.
const secretWord = `(?:password|passwd|secret|token|apikey|api_key|access[_-]?key|private[_-]?key|client[_-]?secret|credential)`

// Marker is what a redacted value is replaced with.
const Marker = "***REDACTED***"

var (
	// key: value / key=value / "key":"value" where the KEY looks like a credential.
	// The unquoted value runs to whitespace/quote/brace (NOT comma — a secret may
	// legitimately contain commas, so stopping at the first ',' leaked the tail).
	reSecretKV = regexp.MustCompile(`(?i)([a-z0-9_.-]*` + secretWord + `[a-z0-9_.-]*)("?\s*[:=]\s*"?)([^\s"'}]+)`)
	// A field name that names a credential: the key half of reSecretKV, whole.
	reSecretField = regexp.MustCompile(`(?i)^[a-z0-9_.-]*` + secretWord + `[a-z0-9_.-]*$`)
	// The value of an Authorization / Proxy-Authorization header, ANY scheme — the
	// scheme keyword is kept, the credential is redacted. Basic <base64> decodes
	// straight to user:password, so it must never survive.
	reAuthHeader = regexp.MustCompile(`(?i)((?:proxy-)?authorization"?\s*[:=]\s*"?(?:bearer|basic|negotiate|digest)\s+)[^\s"']+`)
	// Bare "bearer <token>" / "basic <base64>" not inside an Authorization header.
	// Require ≥16 token chars so ordinary prose ("bearer verification active",
	// "basic authentication") isn't redacted while real tokens (JWTs, opaque tokens,
	// base64 of user:pass ≥16) still are.
	reBearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{16,}`)
	reBasic  = regexp.MustCompile(`(?i)(basic\s+)[A-Za-z0-9+/=_-]{16,}`)
	// A JWT — three base64url segments.
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)
	// A URI with inline credentials (scheme://[user]:pass@host). Username optional
	// (redis://:pass@ is Redis's canonical form); password matched greedily up to
	// the LAST '@' before the host so a '@' inside the password can't leave a tail.
	reURICreds = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|redis|amqp|mongodb|https?)://[^:@/\s]*:)[^\s"']+(@[^@/\s]+)`)
)

// Secrets returns s with credential-shaped values replaced by redaction markers.
func Secrets(s string) string {
	s = reAuthHeader.ReplaceAllString(s, `${1}***REDACTED***`)
	s = reBearer.ReplaceAllString(s, `${1}***REDACTED***`)
	s = reBasic.ReplaceAllString(s, `${1}***REDACTED***`)
	s = reJWT.ReplaceAllString(s, `***REDACTED-JWT***`)
	s = reSecretKV.ReplaceAllString(s, `${1}${2}***REDACTED***`)
	s = reURICreds.ReplaceAllString(s, `${1}***REDACTED***${2}`)
	return s
}

// SecretField reports whether a field name says its value is a credential —
// by the rule Secrets reads the key of a key=value pair with.
func SecretField(name string) bool { return reSecretField.MatchString(name) }

// Document redacts a decoded JSON document — what encoding/json decodes into
// an any: objects, arrays, strings, numbers, booleans and nulls — in place,
// and returns it.
//
// It is how a JSON document is redacted, rather than running Secrets over its
// encoded text: that rule reads text, and an encoded document is not the text
// its strings hold. Over `"note": "token=\"s3cr3t\""` it takes the backslash
// for the value, leaves the secret and unbalances the quotes; under a
// credential-named field it replaces the '[' of an array or a bare number with
// a marker that is no JSON. Here every value under a field whose name names a
// credential is replaced, a nested one as much as a flat one — each string
// and number in it becomes Marker, so an array or an object keeps its shape
// and loses its contents — and every other string is scrubbed with Secrets.
// Field names stay; a boolean or a null says nothing a credential is, nor does
// an empty string.
func Document(v any) any { return document(v, false) }

func document(v any, secret bool) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = document(val, secret || SecretField(k))
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = document(val, secret)
		}
		return x
	case string:
		switch {
		case secret && x != "":
			return Marker
		case secret:
			return x
		}
		return Secrets(x)
	case json.Number, float64, int, int64:
		if secret {
			return Marker
		}
		return x
	}
	return v
}

// JSON is v redacted as a JSON document: encoded, decoded with its numbers
// kept exactly as they were written, and redacted by Document.
func JSON(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return DecodeJSON(raw)
}

var errTrailing = errors.New("more than one JSON value")

// DecodeJSON decodes raw — one JSON value, nothing after it — and redacts it
// by Document. Numbers keep the digits they were written with.
func DecodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errTrailing
	}
	return Document(doc), nil
}

var secretKeyParts = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key",
	"accesskey", "access_key", "privatekey", "private_key",
	"clientsecret", "client_secret", "credential",
}

// LooksSecretKey reports whether a column/field name looks like it holds a
// credential, so the DB browser can mask the whole value rather than print it.
func LooksSecretKey(name string) bool {
	n := strings.ToLower(name)
	for _, p := range secretKeyParts {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}
