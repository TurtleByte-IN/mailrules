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

	"github.com/TurtleByte-IN/mailrules/internal/mailer"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// remindTrials sends the trial-ending reminder to every tenant owed it by now
// (store.TrialReminderTenants), and returns how many it reminded. A tenant that fails does
// not keep the others from their turn; it is tried again next tick.
func (j PlanEnd) remindTrials(ctx context.Context, now time.Time) (int, error) {
	ids, err := j.Store.TrialReminderTenants(ctx, now.Unix())
	if err != nil {
		return 0, err
	}
	sent := 0
	var errs []error
	for _, id := range ids {
		ok, err := j.remindTrial(ctx, id, now)
		if ok {
			sent++
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %d: %w", id, err))
		}
	}
	return sent, errors.Join(errs...)
}

// remindTrial emails the tenant's owners that its trial ends in a few days, once: it is
// recorded once at least one owner got it. A tenant gone or with a plan bought meanwhile
// is passed over. When no owner got it, it returns an error and is tried again next tick.
func (j PlanEnd) remindTrial(ctx context.Context, tenantID int64, now time.Time) (bool, error) {
	p, err := j.Store.TenantPlan(ctx, tenantID, now.Unix())
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil || p.Plan.Plan != store.PlanTrial {
		return false, err
	}
	if j.Sender == nil {
		return false, errors.New("send trial reminder: no mail server is set (MAILRULES_SMTP_*)")
	}
	to, err := j.Store.TenantOwnerEmails(ctx, tenantID)
	if err != nil {
		return false, err
	}
	msg, err := trialReminder(p.TrialEnd, now, j.PublicURL)
	if err != nil {
		return false, err
	}
	sent := 0
	var errs []error
	for _, addr := range to {
		msg.To = addr
		switch err := j.Sender.Send(ctx, msg); {
		case err == nil:
			sent++
		case errors.Is(err, mailer.ErrAddress):
			slog.WarnContext(ctx, "an owner's address cannot take the trial reminder", "tenant_id", tenantID)
		default:
			errs = append(errs, err)
		}
	}
	if sent == 0 {
		if len(errs) == 0 {
			return false, errors.New("send trial reminder: no owner could be sent it")
		}
		return false, fmt.Errorf("send trial reminder: %w", errors.Join(errs...))
	}
	if err := j.Store.TrialReminded(ctx, tenantID, now.Unix()); err != nil {
		return false, err
	}
	slog.InfoContext(ctx, "sent the trial reminder", "tenant_id", tenantID, "owners", sent, "failed", len(errs))
	return true, nil
}

// trialReminder renders the reminder, sent at now, that the trial ending at end ends soon.
// It names no mailbox, address or email.
func trialReminder(end int64, now time.Time, base string) (mailer.Message, error) {
	c := notice{
		Title:  "Your MailRules trial ends on " + date(end),
		Lead:   "Your MailRules trial ends in " + days(end-now.Unix()) + ", on " + date(end) + ". After that, MailRules stops sorting your email.",
		Still:  "Nothing in your mailboxes changes when the trial ends, and you can still sign in and read Activity, your rules and your settings.",
		Action: "Choose a plan on Plan and billing before " + date(end) + " to keep sorting without a break.",
		URL:    strings.TrimRight(base, "/") + "/#/plan",
	}
	var t strings.Builder
	t.WriteString(c.Lead + "\n\n" + c.Still + "\n\n" + c.Action + "\n" + "Plan and billing: " + c.URL + "\n")
	var h bytes.Buffer
	if err := reminderPage.Execute(&h, c); err != nil {
		return mailer.Message{}, fmt.Errorf("render trial reminder: %w", err)
	}
	return mailer.Message{Subject: c.Title, Text: t.String(), HTML: h.String()}, nil
}

var reminderPage = template.Must(template.New("reminder").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title></head>
<body style="margin:0;padding:0;background:#ffffff;color:#1c1c1c;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;">
<div style="max-width:600px;margin:0 auto;padding:24px 16px;">
<h1 style="margin:0 0 16px;font-size:20px;font-weight:600;">{{.Title}}</h1>
<p style="margin:0 0 16px;">{{.Lead}}</p>
<p style="margin:0 0 16px;">{{.Still}}</p>
<p style="margin:0 0 20px;">{{.Action}}</p>
<p style="margin:0;"><a href="{{.URL}}" style="color:#1a4fd6;font-weight:600;">Open Plan and billing</a></p>
</div>
</body></html>
`))
