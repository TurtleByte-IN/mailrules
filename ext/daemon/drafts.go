package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	netmail "net/mail"
	"strings"
	"sync/atomic"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	// draftEvery is how often the worker looks for replies to draft: a reply is written
	// within about this long of the rule acting, plus the time the model takes.
	draftEvery = 2 * time.Second
	// draftBatch is how many jobs one look takes at most.
	draftBatch = 20
	// draftTimeout bounds one Write call (ext.Drafts.Write).
	draftTimeout = 60 * time.Second
	// maxDraftText is how many characters of a reply are saved; a module that writes more
	// is cut there.
	maxDraftText = 20000
	// draftPurpose is the usage ledger's purpose of a drafting model's calls.
	draftPurpose = "draft"
)

// draftsModule is the build's module that writes draft replies, or nil.
func draftsModule(modules []ext.Module) *ext.Drafts {
	for _, m := range modules {
		if m.Drafts != nil {
			return m.Drafts
		}
	}
	return nil
}

// drafter is the generative model a tenant's replies are written with, its ledger, and
// whether its calls go out on the operator's keys (settings.Settings.Drafter).
type drafter func(ctx context.Context, tenantID int64) (models.Generator, store.Ledger, bool, error)

// draftWorker writes the replies the executor queued (store.DraftJob), through the module,
// and has the executor save them: at least once each, across restarts, until the job is
// settled or store.DraftWait has passed.
type draftWorker struct {
	store  *store.Store
	exec   *actions.Exec
	drafts *ext.Drafts
	model  drafter
	// allowance is whether the tenant used up the month's AI-sorted emails (api.Allowance);
	// nil in a build that sells no plans.
	allowance func(ctx context.Context, tenantID int64, now time.Time) (bool, error)
	hub       *events.Hub
	bodyChars int              // MAILRULES_BODY_CHARS: how much of an email the model reads
	every     time.Duration    // 0 = draftEvery
	now       func() time.Time // nil = time.Now
}

func (w *draftWorker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// Run drafts what is waiting now, and then every few seconds, until ctx is done.
func (w *draftWorker) Run(ctx context.Context) {
	every := w.every
	if every == 0 {
		every = draftEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := w.once(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "drafting replies failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// once works through the jobs that are due, oldest first. One that fails does not hold up
// the rest; the errors are returned joined.
func (w *draftWorker) once(ctx context.Context) error {
	waiting, err := w.store.WaitingDrafts(ctx, w.clock().Unix(), draftBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range waiting {
		if ctx.Err() != nil {
			break
		}
		if err := w.hand(ctx, j); err != nil {
			errs = append(errs, fmt.Errorf("draft job %d: %w", j.ID, err))
		}
	}
	return errors.Join(errs...)
}

// hand has one reply written and saved. A job that cannot be done now is tried again later,
// a few seconds at first and at most an hour apart; once store.DraftWait has passed since
// its rule acted, it is given up and Activity says why.
func (w *draftWorker) hand(ctx context.Context, j store.DraftJob) error {
	expired := w.clock().Unix()-j.CreatedAt >= store.DraftWait
	_, err := w.exec.Draft(ctx, j, func(ctx context.Context, mb mail.Mailbox, email mail.MsgRef, msg store.Message) (actions.Reply, error) {
		return w.write(ctx, j, mb, email, expired)
	})
	if err != nil && !errors.Is(err, actions.ErrDraftSettled) {
		if ctx.Err() != nil {
			return nil // stopping: it is tried again after the restart
		}
		if !expired {
			slog.WarnContext(ctx, "a reply could not be drafted yet; it is tried again later",
				"draft_job", j.ID, "account", j.AccountID, "attempts", j.Attempts+1, "error", err.Error())
			return w.store.DraftRetry(ctx, j, w.clock().Add(retryAfter(j.Attempts)).Unix())
		}
		slog.WarnContext(ctx, "a reply could not be drafted in time; giving up", "draft_job", j.ID, "account", j.AccountID, "error", err.Error())
		_, err = w.exec.GiveUpDraft(ctx, j, "the mailbox could not be reached in time to draft a reply")
		if err != nil && !errors.Is(err, actions.ErrDraftSettled) {
			return err
		}
	}
	return w.store.DraftDone(context.WithoutCancel(ctx), j)
}

// write reads the email (and, when the rule allows it, the thread's previous message), has
// the module write the reply on the tenant's model and builds the message to save. What it
// will not draft, for good, is an *actions.SkipDraft with the reason Activity shows; any
// other error is tried again, or, once expired, given up as a SkipDraft too.
func (w *draftWorker) write(ctx context.Context, j store.DraftJob, mb mail.Mailbox, email mail.MsgRef, expired bool) (actions.Reply, error) {
	acct, err := w.store.Account(ctx, j.AccountID)
	if err != nil {
		return actions.Reply{}, err
	}
	from := ownAddress(acct)
	if from == "" {
		return actions.Reply{}, &actions.SkipDraft{Reason: "the mailbox's own address is not known, so a reply cannot be written from it"}
	}
	gen, ledger, metered, err := w.model(ctx, j.TenantID)
	if errors.Is(err, settings.ErrNoComposer) {
		return actions.Reply{}, &actions.SkipDraft{Reason: "no AI model is set up to write replies: choose the rule composer model in Settings"}
	}
	if err != nil {
		return actions.Reply{}, err
	}
	now := w.clock()
	if metered && w.allowance != nil {
		reached, err := w.allowance(ctx, j.TenantID, now)
		if err != nil {
			return actions.Reply{}, fmt.Errorf("check the AI allowance: %w", err)
		}
		if reached {
			return actions.Reply{}, &actions.SkipDraft{Reason: "this month's AI-sorted emails are used up, so no reply was drafted"}
		}
	}
	sum, err := w.read(ctx, mb, email)
	if err != nil {
		return actions.Reply{}, err
	}
	if sum == nil {
		return actions.Reply{}, &actions.SkipDraft{Reason: "the email could not be read, so no reply was drafted"}
	}
	req := ext.DraftRequest{ID: j.ID, TenantID: j.TenantID, RuleID: j.RuleID, RuleName: j.RuleName, Instruction: j.Instruction,
		Mailbox: from, Email: draftEmail(sum)}
	if j.ReadPrevious {
		if prev := w.previous(ctx, mb, j.AccountID, email, sum); prev != nil {
			e := draftEmail(prev)
			req.Previous = &e
		}
	}
	booked := &bookedModel{gen: gen, ledger: ledger, ctx: context.WithoutCancel(ctx), now: w.clock}
	req.Model = booked
	callCtx, cancel := context.WithTimeout(models.WithPurpose(ctx, draftPurpose), draftTimeout)
	text, err := w.drafts.Write(callCtx, req)
	cancel()
	if booked.calls.Load() > 0 {
		w.hub.Publish(j.TenantID, 0, events.UsageUpdated, nil)
	}
	var refused *ext.DraftRefused
	switch {
	case errors.As(err, &refused):
		return actions.Reply{}, &actions.SkipDraft{Reason: oneSentence(refused.Reason, "the drafting module wrote no reply")}
	case err != nil && ctx.Err() == nil && expired:
		slog.WarnContext(ctx, "the drafting module could not write a reply in time; giving up", "draft_job", j.ID, "error", err.Error())
		return actions.Reply{}, &actions.SkipDraft{Reason: "the drafting module could not write a reply"}
	case err != nil:
		return actions.Reply{}, fmt.Errorf("the drafting module: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return actions.Reply{}, &actions.SkipDraft{Reason: "the drafting module wrote no reply"}
	}
	if rs := []rune(text); len(rs) > maxDraftText {
		text = string(rs[:maxDraftText])
	}
	// The reply counts as one AI-sorted email when the model it was written with runs on a
	// key the operator holds, as an email sorted on one does.
	if metered && booked.calls.Load() > 0 {
		if err := w.store.SpendAIEmails(context.WithoutCancel(ctx), j.TenantID, now, 1); err != nil {
			slog.WarnContext(ctx, "could not count a drafted reply against the AI allowance", "tenant", j.TenantID, "error", err.Error())
		}
	}
	raw, id := message.Reply(sum, netmail.Address{Address: from}, text, j.Key, now)
	return actions.Reply{Raw: raw, MessageID: id}, nil
}

// read fetches one email with BODY.PEEK, its text cut as the decision model's is; nil when
// it cannot be read as an email.
func (w *draftWorker) read(ctx context.Context, mb mail.Mailbox, ref mail.MsgRef) (*message.Summary, error) {
	raw, err := mb.Fetch(ctx, ref, 0)
	if err != nil {
		return nil, err
	}
	sum, err := message.Parse(raw, ref.AccountID, w.bodyChars)
	if err != nil {
		return nil, nil //nolint:nilerr // an email that cannot be parsed is skipped, as the pipeline skips it
	}
	return sum, nil
}

// previous finds the message email replies to (its In-Reply-To, else the last of its
// References) by Message-ID, in the mailbox's Sent folder, then the folder the email is in,
// and reads it as read does. nil when it has none, or none is found or readable: the reply
// is then written from the email alone.
func (w *draftWorker) previous(ctx context.Context, mb mail.Mailbox, accountID int64, email mail.MsgRef, sum *message.Summary) *message.Summary {
	parent := parentID(sum.Headers)
	if parent == "" {
		return nil
	}
	folders := []string{}
	if fs, err := w.store.Folders(ctx, accountID); err == nil {
		for _, f := range fs {
			if f.SpecialUse == mail.RoleSent {
				folders = append(folders, f.Name)
			}
		}
	}
	folders = append(folders, email.Folder)
	for _, folder := range folders {
		ref, err := mb.FindByMessageID(ctx, folder, parent)
		if err != nil {
			continue
		}
		prev, err := w.read(ctx, mb, ref)
		if err == nil && prev != nil {
			return prev
		}
	}
	return nil
}

// parentID is the Message-ID, without angle brackets, of the message one with these headers
// replies to: its In-Reply-To, else the last of its References.
func parentID(h map[string][]string) string {
	for _, key := range []string{"In-Reply-To", "References"} {
		if v := h[key]; len(v) > 0 {
			if ids := strings.Fields(v[0]); len(ids) > 0 {
				id := strings.Trim(ids[len(ids)-1], "<>")
				if id != "" && !strings.ContainsAny(id, "<> ") {
					return id
				}
			}
		}
	}
	return ""
}

func draftEmail(s *message.Summary) ext.DraftEmail {
	return ext.DraftEmail{From: s.From, FromName: s.FromName, To: s.To, Subject: s.Subject, Date: s.ReceivedAt, Text: s.Body}
}

// ownAddress is the address a reply from the mailbox is written from: its username when that
// is an address, else the first address the mailbox is known by; "" when none is.
func ownAddress(a store.Account) string {
	if u := strings.ToLower(strings.TrimSpace(a.Username)); strings.Contains(u, "@") {
		return u
	}
	if own := a.OwnAddresses(); len(own) > 0 {
		return own[0]
	}
	return ""
}

// oneSentence is a module's reason as Activity shows it: on one line, at most maxReason
// characters; def when it gave none.
func oneSentence(reason, def string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if rs := []rune(reason); len(rs) > maxReason {
		reason = string(rs[:maxReason-1]) + "…"
	}
	if reason == "" {
		return def
	}
	return reason
}

// bookedModel is the tenant's model as a module writes a reply with it: every call is booked
// on the tenant's usage ledger under draftPurpose, and counted.
type bookedModel struct {
	gen    models.Generator
	ledger store.Ledger
	ctx    context.Context // books even when the module's call was cut off
	now    func() time.Time
	calls  atomic.Int64
}

var _ ext.Generator = (*bookedModel)(nil)

func (b *bookedModel) Generate(ctx context.Context, system, user string, schema json.RawMessage, out any) (ext.Usage, error) {
	u, err := b.gen.Generate(models.WithPurpose(ctx, draftPurpose), system, user, schema, out)
	if err == nil || u.TokensIn+u.TokensOut > 0 { // an unusable answer was paid for too
		b.calls.Add(1)
		if berr := b.ledger.AddUsage(b.ctx, b.now().UTC().Format("2006-01-02"), u.Provider, u.Model, draftPurpose, 1,
			u.TokensIn, u.TokensOut, u.CostUSD); berr != nil {
			slog.WarnContext(ctx, "could not record model usage", "error", berr.Error())
		}
	}
	return u, err
}
