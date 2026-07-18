// Package redact removes credential-shaped values from text so none of the admin
// debug surfaces — the live log viewer, the Kafka event tap, the curated DB
// browser, or the support-bundle export — ever surface passwords/tokens.
//
// Best-effort by design: it targets the common credential shapes rather than
// claiming to catch everything, and is layered on top of admin-only access — not
// instead of it. One implementation, reused everywhere, so every surface redacts
// identically.
package redact

import "regexp"

var (
	// key: value / key=value / "key":"value" where the KEY looks like a credential.
	reSecretKV = regexp.MustCompile(`(?i)([a-z0-9_.-]*(?:password|passwd|secret|token|apikey|api_key|access[_-]?key|private[_-]?key|client[_-]?secret|credential)[a-z0-9_.-]*)("?\s*[:=]\s*"?)([^\s"',}]+)`)
	// Authorization: Bearer <token> and bare "bearer <token>".
	reBearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// A JWT — three base64url segments.
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)
	// A URI with inline credentials (postgres/redis/amqp/mongodb://user:pass@host).
	reURICreds = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|redis|amqp|mongodb|https?)://[^:@/\s]+:)[^@/\s]+(@)`)
)

// Secrets returns s with credential-shaped values replaced by redaction markers.
func Secrets(s string) string {
	s = reSecretKV.ReplaceAllString(s, `${1}${2}***REDACTED***`)
	s = reBearer.ReplaceAllString(s, `${1}***REDACTED***`)
	s = reJWT.ReplaceAllString(s, `***REDACTED-JWT***`)
	s = reURICreds.ReplaceAllString(s, `${1}***REDACTED***${2}`)
	return s
}
