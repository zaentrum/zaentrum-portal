package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
)

// putPod seeds a pod as the cluster holds one: its owner (nil for none), its
// labels and the containers it runs.
func putPod(kube *k8sfake.Server, name string, owner map[string]any, labels map[string]any, containers ...string) {
	md := map[string]any{"name": name, "labels": labels}
	if owner != nil {
		md["ownerReferences"] = []any{owner}
	}
	cs := make([]any, 0, len(containers))
	for _, c := range containers {
		cs = append(cs, map[string]any{"name": c, "image": "ghcr.io/example/" + c + ":1.0.0"})
	}
	kube.Put("pods", map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": md,
		"spec":   map[string]any{"containers": cs},
		"status": map[string]any{"phase": "Running"},
	})
}

func owner(kind, name string) map[string]any {
	return map[string]any{"kind": kind, "name": name, "controller": true}
}

// Each pod says the workload that runs it, from its owner references — the
// Deployment behind its ReplicaSet included, which portal-api never reads —
// so a client finds a workload's pods without guessing from their names.
func TestDebugPodsSayTheirWorkload(t *testing.T) {
	e := newTokenEnv(t)
	putPod(e.kube, "chino-api-7d9f8b6c5d-x2k4p", owner("ReplicaSet", "chino-api-7d9f8b6c5d"),
		map[string]any{"app": "chino-api", "pod-template-hash": "7d9f8b6c5d"}, "app")
	// chino and chino-api are different workloads, whatever their pods' names share.
	putPod(e.kube, "chino-5b6c7d8f9-abcde", owner("ReplicaSet", "chino-5b6c7d8f9"),
		map[string]any{"pod-template-hash": "5b6c7d8f9"}, "app")
	putPod(e.kube, "postgres-0", owner("StatefulSet", "postgres"), nil, "postgres")
	putPod(e.kube, "zaentrum-verify-q7zx2", owner("Job", "zaentrum-verify"), nil, "verify")
	// A ReplicaSet no Deployment made: its pods carry no template hash.
	putPod(e.kube, "standalone-rs-k2l9m", owner("ReplicaSet", "standalone-rs"), nil, "app")
	putPod(e.kube, "debug-shell", nil, nil, "shell", "sidecar")
	// Of two owners, the controlling one.
	e.kube.Put("pods", map[string]any{
		"metadata": map[string]any{"name": "agent-x8k2c", "ownerReferences": []any{
			map[string]any{"kind": "ConfigMap", "name": "agent-config"},
			map[string]any{"kind": "DaemonSet", "name": "agent", "controller": true},
		}},
		"spec":   map[string]any{"containers": []any{map[string]any{"name": "agent"}}},
		"status": map[string]any{"phase": "Running"},
	})

	rec := e.do(adminPortal, http.MethodGet, "/api/portal/debug/pods", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pods = %d %s", rec.Code, rec.Body)
	}
	var pods []struct {
		Pod          string   `json:"pod"`
		Phase        string   `json:"phase"`
		Containers   []string `json:"containers"`
		Workload     string   `json:"workload"`
		WorkloadKind string   `json:"workloadKind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pods); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"chino-api-7d9f8b6c5d-x2k4p": {"Deployment", "chino-api"},
		"chino-5b6c7d8f9-abcde":      {"Deployment", "chino"},
		"postgres-0":                 {"StatefulSet", "postgres"},
		"zaentrum-verify-q7zx2":      {"Job", "zaentrum-verify"},
		"standalone-rs-k2l9m":        {"ReplicaSet", "standalone-rs"},
		"debug-shell":                {"", ""},
		"agent-x8k2c":                {"DaemonSet", "agent"},
	}
	if len(pods) != len(want) {
		t.Fatalf("pods = %+v", pods)
	}
	for _, p := range pods {
		w := want[p.Pod]
		if p.WorkloadKind != w[0] || p.Workload != w[1] || p.Phase != "Running" || len(p.Containers) == 0 {
			t.Errorf("%s: workload %s/%s, want %s/%s (%+v)", p.Pod, p.WorkloadKind, p.Workload, w[0], w[1], p)
		}
	}
	// A pod nothing owns says so with empty strings, not by leaving the field out.
	if !strings.Contains(rec.Body.String(), `"pod":"debug-shell","phase":"Running","containers":["shell","sidecar"],"workload":"","workloadKind":""`) {
		t.Errorf("pods = %s", rec.Body)
	}
}
