// Package ext is the plug-in point of the MailRules daemon: the one public package through
// which a module built outside this repository adds a feature. A module registers API
// routes, served behind the same sign-in and CSRF checks as the built-in ones, and a
// capability the web app reads in GET /api/settings `features`. ext/daemon runs the daemon
// with the modules a build compiles in.
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
	"net/http"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// Module is one feature compiled into the daemon from outside this repository.
type Module struct {
	// Name is the capability the module provides. GET /api/settings reports
	// features.<Name> as true while the module is in the build, and the web app shows the
	// feature's screen only then.
	Name string
	// Routes returns the module's API endpoints, given what the daemon offers them. Each
	// one is served only to a signed-in user, with the CSRF check of every other route.
	Routes func(h Host) []Route
}

// Route is one API endpoint of a module, as http.ServeMux patterns it: a method and a
// path such as "/api/rules/suggest".
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}

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
	// leaves out: INBOX when no folder is named, and at most the newest 2,000 emails. When
	// it returns false it has answered 400 invalid_input.
	Scope(w http.ResponseWriter, r *http.Request, in ScopeInput) (Scope, bool)
	// Mail is the connection to an account, read-only. It fails while the account is not
	// connected; answer that with Fail.
	Mail(accountID int64) (Mail, error)
	// Folders lists the names of the account's folders, sorted.
	Folders(ctx context.Context, accountID int64) ([]string, error)
	// Rules lists the user's rules in priority order.
	Rules(ctx context.Context, userID int64) ([]Rule, error)

	// Composer is the rule composer model chosen in Settings. Its error, when none is set
	// up, is answered with Fail (409 no_composer_model).
	Composer(ctx context.Context) (Generator, error)
	// RecordUsage books one model call on the usage ledger under purpose, one of the
	// ledger's purposes in api/openapi.yaml. A failure to book it is logged, not returned.
	RecordUsage(ctx context.Context, purpose string, u Usage)
	// UsageChanged tells the open screens that the usage ledger changed.
	UsageChanged()
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
