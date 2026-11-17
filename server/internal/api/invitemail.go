package api

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/zaentrum/zaentrum-portal/server/internal/mail"
	"github.com/zaentrum/zaentrum-portal/server/internal/people"
)

// ─── the invite by mail ──────────────────────────────────────────────────────
//
// An admin who adds a person (POST /people with sendInvite) or makes them a
// new link (POST /people/{id}/invite with send) has the link mailed to the
// person's address, while they wait. The answer carries the link as before,
// and mail, what became of the mail:
//
//	sent         the mail server took it
//	not-set-up   mail is not set up, so nothing was sent
//	no-address   the person has no address, so nothing was sent; the address
//	             the realm's import gave the first admin counts as none
//	failed       it was not sent, and mailError says why: the case's code,
//	             the values its sentence names and the sentence of the Words,
//	             as a refusal of the mail routes says it (mail.go) — the
//	             cluster's refusal among them, with its words in detail,
//	             where the platform's resource cannot be read
//
// The link a mail carries and the platform's name in it come from the public
// address alone, never from the request: without a full one mail is not
// ready, or not set up where the operator does not run the platform
// (mailState). The answer's link is the one the admin reached the portal on,
// as before.
//
// Without sendInvite or send, no mail is sent and the answer is as before,
// without mail. With an external identity provider the platform keeps no
// address and makes no invite (people.go): both routes are refused before any
// of this.

// What became of an invite's mail.
const (
	inviteMailSent      = "sent"
	inviteMailNotSetUp  = "not-set-up"
	inviteMailNoAddress = "no-address"
	inviteMailFailed    = "failed"
)

// mailedInvite is what an answer says of an invite's mail.
type mailedInvite struct {
	Mail      string   `json:"mail,omitempty"`
	MailError *refusal `json:"mailError,omitempty"`
}

// failedWith is an invite whose mail failed, and why.
func failedWith(why refusal) mailedInvite {
	return mailedInvite{Mail: inviteMailFailed, MailError: &why}
}

// mailInvite mails person p their invite link — the mail of a person just
// added, or with again the mail of a new link — and answers what became of
// it. Nothing is sent while mail is not set up, nor before it is ready: a
// mail that would go with settings the operator has not applied yet fails,
// with where mail stands as the reason. Nor is anything sent where the
// platform's resource cannot be read: the cluster's refusal is the reason, as
// the test mail answers it.
func (a *API) mailInvite(r *http.Request, p people.Person, link inviteLink, again bool) mailedInvite {
	ctx := r.Context()
	st, err := a.mailState(ctx)
	if err != nil {
		log.Printf("mail: an invite's mail was not sent: the platform's resource could not be read: %v", err)
		_, why := unreadable(err)
		return failedWith(why)
	}
	to := strings.TrimSpace(p.Email)
	switch {
	case !st.setUp:
		return mailedInvite{Mail: inviteMailNotSetUp}
	case to == "" || placeholder(to):
		return mailedInvite{Mail: inviteMailNoAddress}
	case !st.ready:
		return failedWith(st.notReady())
	}
	address, ok := a.mailLink(link)
	if !ok {
		return failedWith(refuseNotReady)
	}
	words := mail.Invite{
		To: to, Name: p.DisplayName, Username: p.Username, Admin: a.adminName(r),
		Host: a.mailPlatformHost(), Link: address, Expires: link.ExpiresAt,
	}
	m := mail.InviteMail(words)
	if again {
		m = mail.NewLinkMail(words)
	}
	if err := mail.New(a.cfg.Mail, a.mailLocalName()).Send(ctx, m); err != nil {
		var f *mail.Failure
		if !errors.As(err, &f) {
			// The settings portal-api runs with name no server or no
			// sender, whatever the resource says.
			log.Printf("mail: %s was refused: mail is not set up here", withArticle(m.Kind))
			return mailedInvite{Mail: inviteMailNotSetUp}
		}
		log.Printf("mail: %s was refused (%s)", withArticle(m.Kind), f.Reason)
		return failedWith(failedSend(f))
	}
	log.Printf("mail: %s was sent", withArticle(m.Kind))
	return mailedInvite{Mail: inviteMailSent}
}

// adminName is the name of the admin who asks, as the people page shows it:
// their account's, else the username their token carries.
func (a *API) adminName(r *http.Request) string {
	if a.people != nil {
		if p, err := a.people.Get(r.Context(), caller(r)); err == nil {
			return p.DisplayName
		}
	}
	return callerName(r)
}

// mailLink is the invite link as a mail carries it: on the platform's public
// address, PORTAL_PUBLIC_URL, alone, never on the address the request came
// to; false without a full one, where mail is not ready (mailState).
func (a *API) mailLink(link inviteLink) (string, bool) {
	if _, ok := a.publicAddress(); !ok {
		return "", false
	}
	return strings.TrimRight(strings.TrimSpace(a.cfg.PublicURL), "/") + link.Path, true
}

// withArticle is a kind of mail as a log line names it: "an invite", "a new
// invite link".
func withArticle(kind string) string {
	if kind != "" && strings.ContainsRune("aeiou", rune(kind[0])) {
		return "an " + kind
	}
	return "a " + kind
}
