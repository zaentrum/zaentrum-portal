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
