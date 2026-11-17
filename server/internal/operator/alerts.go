package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
)

// Alerts.
//
// The five alerts read every fact from the platform's resource, as portal-api
// itself (section Alerts of the chapter Running the platform): since when a
// part is not ready, how full each volume is as the measuring jobs found it,
// when the certificate ends, how the backups went, and how the last update
// and its check went. The thresholds are spec.alerts. The resource holds no
// defaults: a setting left out means its default, which AlertsSettings
// applies; a value outside its range is the cluster's to refuse.

// An alert's setting in spec.alerts: its field, its default and its range.
type AlertSetting struct {
	Field                string
	Default, Least, Most int
}

// The four thresholds, as The five alerts has them.
var (
	SettingPartNotReady = AlertSetting{Field: "partNotReadyMinutes", Default: 10, Least: 1, Most: 1440}
	SettingVolumeFull   = AlertSetting{Field: "volumeFullPercent", Default: 90, Least: 50, Most: 99}
	SettingCertificate  = AlertSetting{Field: "certificateDays", Default: 14, Least: 1, Most: 90}
	SettingBackupAge    = AlertSetting{Field: "backupAgeDays", Default: 2, Least: 1, Most: 30}
	// AlertThresholds are the four, in the order of The five alerts.
	AlertThresholds = []AlertSetting{SettingPartNotReady, SettingVolumeFull, SettingCertificate, SettingBackupAge}
)

// AlertsSettings are spec.alerts with the defaults filled in.
type AlertsSettings struct {
	PartNotReadyMinutes, VolumeFullPercent, CertificateDays, BackupAgeDays int
	// Mail is spec.alerts.mail: whether alerts are mailed.
	Mail bool
}

// Of is the value of setting s.
func (a AlertsSettings) Of(s AlertSetting) int {
	switch s.Field {
	case SettingPartNotReady.Field:
		return a.PartNotReadyMinutes
	case SettingVolumeFull.Field:
		return a.VolumeFullPercent
	case SettingCertificate.Field:
		return a.CertificateDays
	case SettingBackupAge.Field:
		return a.BackupAgeDays
	}
	return 0
}

// alertsSpec is spec.alerts as the resource holds it: nil for a setting it
// leaves out.
type alertsSpec struct {
	PartNotReadyMinutes *int  `json:"partNotReadyMinutes"`
	VolumeFullPercent   *int  `json:"volumeFullPercent"`
	CertificateDays     *int  `json:"certificateDays"`
	BackupAgeDays       *int  `json:"backupAgeDays"`
	Mail                *bool `json:"mail"`
}

// alertsSpecOf reads spec.alerts. Each setting is read on its own, so that one
// this portal cannot read costs that setting, which then has its default.
func alertsSpecOf(raw json.RawMessage) alertsSpec {
	var out alertsSpec
	if isNull(raw) {
		return out
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return out
	}
	number := func(name string) *int {
		var n int
		if json.Unmarshal(fields[name], &n) != nil {
			return nil
		}
		return &n
	}
	out.PartNotReadyMinutes = number(SettingPartNotReady.Field)
	out.VolumeFullPercent = number(SettingVolumeFull.Field)
	out.CertificateDays = number(SettingCertificate.Field)
	out.BackupAgeDays = number(SettingBackupAge.Field)
	var mail bool
	if json.Unmarshal(fields["mail"], &mail) == nil {
		out.Mail = &mail
	}
	return out
}

// settings are the values with their defaults: a setting left out, or one
// outside its range, which the resource's schema refuses, has its default.
func (sp alertsSpec) settings() AlertsSettings {
	value := func(v *int, s AlertSetting) int {
		if v == nil || *v < s.Least || *v > s.Most {
			return s.Default
		}
		return *v
	}
	out := AlertsSettings{
		PartNotReadyMinutes: value(sp.PartNotReadyMinutes, SettingPartNotReady),
		VolumeFullPercent:   value(sp.VolumeFullPercent, SettingVolumeFull),
		CertificateDays:     value(sp.CertificateDays, SettingCertificate),
		BackupAgeDays:       value(sp.BackupAgeDays, SettingBackupAge),
		Mail:                true,
	}
	if sp.Mail != nil {
		out.Mail = *sp.Mail
	}
	return out
}

// ─── the facts ───────────────────────────────────────────────────────────────

// Facts are what the platform's resource says that the alerts read.
type Facts struct {
	// Resource is the resource's name; CreatedAt when it was made.
	Resource  string
	CreatedAt time.Time
	// Phase is status.phase; CurrentVersion status.currentVersion.
	Phase          string
	CurrentVersion string
	// Parts are status.components.
	Parts []Part
	// TLS: the platform has a certificate of its own, spec.tls.
	TLS bool
	// Certificate is status.certificate; nil where the status has none.
	Certificate *Certificate
	// Volumes are status.storage.volumes; Storage says whether the status has
	// status.storage at all, which an operator from before measuring does
	// not write.
	Storage bool
	Volumes []Volume
	// BackupsEnabled is spec.backup.enabled, nil where the resource leaves it
	// out; ExternalDatabases is spec.databases.mode external.
	BackupsEnabled    *bool
	ExternalDatabases bool
	// LastSuccess and LastFailure are status.backup's.
	LastSuccess, LastFailure *time.Time
	// Verification is status.verification; nil where there is none, or none
	// this portal can read.
	Verification *Check
	// Conditions are status.conditions, by their type.
	Conditions map[string]Condition
	// Settings are spec.alerts, the defaults filled in.
	Settings AlertsSettings
}

// Part is one of status.components.
type Part struct {
	Name  string
	Ready bool
	// NotReadySince is when the part was first seen not ready while it is
	// meant to run; nil while it is ready, and while it is meant to run at 0.
	NotReadySince *time.Time
	// Stalled: the part's own rollout has given up. A part can be ready and
	// stalled at once: what ran before still serves. False where the status
	// does not say, as under an operator from before stalled.
	Stalled bool
}

// Certificate is status.certificate.
type Certificate struct {
	NotAfter time.Time
	Names    []string
}

// Volume is one of status.storage.volumes. A field the measuring left out is
// nil: a volume that was not measured has no size, and one whose free space
// could not be read none of that.
type Volume struct {
	Name, Claim string
	// Filesystem is the filesystem's id; "" for one that reports none.
	Filesystem                         string
	RequestBytes, SizeBytes, FreeBytes *int64
	MeasuredAt                         *time.Time
}

// Check is the platform's last self-check, status.verification.
type Check struct {
	// Result is Passed, Failed, Running, Skipped or Error; Trigger is update
	// or request.
	Result, Trigger, Version string
	StartedAt                *time.Time
	Passed, Failed, Warned   int
	Skipped                  int
	// FailedChecks are the names of the checks that failed.
	FailedChecks []string
}

// Condition is one of status.conditions, as much as the alerts read.
type Condition struct {
	Status, Reason, Message string
}

// The reasons of the conditions the alerts read.
const (
	ReasonRolloutStalled = "RolloutStalled"
	ReasonBackupFailed   = "Failed"
	ReasonBackupDisabled = "Disabled"
	// The condition TLS says these where the Secret holds a certificate that
	// can be read: served, for another host, or ended.
	ReasonTLSCertificate = "Certificate"
	ReasonTLSWrongHost   = "WrongHost"
	ReasonTLSExpired     = "Expired"
)

// The conditions the alerts read.
const (
	ConditionReady  = "Ready"
	ConditionBackup = "Backup"
	ConditionTLS    = "TLS"
)

// PhaseRestoring is the phase while the operator restores the databases.
const PhaseRestoring = "Restoring"

// factsCR is the resource as the alerts decode it. Each part of the status is
// read on its own, so that one this portal cannot read costs the alert that
// needs it and no other.
type factsCR struct {
	Metadata struct {
		Name              string    `json:"name"`
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		TLS    json.RawMessage `json:"tls"`
		Backup struct {
			Enabled *bool `json:"enabled"`
		} `json:"backup"`
		Databases struct {
			Mode string `json:"mode"`
		} `json:"databases"`
		Alerts json.RawMessage `json:"alerts"`
	} `json:"spec"`
	Status struct {
		Phase          string          `json:"phase"`
		CurrentVersion string          `json:"currentVersion"`
		Components     json.RawMessage `json:"components"`
		Certificate    json.RawMessage `json:"certificate"`
		Storage        json.RawMessage `json:"storage"`
		Backup         json.RawMessage `json:"backup"`
		Verification   json.RawMessage `json:"verification"`
		Conditions     []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// AlertFacts reads the platform's resource, the first Zaentrum in the
// namespace, for the alerts. ErrNoResource where there is none to read; any
// other error is the cluster's.
func (s *Service) AlertFacts(ctx context.Context) (*Facts, error) {
	if !s.k8s.InCluster() {
		return nil, ErrNoResource
	}
	raw, err := s.k8s.GetResourceList(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural)
	switch {
	case k8s.IsNotFound(err):
		return nil, ErrNoResource
	case err != nil:
		return nil, err
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode the Zaentrum resources: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, ErrNoResource
	}
	var cr factsCR
	if err := json.Unmarshal(list.Items[0], &cr); err != nil {
		return nil, fmt.Errorf("decode the Zaentrum resource: %w", err)
	}
	return factsOf(cr), nil
}

func factsOf(cr factsCR) *Facts {
	f := &Facts{
		Resource:          cr.Metadata.Name,
		CreatedAt:         cr.Metadata.CreationTimestamp.UTC(),
		Phase:             cr.Status.Phase,
		CurrentVersion:    Spaced(strings.TrimSpace(cr.Status.CurrentVersion)),
		TLS:               !isNull(cr.Spec.TLS),
		BackupsEnabled:    cr.Spec.Backup.Enabled,
		ExternalDatabases: cr.Spec.Databases.Mode == "external",
		Conditions:        map[string]Condition{},
		Settings:          alertsSpecOf(cr.Spec.Alerts).settings(),
	}
	for _, c := range cr.Status.Conditions {
		f.Conditions[c.Type] = Condition{Status: c.Status, Reason: c.Reason, Message: c.Message}
	}
	var parts []struct {
		Name          string     `json:"name"`
		Ready         bool       `json:"ready"`
		NotReadySince *time.Time `json:"notReadySince"`
		Stalled       bool       `json:"stalled"`
	}
	if json.Unmarshal(cr.Status.Components, &parts) == nil {
		for _, p := range parts {
			part := Part{Name: Spaced(p.Name), Ready: p.Ready, Stalled: p.Stalled}
			if p.NotReadySince != nil {
				t := p.NotReadySince.UTC()
				part.NotReadySince = &t
			}
			f.Parts = append(f.Parts, part)
		}
	}
	var cert struct {
		NotAfter *time.Time `json:"notAfter"`
		Names    []string   `json:"names"`
	}
	if !isNull(cr.Status.Certificate) && json.Unmarshal(cr.Status.Certificate, &cert) == nil && cert.NotAfter != nil {
		names := make([]string, 0, len(cert.Names))
		for _, n := range cert.Names {
			names = append(names, Spaced(n))
		}
		f.Certificate = &Certificate{NotAfter: cert.NotAfter.UTC(), Names: names}
	}
	var storage struct {
		Volumes []struct {
			Name         string     `json:"name"`
			Claim        string     `json:"claim"`
			Filesystem   string     `json:"filesystem"`
			RequestBytes *int64     `json:"requestBytes"`
			SizeBytes    *int64     `json:"sizeBytes"`
			FreeBytes    *int64     `json:"freeBytes"`
			MeasuredAt   *time.Time `json:"measuredAt"`
		} `json:"volumes"`
	}
	if !isNull(cr.Status.Storage) && json.Unmarshal(cr.Status.Storage, &storage) == nil {
		f.Storage = true
		for _, v := range storage.Volumes {
			f.Volumes = append(f.Volumes, Volume{
				Name: Spaced(v.Name), Claim: Spaced(v.Claim), Filesystem: Spaced(strings.TrimSpace(v.Filesystem)),
				RequestBytes: v.RequestBytes, SizeBytes: v.SizeBytes, FreeBytes: v.FreeBytes, MeasuredAt: v.MeasuredAt,
			})
		}
	}
	var backup struct {
		LastSuccess *time.Time `json:"lastSuccess"`
		LastFailure *time.Time `json:"lastFailure"`
	}
	if !isNull(cr.Status.Backup) && json.Unmarshal(cr.Status.Backup, &backup) == nil {
		f.LastSuccess, f.LastFailure = utc(backup.LastSuccess), utc(backup.LastFailure)
	}
	var rec verificationRecord
	if !isNull(cr.Status.Verification) && json.Unmarshal(cr.Status.Verification, &rec) == nil && strings.TrimSpace(rec.Result) != "" {
		c := &Check{
			Result: strings.TrimSpace(rec.Result), Trigger: rec.Trigger, Version: Spaced(strings.TrimSpace(rec.Version)),
			Passed: rec.Passed, Failed: rec.Failed, Warned: rec.Warned, Skipped: rec.Skipped,
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(rec.StartedAt)); err == nil {
			t = t.UTC()
			c.StartedAt = &t
		}
		for _, check := range rec.Checks {
			if check.Status == "fail" {
				c.FailedChecks = append(c.FailedChecks, Spaced(check.Name))
			}
		}
		f.Verification = c
	}
	return f
}

// Spaced is a value from the status as the alerts write it: a control
// character, a line break among them, is written as a space, in a title, a
// text and a log line alike. The characters that turn text around count as
// control characters, as they do in a notice; so do the separators of lines
// and paragraphs.
func Spaced(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), r == 0x2028, r == 0x2029, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return ' '
		}
		return r
	}, s)
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ─── the settings ────────────────────────────────────────────────────────────

// ErrAlertsUnknown: the cluster does not know alert settings — the resource's
// schema has no spec.alerts, as under an operator from before them — so a
// write of them would save nothing. SetAlerts wraps it with the cluster's
// words.
var ErrAlertsUnknown = errors.New("the cluster does not know spec.alerts")

// SetAlerts writes spec.alerts of the resource named with a JSON merge patch,
// each setting given set and every other kept, and answers the generation the
// write made.
//
// The write validates strictly, as a save of the mail settings does
// (SetMail), so that a schema without spec.alerts refuses it instead of
// dropping the field and answering 200. ErrAlertsUnknown, with the
// apiserver's words, for that refusal — and for an answer that holds no
// spec.alerts, from an apiserver that does not validate strictly. Every other
// refusal is the apiserver's as it came, a value outside its range among them.
func (s *Service) SetAlerts(ctx context.Context, resource string, spec map[string]any) (int64, error) {
	raw, err := s.patchAlerts(ctx, resource, spec, false)
	if err != nil {
		return 0, err
	}
	var cr struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &cr)
	return cr.Metadata.Generation, nil
}

// KnowsAlerts asks the cluster, in a dry run that writes nothing, whether the
// resource's schema has spec.alerts. The operator that brings alert settings
// is the one that says since when a part is not ready, so an operator whose
// schema has none is older than that fact. An error is the cluster's, which
// tells nothing either way.
func (s *Service) KnowsAlerts(ctx context.Context, resource string) (bool, error) {
	_, err := s.patchAlerts(ctx, resource, map[string]any{}, true)
	switch {
	case errors.Is(err, ErrAlertsUnknown):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// patchAlerts is the strict merge patch of spec.alerts, and its answer.
func (s *Service) patchAlerts(ctx context.Context, resource string, spec map[string]any, dryRun bool) (json.RawMessage, error) {
	if err := validName(resource); err != nil {
		return nil, err
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"alerts": spec}})
	if err != nil {
		return nil, err
	}
	raw, err := s.k8s.PatchResourceStrict(ctx, s.cfg.OperatorGroup, s.cfg.OperatorVersion, s.cfg.OperatorPlural, resource, patch, dryRun)
	if fields, words := k8s.UnknownFields(err); slices.Contains(fields, "spec.alerts") {
		return nil, fmt.Errorf("%w: %s", ErrAlertsUnknown, words)
	}
	if err != nil {
		return nil, err
	}
	var cr struct {
		Spec struct {
			Alerts json.RawMessage `json:"alerts"`
		} `json:"spec"`
	}
	if json.Unmarshal(raw, &cr) != nil {
		// The write landed when the answer is unreadable; a caller that
		// retried would write it twice.
		return raw, nil
	}
	if a := bytes.TrimSpace(cr.Spec.Alerts); len(a) == 0 || a[0] != '{' {
		return nil, fmt.Errorf("%w: the apiserver answered the write without spec.alerts", ErrAlertsUnknown)
	}
	return raw, nil
}
