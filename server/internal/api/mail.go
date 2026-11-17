package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zaentrum/zaentrum-portal/server/internal/k8s"
	"github.com/zaentrum/zaentrum-portal/server/internal/mail"
	"github.com/zaentrum/zaentrum-portal/server/internal/operator"
	"github.com/zaentrum/zaentrum-portal/server/internal/people"
)

// ─── mail ────────────────────────────────────────────────────────────────────
//
// portal-api sends the platform's mail through the SMTP server named in the
// platform's resource, spec.mail, which the operator gives it as its
// environment (package mail). The mail page sets it:
//
//	GET    /mail        admin   the settings without the password, and where mail stands
//	PUT    /mail        admin   writes spec.mail; a new password goes into a new Secret
//	DELETE /mail        admin   removes spec.mail
//	POST   /mail/test   admin   the test mail, to the address the admin names
//
// Mail is set up when a server and a sender are named — in the resource, or,
// where the operator does not run the platform, in portal-api's own
// environment — and ready once the operator's condition Mail is Configured
// for the resource as it is and the portal-api that answers runs with its
// settings, with the password where a user is set. While mail is not set up
// nothing is sent, and every answer says so. The test mail is sent while the
// admin waits: the answer says whether the server took it, and why not.
//
// The user is spec.mail.user. The password alone is kept in a Secret
// portal-api creates and never reads, changes or deletes, the way an addon's
// secret values are kept (operator/mail.go): a new password goes into a new
// Secret, which the resource then names, and a change of the user keeps the
// password that is there. The operator gives portal-api the name of the
// Secret its password comes from, which tells a password that changed.

// mailTestSent is the test mail's answer, as the chapter's Words have it.
const mailTestSent = "A test mail was sent to %s."

// refusal is a refusal of a mail route as ADR-0023, part 4, has a server
// answer one: the case's code, the values its sentence names — numbers and
// names as they are — and, apart, what the cluster said, which the page shows
// under details and never as the message. message carries the sentence of
// the chapter's Words until the portal takes it from the message catalogue
// (H.C18), which then needs the code alone.
type refusal struct {
	Error   string         `json:"error"`
	Values  map[string]any `json:"values,omitempty"`
	Message string         `json:"message"`
	Detail  string         `json:"detail,omitempty"`
}

// The cases the mail routes refuse. A code says the case and has one
// sentence. A case another page words the same — an address, reading the
// platform's resource, a cluster that answers otherwise — has one code, named
// for what it is about; one the mail page words its own way starts with
// mail_.
var (
	// The refusals of save.
	refuseNoServer       = refusal{Error: "mail_server_missing", Message: "Type the name of the mail server."}
	refuseNotAServer     = refusal{Error: "mail_server_invalid", Message: "The server is not a host name or an IP address."}
	refusePort           = refusal{Error: "mail_port_invalid", Message: "The port is a number from 1 to 65535."}
	refuseEncryption     = refusal{Error: "mail_encryption_invalid", Message: "Encryption is starttls, tls or none."}
	refuseNoSender       = refusal{Error: "mail_sender_missing", Message: "Type the sender's address."}
	refuseSender         = refusal{Error: "mail_sender_invalid", Message: "The sender's address is not an e-mail address."}
	refuseSenderName     = refusal{Error: "mail_sender_name_too_long", Message: "The sender's name has at most 60 characters."}
	refuseFurther        = refusal{Error: "mail_further_address_invalid", Message: "One of the further addresses is not an e-mail address."}
	refuseFurtherLimit   = refusal{Error: "mail_further_address_limit", Message: "There are more than 10 further addresses. Keep up to 10."}
	refuseUserNoPassword = refusal{Error: "mail_user_needs_password", Message: "Type the password of this user."}
	refuseControl        = refusal{Error: "mail_control_character", Message: "The mail settings hold a character that cannot be used."}

	// An address with accented or non-Latin letters: the sender's, a further
	// one, the test mail's, and a person's on the people page.
	refuseAddressLetters = refusal{Error: "address_unsupported", Message: "Addresses with accented or non-Latin letters are not supported."}

	// The refusals of send test mail. The first two are also the people
	// page's, and the placeholder save's too: the sender's address, or a
	// further one.
	refuseNotAnAddress = refusal{Error: "address_invalid", Message: "This is not an e-mail address."}
	refusePlaceholder  = refusal{Error: "address_placeholder", Message: "This address is a placeholder: nothing is sent to it. Type another."}
	refuseNotSetUp     = refusal{Error: "mail_not_set_up", Message: "Mail is not set up."}
	// Why nothing is sent while mail is set up and not ready yet, as Why a
	// mail failed says it.
	refuseNotReady        = refusal{Error: "mail_not_ready", Message: "The mail settings are still being applied."}
	refusePasswordMissing = refusal{Error: "mail_password_missing", Message: "The mail server's password is missing."}

	// The platform's resource, and the cluster.
	refuseNotRunByOperator = refusal{Error: "mail_not_run_by_operator", Message: "This platform is not run by the operator. Mail is set in its chart's values."}
	refuseMayNotRead       = refusal{Error: "platform_read_forbidden", Message: "The portal is not allowed to read the platform's settings. Update the operator, which gives the portal that right."}
	refuseMayNotChange     = refusal{Error: "mail_write_forbidden", Message: "The portal is not allowed to change the mail settings. Update the operator, which gives the portal that right."}
	refuseClusterRefused   = refusal{Error: "mail_write_refused", Message: "The cluster refused the mail settings."}
	refuseMailUnknown      = refusal{Error: "mail_settings_unknown", Message: "The cluster does not know mail settings. Update the operator, which brings them."}
	refuseClusterFailed    = refusal{Error: "cluster_unexpected_answer", Message: "The cluster did not answer as expected."}
)

// refuseValueKind is a value of the wrong kind, a number for one, in a field
// that has no sentence of its own: the user and the password. The Words name
// the field, which values carries.
func refuseValueKind(field string) refusal {
	return refusal{Error: "mail_value_invalid", Values: map[string]any{"field": field}, Message: field + " has a value of the wrong kind."}
}

// with is the refusal with what the cluster said under details.
func (f refusal) with(detail string) refusal {
	f.Detail = detail
	return f
}

// refuse answers a refusal of a mail route.
func refuse(w http.ResponseWriter, status int, f refusal) {
	writeJSON(w, status, f)
}

// failedSend is a mail the server did not take, as a refusal.
func failedSend(f *mail.Failure) refusal {
	return refusal{Error: f.Code(), Values: f.Values(), Message: f.Sentence()}
}

// addressRefusal is the refusal of an address ReadAddress did not take:
// notOne where it is no address, the refusal for its letters where they are
// accented or non-Latin.
func addressRefusal(err error, notOne refusal) refusal {
	if errors.Is(err, mail.ErrAddressLetters) {
		return refuseAddressLetters
	}
	return notOne
}

// The identity provider, as GET /mail says it.
const (
	identityBundled  = "bundled"
	identityExternal = "external"
)

// placeholderDomain is the domain of the address the realm's import gives the
// first admin. It counts as no address: nothing is mailed to it.
const placeholderDomain = "zaentrum.local"

// maxMailBody bounds what a mail route reads of a request.
const maxMailBody = 64 << 10

// mailHostName is what a server's name may be, besides an IP address.
var mailHostName = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?(\.[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?)*\.?$`)

// mailIdentity is bundled, where the platform manages its people, or
// external.
func (a *API) mailIdentity() string {
	if a.people == nil {
		return identityExternal
	}
	return identityBundled
}

// placeholder: the address is the realm import's placeholder.
func placeholder(address string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(address)), "@"+placeholderDomain)
}

// ─── where mail stands ───────────────────────────────────────────────────────

// mailState is where mail stands.
type mailState struct {
	// resource is the platform's resource; nil where the operator does not
	// run the platform, and mail is portal-api's own environment.
	resource *operator.Mail
	// saved are the settings as saved, with their defaults filled in: the
	// resource's, else portal-api's own.
	saved       mail.Settings
	passwordSet bool
	setUp       bool
	ready       bool
	// secretMissing: the operator says the Secret named is not there, or
	// lacks its key.
	secretMissing bool
	// passwordMissing: mail is not ready for want of the password: the
	// operator says its Secret is missing, or portal-api runs with a user and
	// no password where it runs with everything else.
	passwordMissing bool
	alertMail       bool
}

// notReady is why nothing is sent while mail is set up and not ready, as Why
// a mail failed says it: the password is missing, else the settings are still
// being applied.
func (st mailState) notReady() refusal {
	if st.passwordMissing {
		return refusePasswordMissing
	}
	return refuseNotReady
}

// mailState reads where mail stands. An error is the cluster's.
func (a *API) mailState(ctx context.Context) (mailState, error) {
	own := a.cfg.Mail.Effective()
	st := mailState{alertMail: true}
	var res *operator.Mail
	if a.op != nil {
		var err error
		if res, err = a.op.Mail(ctx); err != nil && !errors.Is(err, operator.ErrNoResource) {
			return st, err
		}
	}
	_, public := a.publicAddress()
	// With a user, portal-api signs in: it sends nothing without the password.
	signsIn := own.Username == "" || own.Password != ""
	if res == nil {
		// The settings portal-api runs with are all there is, the public
		// address among them.
		st.saved, st.passwordSet = own, own.Password != ""
		st.setUp = own.SetUp() && public
		st.ready = st.setUp && signsIn
		st.passwordMissing = st.setUp && !signsIn
		return st, nil
	}
	st.resource = res
	saved := mail.Settings{
		Host: res.Spec.Host, Port: res.Spec.Port, Encryption: res.Spec.Encryption,
		From: res.Spec.From, FromName: res.Spec.FromName, AlertsTo: res.Spec.AlertsTo,
		Username: res.Spec.User,
	}
	st.saved = saved.Effective()
	st.passwordSet = res.Spec.SecretName != ""
	st.setUp = st.saved.SetUp()
	if c := res.Condition; c != nil {
		current := c.ObservedGeneration == 0 || c.ObservedGeneration == res.Generation
		st.secretMissing = current && c.Reason == operator.MailSecretMissing
		applied := st.setUp && current && c.Reason == operator.MailConfigured &&
			runsWith(own, st.saved) && a.cfg.MailSecret == res.Spec.SecretName && public
		st.ready = applied && signsIn
		st.passwordMissing = st.secretMissing || applied && !signsIn
	}
	if res.AlertMail != nil {
		st.alertMail = *res.AlertMail
	}
	return st, nil
}

// runsWith answers whether portal-api runs with the settings saved, the
// password aside: which password it runs with, the name of its Secret says.
func runsWith(own, saved mail.Settings) bool {
	return strings.EqualFold(own.Host, saved.Host) && own.Port == saved.Port && own.Encryption == saved.Encryption &&
		own.From == saved.From && own.FromName == saved.FromName && sameAddresses(own.AlertsTo, saved.AlertsTo) &&
		own.Username == saved.Username
}

func sameAddresses(a, b []string) bool {
	norm := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, s := range in {
			if s = strings.ToLower(strings.TrimSpace(s)); s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(norm(a), norm(b))
}

// ─── GET /mail ───────────────────────────────────────────────────────────────

// mailDoc is GET /mail.
type mailDoc struct {
	// The settings without the password, with their defaults filled in.
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	Encryption string   `json:"encryption"`
	From       string   `json:"from"`
	FromName   string   `json:"fromName"`
	User       string   `json:"user"`
	AlertsTo   []string `json:"alertsTo"`
	// PasswordSet: a password is kept.
	PasswordSet bool `json:"passwordSet"`
	SetUp       bool `json:"setUp"`
	Ready       bool `json:"ready"`
	// SecretMissing: the condition Mail says the password's Secret is
	// missing.
	SecretMissing bool `json:"secretMissing"`
	// RunByOperator is false where the operator does not run the platform.
	RunByOperator bool `json:"runByOperator"`
	// Identity is bundled or external.
	Identity string `json:"identity"`
	// AlertMail is spec.alerts.mail: whether alerts are mailed.
	AlertMail  bool           `json:"alertMail"`
	Recipients mailRecipients `json:"recipients"`
}

// mailRecipients are whom alert mail goes to: each admin with their address,
// and the further addresses.
type mailRecipients struct {
	// Admins is null when they cannot be read — the people client is not set
	// up, or the identity provider did not answer — and empty with an
	// external identity provider, whose admins the platform cannot see.
	Admins  []mailAdmin `json:"admins"`
	Further []string    `json:"further"`
}

// mailAdmin is an admin and their address, "" for none.
type mailAdmin struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// getMail handles GET /mail.
func (a *API) getMail(w http.ResponseWriter, r *http.Request) {
	st, err := a.mailState(r.Context())
	if err != nil {
		mailReadFailed(w, err)
		return
	}
	s := st.saved
	writeJSON(w, http.StatusOK, mailDoc{
		Host: s.Host, Port: s.Port, Encryption: s.Encryption, From: s.From, FromName: s.FromName,
		User: s.Username, AlertsTo: nonNil(s.AlertsTo),
		PasswordSet: st.passwordSet, SetUp: st.setUp, Ready: st.ready,
		SecretMissing: st.secretMissing, RunByOperator: st.resource != nil,
		Identity: a.mailIdentity(), AlertMail: st.alertMail,
		Recipients: a.mailRecipients(r.Context(), s.AlertsTo),
	})
}

// mailRecipients are the admins with their addresses — the placeholder
// counts as none — and the further addresses.
func (a *API) mailRecipients(ctx context.Context, further []string) mailRecipients {
	out := mailRecipients{Further: nonNil(further)}
	admins, err := a.mailAdmins(ctx)
	switch {
	case errors.Is(err, errPeopleUnavailable):
		return out
	case err != nil:
		log.Printf("mail: the admins could not be read: %v", err)
		return out
	}
	out.Admins = admins
	return out
}

// errPeopleUnavailable: the platform manages its people, and its people
// client is not set up, so the admins cannot be read.
var errPeopleUnavailable = errors.New("the people client is not set up: the admins cannot be read")

// mailAdmins are the admins whose account is enabled, each with their
// address, "" for none — the placeholder counts as none: whom alert mail goes
// to besides the further addresses. None with an external identity provider,
// whose admins the platform cannot see; an error where they cannot be read.
func (a *API) mailAdmins(ctx context.Context) ([]mailAdmin, error) {
	switch a.peopleMode() {
	case peopleExternal:
		return []mailAdmin{}, nil
	case peopleUnavailable:
		return nil, errPeopleUnavailable
	}
	list, err := a.people.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []mailAdmin{}
	for _, p := range list {
		if p.Role != people.RoleAdmin || !p.Enabled {
			continue
		}
		address := strings.TrimSpace(p.Email)
		if placeholder(address) {
			address = ""
		}
		out = append(out, mailAdmin{Name: p.DisplayName, Address: address})
	}
	return out, nil
}

// ─── PUT and DELETE /mail ────────────────────────────────────────────────────

// mailResource is the platform's resource for a write; false once it
// answered that there is none, or that it could not be read.
func (a *API) mailResource(w http.ResponseWriter, r *http.Request) (*operator.Mail, bool) {
	if a.op == nil {
		refuse(w, http.StatusConflict, refuseNotRunByOperator)
		return nil, false
	}
	res, err := a.op.Mail(r.Context())
	switch {
	case errors.Is(err, operator.ErrNoResource):
		refuse(w, http.StatusConflict, refuseNotRunByOperator)
	case err != nil:
		mailReadFailed(w, err)
	default:
		return res, true
	}
	return nil, false
}

// mailBody is what PUT /mail takes, each field as it came: clean refuses a
// value of the wrong kind with its field's sentence and code, as any other
// value, and the decoder's words reach no client.
type mailBody struct {
	Host       json.RawMessage `json:"host"`
	Port       json.RawMessage `json:"port"`
	Encryption json.RawMessage `json:"encryption"`
	From       json.RawMessage `json:"from"`
	FromName   json.RawMessage `json:"fromName"`
	User       json.RawMessage `json:"user"`
	// Password left out, null or empty keeps the one kept.
	Password json.RawMessage `json:"password"`
	AlertsTo json.RawMessage `json:"alertsTo"`
}

// leftOut: a field the body does not have, or has as null.
func leftOut(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

// textField reads a field that holds text: "" where it is left out; false
// for a value of another kind.
func textField(raw json.RawMessage) (string, bool) {
	if leftOut(raw) {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// portField reads the port: 0, the encryption's default, where it is left
// out, else a whole number from 1 to 65535; false for anything else — 0, a
// fraction, a number written as text.
func portField(raw json.RawMessage) (int, bool) {
	if leftOut(raw) {
		return 0, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return 0, false
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || f != math.Trunc(f) || f < 1 || f > 65535 {
		return 0, false
	}
	return int(f), true
}

// listField reads a list of text: none where it is left out; false for
// anything but a list of text.
func listField(raw json.RawMessage) ([]string, bool) {
	if leftOut(raw) {
		return nil, true
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := textField(item)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// control: s holds a control character — a line break, a tab, NUL.
func control(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// mailInput is a PUT /mail as it is written.
type mailInput struct {
	host, encryption, from, fromName, user, password string
	port                                             int
	alertsTo                                         []string
}

// clean checks a PUT /mail: first each field's kind, then that no setting
// holds a control character, then each value. A refusal with no code is
// none.
func (b mailBody) clean() (mailInput, refusal) {
	var in mailInput
	host, ok := textField(b.Host)
	if !ok {
		return in, refuseNotAServer
	}
	if in.port, ok = portField(b.Port); !ok {
		return in, refusePort
	}
	encryption, ok := textField(b.Encryption)
	if !ok {
		return in, refuseEncryption
	}
	from, ok := textField(b.From)
	if !ok {
		return in, refuseSender
	}
	fromName, ok := textField(b.FromName)
	if !ok {
		return in, refuseSenderName
	}
	user, ok := textField(b.User)
	if !ok {
		return in, refuseValueKind("user")
	}
	if in.password, ok = textField(b.Password); !ok {
		return in, refuseValueKind("password")
	}
	further, ok := listField(b.AlertsTo)
	if !ok {
		return in, refuseFurther
	}
	// What is written: the settings trimmed, the password as it is typed.
	host, encryption, from, fromName, in.user = strings.TrimSpace(host), strings.TrimSpace(encryption),
		strings.TrimSpace(from), strings.TrimSpace(fromName), strings.TrimSpace(user)
	if control(host) || control(encryption) || control(from) || control(fromName) || control(in.user) || control(in.password) ||
		slices.ContainsFunc(further, func(s string) bool { return control(strings.TrimSpace(s)) }) {
		return in, refuseControl
	}

	if host == "" {
		return in, refuseNoServer
	}
	if in.host, ok = mailHost(host); !ok {
		return in, refuseNotAServer
	}
	if in.encryption = strings.ToLower(encryption); in.encryption != "" && !mail.KnownEncryption(in.encryption) {
		return in, refuseEncryption
	}
	if from == "" {
		return in, refuseNoSender
	}
	var err error
	if in.from, err = mail.ReadAddress(from); err != nil {
		return in, addressRefusal(err, refuseSender)
	}
	if placeholder(in.from) {
		return in, refusePlaceholder
	}
	if in.fromName = fromName; utf8.RuneCountInString(in.fromName) > mail.MaxFromName {
		return in, refuseSenderName
	}
	for _, raw := range further {
		addr, err := mail.ReadAddress(raw)
		if err != nil {
			return in, addressRefusal(err, refuseFurther)
		}
		if placeholder(addr) {
			return in, refusePlaceholder
		}
		if !slices.ContainsFunc(in.alertsTo, func(s string) bool { return strings.EqualFold(s, addr) }) {
			in.alertsTo = append(in.alertsTo, addr)
		}
	}
	if len(in.alertsTo) > mail.MaxAlertsTo {
		return in, refuseFurtherLimit
	}
	return in, refusal{}
}

// mailHost reads a server's name: a host name or an IP address, without a
// port.
func mailHost(raw string) (string, bool) {
	h := strings.TrimSpace(raw)
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if net.ParseIP(h) != nil {
		return h, true
	}
	if h == "" || len(h) > 253 || !mailHostName.MatchString(h) {
		return "", false
	}
	return h, true
}

// spec is spec.mail as the merge patch writes it: every setting the page
// has, a default — and no user — as null. The Secret is not among them.
func (in mailInput) spec() map[string]any {
	spec := map[string]any{"host": in.host, "from": in.from, "port": nil, "encryption": nil, "fromName": nil, "user": nil, "alertsTo": nil}
	if in.port != 0 {
		spec["port"] = in.port
	}
	if in.encryption != "" {
		spec["encryption"] = in.encryption
	}
	if in.fromName != "" {
		spec["fromName"] = in.fromName
	}
	if in.user != "" {
		spec["user"] = in.user
	}
	if len(in.alertsTo) > 0 {
		spec["alertsTo"] = in.alertsTo
	}
	return spec
}

// putMail handles PUT /mail {host, port, encryption, from, fromName, user,
// password, alertsTo}: spec.mail as the page has it, and the generation that
// write made.
//
// The user is written into spec.mail. A new password goes, alone, into a new
// Secret, which spec.mail.secretName then names; a password left out keeps
// the one there, so a change of the user keeps the password. A user is
// refused where no password is kept and none is given: the Secret is needed
// when a user is set.
//
// The write is checked by the apiserver in a dry run first, validated
// strictly as the write itself is (operator.SetMail): a refused write — a
// cluster that does not know mail settings among them — writes nothing and
// leaves no Secret behind.
func (a *API) putMail(w http.ResponseWriter, r *http.Request) {
	cur, ok := a.mailResource(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMailBody)
	var body mailBody
	if !decode(w, r, &body) {
		return
	}
	in, refused := body.clean()
	if refused.Error != "" {
		refuse(w, http.StatusBadRequest, refused)
		return
	}
	ctx := r.Context()
	spec := in.spec()
	newPassword := in.password != ""
	switch {
	case newPassword:
		spec["secretName"] = operator.MailPendingSecret
	case in.user != "" && cur.Spec.SecretName == "":
		refuse(w, http.StatusBadRequest, refuseUserNoPassword)
		return
	}
	// A password left out: secretName is left out of the patch, and the
	// Secret named stays.
	if _, err := a.op.SetMail(ctx, cur.Resource, spec, true); err != nil {
		mailWriteFailed(w, err)
		return
	}
	if newPassword {
		secret, err := a.op.CreateMailSecret(ctx, in.password)
		if err != nil {
			mailWriteFailed(w, err)
			return
		}
		spec["secretName"] = secret
	}
	generation, err := a.op.SetMail(ctx, cur.Resource, spec, false)
	if err != nil {
		if newPassword {
			log.Printf("mail: the settings were not saved; Secret %s, made for the new password, is named by nothing, and the operator removes it", spec["secretName"])
		}
		mailWriteFailed(w, err)
		return
	}
	// Who saved, in the log: the request reaches the cluster as portal-api's
	// own service account.
	if newPassword {
		log.Printf("mail: %s saved the mail settings, with a new password in Secret %s", requester(r), spec["secretName"])
	} else {
		log.Printf("mail: %s saved the mail settings", requester(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"generation": generation})
}

// deleteMail handles DELETE /mail: spec.mail goes, and the answer is the
// generation that made. The Secret it named is the operator's to remove.
func (a *API) deleteMail(w http.ResponseWriter, r *http.Request) {
	cur, ok := a.mailResource(w, r)
	if !ok {
		return
	}
	generation, err := a.op.RemoveMail(r.Context(), cur.Resource)
	if err != nil {
		mailWriteFailed(w, err)
		return
	}
	log.Printf("mail: %s removed the mail settings", requester(r))
	writeJSON(w, http.StatusOK, map[string]any{"generation": generation})
}

// mailReadFailed answers a platform resource the cluster did not let be read.
func mailReadFailed(w http.ResponseWriter, err error) {
	log.Printf("mail: the platform's resource could not be read: %v", err)
	status, f := unreadable(err)
	refuse(w, status, f)
}

// unreadable is the refusal, and its status, for a platform resource the
// cluster did not let be read, with the cluster's words under details. A mail
// is not sent then, whatever portal-api's own settings say.
func unreadable(err error) (int, refusal) {
	if k8s.IsForbidden(err) {
		return http.StatusServiceUnavailable, refuseMayNotRead.with(err.Error())
	}
	return http.StatusBadGateway, refuseClusterFailed.with(err.Error())
}

// mailWriteFailed answers a write the cluster did not take.
func mailWriteFailed(w http.ResponseWriter, err error) {
	var ae *k8s.APIError
	switch {
	case errors.Is(err, operator.ErrMailUnknown):
		log.Printf("mail: the settings were not saved: %v", err)
		refuse(w, http.StatusServiceUnavailable, refuseMailUnknown.with(err.Error()))
	case k8s.IsNotFound(err):
		refuse(w, http.StatusConflict, refuseNotRunByOperator)
	case k8s.IsForbidden(err):
		refuse(w, http.StatusServiceUnavailable, refuseMayNotChange.with(err.Error()))
	case errors.As(err, &ae) && (ae.Code == http.StatusBadRequest || ae.Code == http.StatusUnprocessableEntity):
		refuse(w, http.StatusUnprocessableEntity, refuseClusterRefused.with(err.Error()))
	default:
		refuse(w, http.StatusBadGateway, refuseClusterFailed.with(err.Error()))
	}
}

// ─── POST /mail/test ─────────────────────────────────────────────────────────

// testMail handles POST /mail/test {to}: the test mail, sent while the admin
// waits with the settings portal-api runs with — 200 once the server took it,
// 502 with the reason's code and sentence when it did not. Nothing is sent
// while mail is not set up, nor before it is ready: the refusal says which.
func (a *API) testMail(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMailBody)
	var body struct {
		// To is read as it came: a value of the wrong kind is no address.
		To json.RawMessage `json:"to"`
	}
	if !decode(w, r, &body) {
		return
	}
	st, err := a.mailState(r.Context())
	if err != nil {
		mailReadFailed(w, err)
		return
	}
	switch {
	case !st.setUp:
		refuse(w, http.StatusConflict, refuseNotSetUp)
		return
	case !st.ready:
		refuse(w, http.StatusConflict, st.notReady())
		return
	}
	typed, ok := textField(body.To)
	if !ok {
		refuse(w, http.StatusBadRequest, refuseNotAnAddress)
		return
	}
	to, err := mail.ReadAddress(typed)
	switch {
	case err != nil:
		refuse(w, http.StatusBadRequest, addressRefusal(err, refuseNotAnAddress))
		return
	case placeholder(to):
		refuse(w, http.StatusBadRequest, refusePlaceholder)
		return
	}
	m := mail.TestMail(a.mailPlatformHost(), to)
	if err := mail.New(a.cfg.Mail, a.mailLocalName()).Send(r.Context(), m); err != nil {
		var f *mail.Failure
		if !errors.As(err, &f) {
			// The settings portal-api runs with name no server or no
			// sender, whatever the resource says.
			log.Printf("mail: a %s was refused: mail is not set up here", m.Kind)
			refuse(w, http.StatusConflict, refuseNotSetUp)
			return
		}
		log.Printf("mail: a %s was refused (%s)", m.Kind, f.Reason)
		refuse(w, http.StatusBadGateway, failedSend(f))
		return
	}
	log.Printf("mail: a %s was sent", m.Kind)
	writeJSON(w, http.StatusOK, map[string]any{"to": to, "message": fmt.Sprintf(mailTestSent, to)})
}

// publicAddress is the platform's public address, PORTAL_PUBLIC_URL, where it
// is a full one: http or https, and a host. A link in a mail and the
// platform's name in it come from it alone, never from the request that asked
// for the mail; without it mail is not set up, or not ready, and nothing is
// sent (mailState).
func (a *API) publicAddress() (*url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(a.cfg.PublicURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, false
	}
	return u, true
}

// mailPlatformHost is the platform's name in a mail: the host of its public
// address. Without one nothing is sent, so it is never "".
func (a *API) mailPlatformHost() string {
	if u, ok := a.publicAddress(); ok {
		return u.Host
	}
	return ""
}

// mailLocalName is the name portal-api gives itself to the mail server: the
// platform's host name, else localhost.
func (a *API) mailLocalName() string {
	if u, ok := a.publicAddress(); ok {
		return u.Hostname()
	}
	return "localhost"
}
