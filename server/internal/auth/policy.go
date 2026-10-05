package auth

import (
	"regexp"
	"strings"
)

// Policy decides what a verified token may do. A realm signs tokens for every
// client it serves — the portal, the media apps, the TV and phone clients, the
// CLI, every service account — and portal-api trusts all of them to say who a
// person is. Which of them may administer the platform is a narrower question,
// and the issuer alone does not answer it: a viewer's media token is as valid
// as an admin's portal token, and an admin who signed in to the media app
// carries the admin role in a token issued to it.
//
// So the admin role counts only on a token issued to one of the portal's own
// clients — its browser client and the CLI's (AdminClients). Every other token
// signs a person in for the reads anyone signed in may make: the launchpad,
// /me, the slot rows a product app reads with its user's token.
//
// An addon's service account is the one other writer: it may change the slot
// rows of its own addon and post notices as that addon, and nothing else. Its
// addon is its client id — the confidential client is named after the addon
// — or, where a shared realm names clients per instance, the zaentrum_addon
// claim a hardcoded-claim mapper on that client sets.
type Policy struct {
	AdminRole string
	AddonRole string
	// AdminClients are the client ids whose tokens carry the admin role into
	// admin requests (PORTAL_ADMIN_CLIENTS).
	AdminClients []string
}

// dnsLabel is what an addon key is: one RFC 1123 label. It keeps a key from
// reaching into another addon's row namespace, <key>.<row>.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// grant decides Admin and Addon for a verified principal.
func (p Policy) grant(pr *Principal) {
	pr.Admin = pr.HasRole(p.AdminRole) && (pr.anonymous || p.adminClient(pr.Client))
	pr.Addon = p.addonKey(pr)
}

// adminClient reports whether a token issued to client counts for admin
// requests.
func (p Policy) adminClient(client string) bool {
	if client == "" {
		return false
	}
	for _, c := range p.AdminClients {
		if c == client {
			return true
		}
	}
	return false
}

// addonKey is the addon whose rows an addon service account writes, or "".
//
// The addon role alone does not make one: the role says "an addon", the
// client says which. A person who was given the role by mistake — the realm's
// own description says never to — signs in through a person's client, and a
// service account it is not, so the role buys them nothing here.
func (p Policy) addonKey(pr *Principal) string {
	// The synthetic dev principal names no client, so it is never one.
	if p.AddonRole == "" || !pr.HasRole(p.AddonRole) || !serviceAccount(pr) {
		return ""
	}
	key := pr.addonClaim
	if key == "" {
		key = pr.Client
	}
	if len(key) > 63 || !dnsLabel.MatchString(key) {
		return ""
	}
	return key
}

// IsServiceAccount reports whether the principal is a client's service
// account rather than a person (serviceAccount).
func IsServiceAccount(pr *Principal) bool { return pr != nil && serviceAccount(pr) }

// serviceAccount reports whether a token is a client's own, from the client
// credentials grant: Keycloak names a client's service-account user
// service-account-<client id>; other providers make the client its subject.
func serviceAccount(pr *Principal) bool {
	if pr.Client == "" {
		return false
	}
	return strings.EqualFold(pr.Username, "service-account-"+pr.Client) || pr.Subject == pr.Client
}
