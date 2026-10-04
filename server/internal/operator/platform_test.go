package operator

import (
	"context"
	"strings"
	"testing"
)

func TestPlatformReadsTheResource(t *testing.T) {
	s, fake := rolloutService(t)
	if p, note := s.Platform(context.Background()); p != nil || note == "" {
		t.Fatalf("no resource: %+v, note %q", p, note)
	}
	fake.Put(zaentrums, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum"},
		"spec": map[string]any{
			"hostname": "media.example.com",
			"features": map[string]any{"pipeline": true, "gpu": false, "kafka": true},
			"identity": map[string]any{"mode": "bundled", "issuerScheme": "https"},
		},
	})
	fake.SetStatus(zaentrums, "zaentrum", map[string]any{"controller": map[string]any{"image": "ghcr.io/zaentrum/operator:v0.4.1", "source": "appliance"}})
	p, note := s.Platform(context.Background())
	if p == nil || note != "" {
		t.Fatalf("platform = %+v, note %q", p, note)
	}
	if !p.Pipeline || p.GPU || p.Hostname != "media.example.com" || p.Scheme() != "https" || p.ControllerSource != "appliance" {
		t.Errorf("platform = %+v", p)
	}
	if got := p.DerivedIssuer(); got != "https://media.example.com/auth/realms/zaentrum" {
		t.Errorf("issuer = %q — as the chart derives it", got)
	}
	// An issuer named outright is the issuer; a scheme left out is http.
	p.Issuer, p.IssuerScheme = "https://sso.example.com/realms/media", ""
	if p.DerivedIssuer() != "https://sso.example.com/realms/media" || p.Scheme() != "http" {
		t.Errorf("issuer = %q, scheme %q", p.DerivedIssuer(), p.Scheme())
	}
}

// Whether a node offers a GPU is read when the nodes can be read, and is
// unknown — never a guess — when portal-api's namespaced Role forbids it.
func TestGPUNodes(t *testing.T) {
	s, fake := rolloutService(t)
	fake.Forbidden["nodes"] = true
	if n, known, note := s.GPUNodes(context.Background()); known || n != 0 || !strings.Contains(note, "Role") {
		t.Errorf("forbidden: n=%d known=%v note=%q", n, known, note)
	}
	fake.Forbidden["nodes"] = false
	node := func(name, gpus string) map[string]any {
		capacity := map[string]any{"cpu": "8"}
		if gpus != "" {
			capacity[GPUResource] = gpus
		}
		return map[string]any{"metadata": map[string]any{"name": name}, "status": map[string]any{"capacity": capacity}}
	}
	fake.Put("nodes", node("worker-1", ""))
	if n, known, _ := s.GPUNodes(context.Background()); !known || n != 0 {
		t.Errorf("no GPU node: n=%d known=%v", n, known)
	}
	fake.Put("nodes", node("worker-2", "1"))
	fake.Put("nodes", node("worker-3", "0"))
	if n, known, note := s.GPUNodes(context.Background()); !known || n != 1 || note != "" {
		t.Errorf("one GPU node: n=%d known=%v note=%q", n, known, note)
	}
}

// The library's place is read from the catalog's own Deployment: its scan
// root and the claim mounted over it — the chart's defaults when it cannot be
// read.
func TestMediaVolume(t *testing.T) {
	s, fake := rolloutService(t)
	if got := s.MediaVolume(context.Background(), "katalog-manager-api"); got.Known || got.Path != DefaultMediaPath || got.Claim != "media" || got.Folder != "media" {
		t.Errorf("no Deployment: %+v", got)
	}
	put := func(env []any, mounts []any, volumes []any) {
		fake.Put(deployments, map[string]any{
			"metadata": map[string]any{"name": "katalog-manager-api"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "app", "env": env, "volumeMounts": mounts}},
				"volumes":    volumes,
			}}},
		})
	}
	claim := func(name, claim string) map[string]any {
		return map[string]any{"name": name, "persistentVolumeClaim": map[string]any{"claimName": claim}}
	}
	// As the platform chart renders it.
	put([]any{map[string]any{"name": "NFS_ROOT", "value": "/var/lib/katalog/media"},
		map[string]any{"name": "TMDB_API_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "katalog-tmdb", "key": "api-key"}}}},
		[]any{map[string]any{"name": "schema", "mountPath": "/sql"}, map[string]any{"name": "katalog-data", "mountPath": "/var/lib/katalog"}},
		[]any{claim("katalog-data", "media"), map[string]any{"name": "schema", "configMap": map[string]any{"name": "schema"}}})
	if got := s.MediaVolume(context.Background(), "katalog-manager-api"); got != (MediaVolume{Path: "/var/lib/katalog/media", Claim: "media", Folder: "media", Known: true}) {
		t.Errorf("chart: %+v", got)
	}
	// SCANNER_NFS_ROOT wins, as katalog-manager reads it; a sub path and the
	// deepest mount count.
	put([]any{map[string]any{"name": "NFS_ROOT", "value": "/data"}, map[string]any{"name": "SCANNER_NFS_ROOT", "value": "/library/films/"}},
		[]any{map[string]any{"name": "root", "mountPath": "/"}, map[string]any{"name": "lib", "mountPath": "/library", "subPath": "export"}},
		[]any{claim("root", "system"), claim("lib", "nas-library")})
	if got := s.MediaVolume(context.Background(), "katalog-manager-api"); got != (MediaVolume{Path: "/library/films", Claim: "nas-library", Folder: "export/films", Known: true}) {
		t.Errorf("custom: %+v", got)
	}
	// A root on no claim: the path, and nothing to name it by.
	put([]any{map[string]any{"name": "NFS_ROOT", "value": "/srv/media"}}, nil, nil)
	if got := s.MediaVolume(context.Background(), "katalog-manager-api"); got != (MediaVolume{Path: "/srv/media", Known: true}) {
		t.Errorf("no claim: %+v", got)
	}
	if got := s.MediaVolume(context.Background(), "../secrets"); got.Known {
		t.Errorf("an invalid workload name is never read: %+v", got)
	}
}
