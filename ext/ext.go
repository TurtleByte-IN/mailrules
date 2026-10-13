// Package ext is the plug-in point of the MailRules daemon: the one public package through
// which a module built outside this repository adds a feature. A module registers API
// routes, served behind the same sign-in and CSRF checks as the built-in ones unless it
// marks one Public (served before sign-in) or Webhook (a call from another server), a
// capability the web app reads in GET /api/settings `features`, and optionally the way
// people sign in (Module.SignIn) and are deleted at the sign-in service when they delete
// their own account (Module.DeleteIdentity), the way mailboxes of some providers are
// connected with one-click sign-in (Module.Mailboxes), sign-in forms for mailboxes of
// providers the module connects itself (Module.MailboxForms), what the module is told when a
// mailbox it connected is removed or forgotten (Module.MailboxRemoved), the delivery of
// rule notifications (Module.Notifications), the writing of the draft replies a rule asks
// for (Module.Drafts), what the module is told when the person moves an email MailRules
// filed, with a question it may ask in Activity (Module.Moves),
// the plans the build sells: what each tenant may use and pays for (Module.Billing), the
// invitations and member removals of a team at the sign-in service (Module.Team), the key
// service that holds the key every tenant's data key is wrapped under (Module.Keys), and
// every error the daemon logs, for an error tracker (Module.Errors).
// ext/daemon runs the daemon with the modules a build compiles in.
//
// This package is a contract. What it exports is all a module may use of the daemon: the
// mail it may read (read-only, with BODY.PEEK), the models it may call, the rules and
// folders it may read, the notices of what rules did, the emails a rule wants answered, and
// the helpers that answer HTTP requests the way the built-in routes do. A change to it means
// the modules follow.
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
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/moved"
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
	// MailboxRemoved, when set, is told of every mailbox the module connected (one-click,
	// for one of its Mailboxes providers, or through one of its MailboxForms) whose secret
	// the daemon let go of: the person removed the mailbox, or its owner was forgotten
	// (Host.ForgetIdentity, Delete my account), which deletes the secret at once. The module
	// undoes there what it holds for the mailbox outside the daemon, such as revoking the
	// refresh token or removing the account from the Bridge it runs. Nothing waits for it:
	// the removal is written, in the same transaction, with what to tell, so a restart
	// loses none, and one worker hands each RemovedMailbox over within a few seconds, one at
	// a time, with a ctx that ends after 30 seconds, and again (the same RemovedMailbox.ID)
	// until it returns nil: every one reaches the module at least once. nil means the module
	// has it: the daemon deletes its copy of the secret, and undoing, retries included, is
	// the module's job. An error is logged and the mailbox handed over again, after a few
	// seconds at first and at most an hour apart, until RemovedMailboxDays have passed;
	// then it is dropped with its secret. One is told only once per secret: a mailbox that
	// is removed after its owner was forgotten, with no new secret since, is not told
	// again. Errors must not contain the secret. It needs Mailboxes or MailboxForms.
	MailboxRemoved func(ctx context.Context, m RemovedMailbox) error
	// Notifications, when set, delivers rule notifications. The daemon then offers Settings'
	// Notifications and webhooks, where a team adds destinations of the module's Kinds, and
	// a rule's More options, where a rule chooses the destinations to tell; it stores the
	// destinations, their addresses and secrets encrypted, and for every email a rule with
	// destinations acts on live it writes one notice per destination and hands each to
	// Notify. At most one module of a build sets it.
	Notifications *Notifications
	// Drafts, when set, writes draft replies. The daemon then offers "Draft a reply" on a
	// rule (GET /api/settings reports features.draft_replies, in a build that sells plans
	// only to tenants whose plan lists "draft_replies"), and for every email such a rule acts
	// on live it hands the email to Write and saves the reply in the mailbox's Drafts folder,
	// never sending it. At most one module of a build sets it.
	Drafts *Drafts
	// Moves, when set, tells the module when the person moves an email MailRules filed out
	// of the folder MailRules put it in, in another mail app, and lets it ask in Activity
	// whether to learn the sender. The daemon then watches the mailboxes for such moves
	// (none without it) and GET /api/settings reports features.move_questions. At most one
	// module of a build sets it.
	Moves *Moves
	// Billing, when set, makes the build sell plans (see Billing): the daemon keeps each
	// tenant's plan, which the module sets (Host.SetPlan), limits the tenant to its plan's
	// terms, offers the plan's features only to it, shows Plan and billing (GET
	// /api/settings reports features.billing) and, once a trial or plan ends, stops sorting,
	// keeps the app read-only and deletes the tenant's data on schedule. Without it nothing
	// is limited and every feature of the build is everyone's, as on self-host. At most one
	// module of a build sets it.
	Billing *Billing
	// Team, when set, lets a tenant's owners and admins invite people by email and manage
	// its members on the Members screen (GET /api/settings reports features.team; a plan
	// that lists "team" in its Features offers it to its tenants only). The daemon keeps the
	// members, their roles (RoleOwner, RoleAdmin, RoleMember: enforced on every route, in
	// every build) and the invitations, counts the seats (a member still in the team or an
	// invitation waiting), has the billing module add one for an invitation past the paid
	// seats of a plan sold by the seat and give back those freed (Billing.ChangePlan),
	// refuses an invitation past any other plan's seats (Limits.Seats) and one to an address
	// that has a MailRules account of its own, and records every change in the tenant's
	// audit log; the module sends and revokes the invitations at the sign-in service and
	// removes people from the tenant's organisation there (see Team). It needs a module with
	// SignIn. At most one module of a build sets it.
	Team *Team
	// Keys, when set, holds the key-encryption key in place of the master key (see
	// KeyProvider): every tenant's data key, which seals its mailbox passwords, tokens,
	// model keys and notification addresses, is wrapped and unwrapped by it. Without it the
	// master key does, as on self-host. At most one module of a build sets it, and the build
	// must register that module for every command, not only serve: `mailrules accounts`
	// opens and seals secrets too.
	//
	// The first start with it rewraps, under it, each tenant key the master key wrapped and
	// reseals the removed mailboxes' copies under it, each once; the master key
	// (MAILRULES_MASTER_KEY or MAILRULES_MASTER_KEY_FILE) must stay set until a start logs
	// that nothing needs it any more. A tenant key it wrapped never falls back to the master
	// key. An unwrapped key is kept in memory for MAILRULES_TENANT_KEY_CACHE, then asked for
	// again; requests that need the same key at once share one call.
	Keys KeyProvider
	// Errors, when set, is handed every error `serve` logs (level error, recovered panics
	// included), from its start to its end, to send to an error tracker. It sees exactly
	// the line the log writes, redaction included (see ErrorEvent), never more. Each module
	// that sets it is handed every one.
	//
	// Nothing waits for it: it is called on a goroutine of its own, one error at a time, in
	// the order they were logged, with a ctx that ends a few seconds after the daemon begins
	// to stop. Up to ErrorQueue errors wait for it while it runs; when one more is logged,
	// the oldest waiting is dropped, counted in the metric
	// mailrules_error_events_dropped_total{module} and in the next ErrorEvent.Dropped. It
	// must not log at error level itself (log a failure to send at warn): what it logs at
	// error comes back to it.
	Errors func(ctx context.Context, e ErrorEvent)
}

// ErrorQueue is how many logged errors wait for a module's Errors before the oldest is
// dropped.
const ErrorQueue = 256

// ErrorEvent is an error the daemon logged (Module.Errors), as its log line reads: the
// keys password, secret, token, key, authorization and body are dropped, at any depth, and
// email content is never logged. An email address is there only where the log line has it.
type ErrorEvent struct {
	Time    time.Time
	Message string
	// Attrs are the line's attributes but time, level, msg and stack, as JSON decodes them
	// with numbers kept as json.Number: a group is a map[string]any. They include, where
	// the error is known to belong to one:
	//
	//	tenant_id  the tenant
	//	account    the mailbox's id
	//	route      the request's route, such as "GET /api/rules/{id}" ("unmatched" when no
	//	           route matched)
	//	method     the request's method, on a panic serving it
	//	panic      the value the code panicked with, as fmt.Sprint prints it
	Attrs map[string]any
	// Stack is the stack of the goroutine that panicked, as runtime/debug.Stack writes it,
	// for a recovered panic; empty for any other error.
	Stack string
	// Version is the build's version, as `mailrules version` prints it: an error tracker's
	// release.
	Version string
	// Dropped is how many errors logged just before this one were dropped unhanded: too
	// many were waiting (ErrorQueue).
	Dropped int
}

// KeyProvider is a key-encryption key held outside the daemon, in a key service such as a
// cloud KMS with HSM-backed keys (Module.Keys). The daemon gives it 32-byte data keys to
// wrap and asks it to unwrap them; it never sees the key-encryption key itself. Its methods
// are called at once from many goroutines, with the ctx of the request or job that needs
// the key. No error, and nothing it logs, may contain a key, wrapped or not.
type KeyProvider interface {
	// KeyID names the key-encryption key new keys are wrapped under, as the daemon stores
	// it beside each wrapped key and hands it back to Unwrap: for a cloud KMS, the key's
	// resource name, whose versions the service picks and records in each ciphertext
	// itself. It must not change while the key does not: keys stored under another KeyID
	// are rewrapped at the next start. Not empty, at most 255 bytes, and not "master",
	// which is the master key's.
	KeyID() string
	// Wrap encrypts key for tenantID, binding tenantID into the ciphertext (a KMS's
	// additional authenticated data), so it unwraps for that tenant only. Tenant 0 is a
	// removed mailbox's copy of its secret, which outlives its tenant. Any error means the
	// service could not be asked: the daemon stores nothing (a new tenant's sign-in fails)
	// and answers 503 key_unavailable.
	Wrap(ctx context.Context, tenantID int64, key []byte) ([]byte, error)
	// Unwrap decrypts what Wrap returned for tenantID under keyID. It returns ErrWrongKey
	// when keyID is not a key it holds, or the ciphertext was not wrapped by it for that
	// tenant: the secrets under it are then as good as lost. Any other error (the service
	// does not answer, refuses the call, a quota) is taken as the service being
	// unreachable: the tenant's secrets open again once it answers, and until then its
	// mailboxes wait in key_unavailable and requests that need a secret answer 503.
	Unwrap(ctx context.Context, tenantID int64, keyID string, wrapped []byte) ([]byte, error)
}

var (
	// ErrWrongKey is what KeyProvider.Unwrap returns for a wrapped key it does not open.
	ErrWrongKey = crypto.ErrDecrypt
	// ErrKeyUnavailable is what Host methods that seal or open a secret (Host.SignIn for a
	// new tenant, Host.AddMailbox, Host.ForgetIdentity and the like) return while the
	// build's KeyProvider cannot be reached. Nothing changed; try again later.
	ErrKeyUnavailable = crypto.ErrKeyUnavailable
)

var _ crypto.KeyProvider = KeyProvider(nil)

// NoLimit is a limit that does not limit (Limits.Mailboxes, Limits.AIEmails, Limits.Seats).
const NoLimit = store.NoLimit

// Limits is how much of the hosted service a tenant on a plan may use (PlanTerms.Limits). A
// zero field allows none; NoLimit allows any number. For a plan sold by the seat
// (PlanTerms.PerSeat) they are one seat's: the tenant gets them times its seats.
type Limits struct {
	// Mailboxes is how many mailboxes the tenant may have connected at once, its members'
	// private ones included. A mailbox over it is refused on every path that adds one: the
	// add-mailbox wizard (409 mailbox_limit) and Host.AddMailbox (ErrMailboxLimit). The
	// mailboxes it has already are left alone when the number goes down, but the tenant
	// cannot move to a plan that allows fewer than it has (409 downgrade_refused).
	Mailboxes int
	// AIEmails is how many emails a month (a calendar month in UTC) a model may decide for
	// the tenant on keys the operator holds (MAILRULES_MODE=cloud, the tenant has no key of
	// its own for the provider). Emails a model decides on the tenant's own keys are not
	// counted, nor are those sender rules and rules without an intent decide. An email is
	// counted once when live sorting decides it, however often it is tried again, and once
	// per cleanup check that asks a model about it. Once the month's are used up, sender
	// rules and rules without an intent keep sorting, a decision model on the tenant's own
	// key decides without the operator's fallback, and mail that needs the operator's model
	// waits in Needs review, which says why with a way to the person's own key and to Plan
	// and billing. Nothing is charged for going over. A plan sold by the seat pools it: the
	// whole team draws on AIEmails times its seats.
	AIEmails int
	// FirstCleanupEmails is a one-time grant of emails the tenant's cleanup checks may have a
	// model decide on keys the operator holds before they draw on AIEmails, so a first
	// cleanup of old mail does not use up the month. Each such email of a check takes one of
	// it while any is left, then counts against AIEmails as before. What was used is kept for
	// good: it never resets, and a plan with a larger one gives the difference. Live sorting
	// never draws on it. 0 = none.
	FirstCleanupEmails int
	// Seats is how many people the tenant may have, its members and the invitations waiting
	// together, for a plan not sold by the seat; 0 is read as 1 (the person who made the
	// tenant). An invitation or a new member over it is refused (409 seat_limit, and a
	// sign-in under the team's organisation without an invitation with ErrSeatLimit). The
	// members a tenant has are left alone when the number goes down, but it cannot move to
	// a plan with fewer seats than it has members (409 downgrade_refused). A plan sold by the
	// seat ignores it: the tenant has the seats it bought (Plan.Seats), which an invitation
	// past them raises at once and a seat freed lowers (Billing.ChangePlan); a sign-in
	// without an invitation past them is still refused with ErrSeatLimit.
	Seats int
}

// The roles a person has in their tenant (Who.Role, Route.Role, Invitation.Role), from the
// most they may do to the least. The daemon enforces them on its own routes and on a
// module's routes that ask for one (Route.Role).
const (
	// RoleOwner: the plan and billing, members, and everything an admin does.
	RoleOwner = store.RoleOwner
	// RoleAdmin: mailboxes, the shared rules and sender rules, members, settings, the audit
	// log.
	RoleAdmin = store.RoleAdmin
	// RoleMember: their own mailboxes, sorted by the shared rules.
	RoleMember = store.RoleMember
)

// The plans a tenant may be on (PlanState.Plan, Plan.Plan, Billing.Plans).
const (
	// PlanTrial: no plan bought yet, within TrialDays of the tenant's creation. No billing
	// service is involved and no card is asked for.
	PlanTrial    = store.PlanTrial
	PlanPersonal = store.PlanPersonal
	PlanPro      = store.PlanPro
	// PlanTeam is sold by the seat (PlanTerms.PerSeat, Plan.Seats). It is on sale when
	// Billing.Plans has it.
	PlanTeam = store.PlanTeam
	// PlanEnded: the trial ran out with no plan bought, or a paid plan ended. Nothing is
	// sorted, the app is read-only with a way to subscribe, and the tenant's mailbox
	// passwords, tokens and model keys are deleted EndedSecretsDays later, everything else
	// EndedRemovalDays later. Buying a plan before then gives it all back (mailboxes need
	// their passwords again once they were deleted).
	PlanEnded = store.PlanEnded
)

// The billing intervals (Plan.Interval, CheckoutRequest.Interval).
const (
	IntervalMonth = store.IntervalMonth
	IntervalYear  = store.IntervalYear
)

// The statuses of a paid plan (Plan.Status).
const (
	StatusActive = store.StatusActive
	// StatusPastDue: a renewal is unpaid and the billing service retries it; the plan runs.
	StatusPastDue = store.StatusPastDue
	// StatusCancelsAtPeriodEnd: cancelled; the plan runs to Plan.PeriodEnd and ends there.
	StatusCancelsAtPeriodEnd = store.StatusCancelsAtPeriodEnd
)

const (
	// TrialDays is how long a tenant's trial runs from its creation.
	TrialDays = store.TrialDays
	// EndedSecretsDays is how many days after a plan ends the tenant's mailbox passwords and
	// tokens and its model keys are deleted (modules are told of each mailbox they connected,
	// Module.MailboxRemoved).
	EndedSecretsDays = store.EndedSecretsDays
	// EndedRemovalDays is how many days after a plan ends everything else is removed.
	EndedRemovalDays = store.EndedRemovalDays
)

// Billing is how a module sells plans (Module.Billing). The daemon decides what a plan
// allows from Plans and keeps each tenant's plan; the module talks to the billing service:
// it opens checkout, changes a subscription, links to the service's customer portal and,
// from its webhook route, reports every change with Host.SetPlan.
type Billing struct {
	// Plans are the terms of each plan: PlanTrial, PlanPersonal and PlanPro, each needed,
	// and PlanTeam, which is on sale only when it is here, with PerSeat set. A tenant whose
	// plan has ended is held to none of them: it sorts nothing.
	Plans map[string]PlanTerms
	// Checkout returns what the browser hands to the module's checkout script (Script) to
	// let the signed-in person buy plan, billed every interval, for their tenant: opaque to
	// the daemon, such as the billing service's public client token, the price and the
	// tenant reference the service passes back to the webhook. It must not hold a secret.
	// The daemon calls it only for a tenant that has not subscribed (its plan has no Record)
	// and that has no more mailboxes than plan allows.
	Checkout func(ctx context.Context, c CheckoutRequest) (json.RawMessage, error)
	// ChangePlan moves a tenant that subscribed (its plan has a Record) to c.Plan and
	// c.Interval, with c.Seats seats of a plan sold by the seat, at once, at the billing
	// service: an upgrade charged for what is left of the period, a downgrade credited on
	// the next bill. The daemon refuses a move to a plan that allows fewer mailboxes, or
	// fewer seats, than the tenant has before it calls this. The new plan is what the module
	// then reports with Host.SetPlan; ChangePlan's success alone changes nothing.
	//
	// For a plan sold by the seat the daemon also calls it on its own, with the tenant's
	// plan and interval and only c.Seats changed (MAI-247): before it keeps an invitation
	// past the paid seats, for the seats the team then needs (an error refuses the
	// invitation), and after a seat is freed, for the seats the team uses, never below
	// MinSeats, again until it succeeds. It may ask for the number of seats the
	// subscription has already, for one not yet reported with Host.SetPlan: that must
	// succeed and change nothing.
	ChangePlan func(ctx context.Context, c PlanChange) error
	// Portal returns the address of the billing service's customer portal for a tenant that
	// subscribed, where its people cancel, change the payment method and get invoices
	// (Manage billing).
	Portal func(ctx context.Context, tenantID int64, record string) (url string, err error)
	// Script is the path of the module's Public GET route that serves the checkout script.
	// Plan and billing loads it (same origin) and calls what it defines:
	// window.mailrulesCheckout.open(checkout, done), where checkout is what Checkout
	// returned and done(result) is to be called once with "completed" or "closed". The
	// tenant's plan then changes when the module calls Host.SetPlan.
	Script string
	// Sources are the origins outside the daemon the checkout script loads from, which this
	// build's Content-Security-Policy allows and no other build's does. Each is an https
	// origin, optionally with a *. wildcard for its subdomains (https://*.example.com).
	Sources CheckoutSources
	// Note is a sentence Plan and billing shows under the plan, such as who bills and how
	// taxes are shown. Empty = none.
	Note string
}

// CheckoutSources are the origins a checkout script uses (Billing.Sources).
type CheckoutSources struct {
	Scripts []string // script-src
	Frames  []string // frame-src: a checkout shown in an overlay
	Connect []string // connect-src: what the script calls
	Styles  []string // style-src: style sheets it loads
	Images  []string // img-src
}

// PlanTerms are what one plan gives and how Plan and billing shows it (Billing.Plans).
type PlanTerms struct {
	Name string // as shown: "Personal"
	// Prices are what the plan costs, as shown, by interval: {"month": "$5 a month",
	// "year": "$50 a year"}; for a plan sold by the seat, one seat's: "$8 a seat a month".
	// Empty for the trial.
	Prices map[string]string
	// Includes are the lines the plan's card lists ("3 mailboxes", "3,000 AI-sorted emails a
	// month").
	Includes []string
	Limits   Limits
	// Features are the build's features only tenants on this plan get, named as GET
	// /api/settings reports them ("suggest", "notifications", "team"). A feature some plan
	// lists is offered to a tenant only while its plan lists it: the screens hide it, its
	// routes answer 403 plan_required, and for notifications no notice is written. A feature
	// no plan lists is everyone's.
	Features []string
	// PerSeat sells the plan by the seat (PlanTeam): the tenant buys a number of seats, at
	// least MinSeats, one per member, and its Limits are one seat's, multiplied by its seats.
	PerSeat bool
	// MinSeats is the fewest seats of a plan sold by the seat that may be bought; 0 is read
	// as 1.
	MinSeats int
}

// CheckoutRequest is who is buying what (Billing.Checkout).
type CheckoutRequest struct {
	TenantID int64
	Email    string // the signed-in person's, to fill in at checkout
	Plan     string // PlanPersonal, PlanPro or PlanTeam
	Interval string // IntervalMonth or IntervalYear
	Seats    int    // for a plan sold by the seat, how many; 0 for any other
}

// PlanChange is a subscribed tenant's move to another plan, interval or number of seats
// (Billing.ChangePlan).
type PlanChange struct {
	TenantID int64
	Record   string // the tenant's Plan.Record
	Plan     string // PlanPersonal, PlanPro or PlanTeam
	Interval string // IntervalMonth or IntervalYear
	Seats    int    // for a plan sold by the seat, how many; 0 for any other
}

// Plan is a tenant's plan as the module reports it (Host.SetPlan).
type Plan struct {
	Plan     string // PlanPersonal, PlanPro, PlanTeam or PlanEnded
	Interval string // IntervalMonth or IntervalYear; empty for PlanEnded
	Status   string // StatusActive, StatusPastDue or StatusCancelsAtPeriodEnd; empty for PlanEnded
	// PeriodEnd is when the paid period ends (the plan renews or, cancelled, ends); zero
	// when not known.
	PeriodEnd time.Time
	// Record is the module's own record of the tenant at the billing service, such as its
	// customer and subscription ids: kept as it is, handed back to Checkout, ChangePlan and
	// Portal, never shown.
	Record string
	// Seats is how many seats the tenant bought of a plan sold by the seat (PlanTeam), at
	// least 1; 0 for any other plan.
	Seats int
}

// PlanState is a tenant's plan as the daemon has it (Host.Plan).
type PlanState struct {
	Plan
	// TrialEnd is when the tenant's trial ends or ended: its creation plus TrialDays.
	TrialEnd time.Time
	// EndedAt is when the plan ended (Plan.Plan is PlanEnded); zero while one runs.
	EndedAt time.Time
}

// Who is the signed-in person a request is for (Host.Who).
type Who struct {
	UserID, TenantID int64
	Email            string
	Role             string // RoleOwner, RoleAdmin or RoleMember
}

// Team is how a module invites people to a tenant and removes them from it at the sign-in
// service (Module.Team). Owners and admins invite on the Members screen; the daemon stores
// each invitation, holding a seat (bought first, past the paid seats of a plan sold by the
// seat: Billing.ChangePlan), before it calls Invite. It never calls Invite for an address
// that has a MailRules account of its own, in any tenant (409 has_account): the person
// would belong to two organisations. Once the person accepts at the
// service, which adds them to the tenant's organisation, the module tells the daemon
// (Host.AcceptInvitation); their first sign-in under that organisation (Host.SignIn) joins
// them to the tenant with the invitation's role. Someone the service lets into the
// organisation without an invitation (company sign-in) joins as a member while a seat is
// free. An invitation not accepted within InvitationDays expires and frees its seat.
type Team struct {
	// Invite has the service send inv, and returns the module's reference to it, kept as
	// Invitation.Record and handed back to Revoke. When it fails the daemon withdraws the
	// invitation, freeing its seat, and answers 502 invite_failed.
	Invite func(ctx context.Context, inv Invitation) (record string, err error)
	// Revoke takes back at the service an invitation an owner or admin revoked; the daemon
	// has deleted it already. An error is logged, not shown.
	Revoke func(ctx context.Context, inv Invitation) error
	// RemoveMember removes the person from the tenant's organisation at the service (id.Tenant
	// is the organisation's id there), so they cannot sign in to the team again. The daemon
	// calls it before it removes them; when it fails, nothing is removed (502
	// sign_in_error). On success the daemon removes them as Host.ForgetIdentity does: their
	// sessions end, their mailboxes go with them, the team keeps the rest.
	RemoveMember func(ctx context.Context, tenantID int64, id Identity) error
}

// Invitation is one invitation to a tenant (Team.Invite, Team.Revoke).
type Invitation struct {
	ID       int64
	TenantID int64
	// Organisation is the sign-in service's id of the tenant's organisation (the
	// Identity.Tenant its members sign in under).
	Organisation string
	Email        string
	Role         string // RoleOwner, RoleAdmin or RoleMember
	InvitedBy    string // the inviter's email
	ExpiresAt    time.Time
	Record       string // what Invite returned; empty when Invite is called
}

// InvitationDays is how long an invitation waits to be accepted, and, once accepted, for the
// person's first sign-in.
const InvitationDays = store.InvitationDays

// MailboxSignIn is how a module connects mailboxes with one-click sign-in (Module.Mailboxes).
// The module holds no connection, and the daemon knows nothing of OAuth: it stores the
// secret the module gives it (Host.AddMailbox, Host.ReconnectMailbox), encrypted like an app
// password, never returns it in the API and never logs it, and hands it back to Login just
// before each login.
type MailboxSignIn struct {
	// Providers are the presets whose mailboxes the module signs in to: "gmail", "outlook"
	// or both. GET /api/presets gives each its one-click URL, and lists the Outlook preset
	// only when it is here: Outlook takes no app password, so a build without the module
	// has no Outlook tile. The wizard's Gmail tile is the app-password one in every build:
	// one-click Google sign-in is only for Google Workspace organisations whose admin
	// trusts MailRules, and "gmail" here adds a separate Google Workspace tile (Google).
	Providers []string
	// Google is what the Google Workspace tile shows the person to give their admin;
	// needed with "gmail" in Providers, and only then. Connect answers a personal Google
	// account (not in a Workspace organisation) with Host.MailboxFailed and
	// MailboxWorkspaceOnly, which opens the Gmail app-password guide.
	Google *GoogleSignIn
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

// GoogleSignIn is what a Google Workspace admin needs to trust MailRules in the Google
// Admin console (MailboxSignIn.Google), shown on the wizard's Google Workspace tile.
type GoogleSignIn struct {
	// ClientID is the OAuth client ID of the module's Google sign-in, which the admin
	// adds as a trusted app. Not a secret. Required.
	ClientID string
	// AdminHelpURL is an https page that walks the admin through trusting MailRules, linked
	// from the tile; empty for no link.
	AdminHelpURL string
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

// RemovedMailbox is a mailbox a module connected whose secret the daemon let go of
// (Module.MailboxRemoved).
type RemovedMailbox struct {
	// ID is this hand-over's own: the same on every attempt, so a module that must not undo
	// twice tells them apart by it. A mailbox forgotten, connected again and then removed is
	// handed over twice, with two IDs and two secrets.
	ID int64
	// AccountID is the mailbox, as Host.AddMailbox returned it. It may no longer exist.
	AccountID int64
	Provider  string // NewMailbox.Provider: "gmail", "outlook" or "proton"
	Address   string // the mailbox's address, NewMailbox.Address
	// Secret is the secret last stored for the mailbox: NewMailbox.Secret, or the one
	// Host.ReconnectMailbox or a MailToken.Secret put in its place. Empty when the daemon
	// could not open it under the master key it runs with. Never log it.
	Secret string
}

// RemovedMailboxDays is how long the daemon keeps handing a removed mailbox over to the
// module that connected it (Module.MailboxRemoved) before it drops it with its secret.
const RemovedMailboxDays = store.RemovedMailboxDays

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
	// ErrMailboxLimit is what Host.AddMailbox returns when the signed-in user's tenant has as
	// many mailboxes as its Limits allow. The error's text is the sentence to show the
	// person, such as a form step's Message. Nothing changed.
	ErrMailboxLimit = store.ErrMailboxLimit
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
	// MailboxLimitReached: Host.AddMailbox returned ErrMailboxLimit.
	MailboxLimitReached = "mailbox_limit"
	// MailboxPlanEnded: Host.AddMailbox or Host.ReconnectMailbox returned ErrPlanEnded.
	MailboxPlanEnded = "plan_ended"
	// MailboxWorkspaceOnly: a personal Google account (not in a Google Workspace
	// organisation) signed in to the one-click Google sign-in, which takes only Workspace
	// accounts. The module keeps no token for it. The wizard says personal Gmail connects
	// with an app password and opens the Gmail app-password guide.
	MailboxWorkspaceOnly = "workspace_only"
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

// Drafts is how a module writes draft replies (Module.Drafts). The daemon decides when: a
// job for every email a rule that drafts replies acts on live, written in the transaction
// that records the rule's actions, so a restart loses none. Nothing is drafted in dry-run,
// for a cleanup, an undo or a correction, for an email a sender rule settled, or once the
// rule's actions on the email were undone. The daemon reads the email (BODY.PEEK), pays for
// the model like sorting, saves the reply and records it in Activity, where it can be
// undone; the module only writes the text.
type Drafts struct {
	// Write writes the reply to one email, as plain text: the body only, no greeting line
	// it was not asked for, no quoted original, no headers. One worker calls it, one email
	// at a time, within a few seconds of the rule acting, with a ctx that ends after 60
	// seconds, and calls it again for the same email (the same DraftRequest.ID) until it
	// returns text or DraftWait has passed since the rule acted: a module that keeps state
	// tells repeats apart by ID. An empty text, or an error that wraps a *DraftRefused,
	// writes no reply, for good, and Activity says why. Errors must not contain the email's
	// text, sender or subject. It must ask for the reply only through r.Model.
	Write func(ctx context.Context, r DraftRequest) (string, error)
}

// DraftWait is how long after a rule acted its reply is still tried for: one Write has not
// written by then is given up, and Activity says so.
const DraftWait = store.DraftWait * time.Second

// DraftRequest is one email a rule wants answered.
type DraftRequest struct {
	// ID is the request's own: the same on every hand-over of it.
	ID       int64
	TenantID int64
	RuleID   int64 // 0 once the rule was deleted
	RuleName string
	// Instruction is how the person wants the reply to sound or what it should say, as the
	// rule had it when it acted ("polite no, not looking right now"); empty = none given.
	Instruction string
	// Mailbox is the address the reply is from: the mailbox's own.
	Mailbox string
	// Email is the email to answer.
	Email DraftEmail
	// Previous is the thread's previous message, the one Email replies to, when the rule
	// allows it to be read and the mailbox still has it; nil otherwise.
	Previous *DraftEmail
	// Model is the tenant's generative model (its rule composer model in Settings), on the
	// tenant's own key or else the operator's. The daemon books every call on the tenant's
	// usage ledger under the purpose "draft" and, when the calls go out on a key the
	// operator holds, counts the reply as one AI-sorted email against the tenant's monthly
	// allowance; it does not call Write at all once that allowance is used up.
	Model Generator
}

// DraftEmail is one email as the drafting model may read it: what the decision model is
// shown, no more.
type DraftEmail struct {
	From     string // the sender's address
	FromName string // the sender's display name; empty when the email gave none
	To       []string
	Subject  string
	Date     time.Time // when the server received it
	// Text is the email's plain text, HTML converted and control characters stripped, cut
	// to MAILRULES_BODY_CHARS characters: the trimmed excerpt the decision model gets.
	Text string
}

// DraftRefused is a reply a module will not write, for good: Reason is the short sentence
// Activity shows ("The email asks for nothing a reply could answer."). Never the email's
// text, sender or subject.
type DraftRefused struct{ Reason string }

func (e *DraftRefused) Error() string { return "no draft: " + e.Reason }

// Moves is how a module learns of the emails the person moves out of the folders MailRules
// filed them in (Module.Moves). The daemon does the watching: every Every it looks, on
// each mailbox's own connection and between two emails, at what MailRules filed in the
// last MoveWatchDays (moves, archives, trashes and junks it did live and that nothing
// undid or fixed since), with one search per folder it filed into, and follows each email
// that is gone by its Message-ID. Mail MailRules moves itself (an undo, a fix, a cleanup)
// is never a move. It tells the module only of an email the person filed in the Inbox or an
// ordinary folder; one deleted, archived, marked as junk or expunged is not told. Every
// fetch is BODY.PEEK and the watching changes nothing in the mailbox.
type Moves struct {
	// Every is how often each mailbox is looked at; 0 = MoveSweepEvery. A sweep costs a few
	// IMAP commands per folder MailRules filed into, plus a search per folder for each
	// email found gone.
	Every time.Duration
	// Serves, when set, says whether the mailboxes of a tenant are watched, as each sweep
	// starts; nil = every mailbox of the build. It must be quick: it holds up the
	// mailbox's new mail while it runs.
	Serves func(ctx context.Context, tenantID int64) bool
	// Moved takes one move. One worker calls it, one move at a time, within a few seconds of
	// the daemon finding it, with a ctx that ends after 30 seconds, and calls it again for
	// the same move (the same Move.ID) until it returns nil. An error is logged and the move
	// handed over again, after a few seconds at first and at most an hour apart, until
	// MoveDays have passed. To ask the person, call q.Ask, then or later (q stays valid while
	// the daemon runs).
	Moved func(ctx context.Context, m Move, q Questions) error
}

// MoveSweepEvery is how often each mailbox is looked at when Moves.Every is 0.
const MoveSweepEvery = 10 * time.Minute

// MoveWatchDays is how long after MailRules filed an email a move of it is noticed.
const MoveWatchDays = moved.WatchDays

// MoveDays is how long the daemon keeps a move: what Moves.Moved is handed and what a
// question counts.
const MoveDays = store.MoveDays

// Move is one email the person moved out of the folder MailRules filed it in. It never
// carries the email's text or subject.
type Move struct {
	// ID is the move's own: the same on every hand-over of it, and what Questions.Ask takes.
	ID        int64
	TenantID  int64
	AccountID int64
	// Mailbox is the address of the mailbox the email is in, as Mailboxes names it.
	Mailbox string
	From    string // the sender's address
	Filed   string // the folder MailRules filed it in
	Chosen  string // the folder the person moved it to
	// Inbox: Chosen is the mailbox's Inbox (the folder MailRules watches).
	Inbox bool
	// RuleID is the rule that decided where MailRules filed it, or for an email fixed in
	// MailRules the rule it was fixed to; 0 for none (a sender rule that moves to a folder)
	// or once the rule is deleted. RuleName is its name then.
	RuleID   int64
	RuleName string
	FiledAt  time.Time // when MailRules filed it
	At       time.Time // when the daemon found it moved: the person moved it before
	// Count is how many of the sender's emails the person has moved to Chosen (the Inbox
	// of any mailbox, or a folder of that name) in the last MoveDays, this one included.
	Count int
}

// Questions is where a module asks the person about their moves, in Activity.
type Questions interface {
	// Ask shows, at the top of Activity for the people who see the move's mailbox, the one
	// question about the move's sender and destination: "You moved 2 emails from this
	// sender to Inbox. Always keep them in Inbox?" (or "Always move them to <folder>?"),
	// with Yes and No, counting every move of that sender there the daemon keeps. Asking
	// again while it waits brings its count up to date. Yes stores the sender rule a fix's
	// "Always do this for <sender>" stores: the sender's mail stays in the Inbox, or goes to
	// the folder, on every mailbox of the team. No stores nothing. Once answered, either
	// way, that sender and destination is never asked about again: Ask returns ErrAnswered.
	// A move the daemon no longer keeps is ErrNoMove.
	Ask(ctx context.Context, moveID int64) error
}

var (
	// ErrAnswered is what Questions.Ask returns when the person already answered the
	// question about that sender and destination. Nothing is shown.
	ErrAnswered = store.ErrAnswered
	// ErrNoMove is what Questions.Ask returns for a move the daemon no longer keeps: older
	// than MoveDays, or its mailbox was removed.
	ErrNoMove = errors.New("no such move is kept")
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
	// Role is the least role a signed-in person needs to be served the route (RoleAdmin,
	// RoleOwner); anyone else is answered 403 role_required. Empty serves every member. A
	// route that changes the shared rules, settings or members sets RoleAdmin.
	Role string
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
	// ErrSeatLimit is what Host.SignIn returns for a new person the sign-in service let into
	// a team's organisation without an invitation while every seat of the team's plan is
	// taken (errors.Is). Nothing changed.
	ErrSeatLimit = store.ErrSeatLimit
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
	// SignInSeatsFull: Host.SignIn returned ErrSeatLimit.
	SignInSeatsFull = "seats_full"
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
	// email to id.Email, and starts the browser's session as a password sign-in does. The
	// person who makes a tenant is its owner; one who joins a tenant that exists gets the
	// role of the invitation waiting for their email (Team), or joins as a member. It
	// fails, changing nothing, with ErrTenantMismatch, ErrEmailInUse or ErrSeatLimit
	// (answer those with SignInFailed and the matching code), or with an error of the
	// database.
	SignIn(w http.ResponseWriter, r *http.Request, id Identity) (userID int64, err error)
	// EndSessions signs the identity out of every browser at once. An identity nobody has
	// is not an error.
	EndSessions(ctx context.Context, provider, subject string) error

	// ForgetIdentity is for a person the sign-in service deleted; the daemon does the same
	// for a person who deletes their own account, except that they never get their data
	// back. In one step that changes all or nothing, it ends their sessions as EndSessions
	// does, frees their email (a new identity with that email signs up at once and gets a
	// new user, not their data) and deletes their secrets: their mailboxes are stopped
	// first, then their passwords and tokens are deleted (the module that connected one is
	// told: Module.MailboxRemoved), with, when they are their tenant's only member, the
	// tenant's stored model API keys and the addresses and secrets of its notification
	// destinations.
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
	// storing nothing, with ErrMailboxExists, with ErrMailboxLimit when the tenant has all
	// the mailboxes its Limits allow (checked before the login and again as it stores the
	// mailbox, so two added at once cannot both take the last place), with an error that
	// wraps ErrReconnect when the provider refused the secret or the token, or with an error
	// of the mail server or the database (answer those with MailboxFailed and the matching
	// code, or a form step's Message).
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

	// Who is the signed-in person the request is for: their user, tenant, email and role.
	Who(r *http.Request) Who
	// Plan returns the tenant's plan as the daemon has it now (Module.Billing): the one last
	// set with SetPlan, the trial while no plan was bought within TrialDays of the tenant's
	// creation, else PlanEnded. ErrNoTenant for a tenant that does not exist.
	Plan(ctx context.Context, tenantID int64) (PlanState, error)
	// SetPlan records the tenant's plan as the billing service reports it, such as from the
	// module's webhook route (the tenant comes from the reference Checkout gave the
	// service), and acts on it at once: a plan bought or bought again starts the tenant's
	// mailboxes and lifts read-only; one that ended stops them. Reporting the same plan
	// again changes nothing. It fails, changing nothing, with ErrNoTenant, ErrBadPlan for a
	// plan, interval or status not among the constants, or an error of the database.
	SetPlan(ctx context.Context, tenantID int64, p Plan) error

	// AcceptInvitation records that the person invited to the tenant at email accepted at
	// the sign-in service (Module.Team), such as from the module's webhook route. Their
	// first sign-in under the tenant's organisation then joins them with the invitation's
	// role, which the daemon keeps (the module cannot change it); one who signed in already
	// gets that role now. It fails, changing nothing, with ErrNoInvitation when the tenant
	// has no invitation for the address that has not expired (a tenant that does not exist
	// has none), or an error of the database.
	AcceptInvitation(ctx context.Context, tenantID int64, email string) error
}

var (
	// ErrNoTenant is what Host.Plan and Host.SetPlan return for a tenant that does not exist.
	ErrNoTenant = errors.New("no such tenant")
	// ErrBadPlan is what Host.SetPlan returns for a plan, interval or status it does not know.
	ErrBadPlan = store.ErrBadPlan
	// ErrPlanEnded is what Host.AddMailbox and Host.ReconnectMailbox return while the
	// signed-in user's tenant's plan has ended: nothing is connected until a plan is bought.
	// Its text is the sentence to show. Nothing changed.
	ErrPlanEnded error = planEnded{}
	// ErrNoInvitation is what Host.AcceptInvitation returns for an address the tenant has
	// no invitation for: revoked, expired, or never sent.
	ErrNoInvitation = errors.New("no such invitation")
)

type planEnded struct{}

func (planEnded) Error() string {
	return "Your plan has ended, so MailRules sorts nothing and nothing can be changed. Subscribe on Plan and billing to sort again."
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
