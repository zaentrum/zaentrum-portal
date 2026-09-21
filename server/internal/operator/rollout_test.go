package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
)

// These tests are about one thing: a write has to answer with something a
// waiter can key on. The replica counters cannot do that job — for the first
// seconds after a restart they describe the pods from before it — so every
// write returns the generation it produced, and the console reports the
// generation the controller has observed.

const (
	deployments = "deployments"
	zaentrums   = "zaentrums"
)

func rolloutService(t *testing.T, protected ...string) (*Service, *k8sfake.Server) {
	t.Helper()
	fake := k8sfake.New(t)
	cfg := config.Config{
		OperatorGroup: "zaentrum.io", OperatorVersion: "v1alpha1", OperatorPlural: zaentrums,
		ProtectedNames: protected,
	}
	s := New(fake.Client("zaentrum"), cfg)
	// A fixed clock: two restarts in the same second write the same stamp and
	// therefore change nothing, exactly as kubectl's does.
	at := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { at = at.Add(time.Minute); return at }
	return s, fake
}

// putDeployment seeds a Deployment the way the cluster holds one.
func putDeployment(fake *k8sfake.Server, name string, replicas int, owned bool, restartedAt string) {
	md := map[string]any{"name": name, "labels": map[string]any{"app.kubernetes.io/part-of": "zaentrum"}}
	if owned {
		md["ownerReferences"] = []any{map[string]any{"kind": "Zaentrum", "name": "zaentrum"}}
	}
	tmplMeta := map[string]any{}
	if restartedAt != "" {
		tmplMeta["annotations"] = map[string]any{k8s.RestartedAtAnnotation: restartedAt}
	}
	fake.Put(deployments, map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "metadata": md,
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"template": map[string]any{
				"metadata": tmplMeta,
				"spec": map[string]any{"containers": []any{
					map[string]any{"name": name, "image": "ghcr.io/example/" + name + ":1.4.0"},
				}},
			},
		},
	})
}

// putCR seeds the operator's own resource.
func putCR(fake *k8sfake.Server, version, channel, availableUpdate string) {
	fake.Put(zaentrums, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum"},
		"spec":     map[string]any{"version": version, "channel": channel, "update": map[string]any{"mode": "manual"}},
	})
	fake.SetStatus(zaentrums, "zaentrum", map[string]any{
		"phase": "Ready", "currentVersion": version, "availableUpdate": availableUpdate,
		"observedGeneration": 1,
	})
}

func TestInstancesReportTheRolloutGeneration(t *testing.T) {
	s, fake := rolloutService(t)
	putDeployment(fake, "chino-api", 2, true, "2026-09-21T09:00:00Z")
	fake.SetStatus(deployments, "chino-api", map[string]any{
		"observedGeneration": 1, "replicas": 3, "readyReplicas": 2, "updatedReplicas": 2, "availableReplicas": 2,
	})

	list, err := s.Instances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want one workload, got %+v", list)
	}
	got := list[0]
	if got.Generation != 1 || got.ObservedGeneration != 1 {
		t.Errorf("generation/observedGeneration = %d/%d, want 1/1", got.Generation, got.ObservedGeneration)
	}
	// The total pod count is status.replicas, NOT the count asked for: mid-surge
	// they differ, and that difference is the only thing that says an old pod is
	// still there.
	if got.Replicas != 3 || got.DesiredReplicas != 2 {
		t.Errorf("replicas/desiredReplicas = %d/%d, want 3/2", got.Replicas, got.DesiredReplicas)
	}
	if got.RestartedAt != "2026-09-21T09:00:00Z" {
		t.Errorf("restartedAt = %q", got.RestartedAt)
	}
	// A Deployment that was never restarted this way reports an empty stamp,
	// not a missing field: a client diffs it.
	putDeployment(fake, "katalog-api", 1, true, "")
	list, err = s.Instances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range list {
		if i.Name == "katalog-api" && i.RestartedAt != "" {
			t.Errorf("restartedAt = %q, want empty", i.RestartedAt)
		}
	}
	raw, _ := json.Marshal(list[0])
	for _, field := range []string{`"generation"`, `"observedGeneration"`, `"restartedAt"`, `"replicas"`} {
		if !contains(string(raw), field) {
			t.Errorf("the DTO must carry %s: %s", field, raw)
		}
	}
}

func TestRestartAnswersWithTheGenerationItMade(t *testing.T) {
	s, fake := rolloutService(t)
	putDeployment(fake, "chino-api", 2, false, "")
	fake.SetStatus(deployments, "chino-api", map[string]any{
		"observedGeneration": 1, "readyReplicas": 2, "updatedReplicas": 2,
	})

	w, err := s.Restart(context.Background(), "chino-api")
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "chino-api" || w.Generation != 2 {
		t.Fatalf("Write = %+v, want generation 2 (the stamp changed the spec)", w)
	}
	if w.RestartedAt == "" {
		t.Error("a restart must answer with the stamp it wrote")
	}
	// The stamp actually landed on the pod template, where a rollout sees it.
	obj := fake.Object(deployments, "chino-api")
	tmpl := obj["spec"].(map[string]any)["template"].(map[string]any)
	ann := tmpl["metadata"].(map[string]any)["annotations"].(map[string]any)
	if fmt.Sprint(ann[k8s.RestartedAtAnnotation]) != w.RestartedAt {
		t.Errorf("annotation = %v, answered %q", ann[k8s.RestartedAtAnnotation], w.RestartedAt)
	}
	// The controller has not caught up yet: this is the window in which the
	// counters still describe the old pod, and the generations do not.
	if got := fake.Generation(deployments, "chino-api"); got != 2 {
		t.Errorf("stored generation = %d, want 2", got)
	}
}

func TestScaleAnswersWithTheGenerationItMade(t *testing.T) {
	s, fake := rolloutService(t)
	putDeployment(fake, "chino-api", 1, false, "")

	w, err := s.Scale(context.Background(), "chino-api", 3)
	if err != nil {
		t.Fatal(err)
	}
	if w.Generation != 2 {
		t.Fatalf("Write = %+v, want generation 2", w)
	}
	obj := fake.Object(deployments, "chino-api")
	if got := fmt.Sprint(obj["spec"].(map[string]any)["replicas"]); got != "3" {
		t.Errorf("replicas = %s, want 3", got)
	}
}

// When the operator owns the workload the write goes to the CR — scaling the
// Deployment would be reverted on the next reconcile. The Deployment is then
// untouched, and its generation is a floor a waiter can still use.
func TestScaleGoesToTheOperatorResourceWhenItOwnsTheWorkload(t *testing.T) {
	s, fake := rolloutService(t)
	putDeployment(fake, "chino-api", 1, true, "")
	putCR(fake, "1.4.0", "stable", "")

	w, err := s.Scale(context.Background(), "chino-api", 4)
	if err != nil {
		t.Fatal(err)
	}
	if w.Generation != 1 {
		t.Errorf("the Deployment is untouched, so its generation stands: %+v", w)
	}
	cr := fake.Object(zaentrums, "zaentrum")
	reps, _ := cr["spec"].(map[string]any)["replicas"].(map[string]any)
	if fmt.Sprint(reps["chino-api"]) != "4" {
		t.Fatalf("the CR must carry the replica override: %v", cr["spec"])
	}
	if got := fmt.Sprint(fake.Object(deployments, "chino-api")["spec"].(map[string]any)["replicas"]); got != "1" {
		t.Errorf("the Deployment must not be written: replicas = %s", got)
	}
}

// A workload this namespace does not run is absent, not refused — and the two
// are different answers to a script.
func TestMissingWorkloadIsNotFound(t *testing.T) {
	s, _ := rolloutService(t, "postgres")
	for _, call := range map[string]func() error{
		"restart": func() error { _, err := s.Restart(context.Background(), "nosuch"); return err },
		"scale":   func() error { _, err := s.Scale(context.Background(), "nosuch", 2); return err },
	} {
		if err := call(); !errors.Is(err, ErrNoWorkload) {
			t.Errorf("want ErrNoWorkload, got %v", err)
		}
	}
	// The protected refusal still wins over everything, and is not a 404.
	if _, err := s.Restart(context.Background(), "postgres"); err == nil || errors.Is(err, ErrNoWorkload) {
		t.Errorf("protected must stay a refusal: %v", err)
	}
}

func TestSetOperatorAnswersWithTheVersionAndGeneration(t *testing.T) {
	s, fake := rolloutService(t)
	putCR(fake, "1.4.0", "stable", "1.5.0")

	v := "1.6.0"
	out, err := s.SetOperator(context.Background(), &v, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != "1.6.0" || out.Generation != 2 {
		t.Fatalf("Update = %+v, want 1.6.0 at generation 2", out)
	}
	// And the reads see the same pair, so a waiter can compare them.
	info, err := s.OperatorInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Generation != 2 || info.ObservedGeneration != 1 {
		t.Errorf("OperatorInfo generations = %d/%d, want 2/1 (the operator has not caught up)",
			info.Generation, info.ObservedGeneration)
	}
}

func TestApplyUpdateRefusesAnUpdateThatMoved(t *testing.T) {
	s, fake := rolloutService(t)
	putCR(fake, "1.4.0", "edge", "1.5.0")
	ctx := context.Background()

	// The caller names what it saw, and it is still what is on the shelf.
	out, err := s.ApplyUpdate(ctx, "1.5.0")
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != "1.5.0" || out.Generation != 2 {
		t.Fatalf("Update = %+v", out)
	}

	// Somebody switched the channel in between: applying the version the
	// caller decided on would roll the platform to one nobody chose.
	s2, fake2 := rolloutService(t)
	putCR(fake2, "1.4.0", "edge", "2.0.0-rc1")
	_, err = s2.ApplyUpdate(ctx, "1.5.0")
	if !errors.Is(err, ErrUpdateChanged) {
		t.Fatalf("want ErrUpdateChanged, got %v", err)
	}
	if !contains(err.Error(), "2.0.0-rc1") || !contains(err.Error(), "edge") {
		t.Errorf("the refusal must name what is there now and where: %v", err)
	}
	// Naming nothing still applies whatever is on the shelf.
	if _, err := s2.ApplyUpdate(ctx, ""); err != nil {
		t.Fatalf("an unnamed apply must still work: %v", err)
	}

	// Nothing discovered is still nothing to apply.
	s3, fake3 := rolloutService(t)
	putCR(fake3, "1.4.0", "stable", "")
	if _, err := s3.ApplyUpdate(ctx, ""); err == nil || !contains(err.Error(), "no update available") {
		t.Fatalf("want 'no update available', got %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
