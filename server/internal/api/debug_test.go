package api

import (
	"encoding/json"
	"net/http"
	"net/url"
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

// A log read answers in terms a client can act on: a pod this namespace does
// not run is 404, a read the apiserver refuses as asked is 400 in its words —
// neither is the server failing — and the lines come redacted.
func TestDebugLogsAnswerWhatWasWrong(t *testing.T) {
	e := newTokenEnv(t)
	const pod = "chino-api-7d9f8b6c5d-x2k4p"
	putPod(e.kube, pod, owner("ReplicaSet", "chino-api-7d9f8b6c5d"), map[string]any{"pod-template-hash": "7d9f8b6c5d"}, "app")
	putPod(e.kube, "debug-shell", nil, nil, "shell", "sidecar")
	e.kube.Logs[pod+"/app"] = "2026-10-04T06:00:00.000000001Z started with password=hunter2\n"

	for _, c := range []struct {
		query string
		code  int
		says  string
	}{
		{"pod=" + pod, http.StatusOK, "started with password=***REDACTED***"},
		{"pod=" + pod + "&container=app", http.StatusOK, "started"},
		{"pod=gone-7d9f8b6c5d-x2k4p", http.StatusNotFound, `no pod "gone-7d9f8b6c5d-x2k4p" runs in this namespace`},
		{"pod=" + pod + "&container=sidecar", http.StatusBadRequest, "container sidecar is not valid for pod " + pod},
		{"pod=debug-shell", http.StatusBadRequest, "a container name must be specified for pod debug-shell"},
		{"pod=Not_A_Pod", http.StatusBadRequest, `pod "Not_A_Pod" is no Kubernetes name`},
		{"pod=" + pod + "&container=No_Container", http.StatusBadRequest, `container "No_Container" is no Kubernetes name`},
		{"pod=" + pod + "&sinceTime=yesterday", http.StatusBadRequest, `sinceTime "yesterday" is no RFC 3339 time`},
		{"pod=" + pod + "&sinceTime=2026-10-04T06:00:00Z&since=60", http.StatusBadRequest, "since and sinceTime"},
		{"", http.StatusBadRequest, "pod is required"},
	} {
		rec := e.do(adminPortal, http.MethodGet, "/api/portal/debug/logs?"+c.query, nil)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.says) {
			t.Errorf("logs?%s = %d %q, want %d saying %q", c.query, rec.Code, strings.TrimSpace(rec.Body.String()), c.code, c.says)
		}
	}
}

// sinceTime reaches the apiserver to the nanosecond, in UTC: a follower that
// asks for the lines after the last one it has gets no second's worth twice.
func TestDebugLogsSinceTime(t *testing.T) {
	e := newTokenEnv(t)
	const pod = "chino-api-7d9f8b6c5d-x2k4p"
	putPod(e.kube, pod, nil, nil, "app")
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/debug/logs?pod="+pod+"&sinceTime="+url.QueryEscape("2026-10-04T08:00:00.123456789+02:00"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs = %d %s", rec.Code, rec.Body)
	}
	var sent url.Values
	for _, c := range e.kube.Calls() {
		if strings.HasSuffix(c.Path, "/pods/"+pod+"/log") {
			sent, _ = url.ParseQuery(c.Query)
		}
	}
	if got := sent.Get("sinceTime"); got != "2026-10-04T06:00:00.123456789Z" {
		t.Errorf("sinceTime sent = %q (%v)", got, sent)
	}
	if sent.Has("sinceSeconds") {
		t.Errorf("sinceSeconds sent with sinceTime: %v", sent)
	}
}
