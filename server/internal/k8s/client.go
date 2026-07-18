// Package k8s is a tiny in-cluster Kubernetes REST client (no client-go, to keep
// the distroless image small). It talks to the apiserver over https using the
// pod's ServiceAccount: bearer token (re-read per request — projected tokens
// rotate) + the mounted CA (distroless ships no system CA bundle). It exposes
// only what the portal operator/instances UI needs: list/scale/restart
// Deployments, list Pods, and get/patch a namespaced Custom Resource.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	nsPath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	// maxResponseBytes bounds how much of any single apiserver response body the
	// client will buffer (defense against a runaway pod-log body OOM'ing the pod;
	// normal list/get responses are KBs and pod logs are further capped via
	// limitBytes on the request).
	maxResponseBytes = 16 << 20 // 16 MiB
)

// APIError carries the apiserver's metav1.Status so callers can distinguish
// 401 (stale token), 403 (RBAC), and 404 (absent) cleanly.
type APIError struct {
	Code    int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("k8s %d %s: %s", e.Code, e.Reason, e.Message)
	}
	return fmt.Sprintf("k8s %d %s", e.Code, e.Reason)
}

// NotFound / Forbidden helpers for callers.
func IsNotFound(err error) bool { a, ok := err.(*APIError); return ok && a.Code == http.StatusNotFound }
func IsForbidden(err error) bool {
	a, ok := err.(*APIError)
	return ok && a.Code == http.StatusForbidden
}

// Client is a namespaced in-cluster apiserver client.
type Client struct {
	base      string // https://host:port
	namespace string
	http      *http.Client
	inCluster bool
}

// New builds the client from the in-cluster environment. When not running in a
// cluster (no token/host), it returns a client with InCluster()==false; all API
// methods then return ErrNotInCluster so the operator UI degrades gracefully.
func New() (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	ns := readTrim(nsPath)
	if ns == "" {
		ns = strings.TrimSpace(os.Getenv("PORTAL_NAMESPACE"))
	}
	// Detect in-cluster: apiserver env + a readable token.
	if host == "" || port == "" || !fileExists(tokenPath) {
		return &Client{inCluster: false, namespace: ns}, nil
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse in-cluster CA")
	}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &Client{
		base:      fmt.Sprintf("https://%s:%s", host, port),
		namespace: ns,
		http:      &http.Client{Transport: tr, Timeout: 20 * time.Second},
		inCluster: true,
	}, nil
}

// ErrNotInCluster is returned by API methods when not running in a cluster.
var ErrNotInCluster = &APIError{Code: 0, Reason: "NotInCluster", Message: "portal-api is not running in a Kubernetes cluster"}

func (c *Client) InCluster() bool   { return c.inCluster }
func (c *Client) Namespace() string { return c.namespace }

// do performs a request, re-reading the (rotating) SA token each time.
func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if !c.inCluster {
		return nil, ErrNotInCluster
	}
	token := readTrim(tokenPath)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Backstop: never buffer more than maxResponseBytes from any single response
	// (pod logs are additionally capped server-side via limitBytes; normal API
	// responses are KBs). Bounds portal-api's memory against a runaway body.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &APIError{Code: resp.StatusCode, Reason: resp.Status}
		var st metaStatus
		if json.Unmarshal(data, &st) == nil && st.Message != "" {
			ae.Reason = st.Reason
			ae.Message = st.Message
		}
		return nil, ae
	}
	return data, nil
}

// ─── typed models (minimal subsets) ──────────────────────────────────────────

type metaStatus struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type OwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type Container struct {
	Name            string `json:"name"`
	Image           string `json:"image"`
	ImagePullPolicy string `json:"imagePullPolicy"`
}

type Deployment struct {
	Metadata struct {
		Name            string            `json:"name"`
		Labels          map[string]string `json:"labels"`
		OwnerReferences []OwnerRef        `json:"ownerReferences"`
		CreationTime    time.Time         `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Template struct {
			Spec struct {
				Containers []Container `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas          int32 `json:"replicas"`
		ReadyReplicas     int32 `json:"readyReplicas"`
		UpdatedReplicas   int32 `json:"updatedReplicas"`
		AvailableReplicas int32 `json:"availableReplicas"`
	} `json:"status"`
}

type deploymentList struct {
	Items []Deployment `json:"items"`
}

type Pod struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			RestartCount int32 `json:"restartCount"`
			Ready        bool  `json:"ready"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type podList struct {
	Items []Pod `json:"items"`
}

// ─── operations ──────────────────────────────────────────────────────────────

// ListDeployments lists Deployments in the client namespace, optionally filtered
// by a labelSelector (e.g. "app.kubernetes.io/part-of=zaentrum-demo"; empty = all).
func (c *Client) ListDeployments(ctx context.Context, labelSelector string) ([]Deployment, error) {
	p := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", c.namespace)
	if labelSelector != "" {
		q := url.Values{}
		q.Set("labelSelector", labelSelector)
		p += "?" + q.Encode()
	}
	data, err := c.do(ctx, http.MethodGet, p, "", nil)
	if err != nil {
		return nil, err
	}
	var list deploymentList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetDeployment returns a single Deployment (for ownerRef checks).
func (c *Client) GetDeployment(ctx context.Context, name string) (*Deployment, error) {
	p := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", c.namespace, name)
	data, err := c.do(ctx, http.MethodGet, p, "", nil)
	if err != nil {
		return nil, err
	}
	var d Deployment
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ScaleDeployment sets replicas via the scale subresource (JSON merge patch).
func (c *Client) ScaleDeployment(ctx context.Context, name string, replicas int) error {
	p := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s/scale", c.namespace, name)
	body := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, err := c.do(ctx, http.MethodPatch, p, "application/merge-patch+json", body)
	return err
}

// RestartDeployment triggers a rollout restart (kubectl-compatible) by stamping
// a restartedAt annotation on the pod template. `ts` is an RFC3339 timestamp
// supplied by the caller. With :latest + imagePullPolicy:Always this re-pulls.
func (c *Client) RestartDeployment(ctx context.Context, name, ts string) error {
	p := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", c.namespace, name)
	body := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, ts))
	_, err := c.do(ctx, http.MethodPatch, p, "application/strategic-merge-patch+json", body)
	return err
}

// PodLogs returns a pod container's recent logs (plain text, with timestamps).
// tailLines caps the number of lines; sinceSeconds bounds the age (0 = no bound);
// limitBytes caps the response size server-side (0 = no cap) so a container that
// logs very long lines can't return an unbounded body. The log subresource
// returns text/plain, not JSON, so the body is returned raw.
func (c *Client) PodLogs(ctx context.Context, pod, container string, tailLines, sinceSeconds, limitBytes int) ([]byte, error) {
	q := url.Values{}
	if container != "" {
		q.Set("container", container)
	}
	if tailLines > 0 {
		q.Set("tailLines", strconv.Itoa(tailLines))
	}
	if sinceSeconds > 0 {
		q.Set("sinceSeconds", strconv.Itoa(sinceSeconds))
	}
	if limitBytes > 0 {
		q.Set("limitBytes", strconv.Itoa(limitBytes))
	}
	q.Set("timestamps", "true")
	p := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?%s", c.namespace, url.PathEscape(pod), q.Encode())
	return c.do(ctx, http.MethodGet, p, "", nil)
}

// ListPods lists pods matching a labelSelector (a deployment's matchLabels).
func (c *Client) ListPods(ctx context.Context, labelSelector string) ([]Pod, error) {
	p := fmt.Sprintf("/api/v1/namespaces/%s/pods", c.namespace)
	if labelSelector != "" {
		q := url.Values{}
		q.Set("labelSelector", labelSelector)
		p += "?" + q.Encode()
	}
	data, err := c.do(ctx, http.MethodGet, p, "", nil)
	if err != nil {
		return nil, err
	}
	var list podList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetResourceList GETs a namespaced custom-resource collection (raw JSON) so the
// caller can decode the parts it needs. Returns an *APIError on non-2xx (callers
// treat 404/absent as "feature not present").
func (c *Client) GetResourceList(ctx context.Context, group, version, plural string) (json.RawMessage, error) {
	p := fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s", group, version, c.namespace, plural)
	return c.do(ctx, http.MethodGet, p, "", nil)
}

// PatchResource applies a JSON merge patch to a namespaced custom resource.
func (c *Client) PatchResource(ctx context.Context, group, version, plural, name string, mergePatch []byte) error {
	p := fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s", group, version, c.namespace, plural, name)
	_, err := c.do(ctx, http.MethodPatch, p, "application/merge-patch+json", mergePatch)
	return err
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
