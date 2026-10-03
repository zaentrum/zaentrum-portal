package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/zaentrum/zaentrum-portal/server/internal/auth"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// Slot rows are rendered by product apps as native buttons, inside their own
// page: a link is an <a href>, an action is a fetch that carries the signed-in
// user's bearer. Whatever a row names, a product app goes there with its
// user. So a row's destination is checked where it is written — on install
// from a manifest and in the write API — by one rule:
//
//   - kind is link or action; an action is a POST, and a link has no method
//     (POST is stored for both, as before);
//   - url is a path on this instance (/portal/app/<addon>?q={q}), or an
//     absolute http(s) URL on the instance's own origin. Never another host,
//     never javascript:, data:, file: or any other scheme, never
//     protocol-relative, and no dot segments that climb out of where the
//     path says it goes;
//   - status_url, when set, obeys the same rule;
//   - an action an addon contributes — from its manifest, or written by its
//     service account — goes to that addon's own API, through the portal's
//     proxy (/api/portal/apps/<addon>/…): it carries the user's token, and
//     an addon has no business spending it elsewhere.
//
// Rows written before these rules are filtered on the way out (servable): a
// row whose scheme, kind or method could never have passed is not served.

// slotMethod is the one method a slot action is sent with.
const slotMethod = http.MethodPost

// slotURLInfo is a slot URL as written: trimmed, and the origin it names
// when it is absolute ("" for a path).
type slotURLInfo struct {
	url    string
	origin string
}

// parseSlotURL checks the shape of a slot URL — everything but which origin
// an absolute one names.
func parseSlotURL(raw string) (slotURLInfo, error) {
	u := strings.TrimSpace(raw)
	switch {
	case u == "":
		return slotURLInfo{}, errors.New("url is required")
	case hasControl(u) || strings.ContainsAny(u, " \\"):
		// A browser drops tabs and newlines inside a URL and reads a
		// backslash as a slash: "/\evil.example" is "//evil.example".
		return slotURLInfo{}, fmt.Errorf("url %q must not contain spaces, backslashes or control characters", raw)
	case strings.HasPrefix(u, "//"):
		return slotURLInfo{}, fmt.Errorf("url %q is protocol-relative: it leads to another host", raw)
	case strings.HasPrefix(u, "/"):
		if err := noDotSegments(u); err != nil {
			return slotURLInfo{}, fmt.Errorf("url %q %v", raw, err)
		}
		return slotURLInfo{url: u}, nil
	case !schemePrefix.MatchString(u):
		return slotURLInfo{}, fmt.Errorf("url %q must be a path on this instance, e.g. /portal/app/<addon>?q={q}, or an absolute http(s) URL on its own origin", raw)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return slotURLInfo{}, fmt.Errorf("url %q is not a valid URL", raw)
	}
	switch scheme := strings.ToLower(parsed.Scheme); {
	case scheme != "http" && scheme != "https":
		return slotURLInfo{}, fmt.Errorf("url %q: a slot leads to an http(s) page or API, never to a %s: URL", raw, scheme)
	case parsed.User != nil:
		return slotURLInfo{}, fmt.Errorf("url %q must not carry credentials", raw)
	case parsed.Host == "" || !strings.HasPrefix(u[len(parsed.Scheme):], "://"):
		return slotURLInfo{}, fmt.Errorf("url %q names no host", raw)
	}
	// The path as written, from the first separator after the host on.
	rest := u[len(parsed.Scheme)+3:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[i:]
	} else {
		rest = ""
	}
	if err := noDotSegments(rest); err != nil {
		return slotURLInfo{}, fmt.Errorf("url %q %v", raw, err)
	}
	return slotURLInfo{url: u, origin: originOf(parsed)}, nil
}

// slotURL checks a slot URL and that, when absolute, it is on one of the
// instance's origins.
func slotURL(raw string, origins []string) (string, error) {
	info, err := parseSlotURL(raw)
	if err != nil {
		return "", err
	}
	switch {
	case info.origin == "" || slices.Contains(origins, info.origin):
		return info.url, nil
	case len(origins) == 0:
		return "", fmt.Errorf("url %q is absolute, and this instance knows no origin of its own to check it against — use a path, e.g. /portal/app/<addon>", raw)
	default:
		return "", fmt.Errorf("url %q leads to %s, not to this instance (%s) — a slot may only lead to a path here", raw, info.origin, strings.Join(origins, ", "))
	}
}

// noDotSegments refuses a path that names "." or ".." as a segment, spelled
// out or percent-encoded: a browser resolves both before it sends anything,
// so "/api/portal/apps/example/../../addons" is a request to /api/portal/addons.
func noDotSegments(u string) error {
	path := u
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	decoded, err := decodePath(path)
	if err != nil {
		return err
	}
	for _, seg := range strings.Split(decoded, "/") {
		if seg == "." || seg == ".." {
			return errors.New("must not contain '.' or '..' segments")
		}
	}
	return nil
}

// slotPath is the path of a checked slot URL, relative or absolute.
func slotPath(u string) string {
	if !strings.HasPrefix(u, "/") {
		if parsed, err := url.Parse(u); err == nil {
			u = parsed.EscapedPath()
		}
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// proxyPath is where an addon's own API is reached from a browser: through
// the portal's proxy.
func proxyPath(key string) string { return "/api/portal/apps/" + key + "/" }

// originOf is an origin in one spelling: lower-case scheme and host, the
// default port left out.
func originOf(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	switch {
	case port != "":
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// normaliseOrigin reads an origin someone named — a public base, the portal's
// hostname: http(s), a host, and nothing after it.
func normaliseOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "":
		return "", fmt.Errorf("%q is no origin — scheme and host, e.g. https://media.example.org", raw)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%q is no http(s) origin", raw)
	case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", fmt.Errorf("%q is no origin: nothing may follow the host", raw)
	}
	return originOf(u), nil
}

// slotKind checks a row's kind; empty is a link.
func slotKind(kind string) (string, error) {
	switch kind = strings.TrimSpace(kind); kind {
	case "":
		return "link", nil
	case "link", "action":
		return kind, nil
	}
	return "", fmt.Errorf("kind %q must be link or action", kind)
}

// slotMethodOf checks a row's method: POST, or nothing (POST is stored).
func slotMethodOf(method string) (string, error) {
	switch m := strings.ToUpper(strings.TrimSpace(method)); m {
	case "", slotMethod:
		return slotMethod, nil
	default:
		return "", fmt.Errorf("method %q: an action is sent as a POST, and a link has no method — leave it empty or POST", method)
	}
}

// checkSlot checks a row's destination and returns it as stored. origins are
// the instance's own; actionPrefix, when set, is the only place an action may
// lead: the addon's own API.
func checkSlot(e model.Extension, origins []string, actionPrefix string) (model.Extension, error) {
	var err error
	if e.Kind, err = slotKind(e.Kind); err != nil {
		return e, err
	}
	if e.Method, err = slotMethodOf(e.Method); err != nil {
		return e, err
	}
	if e.URL, err = slotURL(e.URL, origins); err != nil {
		return e, err
	}
	if strings.TrimSpace(e.StatusURL) != "" {
		if e.StatusURL, err = slotURL(e.StatusURL, origins); err != nil {
			return e, fmt.Errorf("status %w", err)
		}
	} else {
		e.StatusURL = ""
	}
	if e.Kind == "action" && actionPrefix != "" && !strings.HasPrefix(slotPath(e.URL), actionPrefix) {
		return e, fmt.Errorf("url %q: an action is sent with the signed-in user's token, so an addon's action leads to its own API — %s…", e.URL, actionPrefix)
	}
	return e, nil
}

// servable is a stored row as it may be served: kind, method and the shape of
// its URL as the rules say — a row written before them that breaks one is not
// served at all, and a status URL that breaks one is dropped. Which origin an
// absolute URL names is checked where rows are written; a read cannot know.
func servable(e model.Extension) (model.Extension, bool) {
	var err error
	if e.Kind, err = slotKind(e.Kind); err != nil {
		return e, false
	}
	if e.Method, err = slotMethodOf(e.Method); err != nil {
		return e, false
	}
	info, err := parseSlotURL(e.URL)
	if err != nil {
		return e, false
	}
	e.URL = info.url
	if s, err := parseSlotURL(e.StatusURL); err == nil {
		e.StatusURL = s.url
	} else {
		e.StatusURL = ""
	}
	return e, true
}

// instanceOrigins are the origins a slot URL may name when a row is written
// through the API: the platform's public origin, and — for an admin, whose
// browser is on the instance — the origin the request came in on.
func (a *API) instanceOrigins(r *http.Request) []string {
	var out []string
	add := func(raw string) {
		if raw == "" {
			return
		}
		if o, err := normaliseOrigin(raw); err == nil && !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	add(a.publicOrigin(r.Context()))
	if p, _ := auth.PrincipalFrom(r.Context()); p != nil && p.Admin {
		add(requestOrigin(r))
	}
	return out
}
