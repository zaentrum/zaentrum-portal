package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Chart addons.
//
// An addon can be a standard Helm chart that the zaentrum-operator installs
// from one ZaentrumAddon resource in the platform namespace: the chart
// reference, non-secret values, and valuesFrom entries naming where the secret
// inputs are kept. portal-api writes that resource. It never renders a chart
// and never applies a workload — the operator does both.
//
// Secrets are create-only for portal-api. Every write of secret inputs creates
// a new immutable Secret holding just those inputs and points their valuesFrom
// entries at it. portal-api never reads, changes or deletes a Secret: a Secret
// no entry references any more, and one whose addon is gone, is the
// operator's to collect.

const (
	// AnnotationPrimary, in a chart's Chart.yaml, names the Service (port 80)
	// that serves the addon's capability manifest.
	AnnotationPrimary = "zaentrum.io/primary"
	// AnnotationTitle is a chart's optional display title.
	AnnotationTitle = "zaentrum.io/title"
	// AnnotationKeepValues on a ZaentrumAddon asks the operator to keep the
	// Secrets holding its values when the addon is deleted.
	AnnotationKeepValues = "zaentrum.io/keep-values"
)

// ChartPhaseReady is the phase of an applied addon whose workloads are ready.
const ChartPhaseReady = "Ready"

// AddonSecretPrefix starts the name of every Secret that belongs to an addon:
// the only Secrets its valuesFrom may reference through portal-api.
func AddonSecretPrefix(addon string) string { return "zaentrum-addon-" + addon + "-" }

// ValuesSecretPrefix is the generateName of the Secrets secret inputs are
// written to.
func ValuesSecretPrefix(addon string) string { return AddonSecretPrefix(addon) + "values-" }

// ChartSource is a chart reference: an oci:// ref with a version (its tag), or
// an https:// link to a chart archive. Digest optionally pins the archive.
type ChartSource struct {
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

// ValuesFrom is one spec.valuesFrom entry (Flux HelmRelease semantics).
type ValuesFrom struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	ValuesKey  string `json:"valuesKey,omitempty"`
	TargetPath string `json:"targetPath,omitempty"`
	Optional   bool   `json:"optional,omitempty"`
}

// SecretRef is where a secret input is read from: a key of a Secret.
type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// ChartObject names one object a plan renders.
type ChartObject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// ChartPlan is the part of status.plan portal-api reasons about. The whole plan
// is passed through untouched as ChartAddon.PlanRaw.
type ChartPlan struct {
	Chart struct {
		Name        string            `json:"name"`
		Version     string            `json:"version"`
		AppVersion  string            `json:"appVersion"`
		Description string            `json:"description"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
	} `json:"chart"`
	ValuesSchema string        `json:"valuesSchema"`
	ValuesErrors []string      `json:"valuesErrors"`
	Violations   []string      `json:"violations"`
	Objects      []ChartObject `json:"objects"`
}

// Renders answers whether the plan renders an object of this kind and name.
func (p *ChartPlan) Renders(kind, name string) bool {
	for _, o := range p.Objects {
		if o.Kind == kind && o.Name == name {
			return true
		}
	}
	return false
}

// ChartComponent is one applied workload with its live readiness.
type ChartComponent struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Ready   int    `json:"ready"`
	Desired int    `json:"desired"`
	Reason  string `json:"reason"`
}

// ChartAddon is a ZaentrumAddon as portal-api reads and writes it.
type ChartAddon struct {
	Name            string
	Generation      int64
	ResourceVersion string
	// Deleting: the resource is being deleted — a finalizer still holds it.
	Deleting bool
	// KeepValues: it carries the keep-values annotation.
	KeepValues bool

	// spec — the fields portal-api manages. valuesFrom is kept entry by
	// entry as read, so what portal-api does not manage is written back as it
	// was: see SecretInputs, SetSecretInput and ClearSecretInput.
	Chart      ChartSource
	Values     json.RawMessage // spec.values; empty when unset
	Suspend    bool
	valuesFrom []map[string]any

	// status — the operator's.
	Phase              string
	Message            string
	ObservedGeneration int64
	LastAppliedChart   *ChartSource
	PlanRaw            json.RawMessage // status.plan as written; empty without a plan
	Plan               *ChartPlan      // decoded PlanRaw; nil without a plan
	Components         []ChartComponent

	// obj is the resource as read. An update writes the managed spec fields
	// onto it, so fields portal-api does not know survive a round trip.
	obj map[string]any
}

// PlanCurrent answers whether the plan describes the current spec: the operator
// has observed this generation and reported a plan for it.
func (a *ChartAddon) PlanCurrent() bool {
	return a.Plan != nil && a.ObservedGeneration == a.Generation
}

// Primary is the Service the chart names as serving the manifest; "" when the
// plan does not say.
func (a *ChartAddon) Primary() string {
	if a.Plan == nil {
		return ""
	}
	return a.Plan.Chart.Annotations[AnnotationPrimary]
}

// ValuesFrom is spec.valuesFrom, typed.
func (a *ChartAddon) ValuesFrom() []ValuesFrom {
	out := make([]ValuesFrom, 0, len(a.valuesFrom))
	for _, e := range a.valuesFrom {
		b, _ := json.Marshal(e)
		var vf ValuesFrom
		_ = json.Unmarshal(b, &vf)
		out = append(out, vf)
	}
	return out
}

// isSecretInput answers whether a valuesFrom entry sets path from a Secret.
func isSecretInput(e map[string]any, path string) bool {
	kind, _ := e["kind"].(string)
	target, _ := e["targetPath"].(string)
	return kind == "Secret" && target != "" && (path == "" || target == path)
}

// SecretInputs maps every values path a Secret sets to where it is read from.
func (a *ChartAddon) SecretInputs() map[string]SecretRef {
	out := map[string]SecretRef{}
	for _, e := range a.valuesFrom {
		if !isSecretInput(e, "") {
			continue
		}
		target, _ := e["targetPath"].(string)
		name, _ := e["name"].(string)
		key, _ := e["valuesKey"].(string)
		if key == "" {
			key = "values.yaml"
		}
		if _, seen := out[target]; !seen {
			out[target] = SecretRef{Name: name, Key: key}
		}
	}
	return out
}

// SetSecretInput points a values path at a key of a Secret. The path's entry
// keeps its place and every field besides name and valuesKey; a path without
// one gets a new entry at the end.
func (a *ChartAddon) SetSecretInput(path string, ref SecretRef) {
	kept := a.valuesFrom[:0:0]
	set := false
	for _, e := range a.valuesFrom {
		if isSecretInput(e, path) {
			if set {
				continue // one entry per path
			}
			e["name"], e["valuesKey"] = ref.Name, ref.Key
			set = true
		}
		kept = append(kept, e)
	}
	if !set {
		kept = append(kept, map[string]any{"kind": "Secret", "name": ref.Name, "valuesKey": ref.Key, "targetPath": path})
	}
	a.valuesFrom = kept
}

// ClearSecretInput removes the Secret entries that set a values path.
func (a *ChartAddon) ClearSecretInput(path string) {
	kept := a.valuesFrom[:0:0]
	for _, e := range a.valuesFrom {
		if !isSecretInput(e, path) {
			kept = append(kept, e)
		}
	}
	a.valuesFrom = kept
}

type chartAddonObject struct {
	Metadata struct {
		Name              string            `json:"name"`
		Generation        int64             `json:"generation"`
		ResourceVersion   string            `json:"resourceVersion"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Chart   ChartSource     `json:"chart"`
		Values  json.RawMessage `json:"values"`
		Suspend bool            `json:"suspend"`
	} `json:"spec"`
	Status struct {
		Phase              string           `json:"phase"`
		Message            string           `json:"message"`
		ObservedGeneration int64            `json:"observedGeneration"`
		LastAppliedChart   *ChartSource     `json:"lastAppliedChart"`
		Plan               json.RawMessage  `json:"plan"`
		Components         []ChartComponent `json:"components"`
	} `json:"status"`
}

func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

func parseChartAddon(raw []byte) (ChartAddon, error) {
	var o chartAddonObject
	if err := json.Unmarshal(raw, &o); err != nil {
		return ChartAddon{}, fmt.Errorf("decode ZaentrumAddon: %w", err)
	}
	// Numbers stay json.Number, so a large integer in values is written back
	// exactly as it was read.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return ChartAddon{}, fmt.Errorf("decode ZaentrumAddon: %w", err)
	}
	a := ChartAddon{
		Name: o.Metadata.Name, Generation: o.Metadata.Generation, ResourceVersion: o.Metadata.ResourceVersion,
		Deleting:   o.Metadata.DeletionTimestamp != nil,
		KeepValues: o.Metadata.Annotations[AnnotationKeepValues] == "true",
		Chart:      o.Spec.Chart, Suspend: o.Spec.Suspend,
		Phase: o.Status.Phase, Message: o.Status.Message, ObservedGeneration: o.Status.ObservedGeneration,
		LastAppliedChart: o.Status.LastAppliedChart, Components: o.Status.Components,
		obj: obj,
	}
	if spec, ok := obj["spec"].(map[string]any); ok {
		entries, _ := spec["valuesFrom"].([]any)
		for _, e := range entries {
			if m, ok := e.(map[string]any); ok {
				a.valuesFrom = append(a.valuesFrom, m)
			}
		}
	}
	if !isNull(o.Spec.Values) {
		a.Values = o.Spec.Values
	}
	if a.LastAppliedChart != nil && a.LastAppliedChart.Ref == "" {
		a.LastAppliedChart = nil // an empty struct is "never applied", not a chart
	}
	if !isNull(o.Status.Plan) {
		var p ChartPlan
		if err := json.Unmarshal(o.Status.Plan, &p); err == nil {
			a.PlanRaw, a.Plan = o.Status.Plan, &p
		}
	}
	return a, nil
}

// spec is the stored spec with the fields portal-api manages set from a.
func (a *ChartAddon) spec(current any) map[string]any {
	spec, _ := current.(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	chart, _ := spec["chart"].(map[string]any)
	if chart == nil {
		chart = map[string]any{}
	}
	chart["ref"] = a.Chart.Ref
	for k, v := range map[string]string{"version": a.Chart.Version, "digest": a.Chart.Digest} {
		if v == "" {
			delete(chart, k)
		} else {
			chart[k] = v
		}
	}
	spec["chart"] = chart
	if isNull(a.Values) {
		delete(spec, "values")
	} else {
		spec["values"] = a.Values
	}
	if len(a.valuesFrom) == 0 {
		delete(spec, "valuesFrom")
	} else {
		spec["valuesFrom"] = a.valuesFrom
	}
	spec["suspend"] = a.Suspend
	return spec
}

func (s *Service) addonResource() (group, version, plural string) {
	plural = s.cfg.AddonPlural
	if plural == "" {
		plural = "zaentrumaddons"
	}
	return s.cfg.OperatorGroup, s.cfg.OperatorVersion, plural
}

// ChartAddons lists the ZaentrumAddons in the namespace. A cluster without the
// resource type answers NotFound; a Role without it, Forbidden.
func (s *Service) ChartAddons(ctx context.Context) ([]ChartAddon, error) {
	g, v, p := s.addonResource()
	raw, err := s.k8s.GetResourceList(ctx, g, v, p)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode ZaentrumAddon list: %w", err)
	}
	out := make([]ChartAddon, 0, len(list.Items))
	for _, item := range list.Items {
		a, err := parseChartAddon(item)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ChartAddon reads one ZaentrumAddon.
func (s *Service) ChartAddon(ctx context.Context, name string) (*ChartAddon, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	g, v, p := s.addonResource()
	raw, err := s.k8s.GetResource(ctx, g, v, p, name)
	if err != nil {
		return nil, err
	}
	a, err := parseChartAddon(raw)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateChartAddon creates a ZaentrumAddon from a's name and managed spec, and
// updates a to the resource as created (its generation, among others). A dry
// run validates the create — everything the apiserver checks — and changes
// nothing, a included.
func (s *Service) CreateChartAddon(ctx context.Context, a *ChartAddon, dryRun bool) error {
	if err := validName(a.Name); err != nil {
		return err
	}
	g, v, p := s.addonResource()
	body, err := json.Marshal(map[string]any{
		"apiVersion": g + "/" + v,
		"kind":       "ZaentrumAddon",
		"metadata": map[string]any{
			"name":   a.Name,
			"labels": map[string]string{LabelAddon: a.Name},
		},
		"spec": a.spec(nil),
	})
	if err != nil {
		return err
	}
	raw, err := s.k8s.CreateResource(ctx, g, v, p, body, dryRun)
	if err != nil || dryRun {
		return err
	}
	return a.refresh(raw)
}

// refresh replaces a with the resource an apiserver answered a write with.
func (a *ChartAddon) refresh(raw []byte) error {
	written, err := parseChartAddon(raw)
	if err != nil {
		return err
	}
	*a = written
	return nil
}

// UpdateChartAddon writes a's managed spec over the resource as it was read,
// and updates a to the resource as written. A change made since the read is
// answered with a conflict (k8s.IsConflict): read again and redo the change,
// never overwrite. A dry run validates the update and changes nothing.
func (s *Service) UpdateChartAddon(ctx context.Context, a *ChartAddon, dryRun bool) error {
	if a.obj == nil {
		return errors.New("update of a ZaentrumAddon that was not read")
	}
	if err := validName(a.Name); err != nil {
		return err
	}
	a.obj["spec"] = a.spec(a.obj["spec"])
	body, err := json.Marshal(a.obj)
	if err != nil {
		return err
	}
	g, v, p := s.addonResource()
	raw, err := s.k8s.UpdateResource(ctx, g, v, p, a.Name, body, dryRun)
	if err != nil || dryRun {
		return err
	}
	return a.refresh(raw)
}

// DeleteChartAddon deletes a ZaentrumAddon. What it owns — workloads, and the
// Secrets with its values unless it carries the keep-values annotation — goes
// with it: the operator's finalizer and garbage collection see to that.
func (s *Service) DeleteChartAddon(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	g, v, p := s.addonResource()
	return s.k8s.DeleteResource(ctx, g, v, p, name)
}

// SetKeepValues sets or removes the keep-values annotation of a ZaentrumAddon:
// a metadata patch that leaves its spec, and so its generation, alone.
func (s *Service) SetKeepValues(ctx context.Context, name string, keep bool) error {
	if err := validName(name); err != nil {
		return err
	}
	var value any // null removes the annotation
	if keep {
		value = "true"
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnotationKeepValues: value}},
	})
	if err != nil {
		return err
	}
	g, v, p := s.addonResource()
	_, err = s.k8s.PatchResource(ctx, g, v, p, name, patch)
	return err
}

// CreateValuesSecret writes secret inputs into a new immutable Secret of the
// addon — keyed by values path, labelled for the addon — and returns the name
// the apiserver generated for it. It never writes to a Secret that exists.
func (s *Service) CreateValuesSecret(ctx context.Context, addon string, values map[string]string) (string, error) {
	if err := validName(addon); err != nil {
		return "", err
	}
	if len(values) == 0 {
		return "", errors.New("a values Secret without values")
	}
	data := make(map[string]string, len(values))
	for k, v := range values {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"immutable":  true,
		"metadata": map[string]any{
			"generateName": ValuesSecretPrefix(addon),
			"labels":       map[string]string{LabelAddon: addon},
		},
		"data": data,
	})
	if err != nil {
		return "", err
	}
	return s.k8s.CreateSecret(ctx, body)
}
