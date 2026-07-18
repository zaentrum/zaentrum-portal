package operator

import (
	"context"
	"regexp"
	"sort"
)

// PodLog is one pod (+ its container names + phase) for the log-viewer selector.
type PodLog struct {
	Pod        string   `json:"pod"`
	Phase      string   `json:"phase"`
	Containers []string `json:"containers"`
}

// LogPods lists the namespace's pods with their container names + phase, so the
// log viewer can offer a pod/container selector. Admin-gated at the router.
func (s *Service) LogPods(ctx context.Context) ([]PodLog, error) {
	pods, err := s.k8s.ListPods(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]PodLog, 0, len(pods))
	for _, p := range pods {
		cs := make([]string, 0, len(p.Spec.Containers))
		for _, c := range p.Spec.Containers {
			cs = append(cs, c.Name)
		}
		out = append(out, PodLog{Pod: p.Metadata.Name, Phase: p.Status.Phase, Containers: cs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pod < out[j].Pod })
	return out, nil
}

// Logs returns a pod container's recent logs with secrets redacted. tail caps the
// number of lines (default 500, max 5000); since bounds age in seconds (0 =
// unbounded). Names are validated before they reach the apiserver URL path.
func (s *Service) Logs(ctx context.Context, pod, container string, tail, since int) (string, error) {
	if err := validName(pod); err != nil {
		return "", err
	}
	if container != "" {
		if err := validName(container); err != nil {
			return "", err
		}
	}
	if tail <= 0 {
		tail = 500
	}
	if tail > 5000 {
		tail = 5000
	}
	if since < 0 {
		since = 0
	}
	raw, err := s.k8s.PodLogs(ctx, pod, container, tail, since)
	if err != nil {
		return "", err
	}
	return ScrubSecrets(string(raw)), nil
}

// ─── secret redaction ─────────────────────────────────────────────────────────
// ScrubSecrets removes credential-shaped values from text so neither the live log
// viewer nor the (future) support-bundle export ever surfaces passwords/tokens.
// Best-effort by design: it targets the common shapes rather than claiming to
// catch everything, and is applied on top of admin-only access — not instead of.
// Exported so the export bundler reuses the exact same redaction.
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

func ScrubSecrets(s string) string {
	s = reSecretKV.ReplaceAllString(s, `${1}${2}***REDACTED***`)
	s = reBearer.ReplaceAllString(s, `${1}***REDACTED***`)
	s = reJWT.ReplaceAllString(s, `***REDACTED-JWT***`)
	s = reURICreds.ReplaceAllString(s, `${1}***REDACTED***${2}`)
	return s
}
