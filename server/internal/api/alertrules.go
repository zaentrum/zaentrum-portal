package api

import (
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// ─── the rules ───────────────────────────────────────────────────────────────
//
// At each check the rules read the facts of the platform's resource and say
// which alert holds, about what, with which title and text; and, for an
// active alert that no longer holds, the sentence of its end (section When
// an alert starts and ends).

// The five kinds, as GET /alerts names them.
const (
	kindPart        = "part-not-ready"
	kindVolume      = "volume-full"
	kindCertificate = "certificate-ends"
	kindBackup      = "no-good-backup"
	kindUpdate      = "update-failed"
)

// alertKinds are the five, in the order of The five alerts.
var alertKinds = []string{kindPart, kindVolume, kindCertificate, kindBackup, kindUpdate}

// The subjects of the kinds that have one.
const (
	aboutCertificate = "certificate"
	aboutBackups     = "backups"
	aboutUpdate      = "update"
)

// alertLink is the page that explains an alert of kind: where open leads, a
// path of the portal.
func alertLink(kind string) string {
	switch kind {
	case kindVolume:
		return "/portal/storage"
	case kindCertificate:
		return "/portal/alerts"
	case kindBackup:
		return "/portal/backups"
	}
	return "/portal/operator"
}

// alertKey is an alert's kind and subject: one is active for each.
type alertKey struct{ kind, about string }

// holding is an alert that holds at a check: its title, its text, the values
// that filled them in, and what the rules keep of it.
type holding struct {
	title, text  string
	values, memo map[string]any
	// noStart: one that is active goes on, and none starts — a part below
	// its setting, or whose own rollout has given up.
	noStart bool
}

// rules are what the rules make of the facts at one check.
type rules struct {
	f      *operator.Facts
	now    time.Time
	holds  map[alertKey]holding
	frozen map[string]bool
}

// evaluate applies the rules to the facts at now. active are the alerts that
// are active, whose memo and values some rules read. While the phase is
// Restoring every part is stopped on purpose: no alert about a part or about
// the update starts or ends.
func evaluate(f *operator.Facts, now time.Time, active []model.Alert) rules {
	r := rules{f: f, now: now, holds: map[alertKey]holding{}, frozen: map[string]bool{}}
	if f.Phase == operator.PhaseRestoring {
		r.frozen[kindPart], r.frozen[kindUpdate] = true, true
	}
	byKind := map[string][]model.Alert{}
	for _, a := range active {
		byKind[a.Kind] = append(byKind[a.Kind], a)
	}
	if !r.frozen[kindPart] {
		r.parts(byKind[kindPart])
	}
	r.volumes(byKind[kindVolume])
	r.certificate()
	r.backups()
	if !r.frozen[kindUpdate] {
		var update *model.Alert
		if list := byKind[kindUpdate]; len(list) > 0 {
			update = &list[0]
		}
		r.update(update)
	}
	return r
}

func (r *rules) hold(kind, about string, h holding) { r.holds[alertKey{kind, about}] = h }

// rolloutStalled: the condition Ready is false with RolloutStalled.
func (r *rules) rolloutStalled() bool {
	c, ok := r.f.Conditions[operator.ConditionReady]
	return ok && c.Status != "True" && c.Reason == operator.ReasonRolloutStalled
}

// ─── A part is not ready ─────────────────────────────────────────────────────

// parts: a part is not ready while it is meant to run since notReadySince. It
// starts an alert once that is older than the setting; a part whose own
// rollout has given up starts none, which An update failed names. An alert's
// text names the setting as it was when the alert started, kept with it, and
// the alert goes on until the part is ready.
func (r *rules) parts(active []model.Alert) {
	setting := r.f.Settings.PartNotReadyMinutes
	started := map[string]int{}
	for _, a := range active {
		var v struct {
			Minutes int `json:"minutes"`
		}
		if json.Unmarshal(a.Values, &v) == nil && v.Minutes > 0 {
			started[a.About] = v.Minutes
		}
	}
	for _, p := range r.f.Parts {
		if p.Ready || p.NotReadySince == nil {
			continue
		}
		minutes, ok := started[p.Name]
		if !ok {
			minutes = setting
		}
		due := !r.now.Before(p.NotReadySince.Add(time.Duration(setting) * time.Minute))
		r.hold(kindPart, p.Name, holding{
			title: partTitle(p.Name), text: partText(p.Name, minutes),
			values:  map[string]any{"part": p.Name, "minutes": minutes},
			noStart: p.Stalled || !due,
		})
	}
}

// partEnd is the sentence of a part's alert that ends: it is ready, it was
// stopped, or it is no part of the platform any more.
func (r *rules) partEnd(a model.Alert) (string, map[string]any) {
	values := map[string]any{"part": a.About}
	i := slices.IndexFunc(r.f.Parts, func(p operator.Part) bool { return p.Name == a.About })
	switch {
	case i < 0:
		return partGone(a.About), values
	case r.f.Parts[i].Ready:
		return partReadyAgain(a.About), values
	}
	return partStopped(a.About), values
}

// ─── A volume is almost full ─────────────────────────────────────────────────

// disk is a volume, or the volumes that report one filesystem id, measured at
// this check.
type disk struct {
	// names are its volumes as the status names them, and words as an alert
	// names them, both in the chapter's order.
	names, words []string
	size, free   int64
}

// used is what is used of the disk: size less free.
func (d disk) used() int64 { return max(0, d.size-d.free) }

// reaches: the disk is at least setting per cent full. It is compared in
// whole numbers, so that a disk exactly at a percentage reaches it.
func (d disk) reaches(setting int) bool { return d.size > 0 && d.used()*100 >= int64(setting)*d.size }

// percent is how full the disk is, in per cent, rounded down.
func (d disk) percent() int {
	if d.size <= 0 {
		return 0
	}
	return int(d.used() * 100 / d.size)
}

// disks are the volumes that were measured — a size, and what is free — by
// disk: volumes that report one filesystem id are one disk at this check,
// with the figures of the one measured last; one that reports none joins
// nothing. A volume without its free space, as the databases' is where the
// database server may not read it, raises none.
func disks(volumes []operator.Volume) []disk {
	var (
		out      []disk
		keys     []string
		measured = map[string]*time.Time{}
	)
	for _, v := range volumes {
		if v.SizeBytes == nil || *v.SizeBytes <= 0 || v.FreeBytes == nil {
			continue
		}
		key := "id " + v.Filesystem
		if v.Filesystem == "" {
			key = "volume " + v.Name
		}
		i := slices.Index(keys, key)
		if i < 0 {
			keys, out = append(keys, key), append(out, disk{})
			i = len(out) - 1
		}
		d := &out[i]
		d.names = append(d.names, v.Name)
		if last, seen := measured[key]; !seen || later(v.MeasuredAt, last) {
			d.size, d.free, measured[key] = *v.SizeBytes, *v.FreeBytes, v.MeasuredAt
		}
	}
	for i := range out {
		slices.SortStableFunc(out[i].names, func(a, b string) int { return volumeRank(a) - volumeRank(b) })
		for _, n := range out[i].names {
			out[i].words = append(out[i].words, volumeWord(n))
		}
	}
	return out
}

// later: t is later than than, a time being later than none.
func later(t, than *time.Time) bool {
	switch {
	case t == nil:
		return false
	case than == nil:
		return true
	}
	return t.After(*than)
}

// volumeRank is where a volume's name comes in an alert: in the order of the
// chapter, one it does not know last.
func volumeRank(name string) int {
	if i := slices.Index(volumeOrder, name); i >= 0 {
		return i
	}
	return len(volumeOrder)
}

// volumeMemo is what the rules keep of a disk's alert: every volume that was
// one disk with its volume while it was active.
type volumeMemo struct {
	Volumes []string `json:"volumes"`
}

// volumes: a disk at or above the setting. Its alert is about its first
// volume, in the chapter's order. The filesystem id only says at each check
// which volumes are one disk, so an id that changes neither ends an alert nor
// starts one: an alert that is active about one of a disk's volumes goes on
// with the disk's figures, and a disk none of whose volumes is the subject of
// an active alert starts one only where none of its volumes was one disk with
// an active alert's.
func (r *rules) volumes(active []model.Alert) {
	setting := r.f.Settings.VolumeFullPercent
	covered := map[string][]string{}
	for _, a := range active {
		var memo volumeMemo
		_ = json.Unmarshal(a.Memo, &memo)
		covered[a.About] = append([]string{a.About}, memo.Volumes...)
	}
	coveredByOne := func(name string) bool {
		for _, names := range covered {
			if slices.Contains(names, name) {
				return true
			}
		}
		return false
	}
	for _, d := range disks(r.f.Volumes) {
		if !d.reaches(setting) {
			continue
		}
		var subjects []string
		for _, n := range d.names {
			if _, ok := covered[n]; ok {
				subjects = append(subjects, n)
			}
		}
		starts := len(subjects) == 0
		if starts {
			if slices.ContainsFunc(d.names, coveredByOne) {
				continue
			}
			subjects = d.names[:1]
		}
		pct := d.percent()
		for _, about := range subjects {
			names := slices.Clone(d.names)
			for _, n := range covered[about] {
				if !slices.Contains(names, n) {
					names = append(names, n)
				}
			}
			slices.SortStableFunc(names, func(a, b string) int { return volumeRank(a) - volumeRank(b) })
			r.hold(kindVolume, about, holding{
				title: volumeTitle(d.words), text: volumeText(d.words, pct, d.free, d.size),
				values:  map[string]any{"volumes": d.words, "percent": pct, "freeBytes": d.free, "sizeBytes": d.size},
				memo:    map[string]any{"volumes": names},
				noStart: !starts,
			})
		}
	}
}

// volumeEnd is the sentence of a disk's alert that ends: its disk is below
// the setting, or its volume is no longer measured.
func (r *rules) volumeEnd(a model.Alert) (string, map[string]any) {
	for _, d := range disks(r.f.Volumes) {
		if slices.Contains(d.names, a.About) {
			pct := d.percent()
			return volumeBelow(d.words, pct), map[string]any{"volumes": d.words, "percent": pct}
		}
	}
	var last struct {
		Volumes []string `json:"volumes"`
	}
	_ = json.Unmarshal(a.Values, &last)
	return volumeUnmeasured(last.Volumes), map[string]any{"volumes": last.Volumes}
}

// ─── The certificate ends soon ───────────────────────────────────────────────

// certificate: the platform's own certificate ends within the setting, or
// has ended. The days until it ends are rounded up.
func (r *rules) certificate() {
	c := r.f.Certificate
	if !r.f.TLS || c == nil {
		return
	}
	left := c.NotAfter.Sub(r.now)
	if left > time.Duration(r.f.Settings.CertificateDays)*24*time.Hour {
		return
	}
	ended := left <= 0
	values := map[string]any{"notAfter": c.NotAfter.Format(time.RFC3339)}
	days := 0
	if !ended {
		days = int(math.Ceil(left.Hours() / 24))
		values["days"] = days
	}
	r.hold(kindCertificate, aboutCertificate, holding{
		title: certificateTitle(days, ended), text: certificateText(c.NotAfter, ended), values: values,
	})
}

// certificateEnd is the sentence of the certificate's alert that ends: it now
// ends later, or the platform serves no certificate of its own any more.
func (r *rules) certificateEnd() (string, map[string]any) {
	if c := r.f.Certificate; r.f.TLS && c != nil {
		return certificateRenewed(c.NotAfter), map[string]any{"notAfter": c.NotAfter.Format(time.RFC3339)}
	}
	return certificateGone, map[string]any{}
}

// ─── No good backup ──────────────────────────────────────────────────────────

// backupsWatched: the platform backs its databases up — they are its own, and
// backups are not off.
func (r *rules) backupsWatched() bool {
	if r.f.ExternalDatabases {
		return false
	}
	c, ok := r.f.Conditions[operator.ConditionBackup]
	return !ok || c.Reason != operator.ReasonBackupDisabled
}

// backups: the last backup failed; or the newest good one is older than the
// setting; or there is none and the platform is older than the setting. The
// days since a backup are rounded down.
func (r *rules) backups() {
	if !r.backupsWatched() {
		return
	}
	f, age := r.f, time.Duration(r.f.Settings.BackupAgeDays)*24*time.Hour
	days := func(since time.Time) int { return int(r.now.Sub(since).Hours() / 24) }
	c := f.Conditions[operator.ConditionBackup]
	switch {
	case c.Reason == operator.ReasonBackupFailed && f.LastFailure != nil:
		values := map[string]any{"failedAt": f.LastFailure.Format(time.RFC3339)}
		if f.LastSuccess != nil {
			values["newestAt"] = f.LastSuccess.Format(time.RFC3339)
		}
		r.hold(kindBackup, aboutBackups, holding{title: backupFailedTitle, text: backupFailedText(*f.LastFailure, f.LastSuccess), values: values})
	case f.LastSuccess != nil && r.now.Sub(*f.LastSuccess) > age:
		n := days(*f.LastSuccess)
		r.hold(kindBackup, aboutBackups, holding{
			title: backupOldTitle(n), text: backupOldText(*f.LastSuccess),
			values: map[string]any{"days": n, "newestAt": f.LastSuccess.Format(time.RFC3339)},
		})
	case f.LastSuccess == nil && !f.CreatedAt.IsZero() && r.now.Sub(f.CreatedAt) > age:
		n := days(f.CreatedAt)
		r.hold(kindBackup, aboutBackups, holding{
			title: backupOldTitle(n), text: backupNoneText(f.CreatedAt),
			values: map[string]any{"days": n, "installedAt": f.CreatedAt.Format(time.RFC3339)},
		})
	}
}

// backupEnd is the sentence of the backups' alert that ends: backups were
// switched off, the databases were made external, a backup was made since it
// started, or else none of its starts holds any more and no backup was made,
// as where the setting was raised.
func (r *rules) backupEnd(a model.Alert) (string, map[string]any) {
	switch {
	case r.f.ExternalDatabases:
		return backupsOutside, map[string]any{}
	case !r.backupsWatched():
		return backupsOff, map[string]any{}
	case r.f.LastSuccess != nil && r.f.LastSuccess.After(a.StartedAt):
		return backupMade(*r.f.LastSuccess), map[string]any{"newestAt": r.f.LastSuccess.Format(time.RFC3339)}
	}
	return backupsWithin, map[string]any{}
}

// ─── An update failed ────────────────────────────────────────────────────────

// updateMemo is what the rules keep of the update's alert: which cause it
// shows, and the check that failed — when it started, and its title, text and
// values — after which only a check that started later and passed ends it.
type updateMemo struct {
	Cause string `json:"cause"`
	// Rollout: the title names a rollout of the version, not an update to
	// it; the sentence of its end says the same.
	Rollout        bool           `json:"rollout,omitempty"`
	CheckStartedAt string         `json:"checkStartedAt,omitempty"`
	CheckTitle     string         `json:"checkTitle,omitempty"`
	CheckText      string         `json:"checkText,omitempty"`
	CheckValues    map[string]any `json:"checkValues,omitempty"`
}

// The causes of An update failed. The third, the updater's report of a
// one-machine install, is read once portal-api can read it (I.C17): it has no
// place here yet.
const (
	causeRollout = "rollout"
	causeCheck   = "check"
)

// update: a rollout has given up, whatever started it; or the last
// self-check was started by an update and ended Failed or Error, and then
// nothing ends it but a later check that passed, whatever started it. A
// rollout that has given up is shown first, naming the parts whose own
// rollout has given up; the check's failure, where it holds too, is kept, and
// shown once the rollout has finished.
func (r *rules) update(active *model.Alert) {
	f := r.f
	var memo updateMemo
	if active != nil {
		_ = json.Unmarshal(active.Memo, &memo)
	}
	check, checkStarted := r.failedCheck(memo)
	if check != nil {
		memo.CheckStartedAt, memo.CheckValues = checkStarted, map[string]any{"version": check.Version}
		memo.CheckTitle = checkTitle(check.Version)
		if check.Result == operator.ResultError {
			memo.CheckText = checkNoResult
		} else {
			total := check.Passed + check.Failed + check.Warned + check.Skipped
			memo.CheckText = checkText(check.Failed, total, check.FailedChecks)
			memo.CheckValues["failed"], memo.CheckValues["total"] = check.Failed, total
			memo.CheckValues["checks"] = nonNil(check.FailedChecks)
		}
	} else if checkStarted == "" {
		memo.CheckStartedAt, memo.CheckTitle, memo.CheckText, memo.CheckValues = "", "", "", nil
	}
	if r.rolloutStalled() {
		parts := []string{}
		for _, p := range f.Parts {
			if p.Stalled {
				parts = append(parts, p.Name)
			}
		}
		rollout := f.Verification != nil && f.Verification.Version != "" && f.Verification.Version == f.CurrentVersion
		text, named := partsText(parts)
		values := map[string]any{"version": f.CurrentVersion, "parts": parts[:named]}
		if named < len(parts) {
			values["more"] = len(parts) - named
		}
		memo.Cause, memo.Rollout = causeRollout, rollout
		r.hold(kindUpdate, aboutUpdate, holding{title: updateTitle(f.CurrentVersion, rollout), text: text, values: values, memo: memoMap(memo)})
		return
	}
	if memo.CheckStartedAt != "" && memo.CheckTitle != "" {
		memo.Cause, memo.Rollout = causeCheck, false
		r.hold(kindUpdate, aboutUpdate, holding{title: memo.CheckTitle, text: memo.CheckText, values: memo.CheckValues, memo: memoMap(memo)})
	}
}

// failedCheck is the check whose failure holds the update's alert, and when
// it started: the last check, where an update started it and it ended Failed
// or Error, or — while the alert holds a check that failed — any later one
// that failed too. checkStarted is "" where no check holds it; a check that
// holds it with nothing new to show answers nil and its start.
func (r *rules) failedCheck(memo updateMemo) (*operator.Check, string) {
	v := r.f.Verification
	failed := v != nil && (v.Result == operator.ResultFailed || v.Result == operator.ResultError)
	started := ""
	if v != nil && v.StartedAt != nil {
		started = v.StartedAt.Format(time.RFC3339)
	}
	if memo.CheckStartedAt == "" {
		if failed && v.Trigger == "update" {
			return v, started
		}
		return nil, ""
	}
	held, _ := time.Parse(time.RFC3339, memo.CheckStartedAt)
	isLater := v != nil && v.StartedAt != nil && v.StartedAt.After(held)
	switch {
	case v != nil && v.Result == operator.ResultPassed && isLater:
		return nil, ""
	case failed:
		return v, started
	}
	return nil, memo.CheckStartedAt
}

// updateEnd is the sentence of the update's alert that ends: by the cause it
// showed, the rollout has finished, or a later check has passed.
func (r *rules) updateEnd(a model.Alert) (string, map[string]any) {
	var memo updateMemo
	_ = json.Unmarshal(a.Memo, &memo)
	if memo.Cause == causeCheck {
		return checkPassed, map[string]any{}
	}
	// The version is the one that runs now; the form is the title's.
	version := r.f.CurrentVersion
	return updateFinished(version, memo.Rollout), map[string]any{"version": version}
}

// ending is the sentence of an active alert's end where it no longer holds.
func (r *rules) ending(a model.Alert) (string, map[string]any) {
	switch a.Kind {
	case kindPart:
		return r.partEnd(a)
	case kindVolume:
		return r.volumeEnd(a)
	case kindCertificate:
		return r.certificateEnd()
	case kindBackup:
		return r.backupEnd(a)
	}
	return r.updateEnd(a)
}

// memoMap is a memo as the store keeps it.
func memoMap(m updateMemo) map[string]any {
	raw, _ := json.Marshal(m)
	return rawMap(raw)
}

// rawMap is a JSON object as a map; empty for anything else.
func rawMap(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// ─── what is watched ─────────────────────────────────────────────────────────

// The states of the column now, one for each of its sentences.
const (
	nowPartsReady           = "parts-ready"
	nowPartsNotReady        = "parts-not-ready"
	nowFullest              = "fullest"
	nowNotMeasured          = "not-measured"
	nowCertificateEnds      = "certificate-ends"
	nowNoCertificate        = "no-certificate"
	nowNewestBackup         = "newest-backup"
	nowBackupsOff           = "backups-off"
	nowDatabasesExternal    = "databases-external"
	nowVersionRuns          = "version-runs"
	nowNoBackupYet          = "no-backup-yet"
	nowCertificateUnread    = "certificate-unreadable"
	nowDatabasesNoVolume    = "databases-without-volume"
	nowUpdateForParts       = "update-operator-parts"
	nowUpdateForVolumes     = "update-operator-volumes"
	nowUpdateForCertificate = "update-operator-certificate"
)

// watchedAlert is one of the five as GET /alerts says what is watched: its
// setting with its range, where it has one, and what the status says now.
type watchedAlert struct {
	Kind    string     `json:"kind"`
	Setting *int       `json:"setting"`
	Least   *int       `json:"least,omitempty"`
	Most    *int       `json:"most,omitempty"`
	Now     watchedNow `json:"now"`
}

// watchedNow is a sentence of the column now: its state, and its values.
type watchedNow struct {
	State  string         `json:"state"`
	Values map[string]any `json:"values,omitempty"`
}

// watched is what is watched, by the facts. knowsSince is false under an
// operator older than notReadySince, which does not say since when a part is
// not ready.
func watched(f *operator.Facts, knowsSince bool) []watchedAlert {
	setting := func(s operator.AlertSetting) (*int, *int, *int) {
		v, least, most := f.Settings.Of(s), s.Least, s.Most
		return &v, &least, &most
	}
	out := make([]watchedAlert, 0, len(alertKinds))
	partsNow := func() watchedNow {
		if !knowsSince {
			return watchedNow{State: nowUpdateForParts}
		}
		watchedParts, notReady := 0, 0
		for _, p := range f.Parts {
			switch {
			case p.Ready:
				watchedParts++
			case p.NotReadySince != nil:
				watchedParts++
				notReady++
			}
		}
		if notReady > 0 {
			return watchedNow{State: nowPartsNotReady, Values: map[string]any{"n": notReady}}
		}
		return watchedNow{State: nowPartsReady, Values: map[string]any{"n": watchedParts}}
	}
	volumesNow := func() watchedNow {
		if !f.Storage {
			return watchedNow{State: nowUpdateForVolumes}
		}
		fullest, found := 0, false
		for _, d := range disks(f.Volumes) {
			if p := d.percent(); !found || p > fullest {
				fullest, found = p, true
			}
		}
		if !found {
			return watchedNow{State: nowNotMeasured}
		}
		return watchedNow{State: nowFullest, Values: map[string]any{"n": fullest}}
	}
	certificateNow := func() watchedNow {
		switch {
		case !f.TLS:
			return watchedNow{State: nowNoCertificate}
		case f.Certificate != nil:
			return watchedNow{State: nowCertificateEnds, Values: map[string]any{"date": f.Certificate.NotAfter.Format(time.RFC3339)}}
		}
		switch f.Conditions[operator.ConditionTLS].Reason {
		case operator.ReasonTLSCertificate, operator.ReasonTLSWrongHost, operator.ReasonTLSExpired:
			// The Secret holds a certificate that can be read, and the
			// status says nothing of when it ends.
			return watchedNow{State: nowUpdateForCertificate}
		}
		return watchedNow{State: nowCertificateUnread}
	}
	backupsNow := func() watchedNow {
		switch {
		case f.ExternalDatabases:
			return watchedNow{State: nowDatabasesExternal}
		case f.Conditions[operator.ConditionBackup].Reason == operator.ReasonBackupDisabled:
			if e := f.BackupsEnabled; e != nil && !*e {
				return watchedNow{State: nowBackupsOff}
			}
			return watchedNow{State: nowDatabasesNoVolume}
		case f.LastSuccess != nil:
			return watchedNow{State: nowNewestBackup, Values: map[string]any{"date": f.LastSuccess.Format(time.RFC3339)}}
		}
		return watchedNow{State: nowNoBackupYet}
	}
	s, l, m := setting(operator.SettingPartNotReady)
	out = append(out, watchedAlert{Kind: kindPart, Setting: s, Least: l, Most: m, Now: partsNow()})
	s, l, m = setting(operator.SettingVolumeFull)
	out = append(out, watchedAlert{Kind: kindVolume, Setting: s, Least: l, Most: m, Now: volumesNow()})
	s, l, m = setting(operator.SettingCertificate)
	out = append(out, watchedAlert{Kind: kindCertificate, Setting: s, Least: l, Most: m, Now: certificateNow()})
	s, l, m = setting(operator.SettingBackupAge)
	out = append(out, watchedAlert{Kind: kindBackup, Setting: s, Least: l, Most: m, Now: backupsNow()})
	out = append(out, watchedAlert{Kind: kindUpdate, Now: watchedNow{State: nowVersionRuns, Values: map[string]any{"version": f.CurrentVersion}}})
	return out
}
