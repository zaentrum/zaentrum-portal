package operator

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
		},
		"components": []any{map[string]any{"name": "example-worker", "kind": "Deployment", "ready": 0, "desired": 1}},
	})

	a, err := svc.ChartAddon(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !a.PlanCurrent() || a.Primary() != "example" || len(a.Plan.ValuesErrors) != 1 || a.LastAppliedChart != nil {
		t.Errorf("status = %+v plan=%+v", a, a.Plan)
	}
	if a.Chart.Version != "1.0.0" || len(a.ValuesFrom) != 1 || a.Components[0].Desired != 1 {
		t.Errorf("decoded = %+v", a)
	}

	a.Chart.Version = "1.1.0"
	a.Chart.Digest = ""
	a.Suspend = true
	a.ValuesFrom = append(a.ValuesFrom, ValuesFrom{Kind: "Secret", Name: ValuesSecretName("example"), ValuesKey: "config.password", TargetPath: "config.password"})
	if err := svc.UpdateChartAddon(ctx, a); err != nil {
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
	if gen := fake.Generation(addons, "example"); gen != 2 {
		t.Errorf("a spec change moves the generation: %v", gen)
	}

	// The read is stale now: an update from it is a conflict, never a blind write.
	if err := svc.UpdateChartAddon(ctx, a); !k8s.IsConflict(err) {
		t.Errorf("stale update = %v, want a conflict", err)
	}
}

func TestCreateAndDeleteChartAddon(t *testing.T) {
	svc, fake := chartService(t)
	ctx := context.Background()
	a := &ChartAddon{
		Name:   "example",
		Chart:  ChartSource{Ref: "https://charts.example.org/example-1.2.0.tgz"},
		Values: json.RawMessage(`{"worker":{"replicas":2}}`), Suspend: true,
	}
	if err := svc.CreateChartAddon(ctx, a); err != nil {
		t.Fatal(err)
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
	if _, ok := spec["valuesFrom"]; ok {
		t.Errorf("no secret inputs ⇒ no valuesFrom: %v", spec)
	}
	list, err := svc.ChartAddons(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "example" || list[0].Plan != nil {
		t.Fatalf("list = %+v, %v", list, err)
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

func decodeSecretValue(t *testing.T, sec map[string]any, key string) string {
	t.Helper()
	data, _ := sec["data"].(map[string]any)
	v, ok := data[key].(string)
	if !ok {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return string(b)
}

// Secret inputs are written by create or merge patch; k8sfake fails the test
// on any read of a Secret.
func TestWriteAddonSecretsNeverReads(t *testing.T) {
	svc, fake := chartService(t)
	ctx := context.Background()
	name := ValuesSecretName("example")

	// Clearing keys of a Secret that does not exist is nothing to do.
	if err := svc.WriteAddonSecrets(ctx, "example", nil, []string{"config.password"}); err != nil {
		t.Fatal(err)
	}
	if fake.Secret(name) != nil {
		t.Fatal("clearing must not create a Secret")
	}

	if err := svc.WriteAddonSecrets(ctx, "example", map[string]string{"config.password": "s3cret", "database.url": "postgres://db"}, nil); err != nil {
		t.Fatal(err)
	}
	sec := fake.Secret(name)
	if sec == nil || decodeSecretValue(t, sec, "config.password") != "s3cret" || decodeSecretValue(t, sec, "database.url") != "postgres://db" {
		t.Fatalf("created = %v", sec)
	}
	if labels := sec["metadata"].(map[string]any)["labels"].(map[string]any); labels["zaentrum.io/addon"] != "example" {
		t.Errorf("labels = %v", labels)
	}

	// A kept Secret written again becomes the addon's again.
	fake.PutSecret(map[string]any{
		"metadata": map[string]any{"name": name, "labels": map[string]any{"zaentrum.io/addon": "example", LabelKeep: "true"}},
		"data":     sec["data"],
	})
	if err := svc.WriteAddonSecrets(ctx, "example", map[string]string{"config.password": "changed"}, []string{"database.url"}); err != nil {
		t.Fatal(err)
	}
	sec = fake.Secret(name)
	if decodeSecretValue(t, sec, "config.password") != "changed" || decodeSecretValue(t, sec, "database.url") != "" {
		t.Errorf("patched = %v", sec)
	}
	if labels := sec["metadata"].(map[string]any)["labels"].(map[string]any); labels[LabelKeep] != nil || labels["zaentrum.io/addon"] != "example" {
		t.Errorf("labels after a write = %v", labels)
	}
	for _, c := range fake.Calls() {
		if strings.Contains(c.Path, "/secrets") && c.Method != "POST" && c.Method != "PATCH" {
			t.Errorf("unexpected secret call %s %s", c.Method, c.Path)
		}
	}
}

func TestKeepAndDeleteAddonSecrets(t *testing.T) {
	svc, fake := chartService(t)
	ctx := context.Background()
	name := ValuesSecretName("example")

	// Nothing to keep or delete is not an error.
	if err := svc.KeepAddonSecrets(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteAddonSecrets(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	fake.PutSecret(map[string]any{
		"metadata": map[string]any{
			"name":            name,
			"labels":          map[string]any{"zaentrum.io/addon": "example"},
			"ownerReferences": []any{map[string]any{"kind": "ZaentrumAddon", "name": "example", "controller": false}},
		},
		"data": map[string]any{"config.password": "czNjcmV0"},
	})
	if err := svc.KeepAddonSecrets(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	md := fake.Secret(name)["metadata"].(map[string]any)
	if md["labels"].(map[string]any)[LabelKeep] != "true" || md["ownerReferences"] != nil {
		t.Errorf("kept secret metadata = %v — labelled keep, and nothing for garbage collection to follow", md)
	}
	if err := svc.DeleteAddonSecrets(ctx, "example"); err != nil || fake.Secret(name) != nil {
		t.Errorf("delete = %v, secret %v", err, fake.Secret(name))
	}
}
