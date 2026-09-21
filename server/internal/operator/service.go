// Package operator backs the portal's operator/instances console. It reads live
// Deployments (observed state) and, when the platform is managed by the
// zaentrum-operator's Zaentrum CR, reads/patches that CR (desired state). This makes
// the console work both for a plain-manifest deployment (the demo — scale/restart
// act on Deployments directly and persist) and for an all-in-one appliance where
// the operator reconciles the platform (scale a service by patching the CR, since
// the operator re-applies Deployments and would otherwise revert a raw edit).
package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

// dns1123 is the allowed shape of a Deployment name. Validating it before we
// interpolate the name into the apiserver URL path makes traversal impossible by
// construction (defense-in-depth; the router + apiserver already block it).
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func validName(name string) error {
	if len(name) == 0 || len(name) > 253 || !dns1123.MatchString(name) {
		return fmt.Errorf("invalid service name %q", name)
	}
	return nil
}

// Service is the operator/instances service.
type Service struct {
	k8s       *k8s.Client
	cfg       config.Config
	protected map[string]bool
	now       func() time.Time // injectable for tests
}

func New(client *k8s.Client, cfg config.Config) *Service {
	prot := make(map[string]bool, len(cfg.ProtectedNames))
	for _, n := range cfg.ProtectedNames {
		prot[n] = true
	}
	return &Service{k8s: client, cfg: cfg, protected: prot, now: time.Now}
}

// Available reports whether instance management is possible (in-cluster).
func (s *Service) Available() bool { return s.k8s.InCluster() }

// ─── DTOs (JSON to the UI) ───────────────────────────────────────────────────

type Instance struct {
	Name              string `json:"name"`
	Image             string `json:"image"`
	DesiredReplicas   int    `json:"desiredReplicas"`
	ReadyReplicas     int    `json:"readyReplicas"`
	UpdatedReplicas   int    `json:"updatedReplicas"`
	AvailableReplicas int    `json:"availableReplicas"`
	Restarts          int    `json:"restarts"`
	Phase             string `json:"phase"` // ready|progressing|degraded|stopped
	Protected         bool   `json:"protected"`
	OperatorManaged   bool   `json:"operatorManaged"`
	// Group is how an operator has to reason about this workload:
	//   platform — the operator renders it from the chart; it upgrades with it
	//   addon    — deployed alongside, with its own repo, lifecycle and version
	//   other    — running here, claimed by neither; worth seeing precisely
	//              because nothing owns it
	Group string `json:"group"`
	// Reason is why the workload is not healthy, in the words the cluster
	// used. Empty when it is fine.
	Reason     string `json:"reason"`
	AlwaysPull bool   `json:"alwaysPull"` // image re-pulls on restart
	// Addon and Component are the grouping labels an addon's deployment
	// channel stamps (zaentrum.io/addon, zaentrum.io/component). Metadata
	// only: the console uses them to place a workload under its addon, never
	// to select anything.
	Addon     string `json:"addon,omitempty"`
	Component string `json:"component,omitempty"`
	// Generation and ObservedGeneration are what makes a wait exact.
	//
	// The replica counters alone cannot answer "has my rollout started?": for
	// a few seconds after a restart they describe the pods from BEFORE it —
	// ready=1, updated=1, all of it true, all of it about the old pod. A
	// client that waits on them reports success while the thing it asked for
	// has not begun. Generation counts spec changes, ObservedGeneration says
	// which one the Deployment controller has acted on, and a write answers
	// with the generation it produced (see Write), so the two together are a
	// gate the old pods cannot pass.
	Generation         int64 `json:"generation"`
	ObservedGeneration int64 `json:"observedGeneration"`
	// RestartedAt is the rollout-restart stamp on the pod template, empty when
	// there is none. A restart moves it, so a client can also tell ITS restart
	// from one somebody else asked for.
	RestartedAt string `json:"restartedAt"`
}

// Write is what a write to one workload produced: the Deployment generation to
// wait for, and — for a restart — the stamp it wrote.
type Write struct {
	Name        string `json:"name"`
	Generation  int64  `json:"generation"`
	RestartedAt string `json:"restartedAt,omitempty"`
}

// Update is what a write to the platform produced: the version the operator's
// resource now asks for, and the generation that write made.
type Update struct {
	Version    string `json:"version"`
	Generation int64  `json:"generation"`
}

// ErrNoWorkload: this namespace runs no Deployment by that name. Definitive,
// and different in kind from a refusal — the API answers it 404 so a client
// can branch on it instead of parsing a message.
var ErrNoWorkload = errors.New("no such workload")

// ErrUpdateChanged: the update on the shelf is no longer the one the caller
// named. The channel moved, or the operator discovered another one since the
// caller looked. Applying it anyway would roll the platform to a version
// nobody asked for, so the write is refused (409) and the caller looks again.
var ErrUpdateChanged = errors.New("the available update changed")

// Grouping labels. They are read, never written, and never used as selectors:
// selectors are immutable, and a label that could move pods would make adding
// it to an already-running addon a breaking change.
const (
	LabelAddon     = "zaentrum.io/addon"
	LabelComponent = "zaentrum.io/component"
)

type Component struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	Image string `json:"image"`
}

type OperatorInfo struct {
	Present         bool        `json:"present"`
	Name            string      `json:"name"`
	Channel         string      `json:"channel"`
	Version         string      `json:"version"`
	UpdateMode      string      `json:"updateMode"`
	Hostname        string      `json:"hostname"`
	Phase           string      `json:"phase"`
	CurrentVersion  string      `json:"currentVersion"`
	AvailableUpdate string      `json:"availableUpdate"`
	Components      []Component `json:"components"`
	// Generation and ObservedGeneration are the same gate the workloads carry,
	// one level up: the spec generation of the operator's resource, and the one
	// its status was written for. A client that changed the platform waits
	// until the operator has reconciled ITS write, not merely until something
	// reports Ready.
	Generation         int64 `json:"generation"`
	ObservedGeneration int64 `json:"observedGeneration"`
	// Note surfaces a hint when the CR is absent or unreadable (e.g. demo mode).
	Note string `json:"note,omitempty"`
}

// ─── instances (observed) ────────────────────────────────────────────────────

// Instances lists the platform Deployments with folded pod restarts + status.
func (s *Service) Instances(ctx context.Context) ([]Instance, error) {
	deploys, err := s.k8s.ListDeployments(ctx, s.cfg.InstanceSelector)
	if err != nil {
		return nil, err
	}
	pods, err := s.k8s.ListPods(ctx, "")
	if err != nil {
		// Pods are best-effort (restart counts); don't fail the whole list.
		pods = nil
	}

	out := make([]Instance, 0, len(deploys))
	for _, d := range deploys {
		restarts := sumRestartsForDeployment(pods, d)
		img, pull := primaryContainer(d)
		desired := int(deref(d.Spec.Replicas))
		addon, component := addonLabels(d)
		out = append(out, Instance{
			Name:              d.Metadata.Name,
			Image:             img,
			DesiredReplicas:   desired,
			ReadyReplicas:     int(d.Status.ReadyReplicas),
			UpdatedReplicas:   int(d.Status.UpdatedReplicas),
			AvailableReplicas: int(d.Status.AvailableReplicas),
			Restarts:          restarts,
			Phase: phaseWithReason(
				phaseOf(desired, int(d.Status.ReadyReplicas), int(d.Status.UpdatedReplicas)),
				unhealthyReason(pods, d)),
			Protected:          s.protected[d.Metadata.Name],
			OperatorManaged:    ownedByZaentrum(d),
			Group:              groupOf(d),
			Reason:             unhealthyReason(pods, d),
			AlwaysPull:         strings.EqualFold(pull, "Always") || strings.HasSuffix(img, ":latest"),
			Addon:              addon,
			Component:          component,
			Generation:         d.Metadata.Generation,
			ObservedGeneration: d.Status.ObservedGeneration,
			RestartedAt:        d.RestartedAt(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Scale sets a deployment's replica count. Guarded against protected
// (stateful) deployments. When the platform is operator-managed AND this
// deployment is owned by the Zaentrum CR, it patches the CR's spec.replicas map
// (durable — the operator reconciles it); otherwise it scales the Deployment
// directly (the demo/plain-manifest case).
func (s *Service) Scale(ctx context.Context, name string, replicas int) (Write, error) {
	if err := validName(name); err != nil {
		return Write{}, err
	}
	if s.protected[name] {
		return Write{}, fmt.Errorf("%q is a protected (stateful) service and cannot be scaled from here", name)
	}
	if replicas < 0 || replicas > 20 {
		return Write{}, fmt.Errorf("replicas must be between 0 and 20")
	}
	// One read answers two questions: does this workload exist at all — a
	// caller that names one we do not run gets 404, not a refusal — and who
	// owns it, which decides where the write goes.
	d, err := s.k8s.GetDeployment(ctx, name)
	if err != nil {
		if k8s.IsNotFound(err) {
			return Write{}, fmt.Errorf("%w: %s", ErrNoWorkload, name)
		}
		return Write{}, err
	}
	// Prefer the CR path only when an operator owns THIS deployment (else a raw
	// scale would be reverted by the operator's periodic re-apply).
	if info, _ := s.operatorInfo(ctx); info.Present && ownedByZaentrum(*d) {
		patch := []byte(fmt.Sprintf(`{"spec":{"replicas":{%q:%d}}}`, name, replicas))
		if _, err := s.k8s.PatchResource(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural, info.Name, patch); err != nil {
			return Write{}, err
		}
		// The Deployment is untouched here: the operator will rewrite it on
		// its next reconcile. Its generation now is therefore a lower bound —
		// it lets a waiter reject a stale status, and the replica count it is
		// waiting for is what actually says the change landed.
		return Write{Name: name, Generation: d.Metadata.Generation, RestartedAt: d.RestartedAt()}, nil
	}
	patched, err := s.k8s.ScaleDeployment(ctx, name, replicas)
	if err != nil {
		if k8s.IsNotFound(err) {
			return Write{}, fmt.Errorf("%w: %s", ErrNoWorkload, name)
		}
		return Write{}, err
	}
	return Write{Name: name, Generation: patched.Metadata.Generation, RestartedAt: patched.RestartedAt()}, nil
}

// Restart rolls a deployment (re-pulling :latest with imagePullPolicy:Always).
// Guarded against protected deployments.
func (s *Service) Restart(ctx context.Context, name string) (Write, error) {
	if err := validName(name); err != nil {
		return Write{}, err
	}
	if s.protected[name] {
		return Write{}, fmt.Errorf("%q is a protected (stateful) service and cannot be restarted from here", name)
	}
	d, err := s.k8s.RestartDeployment(ctx, name, s.now().UTC().Format(time.RFC3339))
	if err != nil {
		if k8s.IsNotFound(err) {
			return Write{}, fmt.Errorf("%w: %s", ErrNoWorkload, name)
		}
		return Write{}, err
	}
	// The generation this stamp produced, and the stamp itself: a waiter needs
	// both, because "ready" is true of the pods that were already running.
	return Write{Name: name, Generation: d.Metadata.Generation, RestartedAt: d.RestartedAt()}, nil
}

// ─── operator (desired state via the Zaentrum CR) ───────────────────────────────

// zaentrumList is the minimal decode of a Zaentrum CR collection.
type zaentrumList struct {
	Items []zaentrumCR `json:"items"`
}

type zaentrumCR struct {
	Metadata struct {
		Name       string `json:"name"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Channel  string `json:"channel"`
		Version  string `json:"version"`
		Hostname string `json:"hostname"`
		Update   struct {
			Mode string `json:"mode"`
		} `json:"update"`
	} `json:"spec"`
	Status struct {
		Phase              string `json:"phase"`
		CurrentVersion     string `json:"currentVersion"`
		AvailableUpdate    string `json:"availableUpdate"`
		ObservedGeneration int64  `json:"observedGeneration"`
		Components         []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
			Image string `json:"image"`
		} `json:"components"`
	} `json:"status"`
}

// OperatorInfo returns the Zaentrum CR summary, or {Present:false} + a note when the
// CRD/CR is absent (the demo) — never a hard error for that case.
func (s *Service) OperatorInfo(ctx context.Context) (OperatorInfo, error) {
	return s.operatorInfo(ctx)
}

func (s *Service) operatorInfo(ctx context.Context) (OperatorInfo, error) {
	if !s.k8s.InCluster() {
		return OperatorInfo{Present: false, Note: "not running in a cluster"}, nil
	}
	raw, err := s.k8s.GetResourceList(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural)
	if err != nil {
		if k8s.IsNotFound(err) {
			return OperatorInfo{Present: false, Note: "no operator detected — managing deployments directly"}, nil
		}
		if k8s.IsForbidden(err) {
			return OperatorInfo{Present: false, Note: "operator status not readable (insufficient permissions)"}, nil
		}
		return OperatorInfo{Present: false, Note: "operator status unavailable"}, nil
	}
	var list zaentrumList
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Items) == 0 {
		return OperatorInfo{Present: false, Note: "no operator instance found"}, nil
	}
	it := list.Items[0]
	comps := make([]Component, 0, len(it.Status.Components))
	for _, c := range it.Status.Components {
		comps = append(comps, Component{Name: c.Name, Ready: c.Ready, Image: c.Image})
	}
	return OperatorInfo{
		Present:            true,
		Name:               it.Metadata.Name,
		Channel:            it.Spec.Channel,
		Version:            it.Spec.Version,
		UpdateMode:         it.Spec.Update.Mode,
		Hostname:           it.Spec.Hostname,
		Phase:              it.Status.Phase,
		CurrentVersion:     it.Status.CurrentVersion,
		AvailableUpdate:    it.Status.AvailableUpdate,
		Components:         comps,
		Generation:         it.Metadata.Generation,
		ObservedGeneration: it.Status.ObservedGeneration,
	}, nil
}

// SetOperator patches the Zaentrum CR spec (version/channel/update mode). Empty
// fields are left unchanged. Requires an operator to be present. It answers
// with the version the resource now asks for and the generation the patch
// made, so a caller can wait for the operator to reconcile THIS write.
func (s *Service) SetOperator(ctx context.Context, version, channel, updateMode *string) (Update, error) {
	info, _ := s.operatorInfo(ctx)
	if !info.Present {
		return Update{}, fmt.Errorf("no operator instance to configure")
	}
	spec := map[string]any{}
	if version != nil {
		spec["version"] = *version
	}
	if channel != nil {
		spec["channel"] = *channel
	}
	if updateMode != nil {
		spec["update"] = map[string]any{"mode": *updateMode}
	}
	if len(spec) == 0 {
		return Update{}, fmt.Errorf("nothing to change")
	}
	patch, _ := json.Marshal(map[string]any{"spec": spec})
	return s.patchOperator(ctx, info.Name, patch)
}

// ApplyUpdate pins spec.version to the channel's available update (status.availableUpdate),
// i.e. "update now" — the operator then rolls every service to that tag.
//
// expect, when given, is the update the caller saw when it decided. If the
// operator has since discovered another one — because the channel changed, or
// because a newer release landed — the write is refused with ErrUpdateChanged
// rather than rolling the platform to a version nobody chose.
func (s *Service) ApplyUpdate(ctx context.Context, expect string) (Update, error) {
	info, _ := s.operatorInfo(ctx)
	if !info.Present {
		return Update{}, fmt.Errorf("no operator instance to update")
	}
	if info.AvailableUpdate == "" {
		return Update{}, fmt.Errorf("no update available")
	}
	if expect != "" && expect != info.AvailableUpdate {
		return Update{}, fmt.Errorf("%w: %s is on the %s channel now, not %s",
			ErrUpdateChanged, info.AvailableUpdate, channelOr(info.Channel), expect)
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"version":%q}}`, info.AvailableUpdate))
	return s.patchOperator(ctx, info.Name, patch)
}

// patchOperator applies a patch to the operator's resource and reads back what
// the apiserver made of it.
func (s *Service) patchOperator(ctx context.Context, name string, patch []byte) (Update, error) {
	raw, err := s.k8s.PatchResource(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural, name, patch)
	if err != nil {
		return Update{}, err
	}
	var cr zaentrumCR
	if err := json.Unmarshal(raw, &cr); err != nil {
		// The write landed; only the answer was unreadable. Say so rather than
		// reporting a failure that did not happen — a caller that retried would
		// be applying it twice.
		return Update{}, nil
	}
	return Update{Version: cr.Spec.Version, Generation: cr.Metadata.Generation}, nil
}

func channelOr(c string) string {
	if strings.TrimSpace(c) == "" {
		return "configured"
	}
	return c
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func deref(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func primaryContainer(d k8s.Deployment) (image, pullPolicy string) {
	if len(d.Spec.Template.Spec.Containers) > 0 {
		c := d.Spec.Template.Spec.Containers[0]
		return c.Image, c.ImagePullPolicy
	}
	return "", ""
}

func ownedByZaentrum(d k8s.Deployment) bool {
	for _, o := range d.Metadata.OwnerReferences {
		if o.Kind == "Zaentrum" {
			return true
		}
	}
	return false
}

// unhealthyReason reports why a deployment's pods are not running, taken from
// the container state the cluster itself set.
//
// This exists because "degraded" alone is not actionable. Beta sat in
// ImagePullBackOff for 30 hours across 13 services; the console showed
// "degraded" for every one of them and never said the four words —
// "cannot pull the image" — that point at the fix.
//
// Waiting beats terminated: a container that crashed and is now waiting to be
// restarted reports both, and the waiting reason is the current state.
func unhealthyReason(pods []k8s.Pod, d k8s.Deployment) string {
	for _, p := range pods {
		if !podMatches(p, d) {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				continue
			}
			if w := cs.State.Waiting; w != nil && w.Reason != "" {
				// ContainerCreating and PodInitializing are the normal path
				// through a rollout, not a fault to report.
				if w.Reason == "ContainerCreating" || w.Reason == "PodInitializing" {
					continue
				}
				return w.Reason
			}
			if t := cs.State.Terminated; t != nil && t.Reason != "" && t.Reason != "Completed" {
				return fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
			}
		}
	}
	return ""
}

// addonLabels reads the grouping labels. A component label without an addon
// label names nothing — a component is only ever a component OF something.
func addonLabels(d k8s.Deployment) (addon, component string) {
	addon = strings.TrimSpace(d.Metadata.Labels[LabelAddon])
	if addon == "" {
		return "", ""
	}
	return addon, strings.TrimSpace(d.Metadata.Labels[LabelComponent])
}

// groupOf classifies a workload.
//
// Owner references are the authority for "platform": the operator sets them, so
// they cannot drift from what it actually reconciles. Addons have no owner ref
// (nothing owns them — that IS the distinction), so they are identified by
// labels their deployment channel stamps: `zaentrum.io/addon: <addon key>`,
// or the older part-of label the addons kustomization applies,
// `app.kubernetes.io/part-of: <namespace>-addons`.
//
// The part-of suffix is matched rather than the full value because the prefix
// is the environment name — beta stamps `zaentrum-beta-addons`, and hardcoding
// that would silently classify every addon as "other" in any other install.
func groupOf(d k8s.Deployment) string {
	if ownedByZaentrum(d) {
		return "platform"
	}
	if addon, _ := addonLabels(d); addon != "" {
		return "addon"
	}
	if strings.HasSuffix(d.Metadata.Labels["app.kubernetes.io/part-of"], "-addons") {
		return "addon"
	}
	return "other"
}

// phaseWithReason refuses to call a workload "ready" while one of its pods is
// failing.
//
// This is the exact mechanism that hid a 36-hour outage, reproduced inside the
// console meant to reveal it. During a stuck rollout the counters describe two
// DIFFERENT pods:
//
//	readyReplicas=1    the old pod, still serving, days old
//	updatedReplicas=1  the new pod, in ImagePullBackOff
//
// phaseOf() sees 1/1/1 and says "ready" — every field it looks at is true, and
// the conclusion is false. Beta rendered a green "ready" badge next to
// "ErrImagePull" on the same row.
//
// So the reason is authoritative over the counters: if a pod is broken, the
// deployment is not ready, whatever the arithmetic says.
func phaseWithReason(phase, reason string) string {
	if reason != "" && phase == "ready" {
		return "degraded"
	}
	return phase
}

func phaseOf(desired, ready, updated int) string {
	switch {
	case desired == 0:
		return "stopped"
	case ready >= desired && updated >= desired:
		return "ready"
	case ready == 0:
		return "degraded"
	default:
		return "progressing"
	}
}

// podMatches attributes a pod to a deployment by the deployment's selector
// matchLabels (the correct Kubernetes way — the pod must carry all of them).
func podMatches(p k8s.Pod, d k8s.Deployment) bool {
	sel := d.Spec.Selector.MatchLabels
	if len(sel) == 0 {
		return false
	}
	for k, v := range sel {
		if p.Metadata.Labels[k] != v {
			return false
		}
	}
	return true
}

func sumRestartsForDeployment(pods []k8s.Pod, d k8s.Deployment) int {
	total := 0
	for _, p := range pods {
		if !podMatches(p, d) {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			total += int(cs.RestartCount)
		}
	}
	return total
}
