package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/zaentrum/zaentrum-portal/server/internal/mail"
	"github.com/zaentrum/zaentrum-portal/server/internal/model"
)

// ─── an alert's mails ────────────────────────────────────────────────────────
//
// An alert is mailed when it starts and when it ends (section By mail). The
// start mail goes to every admin whose account is enabled and has an address,
// and to every further address that is no placeholder: the addresses the mail
// page lists under alerts go to, each once. The end mail goes to the
// addresses the start mail reached. Each mail is plain text to one address.
//
// For the same alert about the same subject one start mail is sent in 24
// hours, counted from when the mail server first took the mail of the last
// start for an address: an alert that starts again sooner is not mailed, and
// neither is its end. A mail the server does not take is tried again after 5
// minutes, three times in all for each address, by whichever portal-api runs
// then; so is one held back while mail is set up and not ready, and one whose
// mail settings could not be read, each a try with its reason. Nothing is sent
// while mail is not set up, while spec.alerts.mail is false, or when nobody
// has an address: an alert that starts then is never mailed, not later
// either, and neither is its end; a mail that meets one of them at a try
// again goes no further. An address the start mail has not reached when the
// alert ends gets neither mail: no try of the start mail is made after the
// end.
//
// An address gets a start mail once and an end mail once, however many
// portal-apis run: one holds an alert's mail at a time (store/alerts.go), and
// what became of it is kept after each address, so a portal-api that stops
// while it sends repeats the mail to one address at most, and a try it left
// is made again by another. The addresses are kept until the end mail is
// done, and their number after that.

const (
	// alertMailTries are the tries for each address; alertMailAgain how long
	// after one the next is made.
	alertMailTries = 3
	alertMailAgain = 5 * time.Minute
	// alertMailHold is how long a portal-api holds an alert's mail at a time,
	// by the database's clock: each address's record keeps it.
	alertMailHold = 2 * time.Minute
	// alertMailOnce is the time in which one start mail is sent for an alert
	// about one subject.
	alertMailOnce = 24 * time.Hour
)

// The states of an alert's mail, one for each sentence of the column mail.
const (
	mailStateSent      = "sent"
	mailStateSending   = "sending"
	mailStateNotSetUp  = "not-set-up"
	mailStateOff       = "off"
	mailStateNoAddress = "no-address"
	// mailStateAdminsUnread: the admins could not be read in three tries,
	// and there is no further address.
	mailStateAdminsUnread = "admins-unread"
	mailStateSentRecently = "sent-recently"
	mailStateFailed       = "failed"
	// mailStateEndedFirst: the alert ended before its start mail got a
	// single try, and neither mail goes.
	mailStateEndedFirst = "ended-before-mail"
)

// alertMail is what became of an alert's mails, as the alerts table keeps it.
type alertMail struct {
	Start mailRound `json:"start"`
	// End is the end mail: nil until the alert has ended, and where its start
	// mail reached nobody.
	End *mailRound `json:"end,omitempty"`
}

// mailRound is one mail of an alert, to each of its addresses.
type mailRound struct {
	// Begun: its addresses were read, or why nothing is sent was found.
	Begun bool `json:"begun,omitempty"`
	// Held is why nothing is sent — not-set-up, off, no-address or
	// sent-recently — found as the mail began, and "" where it goes out.
	Held string `json:"held,omitempty"`
	// Stopped is why the mail went no further — not-set-up or off — found at
	// a try again: an address it had not reached gets it not later either.
	Stopped string `json:"stopped,omitempty"`
	// Subject and Text are the start mail's: the alert's title and text as
	// they were when it started.
	Subject string `json:"subject,omitempty"`
	Text    string `json:"text,omitempty"`
	// Admins: the admins' addresses could not be read yet, and are tried
	// again; AdminTries counts the tries that failed.
	Admins     bool     `json:"admins,omitempty"`
	AdminTries int      `json:"adminTries,omitempty"`
	To         []mailTo `json:"to,omitempty"`
	// Tries and Error are the mail's own before it began: each time its mail
	// settings could not be read counts as a try, the cluster's refusal its
	// reason.
	Tries int      `json:"tries,omitempty"`
	Error *refusal `json:"error,omitempty"`
	// Dropped: the alert ended before this mail reached every address, and
	// no try of it is made after the end.
	Dropped bool `json:"dropped,omitempty"`
	// Done: the alert's end mail is done, and the addresses are gone: what
	// is kept is how many it was for, how many took it, and one reason with
	// no address in it.
	Done      bool     `json:"done,omitempty"`
	Addresses int      `json:"addresses,omitempty"`
	Taken     int      `json:"taken,omitempty"`
	Reason    *refusal `json:"reason,omitempty"`
}

// mailTo is one address of a mail, and what became of it.
type mailTo struct {
	Address string `json:"address"`
	Sent    bool   `json:"sent,omitempty"`
	Tries   int    `json:"tries,omitempty"`
	// Error is why the last try did not get it taken.
	Error *refusal `json:"error,omitempty"`
}

// waiting: a try of the mail is still to come.
func (r mailRound) waiting() bool {
	switch {
	case r.Done || r.Dropped || r.Held != "" || r.Stopped != "":
		return false
	case !r.Begun:
		return r.Tries < alertMailTries
	case r.Admins:
		return true
	}
	return slices.ContainsFunc(r.To, func(t mailTo) bool { return !t.Sent && t.Tries < alertMailTries })
}

// taken are the addresses the mail server took the mail for.
func (r mailRound) taken() []string {
	var out []string
	for _, t := range r.To {
		if t.Sent {
			out = append(out, t.Address)
		}
	}
	return out
}

// reason is why the mail did not reach every address: the last reason of the
// first address that did not take it, else the mail's own.
func (r mailRound) reason() *refusal {
	for _, t := range r.To {
		if !t.Sent && t.Error != nil {
			return t.Error
		}
	}
	if r.Reason != nil {
		return r.Reason
	}
	return r.Error
}

// alertMailView is an alert's mail as the routes answer it: its state, the
// number of addresses it is for, how many took it where some did and others
// did not or wait, and with failed the reason.
type alertMailView struct {
	State     string   `json:"state"`
	Addresses int      `json:"addresses"`
	Taken     *int     `json:"taken,omitempty"`
	Error     *refusal `json:"error,omitempty"`
}

// mailView is what the column mail says of an alert: of its start mail while
// it is active, and of its end mail once it has ended — of its start mail,
// where no end mail was due. Once the server took a mail for an address it is
// sent, to so many of so many, whatever changes afterwards, and a mail that
// failed has its reason.
func mailView(raw json.RawMessage, ended bool) alertMailView {
	var m alertMail
	_ = json.Unmarshal(raw, &m)
	r := m.Start
	if ended && m.End != nil {
		r = *m.End
	}
	if r.Held != "" {
		return alertMailView{State: r.Held}
	}
	total, taken := len(r.To), len(r.taken())
	if r.Done {
		total, taken = r.Addresses, r.Taken
	}
	some := func(v alertMailView) alertMailView {
		if taken > 0 && taken < total {
			n := taken
			v.Taken = &n
		}
		return v
	}
	switch {
	case taken == 0 && r.Stopped != "":
		return alertMailView{State: r.Stopped}
	case r.Dropped && taken == 0 && r.reason() == nil:
		return alertMailView{State: mailStateEndedFirst}
	case r.waiting():
		return some(alertMailView{State: mailStateSending, Addresses: total})
	case !r.Begun:
		return alertMailView{State: mailStateFailed, Error: r.Error}
	case total == 0:
		return alertMailView{State: mailStateNoAddress}
	case taken == total:
		return alertMailView{State: mailStateSent, Addresses: total}
	}
	// Where the mail went no further before it was tried for the others,
	// why is the reason it went no further.
	why := r.reason()
	switch {
	case why != nil:
	case r.Stopped == mailStateNotSetUp:
		why = &refuseNotSetUp
	case r.Stopped == mailStateOff:
		why = &refuseAlertMailOff
	}
	return some(alertMailView{State: mailStateFailed, Addresses: total, Error: why})
}

// refuseAlertMailOff is why a mail went no further to the addresses it had
// not tried for once alert mail was switched off: a sentence the alerts page
// words its own way.
var refuseAlertMailOff = refusal{Error: "alerts_mail_off", Message: "Alert mail is off."}

// refuseAnAddress is the refusal of an address once the address is no longer
// kept, as Why a mail failed words it: “The mail server refused the address
// {address}.” without its value.
var refuseAnAddress = refusal{Error: "mail_recipient_refused", Message: "The mail server refused an address."}

// done is the mail with its addresses gone, the alert's end mail being done:
// how many each mail was for, how many took it, and its reason, which says
// “The mail server refused an address.” where it named one.
func (m *alertMail) done() {
	var addresses []string
	for _, r := range []*mailRound{&m.Start, m.End} {
		if r != nil {
			for _, t := range r.To {
				addresses = append(addresses, strings.ToLower(t.Address))
			}
		}
	}
	// names: the reason holds one of the addresses, in its values or its
	// words.
	names := func(f *refusal) bool {
		words := []string{strings.ToLower(f.Message), strings.ToLower(f.Detail)}
		for _, v := range f.Values {
			if s, ok := v.(string); ok {
				words = append(words, strings.ToLower(s))
			}
		}
		return slices.ContainsFunc(addresses, func(address string) bool {
			return slices.ContainsFunc(words, func(w string) bool { return strings.Contains(w, address) })
		})
	}
	forget := func(r *mailRound) {
		if r == nil || r.Done {
			return
		}
		r.Addresses, r.Taken = len(r.To), len(r.taken())
		why := r.reason()
		if why != nil && names(why) {
			an := refuseAnAddress
			why = &an
		}
		r.Reason, r.To, r.Done = why, nil, true
	}
	forget(&m.Start)
	forget(m.End)
}

// ─── carrying the mails on ───────────────────────────────────────────────────

// alertRun is one check of the alerts, as their mails see it: where mail
// stands, read once, and whether the check said in the log that it could not
// be read.
type alertRun struct {
	read  bool
	state mailState
	err   error
	said  bool
}

// mailState is where mail stands at this check; an error is the cluster's,
// said in the log once a check.
func (a *API) runMailState(ctx context.Context, run *alertRun) (mailState, error) {
	if !run.read {
		run.state, run.err = a.mailState(ctx)
		run.read = true
	}
	if run.err != nil && !run.said {
		run.said = true
		log.Printf("alerts: the platform's resource could not be read: %s", spaced(run.err.Error()))
	}
	return run.state, run.err
}

// mailing is this portal-api carrying an alert's mails on while it holds
// them: it keeps what became of them after each address, and stops where it
// no longer holds them, where portal-api stops, or where the check's time is
// nearly up.
type mailing struct {
	a   *API
	ctx context.Context
	al  model.Alert
	m   alertMail
	// st is where mail stands, and stErr why its settings could not be read.
	st    mailState
	stErr error
	// lost: this portal-api no longer holds the mail, or what became of it
	// could not be written; stopped: it stopped before every try was made,
	// which whichever portal-api runs next makes at once; endedMeanwhile:
	// another portal-api ended the alert while this one carried its mail on.
	lost, stopped, endedMeanwhile bool
}

// keep writes what became of the mail, and keeps holding it; false where it
// could not, and then nothing more is tried. startMailed: the server took the
// start mail for an address. Each write says whether the alert has ended
// meanwhile.
func (g *mailing) keep(startMailed bool) bool {
	if g.lost {
		return false
	}
	raw, err := json.Marshal(g.m)
	if err == nil {
		ctx, cancel := dbContext(g.ctx)
		var held, ended bool
		held, ended, err = g.a.alerts.KeepMail(ctx, g.al.ID, g.a.holder(), raw, startMailed, alertMailHold)
		cancel()
		if err == nil && !held {
			g.lost = true
			return false
		}
		if ended && g.al.EndedAt == nil {
			g.endedMeanwhile = true
		}
	}
	if err != nil {
		log.Printf("alerts: what became of an alert's mail could not be kept: %s", spaced(err.Error()))
		g.lost = true
		return false
	}
	return true
}

// release writes what became of the mail and lets it go, its next try due
// after next, none for nil.
func (g *mailing) release(next *time.Duration) {
	if g.lost {
		return
	}
	raw, err := json.Marshal(g.m)
	if err == nil {
		ctx, cancel := dbContext(g.ctx)
		_, err = g.a.alerts.ReleaseMail(ctx, g.al.ID, g.a.holder(), raw, next, g.al.EndedAt != nil)
		cancel()
	}
	if err != nil {
		log.Printf("alerts: what became of an alert's mail could not be kept: %s", spaced(err.Error()))
	}
}

// mailAlert carries an alert's mails on, while this portal-api holds them: it
// begins the start mail, makes the tries that are due, begins the end mail
// once the alert has ended — to the addresses the start mail reached, an
// address it had not reached getting neither — and keeps what became of them
// after each address.
func (a *API) mailAlert(ctx context.Context, run *alertRun, al model.Alert) {
	g := &mailing{a: a, ctx: ctx, al: al}
	_ = json.Unmarshal(al.Mail, &g.m)
	g.st, g.stErr = a.runMailState(ctx, run)
	m := &g.m
	ended := al.EndedAt != nil
	host := a.mailPlatformHost()
	switch start := &m.Start; {
	case !start.waiting():
	case ended:
		// An address the start mail has not reached when the alert ends gets
		// neither mail.
		start.Dropped = true
	case !start.Begun && g.stErr != nil:
		g.unreadableTry(start, mail.KindAlert)
	default:
		first := !start.Begun
		if first {
			a.beginStart(ctx, al, start, g.st)
			if !g.keep(false) {
				return
			}
		}
		if start.waiting() {
			g.try(start, !first, true, func(to string) mail.Message {
				return mail.AlertMail(mail.AlertStart{To: to, Host: host, Link: a.alertAddress(al.Link), Title: start.Subject, Text: start.Text})
			})
		}
	}
	if g.lost {
		return
	}
	if ended && m.End == nil && !m.Start.waiting() {
		if to := m.Start.taken(); len(to) > 0 {
			end := &mailRound{Begun: true}
			for _, address := range to {
				end.To = append(end.To, mailTo{Address: address})
			}
			m.End = end
		}
	}
	if end := m.End; end != nil && end.waiting() && !g.stopped {
		endedAt := *al.EndedAt
		g.try(end, false, false, func(to string) mail.Message {
			return mail.AlertEndMail(mail.AlertEnd{To: to, Host: host, Title: al.Title, Sentence: al.EndText, Started: al.StartedAt, Ended: endedAt})
		})
	}
	if g.lost {
		return
	}
	g.release(g.next())
}

// next is when the mail's next try is due: at once where this portal-api
// stopped before it made every try, after 5 minutes where a try is still to
// come, and none where every mail is done — the alert's end mail among them,
// and then its addresses go.
func (g *mailing) next() *time.Duration {
	m := &g.m
	switch {
	case g.stopped:
		now := time.Duration(0)
		return &now
	case m.Start.waiting() || m.End != nil && m.End.waiting():
		again := alertMailAgain
		return &again
	case g.al.EndedAt != nil:
		m.done()
	}
	return nil
}

// unreadableTry counts the mail settings that could not be read as a try of
// a mail that has not begun, with the cluster's refusal as its reason.
func (g *mailing) unreadableTry(r *mailRound, kind string) {
	_, why := unreadable(g.stErr)
	r.Tries++
	r.Error = &why
	logTry(kind, r.Tries, why.Error)
}

// logTry says in the log that a try of a mail of kind was refused, and
// whether it is tried again.
func logTry(kind string, tries int, reason string) {
	if tries >= alertMailTries {
		log.Printf("mail: %s was given up after %d tries (%s)", withArticle(kind), alertMailTries, reason)
		return
	}
	log.Printf("mail: %s was refused (%s); it is tried again in 5 minutes", withArticle(kind), reason)
}

// beginStart begins an alert's start mail: why nothing is sent, or the
// addresses it goes to.
func (a *API) beginStart(ctx context.Context, al model.Alert, r *mailRound, st mailState) {
	// The tries its settings could not be read count for each address.
	carried := r.Tries
	r.Begun, r.Subject, r.Text, r.Tries, r.Error = true, al.Title, al.Text, 0, nil
	switch {
	case !st.setUp:
		r.Held = mailStateNotSetUp
		return
	case !st.alertMail:
		r.Held = mailStateOff
		return
	}
	cctx, cancel := callContext(ctx)
	recent, err := a.alerts.StartMailedWithin(cctx, al.Kind, al.About, alertMailOnce)
	cancel()
	if err != nil {
		log.Printf("alerts: whether a start was mailed in the last 24 hours could not be read, so it is mailed: %s", spaced(err.Error()))
	}
	if recent {
		r.Held = mailStateSentRecently
		log.Printf("mail: %s was not sent: one was sent less than a day ago", withArticle(mail.KindAlert))
		return
	}
	further := st.saved.AlertsTo
	admins, err := a.mailAdmins(ctx)
	if err != nil {
		r.Admins, r.AdminTries = true, 1
		if len(further) > 0 {
			log.Printf("mail: the admins could not be read, so %s went to the further addresses alone: %s", withArticle(mail.KindAlert), spaced(err.Error()))
		}
	}
	r.To = alertAddresses(nil, admins, further)
	for i := range r.To {
		r.To[i].Tries = carried
	}
	if len(r.To) == 0 && !r.Admins {
		r.Held = mailStateNoAddress
	}
}

// alertAddresses are to with the admins' addresses and the further ones that
// are no placeholder added, each address once.
func alertAddresses(to []mailTo, admins []mailAdmin, further []string) []mailTo {
	add := func(address string) {
		address = strings.TrimSpace(address)
		if address == "" || placeholder(address) || slices.ContainsFunc(to, func(t mailTo) bool { return strings.EqualFold(t.Address, address) }) {
			return
		}
		to = append(to, mailTo{Address: address})
	}
	for _, ad := range admins {
		add(ad.Address)
	}
	for _, f := range further {
		add(f)
	}
	return to
}

// try makes the tries of a mail that are due, keeping what became of it after
// each address. With readAdmins it reads the admins' addresses again where
// they could not be read. Each address that has not taken it and has tries
// left gets one: nothing while mail is set up and not ready, or while its
// settings could not be read, which counts as a try with why as its reason.
// Where mail is not set up, or alert mail is off, the mail goes no further,
// not later either.
func (g *mailing) try(r *mailRound, readAdmins, start bool, message func(to string) mail.Message) {
	a, st := g.a, g.st
	switch {
	case g.stErr != nil:
	case !st.setUp:
		r.Stopped = mailStateNotSetUp
		return
	case !st.alertMail:
		r.Stopped = mailStateOff
		return
	}
	if r.Admins && readAdmins && g.stErr == nil {
		unread := false
		if admins, err := a.mailAdmins(g.ctx); err == nil {
			r.Admins = false
			r.To = alertAddresses(r.To, admins, nil)
		} else if r.AdminTries++; r.AdminTries >= alertMailTries {
			r.Admins, unread = false, true
		}
		if len(r.To) == 0 && !r.Admins {
			// Nobody to send it to: because the admins could not be read,
			// or because none of them has an address.
			r.Held = mailStateNoAddress
			if unread {
				r.Held = mailStateAdminsUnread
			}
		}
		if !g.keep(false) || r.Held != "" {
			return
		}
	}
	for i := range r.To {
		t := &r.To[i]
		if t.Sent || t.Tries >= alertMailTries {
			continue
		}
		if start && g.endedMeanwhile {
			// No try of the start mail is made after the end: the end mail
			// is due at once, to the addresses it reached.
			return
		}
		if alertStopping(g.ctx) {
			g.stopped = true
			return
		}
		t.Tries++
		m := message(t.Address)
		var err error
		switch {
		case g.stErr != nil:
			_, why := unreadable(g.stErr)
			t.Error = &why
			logTry(m.Kind, t.Tries, why.Error)
		case !st.ready:
			why := st.notReady()
			t.Error = &why
			logTry(m.Kind, t.Tries, why.Error)
		default:
			// The address it is on goes to its end, whatever stops
			// portal-api meanwhile.
			err = a.send(context.WithoutCancel(g.ctx), m)
			var f *mail.Failure
			switch {
			case err == nil:
				t.Sent, t.Error = true, nil
				log.Printf("mail: %s was sent", withArticle(m.Kind))
			case errors.As(err, &f):
				why := failedSend(f)
				t.Error = &why
				logTry(m.Kind, t.Tries, string(f.Reason))
			default:
				// The settings portal-api runs with name no server or no
				// sender, whatever the resource says.
				why := refuseNotReady
				t.Error = &why
				logTry(m.Kind, t.Tries, why.Error)
			}
		}
		if !g.keep(start && t.Sent) {
			return
		}
	}
}

// send sends a mail through the server portal-api runs with; tests send
// through their own.
func (a *API) send(ctx context.Context, m mail.Message) error {
	if a.sendMail != nil {
		return a.sendMail(ctx, m)
	}
	return mail.New(a.cfg.Mail, a.mailLocalName()).Send(ctx, m)
}

// alertAddress is an alert's link as a mail carries it: on the platform's
// public address. Without a full one mail is not ready and nothing is sent.
func (a *API) alertAddress(link string) string {
	return strings.TrimRight(strings.TrimSpace(a.cfg.PublicURL), "/") + link
}
