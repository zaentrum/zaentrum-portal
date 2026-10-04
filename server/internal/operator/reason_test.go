package operator

import (
	"context"
	"testing"
)

// A pod no node can take is Unschedulable — the reason, and the workload is
// degraded however its counters read, as for any failing pod.
func TestAnUnschedulablePodIsTheReason(t *testing.T) {
	s, fake := rolloutService(t)
	putDeployment(fake, "transcoder", 1, true, "")
	fake.SetStatus(deployments, "transcoder", map[string]any{"replicas": 2, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1, "observedGeneration": 1})
	fake.Put("pods", map[string]any{
		"metadata": map[string]any{"name": "transcoder-6d8f9b7c5-q2x4z", "labels": map[string]any{"app": "transcoder"}},
		"status": map[string]any{"phase": "Pending", "conditions": []any{map[string]any{
			"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
			"message": "0/1 nodes are available: 1 Insufficient nvidia.com/gpu.",
		}}},
	})
	list, err := s.Instances(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("instances = %+v, %v", list, err)
	}
	if in := list[0]; in.Reason != "Unschedulable" || in.Phase != "degraded" {
		t.Errorf("transcoder = reason %q, phase %q", in.Reason, in.Phase)
	}
}
