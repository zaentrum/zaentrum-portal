package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// Registering chart addons.
//
// The operator installs a chart addon's workloads; the registry learns what the
// addon contributes the way it learns it for any addon — from the capability
// manifest its primary Service serves (ADR-0009/0010). A loop in portal-api
// does that. Every registrationEvery, and whenever an install (or a read of a
// ready addon) kicks it, it lists the ZaentrumAddons and, for each one the
// operator reports Ready, reads the manifest at http://<zaentrum.io/primary>
// and runs the install when the registry does not hold the addon, holds
// another manifest, or holds another chart. The chart is recorded as the
// addon's source. A chart addon whose resource is gone — deleted through a
// deploy repository, say — is unregistered.

const registrationEvery = 15 * time.Second

// regStep is one addon the loop looks at in a pass.
type regStep struct {
	name       string
	unregister bool // the resource is gone; its registry rows go too
}

// registrationSteps decides a pass from the resources and the registry:
// register every ready addon, unregister every chart addon without a
// resource. Whether a ready addon's registration changes anything is decided
// once its manifest is read (needsRegistration).
func registrationSteps(items []operator.ChartAddon, registered []model.Addon) []regStep {
	present := map[string]bool{}
	var out []regStep
	for _, it := range items {
		present[it.Name] = true
		if it.Phase == operator.ChartPhaseReady {
			out = append(out, regStep{name: it.Name})
		}
	}
	for _, ad := range registered {
		if ad.ChartRef != "" && !present[ad.Key] {
			out = append(out, regStep{name: ad.Key, unregister: true})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// needsRegistration: the registry lacks the addon, or holds another manifest,
// or another chart than the one running.
func needsRegistration(reg *model.Addon, manifestSHA string, chart operator.ChartSource) bool {
	return reg == nil || reg.ManifestSHA256 != manifestSHA ||
		reg.ChartRef != chart.Ref || reg.ChartVersion != chart.Version
}

// runningChart is the chart an addon runs: the one the operator last applied,
// or the spec's while the operator reports none.
func runningChart(ca operator.ChartAddon) operator.ChartSource {
	if ca.LastAppliedChart != nil {
		return *ca.LastAppliedChart
	}
	return ca.Chart
}

// RunAddonRegistration keeps chart addons registered until ctx ends. Outside a
// cluster there is nothing to register, and it returns at once.
func (a *API) RunAddonRegistration(ctx context.Context) {
	if a.charts == nil || !a.charts.Available() {
		return
	}
	ticker := time.NewTicker(registrationEvery)
	defer ticker.Stop()
	for {
		a.syncChartAddons(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.registration.kick:
		}
	}
}

// syncChartAddons is one pass of the loop.
func (a *API) syncChartAddons(ctx context.Context) {
	items, err := a.charts.ChartAddons(ctx)
	if err != nil {
		// No resource type, no permission or no answer: nothing to register
		// now — and nothing is unregistered on a read that failed.
		a.setListError(err)
		return
	}
	a.setListError(nil)
	registered, err := a.addons.ListAddons(ctx)
	if err != nil {
		log.Printf("addons: registration cannot read the registry: %v", err)
		return
	}
	byName := make(map[string]operator.ChartAddon, len(items))
	for _, it := range items {
		byName[it.Name] = it
	}
	for _, step := range registrationSteps(items, registered) {
		if ctx.Err() != nil {
			return
		}
		if step.unregister {
			a.unregisterChartAddon(ctx, step.name)
			continue
		}
		a.setRegistrationError(step.name, a.registerChartAddon(ctx, byName[step.name]))
	}
}

// registerChartAddon runs the install for a ready chart addon from its primary
// Service, when the registry does not hold what runs.
func (a *API) registerChartAddon(ctx context.Context, ca operator.ChartAddon) error {
	primary := ca.Primary()
	if !isDNSLabel(primary) {
		return fmt.Errorf("the chart does not name its primary Service: its %s annotation is %q", operator.AnnotationPrimary, primary)
	}
	proxyURL := "http://" + primary
	fetch := a.registration.fetch
	if fetch == nil {
		fetch = fetchManifest
	}
	// Read outside the lock: an addon that does not answer holds up nobody.
	d, err := fetch(ctx, proxyURL)
	if err != nil {
		return err
	}
	if service := strings.TrimSpace(d.Service); service != ca.Name {
		return fmt.Errorf("the manifest at %s names service %q, but the addon is installed as %q — a chart's release name must be its manifest's service",
			proxyURL, service, ca.Name)
	}
	_, sum := canonicalManifest(d)
	chart := runningChart(ca)

	a.registration.mu.Lock()
	defer a.registration.mu.Unlock()
	// Removed, or no longer ready, while the manifest was read: not now.
	current, err := a.charts.ChartAddon(ctx, ca.Name)
	switch {
	case k8s.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case current.Phase != operator.ChartPhaseReady:
		return nil
	}
	var reg registered
	if reg.addon, err = a.addons.GetAddon(ctx, ca.Name); errors.Is(err, store.ErrNotFound) {
		reg.addon = nil
	} else if err != nil {
		return err
	}
	if !needsRegistration(reg.addon, sum, chart) {
		return nil
	}
	plan, err := planAddon(proxyURL, d, a.defaultSpace(ctx), a.publicOrigin(ctx))
	if err != nil {
		return err
	}
	plan.Components = chartComponents(*current, plan.Components)
	if reg.app, err = a.addons.GetApp(ctx, plan.App.Key); errors.Is(err, store.ErrNotFound) {
		reg.app = nil
	} else if err != nil {
		return err
	}
	claims, err := a.addons.WorkloadClaims(ctx)
	if err != nil {
		return err
	}
	live, known := a.liveWorkloads(ctx)
	if !known && a.inCluster() {
		return errors.New("the platform cannot list its workloads right now, so it cannot verify that no component is a platform deployment — retrying")
	}
	// A chart addon's primary is wherever its chart puts it: a new address
	// from a new chart is no move an admin has to confirm. An addon added by
	// address under the same key still is.
	moves := reg.addon != nil && reg.addon.ChartRef != ""
	if err := installConflict(plan, reg, moves, platformWorkloads(live), claims); err != nil {
		return err
	}
	in := plan.install()
	in.Addon.ChartRef, in.Addon.ChartVersion = chart.Ref, chart.Version
	if err := a.addons.InstallAddon(ctx, in); err != nil {
		return err
	}
	invalidateDiscovery()
	log.Printf("addons: registered %s from chart %s %s", ca.Name, chart.Ref, chart.Version)
	return nil
}

// unregisterChartAddon removes the registry rows of a chart addon whose
// resource is gone.
func (a *API) unregisterChartAddon(ctx context.Context, name string) {
	a.registration.mu.Lock()
	defer a.registration.mu.Unlock()
	// Gone for certain — read again under the lock, so an addon added a moment
	// ago keeps its rows.
	if _, err := a.charts.ChartAddon(ctx, name); !k8s.IsNotFound(err) {
		return
	}
	removed, err := a.removeChartRegistration(ctx, name)
	if err != nil {
		log.Printf("addons: cannot unregister %s, whose ZaentrumAddon is gone: %v", name, err)
		return
	}
	if removed != nil {
		log.Printf("addons: unregistered %s — its ZaentrumAddon is gone", name)
	}
}

// setRegistrationError records how an addon's registration went, logging a
// change only: the loop runs every few seconds.
func (a *API) setRegistrationError(name string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.registration.stateMu.Lock()
	defer a.registration.stateMu.Unlock()
	if a.registration.errors == nil {
		a.registration.errors = map[string]string{}
	}
	if a.registration.errors[name] == msg {
		return
	}
	if msg == "" {
		delete(a.registration.errors, name)
		return
	}
	a.registration.errors[name] = msg
	log.Printf("addons: cannot register %s: %s", name, msg)
}

// setListError logs a failure to list the addons once, and its end.
func (a *API) setListError(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.registration.stateMu.Lock()
	defer a.registration.stateMu.Unlock()
	if msg == a.registration.listError {
		return
	}
	switch {
	case msg == "":
		log.Printf("addons: ZaentrumAddons can be listed again")
	case k8s.IsNotFound(err):
		log.Printf("addons: this cluster serves no ZaentrumAddon resource — chart addons are not registered")
	default:
		log.Printf("addons: cannot list ZaentrumAddons: %s", msg)
	}
	a.registration.listError = msg
}

// publicOrigin is the portal's public origin for the slot URLs a registration
// writes: the platform's hostname when its Zaentrum resource names one, else
// the origin an admin last reached the chart endpoints on, else none — the
// URLs then stay relative.
func (a *API) publicOrigin(ctx context.Context) string {
	if a.charts != nil {
		if info, err := a.charts.OperatorInfo(ctx); err == nil && info.Present && info.Hostname != "" {
			return "https://" + info.Hostname
		}
	}
	a.registration.stateMu.Lock()
	defer a.registration.stateMu.Unlock()
	return a.registration.origin
}

// ─── the addons list ─────────────────────────────────────────────────────────

// chartInfo is where a chart addon comes from, in the addons list.
type chartInfo struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
	Digest  string `json:"digest,omitempty"`
	// LastApplied is the chart running; nil while nothing was applied.
	LastApplied *operator.ChartSource `json:"lastApplied"`
}

// chartAddonsByName lists the ZaentrumAddons for a read; nil when they cannot
// be listed, which leaves the addons list as the registry has it.
func (a *API) chartAddonsByName(ctx context.Context) map[string]operator.ChartAddon {
	if a.charts == nil || !a.charts.Available() {
		return nil
	}
	items, err := a.charts.ChartAddons(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]operator.ChartAddon, len(items))
	for _, it := range items {
		out[it.Name] = it
	}
	return out
}

// withChart merges a ZaentrumAddon into an addons list row.
func (row *installedAddon) withChart(ca operator.ChartAddon) {
	row.Chart = &chartInfo{Ref: ca.Chart.Ref, Version: ca.Chart.Version, Digest: ca.Chart.Digest, LastApplied: ca.LastAppliedChart}
	row.Phase, row.Suspended = ca.Phase, ca.Suspend
	row.RefreshAvailable = false // the registration loop refreshes a chart addon
}

// chartComponents are the components a chart addon is registered with, named
// the way the operator labels its workloads (zaentrum.io/component): by
// Deployment name. They are the Deployments the addon's status reports — the
// one the chart's primary annotation names is the primary, every other one
// required — with the summary the manifest gives a workload it declares.
// Without components in the status, the manifest's components stand in, named
// and ranked the same way. The primary serves the manifest, so it is always
// one.
func chartComponents(ca operator.ChartAddon, declared []model.AddonComponent) []model.AddonComponent {
	byWorkload := make(map[string]model.AddonComponent, len(declared))
	for _, c := range declared {
		byWorkload[c.Workload] = c
	}
	primary := ca.Primary()
	seen := map[string]bool{}
	var out []model.AddonComponent
	add := func(workload string) {
		if seen[workload] || !isDNSLabel(workload) {
			return
		}
		seen[workload] = true
		role := roleRequired
		if workload == primary {
			role = rolePrimary
		}
		out = append(out, model.AddonComponent{Name: workload, Workload: workload, Role: role, Summary: byWorkload[workload].Summary})
	}
	add(primary)
	for _, c := range ca.Components {
		add(c.Name)
	}
	if len(ca.Components) == 0 {
		for _, c := range declared {
			add(c.Workload)
		}
	}
	for i := range out {
		out[i].Order = i
	}
	return out
}

// workloadTopics maps each workload a manifest declares to its topics.
func workloadTopics(d Descriptor) map[string][]string {
	byName := componentTopics(d)
	out := map[string][]string{}
	for _, c := range d.Components {
		if topics := byName[strings.TrimSpace(c.Name)]; len(topics) > 0 {
			out[strings.TrimSpace(c.Workload)] = topics
		}
	}
	return out
}

// chartComponentViews are a chart addon's components as its status reports
// them, named by Deployment: the workload the chart names primary is the
// primary, every other one required. The live workload state wins when the
// platform can see it; summaries come from the registered components, topics
// (by workload) from the manifest.
func chartComponentViews(ca operator.ChartAddon, declared []model.AddonComponent, topics map[string][]string, live map[string]operator.Instance, known bool) []componentView {
	byWorkload := make(map[string]model.AddonComponent, len(declared))
	for _, c := range declared {
		byWorkload[c.Workload] = c
	}
	primary := ca.Primary()
	out := make([]componentView, 0, len(ca.Components))
	for _, c := range ca.Components {
		v := componentView{Name: c.Name, Workload: c.Name, Role: roleRequired}
		if c.Name == primary {
			v.Role = rolePrimary
		}
		v.Summary, v.Topics = byWorkload[c.Name].Summary, topics[c.Name]
		if in, ok := live[c.Name]; known && ok {
			v.Phase, v.Reason = ptr(in.Phase), ptr(in.Reason)
			v.Ready, v.Desired, v.Restarts = ptr(in.ReadyReplicas), ptr(in.DesiredReplicas), ptr(in.Restarts)
		} else {
			v.Phase, v.Ready, v.Desired, v.Reason = ptr(chartComponentPhase(c)), ptr(c.Ready), ptr(c.Desired), ptr(c.Reason)
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Role == rolePrimary && out[j].Role != rolePrimary })
	return out
}

// chartComponentPhase is a component's phase from its status counters, in the
// operator console's words.
func chartComponentPhase(c operator.ChartComponent) string {
	switch {
	case c.Desired == 0:
		return "stopped"
	case c.Ready >= c.Desired && c.Reason == "":
		return "ready"
	case c.Ready == 0:
		return "degraded"
	default:
		return "progressing"
	}
}

// chartOnlyRow is an addons list row for a chart addon the registry does not
// hold yet: planned, installing, failed — or ready and about to be registered.
func (a *API) chartOnlyRow(ca operator.ChartAddon, live map[string]operator.Instance, known bool) installedAddon {
	row := installedAddon{Key: ca.Name, Title: ca.Name, RegistrationError: a.registrationError(ca.Name)}
	if ca.Plan != nil {
		switch {
		case ca.Plan.Chart.Annotations[operator.AnnotationTitle] != "":
			row.Title = ca.Plan.Chart.Annotations[operator.AnnotationTitle]
		case ca.Plan.Chart.Name != "":
			row.Title = ca.Plan.Chart.Name
		}
	}
	row.Components = chartComponentViews(ca, nil, nil, live, known)
	row.withChart(ca)
	return row
}
