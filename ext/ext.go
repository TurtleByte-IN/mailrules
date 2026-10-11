// Package ext is the plug-in point of the MailRules daemon: the one public package through
// which a module built outside this repository adds a feature. A module registers API
// routes, served behind the same sign-in and CSRF checks as the built-in ones unless it
// marks one Public (served before sign-in) or Webhook (a call from another server), a
// capability the web app reads in GET /api/settings `features`, and optionally the way
// people sign in (Module.SignIn) and are deleted at the sign-in service when they delete
// their own account (Module.DeleteIdentity), the way mailboxes of some providers are
// connected with one-click sign-in (Module.Mailboxes), sign-in forms for mailboxes of
// providers the module connects itself (Module.MailboxForms), and the delivery of rule
// notifications (Module.Notifications).
// ext/daemon runs the daemon with the modules a build compiles in.
//
// This package is a contract. What it exports is all a module may use of the daemon: the
// mail it may read (read-only, with BODY.PEEK), the models it may call, the rules and
// folders it may read, the notices of what rules did, and the helpers that answer HTTP
// requests the way the built-in routes do. A change to it means the modules follow.
package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// Module is one feature compiled into the daemon from outside this repository.
type Module struct {
	// Name is the capability the module provides. GET /api/settings reports
	// features.<Name> as true while the module is in the build, and the web app shows the
	// feature's screen only then.
	Name string
	// Routes returns the module's API endpoints, given what the daemon offers them. Each
	// one is served only to a signed-in user, with the CSRF check of every other route,
	// unless the route says otherwise (Route.Public, Route.Webhook). Routes is called while
	// the daemon starts, before it serves anything, perhaps more than once: build the list,
	// do not call h.
	Routes func(h Host) []Route
	// SignIn is the path of the module's Public GET route that starts signing someone in.
	// When a module sets it, the build offers no password sign-in or first-run setup, and
	// the sign-in screen sends the browser there. At most one module of a build sets it.
	SignIn string
	// SignOut is the path of the module's Public GET route the browser goes to after the
	// daemon has ended its session, so the sign-in service ends its own. It needs SignIn.
	SignOut string
	// DeleteIdentity, when set, deletes a person at the sign-in service when they delete
	// their own account in Settings (Delete my account, POST /api/auth/delete-account),
	// which every build with SignIn offers (GET /api/settings reports
	// features.delete_account). provider and subject are the Identity the person signed in
	// as. The daemon calls it first and deletes nothing when it returns an error, which it
	// logs and does not show; when it returns nil the daemon deletes the person's MailRules
	// account at once (Host.ForgetIdentity, except that the same identity signing in again
	// gets a new, empty user, never their data) and ends the request's session, so the
	// service's own deletion notice calling ForgetIdentity afterwards changes nothing.
	// Without it, Delete my account deletes only the MailRules account and the person's
	// login at the sign-in service stays. It needs SignIn.
	DeleteIdentity func(ctx context.Context, provider, subject string) error
	// Mailboxes, when set, connects mailboxes of some providers with one-click sign-in: the
	// person signs in to the provider (OAuth) instead of pasting an app password, and the
	// daemon logs in to IMAP with an access token the module gives it (SASL XOAUTH2).
	// Reading, rules, moving, IDLE and undo are the daemon's own IMAP code, as for every
	// mailbox. At most one module of a build sets it.
	Mailboxes *MailboxSignIn
	// MailboxForms, when set, connects mailboxes of some providers through a sign-in form
	// on the add-mailbox screen that the module answers, step by step: the provider's
	// username and password, then a two-factor code or a separate mailbox password when the
	// account asks for them. The module signs in with them wherever it keeps the connection
	// (for Proton, the operator's Proton Mail Bridge), and adds the mailbox with the IMAP
	// password it gets back (Host.AddMailbox, NewMailbox.Password). The person never sees
	// that password, so when the server stops accepting it (Bridge lost the account's
	// session) the mailbox stops in reconnect_needed, not auth_failed, and its Reconnect
	// button opens the form again (MailboxForm.Path). At most one form per provider in a
	// build.
	MailboxForms []MailboxForm
	// Notifications, when set, delivers rule notifications. The daemon then offers Settings'
	// Notifications and webhooks, where a team adds destinations of the module's Kinds, and
	// a rule's More options, where a rule chooses the destinations to tell; it stores the
	// destinations, their addresses and secrets encrypted, and for every email a rule with
	// destinations acts on live it writes one notice per destination and hands each to
	// Notify. At most one module of a build sets it.
	Notifications *Notifications
}

// MailboxSignIn is how a module connects mailboxes with one-click sign-in (Module.Mailboxes).
// The module holds no connection, and the daemon knows nothing of OAuth: it stores the
// secret the module gives it (Host.AddMailbox, Host.ReconnectMailbox), encrypted like an app
// password, never returns it in the API and never logs it, and hands it back to Login just
// before each login.
type MailboxSignIn struct {
	// Providers are the presets whose mailboxes the module signs in to: "gmail", "outlook"
	// or both. GET /api/presets offers a one-click tile for each, and lists the Outlook
	// preset only when it is here: Outlook takes no app password. A build without the
	// module has no Outlook tile, and its Gmail tile is the app-password one.
	Providers []string
	// Connect is the path of the module's GET route, served to a signed-in user (not
	// Public), that starts the provider's sign-in. The wizard's one-click tile sends the
	// browser to Connect?provider=<preset>; the Reconnect button of a mailbox in
	// reconnect_needed to Connect?provider=<preset>&account=<id>. The route ends with
	// Host.MailboxAdded, Host.MailboxReconnected or Host.MailboxFailed.
	Connect string
	// Login is called just before every IMAP login to a mailbox the module connected, with
	// the mailbox's provider and the secret stored for it (such as a refresh token), and
	// returns the access token the daemon authenticates with as the mailbox's address. It
	// is called for one mailbox at a time, and again for each new connection: a session the
	// provider closes when its token expires (Gmail and Outlook do after about an hour) is
	// a normal reconnect with a fresh token. ErrReconnect (or an error that wraps it) means
	// the secret no longer works, revoked or expired: the mailbox stops in
	// reconnect_needed, and no action runs, until the person signs in again. Any other
	// error is retried as a lost connection. Errors are shown to the person and logged, so
	// they must not contain the secret or a token.
	Login func(ctx context.Context, provider, secret string) (MailToken, error)
}

// MailToken is what MailboxSignIn.Login returns.
type MailToken struct {
	AccessToken string
	// Secret, when not empty, replaces the stored secret (a rotated refresh token). The
	// daemon stores it, encrypted, before it authenticates with AccessToken.
	Secret string
}

// NewMailbox is a mailbox a module connects with Host.AddMailbox.
type NewMailbox struct {
	// Provider is one of MailboxSignIn.Providers, or with Password the Provider of one of
	// the module's MailboxForms.
	Provider string
	Address  string // the mailbox's email address, which IMAP signs in as
	Host     string // the IMAP server; empty = the preset's (imap.gmail.com, outlook.office365.com, 127.0.0.1)
	Port     int    // 0 = the preset's (993; 1143 for proton)
	// TLSMode is "implicit" or "starttls"; empty = the preset's (implicit; starttls for
	// proton).
	TLSMode string
	// Secret is what Login turns into an access token, such as a refresh token; with
	// Password, the password IMAP logs in with.
	Secret string
	// Password: Secret is a password the daemon logs in with (IMAP LOGIN), as with an app
	// password, and Login is not called; the mailbox is then like any password mailbox.
	// For a provider of the module's MailboxForms.
	Password bool
	// CertFingerprint, when not empty, is the SHA-256 fingerprint (64 hexadecimal digits,
	// colons allowed) of the one certificate the server may present, such as the
	// self-made one of Proton Mail Bridge; empty = the system's trust store decides.
	CertFingerprint string
}

// MailboxForm is a sign-in form for one provider's mailboxes that a module answers
// (Module.MailboxForms). The daemon only shows it: what is typed into it goes to the
// module's route and nowhere else, and the daemon neither stores nor logs it.
type MailboxForm struct {
	// Provider is the preset whose mailboxes the form connects: "proton". GET /api/presets
	// gives the preset a form_url, the wizard shows a sign-in tile for it, and the preset's
	// own password tile (a Bridge on the daemon's machine) is not offered.
	Provider string
	// Path is the module's POST route under /api/, served to a signed-in user (not Public),
	// that each step of the form is posted to as a MailboxFormInput (read it with
	// Host.ReadJSON). It answers 200 with a MailboxFormStep (Host.WriteJSON): the next step
	// to ask for, the same step again with a sentence saying what was wrong, or the mailbox
	// it added. The Reconnect button of a mailbox the form connected opens the same form,
	// and every step it posts carries the mailbox's id (MailboxFormInput.AccountID): the
	// module signs the person in again and gives the mailbox its new password with
	// Host.ReconnectMailbox instead of adding one.
	Path string
}

// The steps of a mailbox sign-in form (MailboxFormInput.Step, MailboxFormStep.Step).
const (
	// MailboxStepSignIn asks for the provider's username and password. The form starts here.
	MailboxStepSignIn = "sign_in"
	// MailboxStepCode asks for the account's two-factor code.
	MailboxStepCode = "code"
	// MailboxStepMailboxPassword asks for the account's separate mailbox password.
	MailboxStepMailboxPassword = "mailbox_password"
)

// MailboxFormInput is one step of a mailbox sign-in form as the browser posts it.
type MailboxFormInput struct {
	Step string `json:"step"` // one of the MailboxStep* constants
	// State is what the previous answer's MailboxFormStep.State was; empty on the first step.
	State    string `json:"state,omitempty"`
	Username string `json:"username,omitempty"` // MailboxStepSignIn
	Password string `json:"password,omitempty"` // MailboxStepSignIn and MailboxStepMailboxPassword
	Code     string `json:"code,omitempty"`     // MailboxStepCode
	// AccountID, on every step of a form opened by a mailbox's Reconnect button, is that
	// mailbox: sign the person in again and end with Host.ReconnectMailbox for it, as the
	// same address. 0 when the form adds a mailbox.
	AccountID int64 `json:"account_id,omitempty"`
}

// MailboxFormStep is what a module's form route answers for one step.
type MailboxFormStep struct {
	// Step is the step the form asks for next, one of the MailboxStep* constants; empty
	// once AccountID is set. Answer the step just posted again, with Message, to have it
	// typed again; MailboxStepSignIn starts over.
	Step string `json:"step,omitempty"`
	// State is anything the module needs to know which sign-in the next step belongs to;
	// the browser posts it back unread. It must not hold a password or a code.
	State string `json:"state,omitempty"`
	// Message is a sentence shown above the step, such as why the password or the code was
	// refused, or that the provider's free plan cannot be connected.
	Message string `json:"message,omitempty"`
	// AccountID is the mailbox Host.AddMailbox connected: the wizard goes on at its Rules
	// step, as for any other mailbox. For a form opened to reconnect a mailbox, it is that
	// mailbox once Host.ReconnectMailbox has taken the new password: the form closes and
	// Mailboxes says the mailbox is connected again.
	AccountID int64 `json:"account_id,omitempty"`
}

var (
	// ErrReconnect is what MailboxSignIn.Login returns when the stored secret no longer
	// works: the mailbox waits in reconnect_needed until the person signs in again. The
	// mail server refusing the access token puts the mailbox there too.
	ErrReconnect = mail.ErrReconnect
	// ErrMailboxExists is what Host.AddMailbox returns when the signed-in user's team
	// already has the mailbox. Nothing changed.
	ErrMailboxExists = errors.New("this mailbox is already connected")
	// ErrMailboxMismatch is what Host.ReconnectMailbox returns when the person signed in to
	// the provider as another address than the mailbox's. Nothing changed.
	ErrMailboxMismatch = errors.New("signed in as another address than the mailbox's")
	// ErrNoMailbox is what Host.ReconnectMailbox returns for a mailbox that is not one the
	// module connected (one-click for one of its providers, or through one of its sign-in
	// forms) and the signed-in user added.
	ErrNoMailbox = errors.New("no such mailbox connected by the module")
)

// The codes Host.MailboxFailed sends the browser back to Mailboxes with.
const (
	// MailboxRefused: the person declined, or the provider refused the sign-in (a bad or
	// expired code, a state mismatch, a permission not granted).
	MailboxRefused = "refused"
	// MailboxUnavailable: the provider's sign-in could not be reached.
	MailboxUnavailable = "unavailable"
	// MailboxExists: Host.AddMailbox returned ErrMailboxExists.
	MailboxExists = "exists"
	// MailboxMismatch: Host.ReconnectMailbox returned ErrMailboxMismatch.
	MailboxMismatch = "mismatch"
	// MailboxConnectFailed: Host.AddMailbox or Host.ReconnectMailbox failed otherwise: the
	// mail server refused the login or could not be reached.
	MailboxConnectFailed = "connect_failed"
)

// Notifications is how a module delivers rule notifications (Module.Notifications). The
// daemon decides what is told: a notice for every email a rule acts on live, one per
// destination the rule names, written in the transaction that records the rule's actions,
// so a restart loses none. Nothing is told of dry-run, of a cleanup, of an undo or a
// correction, or of an email a sender rule settled. The module only delivers.
type Notifications struct {
	// Kinds are the destination kinds the module delivers to: DestinationWebhook,
	// DestinationSlack or both. Settings offers only these.
	Kinds []string
	// Notify takes one notice for one destination. One worker calls it, one notice at a
	// time, within a few seconds of the rule acting, with a ctx that ends after 30 seconds,
	// and calls it again for the same notice (the same Notice.ID) until it returns nil:
	// every notice reaches the module at least once, so a module that must not deliver one
	// twice tells them apart by ID. nil means the module has the notice: the daemon no
	// longer keeps the email's details, and delivering it, retries included, is the
	// module's job. Report each outcome with d.Record, then or later (d stays valid while
	// the daemon runs). An error is logged and the notice handed over again, after a few
	// seconds at first and at most an hour apart, until NoticeDays have passed. Errors, like
	// a Delivery's Reason, must not contain the destination's URL or secret, nor the email's
	// sender or subject.
	Notify func(ctx context.Context, n Notice, d Deliveries) error
	// Start, when set, is called once as the daemon starts, in a goroutine of its own, with
	// a ctx that ends when the daemon stops; the daemon waits for it to return before it
	// closes the database. A module that retries deliveries on its own runs its loop here.
	Start func(ctx context.Context, d Deliveries)
}

// The kinds of destination a module may deliver to (Notifications.Kinds, Destination.Kind).
const (
	// DestinationWebhook: a POST of JSON to the destination's URL, signed with its Secret, so
	// the receiving server can tell the notice came from MailRules.
	DestinationWebhook = store.DestinationWebhook
	// DestinationSlack: a message through a Slack incoming webhook (https://hooks.slack.com/…).
	DestinationSlack = store.DestinationSlack
)

// NoticeDays is how long the daemon keeps a notice and what was reported of delivering it.
const NoticeDays = store.NoticeDays

// Destination is where one notice goes, as a team added it in Settings.
type Destination struct {
	ID   int64
	Name string
	Kind string // DestinationWebhook or DestinationSlack
	URL  string // the address: never log it
	// Secret is a webhook's signing secret, shown to the person once when they added the
	// destination: sign every delivery with it. Empty for DestinationSlack. Never log it.
	Secret string
}

// Notice is what one rule did to one email, for one destination. It never carries the
// email's text. Marshalled as JSON it leaves the destination out: it is a webhook body as
// it stands.
type Notice struct {
	// ID is the notice's own: the same on every hand-over of it, and what Delivery names.
	ID          int64       `json:"id"`
	TenantID    int64       `json:"tenant_id"`
	Destination Destination `json:"-"`
	// Mailbox is the address of the mailbox the email is in, as Mailboxes names it.
	Mailbox  string `json:"mailbox"`
	RuleID   int64  `json:"rule_id"` // 0 once the rule was deleted
	RuleName string `json:"rule_name"`
	// Actions are the rule's actions that were done, in order: move, archive, trash, junk,
	// flag, unflag, read, unread or keep. A run a failure cut short lists those done before.
	Actions []string `json:"actions"`
	Folder  string   `json:"folder"` // where the email is after them
	From    string   `json:"from"`   // the sender's address
	// FromName is the sender's display name; empty when the email gave none.
	FromName string    `json:"from_name"`
	Subject  string    `json:"subject"`
	At       time.Time `json:"at"` // when the rule acted
	// ActivityID is the email in the web app's Activity; Link opens it there
	// (MAILRULES_PUBLIC_URL + #/activity?id=<ActivityID>).
	ActivityID int64  `json:"activity_id"`
	Link       string `json:"link"`
}

// Deliveries is where a module reports what became of the notices it took.
type Deliveries interface {
	// Record stores the outcome of delivering a notice, which Settings lists under the
	// destination's Recent deliveries (the last 20, kept NoticeDays). A module that retries
	// may record each attempt or only the last. ErrNoNotice when the notice is no longer
	// kept: its destination or mailbox was removed, or it is older than NoticeDays.
	Record(ctx context.Context, d Delivery) error
}

// Delivery is one outcome of delivering a notice.
type Delivery struct {
	NoticeID int64
	Sent     bool
	// Reason, when not Sent, is why, as a short sentence the person reads in Settings
	// ("Slack answered 404: the webhook was removed."). Never the URL, the secret, the
	// sender or the subject.
	Reason string
	At     time.Time // when it happened; zero = now
}

// ErrNoNotice is what Deliveries.Record returns for a notice the daemon no longer keeps.
var ErrNoNotice = errors.New("no such notice is kept")

// Route is one API endpoint of a module, as http.ServeMux patterns it: a method and a
// path such as "/api/rules/suggest".
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
	// Public serves the route to someone not signed in. CSRF still applies to methods
	// other than GET and HEAD unless Webhook is set.
	Public bool
	// Webhook (with Public) exempts the route from CSRF: a call from another server, which
	// carries no session or token. The handler must authenticate the caller itself, for
	// example by checking a signature header.
	Webhook bool
}

// Identity is a person as a sign-in service names them.
type Identity struct {
	Provider string // the sign-in service, such as "workos"
	Subject  string // the service's stable id for the person
	Email    string
	Tenant   string // the service's id of the organisation they signed in under
}

var (
	// ErrTenantMismatch is what Host.SignIn returns for a returning person who arrived
	// under a different organisation than the one they signed up with. Nothing changed.
	ErrTenantMismatch = store.ErrTenantMismatch
	// ErrEmailInUse is what Host.SignIn returns when another user of the daemon already
	// has the email. Nothing changed.
	ErrEmailInUse = store.ErrEmailInUse
)

// ForgetDays is how many days after Host.ForgetIdentity a forgotten person's data is
// removed.
const ForgetDays = store.ForgetDays

// The codes Host.SignInFailed sends the browser back to the sign-in screen with.
const (
	// SignInUnavailable: the sign-in service could not be reached.
	SignInUnavailable = "unavailable"
	// SignInRefused: the service refused the sign-in (a bad or expired code, a state
	// mismatch).
	SignInRefused = "refused"
	// SignInChooseTenant: the person belongs to several organisations and chose none.
	SignInChooseTenant = "choose_tenant"
	// SignInTenantMismatch: Host.SignIn returned ErrTenantMismatch.
	SignInTenantMismatch = "tenant_mismatch"
	// SignInEmailInUse: Host.SignIn returned ErrEmailInUse.
	SignInEmailInUse = "email_in_use"
)

// Host is what the daemon gives a module's routes.
type Host interface {
	// ReadJSON decodes a small JSON request body into v, refusing unknown fields. When it
	// returns false it has answered 400 invalid_json.
	ReadJSON(w http.ResponseWriter, r *http.Request, v any) bool
	// WriteJSON answers v as JSON with status.
	WriteJSON(w http.ResponseWriter, status int, v any)
	// Invalid answers 400 invalid_input for the request field at path, with message as the
	// sentence the screen shows.
	Invalid(w http.ResponseWriter, path, message string)
	// Fail answers for an error from the mailbox, a model or the database the way the
	// built-in AI features do: 409 no_composer_model or anthropic_workspace_needed, 502
	// model_error for an error that wraps ErrModel, 400 invalid_input at folder for a folder
	// the account does not have, 409 account_offline, 502 mailbox_error, else 500. A client
	// that dropped the request gets nothing more.
	Fail(w http.ResponseWriter, r *http.Request, err error)
	// WantsStream reports whether the client asked for server-sent events (its Accept
	// header names text/event-stream).
	WantsStream(r *http.Request) bool
	// EventStream writes server-sent events to w.
	EventStream(w http.ResponseWriter) EventStream

	// UserID is the signed-in user the request is for.
	UserID(r *http.Request) int64
	// Scope checks a selection of mail the way a cleanup check takes it and fills in what it
	// leaves out: INBOX when no folder is named, and at most the newest 2,000 emails. An
	// account the user cannot see is refused as a missing one. When it returns false it has
	// answered 400 invalid_input.
	Scope(w http.ResponseWriter, r *http.Request, in ScopeInput) (Scope, bool)
	// Mail is the connection to an account, read-only; the id must come from Scope. It
	// fails while the account is not connected; answer that with Fail.
	Mail(accountID int64) (Mail, error)
	// Folders lists the names of the account's folders, sorted; the id must come from Scope.
	Folders(ctx context.Context, accountID int64) ([]string, error)
	// Rules lists the rules of the user's tenant in priority order.
	Rules(ctx context.Context, userID int64) ([]Rule, error)

	// Composer is the rule composer model chosen in the signed-in user's tenant's Settings;
	// ctx must be the request's. Its error, when none is set up, is answered with Fail (409
	// no_composer_model).
	Composer(ctx context.Context) (Generator, error)
	// RecordUsage books one model call on the signed-in user's tenant's usage ledger under
	// purpose, one of the ledger's purposes in api/openapi.yaml; ctx must be the request's.
	// A failure to book it is logged, not returned.
	RecordUsage(ctx context.Context, purpose string, u Usage)
	// UsageChanged tells the open screens of the signed-in user's tenant that its usage
	// ledger changed; ctx must be the request's.
	UsageChanged(ctx context.Context)

	// SignIn signs in the person a sign-in service vouched for, from the module's SignIn
	// flow: it finds their tenant by (Provider, Tenant), or makes it, finds their user by
	// (Provider, Subject), or makes one in that tenant who has no password, sets the user's
	// email to id.Email, and starts the browser's session as a password sign-in does. It
	// fails, changing nothing, with ErrTenantMismatch or ErrEmailInUse (answer those with
	// SignInFailed and the matching code), or with an error of the database.
	SignIn(w http.ResponseWriter, r *http.Request, id Identity) (userID int64, err error)
	// EndSessions signs the identity out of every browser at once. An identity nobody has
	// is not an error.
	EndSessions(ctx context.Context, provider, subject string) error

	// ForgetIdentity is for a person the sign-in service deleted; the daemon does the same
	// for a person who deletes their own account, except that they never get their data
	// back. In one step that changes all or nothing, it ends their sessions as EndSessions
	// does, frees their email (a new identity with that email signs up at once and gets a
	// new user, not their data) and deletes their secrets: their mailboxes are stopped
	// first, then their passwords and tokens are deleted, with, when they are their
	// tenant's only member, the tenant's stored model API keys and the addresses and
	// secrets of its notification destinations.
	// The rest of their data is removed ForgetDays later, unless the same identity
	// (Provider, Subject) signs in again first, which calls the removal off and gives their
	// rules and history back; their mailboxes then wait for a new password, the model keys
	// stay unset and the destinations must be added again. A person who is their tenant's
	// only member takes the whole tenant with them: its mailboxes and everything MailRules
	// recorded of them, rules, sender rules, settings, usage ledger, and notification
	// destinations with their notices and delivery log. A member of a team takes all the
	// mailboxes they own, shared or not; the team keeps the rest. An identity nobody has is
	// not an error, and forgetting one again changes nothing, the date of the removal
	// included. The self-host admin, who has no identity, cannot be forgotten.
	ForgetIdentity(ctx context.Context, provider, subject string) error

	// SignInFailed sends the browser back to the sign-in screen with code, one of the
	// SignIn* codes (303 to /?signin_error=<code>).
	SignInFailed(w http.ResponseWriter, r *http.Request, code string)

	// AddMailbox connects a mailbox the module signs in to (Module.Mailboxes, or with
	// m.Password Module.MailboxForms) for the signed-in user, from the module's Connect or
	// form route; ctx must be the request's. It does what the add-mailbox wizard does with
	// an app password: it logs in once (calling MailboxSignIn.Login with m.Secret, or with
	// m.Password logging in with it as the password), checking the server's certificate
	// against m.CertFingerprint when set, reads the folders, stores the mailbox with its
	// secret encrypted, sorts only mail that arrives from now on, and starts watching it,
	// in dry-run while the team's dry-run is on (as it is until the person turns it off).
	// It returns the mailbox's id, for MailboxAdded or MailboxFormStep.AccountID. It fails,
	// storing nothing, with ErrMailboxExists, with an error that wraps ErrReconnect when
	// the provider refused the secret or the token, or with an error of the mail server or
	// the database (answer those with MailboxFailed and the matching code, or a form step's
	// Message).
	AddMailbox(ctx context.Context, m NewMailbox) (accountID int64, err error)
	// ReconnectMailbox gives a mailbox the signed-in user added through the module a new
	// secret, from the module's Connect route called with account=<id> (a one-click
	// mailbox: a new refresh token) or from its form route posted with
	// MailboxFormInput.AccountID (a mailbox its sign-in form connected: the new password);
	// ctx must be the request's. address is the one the person just signed in to the
	// provider as. It logs in once with the new secret, then stores it, encrypted, in place
	// of the old one and connects the mailbox again, with its rules, activity and undo
	// history as they were: how a mailbox in reconnect_needed comes back. It fails,
	// changing nothing, with ErrNoMailbox, ErrMailboxMismatch, or as AddMailbox does.
	ReconnectMailbox(ctx context.Context, accountID int64, address, secret string) error
	// MailboxAdded sends the browser to the add-mailbox wizard at its Rules step for the
	// mailbox AddMailbox connected (303 to /#/accounts?added=<id>).
	MailboxAdded(w http.ResponseWriter, r *http.Request, accountID int64)
	// MailboxReconnected sends the browser to Mailboxes, which says the mailbox is
	// connected again (303 to /#/accounts?reconnected=<id>).
	MailboxReconnected(w http.ResponseWriter, r *http.Request, accountID int64)
	// MailboxFailed sends the browser back to Mailboxes with code, one of the Mailbox*
	// codes, which it explains (303 to /#/accounts?mailbox_error=<code>).
	MailboxFailed(w http.ResponseWriter, r *http.Request, code string)
}

// ScopeInput is a selection of mail as a request sends it: the fields of the contract's
// CleanupCheckRequest. Embed it in a request body to read them.
type ScopeInput struct {
	AccountID int64  `json:"account_id"`
	Folder    string `json:"folder"`
	Since     *int64 `json:"since"` // unix seconds; null or 0 = all of the folder
	Limit     *int   `json:"limit"`
}

// Scope is a checked selection of mail: the newest Limit emails of Folder received on or
// after Since (zero = all of it).
type Scope struct {
	AccountID int64
	Folder    string
	Since     time.Time
	Limit     int
}

// EventStream writes server-sent events to one response. Its headers go out with the first
// event, so a request that fails before it has anything to report can still be answered
// as plain JSON.
type EventStream interface {
	// Send writes one event, its data v as JSON, and flushes it.
	Send(event string, v any)
	// Started reports whether an event has been sent, after which the answer can only go on
	// as events.
	Started() bool
	// Fail sends the error event of a run that stopped after the stream began: the
	// contract's ErrorBody, anthropic_workspace_needed when that is why, else code and
	// message.
	Fail(err error, code, message string)
}

// Mail reads the mail already in one account's folders. Every fetch is BODY.PEEK, so
// nothing is marked read, and nothing here can move, flag or delete a message.
type Mail interface {
	// List lists the newest limit emails of folder received on or after since (zero = all
	// of it), newest last, and how many the folder holds in that range before limit cut it.
	// A folder the account does not have fails with an error Fail answers as such.
	List(ctx context.Context, folder string, since time.Time, limit int) (refs []MsgRef, matched int, err error)
	// Fetch reads the listed emails in batches and calls each for every one, with its index
	// in refs, its summary (nil when it is gone or cannot be read as an email) and whether
	// the owner has read it. each is called for several emails at once; its first error
	// ends the fetch and is returned.
	Fetch(ctx context.Context, refs []MsgRef, opts FetchOptions, each func(ctx context.Context, i int, email *Summary, seen bool) error) error
}

// MsgRef names one listed email.
type MsgRef = mail.MsgRef

// FetchOptions says how much of each email's text Mail.Fetch reads: NoBody for the
// headers only (Summary.Body is then empty), else the text cut to BodyChars characters
// (0 = all of it).
type FetchOptions = composer.FetchOptions

// Summary is one email as the rules see it: sender, subject, headers, text and the
// signals (IsContact and the rest) the daemon fills in.
type Summary = message.Summary

// ParseEmail reads a raw RFC 5322 email the way the daemon reads a fetched one, for
// accountID, its text cut to bodyChars characters (0 = all of it). The contact signals
// and ReceivedAt are left for the caller. It is for fakes of Mail in tests.
func ParseEmail(raw []byte, accountID int64, bodyChars int) (*Summary, error) {
	header, text := raw, []byte(nil)
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		header, text = raw[:i+4], raw[i+4:]
	}
	return message.Parse(&message.Raw{Header: header, Text: text}, accountID, bodyChars)
}

// Generator is a generative model asked for structured output: Generate sends system and
// user with schema, the JSON schema of the answer, and decodes the answer into out. An
// answer that is not that JSON fails with an error that wraps ErrBadOutput; one that was
// paid for still reports its Usage.
type Generator interface {
	Generate(ctx context.Context, system, user string, schema json.RawMessage, out any) (Usage, error)
}

// Usage is what one model call cost: provider, model, tokens and dollars.
type Usage = models.Usage

// WithPurpose says why the model calls made under ctx are made (the usage ledger's
// purpose, such as "suggest"), which the daemon's debug line of every call repeats.
func WithPurpose(ctx context.Context, purpose string) context.Context {
	return models.WithPurpose(ctx, purpose)
}

var (
	// ErrBadOutput is a model that answered, but not with the JSON asked for.
	ErrBadOutput = models.ErrBadOutput
	// ErrModel marks a failure of the generative model: it could not be reached, or its
	// answer could not be used. Host.Fail answers an error that wraps it with 502
	// model_error.
	ErrModel = composer.ErrModel
)

// ComposerFromEnv builds the rule composer model the environment names
// (MAILRULES_COMPOSER_MODEL and its provider's key or URL, as the daemon reads them),
// with no database, and returns it with its name. It is for evals that call a real model.
func ComposerFromEnv(getenv func(string) string) (Generator, string, error) {
	cfg, err := config.Load(nil, getenv)
	if err != nil {
		return nil, "", err
	}
	if err := cfg.ComposerReady(); err != nil {
		return nil, "", err
	}
	gen, err := models.NewGenerator(cfg, cfg.ComposerModel, models.Deps{Caller: models.NewCaller(2), Prices: models.DefaultPrices()})
	if err != nil {
		return nil, "", err
	}
	return gen, cfg.ComposerModel, nil
}

// Rule is a rule as the daemon stores and runs it; Cond is a condition tree and Action
// one of its actions.
type (
	Rule   = rules.Rule
	Cond   = rules.Cond
	Action = rules.Action
)

// Action types a module names.
const (
	ActMove    = rules.ActMove
	ActArchive = rules.ActArchive
	ActTrash   = rules.ActTrash
)

// Draft is a proposed rule as a card to review: the contract's RuleDraft. Problem is one
// thing wrong with it; Row is one sample email it would take, the contract's TestRow.
type (
	Draft   = composer.Draft
	Problem = composer.Problem
	Row     = composer.Row
)

// Stages a Row gives for a sample email: taken by the rule's conditions, or put there with
// no rule deciding it.
const (
	StageCondition = string(rules.StageCondition)
	StageNone      = string(rules.StageNone)
)

// CardSamples is how many sample emails a draft card shows.
const CardSamples = composer.MaxSamples

// ReadDraft reads one rule an AI wrote, raw as the composer's rule schema has it, into a
// card validated as a composed draft is, with no words of the owner's (Said stays empty).
// It never fails: what cannot be read is left out and what is wrong goes on the card.
// existing are the user's rules, folders the folder names that exist; named lists the
// folders the AI proposed to create, so a move to one is allowed and listed in NewFolders.
func ReadDraft(raw json.RawMessage, named string, existing []Rule, folders []string) Draft {
	return composer.ReadDraft(raw, named, existing, folders)
}

// Matches reports whether rule, run on its own with no model, takes email at now: it is
// enabled, for the email's account or every account, its conditions match and its
// exceptions do not. A rule by meaning (an Intent) needs a model, so it never matches here.
func Matches(rule Rule, email *Summary, now time.Time) bool {
	rule.ID = -1 // 0 is "no rule" in an evaluation
	ev := rules.Evaluate(*email, []Rule{rule}, nil, rules.Options{Now: now})
	return ev.Final != nil && ev.Final.RuleID == rule.ID
}

// RuleGrammar is how a rule's conditions and actions are written, for a prompt: the text
// the rule composer is given, from the one list of fields and action types the daemon
// accepts.
func RuleGrammar() string { return composer.RuleGrammar() }

// RuleLine is how a rule is shown to a model: the fields it may reason about, as JSON.
func RuleLine(r Rule) string { return composer.RuleLine(r) }

// CondSchema is a condition tree in an output schema, as the composer asks for one.
func CondSchema() map[string]any { return composer.CondSchema() }

// ActionsSchema is a rule's list of actions in an output schema, as the composer asks for
// one.
func ActionsSchema() map[string]any { return composer.ActionsSchema() }
