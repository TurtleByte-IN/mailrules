package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mailer"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// The end-of-plan schedule, in days from the moment a tenant's plan ended (store.PlanState
// .EndedAt). Warning 1 goes on the day; warning 2 NoticeDays before the mailbox passwords
// and tokens and the model keys go (store.EndedSecretsDays); warning 3 NoticeDays before
// everything else goes (store.EndedRemovalDays).
const (
	// NoticeDays is the least time between a warning and the deletion it announces.
	NoticeDays    = 7
	warn2Days     = store.EndedSecretsDays - NoticeDays
	warn3Days     = store.EndedRemovalDays - NoticeDays
	planEndPeriod = time.Minute
	day           = 24 * 3600
)

// PlanEnd is the end-of-plan job of a build that sells plans (ext.Module.Billing). For each
// tenant but the self-host one whose plan has ended (store.TenantPlan) it keeps its
// mailboxes stopped, emails its members the three warnings, deletes its mailbox passwords
// and tokens and its model keys after store.EndedSecretsDays, and removes the tenant after
// store.EndedRemovalDays. Each step is recorded on the tenant's plan, so it is taken once,
// across ticks and restarts; a plan bought again clears them, and stops the schedule.
//
// A warning missed (the daemon was down) is sent late only while no later one is due:
// only the latest due warning goes, so a tenant first seen 25 days after its end gets
// warning 2, not 1 then 2. And a deletion never comes sooner than NoticeDays after the
// warning that announces it was sent (warning 2 for the secrets, 3 for the removal; 3
// announces both when 2 was skipped): a tenant first seen late is warned first, and its
// deletions wait the notice the warning gives. The dates each warning states are the ones
// the job will keep to.
type PlanEnd struct {
	Store *store.Store
	// Stop stops an account's supervisor and waits until it has; stopping one that is not
	// running does nothing. Start starts one again, for a mailbox stopped as a plan was
	// bought at that moment (ext.Host.SetPlan starts them once the plan is stored, which may
	// be just before the job stopped them).
	Stop  func(accountID int64)
	Start func(store.Account)
	Hub   *events.Hub // tells the tenant's open screens its mailboxes lost their secrets; may be nil
	// Sender sends the warnings (the summary email's mail server); the daemon does not start
	// a build that sells plans without one. A warning that reaches no member is not recorded
	// and is tried again next tick, so no deletion comes before a warning was delivered.
	Sender    mailer.Sender
	PublicURL string           // where links in the warnings lead (MAILRULES_PUBLIC_URL)
	Now       func() time.Time // nil = time.Now
	Every     time.Duration    // 0 = every minute
}

func (j PlanEnd) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// Once takes, for every tenant whose plan has ended, the steps due by now, and returns how
// many it took. A tenant that fails does not keep the others from their turn; the failures
// are returned joined.
func (j PlanEnd) Once(ctx context.Context) (steps int, err error) {
	ids, err := j.Store.PlanEndTenants(ctx, j.now().Unix())
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, id := range ids {
		if id == store.SelfHostTenant { // never listed; the self-host tenant has no plan
			continue
		}
		n, err := j.tenant(ctx, id, j.now())
		steps += n
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %d: %w", id, err))
		}
	}
	return steps, errors.Join(errs...)
}

// tenant takes one tenant's steps due by now. A plan bought again meanwhile
// (store.ErrPlanNotEnded) ends it, with nothing more done.
func (j PlanEnd) tenant(ctx context.Context, tenantID int64, now time.Time) (int, error) {
	n, err := j.steps(ctx, tenantID, now)
	if errors.Is(err, store.ErrPlanNotEnded) || errors.Is(err, store.ErrNotFound) {
		return n, nil
	}
	return n, err
}

func (j PlanEnd) steps(ctx context.Context, tenantID int64, now time.Time) (int, error) {
	at := now.Unix()
	p, err := j.Store.EndedPlan(ctx, tenantID, at)
	if err != nil || !p.Ended() {
		return 0, err
	}
	if p.Warned == 0 && !p.SecretsDeleted {
		if _, err := j.Store.EndTrial(ctx, tenantID, at); err != nil {
			return 0, err
		}
	}
	// Sorting stopped when the plan ended (pipeline.Pipeline.Halted); the mailboxes stop too.
	if err := j.stopAll(ctx, tenantID); err != nil {
		return 0, err
	}
	steps := 0
	if due := dueWarning(p.EndedAt, at); p.Warned < due {
		if err := j.warn(ctx, tenantID, p.PlanState, due, now); err != nil {
			return steps, err
		}
		if err := j.Store.PlanWarned(ctx, tenantID, p.EndedAt, due, at); err != nil {
			return steps, err
		}
		p.Warned, p.WarnedAt = due, at
		steps++
	}
	if !p.SecretsDeleted && at >= secretsAt(p.EndedAt) && p.Warned >= 2 && at >= p.WarnedAt+NoticeDays*day {
		if err := j.stopAll(ctx, tenantID); err != nil {
			return steps, err
		}
		ids, err := j.Store.DeleteTenantSecrets(ctx, tenantID, p.EndedAt, at)
		if err != nil {
			return steps, err
		}
		for _, id := range ids {
			j.Stop(id) // one started meanwhile can no longer log in; this ends its session
			if a, err := j.Store.Account(ctx, id); err == nil && j.Hub != nil {
				j.Hub.Publish(tenantID, id, events.AccountStatus, a)
			}
		}
		slog.InfoContext(ctx, "deleted the secrets of a tenant whose plan ended", "tenant_id", tenantID, "mailboxes", len(ids))
		steps++
	}
	if at >= removeAt(p.EndedAt) && p.Warned >= 3 && at >= p.WarnedAt+NoticeDays*day {
		if err := j.stopAll(ctx, tenantID); err != nil {
			return steps, err
		}
		gone, err := j.Store.RemoveEndedTenant(ctx, tenantID, p.EndedAt, at)
		if err != nil {
			return steps, err
		}
		slog.InfoContext(ctx, "removed a tenant whose plan ended", "tenant_id", tenantID, "mailboxes", len(gone.Accounts))
		steps++
	}
	return steps, nil
}

// stopAll stops every mailbox of the tenant. When the plan no longer ended by then (one
// bought meanwhile, whose start the stop may have undone), it starts again those that may
// run and answers store.ErrPlanNotEnded.
func (j PlanEnd) stopAll(ctx context.Context, tenantID int64) error {
	accounts, err := j.Store.TenantAccounts(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		j.Stop(a.ID)
	}
	p, err := j.Store.TenantPlan(ctx, tenantID, j.now().Unix())
	if err != nil || p.Ended() {
		return err
	}
	if accounts, err = j.Store.TenantAccounts(ctx, tenantID); err != nil {
		return err
	}
	for _, a := range accounts {
		if a.Status != StatusPaused && !a.SecretGone {
			j.Start(a)
		}
	}
	return store.ErrPlanNotEnded
}

func secretsAt(endedAt int64) int64 { return endedAt + store.EndedSecretsDays*day }
func removeAt(endedAt int64) int64  { return endedAt + store.EndedRemovalDays*day }

// dueWarning is the latest warning whose day has come by now: 1 from the end, 2 from
// warn2Days after, 3 from warn3Days after.
func dueWarning(endedAt, now int64) int {
	switch {
	case now >= endedAt+warn3Days*day:
		return 3
	case now >= endedAt+warn2Days*day:
		return 2
	case now >= endedAt:
		return 1
	}
	return 0
}

// warn sends warning n to every member of the tenant. It counts as given once at least one
// member got it; an address the mail server refuses is passed over. When nobody got it (no
// mail server, no member, every send failed), it returns an error and the warning is tried
// again next tick: the schedule waits, and no deletion comes before a delivered warning.
func (j PlanEnd) warn(ctx context.Context, tenantID int64, p store.PlanState, n int, now time.Time) error {
	to, err := j.Store.TenantEmails(ctx, tenantID)
	if err != nil {
		return err
	}
	if j.Sender == nil {
		return fmt.Errorf("send end-of-plan warning %d: no mail server is set (MAILRULES_SMTP_*)", n)
	}
	msg, err := warning(p, n, now, j.PublicURL)
	if err != nil {
		return err
	}
	sent := 0
	var errs []error
	for _, addr := range to {
		msg.To = addr
		switch err := j.Sender.Send(ctx, msg); {
		case err == nil:
			sent++
		case errors.Is(err, mailer.ErrAddress):
			slog.WarnContext(ctx, "a member's address cannot take the end-of-plan warning", "tenant_id", tenantID, "warning", n)
		default:
			errs = append(errs, err)
		}
	}
	// Some got it: the rest are not sent the same warning again and again.
	if sent == 0 {
		if len(errs) == 0 {
			return fmt.Errorf("send end-of-plan warning %d: no member could be sent it", n)
		}
		return fmt.Errorf("send end-of-plan warning %d: %w", n, errors.Join(errs...))
	}
	slog.InfoContext(ctx, "sent an end-of-plan warning", "tenant_id", tenantID, "warning", n, "members", sent, "failed", len(errs))
	return nil
}

// notice is one warning's content, shared by its text and HTML.
type notice struct {
	Title   string
	Lead    string
	Still   string
	Deletes []string
	Action  string
	URL     string
}

const dateLayout = "2 January 2006"

func date(at int64) string { return time.Unix(at, 0).UTC().Format(dateLayout) }

// warning renders warning n (1 to 3), sent at now, about the plan that ended at p.EndedAt.
// It states the dates the job keeps to: a deletion comes on its day, or NoticeDays after
// the warning announcing it, whichever is later. It names no mailbox, address or email.
func warning(p store.PlanState, n int, now time.Time, base string) (mailer.Message, error) {
	what := "plan"
	if p.EndedAt == p.TrialEnd {
		what = "trial"
	}
	ended, at := date(p.EndedAt), now.Unix()
	secrets, removal := secretsAt(p.EndedAt), removeAt(p.EndedAt)
	if n >= 2 {
		secrets = max(secrets, at+NoticeDays*day)
	}
	if n == 3 {
		removal = max(removal, at+NoticeDays*day)
	}
	c := notice{
		Still: "You can still sign in and read Activity, your rules and your settings. Nothing in your mailboxes was changed.",
		URL:   strings.TrimRight(base, "/") + "/#/plan",
	}
	secretsLine := "On " + date(secrets) + ", the passwords and sign-ins of your mailboxes and your saved AI model keys."
	removalLine := "On " + date(removal) + ", everything else: what MailRules kept of your mailboxes and your email, your rules, your settings and your sign-in."
	switch n {
	case 1:
		c.Title = "Your MailRules " + what + " has ended"
		c.Lead = "Your MailRules " + what + " ended on " + ended + ", so MailRules has stopped sorting your email."
	case 2:
		c.Title = "MailRules will delete your mailbox passwords on " + date(secrets)
		c.Lead = "Your MailRules " + what + " ended on " + ended + ", so MailRules sorts nothing. In " + days(secrets-at) +
			", on " + date(secrets) + ", it deletes the passwords and sign-ins of your mailboxes and your saved AI model keys."
	case 3:
		c.Title = "MailRules will delete your data on " + date(removal)
		c.Lead = "Your MailRules " + what + " ended on " + ended + ", so MailRules sorts nothing. In " + days(removal-at) +
			", on " + date(removal) + ", it deletes everything it kept for you."
	default:
		return mailer.Message{}, fmt.Errorf("end-of-plan warning %d: there are 3", n)
	}
	if p.SecretsDeleted {
		c.Still += " The passwords and sign-ins of your mailboxes and your saved AI model keys were deleted already."
	} else {
		c.Deletes = append(c.Deletes, secretsLine)
	}
	c.Deletes = append(c.Deletes, removalLine)
	c.Action = "Subscribe on Plan and billing before " + date(removal) + " to sort again and keep everything."
	if p.SecretsDeleted {
		c.Action += " You will need to enter your mailbox passwords again."
	} else {
		c.Action += " After " + date(secrets) + " you will need to enter your mailbox passwords again."
	}

	var t strings.Builder
	line := func(s string) { t.WriteString(s + "\n") }
	line(c.Lead)
	line("")
	line(c.Still)
	line("")
	line("What will be deleted:")
	for _, d := range c.Deletes {
		line("- " + d)
	}
	line("")
	line(c.Action)
	line("Plan and billing: " + c.URL)
	var h bytes.Buffer
	if err := warningPage.Execute(&h, c); err != nil {
		return mailer.Message{}, fmt.Errorf("render end-of-plan warning: %w", err)
	}
	return mailer.Message{Subject: c.Title, Text: t.String(), HTML: h.String()}, nil
}

// days says a span of seconds in whole days, rounded up.
func days(seconds int64) string {
	d := (seconds + day - 1) / day
	if d == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", d)
}

var warningPage = template.Must(template.New("warning").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title></head>
<body style="margin:0;padding:0;background:#ffffff;color:#1c1c1c;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;">
<div style="max-width:600px;margin:0 auto;padding:24px 16px;">
<h1 style="margin:0 0 16px;font-size:20px;font-weight:600;">{{.Title}}</h1>
<p style="margin:0 0 16px;">{{.Lead}}</p>
<p style="margin:0 0 16px;">{{.Still}}</p>
<h2 style="margin:0 0 6px;font-size:16px;font-weight:600;">What will be deleted</h2>
<ul style="margin:0 0 16px;padding-left:20px;">{{range .Deletes}}<li>{{.}}</li>{{end}}</ul>
<p style="margin:0 0 20px;">{{.Action}}</p>
<p style="margin:0;"><a href="{{.URL}}" style="color:#1a4fd6;font-weight:600;">Open Plan and billing</a></p>
</div>
</body></html>
`))

// Run runs the job now and then every Every, until ctx is done.
func (j PlanEnd) Run(ctx context.Context) {
	tick := time.NewTicker(or(j.Every, planEndPeriod))
	defer tick.Stop()
	for {
		switch steps, err := j.Once(ctx); {
		case err != nil && ctx.Err() == nil:
			slog.ErrorContext(ctx, "the end-of-plan job failed", "error", err.Error())
		case steps > 0:
			slog.InfoContext(ctx, "end of plan", "steps", steps)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
