package operator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
)

const addons = "zaentrumaddons"

func chartService(t *testing.T) (*Service, *k8sfake.Server) {
	t.Helper()
	fake := k8sfake.New(t)
	cfg := config.Config{OperatorGroup: "zaentrum.io", OperatorVersion: "v1alpha1", AddonPlural: addons}
	return New(fake.Client("zaentrum"), cfg), fake
}

func TestChartAddonRoundTripKeepsUnknownFields(t *testing.T) {
	svc, fake := chartService(t)
	ctx := context.Background()
	fake.Put(addons, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "ZaentrumAddon",
		"metadata": map[string]any{"name": "example", "annotations": map[string]any{"note": "kept"}},
		"spec": map[string]any{
			"chart":  map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.0.0", "future": "kept"},
			"values": map[string]any{"replicas": 12345678901234567, "worker": map[string]any{"enabled": true}},
			"valuesFrom": []any{
				map[string]any{"kind": "ConfigMap", "name": "shared", "valuesKey": "values.yaml"},
			},
			"interval": "5m",
		},
	})
	fake.SetStatus(addons, "example", map[string]any{
		"phase": "Planned", "observedGeneration": 1,
		"plan": map[string]any{
			"chart":        map[string]any{"name": "example", "version": "1.0.0", "annotations": map[string]any{AnnotationPrimary: "example"}},
			"valuesSchema": `{"type":"object"}`,
			"valuesErrors": []any{"config.password is required"},
			"objects":      []any{map[string]any{"kind": "Service", "name": "example"}},
		},
		"components": []any{map[string]any{"name": "example-worker", "kind": "Deployment", "ready": 0, "desired": 1}},
	})

	a, err := svc.ChartAddon(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !a.PlanCurrent() || a.Primary() != "example" || len(a.Plan.ValuesErrors) != 1 || a.LastAppliedChart != nil || a.Deleting || a.KeepValues {
		t.Errorf("status = %+v plan=%+v", a, a.Plan)
	}
	if !a.Plan.Renders("Service", "example") || a.Plan.Renders("Deployment", "example") {
		t.Errorf("plan objects = %+v", a.Plan.Objects)
	}
	if a.Chart.Version != "1.0.0" || len(a.ValuesFrom()) != 1 || a.Components[0].Desired != 1 {
		t.Errorf("decoded = %+v", a)
	}

	a.Chart.Version = "1.1.0"
	a.Suspend = true
	a.SetSecretInput("config.password", SecretRef{Name: "zaentrum-addon-example-values-abcde", Key: "config.password"})
	if err := svc.UpdateChartAddon(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	got := fake.Object(addons, "example")
	spec := got["spec"].(map[string]any)
	if spec["interval"] != "5m" || spec["chart"].(map[string]any)["future"] != "kept" {
		t.Errorf("fields portal-api does not manage must survive an update: %v", spec)
	}
	if got["metadata"].(map[string]any)["annotations"].(map[string]any)["note"] != "kept" {
		t.Errorf("metadata must survive: %v", got["metadata"])
	}
	b, _ := json.Marshal(spec["values"])
	if !strings.Contains(string(b), "12345678901234567") {
		t.Errorf("a large integer must be written back exactly: %s", b)
	}
	if spec["suspend"] != true || spec["chart"].(map[string]any)["version"] != "1.1.0" || len(spec["valuesFrom"].([]any)) != 2 {
		t.Errorf("managed fields = %v", spec)
	}
	if gen := fake.Generation(addons, "example"); gen != 2 || a.Generation != 2 || a.ObservedGeneration != 1 {
		t.Errorf("a spec change moves the generation: stored %v, answered %d/%d", gen, a.Generation, a.ObservedGeneration)
	}
	stale, _ := svc.ChartAddon(ctx, "example")
	fake.SetStatus(addons, "example", map[string]any{"phase": "Planned"})
	// The read is stale now: an update from it is a conflict, never a blind write.
	if err := svc.UpdateChartAddon(ctx, stale, false); !k8s.IsConflict(err) {
		t.Errorf("stale update = %v, want a conflict", err)
	}
}

// Secret inputs are valuesFrom entries portal-api edits one path at a time;
// every other entry — its place and its fields — stays as it was.
func TestSecretInputsLeaveOtherEntriesAlone(t *testing.T) {
	svc, fake := chartService(t)
	ctx := context.Background()
	fake.Put(addons, map[string]any{
		"metadata": map[string]any{"name": "example"},
		"spec": map[string]any{
			"chart": map[string]any{"ref": "oci://registry.example.org/charts/example", "version": "1.0.0"},
			"valuesFrom": []any{
				map[string]any{"kind": "Secret", "name": "shared", "valuesKey": "values.yaml", "optional": true},
				map[string]any{"kind": "Secret", "name": "gitops-token", "valuesKey": "token", "targetPath": "config.token", "optional": true, "future": "kept"},
				map[string]any{"kind": "ConfigMap", "name": "tuning", "valuesKey": "replicas", "targetPath": "worker.replicas"},
				map[string]any{"kind": "Secret", "name": "zaentrum-addon-example-values-old", "valuesKey": "config.password", "targetPath": "config.password"},
			},
		},
	})
	a, err := svc.ChartAddon(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	inputs := a.SecretInputs()
	if len(inputs) != 2 || inputs["config.token"] != (SecretRef{Name: "gitops-token", Key: "token"}) ||
		inputs["config.password"].Name != "zaentrum-addon-example-values-old" {
		t.Errorf("secret inputs = %v — a ConfigMap and a whole values document are not secret inputs", inputs)
	}
	a.SetSecretInput("config.token", SecretRef{Name: "zaentrum-addon-example-values-new", Key: "config.token"})
	a.SetSecretInput("database.url", SecretRef{Name: "zaentrum-addon-example-values-new", Key: "database.url"})
	a.ClearSecretInput("config.password")
	a.ClearSecretInput("worker.replicas") // a ConfigMap entry is no secret input to clear
	if err := svc.UpdateChartAddon(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(fake.Object(addons, "example")["spec"].(map[string]any)["valuesFrom"])
	want := `[{"kind":"Secret","name":"shared","optional":true,"valuesKey":"values.yaml"},` +
		`{"future":"kept","kind":"Secret","name":"zaentrum-addon-example-values-new","optional":true,"targetPath":"config.token","valuesKey":"config.token"},` +
		`{"kind":"ConfigMap","name":"tuning","targetPath":"worker.replicas","valuesKey":"replicas"},` +
		`{"kind":"Secret","name":"zaentrum-addon-example-values-new","targetPath":"database.url","valuesKey":"database.url"}]`
	if string(b) != want {
		t.Errorf("valuesFrom =\n%s\nwant\n%s", b, want)
	}
}

func TestCreateDryRunAndDeleteChartAddon(t *testing.T) {
	svc, fake := chartService(t)
	ctx := context.Background()
	a := &ChartAddon{
		Name:   "example",
		Chart:  ChartSource{Ref: "https://charts.example.org/example-1.2.0.tgz"},
		Values: json.RawMessage(`{"worker":{"replicas":2}}`), Suspend: true,
	}
	a.SetSecretInput("config.password", SecretRef{Name: "zaentrum-addon-example-values-abcde", Key: "config.password"})

	// A dry run is validated like the real thing and stores nothing.
	fake.Validate = func(_ string, obj map[string]any) error {
		for _, e := range obj["spec"].(map[string]any)["valuesFrom"].([]any) {
			if len(e.(map[string]any)["targetPath"].(string)) > 250 {
				return errors.New("spec.valuesFrom[0].targetPath: Too long: may not be more than 250 bytes")
			}
		}
		return nil
	}
	if err := svc.CreateChartAddon(ctx, a, true); err != nil || fake.Object(addons, "example") != nil || a.Generation != 0 {
		t.Fatalf("dry run = %v, stored %v, addon %+v", err, fake.Object(addons, "example"), a)
	}
	long := *a
	long.SetSecretInput(strings.Repeat("p", 251), SecretRef{Name: "x", Key: "y"})
	var ae *k8s.APIError
	if err := svc.CreateChartAddon(ctx, &long, true); !errors.As(err, &ae) || ae.Code != 422 {
		t.Errorf("an invalid dry run = %v, want 422", err)
	}

	if err := svc.CreateChartAddon(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if a.Generation != 1 || a.ObservedGeneration != 0 || a.ResourceVersion == "" || a.Chart.Ref != "https://charts.example.org/example-1.2.0.tgz" {
		t.Errorf("a create must leave the addon as created: %+v", a)
	}
	got := fake.Object(addons, "example")
	if got["kind"] != "ZaentrumAddon" || got["apiVersion"] != "zaentrum.io/v1alpha1" {
		t.Errorf("created = %v", got)
	}
	spec := got["spec"].(map[string]any)
	chart := spec["chart"].(map[string]any)
	if _, ok := chart["version"]; ok || chart["ref"] != a.Chart.Ref || spec["suspend"] != true {
		t.Errorf("spec = %v — empty version and digest are omitted", spec)
	}
	// A dry-run update changes nothing either.
	a.Suspend = false
	if err := svc.UpdateChartAddon(ctx, a, true); err != nil || fake.Object(addons, "example")["spec"].(map[string]any)["suspend"] != true {
		t.Errorf("dry-run update = %v", err)
	}

	list, err := svc.ChartAddons(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "example" || list[0].Plan != nil {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := svc.SetKeepValues(ctx, "example", true); err != nil {
		t.Fatal(err)
	}
	kept, _ := svc.ChartAddon(ctx, "example")
	if !kept.KeepValues || kept.Generation != 1 {
		t.Errorf("keep-values is metadata: kept=%v generation=%d", kept.KeepValues, kept.Generation)
	}
	if err := svc.SetKeepValues(ctx, "example", false); err != nil {
		t.Fatal(err)
	}
	if again, _ := svc.ChartAddon(ctx, "example"); again.KeepValues {
		t.Error("the annotation must be removable")
	}
	if err := svc.DeleteChartAddon(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteChartAddon(ctx, "example"); !k8s.IsNotFound(err) {
		t.Errorf("deleting twice = %v", err)
	}
	fake.Unserved[addons] = true
	if _, err := svc.ChartAddons(ctx); !k8s.IsNotFound(err) {
		t.Errorf("a cluster without the resource type = %v, want NotFound", err)
	}
}

// Every write of secret inputs is a new immutable Secret, created from a
// generateName; k8sfake fails the test on anything but a create.
func TestCreateValuesSecret(t *testing.T) {
	svc, fake := chartService(t)
	ctx := context.Background()
	first, err := svc.CreateValuesSecret(ctx, "example", map[string]string{"config.password": "s3cret", "database.url": "postgres://db"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateValuesSecret(ctx, "example", map[string]string{"config.password": "changed"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "zaentrum-addon-example-values-") || !strings.HasPrefix(second, "zaentrum-addon-example-values-") {
		t.Fatalf("names = %q, %q", first, second)
	}
	sec := fake.Secret(first)
	md := sec["metadata"].(map[string]any)
	if sec["immutable"] != true || sec["type"] != "Opaque" || md["labels"].(map[string]any)["zaentrum.io/addon"] != "example" {
		t.Errorf("secret = %v", sec)
	}
	data := sec["data"].(map[string]any)
	if len(data) != 2 || data["config.password"] != base64.StdEncoding.EncodeToString([]byte("s3cret")) {
		t.Errorf("data = %v", data)
	}
	if got := fake.Secret(second)["data"].(map[string]any); len(got) != 1 {
		t.Errorf("a write holds only its own inputs: %v", got)
	}
	for _, c := range fake.Calls() {
		if strings.Contains(c.Path, "/secrets") && (c.Method != "POST" || !strings.Contains(c.Accept, "PartialObjectMetadata")) {
			t.Errorf("unexpected secret call %+v", c)
		}
	}
	if _, err := svc.CreateValuesSecret(ctx, "example", nil); err == nil {
		t.Error("a Secret without values must not be created")
	}
}
