package operator

import (
	"context"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/internal/config"
	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

func newSvc(t *testing.T) *Service {
	t.Helper()
	// k8s.New with no in-cluster env → Available()==false; guards must trip
	// before any network call.
	client, err := k8s.New()
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	cfg := config.Config{ProtectedNames: []string{"postgres", "kafka", "valkey", "keycloak"}}
	return New(client, cfg)
}

func TestPhaseOf(t *testing.T) {
	cases := []struct {
		desired, ready, updated int
		want                    string
	}{
		{0, 0, 0, "stopped"},
		{2, 2, 2, "ready"},
		{2, 0, 0, "degraded"},
		{2, 1, 1, "progressing"},
		{1, 1, 0, "progressing"},
	}
	for _, c := range cases {
		if got := phaseOf(c.desired, c.ready, c.updated); got != c.want {
			t.Errorf("phaseOf(%d,%d,%d)=%q want %q", c.desired, c.ready, c.updated, got, c.want)
		}
	}
}

func TestPodMatchesByLabels(t *testing.T) {
	var d k8s.Deployment
	d.Spec.Selector.MatchLabels = map[string]string{"app": "chino-api"}
	match := k8s.Pod{}
	match.Metadata.Labels = map[string]string{"app": "chino-api", "pod-template-hash": "x"}
	miss := k8s.Pod{}
	miss.Metadata.Labels = map[string]string{"app": "chino-web"}
	if !podMatches(match, d) {
		t.Error("expected pod to match by label subset")
	}
	if podMatches(miss, d) {
		t.Error("expected non-matching pod to be excluded")
	}
	// A deployment with no selector must never capture pods (avoids over-counting).
	var noSel k8s.Deployment
	if podMatches(match, noSel) {
		t.Error("empty selector should match nothing")
	}
}

func TestScaleGuards(t *testing.T) {
	s := newSvc(t)
	if err := s.Scale(context.Background(), "postgres", 2); err == nil {
		t.Error("scaling a protected service should error before any k8s call")
	}
	if err := s.Scale(context.Background(), "chino-api", 99); err == nil {
		t.Error("out-of-range replicas should error")
	}
	if err := s.Restart(context.Background(), "kafka"); err == nil {
		t.Error("restarting a protected service should error")
	}
}

func TestValidName(t *testing.T) {
	ok := []string{"chino-api", "katalog-manager-api", "a", "a1", "x.y"}
	bad := []string{"", "../secrets", "..%2Fsecrets", "Foo", "a/b", "a_b", "a ", "-a", "a-"}
	for _, n := range ok {
		if err := validName(n); err != nil {
			t.Errorf("validName(%q) unexpected error: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := validName(n); err == nil {
			t.Errorf("validName(%q) should reject", n)
		}
	}
}

func TestOperatorInfoAbsentWhenNotInCluster(t *testing.T) {
	s := newSvc(t)
	info, err := s.OperatorInfo(context.Background())
	if err != nil {
		t.Fatalf("OperatorInfo: %v", err)
	}
	if info.Present {
		t.Error("operator should not be present outside a cluster")
	}
	if info.Note == "" {
		t.Error("expected an explanatory note when absent")
	}
}

func TestOwnedByStube(t *testing.T) {
	var d k8s.Deployment
	if ownedByZaentrum(d) {
		t.Error("no owner refs → not operator-managed")
	}
	d.Metadata.OwnerReferences = []k8s.OwnerRef{{Kind: "Zaentrum", Name: "zaentrum"}}
	if !ownedByZaentrum(d) {
		t.Error("Stube owner ref → operator-managed")
	}
}

// A deployment with no owner ref but the addons part-of label is an addon.
// This is the case the console exists to show: every addon workload in beta
// carries no owner reference at all, so owner-refs alone would put all five of
// them in "other".
func TestGroupOf(t *testing.T) {
	dep := func(owner string, partOf string) k8s.Deployment {
		var d k8s.Deployment
		d.Metadata.Labels = map[string]string{}
		if partOf != "" {
			d.Metadata.Labels["app.kubernetes.io/part-of"] = partOf
		}
		if owner != "" {
			d.Metadata.OwnerReferences = []k8s.OwnerRef{{Kind: owner}}
		}
		return d
	}
	cases := []struct {
		name   string
		d      k8s.Deployment
		expect string
	}{
		{"operator-owned is platform", dep("Zaentrum", "zaentrum-beta"), "platform"},
		{"addons label is an addon", dep("", "zaentrum-beta-addons"), "addon"},
		// The prefix is the environment name, so the suffix must be what matches.
		{"any environment's addons label", dep("", "zaentrum-prod-addons"), "addon"},
		{"platform label without owner is not an addon", dep("", "zaentrum-beta"), "other"},
		{"no labels at all", dep("", ""), "other"},
		// Ownership wins: if the operator reconciles it, a stale label must not
		// move it out of the group whose upgrades it actually follows.
		{"owner ref beats a stale addons label", dep("Zaentrum", "zaentrum-beta-addons"), "platform"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := groupOf(c.d); got != c.expect {
				t.Fatalf("groupOf() = %q, want %q", got, c.expect)
			}
		})
	}
}

// The 30-hour beta outage in one test: 13 services degraded, and the console
// said only "degraded". The reason has to reach the screen.
func TestUnhealthyReason(t *testing.T) {
	var d k8s.Deployment
	d.Metadata.Name = "transcoder"
	d.Spec.Selector.MatchLabels = map[string]string{"app": "transcoder"}

	pod := func(ready bool, waiting, terminated string, exit int) k8s.Pod {
		var p k8s.Pod
		p.Metadata.Labels = map[string]string{"app": "transcoder"}
		var cs struct {
			RestartCount int32 `json:"restartCount"`
			Ready        bool  `json:"ready"`
			State        struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					Reason   string `json:"reason"`
					Message  string `json:"message"`
					ExitCode int    `json:"exitCode"`
				} `json:"terminated"`
			} `json:"state"`
		}
		cs.Ready = ready
		if waiting != "" {
			cs.State.Waiting = &struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			}{Reason: waiting}
		}
		if terminated != "" {
			cs.State.Terminated = &struct {
				Reason   string `json:"reason"`
				Message  string `json:"message"`
				ExitCode int    `json:"exitCode"`
			}{Reason: terminated, ExitCode: exit}
		}
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, cs)
		return p
	}

	cases := []struct {
		name   string
		pods   []k8s.Pod
		expect string
	}{
		{"the actual outage", []k8s.Pod{pod(false, "ImagePullBackOff", "", 0)}, "ImagePullBackOff"},
		{"crashloop", []k8s.Pod{pod(false, "CrashLoopBackOff", "Error", 1)}, "CrashLoopBackOff"},
		{"terminated with no waiting state", []k8s.Pod{pod(false, "", "OOMKilled", 137)}, "OOMKilled (exit 137)"},
		{"healthy pods report nothing", []k8s.Pod{pod(true, "", "", 0)}, ""},
		// A rollout in progress is not a fault; reporting it would make the
		// field noise and train people to ignore it.
		{"mid-rollout is not a reason", []k8s.Pod{pod(false, "ContainerCreating", "", 0)}, ""},
		{"no pods at all", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unhealthyReason(c.pods, d); got != c.expect {
				t.Fatalf("unhealthyReason() = %q, want %q", got, c.expect)
			}
		})
	}
}

// A stuck rollout reports ready=1 and updated=1 for two DIFFERENT pods, so the
// arithmetic says "ready" while nothing new can start. Beta showed a green
// "ready" badge beside "ErrImagePull" on the same row.
func TestPhaseWithReason(t *testing.T) {
	cases := []struct {
		name           string
		phase, reason  string
		expect         string
	}{
		{"ready with a broken pod is degraded", "ready", "ImagePullBackOff", "degraded"},
		{"ready with nothing wrong stays ready", "ready", "", "ready"},
		// Never upgrade: a reason must not make a stopped or degraded workload
		// look better than it is.
		{"stopped stays stopped", "stopped", "ImagePullBackOff", "stopped"},
		{"degraded stays degraded", "degraded", "CrashLoopBackOff", "degraded"},
		{"progressing is left alone — that is a normal rollout", "progressing", "ContainerCreating", "progressing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := phaseWithReason(c.phase, c.reason); got != c.expect {
				t.Fatalf("phaseWithReason(%q,%q) = %q, want %q", c.phase, c.reason, got, c.expect)
			}
		})
	}
}
