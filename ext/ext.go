// Package ext is the plug-in point of the MailRules daemon: the one public package through
// which a module built outside this repository adds a feature. A module registers API
// routes, served behind the same sign-in and CSRF checks as the built-in ones unless it
// marks one Public (served before sign-in) or Webhook (a call from another server), a
// capability the web app reads in GET /api/settings `features`, and optionally the way
// people sign in (Module.SignIn), the way mailboxes of some providers are connected with
// one-click sign-in (Module.Mailboxes), and sign-in forms for mailboxes of providers the
// module connects itself (Module.MailboxForms). ext/daemon runs the daemon with the
// modules a build compiles in.
//
// This package is a contract. What it exports is all a module may use of the daemon: the
// mail it may read (read-only, with BODY.PEEK), the models it may call, the rules and
// folders it may read, and the helpers that answer HTTP requests the way the built-in
// routes do. A change to it means the modules follow.
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
	// password it gets back (Host.AddMailbox, NewMailbox.Password). At most one form per
	// provider in a build.
	MailboxForms []MailboxForm
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
	// it added.
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
	// step, as for any other mailbox.
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
	// ErrNoMailbox is what Host.ReconnectMailbox returns for a mailbox that is not a
	// one-click mailbox of the module's providers that the signed-in user added.
	ErrNoMailbox = errors.New("no such one-click mailbox")
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
	// ReconnectMailbox gives a one-click mailbox the signed-in user added a new secret,
	// from the module's Connect route called with account=<id>; ctx must be the
	// request's. address is the one the person just signed in to the provider as. It logs
	// in once with the new secret, then stores it and connects the mailbox again: how a
	// mailbox in reconnect_needed comes back. It fails, changing nothing, with
	// ErrNoMailbox, ErrMailboxMismatch, or as AddMailbox does.
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
