// Package k8s is a tiny in-cluster Kubernetes REST client (no client-go, to keep
// the distroless image small). It talks to the apiserver over https using the
// pod's ServiceAccount: bearer token (re-read per request — projected tokens
// rotate) + the mounted CA (distroless ships no system CA bundle). It exposes
// only what the portal needs: list/scale/restart Deployments, list Pods,
// get/list/create/update/patch/delete a namespaced Custom Resource, and CREATE
// a Secret — there is deliberately no way to read, change or delete one.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
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

// NotFound / Forbidden / Conflict helpers for callers. They see through
// wrapped errors.
func IsNotFound(err error) bool  { return hasCode(err, http.StatusNotFound) }
func IsForbidden(err error) bool { return hasCode(err, http.StatusForbidden) }

// IsConflict: an update carried a stale resourceVersion, or a create named an
// object that already exists.
func IsConflict(err error) bool { return hasCode(err, http.StatusConflict) }

func hasCode(err error, code int) bool {
	var a *APIError
	return errors.As(err, &a) && a.Code == code
}

// Client is a namespaced in-cluster apiserver client.
type Client struct {
	base      string // https://host:port
	namespace string
	http      *http.Client
	inCluster bool
	token     func() string
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
		token:     func() string { return readTrim(tokenPath) },
	}, nil
}

// NewAt builds a client for the apiserver at base (scheme://host:port) acting
// in namespace with a fixed bearer token. Tests point it at a fake apiserver.
func NewAt(base, namespace, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		base:      strings.TrimRight(base, "/"),
		namespace: namespace,
		http:      hc,
		inCluster: true,
		token:     func() string { return token },
	}
}

// ErrNotInCluster is returned by API methods when not running in a cluster.
var ErrNotInCluster = &APIError{Code: 0, Reason: "NotInCluster", Message: "portal-api is not running in a Kubernetes cluster"}

func (c *Client) InCluster() bool   { return c.inCluster }
func (c *Client) Namespace() string { return c.namespace }

// do performs a request and returns the answer.
func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	return c.send(ctx, method, path, contentType, "application/json", body, nil)
}

// send performs a request, re-reading the (rotating) SA token each time. With
// decode set, a successful answer is decoded into it as it streams and not
// kept: a Secret's create is answered with the Secret, and the caller takes
// only its name. Error answers are Status objects and are always read.
func (c *Client) send(ctx context.Context, method, path, contentType, accept string, body []byte, decode any) ([]byte, error) {
	if !c.inCluster {
		return nil, ErrNotInCluster
	}
	token := ""
	if c.token != nil {
		token = c.token()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if decode != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body := io.LimitReader(resp.Body, maxResponseBytes)
		err := json.NewDecoder(body).Decode(decode)
		_, _ = io.Copy(io.Discard, body)
		return nil, err
	}
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
			// State carries WHY a container is not running. Without it the
			// console can say "degraded" but not "cannot pull the image",
			// which is the only part an operator can act on.
			State struct {
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

// dryRun is the query that makes a write validate — admission, schema, every
// check the apiserver runs — without persisting anything.
func dryRun(on bool) string {
	if on {
		return "?dryRun=All"
	}
	return ""
}

// GetResource GETs one namespaced custom resource (raw JSON).
func (c *Client) GetResource(ctx context.Context, group, version, plural, name string) (json.RawMessage, error) {
	p := fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s", group, version, c.namespace, plural, url.PathEscape(name))
	return c.do(ctx, http.MethodGet, p, "", nil)
}

// CreateResource POSTs a namespaced custom resource and returns it as created —
// or, with dryRun, as it would be created.
func (c *Client) CreateResource(ctx context.Context, group, version, plural string, obj []byte, dry bool) (json.RawMessage, error) {
	p := fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s", group, version, c.namespace, plural)
	return c.do(ctx, http.MethodPost, p+dryRun(dry), "application/json", obj)
}

// UpdateResource PUTs a namespaced custom resource. The body carries the
// metadata.resourceVersion it was read at; a stale one is answered 409, so a
// read-modify-write never overwrites a change it did not see. With dryRun the
// update is validated and not persisted.
func (c *Client) UpdateResource(ctx context.Context, group, version, plural, name string, obj []byte, dry bool) (json.RawMessage, error) {
	p := fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s", group, version, c.namespace, plural, url.PathEscape(name))
	return c.do(ctx, http.MethodPut, p+dryRun(dry), "application/json", obj)
}

// DeleteResource deletes a namespaced custom resource. Dependents go by owner
// reference garbage collection (background propagation, the default).
func (c *Client) DeleteResource(ctx context.Context, group, version, plural, name string) error {
	p := fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s", group, version, c.namespace, plural, url.PathEscape(name))
	_, err := c.do(ctx, http.MethodDelete, p, "", nil)
	return err
}

// ─── secrets: create-only ────────────────────────────────────────────────────
//
// portal-api writes the secret inputs an admin types into a new Secret each
// time and never reads, changes or deletes a Secret: there is no get, list,
// watch, patch, update or delete here. Its Role grants create only; the
// operator collects Secrets nothing references any more.

// partialMetadata asks the apiserver to answer with an object's metadata only.
const partialMetadata = "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1,application/json"

// CreateSecret creates a Secret in the client namespace and returns its name —
// the one the apiserver generated when obj asks for a generateName. The answer
// is requested as metadata only, and only metadata.name is taken from it.
func (c *Client) CreateSecret(ctx context.Context, obj []byte) (string, error) {
	p := fmt.Sprintf("/api/v1/namespaces/%s/secrets", c.namespace)
	var created struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if _, err := c.send(ctx, http.MethodPost, p, "application/json", partialMetadata, obj, &created); err != nil {
		return "", err
	}
	if created.Metadata.Name == "" {
		return "", fmt.Errorf("the apiserver created a Secret without saying its name")
	}
	return created.Metadata.Name, nil
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
