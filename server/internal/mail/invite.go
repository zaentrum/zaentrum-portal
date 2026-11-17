package mail

import (
	"strings"
	"time"
)

// The kinds of an invite's mail, as the log names them.
const (
	// KindInvite: an admin added a person, and mails them their link.
	KindInvite = "invite"
	// KindNewLink: an admin made a person a new link, and mails it.
	KindNewLink = "new invite link"
)

// Invite is what an invite's mail says.
type Invite struct {
	// To is the person's address; Name what the platform calls them, and
	// Username what they sign in with.
	To, Name, Username string
	// Admin is the name of the admin who made the link.
	Admin string
	// Host is the platform's name, and Link the invite link, whole.
	Host, Link string
	// Expires is when the link stops working.
	Expires time.Time
}

// InviteMail is the mail of a person an admin added: their account, and the
// link to choose their password with.
func InviteMail(i Invite) Message {
	return Message{
		Kind:    KindInvite,
		To:      i.To,
		Subject: "Your account on " + i.Host,
		Text: paragraphs(
			"Hello "+i.Name+",",
			i.Admin+" made an account for you on "+i.Host+". Your username is "+i.Username+".",
			"Open this link to choose your password:",
			i.Link,
			"The link works once, until "+When(i.Expires)+". After that, ask "+i.Admin+" for a new one.",
			"If you did not expect this mail, ignore it.",
		),
	}
}

// NewLinkMail is the mail of a new invite link, for a person who has an
// account: the link to choose a new password with.
func NewLinkMail(i Invite) Message {
	return Message{
		Kind:    KindNewLink,
		To:      i.To,
		Subject: "A new link for your account on " + i.Host,
		Text: paragraphs(
			"Hello "+i.Name+",",
			i.Admin+" made a new link for your account "+i.Username+" on "+i.Host+".",
			"Open it to choose a new password:",
			i.Link,
			"The link works once, until "+When(i.Expires)+". Your password stays as it is until you use the link.",
			"If you did not expect this mail, ignore it.",
		),
	}
}

// When is a time as a mail writes it: in UTC, and saying so, “15 November
// 2026, 14:02 UTC”.
func When(t time.Time) string {
	return t.UTC().Format("2 January 2006, 15:04") + " UTC"
}

// paragraphs are a mail's text: each part a paragraph of its own.
func paragraphs(parts ...string) string {
	return strings.Join(parts, "\n\n") + "\n"
}
