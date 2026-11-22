package mail

import "time"

// The kinds of an alert's mails, as the log names them.
const (
	// KindAlert: an alert started, and is mailed to whom it goes to.
	KindAlert = "alert"
	// KindAlertEnd: an alert ended, and is mailed to whom its start reached.
	KindAlertEnd = "alert's end"
)

// alertWhy is the last line of an alert's mails: why the reader gets it.
func alertWhy(host string) string {
	return "You get this mail because you are an admin of " + host + ", or your address is in its alert settings."
}

// AlertStart is what the mail of an alert that started says.
type AlertStart struct {
	// To is the address; Host the platform's name, and Link the page that
	// explains the alert, whole.
	To, Host, Link string
	// Title and Text are the alert's as they were when it started.
	Title, Text string
}

// AlertMail is the mail of an alert that started: its title, its text, where
// to see it, and why the reader gets it.
func AlertMail(a AlertStart) Message {
	return Message{
		Kind:    KindAlert,
		To:      a.To,
		Subject: a.Title,
		Text:    paragraphs(a.Text, "See it: "+a.Link, alertWhy(a.Host)),
	}
}

// AlertEnd is what the mail of an alert that ended says.
type AlertEnd struct {
	To, Host string
	// Title is the alert's last; Sentence the sentence of its end.
	Title, Sentence string
	// Started and Ended are when it started and when it ended.
	Started, Ended time.Time
}

// AlertEndMail is the mail of an alert that ended: the sentence of its end,
// when it started and ended, and why the reader gets it.
func AlertEndMail(e AlertEnd) Message {
	return Message{
		Kind:    KindAlertEnd,
		To:      e.To,
		Subject: "Ended: " + e.Title,
		Text: paragraphs(
			e.Sentence,
			"It started on "+When(e.Started)+" and ended on "+When(e.Ended)+".",
			alertWhy(e.Host),
		),
	}
}
