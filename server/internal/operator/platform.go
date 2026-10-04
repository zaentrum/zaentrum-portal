package operator

import (
	"context"
	"path"
	"strconv"
	"strings"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

// Platform is what the operator's resource says about the platform, as the
// setup checklist reads it: whether the media pipeline and the GPU are asked
// for, where the platform is reached and signed in to, and how the operator
// itself was installed.
type Platform struct {
	Name     string
	Hostname string
	// Pipeline and GPU are spec.features.pipeline and .gpu.
	Pipeline bool
	GPU      bool
	// IdentityMode is bundled or external ("" reads as bundled); Issuer is
	// spec.identity.issuer, "" when the operator derives it from IssuerScheme
	// and Hostname; IssuerScheme is http or https ("" reads as http, the
	// resource's default).
	IdentityMode string
	Issuer       string
	IssuerScheme string
	// ControllerSource is how the operator's own controller was installed —
	// olm, manifest, appliance, unknown — "" when it does not say.
	ControllerSource string
}

// Platform reads the operator's resource; nil and why when there is none to
// read — no operator is a way to run the platform, never an error.
func (s *Service) Platform(ctx context.Context) (*Platform, string) {
	cr, note := s.zaentrum(ctx)
	if cr == nil {
		return nil, note
	}
	p := &Platform{
		Name:         cr.Metadata.Name,
		Hostname:     strings.TrimSpace(cr.Spec.Hostname),
		Pipeline:     cr.Spec.Features.Pipeline,
		GPU:          cr.Spec.Features.GPU,
		IdentityMode: strings.TrimSpace(cr.Spec.Identity.Mode),
		Issuer:       strings.TrimSpace(cr.Spec.Identity.Issuer),
		IssuerScheme: strings.TrimSpace(cr.Spec.Identity.IssuerScheme),
	}
	if c := controllerOf(*cr); c != nil {
		p.ControllerSource = c.Source
	}
	return p, ""
}

// Scheme is the scheme the platform is reached with: the issuer's, which the
// operator derives the bundled Keycloak's address from too, and which the
// resource says is https exactly when TLS is terminated in front of the
// ingress.
func (p Platform) Scheme() string {
	if strings.EqualFold(p.IssuerScheme, "https") {
		return "https"
	}
	return "http"
}

// DerivedIssuer is the issuer the platform signs in at: spec.identity.issuer,
// else the bundled realm's on the hostname — as the chart's z.issuer derives
// it.
func (p Platform) DerivedIssuer() string {
	if p.Issuer != "" {
		return p.Issuer
	}
	if p.Hostname == "" {
		return ""
	}
	return p.Scheme() + "://" + p.Hostname + "/auth/realms/zaentrum"
}

// GPUResource is the resource the NVIDIA device plugin offers a GPU as — the
// one the platform's transcoder asks for.
const GPUResource = "nvidia.com/gpu"

// GPUNodes counts the nodes that offer an NVIDIA GPU (capacity GPUResource
// above zero). known is false when the nodes cannot be read, and note says
// why: they are cluster-scoped, and the Role portal-api runs with is
// namespaced, so on a stock install they never can be — the answer is then
// unknown rather than a guess.
func (s *Service) GPUNodes(ctx context.Context) (n int, known bool, note string) {
	nodes, err := s.k8s.ListNodes(ctx)
	switch {
	case k8s.IsForbidden(err):
		return 0, false, "portal-api's Role grants no reads of the cluster's nodes, so whether one offers a GPU cannot be told from here"
	case err != nil:
		return 0, false, "the cluster's nodes could not be read: " + err.Error()
	}
	for _, nd := range nodes {
		if q, err := strconv.ParseInt(strings.TrimSpace(nd.Status.Capacity[GPUResource]), 10, 64); err == nil && q > 0 {
			n++
		}
	}
	return n, true, ""
}

// DefaultMediaPath is where the platform chart has the catalog read the
// library: the media/ folder of the media claim, mounted at /var/lib/katalog.
const DefaultMediaPath = "/var/lib/katalog/media"

// MediaVolume is where the catalog reads the library: Path as katalog-manager
// sees it, the claim that holds it and the folder inside that claim. Known is
// false when the catalog's own Deployment could not be read, and the chart's
// defaults stand in.
type MediaVolume struct {
	Path   string `json:"path"`
	Claim  string `json:"claim"`
	Folder string `json:"folder"`
	Known  bool   `json:"known"`
}

// MediaVolume reads where the catalog's workload — katalog-manager, by its
// Deployment name — scans the library: its scan root (SCANNER_NFS_ROOT, else
// NFS_ROOT, as katalog-manager reads them), and the claim mounted over it.
func (s *Service) MediaVolume(ctx context.Context, workload string) MediaVolume {
	def := MediaVolume{Path: DefaultMediaPath, Claim: "media", Folder: "media"}
	if validName(workload) != nil {
		return def
	}
	d, err := s.k8s.GetDeployment(ctx, workload)
	if err != nil {
		return def
	}
	out := MediaVolume{Path: DefaultMediaPath, Known: true}
	containers := d.Spec.Template.Spec.Containers
	var c *k8s.Container
	for i := range containers {
		if root := envOf(containers[i], "SCANNER_NFS_ROOT", "NFS_ROOT"); root != "" {
			out.Path, c = path.Clean(root), &containers[i]
			break
		}
	}
	if c == nil && len(containers) > 0 {
		c = &containers[0]
	}
	if c == nil {
		return out
	}
	// The mount the scan root lies under — the deepest one.
	var mount *k8s.VolumeMount
	for i := range c.VolumeMounts {
		m := &c.VolumeMounts[i]
		mp := path.Clean(m.MountPath)
		if (out.Path == mp || strings.HasPrefix(out.Path, strings.TrimSuffix(mp, "/")+"/")) && (mount == nil || len(mp) > len(path.Clean(mount.MountPath))) {
			mount = m
		}
	}
	if mount == nil {
		return out
	}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == mount.Name && v.PersistentVolumeClaim != nil {
			out.Claim = v.PersistentVolumeClaim.ClaimName
			rel := strings.TrimPrefix(strings.TrimPrefix(out.Path, path.Clean(mount.MountPath)), "/")
			out.Folder = strings.Trim(path.Join(mount.SubPath, rel), "/")
			if out.Folder == "." {
				out.Folder = ""
			}
		}
	}
	return out
}

// envOf is the first of keys a container's spec sets to a value.
func envOf(c k8s.Container, keys ...string) string {
	for _, k := range keys {
		for _, e := range c.Env {
			if e.Name == k && strings.TrimSpace(e.Value) != "" {
				return strings.TrimSpace(e.Value)
			}
		}
	}
	return ""
}
