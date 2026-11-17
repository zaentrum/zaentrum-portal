package operator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// R.C8: the alerts read the facts of the platform's resource in the shapes of
// the chapter: since when each part is not ready, the certificate's end, each
// volume as the measuring found it — a free space of 0 is a full disk, not
// one not measured — the backups, the last check and the conditions; a part
// of the status this portal cannot read costs that fact alone; spec.alerts
// has its defaults where it leaves a setting out, or holds one outside its
// range.
func TestRC8TheAlertsReadTheirFacts(t *testing.T) {
	s, fake := rolloutService(t)
	ctx := context.Background()
	if f, err := s.AlertFacts(ctx); f != nil || !errors.Is(err, ErrNoResource) {
		t.Fatalf("no resource: %+v, %v", f, err)
	}
	fake.Put(zaentrums, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum", "creationTimestamp": "2026-11-01T08:00:00Z"},
		"spec": map[string]any{
			"tls":       map[string]any{"secretName": "zaentrum-tls"},
			"backup":    map[string]any{"enabled": false},
			"databases": map[string]any{"mode": "perApp"},
			"alerts":    map[string]any{"partNotReadyMinutes": 5, "volumeFullPercent": 101, "certificateDays": "ten", "mail": false},
		},
	})
	fake.SetStatus(zaentrums, "zaentrum", map[string]any{
		"phase": "Degraded", "currentVersion": "v0.6.0",
		"components": []any{
			map[string]any{"name": "katalog-api", "ready": false, "notReadySince": "2026-11-16T11:00:00+01:00"},
			map[string]any{"name": "chino-api", "ready": true},
		},
		"certificate": map[string]any{"notAfter": "2026-12-01T00:00:00Z", "names": []any{"zaentrum.example.org"}},
		"storage": map[string]any{"volumes": []any{
			map[string]any{"name": "media", "claim": "media", "requestBytes": 500, "sizeBytes": 400, "freeBytes": 0, "filesystem": "node-1:41", "measuredAt": "2026-11-16T10:00:00Z"},
			map[string]any{"name": "postgres-data", "claim": "postgres-data", "requestBytes": 100},
		}},
		"backup":       map[string]any{"lastSuccess": "2026-11-15T00:00:07Z", "lastFailure": "2026-11-16T00:00:05Z"},
		"verification": map[string]any{"result": "Failed", "trigger": "update", "version": "v0.6.0", "startedAt": "2026-11-16T09:00:00Z", "passed": 12, "failed": 2, "checks": []any{map[string]any{"name": "tls", "status": "fail"}, map[string]any{"name": "web", "status": "ok"}, map[string]any{"name": "oidc", "status": "fail"}}},
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "RolloutStalled", "message": "rollout stalled: katalog-api (ProgressDeadlineExceeded)"},
			map[string]any{"type": "Backup", "status": "False", "reason": "Failed", "message": "backup-1 failed: no reason given"},
		},
	})
	f, err := s.AlertFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Resource != "zaentrum" || !f.CreatedAt.Equal(time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)) || f.Phase != "Degraded" || f.CurrentVersion != "v0.6.0" || !f.TLS ||
		f.BackupsEnabled == nil || *f.BackupsEnabled || f.ExternalDatabases {
		t.Fatalf("facts %+v", f)
	}
	if len(f.Parts) != 2 || f.Parts[0].NotReadySince == nil || !f.Parts[0].NotReadySince.Equal(time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)) ||
		f.Parts[0].NotReadySince.Location() != time.UTC || f.Parts[1].NotReadySince != nil || !f.Parts[1].Ready {
		t.Fatalf("parts %+v", f.Parts)
	}
	if f.Certificate == nil || !f.Certificate.NotAfter.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("certificate %+v", f.Certificate)
	}
	if !f.Storage || len(f.Volumes) != 2 || f.Volumes[0].FreeBytes == nil || *f.Volumes[0].FreeBytes != 0 || f.Volumes[0].Filesystem != "node-1:41" ||
		f.Volumes[1].SizeBytes != nil || f.Volumes[1].FreeBytes != nil || f.Volumes[1].RequestBytes == nil {
		t.Fatalf("volumes %+v", f.Volumes)
	}
	if f.LastSuccess == nil || f.LastFailure == nil || f.Conditions[ConditionBackup].Reason != ReasonBackupFailed ||
		!strings.HasPrefix(f.Conditions[ConditionReady].Message, "rollout stalled: ") {
		t.Fatalf("backups and conditions %+v %+v", f, f.Conditions)
	}
	if v := f.Verification; v == nil || v.Result != ResultFailed || v.Trigger != "update" || v.Version != "v0.6.0" || v.Failed != 2 ||
		strings.Join(v.FailedChecks, ",") != "tls,oidc" || v.StartedAt == nil {
		t.Fatalf("verification %+v", f.Verification)
	}
	if st := f.Settings; st.PartNotReadyMinutes != 5 || st.VolumeFullPercent != 90 || st.CertificateDays != 14 || st.BackupAgeDays != 2 || st.Mail {
		t.Fatalf("settings %+v", st)
	}
	// The mail page reads spec.alerts.mail as the alerts do.
	if m, err := s.Mail(ctx); err != nil || m.AlertMail == nil || *m.AlertMail {
		t.Fatalf("alert mail %+v, %v", m, err)
	}

	// A status part this portal cannot read costs that fact alone.
	obj := fake.Object(zaentrums, "zaentrum")
	status := obj["status"].(map[string]any)
	status["storage"] = "measured"
	status["certificate"] = map[string]any{"notAfter": "soon"}
	fake.SetStatus(zaentrums, "zaentrum", status)
	f, err = s.AlertFacts(ctx)
	if err != nil || f.Storage || f.Certificate != nil || len(f.Parts) != 2 || f.LastSuccess == nil {
		t.Fatalf("with a part unreadable: %+v, %v", f, err)
	}
	// Left out: the defaults.
	obj = fake.Object(zaentrums, "zaentrum")
	delete(obj["spec"].(map[string]any), "alerts")
	fake.Put(zaentrums, obj)
	if f, err = s.AlertFacts(ctx); err != nil || f.Settings != (AlertsSettings{PartNotReadyMinutes: 10, VolumeFullPercent: 90, CertificateDays: 14, BackupAgeDays: 2, Mail: true}) {
		t.Fatalf("defaults %+v, %v", f.Settings, err)
	}
}

// R.C8: the facts say which parts' own rollout has given up, from stalled;
// and a value from the status holds no control character: each, a line break
// among them, is a space.
func TestRC8TheFactsSayWhichRolloutHasGivenUp(t *testing.T) {
	s, fake := rolloutService(t)
	fake.Put(zaentrums, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum", "creationTimestamp": "2026-11-01T08:00:00Z"},
		"spec":     map[string]any{},
	})
	fake.SetStatus(zaentrums, "zaentrum", map[string]any{
		"currentVersion": "v0.6.0\u2028next",
		"components": []any{
			map[string]any{"name": "katalog-api", "ready": true, "stalled": true},
			map[string]any{"name": "chino\napi", "ready": false, "notReadySince": "2026-11-16T10:00:00Z"},
			map[string]any{"name": "trans\u202ecoder", "ready": true, "stalled": false},
		},
		"verification": map[string]any{"result": "Failed", "trigger": "update", "version": "v0.6.0", "startedAt": "2026-11-16T09:00:00Z",
			"failed": 1, "checks": []any{map[string]any{"name": "tls\r\nlogin", "status": "fail"}}},
		"storage": map[string]any{"volumes": []any{map[string]any{"name": "media\t", "claim": "media", "requestBytes": 1}}},
	})
	f, err := s.AlertFacts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Parts) != 3 || !f.Parts[0].Stalled || !f.Parts[0].Ready || f.Parts[1].Stalled || f.Parts[2].Stalled {
		t.Fatalf("parts %+v", f.Parts)
	}
	got := []string{f.CurrentVersion, f.Parts[1].Name, f.Parts[2].Name, f.Verification.FailedChecks[0], f.Volumes[0].Name}
	if want := []string{"v0.6.0 next", "chino api", "trans coder", "tls  login", "media "}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("values %q, want %q", got, want)
	}
}

// R.C8: the alert settings are written with a merge patch of spec.alerts,
// validated strictly; a cluster that does not know them refuses the write,
// and one that drops them answers without them, and both are
// ErrAlertsUnknown; KnowsAlerts asks in a dry run, which writes nothing.
func TestRC8TheAlertSettingsAreWrittenStrictly(t *testing.T) {
	s, fake := rolloutService(t)
	ctx := context.Background()
	fake.Put(zaentrums, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum", "metadata": map[string]any{"name": "zaentrum"},
		"spec": map[string]any{"version": "v0.6.0", "alerts": map[string]any{"mail": false}},
	})
	if yes, err := s.KnowsAlerts(ctx, "zaentrum"); err != nil || !yes {
		t.Fatalf("knows: %v %v", yes, err)
	}
	gen, err := s.SetAlerts(ctx, "zaentrum", map[string]any{"backupAgeDays": 3})
	if err != nil || gen != 2 {
		t.Fatalf("set: %d %v", gen, err)
	}
	a := fake.Object(zaentrums, "zaentrum")["spec"].(map[string]any)["alerts"].(map[string]any)
	if len(a) != 2 || a["mail"] != false {
		t.Fatalf("spec.alerts %v", a)
	}
	var strict, dry int
	for _, c := range fake.Calls() {
		if c.Method == http.MethodPatch && strings.Contains(c.Query, "fieldValidation=Strict") {
			strict++
			if strings.Contains(c.Query, "dryRun=All") {
				dry++
			}
		}
	}
	if strict != 2 || dry != 1 {
		t.Fatalf("%d strict patches, %d dry runs", strict, dry)
	}

	fake.Unknown = func(plural string, obj map[string]any) []string {
		if spec, _ := obj["spec"].(map[string]any); spec["alerts"] != nil {
			return []string{"spec.alerts"}
		}
		return nil
	}
	obj := fake.Object(zaentrums, "zaentrum")
	delete(obj["spec"].(map[string]any), "alerts")
	fake.Put(zaentrums, obj)
	if yes, err := s.KnowsAlerts(ctx, "zaentrum"); err != nil || yes {
		t.Fatalf("an older schema, knows: %v %v", yes, err)
	}
	if _, err := s.SetAlerts(ctx, "zaentrum", map[string]any{"backupAgeDays": 3}); !errors.Is(err, ErrAlertsUnknown) || !strings.Contains(err.Error(), `unknown field "spec.alerts"`) {
		t.Fatalf("an older schema: %v", err)
	}
	fake.NoFieldValidation = true
	if _, err := s.SetAlerts(ctx, "zaentrum", map[string]any{"backupAgeDays": 3}); !errors.Is(err, ErrAlertsUnknown) {
		t.Fatalf("an apiserver that drops them: %v", err)
	}
	if spec := fake.Object(zaentrums, "zaentrum")["spec"].(map[string]any); spec["alerts"] != nil {
		t.Fatalf("written: %v", spec)
	}
}

// R.C8: the status as the operator writes it today, with what R.C6, R.C7,
// R.C10, R.C11 and R.C12 added to it, is read whole: every part this portal
// reads, and nothing of what it does not read costs a fact. The shapes are a
// one-machine install's, as it printed them after a backup and a measuring
// asked for, the node's name made neutral.
func TestRC8TheStatusAsTheOperatorWritesItToday(t *testing.T) {
	s, fake := rolloutService(t)
	ctx := context.Background()
	fake.Put(zaentrums, map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum", "creationTimestamp": "2026-11-22T16:45:20Z"},
		"spec":     map[string]any{"channel": "edge", "version": "latest", "storage": map[string]any{"mediaSize": "50Gi"}},
	})
	status := func(measured map[string]any) map[string]any {
		return map[string]any{
			"phase": "Ready", "currentVersion": "latest",
			"components": []any{
				map[string]any{"name": "chino-api", "ready": true, "image": "ghcr.io/zaentrum/chino-api:latest"},
				map[string]any{"name": "katalog-api", "ready": false, "notReadySince": "2026-11-22T16:46:00Z", "stalled": true},
			},
			"storage": map[string]any{
				"request": "proof-1",
				"databases": []any{
					map[string]any{"name": "chino", "bytes": 8109079}, map[string]any{"name": "katalog", "bytes": 10288151},
					map[string]any{"name": "keycloak", "bytes": 13237271}, map[string]any{"name": "portal", "bytes": 8330263},
				},
				"volumes": []any{
					map[string]any{"name": "media", "claim": "media", "requestBytes": 53687091200, "sizeBytes": 154993672192,
						"freeBytes": 78273314816, "filesystem": "node-a:fecaee7bebde1df8", "measuredAt": "2026-11-22T16:48:27Z"},
					map[string]any{"name": "postgres-data", "claim": "postgres-data", "requestBytes": 10737418240, "sizeBytes": 154993672192,
						"freeBytes": 78273376256, "filesystem": "node-a:fecaee7bebde1df8", "measuredAt": "2026-11-22T16:48:27Z"},
					measured,
				},
			},
			"backup": map[string]any{
				"request": "proof-1", "lastSuccess": "2026-11-22T16:48:11Z", "lastDump": "2026-11-22T16-48-07Z",
				"dumps": []any{"2026-11-22T16-48-07Z"}, "job": "zaentrum-backup-abcde",
				"sets": []any{map[string]any{"name": "2026-11-22T16-48-07Z", "bytes": 417792, "whole": true, "version": "latest",
					"own": true, "versionRule": "ok"}},
				"restore": map[string]any{"request": "2026-11-20T00-00-05Z", "result": "Succeeded", "job": "zaentrum-restore-abcde",
					"startedAt": "2026-11-21T10:00:00Z", "finishedAt": "2026-11-21T10:04:00Z", "message": "restored chino, katalog, keycloak, portal"},
			},
			"controller": map[string]any{"version": "v0.5.1", "image": "ghcr.io/zaentrum/operator:v0.5.1", "source": "appliance"},
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "reason": "AllReady", "message": "every component is ready"},
				map[string]any{"type": "Backup", "status": "True", "reason": "Succeeded", "message": "the newest backup is from 2026-11-22T16-48-07Z"},
				map[string]any{"type": "StorageMeasured", "status": "True", "reason": "Measured",
					"message": "media, postgres-data and backups on one disk: 72.9 GiB free of 144.3 GiB; the databases take 38.1 MiB; 1 set on backups"},
				map[string]any{"type": "DatabasePersistent", "status": "True", "reason": "OnClaim", "message": "the bundled Postgres keeps its data on claim postgres-data"},
			},
		}
	}
	fake.SetStatus(zaentrums, "zaentrum", status(map[string]any{"name": "backups", "claim": "backups", "requestBytes": 5368709120,
		"sizeBytes": 154993672192, "freeBytes": 78273302528, "filesystem": "node-a:fecaee7bebde1df8", "measuredAt": "2026-11-22T16:48:27Z"}))
	f, err := s.AlertFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Storage || len(f.Volumes) != 3 {
		t.Fatalf("the volumes are read: %+v", f.Volumes)
	}
	for _, v := range f.Volumes {
		if v.SizeBytes == nil || v.FreeBytes == nil || v.Filesystem != "node-a:fecaee7bebde1df8" || v.MeasuredAt == nil {
			t.Fatalf("a measured volume, one disk: %+v", v)
		}
	}
	if f.LastSuccess == nil || !f.LastSuccess.Equal(time.Date(2026, 11, 22, 16, 48, 11, 0, time.UTC)) ||
		f.Conditions[ConditionBackup].Reason != "Succeeded" {
		t.Fatalf("the backups are read beside the sets, the request and the restore: %+v", f)
	}
	if len(f.Parts) != 2 || f.Parts[1].NotReadySince == nil || !f.Parts[1].Stalled {
		t.Fatalf("the parts are read: %+v", f.Parts)
	}

	// The backups' claim not measured, as while a backup holds it: listed with
	// its request alone, and the condition NotMeasured. It costs no other fact.
	fake.SetStatus(zaentrums, "zaentrum", func() map[string]any {
		st := status(map[string]any{"name": "backups", "claim": "backups", "requestBytes": 5368709120})
		st["conditions"] = []any{map[string]any{"type": "StorageMeasured", "status": "False", "reason": "NotMeasured",
			"message": "backups was not measured: a backup holds it; nothing is measured yet"}}
		return st
	}())
	f, err = s.AlertFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Storage || len(f.Volumes) != 3 || f.Volumes[2].SizeBytes != nil || f.Volumes[2].FreeBytes != nil || f.Volumes[2].RequestBytes == nil ||
		f.Volumes[0].SizeBytes == nil {
		t.Fatalf("one volume not measured, the others read: %+v", f.Volumes)
	}
}
