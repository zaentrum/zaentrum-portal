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
	AlwaysPull        bool   `json:"alwaysPull"` // image re-pulls on restart
}

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
		out = append(out, Instance{
			Name:              d.Metadata.Name,
			Image:             img,
			DesiredReplicas:   desired,
			ReadyReplicas:     int(d.Status.ReadyReplicas),
			UpdatedReplicas:   int(d.Status.UpdatedReplicas),
			AvailableReplicas: int(d.Status.AvailableReplicas),
			Restarts:          restarts,
			Phase:             phaseOf(desired, int(d.Status.ReadyReplicas), int(d.Status.UpdatedReplicas)),
			Protected:         s.protected[d.Metadata.Name],
			OperatorManaged:   ownedByZaentrum(d),
			AlwaysPull:        strings.EqualFold(pull, "Always") || strings.HasSuffix(img, ":latest"),
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
func (s *Service) Scale(ctx context.Context, name string, replicas int) error {
	if err := validName(name); err != nil {
		return err
	}
	if s.protected[name] {
		return fmt.Errorf("%q is a protected (stateful) service and cannot be scaled from here", name)
	}
	if replicas < 0 || replicas > 20 {
		return fmt.Errorf("replicas must be between 0 and 20")
	}
	// Prefer the CR path only when an operator owns THIS deployment (else a raw
	// scale would be reverted by the operator's periodic re-apply).
	if info, _ := s.operatorInfo(ctx); info.Present {
		if d, err := s.k8s.GetDeployment(ctx, name); err == nil && ownedByZaentrum(*d) {
			patch := []byte(fmt.Sprintf(`{"spec":{"replicas":{%q:%d}}}`, name, replicas))
			return s.k8s.PatchResource(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural, info.Name, patch)
		}
	}
	return s.k8s.ScaleDeployment(ctx, name, replicas)
}

// Restart rolls a deployment (re-pulling :latest with imagePullPolicy:Always).
// Guarded against protected deployments.
func (s *Service) Restart(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if s.protected[name] {
		return fmt.Errorf("%q is a protected (stateful) service and cannot be restarted from here", name)
	}
	return s.k8s.RestartDeployment(ctx, name, s.now().UTC().Format(time.RFC3339))
}

// ─── operator (desired state via the Zaentrum CR) ───────────────────────────────

// zaentrumList is the minimal decode of a Zaentrum CR collection.
type zaentrumList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
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
			Phase           string `json:"phase"`
			CurrentVersion  string `json:"currentVersion"`
			AvailableUpdate string `json:"availableUpdate"`
			Components      []struct {
				Name  string `json:"name"`
				Ready bool   `json:"ready"`
				Image string `json:"image"`
			} `json:"components"`
		} `json:"status"`
	} `json:"items"`
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
		Present:         true,
		Name:            it.Metadata.Name,
		Channel:         it.Spec.Channel,
		Version:         it.Spec.Version,
		UpdateMode:      it.Spec.Update.Mode,
		Hostname:        it.Spec.Hostname,
		Phase:           it.Status.Phase,
		CurrentVersion:  it.Status.CurrentVersion,
		AvailableUpdate: it.Status.AvailableUpdate,
		Components:      comps,
	}, nil
}

// SetOperator patches the Zaentrum CR spec (version/channel/update mode). Empty
// fields are left unchanged. Requires an operator to be present.
func (s *Service) SetOperator(ctx context.Context, version, channel, updateMode *string) error {
	info, _ := s.operatorInfo(ctx)
	if !info.Present {
		return fmt.Errorf("no operator instance to configure")
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
		return fmt.Errorf("nothing to change")
	}
	patch, _ := json.Marshal(map[string]any{"spec": spec})
	return s.k8s.PatchResource(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural, info.Name, patch)
}

// ApplyUpdate pins spec.version to the channel's available update (status.availableUpdate),
// i.e. "update now" — the operator then rolls every service to that tag.
func (s *Service) ApplyUpdate(ctx context.Context) error {
	info, _ := s.operatorInfo(ctx)
	if !info.Present {
		return fmt.Errorf("no operator instance to update")
	}
	if info.AvailableUpdate == "" {
		return fmt.Errorf("no update available")
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"version":%q}}`, info.AvailableUpdate))
	return s.k8s.PatchResource(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural, info.Name, patch)
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
