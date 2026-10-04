package operator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/redact"
)

// PodLog is one pod (+ its container names + phase) for the log-viewer selector.
type PodLog struct {
	Pod        string   `json:"pod"`
	Phase      string   `json:"phase"`
	Containers []string `json:"containers"`
	// Workload is the name of what runs the pod, from its owner references,
	// and WorkloadKind its kind: Deployment, StatefulSet, DaemonSet, Job, or
	// ReplicaSet for one no Deployment made. Both "" for a pod nothing owns.
	// A client finds a workload's pods by it rather than by guessing from
	// their names.
	Workload     string `json:"workload"`
	WorkloadKind string `json:"workloadKind"`
}

// workloadOf is what runs a pod: its controlling owner — the first owner when
// none says it controls — and, for a ReplicaSet a Deployment made, that
// Deployment. A Deployment names its ReplicaSets <deployment>-<template hash>
// and labels their pods pod-template-hash; the ReplicaSet itself is not read,
// which portal-api's Role does not grant.
func workloadOf(p k8s.Pod) (kind, name string) {
	refs := p.Metadata.OwnerReferences
	if len(refs) == 0 {
		return "", ""
	}
	owner := refs[0]
	for _, o := range refs {
		if o.Controller {
			owner = o
			break
		}
	}
	if owner.Kind == "ReplicaSet" {
		if hash := p.Metadata.Labels["pod-template-hash"]; hash != "" {
			if deployment, ok := strings.CutSuffix(owner.Name, "-"+hash); ok && deployment != "" {
				return "Deployment", deployment
			}
		}
	}
	return owner.Kind, owner.Name
}

// Namespace is the namespace the console operates in (empty when not in-cluster).
func (s *Service) Namespace() string { return s.k8s.Namespace() }

// LogPods lists the namespace's pods with their container names + phase, so the
// log viewer can offer a pod/container selector. Admin-gated at the router.
func (s *Service) LogPods(ctx context.Context) ([]PodLog, error) {
	pods, err := s.k8s.ListPods(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]PodLog, 0, len(pods))
	for _, p := range pods {
		cs := make([]string, 0, len(p.Spec.Containers))
		for _, c := range p.Spec.Containers {
			cs = append(cs, c.Name)
		}
		kind, workload := workloadOf(p)
		out = append(out, PodLog{Pod: p.Metadata.Name, Phase: p.Status.Phase, Containers: cs, Workload: workload, WorkloadKind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pod < out[j].Pod })
	return out, nil
}

// maxLogBytes caps the response of a single container-log fetch (both the
// interactive viewer and each per-container slice of the support bundle), so a
// container that logs very long lines can't return an unbounded body regardless
// of the line-count cap.
const maxLogBytes = 2 << 20 // 2 MiB

// LogQuery is a read of one pod container's log.
type LogQuery struct {
	Pod       string
	Container string // "" for the pod's only container
	// Tail caps the number of lines: 500 when 0, at most 5000.
	Tail int
	// Since (seconds) or SinceTime bounds the age of the lines; zero is no
	// bound, and they are not given together.
	Since     int
	SinceTime time.Time
}

// ErrBadLogQuery: a log read the apiserver is never asked for — a pod or
// container name that is no Kubernetes name, or since and sinceTime together.
var ErrBadLogQuery = errors.New("bad log query")

// Logs returns a pod container's recent logs with secrets redacted. The
// response is byte-capped (maxLogBytes) server-side, and names are validated
// before they reach the apiserver URL path.
func (s *Service) Logs(ctx context.Context, q LogQuery) (string, error) {
	if validName(q.Pod) != nil {
		return "", fmt.Errorf("%w: pod %q is no Kubernetes name", ErrBadLogQuery, q.Pod)
	}
	if q.Container != "" && validName(q.Container) != nil {
		return "", fmt.Errorf("%w: container %q is no Kubernetes name", ErrBadLogQuery, q.Container)
	}
	if q.Since > 0 && !q.SinceTime.IsZero() {
		return "", fmt.Errorf("%w: since and sinceTime both bound the lines — give one", ErrBadLogQuery)
	}
	tail := q.Tail
	if tail <= 0 {
		tail = 500
	}
	if tail > 5000 {
		tail = 5000
	}
	since := max(q.Since, 0)
	raw, err := s.k8s.PodLogs(ctx, q.Pod, k8s.LogOptions{
		Container: q.Container, TailLines: tail, SinceSeconds: since, SinceTime: q.SinceTime, LimitBytes: maxLogBytes,
	})
	if err != nil {
		return "", err
	}
	return ScrubSecrets(string(raw)), nil
}

// ScrubSecrets removes credential-shaped values from text so neither the live
// log viewer nor the support-bundle export ever surfaces passwords/tokens. The
// implementation lives in the shared redact package so every admin surface (logs,
// Kafka tap, DB browser, export) redacts identically; kept here as a thin alias
// for existing callers and the package test.
func ScrubSecrets(s string) string { return redact.Secrets(s) }
