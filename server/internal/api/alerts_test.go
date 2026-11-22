package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s/k8sfake"
	"github.com/zaentrum/zaentrum-portal/server/internal/keycloak/kctest"
	zmail "github.com/zaentrum/zaentrum-portal/server/internal/mail"
	"github.com/zaentrum/zaentrum-portal/server/internal/mail/mailtest"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
)

// alertNow is when the tests' checks run, until a test moves the clock.
var alertNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// nbsp is the space that does not break, U+00A0, as the catalog console
// writes it between a size and its unit.
var nbsp = string(rune(0xa0))

// alertEnv is portal-api with the platform's resource in the fake apiserver,
// made two days before alertNow, mail ready through an SMTP server on
// 127.0.0.1, the realm's people in the fake Keycloak, the alerts in memory,
// and a clock the test moves.
type alertEnv struct {
	*mailEnv
	alerts  *fakeAlerts
	clock   time.Time
	created time.Time
	spec    map[string]any
	status  map[string]any
}

func newAlertEnv(t *testing.T, o mailtest.Options) *alertEnv {
	t.Helper()
	e := &alertEnv{mailEnv: newMailEnv(t, true, o), clock: alertNow, created: alertNow.Add(-48 * time.Hour)}
	e.alerts = newFakeAlerts(func() time.Time { return e.clock })
	e.api.alerts = e.alerts
	e.api.now = func() time.Time { return e.clock }
	s := e.settings(zmail.None, "", "")
	e.api.cfg.Mail, e.api.cfg.MailSecret = s, ""
	e.spec = map[string]any{"version": "v0.6.0", "hostname": "zaentrum.example.org", "mail": specOf(s, "")}
	// A good backup an hour ago: no backup alert, unless a test moves the
	// clock past the setting.
	e.status = map[string]any{
		"phase": "Ready", "currentVersion": "v0.6.0",
		"backup":     map[string]any{"lastSuccess": alertNow.Add(-time.Hour).Format(time.RFC3339)},
		"conditions": []any{cond("Backup", "True", "Succeeded", "")},
	}
	e.put()
	return e
}

// put stores the resource as e has it, and its status with the condition
// Mail Configured beside the conditions the status names.
func (e *alertEnv) put() {
	e.t.Helper()
	e.kube.Put("zaentrums", map[string]any{
		"apiVersion": "zaentrum.io/v1alpha1", "kind": "Zaentrum",
		"metadata": map[string]any{"name": "zaentrum", "creationTimestamp": e.created.Format(time.RFC3339)},
		"spec":     e.spec,
	})
	status := map[string]any{}
	for k, v := range e.status {
		status[k] = v
	}
	conds, _ := status["conditions"].([]any)
	status["conditions"] = append([]any{cond("Mail", "True", operator.MailConfigured, "")}, conds...)
	e.kube.SetStatus("zaentrums", "zaentrum", status)
}

// set has the status say v at k, and stores it.
func (e *alertEnv) set(k string, v any) {
	e.t.Helper()
	if v == nil {
		delete(e.status, k)
	} else {
		e.status[k] = v
	}
	e.put()
}

func cond(typ, status, reason, message string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": message, "observedGeneration": 0, "lastTransitionTime": "2026-11-15T10:00:00Z"}
}

func part(name string, ready bool, since time.Time) map[string]any {
	p := map[string]any{"name": name, "ready": ready, "image": "ghcr.io/zaentrum/" + name + ":v0.6.0"}
	if !since.IsZero() {
		p["notReadySince"] = since.Format(time.RFC3339)
	}
	return p
}

// stalledPart is a part whose own rollout has given up.
func stalledPart(name string, ready bool, since time.Time) map[string]any {
	p := part(name, ready, since)
	p["stalled"] = true
	return p
}

func (e *alertEnv) check() {
	e.t.Helper()
	e.api.checkAlerts(context.Background())
}

func (e *alertEnv) later(d time.Duration) { e.clock = e.clock.Add(d) }

// alertsGot is GET /alerts as it came.
type alertsGot struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Alerts    []struct {
		ID, Kind, About, Title, Text, Link string
		Values                             map[string]any
		StartedAt                          time.Time
		EndedAt                            *time.Time
		Mail                               struct {
			State     string   `json:"state"`
			Addresses int      `json:"addresses"`
			Taken     *int     `json:"taken"`
			Error     *refusal `json:"error"`
		}
		Seen bool
	} `json:"alerts"`
	Unseen  int `json:"unseen"`
	Watched []struct {
		Kind    string `json:"kind"`
		Setting *int   `json:"setting"`
		Least   *int   `json:"least"`
		Most    *int   `json:"most"`
		Now     struct {
			State  string         `json:"state"`
			Values map[string]any `json:"values"`
		} `json:"now"`
	} `json:"watched"`
}

func (e *alertEnv) get() alertsGot {
	e.t.Helper()
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/alerts", nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /alerts = %d %s", rec.Code, rec.Body)
	}
	return decodeAs[alertsGot](e.t, rec)
}

// of are the alerts the table holds of kind, the oldest first.
func (e *alertEnv) of(kind string) []model.Alert {
	var out []model.Alert
	for _, a := range e.alerts.all() {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// only is the one alert the table holds of kind, failing the test unless
// there is exactly one.
func (e *alertEnv) only(kind string) model.Alert {
	e.t.Helper()
	out := e.of(kind)
	if len(out) != 1 {
		e.t.Fatalf("%d alerts of %s, want 1: %+v", len(out), kind, out)
	}
	return out[0]
}

func (e *alertEnv) none(kind string) {
	e.t.Helper()
	for _, a := range e.alerts.all() {
		if a.Kind == kind {
			e.t.Fatalf("an alert of %s: %+v", kind, a)
		}
	}
}

func valuesOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("values %s: %v", raw, err)
	}
	return v
}

// R.C8: the four alert routes are the admins', on a token of the portal's own
// clients, as every console's.
func TestRC8AlertRoutesAreTheAdmins(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/portal/alerts"}, {http.MethodPatch, "/api/portal/alerts/settings"},
		{http.MethodPost, "/api/portal/alerts/00000000-0000-4000-8000-000000000001/read"}, {http.MethodPost, "/api/portal/alerts/read-all"},
	}
	refused := map[string]struct {
		claims map[string]any
		code   int
	}{
		"no bearer":                      {nil, http.StatusUnauthorized},
		"a viewer":                       {viewerPortal, http.StatusForbidden},
		"an admin through the media app": {adminMedia, http.StatusForbidden},
		"an addon's service account":     {addonSample, http.StatusForbidden},
	}
	for _, rt := range routes {
		for name, c := range refused {
			if rec := e.do(c.claims, rt.method, rt.path, map[string]any{"mail": false}); rec.Code != c.code {
				t.Errorf("%s %s as %s = %d, want %d", rt.method, rt.path, name, rec.Code, c.code)
			}
		}
	}
	if n := len(e.writes()); n != 0 {
		t.Errorf("refused callers wrote %d times", n)
	}
	for _, rt := range routes {
		if rec := e.do(adminCLI, rt.method, rt.path, map[string]any{"mail": false}); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("%s %s as the admin through the CLI = %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
}

// R.C8: a part not ready since notReadySince starts its alert once that is
// older than the setting, named by the setting in minutes; it ends when the
// part is ready, was stopped, or is no part of the platform any more, each
// with its sentence.
func TestRC8APartThatIsNotReady(t *testing.T) {
	t.Run("R.C8 it starts past the setting, and is ready again", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-9*time.Minute)), part("chino-api", true, time.Time{})})
		e.check()
		e.none(kindPart)
		e.later(time.Minute + time.Second)
		e.check()
		a := e.only(kindPart)
		if a.About != "katalog-api" || a.Title != "A part is not ready: katalog-api" || a.Link != "/portal/operator" ||
			a.Text != "katalog-api has not been ready for more than 10 minutes. Open operator to see why." {
			t.Fatalf("started as %+v", a)
		}
		if v := valuesOf(t, a.Values); v["part"] != "katalog-api" || v["minutes"] != float64(10) {
			t.Errorf("values %v", v)
		}
		// Its text names the setting, not how long it has been: it does not
		// change while the part stays not ready.
		e.later(time.Hour)
		e.check()
		if a2 := e.only(kindPart); a2.Text != a.Text || a2.EndedAt != nil {
			t.Fatalf("an hour later %+v", a2)
		}
		e.set("components", []any{part("katalog-api", true, time.Time{}), part("chino-api", true, time.Time{})})
		e.check()
		a = e.only(kindPart)
		if a.EndedAt == nil || a.EndText != "katalog-api is ready again." || !a.EndedAt.Equal(e.clock) {
			t.Fatalf("ended as %+v", a)
		}
	})
	t.Run("R.C8 a count of one is written in the singular", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.spec["alerts"] = map[string]any{"partNotReadyMinutes": 1}
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Minute-time.Second))})
		e.check()
		if a := e.only(kindPart); a.Text != "katalog-api has not been ready for more than 1 minute. Open operator to see why." {
			t.Fatalf("text %q", a.Text)
		}
	})
	t.Run("R.C8 a part that was stopped, and one that is gone", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		since := alertNow.Add(-time.Hour)
		e.set("components", []any{part("katalog-api", false, since), part("transcoder", false, since)})
		e.check()
		// Meant to run at 0, a part has no notReadySince.
		e.set("components", []any{part("katalog-api", false, time.Time{})})
		e.check()
		got := map[string]string{}
		for _, a := range e.of(kindPart) {
			got[a.About] = a.EndText
		}
		if got["katalog-api"] != "katalog-api was stopped, so it is not watched now." || got["transcoder"] != "transcoder is no longer a part of the platform." {
			t.Fatalf("ends %v", got)
		}
		// Stopped, a part starts none.
		e.later(time.Hour)
		e.check()
		if n := len(e.of(kindPart)); n != 2 {
			t.Fatalf("%d alerts", n)
		}
	})
	t.Run("R.C8 while the phase is Restoring nothing starts or ends for a part", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		e.status["phase"] = "Restoring"
		e.set("components", []any{part("chino-api", false, alertNow.Add(-time.Hour))})
		e.check()
		if a := e.only(kindPart); a.About != "katalog-api" || a.EndedAt != nil {
			t.Fatalf("during the restore %+v", a)
		}
	})
	t.Run("R.C8 a part whose own rollout has given up starts none; another raises its own, and one that is active goes on", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		e.status["phase"] = "Degraded"
		e.status["conditions"] = []any{cond("Ready", "False", "RolloutStalled", "rollout stalled: chino-api (ProgressDeadlineExceeded)")}
		e.set("components", []any{
			stalledPart("katalog-api", false, alertNow.Add(-time.Hour)), stalledPart("chino-api", false, alertNow.Add(-time.Hour)),
			part("transcoder", false, alertNow.Add(-time.Hour)),
		})
		e.check()
		got := map[string]bool{}
		for _, a := range e.of(kindPart) {
			got[a.About] = a.EndedAt == nil
		}
		if len(got) != 2 || !got["katalog-api"] || !got["transcoder"] {
			t.Fatalf("while chino-api's rollout has given up: %v", got)
		}
		e.set("components", []any{part("katalog-api", true, time.Time{}), stalledPart("chino-api", false, alertNow.Add(-time.Hour)), part("transcoder", false, alertNow.Add(-time.Hour))})
		e.check()
		for _, a := range e.of(kindPart) {
			if a.About == "katalog-api" && a.EndText != "katalog-api is ready again." {
				t.Fatalf("it did not go on to its end: %+v", a)
			}
		}
	})
	t.Run("R.C8 {minutes} is the setting as it was when the alert started, and the alert goes on until the part is ready", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-11*time.Minute))})
		e.check()
		e.spec["alerts"] = map[string]any{"partNotReadyMinutes": 60}
		e.put()
		e.later(time.Minute)
		e.check()
		a := e.only(kindPart)
		if a.EndedAt != nil || a.Text != "katalog-api has not been ready for more than 10 minutes. Open operator to see why." || valuesOf(t, a.Values)["minutes"] != float64(10) {
			t.Fatalf("with the setting raised: %+v %s", a, a.Values)
		}
		// Another part that falls now counts by the new setting.
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-11*time.Minute)), part("chino-api", false, e.clock.Add(-61*time.Minute))})
		e.check()
		for _, a := range e.of(kindPart) {
			if a.About == "chino-api" && a.Text != "chino-api has not been ready for more than 60 minutes. Open operator to see why." {
				t.Fatalf("the new one: %q", a.Text)
			}
		}
	})
}

// volume is a measured volume of status.storage.
func volume(name, filesystem string, size, free int64) map[string]any {
	v := map[string]any{"name": name, "claim": name, "requestBytes": size, "sizeBytes": size, "freeBytes": free, "measuredAt": "2026-11-16T11:00:00Z"}
	if filesystem != "" {
		v["filesystem"] = filesystem
	}
	return v
}

// R.C8: a disk at or above the setting starts its alert — the percentage
// compared before it is rounded down, in whole numbers — about its first
// volume; volumes on one disk are one alert, a volume without a filesystem id
// joins nothing, and one whose free space is not known raises none. Sizes are
// written as the storage page writes them.
func TestRC8AVolumeThatIsAlmostFull(t *testing.T) {
	gb := int64(1e9)
	t.Run("R.C8 one volume, compared before it is rounded", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 100*gb, 10400*gb/1000)}})
		e.check()
		e.none(kindVolume) // 89.6 %
		// 90.6 %: written rounded down.
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 100*gb, 9400*gb/1000)}})
		e.check()
		a := e.only(kindVolume)
		want := "The library volume is 90 % full: 9.4" + nbsp + "GB of 100" + nbsp + "GB are free. Open storage to see what takes the room."
		if a.About != "media" || a.Title != "A volume is almost full: library" || a.Text != want || a.Link != "/portal/storage" {
			t.Fatalf("started as %q %q %q", a.About, a.Title, a.Text)
		}
		v := valuesOf(t, a.Values)
		if v["percent"] != float64(90) || v["freeBytes"] != float64(9.4e9) || v["sizeBytes"] != float64(1e11) || !slices.Equal(anyStrings(v["volumes"]), []string{"library"}) {
			t.Errorf("values %v", v)
		}
		// Its figures are written again; it changes only when it ends.
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 100*gb, 5*gb)}})
		e.check()
		if a = e.only(kindVolume); !strings.HasPrefix(a.Text, "The library volume is 95 % full: 5"+nbsp+"GB of") {
			t.Fatalf("written again as %q", a.Text)
		}
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 100*gb, 15*gb)}})
		e.check()
		if a = e.only(kindVolume); a.EndText != "The library volume is at 85 %." {
			t.Fatalf("ended as %q", a.EndText)
		}
	})
	t.Run("R.C8 volumes on one disk are one alert", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("storage", map[string]any{"volumes": []any{
			volume("backups", "node-1:41", 2000*gb, 160*gb), volume("media", "node-1:41", 2000*gb, 160*gb),
			volume("postgres-data", "node-1:41", 2000*gb, 160*gb), volume("kafka", "", 10*gb, 1*gb/2),
		}})
		e.check()
		var disk, broker model.Alert
		for _, a := range e.alerts.all() {
			if a.About == "media" {
				disk = a
			} else {
				broker = a
			}
		}
		if disk.Title != "A volume is almost full: library, databases and backups" ||
			disk.Text != "The disk of library, databases and backups is 92 % full: 160"+nbsp+"GB of 2"+nbsp+"TB are free. Open storage to see what takes the room." {
			t.Fatalf("one disk: %q %q", disk.Title, disk.Text)
		}
		if broker.About != "kafka" || broker.Title != "A volume is almost full: event broker" ||
			broker.Text != "The event broker volume is 95 % full: 500"+nbsp+"MB of 10"+nbsp+"GB are free. Open storage to see what takes the room." {
			t.Fatalf("no filesystem id: %q %q %q", broker.About, broker.Title, broker.Text)
		}
		e.set("storage", map[string]any{"volumes": []any{
			volume("backups", "node-1:41", 2000*gb, 400*gb), volume("media", "node-1:41", 2000*gb, 400*gb),
			volume("postgres-data", "node-1:41", 2000*gb, 400*gb),
		}})
		e.check()
		for _, a := range e.alerts.all() {
			want := map[string]string{"media": "The disk of library, databases and backups is at 80 %.", "kafka": "The event broker volume is not measured any more."}[a.About]
			if a.EndText != want {
				t.Errorf("%s ended as %q, want %q", a.About, a.EndText, want)
			}
		}
	})
	t.Run("R.C8 a volume whose free space is not known raises none; a full one is 100 %", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		db := volume("postgres-data", "", 20*gb, 0)
		delete(db, "freeBytes")
		delete(db, "sizeBytes")
		e.set("storage", map[string]any{"volumes": []any{db, volume("backups", "node-2:7", 5*gb, 0)}})
		e.check()
		a := e.only(kindVolume)
		if a.About != "backups" || a.Text != "The backups volume is 100 % full: 0"+nbsp+"MB of 5"+nbsp+"GB are free. Open storage to see what takes the room." {
			t.Fatalf("%q %q", a.About, a.Text)
		}
	})
	t.Run("R.C8 volumes that report no filesystem id are measured apart", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("storage", map[string]any{"volumes": []any{volume("media", "", 100*gb, 5*gb), volume("backups", "", 100*gb, 4*gb)}})
		e.check()
		var got []string
		for _, a := range e.of(kindVolume) {
			got = append(got, a.About+": "+a.Title)
		}
		if want := []string{"media: A volume is almost full: library", "backups: A volume is almost full: backups"}; !slices.Equal(sorted(got), sorted(want)) {
			t.Fatalf("alerts %v, want %v", got, want)
		}
	})
	t.Run("R.C8 a disk exactly at the setting reaches it, and reads it", func(t *testing.T) {
		for _, pct := range []int64{57, 58} {
			e := newAlertEnv(t, mailtest.Options{})
			e.spec["alerts"] = map[string]any{"volumeFullPercent": pct}
			e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 100*gb, (100-pct)*gb)}})
			e.check()
			a := e.only(kindVolume)
			if v := valuesOf(t, a.Values); v["percent"] != float64(pct) || !strings.HasPrefix(a.Text, "The library volume is "+strconv.FormatInt(pct, 10)+" % full") {
				t.Errorf("at %d %%: %q %v", pct, a.Text, v)
			}
			if w := e.get().Watched[1]; w.Now.Values["n"] != float64(pct) {
				t.Errorf("at %d %%, the fullest is at %v", pct, w.Now.Values)
			}
		}
	})
	t.Run("R.C8 an id that changes neither ends an alert nor starts one", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		full := func(ids ...string) {
			var vols []any
			for i, name := range []string{"media", "postgres-data", "backups"}[:len(ids)] {
				vols = append(vols, volume(name, ids[i], 2000*gb, 100*gb))
			}
			e.set("storage", map[string]any{"volumes": vols})
			e.check()
		}
		full("node-1:41")
		a := e.only(kindVolume)
		// The measuring pod runs on another node.
		full("node-2:41")
		if b := e.only(kindVolume); b.ID != a.ID || b.EndedAt != nil {
			t.Fatalf("on another node: %+v", b)
		}
		// Three volumes of one disk, measured one after another as the id
		// changes.
		e2 := newAlertEnv(t, mailtest.Options{})
		e = e2
		full("node-1:41", "node-1:41", "node-1:41")
		a = e.only(kindVolume)
		if a.About != "media" || a.Title != "A volume is almost full: library, databases and backups" {
			t.Fatalf("one disk: %q %q", a.About, a.Title)
		}
		full("node-1:41", "node-1:41", "node-2:41")
		full("node-2:41", "node-1:41", "node-2:41")
		full("node-2:41", "node-2:41", "node-2:41")
		if b := e.only(kindVolume); b.ID != a.ID || b.EndedAt != nil || b.Title != a.Title {
			t.Fatalf("after the id changed: %+v", b)
		}
		// The disk is below the setting: it ends once.
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-2:41", 2000*gb, 1000*gb), volume("postgres-data", "node-2:41", 2000*gb, 1000*gb), volume("backups", "node-2:41", 2000*gb, 1000*gb)}})
		e.check()
		if b := e.only(kindVolume); b.EndText != "The disk of library, databases and backups is at 50 %." {
			t.Fatalf("ended as %q", b.EndText)
		}
	})
	endsAndActive := func(e *alertEnv) (ends, active []string) {
		for _, a := range e.of(kindVolume) {
			if a.EndedAt != nil {
				ends = append(ends, a.About+": "+a.EndText)
			} else {
				active = append(active, a.About+": "+a.Title)
			}
		}
		slices.Sort(active)
		return ends, active
	}
	t.Run("R.C8 a volume on another id and another size is on another disk from that check on", func(t *testing.T) {
		// The library and the backups on one disk at 92 %, the alert about
		// the library; the backups move to a disk of their own at 98 %, then
		// 100 %.
		e := newAlertEnv(t, mailtest.Options{})
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 160*gb), volume("backups", "node-1:41", 2000*gb, 160*gb)}})
		e.check()
		disk := e.only(kindVolume)
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 160*gb), volume("backups", "node-1:9", 500*gb, 10*gb)}})
		e.check()
		ends, active := endsAndActive(e)
		if len(ends) != 0 || !slices.Equal(active, []string{"backups: A volume is almost full: backups", "media: A volume is almost full: library"}) {
			t.Fatalf("after the move: ended %v, active %v", ends, active)
		}
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 160*gb), volume("backups", "node-1:9", 500*gb, 0)}})
		e.check()
		for _, a := range e.of(kindVolume) {
			switch {
			case a.EndedAt != nil:
				t.Errorf("%s ended: %q", a.About, a.EndText)
			case a.About == "media" && (a.ID != disk.ID || a.Text != "The library volume is 92 % full: 160"+nbsp+"GB of 2"+nbsp+"TB are free. Open storage to see what takes the room."):
				t.Errorf("the library's alert: %+v", a)
			case a.About == "backups" && a.Text != "The backups volume is 100 % full: 0"+nbsp+"MB of 500"+nbsp+"GB are free. Open storage to see what takes the room.":
				t.Errorf("the backups' alert: %q", a.Text)
			}
		}
	})
	t.Run("R.C8 a volume that leaves for a disk of another size raises its own at that check, as the disk's alert ends", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 100*gb), volume("backups", "node-1:41", 2000*gb, 100*gb)}})
		e.check()
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 1000*gb), volume("backups", "node-3:5", 500*gb, 10*gb)}})
		e.check()
		if ends, active := endsAndActive(e); !slices.Equal(ends, []string{"media: The library volume is at 50 %."}) || !slices.Equal(active, []string{"backups: A volume is almost full: backups"}) {
			t.Fatalf("ended %v, active %v", ends, active)
		}
	})
	t.Run("R.C8 a volume that moves to a disk of exactly the same size stays with the alert until it ends", func(t *testing.T) {
		// Taken for the same disk under a changed id: it raises its own only
		// once the disk's alert has ended.
		e := newAlertEnv(t, mailtest.Options{})
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 100*gb), volume("backups", "node-1:41", 2000*gb, 100*gb)}})
		e.check()
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 100*gb), volume("backups", "node-3:5", 2000*gb, 10*gb)}})
		e.check()
		if ends, active := endsAndActive(e); len(ends) != 0 || !slices.Equal(active, []string{"media: A volume is almost full: library"}) {
			t.Fatalf("after a move to a disk of the same size: ended %v, active %v", ends, active)
		}
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 1000*gb), volume("backups", "node-3:5", 2000*gb, 10*gb)}})
		e.check()
		if ends, active := endsAndActive(e); !slices.Equal(ends, []string{"media: The library volume is at 50 %."}) || len(active) != 0 {
			t.Fatalf("as the disk's alert ends: ended %v, active %v", ends, active)
		}
		e.check()
		if _, active := endsAndActive(e); !slices.Equal(active, []string{"backups: A volume is almost full: backups"}) {
			t.Fatalf("a check later: active %v", active)
		}
	})
	t.Run("R.C8 figures older than 2 hours say when they were measured", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("storage", map[string]any{"volumes": []any{volume("backups", "node-2:7", 500*gb, 10*gb)}})
		e.check()
		fresh := "The backups volume is 98 % full: 10" + nbsp + "GB of 500" + nbsp + "GB are free. Open storage to see what takes the room."
		if a := e.only(kindVolume); a.Text != fresh {
			t.Fatalf("an hour old: %q", a.Text)
		}
		// Exactly 2 hours old: not yet.
		e.later(time.Hour)
		e.check()
		if a := e.only(kindVolume); a.Text != fresh {
			t.Fatalf("2 hours old: %q", a.Text)
		}
		e.later(time.Second)
		e.check()
		a := e.only(kindVolume)
		if a.Text != "The backups volume is 98 % full, as measured on 16 November 2026, 11:00 UTC: 10"+nbsp+"GB of 500"+nbsp+"GB are free. Open storage to see what takes the room." {
			t.Fatalf("past 2 hours: %q", a.Text)
		}
		var v map[string]any
		if json.Unmarshal(a.Values, &v) != nil || v["measuredAt"] != "2026-11-16T11:00:00Z" {
			t.Fatalf("values %s", a.Values)
		}
		// Measured again: the sentence as it was.
		vol := volume("backups", "node-2:7", 500*gb, 10*gb)
		vol["measuredAt"] = e.clock.Format(time.RFC3339)
		e.set("storage", map[string]any{"volumes": []any{vol}})
		e.check()
		if a := e.only(kindVolume); a.Text != fresh {
			t.Fatalf("measured again: %q", a.Text)
		}
	})
	t.Run("R.C8 a disk of several that is no longer measured", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 2000*gb, 100*gb), volume("backups", "node-1:41", 2000*gb, 100*gb)}})
		e.check()
		e.set("storage", map[string]any{"volumes": []any{}})
		e.check()
		if a := e.only(kindVolume); a.EndText != "The disk of library and backups is not measured any more." {
			t.Fatalf("ended as %q", a.EndText)
		}
	})
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// R.C8: the platform's own certificate within the setting of its end starts
// its alert, the days until it ends rounded up; past its end the title and
// the text say it has ended; it ends when the certificate ends later, or the
// platform serves none of its own.
func TestRC8TheCertificateThatEndsSoon(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.spec["tls"] = map[string]any{"secretName": "zaentrum-tls"}
	cert := func(notAfter time.Time) map[string]any {
		return map[string]any{"notAfter": notAfter.Format(time.RFC3339), "names": []any{"zaentrum.example.org"}}
	}
	e.set("certificate", cert(alertNow.Add(14*24*time.Hour+time.Second)))
	e.check()
	e.none(kindCertificate)
	ends := alertNow.Add(13*24*time.Hour + 12*time.Hour)
	e.set("certificate", cert(ends))
	e.check()
	a := e.only(kindCertificate)
	if a.About != "certificate" || a.Title != "The certificate ends in 14 days" || a.Link != "/portal/alerts" ||
		a.Text != "The platform's certificate ends on 30 November 2026, 00:00 UTC. After that browsers warn and the apps stop connecting. Renew it." {
		t.Fatalf("started as %q %q %q", a.About, a.Title, a.Text)
	}
	e.later(13 * 24 * time.Hour)
	e.check()
	if a = e.only(kindCertificate); a.Title != "The certificate ends in 1 day" {
		t.Fatalf("half a day before: %q", a.Title)
	}
	e.later(time.Hour * 13)
	e.check()
	if a = e.only(kindCertificate); a.Title != "The certificate has ended" ||
		a.Text != "The platform's certificate ended on 30 November 2026, 00:00 UTC. Browsers warn and the apps do not connect. Renew it." {
		t.Fatalf("after its end: %q %q", a.Title, a.Text)
	}
	e.set("certificate", cert(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)))
	e.check()
	if a = e.only(kindCertificate); a.EndText != "The certificate now ends on 1 March 2027, 00:00 UTC." {
		t.Fatalf("renewed: %q", a.EndText)
	}
	// Again, and then the platform serves no certificate of its own.
	e.set("certificate", cert(e.clock.Add(24*time.Hour)))
	e.check()
	delete(e.spec, "tls")
	e.set("certificate", nil)
	e.check()
	certs := e.of(kindCertificate)
	for _, a := range certs {
		if a.EndText == "" {
			t.Fatalf("still active: %+v", a)
		}
	}
	if last := certs[len(certs)-1]; len(certs) != 2 || last.EndText != "The platform serves no certificate of its own now." {
		t.Fatalf("its own no more: %q", last.EndText)
	}
}

// R.C8: with spec.tls still set, a certificate the status no longer says the
// end of ends its alert with the sentence that is true: it cannot be read, or
// an older operator does not say when it ends.
func TestRC8ACertificateThatIsNoLongerRead(t *testing.T) {
	for _, c := range []struct{ name, status, reason, end string }{
		{"R.C8 it cannot be read", "False", "SecretNotFound", "The platform's certificate cannot be read now, so it is not watched."},
		{"R.C8 an older operator reads it and says nothing of its end", "True", operator.ReasonTLSCertificate, "The operator no longer says when the certificate ends, so it is not watched. Update the operator."},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newAlertEnv(t, mailtest.Options{})
			e.spec["tls"] = map[string]any{"secretName": "zaentrum-tls"}
			e.set("certificate", map[string]any{"notAfter": alertNow.Add(24 * time.Hour).Format(time.RFC3339), "names": []any{"zaentrum.example.org"}})
			e.check()
			e.only(kindCertificate)
			e.status["certificate"] = nil
			e.status["conditions"] = []any{cond("TLS", c.status, c.reason, "")}
			e.put()
			e.check()
			if a := e.only(kindCertificate); a.EndedAt == nil || a.EndText != c.end {
				t.Fatalf("ended as %q", a.EndText)
			}
		})
	}
}

// R.C8: the certificate's two edges. One that ends exactly the setting's days
// from now is within it; one that ends now has ended.
func TestRC8TheCertificatesEdges(t *testing.T) {
	cert := func(notAfter time.Time) map[string]any {
		return map[string]any{"notAfter": notAfter.Format(time.RFC3339), "names": []any{"zaentrum.example.org"}}
	}
	t.Run("R.C8 exactly the setting's days", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.spec["tls"] = map[string]any{"secretName": "zaentrum-tls"}
		e.set("certificate", cert(alertNow.Add(14*24*time.Hour)))
		e.check()
		if a := e.only(kindCertificate); a.Title != "The certificate ends in 14 days" {
			t.Fatalf("at the setting: %q", a.Title)
		}
	})
	t.Run("R.C8 the instant it ends", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.spec["tls"] = map[string]any{"secretName": "zaentrum-tls"}
		e.set("certificate", cert(alertNow))
		e.check()
		if a := e.only(kindCertificate); a.Title != "The certificate has ended" {
			t.Fatalf("at its end: %q", a.Title)
		}
	})
}

// R.C8: no good backup — the last one failed, the newest good one is older
// than the setting, or none was ever made in a platform older than it — each
// with its title and text; it ends when a backup was made, when none of the
// three holds and none was made, or when backups were switched off or the
// databases made external, and is not watched there.
func TestRC8NoGoodBackup(t *testing.T) {
	failedAt, goodAt := "2026-11-16T00:00:05Z", "2026-11-15T00:00:07Z"
	backup := func(reason string) map[string]any { return cond("Backup", "False", reason, "") }
	t.Run("R.C8 the last backup failed, and ends only once none holds", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.status["conditions"] = []any{backup("Failed")}
		e.set("backup", map[string]any{"lastFailure": failedAt, "lastSuccess": goodAt})
		e.check()
		a := e.only(kindBackup)
		if a.About != "backups" || a.Title != "The last backup failed" || a.Link != "/portal/backups" ||
			a.Text != "The backup of 16 November 2026, 00:00 UTC failed. The newest good one is from 15 November 2026, 00:00 UTC. Open backups to see why." {
			t.Fatalf("started as %q %q", a.Title, a.Text)
		}
		// The newest good one is within the setting: it holds while the last
		// one failed.
		e.later(time.Hour)
		e.check()
		if a = e.only(kindBackup); a.EndedAt != nil {
			t.Fatalf("it ended while the last backup failed: %q", a.EndText)
		}
		e.status["conditions"] = []any{cond("Backup", "True", "Succeeded", "")}
		e.set("backup", map[string]any{"lastFailure": failedAt, "lastSuccess": "2026-11-16T12:30:00Z"})
		e.check()
		if a = e.only(kindBackup); a.EndText != "A backup was made on 16 November 2026, 12:30 UTC." {
			t.Fatalf("ended as %q", a.EndText)
		}
	})
	t.Run("R.C8 one that failed with no good one yet", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.status["conditions"] = []any{backup("Failed")}
		e.set("backup", map[string]any{"lastFailure": failedAt})
		e.check()
		if a := e.only(kindBackup); a.Text != "The backup of 16 November 2026, 00:00 UTC failed. There is no good backup yet. Open backups to see why." {
			t.Fatalf("%q", a.Text)
		}
	})
	t.Run("R.C8 the newest is older than the setting, the days rounded down", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.created = alertNow.Add(-30 * 24 * time.Hour)
		e.status["conditions"] = []any{cond("Backup", "True", "Succeeded", "")}
		e.set("backup", map[string]any{"lastSuccess": "2026-11-13T00:00:07Z"})
		e.check()
		if a := e.only(kindBackup); a.Title != "No backup for 3 days" ||
			a.Text != "The newest backup is from 13 November 2026, 00:00 UTC. Open backups to see why none was made since." {
			t.Fatalf("%q %q", a.Title, a.Text)
		}
		e2 := newAlertEnv(t, mailtest.Options{})
		e2.spec["alerts"] = map[string]any{"backupAgeDays": 1}
		e2.set("backup", map[string]any{"lastSuccess": "2026-11-15T00:00:07Z"})
		e2.check()
		if a := e2.only(kindBackup); a.Title != "No backup for 1 day" {
			t.Fatalf("%q", a.Title)
		}
	})
	t.Run("R.C8 none of the three holds any more and none was made", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			backup map[string]any
		}{
			{"the newest one", map[string]any{"lastSuccess": "2026-11-13T00:00:07Z"}},
			{"none ever made", map[string]any{}},
		} {
			e := newAlertEnv(t, mailtest.Options{})
			e.created = alertNow.Add(-5 * 24 * time.Hour)
			e.status["conditions"] = []any{cond("Backup", "Unknown", "Scheduled", "")}
			e.set("backup", c.backup)
			e.check()
			e.only(kindBackup)
			e.spec["alerts"] = map[string]any{"backupAgeDays": 30}
			e.put()
			e.check()
			if a := e.only(kindBackup); a.EndText != "The backups are within the setting again." {
				t.Errorf("%s, the setting raised: ended as %q", c.name, a.EndText)
			}
		}
	})
	t.Run("R.C8 none was ever made", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.created = time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)
		e.status["conditions"] = []any{cond("Backup", "Unknown", "Scheduled", "")}
		delete(e.status, "backup")
		e.put()
		e.check()
		if a := e.only(kindBackup); a.Title != "No backup for 5 days" ||
			a.Text != "No backup has been made since the platform was installed on 11 November 2026, 09:30 UTC. Open backups to see why." {
			t.Fatalf("%q %q", a.Title, a.Text)
		}
		// A platform younger than the setting raises none.
		e2 := newAlertEnv(t, mailtest.Options{})
		e2.created = alertNow.Add(-36 * time.Hour)
		delete(e2.status, "backup")
		e2.put()
		e2.check()
		e2.none(kindBackup)
	})
	t.Run("R.C8 backups switched off, and databases made external", func(t *testing.T) {
		for _, c := range []struct {
			name, want string
			change     func(e *alertEnv)
		}{
			{"off", "Backups are off, so they are not watched now.", func(e *alertEnv) {
				e.spec["backup"] = map[string]any{"enabled": false}
				e.status["conditions"] = []any{backup("Disabled")}
			}},
			{"external", "The databases run outside the platform now, so their backups are not watched here.", func(e *alertEnv) {
				e.spec["databases"] = map[string]any{"mode": "external"}
				e.status["conditions"] = []any{}
				delete(e.status, "backup")
			}},
		} {
			e := newAlertEnv(t, mailtest.Options{})
			e.status["conditions"] = []any{backup("Failed")}
			e.set("backup", map[string]any{"lastFailure": failedAt})
			e.check()
			c.change(e)
			e.put()
			e.check()
			if a := e.only(kindBackup); a.EndText != c.want {
				t.Errorf("%s: ended as %q", c.name, a.EndText)
			}
			// Not watched there: none starts.
			e.later(48 * time.Hour)
			e.check()
			if n := len(e.of(kindBackup)); n != 1 {
				t.Errorf("%s: %d alerts", c.name, n)
			}
		}
	})
}

// verification is status.verification of a check that ended as result.
func verification(result, trigger, version string, started time.Time, failed ...string) map[string]any {
	checks := []any{}
	for _, f := range failed {
		checks = append(checks, map[string]any{"name": f, "status": "fail"})
	}
	return map[string]any{
		"result": result, "trigger": trigger, "version": version, "startedAt": started.Format(time.RFC3339),
		"passed": 12, "failed": len(failed), "warned": 0, "skipped": 0, "checks": checks, "message": "the operator's own line",
	}
}

// R.C8: an update failed — a rollout that has given up, whatever started it,
// named as an update to a version the last check did not run against, else
// as a rollout of it, with the parts whose own rollout has given up; or the
// check after an update failed, which then ends only once a later check has
// passed, whatever started that one. While the phase is Restoring it neither
// starts nor ends.
func TestRC8AnUpdateThatFailed(t *testing.T) {
	stalled := func(e *alertEnv, message string, parts ...any) {
		e.status["phase"] = "Degraded"
		e.status["conditions"] = []any{cond("Ready", "False", "RolloutStalled", message)}
		e.set("components", parts)
	}
	ready := func(e *alertEnv) {
		e.status["phase"] = "Ready"
		e.status["conditions"] = []any{cond("Ready", "True", "AllComponentsReady", "all components are ready")}
		e.set("components", []any{part("katalog-api", true, time.Time{}), part("chino-api", true, time.Time{})})
	}
	since := alertNow.Add(-15 * time.Minute)
	t.Run("R.C8 an update that did not finish", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.status["verification"] = verification("Passed", "update", "v0.5.0", alertNow.Add(-24*time.Hour))
		stalled(e, "rollout stalled: katalog-api (ProgressDeadlineExceeded)",
			part("transcoder", false, since), stalledPart("katalog-api", false, since), stalledPart("chino-api", false, since))
		e.check()
		a := e.only(kindUpdate)
		if a.About != "update" || a.Title != "The update to v0.6.0 did not finish" || a.Link != "/portal/operator" ||
			a.Text != "These parts could not start: katalog-api and chino-api. What ran before keeps running where it can. Open operator to see why." {
			t.Fatalf("%q %q", a.Title, a.Text)
		}
		if v := valuesOf(t, a.Values); v["version"] != "v0.6.0" || !slices.Equal(anyStrings(v["parts"]), []string{"katalog-api", "chino-api"}) {
			t.Errorf("values %v", v)
		}
		// Its parts are named here; a part that is not ready of itself raises
		// its own.
		if parts := e.of(kindPart); len(parts) != 1 || parts[0].About != "transcoder" {
			t.Fatalf("part alerts %+v", parts)
		}
		// The check of the version the rollout brings runs once it has
		// finished: the end still names an update, as the title did.
		e.status["verification"] = verification("Running", "update", "v0.6.0", alertNow)
		ready(e)
		e.check()
		if a = e.only(kindUpdate); a.EndText != "The update to v0.6.0 has finished." {
			t.Fatalf("ended as %q", a.EndText)
		}
	})
	t.Run("R.C8 a rollout of the version last checked", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.status["verification"] = verification("Passed", "update", "v0.6.0", alertNow.Add(-24*time.Hour))
		// A part whose old pods still serve reads ready: the condition names
		// it.
		stalled(e, "rollout stalled: katalog-api (ProgressDeadlineExceeded), chino-api (ProgressDeadlineExceeded)",
			stalledPart("katalog-api", true, time.Time{}), stalledPart("chino-api", true, time.Time{}))
		e.check()
		a := e.only(kindUpdate)
		if a.Title != "The rollout of v0.6.0 did not finish" ||
			a.Text != "These parts could not start: katalog-api and chino-api. What ran before keeps running where it can. Open operator to see why." {
			t.Fatalf("%q %q", a.Title, a.Text)
		}
		ready(e)
		e.check()
		if a = e.only(kindUpdate); a.EndText != "The rollout of v0.6.0 has finished." {
			t.Fatalf("ended as %q", a.EndText)
		}
	})
	t.Run("R.C8 more parts than fit", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		var parts []any
		for i := 0; i < 30; i++ {
			parts = append(parts, stalledPart("a-part-with-a-long-name-"+string(rune('a'+i%26))+strings.Repeat("x", i/26), false, since))
		}
		stalled(e, "rollout stalled: …", parts...)
		e.check()
		a := e.only(kindUpdate)
		if n := utf8.RuneCountInString(a.Text); n > 280 || !strings.Contains(a.Text, " more. What ran before keeps running where it can. Open operator to see why.") {
			t.Fatalf("%d characters: %q", n, a.Text)
		}
		v := valuesOf(t, a.Values)
		if named, more := len(anyStrings(v["parts"])), v["more"]; more == nil || named+int(more.(float64)) != 30 || !strings.Contains(a.Text, " and "+strconv.Itoa(int(more.(float64)))+" more") {
			t.Fatalf("values %v for %q", v, a.Text)
		}
	})
	t.Run("R.C8 one part", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		stalled(e, "rollout stalled: katalog-api (ProgressDeadlineExceeded)", stalledPart("katalog-api", false, since), part("chino-api", true, time.Time{}))
		e.check()
		if a := e.only(kindUpdate); a.Text != "This part could not start: katalog-api. What ran before keeps running where it can. Open operator to see why." {
			t.Fatalf("%q", a.Text)
		}
	})
	t.Run("R.C8 under an operator that says not which parts, the rollout names none", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		stalled(e, "rollout stalled: katalog-api (ProgressDeadlineExceeded)", part("katalog-api", true, time.Time{}), part("chino-api", true, time.Time{}))
		e.check()
		a := e.only(kindUpdate)
		if a.Title != "The update to v0.6.0 did not finish" || a.Text != "The rollout did not finish. What ran before keeps running where it can. Open operator to see why." {
			t.Fatalf("%q %q", a.Title, a.Text)
		}
		if v := valuesOf(t, a.Values); v["version"] != "v0.6.0" || len(anyStrings(v["parts"])) != 0 || v["more"] != nil {
			t.Errorf("values %v", v)
		}
	})
	t.Run("R.C8 while the phase is Restoring it neither starts nor ends", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		stalled(e, "rollout stalled: katalog-api (ProgressDeadlineExceeded)", stalledPart("katalog-api", false, since))
		e.check()
		a := e.only(kindUpdate)
		// A restore begins: Ready reads Restoring, and every part is stopped.
		e.status["phase"] = "Restoring"
		e.status["conditions"] = []any{cond("Ready", "False", "Restoring", "the databases are being restored")}
		e.set("components", []any{part("katalog-api", false, time.Time{})})
		e.check()
		if b := e.only(kindUpdate); b.EndedAt != nil {
			t.Fatalf("ended during the restore: %q", b.EndText)
		}
		// One that would start waits for the restore's end too.
		e2 := newAlertEnv(t, mailtest.Options{})
		e2.status["phase"] = "Restoring"
		e2.status["conditions"] = []any{cond("Ready", "False", "RolloutStalled", "rollout stalled: katalog-api (ProgressDeadlineExceeded)")}
		e2.set("verification", verification("Failed", "update", "v0.6.0", alertNow.Add(-time.Hour), "tls"))
		e2.check()
		e2.none(kindUpdate)
		// Once the restore has ended, the rules hold again.
		ready(e)
		e.check()
		if b := e.only(kindUpdate); b.ID != a.ID || b.EndText != "The update to v0.6.0 has finished." {
			t.Fatalf("after the restore: %+v", b)
		}
	})
	t.Run("R.C8 the check after an update failed, and a later check passed", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("verification", verification("Failed", "update", "v0.6.0", alertNow.Add(-time.Hour), "oidc-login", "tls"))
		e.check()
		a := e.only(kindUpdate)
		if a.Title != "The check after the update to v0.6.0 failed" || a.Text != "2 of 14 checks failed: oidc-login and tls. Open operator to see the checks." {
			t.Fatalf("%q %q", a.Title, a.Text)
		}
		// A check asked for runs, and fails too: it goes on.
		e.later(10 * time.Minute)
		e.set("verification", verification("Running", "request", "v0.6.0", e.clock))
		e.check()
		e.set("verification", verification("Failed", "request", "v0.6.0", e.clock, "tls"))
		e.check()
		if a = e.only(kindUpdate); a.EndedAt != nil || a.Text != "1 of 13 checks failed: tls. Open operator to see the checks." {
			t.Fatalf("after a check asked for failed: %+v", a)
		}
		e.later(10 * time.Minute)
		e.set("verification", verification("Passed", "request", "v0.6.0", e.clock))
		e.check()
		if a = e.only(kindUpdate); a.EndText != "The check has passed." {
			t.Fatalf("ended as %q", a.EndText)
		}
	})
	t.Run("R.C8 a check without a result, and one asked for that failed", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		check := verification("Error", "update", "v0.6.0", alertNow.Add(-time.Hour))
		check["passed"] = 0
		e.set("verification", check)
		e.check()
		if a := e.only(kindUpdate); a.Title != "The check after the update to v0.6.0 failed" || a.Text != "The check could not reach a result. Open operator to see why." {
			t.Fatalf("%q %q", a.Title, a.Text)
		}
		e2 := newAlertEnv(t, mailtest.Options{})
		e2.set("verification", verification("Failed", "request", "v0.6.0", alertNow.Add(-time.Hour), "tls"))
		e2.check()
		e2.none(kindUpdate)
		one := verification("Failed", "update", "v0.6.0", alertNow.Add(-time.Hour), "tls")
		one["passed"] = 0
		e2.set("verification", one)
		e2.check()
		if a := e2.only(kindUpdate); a.Text != "1 of 1 check failed: tls. Open operator to see the checks." {
			t.Fatalf("%q", a.Text)
		}
	})
}

// R.C8: a platform whose resource is younger than 1 hour raises no alert; one
// the operator does not run raises none, and GET /alerts says so and nothing
// else; where the resource cannot be read nothing starts or ends, and GET
// answers the cluster's refusal.
func TestRC8WhereNoAlertIsRaised(t *testing.T) {
	t.Run("R.C8 a resource younger than 1 hour", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.created = alertNow.Add(-59 * time.Minute)
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		e.none(kindPart)
		e.later(time.Minute)
		e.check()
		e.only(kindPart)
	})
	t.Run("R.C8 a platform the operator does not run", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.kube.Remove("zaentrums", "zaentrum")
		e.check()
		if n := len(e.alerts.all()); n != 0 {
			t.Fatalf("%d alerts", n)
		}
		rec := e.do(adminPortal, http.MethodGet, "/api/portal/alerts", nil)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"available":false,"reason":"no-operator"}` {
			t.Fatalf("GET = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("R.C8 a resource that cannot be read", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		e.kube.Forbidden["zaentrums"] = true
		e.set("components", []any{part("katalog-api", true, time.Time{})})
		e.check()
		if a := e.only(kindPart); a.EndedAt != nil {
			t.Fatalf("ended while the resource could not be read: %+v", a)
		}
		rec := e.do(adminPortal, http.MethodGet, "/api/portal/alerts", nil)
		var why refusal
		_ = json.Unmarshal(rec.Body.Bytes(), &why)
		if rec.Code != http.StatusServiceUnavailable || why.Error != "platform_read_forbidden" ||
			why.Message != "The portal is not allowed to read the platform's settings. Update the operator, which gives the portal that right." || why.Detail == "" {
			t.Fatalf("GET = %d %s", rec.Code, rec.Body)
		}
	})
}

// R.C8: GET /alerts answers each alert with its fields and the values beside
// its title and text, the active ones first and then the newest; unseen, what
// the bell shows that the admin has not seen; and the five that are watched,
// each with its setting and its range, and what the status says now.
func TestRC8GetAlerts(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.spec["alerts"] = map[string]any{"volumeFullPercent": 80}
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour)), part("chino-api", true, time.Time{}), part("transcoder", false, time.Time{})})
	e.check()
	rec := e.do(adminPortal, http.MethodGet, "/api/portal/alerts", nil)
	if got := keys(fieldsOf(t, rec)); !slices.Equal(got, []string{"alerts", "available", "unseen", "watched"}) {
		t.Fatalf("fields %v", got)
	}
	var raw struct {
		Alerts []map[string]json.RawMessage `json:"alerts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if len(raw.Alerts) != 1 || !slices.Equal(keys(raw.Alerts[0]), []string{"about", "endedAt", "id", "kind", "link", "mail", "seen", "startedAt", "text", "title", "values"}) {
		t.Fatalf("an alert's fields: %v", raw.Alerts)
	}
	got := e.get()
	if !got.Available || got.Unseen != 1 || len(got.Alerts) != 1 || got.Alerts[0].Kind != kindPart || got.Alerts[0].Seen ||
		got.Alerts[0].Values["part"] != "katalog-api" || !got.Alerts[0].StartedAt.Equal(alertNow) {
		t.Fatalf("%+v", got)
	}
	wantWatched := []struct {
		kind, state             string
		setting, least, most, n int
	}{
		{kindPart, nowPartsNotReady, 10, 1, 1440, 1},
		{kindVolume, nowUpdateForVolumes, 80, 50, 99, -1},
		{kindCertificate, nowNoCertificate, 14, 1, 90, -1},
		{kindBackup, nowNewestBackup, 2, 1, 30, -1},
		{kindUpdate, nowVersionRuns, -1, -1, -1, -1},
	}
	if len(got.Watched) != 5 {
		t.Fatalf("watched %+v", got.Watched)
	}
	for i, w := range wantWatched {
		g := got.Watched[i]
		if g.Kind != w.kind || g.Now.State != w.state {
			t.Errorf("watched[%d] = %s %s, want %s %s", i, g.Kind, g.Now.State, w.kind, w.state)
		}
		if w.setting >= 0 && (g.Setting == nil || *g.Setting != w.setting || *g.Least != w.least || *g.Most != w.most) {
			t.Errorf("%s setting %v %v %v", g.Kind, g.Setting, g.Least, g.Most)
		}
		if w.setting < 0 && (g.Setting != nil || g.Least != nil || g.Most != nil) {
			t.Errorf("%s has a setting", g.Kind)
		}
		if w.n >= 0 && g.Now.Values["n"] != float64(w.n) {
			t.Errorf("%s now %v", g.Kind, g.Now.Values)
		}
	}
	if got.Watched[4].Now.Values["version"] != "v0.6.0" {
		t.Errorf("update now %v", got.Watched[4].Now.Values)
	}
}

// R.C8: what is watched says, for each alert, what the status says now, or
// why the alert is not watched — under an operator older than a fact, to
// update the operator.
func TestRC8WhatIsWatchedNow(t *testing.T) {
	now := func(e *alertEnv, kind string) (string, map[string]any) {
		for _, w := range e.get().Watched {
			if w.Kind == kind {
				return w.Now.State, w.Now.Values
			}
		}
		t.Fatalf("no %s watched", kind)
		return "", nil
	}
	e := newAlertEnv(t, mailtest.Options{})
	e.set("components", []any{part("katalog-api", true, time.Time{}), part("chino-api", true, time.Time{}), part("transcoder", false, time.Time{})})
	if s, v := now(e, kindPart); s != nowPartsReady || v["n"] != float64(2) {
		t.Errorf("parts %s %v", s, v)
	}
	gb := int64(1e9)
	e.set("storage", map[string]any{"volumes": []any{map[string]any{"name": "media", "claim": "media", "requestBytes": 100 * gb}}})
	if s, _ := now(e, kindVolume); s != nowNotMeasured {
		t.Errorf("volumes %s", s)
	}
	e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 100*gb, 38*gb), volume("backups", "node-1:9", 10*gb, 9*gb)}})
	if s, v := now(e, kindVolume); s != nowFullest || v["n"] != float64(62) {
		t.Errorf("volumes %s %v", s, v)
	}
	e.spec["tls"] = map[string]any{"secretName": "zaentrum-tls"}
	e.status["conditions"] = []any{cond("TLS", "False", "SecretMissing", "there is no Secret zaentrum-tls")}
	e.put()
	if s, _ := now(e, kindCertificate); s != nowCertificateUnread {
		t.Errorf("certificate %s", s)
	}
	e.status["conditions"] = []any{cond("TLS", "True", "Certificate", "served")}
	e.put()
	if s, _ := now(e, kindCertificate); s != nowUpdateForCertificate {
		t.Errorf("certificate under an older operator %s", s)
	}
	e.set("certificate", map[string]any{"notAfter": "2027-01-01T00:00:00Z", "names": []any{"zaentrum.example.org"}})
	if s, v := now(e, kindCertificate); s != nowCertificateEnds || v["date"] != "2027-01-01T00:00:00Z" {
		t.Errorf("certificate %s %v", s, v)
	}
	e.set("backup", map[string]any{"lastSuccess": "2026-11-16T00:00:07Z"})
	if s, v := now(e, kindBackup); s != nowNewestBackup || v["date"] != "2026-11-16T00:00:07Z" {
		t.Errorf("backups %s %v", s, v)
	}
	e.status["conditions"] = []any{cond("Backup", "False", "Disabled", "")}
	e.put()
	if s, _ := now(e, kindBackup); s != nowDatabasesNoVolume {
		t.Errorf("backups on an emptyDir %s", s)
	}
	e.spec["backup"] = map[string]any{"enabled": false}
	e.put()
	if s, _ := now(e, kindBackup); s != nowBackupsOff {
		t.Errorf("backups off %s", s)
	}
	e.spec["databases"] = map[string]any{"mode": "external"}
	e.put()
	if s, _ := now(e, kindBackup); s != nowDatabasesExternal {
		t.Errorf("external %s", s)
	}
	// An operator whose schema has no alert settings does not say since when
	// a part is not ready, and measures nothing.
	old := newAlertEnv(t, mailtest.Options{})
	old.kube.Unknown = func(plural string, obj map[string]any) []string {
		if spec, _ := obj["spec"].(map[string]any); spec["alerts"] != nil {
			return []string{"spec.alerts"}
		}
		return nil
	}
	old.set("components", []any{part("katalog-api", true, time.Time{})})
	if s, _ := now(old, kindPart); s != nowUpdateForParts {
		t.Errorf("parts under an older operator %s", s)
	}
	if s, _ := now(old, kindVolume); s != nowUpdateForVolumes {
		t.Errorf("volumes under an older operator %s", s)
	}
	if spec, _ := old.kube.Object("zaentrums", "zaentrum")["spec"].(map[string]any); spec["alerts"] != nil {
		t.Errorf("the question wrote spec.alerts: %v", spec)
	}
}

// R.C8: a value that would take a title past 80 characters or a text past
// 280 is cut, and ends with “…”; a count of one is in the singular; sizes are
// written as the storage page writes them.
func TestRC8TheAlertsWords(t *testing.T) {
	long := strings.Repeat("x", 100)
	if s := partTitle(long); utf8.RuneCountInString(s) != 80 || !strings.HasSuffix(s, "x…") || !strings.HasPrefix(s, "A part is not ready: x") {
		t.Errorf("title %q", s)
	}
	if s := partText(strings.Repeat("y", 300), 10); utf8.RuneCountInString(s) != 280 || !strings.Contains(s, "y… has not been ready for more than 10 minutes.") {
		t.Errorf("text %q", s)
	}
	if s := updateTitle(strings.Repeat("v", 128), false); utf8.RuneCountInString(s) != 80 || !strings.HasSuffix(s, "… did not finish") {
		t.Errorf("update title %q", s)
	}
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0 MB"}, {1, "1 MB"}, {400_000, "1 MB"}, {724_000_000, "724 MB"}, {999_400_000, "999 MB"}, {999_600_000, "1 GB"},
		{1_400_000_000, "1.4 GB"}, {18_000_000_000, "18 GB"}, {999_960_000_000, "1 TB"}, {2_500_000_000_000, "2.5 TB"},
	} {
		if got := strings.ReplaceAll(sizeText(c.n), nbsp, " "); got != c.want {
			t.Errorf("sizeText(%d) = %q, want %q", c.n, got, c.want)
		}
		if !strings.Contains(sizeText(c.n), nbsp) {
			t.Errorf("sizeText(%d) has no space that does not break", c.n)
		}
	}
	if got := []string{counted(1, "day", "days"), counted(2, "day", "days"), counted(0, "day", "days")}; !slices.Equal(got, []string{"1 day", "2 days", "0 days"}) {
		t.Errorf("counted %v", got)
	}
	if got := joined([]string{"library", "databases", "backups"}); got != "library, databases and backups" {
		t.Errorf("joined %q", got)
	}
}

// R.C8: alert mail goes to the admins whose account is enabled and has an
// address, as the mail page lists them under alerts go to: a disabled
// account is listed there no more.
func TestRC8AlertsGoToTheEnabledAdmins(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.kc.Add(kctest.Seed{Username: "carl", FirstName: "Carl", Email: "carl@example.org", Disabled: true, Roles: []string{kctest.AdminRole, kctest.UserRole}})
	e.kc.Add(kctest.Seed{Username: "bert", FirstName: "Bert", Email: "bert@example.org", Roles: []string{kctest.AdminRole, kctest.UserRole}})
	doc := e.getMail()
	var names []string
	for _, a := range doc.Recipients.Admins {
		names = append(names, a.Name+" <"+a.Address+">")
	}
	if !slices.Equal(names, []string{"Anna <>", "Bert <bert@example.org>", "Zaentrum Administrator <owner@example.org>"}) {
		t.Fatalf("alerts go to %v", names)
	}
}

// patch is PATCH /alerts/settings as the admin, and the refusal it answered,
// one with no code where it answered none.
func (e *alertEnv) patch(body any) (int, refusal, string) {
	e.t.Helper()
	rec := e.do(adminPortal, http.MethodPatch, "/api/portal/alerts/settings", body)
	var why refusal
	_ = json.Unmarshal(rec.Body.Bytes(), &why)
	return rec.Code, why, rec.Body.String()
}

// alertsSpec is spec.alerts as the resource holds it.
func (e *alertEnv) alertsSpec() map[string]any {
	spec, _ := e.kube.Object("zaentrums", "zaentrum")["spec"].(map[string]any)
	a, _ := spec["alerts"].(map[string]any)
	return a
}

// R.C8: PATCH /alerts/settings writes any of the four thresholds and mail
// into spec.alerts, validated strictly, and answers the generation; it checks
// the ranges itself, refusing a value outside its range or of the wrong kind
// with its range; the settings it wrote are the next check's.
func TestRC8TheAlertSettings(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	logs := captureLog(t)
	code, _, body := e.patch(map[string]any{"partNotReadyMinutes": 5, "mail": false})
	if code != http.StatusOK || strings.TrimSpace(body) != `{"generation":2}` {
		t.Fatalf("PATCH = %d %s", code, body)
	}
	if a := e.alertsSpec(); a["partNotReadyMinutes"] != json.Number("5") || a["mail"] != false || len(a) != 2 {
		t.Fatalf("spec.alerts %v", a)
	}
	strict := false
	for _, c := range e.kube.Calls() {
		if c.Method == http.MethodPatch && strings.Contains(c.Query, "fieldValidation=Strict") && !strings.Contains(c.Query, "dryRun") {
			strict = true
		}
	}
	if !strict {
		t.Error("the write was not validated strictly")
	}
	if !strings.Contains(logs.String(), "alerts: admin saved the alert settings") {
		t.Errorf("the log:\n%s", logs)
	}
	// Another write keeps what it does not name.
	if code, _, body := e.patch(map[string]any{"volumeFullPercent": 80}); code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", code, body)
	}
	if a := e.alertsSpec(); a["partNotReadyMinutes"] != json.Number("5") || a["volumeFullPercent"] != json.Number("80") || a["mail"] != false {
		t.Fatalf("spec.alerts %v", a)
	}
	// The next check counts by them.
	e.spec, _ = e.kube.Object("zaentrums", "zaentrum")["spec"].(map[string]any)
	e.set("components", []any{part("katalog-api", false, alertNow.Add(-5*time.Minute-time.Second))})
	e.check()
	if a := e.only(kindPart); a.Text != "katalog-api has not been ready for more than 5 minutes. Open operator to see why." {
		t.Fatalf("%q", a.Text)
	}
	if state, _, _, _ := e.mailOf(kindPart); state != mailStateOff {
		t.Fatalf("mail %s", state)
	}
	if w := e.get().Watched; *w[0].Setting != 5 || *w[1].Setting != 80 || *w[2].Setting != 14 || *w[3].Setting != 2 {
		t.Fatalf("watched settings %d %d %d %d", *w[0].Setting, *w[1].Setting, *w[2].Setting, *w[3].Setting)
	}

	before := len(e.writes())
	for _, c := range []struct {
		body        map[string]any
		field       string
		least, most int
	}{
		{map[string]any{"partNotReadyMinutes": 0}, "partNotReadyMinutes", 1, 1440},
		{map[string]any{"partNotReadyMinutes": 1441}, "partNotReadyMinutes", 1, 1440},
		{map[string]any{"volumeFullPercent": 49}, "volumeFullPercent", 50, 99},
		{map[string]any{"volumeFullPercent": 100}, "volumeFullPercent", 50, 99},
		{map[string]any{"certificateDays": 0}, "certificateDays", 1, 90},
		{map[string]any{"certificateDays": 91}, "certificateDays", 1, 90},
		{map[string]any{"backupAgeDays": 0}, "backupAgeDays", 1, 30},
		{map[string]any{"backupAgeDays": 31}, "backupAgeDays", 1, 30},
		{map[string]any{"backupAgeDays": 2.5}, "backupAgeDays", 1, 30},
		{map[string]any{"backupAgeDays": "2"}, "backupAgeDays", 1, 30},
		{map[string]any{"mail": true, "certificateDays": []any{3}}, "certificateDays", 1, 90},
	} {
		code, why, body := e.patch(c.body)
		want := "A number from " + strconv.Itoa(c.least) + " to " + strconv.Itoa(c.most) + "."
		if code != http.StatusBadRequest || why.Error != "alerts_threshold_out_of_range" || why.Message != want ||
			why.Values["field"] != c.field || why.Values["least"] != float64(c.least) || why.Values["most"] != float64(c.most) {
			t.Errorf("%v = %d %s", c.body, code, body)
		}
	}
	for name, b := range map[string]any{"mail of the wrong kind": map[string]any{"mail": "yes"}, "nothing named": map[string]any{}, "a field it does not take": map[string]any{"mail": true, "colour": "red"}} {
		if code, why, body := e.patch(b); code != http.StatusBadRequest || why.Error != "" {
			t.Errorf("%s = %d %s", name, code, body)
		}
	}
	if n := len(e.writes()) - before; n != 0 {
		t.Errorf("refused writes wrote %d times", n)
	}
}

// R.C8: where the cluster does not know alert settings, as under an operator
// from before them, PATCH writes nothing and says so; each other refusal of
// the cluster is answered as on the mail page, with the cluster's words under
// details.
func TestRC8TheAlertSettingsTheClusterRefuses(t *testing.T) {
	unknown := func(plural string, obj map[string]any) []string {
		if spec, _ := obj["spec"].(map[string]any); spec["alerts"] != nil {
			return []string{"spec.alerts"}
		}
		return nil
	}
	for _, c := range []struct {
		name, code, message string
		status              int
		detail              bool
		setUp               func(e *alertEnv)
	}{
		{"the cluster does not know alert settings", "alerts_settings_unknown", "The cluster does not know alert settings. Update the operator, which brings them.",
			http.StatusServiceUnavailable, true, func(e *alertEnv) { e.kube.Unknown = unknown }},
		{"an apiserver that does not validate strictly drops them", "alerts_settings_unknown", "The cluster does not know alert settings. Update the operator, which brings them.",
			http.StatusServiceUnavailable, true, func(e *alertEnv) { e.kube.Unknown, e.kube.NoFieldValidation = unknown, true }},
		{"the cluster refuses the settings", "alerts_write_refused", "The cluster refused the alert settings.", http.StatusUnprocessableEntity, true,
			func(e *alertEnv) { e.kube.Fail = failPatch(http.StatusUnprocessableEntity) }},
		{"the portal may not change them", "alerts_write_forbidden", "The portal is not allowed to change the alert settings. Update the operator, which gives the portal that right.",
			http.StatusServiceUnavailable, true, func(e *alertEnv) { e.kube.Fail = failPatch(http.StatusForbidden) }},
		{"any other answer", "cluster_unexpected_answer", "The cluster did not answer as expected.", http.StatusBadGateway, true,
			func(e *alertEnv) { e.kube.Fail = failPatch(http.StatusInternalServerError) }},
		{"the portal may not read the resource", "platform_read_forbidden", "The portal is not allowed to read the platform's settings. Update the operator, which gives the portal that right.",
			http.StatusServiceUnavailable, true, func(e *alertEnv) { e.kube.Forbidden["zaentrums"] = true }},
		{"the platform is not run by the operator", "alerts_not_run_by_operator", "This platform is not run by the operator, so nothing watches it from here.",
			http.StatusConflict, false, func(e *alertEnv) { e.kube.Remove("zaentrums", "zaentrum") }},
	} {
		t.Run("R.C8 "+c.name, func(t *testing.T) {
			e := newAlertEnv(t, mailtest.Options{})
			captureLog(t)
			c.setUp(e)
			code, why, body := e.patch(map[string]any{"certificateDays": 30})
			if code != c.status || why.Error != c.code || why.Message != c.message || (why.Detail != "") != c.detail {
				t.Fatalf("PATCH = %d %s", code, body)
			}
			if o := e.kube.Object("zaentrums", "zaentrum"); o != nil {
				if spec, _ := o["spec"].(map[string]any); spec["alerts"] != nil {
					t.Fatalf("written: %v", spec["alerts"])
				}
			}
		})
	}
	e := newAlertEnv(t, mailtest.Options{})
	e.api.op = nil
	if code, why, body := e.patch(map[string]any{"mail": true}); code != http.StatusConflict || why.Error != "alerts_not_run_by_operator" {
		t.Fatalf("without an operator: %d %s", code, body)
	}
}

// failPatch fails every PATCH of the resource with code, as the cluster
// would.
func failPatch(code int) func(c k8sfake.Call) (int, string, bool) {
	return func(c k8sfake.Call) (int, string, bool) {
		if c.Method == http.MethodPatch && strings.Contains(c.Path, "/zaentrums/") {
			return code, "patch refused for the test", true
		}
		return 0, "", false
	}
}

// R.C8: GET /alerts answers every active alert, the newest first, then those
// that ended, the newest end first: the newest 500 that ended in the last 90
// days, and more where older ones are kept.
func TestRC8GetAlertsInOrderAndBounded(t *testing.T) {
	t.Run("R.C8 the active ones first, the newest first, then the newest that ended", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		since := alertNow.Add(-time.Hour)
		parts := func(ready ...string) {
			var list []any
			for _, n := range []string{"katalog-api", "transcoder", "chino-api", "packager"} {
				list = append(list, part(n, slices.Contains(ready, n), map[bool]time.Time{false: since}[slices.Contains(ready, n)]))
			}
			e.set("components", list)
			e.check()
			e.later(time.Minute)
		}
		parts("chino-api", "packager")
		parts()
		parts("transcoder")
		parts("transcoder", "packager")
		var got []string
		for _, a := range e.get().Alerts {
			got = append(got, a.About)
		}
		if want := []string{"chino-api", "katalog-api", "packager", "transcoder"}; !slices.Equal(got, want) {
			t.Fatalf("listed %v, want %v", got, want)
		}
	})
	t.Run("R.C8 the newest 500 that ended, and more where older ones are kept", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		e.alerts.mu.Lock()
		for i := 0; i < 501; i++ {
			ended := alertNow.Add(-time.Duration(i+1) * time.Hour)
			e.alerts.rows = append(e.alerts.rows, &fakeAlert{a: model.Alert{
				ID: "10000000-0000-4000-8000-" + strconv.FormatInt(int64(100000000000+i), 10), Kind: kindPart, About: "part-" + strconv.Itoa(i),
				Title: "A part is not ready: part", Text: "part is ready again.", Values: json.RawMessage(`{}`), Link: "/portal/operator",
				StartedAt: ended.Add(-time.Minute), EndedAt: &ended, EndText: "part is ready again.", Mail: json.RawMessage(`{}`),
			}})
		}
		e.alerts.mu.Unlock()
		rec := e.do(adminPortal, http.MethodGet, "/api/portal/alerts", nil)
		var doc struct {
			Alerts []struct {
				About   string
				EndedAt *time.Time
			} `json:"alerts"`
			More *bool `json:"more"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		if len(doc.Alerts) != 501 || doc.Alerts[0].About != "katalog-api" || doc.Alerts[1].About != "part-0" || doc.Alerts[500].About != "part-499" || doc.More == nil || !*doc.More {
			t.Fatalf("%d listed, more %v, first %+v", len(doc.Alerts), doc.More, doc.Alerts[:2])
		}
		// 500 that ended: none is left out, and more is not there.
		e.alerts.mu.Lock()
		e.alerts.rows = e.alerts.rows[:len(e.alerts.rows)-1]
		e.alerts.mu.Unlock()
		if got := keys(fieldsOf(t, e.do(adminPortal, http.MethodGet, "/api/portal/alerts", nil))); slices.Contains(got, "more") {
			t.Fatalf("fields %v", got)
		}
	})
	t.Run("R.C8 read-all marks what the admin had not seen, and answers how many", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour)), part("chino-api", false, alertNow.Add(-time.Hour))})
		e.check()
		got := e.get()
		e.do(adminPortal, http.MethodPost, "/api/portal/alerts/"+got.Alerts[0].ID+"/read", nil)
		rec := e.do(adminPortal, http.MethodPost, "/api/portal/alerts/read-all", nil)
		if strings.TrimSpace(rec.Body.String()) != `{"seen":1,"unseen":0}` {
			t.Fatalf("read-all = %s", rec.Body)
		}
		if rec := e.do(adminPortal, http.MethodPost, "/api/portal/alerts/read-all", nil); strings.TrimSpace(rec.Body.String()) != `{"seen":0,"unseen":0}` {
			t.Fatalf("read-all again = %s", rec.Body)
		}
	})
}

// dryRuns are the dry-run writes of the platform's resource the cluster was
// asked.
func (e *alertEnv) dryRuns() int {
	n := 0
	for _, c := range e.kube.Calls() {
		if c.Method == http.MethodPatch && strings.Contains(c.Query, "dryRun") {
			n++
		}
	}
	return n
}

// R.C8: whether the cluster knows alert settings is asked with a dry run, and
// its answer kept 5 minutes; so is a dry run that was refused or failed for
// another reason, which tells nothing: then it could not be learned, and the
// column now says so. A part the status says it of answers it without one.
func TestRC8WhetherTheClusterKnowsAlertSettingsIsKept(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	e.set("components", []any{part("katalog-api", true, time.Time{})})
	e.get()
	e.get()
	if n := e.dryRuns(); n != 1 {
		t.Fatalf("%d dry runs for two answers", n)
	}
	e.later(5 * time.Minute)
	e.get()
	if n := e.dryRuns(); n != 2 {
		t.Fatalf("%d dry runs 5 minutes later", n)
	}
	e.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
		if c.Method == http.MethodPatch && strings.Contains(c.Query, "dryRun") {
			return http.StatusInternalServerError, "the apiserver is busy", true
		}
		return 0, "", false
	}
	e.later(5 * time.Minute)
	for i := 0; i < 3; i++ {
		if w := e.get().Watched[0]; w.Now.State != nowPartsUnlearned {
			t.Fatalf("after a dry run that failed, parts %s", w.Now.State)
		}
		e.later(time.Minute)
	}
	if n := e.dryRuns(); n != 3 {
		t.Fatalf("%d dry runs: the failed one was not kept", n)
	}
	e.later(2 * time.Minute)
	e.get()
	if n := e.dryRuns(); n != 4 {
		t.Fatalf("%d dry runs 5 minutes after the failed one", n)
	}

	// Under an older operator whose role refuses the patch: a part that is
	// not ready, and that it says nothing of, is not counted as watched.
	old := newAlertEnv(t, mailtest.Options{})
	old.kube.Fail = func(c k8sfake.Call) (int, string, bool) {
		if c.Method == http.MethodPatch && strings.Contains(c.Query, "dryRun") {
			return http.StatusForbidden, "patching is not allowed", true
		}
		return 0, "", false
	}
	old.set("components", []any{part("katalog-api", true, time.Time{}), part("katalog-stream", false, time.Time{})})
	if w := old.get().Watched[0]; w.Now.State != nowPartsUnlearned {
		t.Fatalf("a refused dry run: parts %s %v", w.Now.State, w.Now.Values)
	}

	// A part the status says since when of answers it: no dry run, which
	// would have been refused.
	old.set("components", []any{part("katalog-api", true, time.Time{}), part("katalog-stream", false, alertNow.Add(-time.Minute))})
	old.later(5 * time.Minute)
	before := old.dryRuns()
	if w := old.get().Watched[0]; w.Now.State != nowPartsNotReady || w.Now.Values["n"] != float64(1) {
		t.Fatalf("a part that says since when: parts %s %v", w.Now.State, w.Now.Values)
	}
	if n := old.dryRuns(); n != before {
		t.Fatalf("%d dry runs where a part answered it", n-before)
	}
}

// R.C8: a call of the portal's database that fails is said in the log, and
// the other alerts carry on; GET answers 500.
func TestRC8AFailedCallOfThePortalsDatabase(t *testing.T) {
	gb := int64(1e9)
	t.Run("R.C8 the active alerts cannot be read: none starts or ends", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		logs := captureLog(t)
		e.alerts.failed = errors.New("the database\ndid not answer")
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		if !strings.Contains(logs.String(), "alerts: the active alerts could not be read, so none starts or ends: the database did not answer\n") {
			t.Errorf("the log:\n%s", logs)
		}
		if rec := e.do(adminPortal, http.MethodGet, "/api/portal/alerts", nil); rec.Code != http.StatusInternalServerError {
			t.Errorf("GET = %d %s", rec.Code, rec.Body)
		}
		e.alerts.failed = nil
		e.check()
		e.only(kindPart)
	})
	t.Run("R.C8 one alert that cannot be written: the others carry on", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		logs := captureLog(t)
		refused := errors.New("the database refused it")
		e.alerts.fail = func(method string, a model.Alert) error {
			switch {
			case method == "StartAlert" && a.About == "katalog-api", method == "EndAlert" && a.About == "chino-api",
				method == "RewriteAlert" && a.Kind == kindVolume:
				return refused
			}
			return nil
		}
		since := alertNow.Add(-time.Hour)
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 100*gb, 5*gb)}})
		e.set("components", []any{part("katalog-api", false, since), part("chino-api", false, since)})
		e.check()
		if got := e.of(kindPart); len(got) != 1 || got[0].About != "chino-api" {
			t.Fatalf("started %+v", got)
		}
		e.only(kindVolume)
		e.set("storage", map[string]any{"volumes": []any{volume("media", "node-1:41", 100*gb, 4*gb)}})
		e.set("components", []any{part("katalog-api", false, since), part("chino-api", true, time.Time{}), part("transcoder", true, time.Time{})})
		e.check()
		if a := e.only(kindVolume); !strings.HasPrefix(a.Text, "The library volume is 95 %") {
			t.Errorf("written again: %q", a.Text)
		}
		if a := e.of(kindPart); len(a) != 1 || a[0].EndedAt != nil {
			t.Errorf("parts %+v", a)
		}
		for _, want := range []string{
			"alerts: part-not-ready about katalog-api could not be started: the database refused it",
			"alerts: part-not-ready about chino-api could not be ended: the database refused it",
			"alerts: volume-full about media could not be written again: the database refused it",
		} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("the log lacks %q:\n%s", want, logs)
			}
		}
		// The next check writes what this one could not.
		e.alerts.fail = nil
		e.check()
		if a := e.of(kindPart); len(a) != 2 {
			t.Errorf("parts %+v", a)
		}
	})
	t.Run("R.C8 the mails' calls that fail", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		logs := captureLog(t)
		refused := errors.New("the database refused it")
		failing := map[string]bool{"StartMailedWithin": true, "SweepAlerts": true}
		e.alerts.fail = func(method string, a model.Alert) error {
			if failing[method] {
				return refused
			}
			return nil
		}
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		if to, _ := e.mailed(); len(to) != 2 {
			t.Errorf("whether it was mailed could not be read: mailed to %v", to)
		}
		failing = map[string]bool{"DueMail": true, "KeepMail": true}
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour)), part("chino-api", false, alertNow.Add(-time.Hour))})
		e.check()
		if to, _ := e.mailed(); len(to) != 2 {
			t.Errorf("what became of it could not be kept: mailed to %v", to)
		}
		for _, want := range []string{
			"alerts: whether a start was mailed in the last 24 hours could not be read, so it is mailed: the database refused it",
			"alerts: the alerts past their keeping stay for now: the database refused it",
			"alerts: the tries of the alerts' mails could not be read: the database refused it",
			"alerts: what became of an alert's mail could not be kept: the database refused it",
		} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("the log lacks %q:\n%s", want, logs)
			}
		}
		// Once its lease has ended, the mail is tried by whichever
		// portal-api runs then.
		e.alerts.fail = nil
		e.later(alertMailHold)
		e.check()
		if to, _ := e.mailed(); len(to) != 4 {
			t.Errorf("mailed to %v", to)
		}
	})
}

// R.C8: a control character in a value from the status, a line break among
// them, is written as a space, in a title, a text and a log line alike.
func TestRC8AControlCharacterIsWrittenAsASpace(t *testing.T) {
	e := newAlertEnv(t, mailtest.Options{})
	logs := captureLog(t)
	e.set("components", []any{part("katalog\napi", false, alertNow.Add(-time.Hour))})
	e.set("verification", verification("Failed", "update", "v0.6.0\t", alertNow.Add(-time.Hour), "tls\r\nlogin"))
	e.check()
	a := e.only(kindPart)
	if a.About != "katalog api" || a.Title != "A part is not ready: katalog api" || !strings.HasPrefix(a.Text, "katalog api has not been ready") {
		t.Errorf("%q %q %q", a.About, a.Title, a.Text)
	}
	u := e.only(kindUpdate)
	if u.Title != "The check after the update to v0.6.0 failed" || u.Text != "1 of 13 checks failed: tls  login. Open operator to see the checks." {
		t.Errorf("%q %q", u.Title, u.Text)
	}
	if !strings.Contains(logs.String(), "alerts: part-not-ready about katalog api started\n") || strings.ContainsAny(strings.ReplaceAll(logs.String(), "\n", ""), "\r\t\a") {
		t.Errorf("the log:\n%q", logs)
	}
}

// R.C8: a value cut to fit a title or a text keeps whole characters, however
// many bytes each takes.
func TestRC8TheCutKeepsWholeCharacters(t *testing.T) {
	for _, c := range []struct {
		got  string
		most int
		tail string
	}{
		{partTitle(strings.Repeat("ä", 100)), 80, "ä…"},
		{partTitle(strings.Repeat("日本", 50)), 80, "…"},
		{partText(strings.Repeat("😀", 300), 10), 280, "😀… has not been ready for more than 10 minutes. Open operator to see why."},
		{updateTitle(strings.Repeat("ü", 128), true), 80, "ü… did not finish"},
	} {
		if !utf8.ValidString(c.got) || utf8.RuneCountInString(c.got) != c.most || !strings.HasSuffix(c.got, c.tail) {
			t.Errorf("%d characters, valid %v: %q", utf8.RuneCountInString(c.got), utf8.ValidString(c.got), c.got)
		}
	}
}

// R.C8: the edges the look found, each a state a platform can be in. A part
// exactly as old as the setting starts nothing, as its text says "more
// than"; so does a backup, and a platform with none, exactly the setting old.
// A check that passed ends the update's alert only where it started after the
// one that failed. A rollout that gave up while Ready is true raises nothing.
// One address in two spellings, as an admin's and a further one, gets one
// mail. A volume with a size and no free space raises nothing.
func TestRC8TheEdgesOfTheRules(t *testing.T) {
	gb := int64(1e9)
	t.Run("R.C8 a part exactly the setting old starts nothing, a second later it starts", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-10*time.Minute))})
		e.check()
		if n := len(e.of(kindPart)); n != 0 {
			t.Fatalf("%d alerts at exactly the setting", n)
		}
		e.later(time.Second)
		e.check()
		if a := e.only(kindPart); a.Text != "katalog-api has not been ready for more than 10 minutes. Open operator to see why." {
			t.Fatalf("%q", a.Text)
		}
	})
	t.Run("R.C8 a backup, and a platform with none, exactly the setting old", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.set("backup", map[string]any{"lastSuccess": alertNow.Add(-48 * time.Hour).Format(time.RFC3339)})
		e.check()
		if n := len(e.of(kindBackup)); n != 0 {
			t.Fatalf("%d alerts for a backup exactly 2 days old", n)
		}
		e.later(time.Second)
		e.check()
		if a := e.only(kindBackup); a.Title != "No backup for 2 days" {
			t.Fatalf("%q", a.Title)
		}
		// None made, the platform installed exactly 2 days ago.
		e2 := newAlertEnv(t, mailtest.Options{})
		e2.status["conditions"] = []any{cond("Backup", "True", "Scheduled", "")}
		e2.set("backup", map[string]any{})
		e2.check()
		if n := len(e2.of(kindBackup)); n != 0 {
			t.Fatalf("%d alerts for a platform exactly 2 days old", n)
		}
		e2.later(time.Second)
		e2.check()
		if a := e2.only(kindBackup); a.Title != "No backup for 2 days" {
			t.Fatalf("%q", a.Title)
		}
	})
	t.Run("R.C8 a check that passed ends the alert only where it started after the one that failed", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		failedAt := alertNow.Add(-time.Hour)
		e.set("verification", verification("Failed", "update", "v0.6.0", failedAt, "tls"))
		e.check()
		a := e.only(kindUpdate)
		e.set("verification", verification("Passed", "request", "v0.6.0", failedAt))
		e.check()
		if b := e.only(kindUpdate); b.ID != a.ID || b.EndedAt != nil {
			t.Fatalf("a passed check that started with the failed one: %+v", b)
		}
		e.set("verification", verification("Passed", "request", "v0.6.0", failedAt.Add(time.Second)))
		e.check()
		if b := e.only(kindUpdate); b.EndText != "The check has passed." {
			t.Fatalf("a later passed check: %q", b.EndText)
		}
	})
	t.Run("R.C8 RolloutStalled while Ready is true raises nothing", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.status["conditions"] = []any{cond("Ready", "True", "RolloutStalled", "rollout stalled: chino-api (ProgressDeadlineExceeded)"), cond("Backup", "True", "Succeeded", "")}
		e.set("components", []any{stalledPart("chino-api", true, time.Time{})})
		e.check()
		if n := len(e.of(kindUpdate)); n != 0 {
			t.Fatalf("%d update alerts while Ready is true", n)
		}
	})
	t.Run("R.C8 one address in two spellings, an admin's and a further one, gets one mail", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		e.api.cfg.Mail.AlertsTo = []string{"ops@example.org", "OWNER@Example.org"}
		e.spec["mail"].(map[string]any)["alertsTo"] = []string{"ops@example.org", "OWNER@Example.org"}
		e.set("components", []any{part("katalog-api", false, alertNow.Add(-time.Hour))})
		e.check()
		to, _ := e.mailed()
		for i := range to {
			to[i] = strings.ToLower(to[i])
		}
		slices.Sort(to)
		if !slices.Equal(to, []string{"ops@example.org", "owner@example.org"}) {
			t.Fatalf("mailed to %v", to)
		}
	})
	t.Run("R.C8 a volume with a size and no free space raises nothing", func(t *testing.T) {
		e := newAlertEnv(t, mailtest.Options{})
		v := volume("backups", "node-2:7", 500*gb, 0)
		delete(v, "freeBytes")
		e.set("storage", map[string]any{"volumes": []any{v}})
		e.check()
		if n := len(e.of(kindVolume)); n != 0 {
			t.Fatalf("%d alerts", n)
		}
	})
}
