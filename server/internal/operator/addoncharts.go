package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

// Chart addons.
//
// An addon can be a standard Helm chart that the zaentrum-operator installs
// from one ZaentrumAddon resource in the platform namespace: the chart
// reference, non-secret values, and valuesFrom entries naming where the secret
// inputs are kept. portal-api writes that resource and the addon's values
// Secret. It never renders a chart and never applies a workload — the operator
// does both — and it never reads a Secret back: nothing needs the values once
// they are written, so nothing here can fetch them.

const (
	// AnnotationPrimary, in a chart's Chart.yaml, names the Service (port 80)
	// that serves the addon's capability manifest.
	AnnotationPrimary = "zaentrum.io/primary"
	// AnnotationTitle is a chart's optional display title.
	AnnotationTitle = "zaentrum.io/title"
	// LabelKeep on a values Secret keeps it when its addon is removed.
	LabelKeep = "zaentrum.io/keep"
)

// ChartPhaseReady is the phase of an applied addon whose workloads are ready.
const ChartPhaseReady = "Ready"

// ValuesSecretName is the Secret holding an addon's secret inputs, one key per
// dotted values path.
func ValuesSecretName(addon string) string { return "zaentrum-addon-" + addon + "-values" }

// GeneratedSecretName is the Secret the operator keeps the values it generated
// for an addon in.
func GeneratedSecretName(addon string) string { return "zaentrum-addon-" + addon + "-generated" }

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
	ValuesSchema string   `json:"valuesSchema"`
	ValuesErrors []string `json:"valuesErrors"`
	Violations   []string `json:"violations"`
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

	// spec — the fields portal-api manages.
	Chart      ChartSource
	Values     json.RawMessage // spec.values; empty when unset
	ValuesFrom []ValuesFrom
	Suspend    bool

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

type chartAddonObject struct {
	Metadata struct {
		Name            string `json:"name"`
		Generation      int64  `json:"generation"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		Chart      ChartSource     `json:"chart"`
		Values     json.RawMessage `json:"values"`
		ValuesFrom []ValuesFrom    `json:"valuesFrom"`
		Suspend    bool            `json:"suspend"`
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
		Chart: o.Spec.Chart, ValuesFrom: o.Spec.ValuesFrom, Suspend: o.Spec.Suspend,
		Phase: o.Status.Phase, Message: o.Status.Message, ObservedGeneration: o.Status.ObservedGeneration,
		LastAppliedChart: o.Status.LastAppliedChart, Components: o.Status.Components,
		obj: obj,
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
	if len(a.ValuesFrom) == 0 {
		delete(spec, "valuesFrom")
	} else {
		spec["valuesFrom"] = a.ValuesFrom
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
// updates a to the resource as created (its generation, among others).
func (s *Service) CreateChartAddon(ctx context.Context, a *ChartAddon) error {
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
	raw, err := s.k8s.CreateResource(ctx, g, v, p, body)
	if err != nil {
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
// never overwrite.
func (s *Service) UpdateChartAddon(ctx context.Context, a *ChartAddon) error {
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
	raw, err := s.k8s.UpdateResource(ctx, g, v, p, a.Name, body)
	if err != nil {
		return err
	}
	return a.refresh(raw)
}

// DeleteChartAddon deletes a ZaentrumAddon. What it owns — workloads, the
// generated Secret, a values Secret without the keep label — goes with it by
// garbage collection.
func (s *Service) DeleteChartAddon(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	g, v, p := s.addonResource()
	return s.k8s.DeleteResource(ctx, g, v, p, name)
}

// WriteAddonSecrets sets and clears keys in an addon's values Secret without
// reading it: one merge patch when the Secret exists, a create when it does
// not. Writing to a kept Secret makes it the addon's again (the keep label is
// a decision made at removal, not a property of the values).
func (s *Service) WriteAddonSecrets(ctx context.Context, addon string, set map[string]string, clear []string) error {
	if err := validName(addon); err != nil {
		return err
	}
	name := ValuesSecretName(addon)
	data := map[string]any{}
	for _, k := range clear {
		data[k] = nil
	}
	for k, v := range set {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	if len(data) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": map[string]any{LabelAddon: addon, LabelKeep: nil}},
		"data":     data,
	})
	if err != nil {
		return err
	}
	switch err := s.k8s.PatchSecret(ctx, name, patch); {
	case err == nil:
		return nil
	case !k8s.IsNotFound(err):
		return err
	case len(set) == 0:
		return nil // only clearing keys of a Secret that does not exist
	}
	created := map[string]string{}
	for k, v := range set {
		created[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	obj, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]string{LabelAddon: addon},
		},
		"data": created,
	})
	if err != nil {
		return err
	}
	if err := s.k8s.CreateSecret(ctx, obj); k8s.IsConflict(err) {
		return s.k8s.PatchSecret(ctx, name, patch) // created meanwhile
	} else if err != nil {
		return err
	}
	return nil
}

// KeepAddonSecrets makes an addon's values outlive the addon — the Secret
// with its secret inputs and the one with the values the operator generated:
// the keep label, so the operator stops owning them, and no owner reference,
// so garbage collection has nothing to follow when the addon goes. A missing
// Secret has nothing to keep.
func (s *Service) KeepAddonSecrets(ctx context.Context, addon string) error {
	if err := validName(addon); err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"labels":          map[string]string{LabelKeep: "true"},
			"ownerReferences": nil,
		},
	})
	if err != nil {
		return err
	}
	for _, name := range []string{ValuesSecretName(addon), GeneratedSecretName(addon)} {
		if err := s.k8s.PatchSecret(ctx, name, patch); err != nil && !k8s.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// DeleteAddonSecrets deletes an addon's values Secret and the one with its
// generated values, whichever exist — also when a removal that kept them left
// them without an owner for garbage collection to follow.
func (s *Service) DeleteAddonSecrets(ctx context.Context, addon string) error {
	if err := validName(addon); err != nil {
		return err
	}
	for _, name := range []string{ValuesSecretName(addon), GeneratedSecretName(addon)} {
		if err := s.k8s.DeleteSecret(ctx, name); err != nil && !k8s.IsNotFound(err) {
			return err
		}
	}
	return nil
}
