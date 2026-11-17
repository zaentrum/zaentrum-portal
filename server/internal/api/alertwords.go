package api

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zaentrum/zaentrum-portal/server/internal/mail"
)

// ─── the alerts' words ───────────────────────────────────────────────────────
//
// Every title, text and sentence of an end is the chapter's, as its section
// Words, The alerts, has it. A title has at most 80 characters and a text at
// most 280: a value that would take one past is cut, and ends with "…". A
// count of one is written in the singular. A date is written as a mail writes
// it, in UTC; a size as the storage page writes it.

const (
	maxAlertTitle = 80
	maxAlertText  = 280
)

// cutMark ends a value that was cut; noBreak stands between a size and its
// unit, as the catalog console writes sizes.
var (
	cutMark = string(rune(0x2026))
	noBreak = string(rune(0xa0))
)

// counted is n with its noun, in the singular for one: "1 day", "14 days".
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// cutTo is s cut to at most n characters, ending with "…" where it was cut.
func cutTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return cutMark
	}
	return string(r[:n-1]) + cutMark
}

// fitted is before, value and after, the value cut so that the whole has at
// most limit characters.
func fitted(limit int, before, value, after string) string {
	room := limit - utf8.RuneCountInString(before) - utf8.RuneCountInString(after)
	return before + cutTo(value, room) + after
}

// joined joins names with commas and “and”: “library, databases and
// backups”.
func joined(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// when is a date in a title or a text: as a mail writes it, in UTC.
func when(t time.Time) string { return mail.When(t) }

// sizeText is a size as the storage page writes it, as the catalog console
// does: in decimal units, in MB below 1 GB (never 0 MB for something there
// is), then in GB with one decimal and no trailing zero, and in TB from 1,000
// GB; the unit after a space that does not break.
func sizeText(n int64) string {
	if n < 0 {
		n = 0
	}
	if mb := math.Round(float64(n) / 1e6); mb < 1000 {
		v := int64(mb)
		if n > 0 && v < 1 {
			v = 1
		}
		return strconv.FormatInt(v, 10) + noBreak + "MB"
	}
	if math.Round(float64(n)/1e9*10) < 10000 {
		return tenths(float64(n)/1e9, "GB")
	}
	return tenths(float64(n)/1e12, "TB")
}

// tenths is v with one decimal, none where it is a whole number.
func tenths(v float64, unit string) string {
	t := int64(math.Round(v * 10))
	if t%10 == 0 {
		return strconv.FormatInt(t/10, 10) + noBreak + unit
	}
	return strconv.FormatInt(t/10, 10) + "." + strconv.FormatInt(t%10, 10) + noBreak + unit
}

// ─── A part is not ready ─────────────────────────────────────────────────────

func partTitle(part string) string {
	return fitted(maxAlertTitle, "A part is not ready: ", part, "")
}

// partText names the setting, in minutes.
func partText(part string, minutes int) string {
	return fitted(maxAlertText, "", part, " has not been ready for more than "+counted(minutes, "minute", "minutes")+". Open operator to see why.")
}

func partReadyAgain(part string) string { return fitted(maxAlertText, "", part, " is ready again.") }

func partStopped(part string) string {
	return fitted(maxAlertText, "", part, " was stopped, so it is not watched now.")
}

func partGone(part string) string {
	return fitted(maxAlertText, "", part, " is no longer a part of the platform.")
}

// ─── A volume is almost full ─────────────────────────────────────────────────

// The volumes' names in an alert, by the name status.storage gives them, in
// the order an alert names them.
var (
	volumeOrder = []string{"media", "postgres-data", "backups", "kafka"}
	volumeWords = map[string]string{"media": "library", "postgres-data": "databases", "backups": "backups", "kafka": "event broker"}
)

// volumeWord is a volume's name in an alert; a name the chapter does not know
// is written as the status has it.
func volumeWord(name string) string {
	if w, ok := volumeWords[name]; ok {
		return w
	}
	return name
}

func volumeTitle(words []string) string {
	return fitted(maxAlertTitle, "A volume is almost full: ", joined(words), "")
}

// volumeText is the text of one volume, or of the disk several share.
func volumeText(words []string, percent int, free, size int64) string {
	tail := " is " + strconv.Itoa(percent) + " % full: " + sizeText(free) + " of " + sizeText(size) +
		" are free. Open storage to see what takes the room."
	if len(words) > 1 {
		return fitted(maxAlertText, "The disk of ", joined(words), tail)
	}
	return fitted(maxAlertText, "The ", joined(words), " volume"+tail)
}

func volumeBelow(words []string, percent int) string {
	if len(words) > 1 {
		return fitted(maxAlertText, "The disk of ", joined(words), " is at "+strconv.Itoa(percent)+" %.")
	}
	return fitted(maxAlertText, "The ", joined(words), " volume is at "+strconv.Itoa(percent)+" %.")
}

// volumeUnmeasured is the end of a disk's alert where it is no longer
// measured: of one volume, or of the disk several share.
func volumeUnmeasured(words []string) string {
	if len(words) > 1 {
		return fitted(maxAlertText, "The disk of ", joined(words), " is not measured any more.")
	}
	return fitted(maxAlertText, "The ", joined(words), " volume is not measured any more.")
}

// ─── The certificate ends soon ───────────────────────────────────────────────

func certificateTitle(days int, ended bool) string {
	if ended {
		return "The certificate has ended"
	}
	return "The certificate ends in " + counted(days, "day", "days")
}

func certificateText(notAfter time.Time, ended bool) string {
	if ended {
		return "The platform's certificate ended on " + when(notAfter) + ". Browsers warn and the apps do not connect. Renew it."
	}
	return "The platform's certificate ends on " + when(notAfter) + ". After that browsers warn and the apps stop connecting. Renew it."
}

func certificateRenewed(notAfter time.Time) string {
	return "The certificate now ends on " + when(notAfter) + "."
}

const certificateGone = "The platform serves no certificate of its own now."

// ─── No good backup ──────────────────────────────────────────────────────────

const backupFailedTitle = "The last backup failed"

func backupOldTitle(days int) string { return "No backup for " + counted(days, "day", "days") }

// backupFailedText is the text of a backup that failed, with the newest good
// one where there is one.
func backupFailedText(failed time.Time, newest *time.Time) string {
	if newest == nil {
		return "The backup of " + when(failed) + " failed. There is no good backup yet. Open backups to see why."
	}
	return "The backup of " + when(failed) + " failed. The newest good one is from " + when(*newest) + ". Open backups to see why."
}

func backupOldText(newest time.Time) string {
	return "The newest backup is from " + when(newest) + ". Open backups to see why none was made since."
}

func backupNoneText(installed time.Time) string {
	return "No backup has been made since the platform was installed on " + when(installed) + ". Open backups to see why."
}

func backupMade(newest time.Time) string { return "A backup was made on " + when(newest) + "." }

const (
	backupsWithin  = "The backups are within the setting again."
	backupsOff     = "Backups are off, so they are not watched now."
	backupsOutside = "The databases run outside the platform now, so their backups are not watched here."
)

// ─── An update failed ────────────────────────────────────────────────────────

// updateTitle is the title of a rollout that has given up: of an update to
// version, or, where version is the one the last check ran against, of a
// rollout of it.
func updateTitle(version string, rollout bool) string {
	if rollout {
		return fitted(maxAlertTitle, "The rollout of ", version, " did not finish")
	}
	return fitted(maxAlertTitle, "The update to ", version, " did not finish")
}

func checkTitle(version string) string {
	return fitted(maxAlertTitle, "The check after the update to ", version, " failed")
}

// partsText names the parts whose rollout has given up: none, where the
// status names none; one; all of them where they fit, else as many as fit and
// how many more. named is how many it names.
func partsText(parts []string) (text string, named int) {
	const head, tail = "These parts could not start: ", ". What ran before keeps running where it can. Open operator to see why."
	switch len(parts) {
	case 0:
		return "The rollout did not finish. What ran before keeps running where it can. Open operator to see why.", 0
	case 1:
		return fitted(maxAlertText, "This part could not start: ", parts[0], tail), 1
	}
	if all := head + joined(parts) + tail; utf8.RuneCountInString(all) <= maxAlertText {
		return all, len(parts)
	}
	for k := len(parts) - 1; k >= 1; k-- {
		s := head + strings.Join(parts[:k], ", ") + " and " + strconv.Itoa(len(parts)-k) + " more" + tail
		if utf8.RuneCountInString(s) <= maxAlertText {
			return s, k
		}
	}
	more := ""
	if len(parts) > 1 {
		more = " and " + strconv.Itoa(len(parts)-1) + " more"
	}
	return fitted(maxAlertText, head, parts[0], more+tail), 1
}

// checkText is the text of a check that failed: how many of how many checks,
// and which.
func checkText(failed, total int, checks []string) string {
	return fitted(maxAlertText, strconv.Itoa(failed)+" of "+counted(total, "check", "checks")+" failed: ", joined(checks),
		". Open operator to see the checks.")
}

const checkNoResult = "The check could not reach a result. Open operator to see why."

func updateFinished(version string, rollout bool) string {
	if rollout {
		return fitted(maxAlertText, "The rollout of ", version, " has finished.")
	}
	return fitted(maxAlertText, "The update to ", version, " has finished.")
}

const checkPassed = "The check has passed."
