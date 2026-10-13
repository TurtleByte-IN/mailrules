package actions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// DraftFlags are the flags a saved draft reply gets: a draft, and read, so it adds nothing
// to the mailbox's unread count.
var DraftFlags = []string{`\Draft`, `\Seen`}

// Reply is a draft reply ready to save: the whole message and its Message-ID, without angle
// brackets, which must be the same on every attempt for the same job.
type Reply struct {
	Raw       []byte
	MessageID string
}

// SkipDraft is a reply that will not be written, for good: Reason is the sentence the draft
// action's error records ("This month's AI-sorted emails are used up.").
type SkipDraft struct{ Reason string }

func (e *SkipDraft) Error() string { return e.Reason }

// ErrDraftSettled means a draft job has nothing left to do: the rule's actions were undone
// or corrected since, its email's row is gone, or its draft action is recorded already. No
// row is written for it.
var ErrDraftSettled = errors.New("the draft reply is settled")

// DraftWriter writes the reply to msg, which is at email in mb now: it reads the email, has
// the reply written and builds the message. A *SkipDraft is recorded as the draft action's
// failure; any other error leaves nothing recorded, for the caller to try again later.
type DraftWriter func(ctx context.Context, mb mail.Mailbox, email mail.MsgRef, msg store.Message) (Reply, error)

// Draft saves the reply a rule's live run asked for (store.DraftJob) in the account's Drafts
// folder, the one the server marks \Drafts: MailRules never guesses one or creates one, and
// never sends what it saves. It is a mailbox change like any other: in the tenant's dry-run
// it records a dry_run row and writes nothing (the writer is not called, so no model is
// paid for); otherwise it writes the row first, then appends, then records the outcome, with
// the draft's place, so the draft can be undone by itself or with its batch. A draft found
// in the folder already under the reply's Message-ID (saved before a crash) is not saved
// again. The writer runs before the account's lock is taken, so a slow model holds up
// nothing; the rule's run is checked again under the lock. ErrDraftSettled when there is
// nothing left to do. Any other error is the caller's to retry: nothing is recorded.
func (x *Exec) Draft(ctx context.Context, j store.DraftJob, write DraftWriter) (ActionRecord, error) {
	return x.draft(ctx, j, write, "")
}

// GiveUpDraft records the reply of a draft job that is given up before Draft could save it
// (its mailbox stayed unreachable) as a failed draft action that says why: reason. In
// dry-run it is recorded as Draft records it. ErrDraftSettled as Draft.
func (x *Exec) GiveUpDraft(ctx context.Context, j store.DraftJob, reason string) (ActionRecord, error) {
	return x.draft(ctx, j, nil, reason)
}

func (x *Exec) draft(ctx context.Context, j store.DraftJob, write DraftWriter, giveUp string) (ActionRecord, error) {
	msg, err := x.Store.Message(ctx, j.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		return ActionRecord{}, ErrDraftSettled
	}
	if err != nil {
		return ActionRecord{}, err
	}
	prev, found, err := x.Store.DecisionDraft(ctx, j.TenantID, j.DecisionID)
	if err != nil {
		return ActionRecord{}, err
	}
	interrupted := found && prev.Status == store.ActionFailed && prev.Error == store.ActionInterrupted
	if found && !interrupted {
		return prev, ErrDraftSettled
	}
	if err := x.inEffect(ctx, j); err != nil {
		return ActionRecord{}, err
	}
	at := msg.Location()
	rec := ActionRecord{DecisionID: j.DecisionID, BatchID: j.BatchID, MessageID: j.MessageID, AccountID: j.AccountID,
		Kind: store.ActionDraft, Before: store.Snapshot{Folder: at.Folder, UIDValidity: at.UIDValidity, UID: at.UID}, CreatedAt: x.now()}
	if interrupted {
		rec.ID = prev.ID
	}
	recCtx := context.WithoutCancel(ctx)
	// record writes rec as it ends, with no mailbox change made.
	record := func(status, reason string) (ActionRecord, error) {
		rec.Status, rec.Error = status, reason
		if rec.ID == 0 {
			if rec.ID, err = x.Store.InsertAction(recCtx, rec); err != nil {
				return rec, err
			}
		} else if err := x.Store.FinishAction(recCtx, rec, store.FollowUps{}); err != nil {
			return rec, err
		}
		x.published(recCtx, j.TenantID, msg)
		return rec, nil
	}

	dry, err := x.Store.DryRun(ctx, j.TenantID, x.DryRunDefault)
	if err != nil {
		return ActionRecord{}, err
	}
	if dry {
		return record(store.ActionDryRun, "")
	}
	if giveUp != "" {
		return record(store.ActionFailed, giveUp)
	}
	mb, err := x.Accounts.Mailbox(j.AccountID)
	if err != nil {
		return ActionRecord{}, err
	}
	if rec.Folder, err = x.roleFolder(ctx, j.AccountID, mail.RoleDrafts); err != nil {
		var nf *NoFolderError
		if errors.As(err, &nf) {
			return record(store.ActionFailed, err.Error())
		}
		return ActionRecord{}, err
	}
	email, err := find(ctx, mb, at, msg.MessageID, at)
	if errors.Is(err, ErrGone) {
		return record(store.ActionFailed, "the email was moved or deleted before a reply was drafted")
	}
	if err != nil {
		return ActionRecord{}, err
	}
	rec.Before.Folder, rec.Before.UIDValidity, rec.Before.UID = email.Folder, email.UIDValidity, email.UID
	reply, err := write(ctx, mb, email, msg)
	var skip *SkipDraft
	if errors.As(err, &skip) {
		return record(store.ActionFailed, skip.Reason)
	}
	if err != nil {
		return ActionRecord{}, err
	}

	defer x.Accounts.Lock(j.AccountID)()
	if err := x.inEffect(ctx, j); err != nil { // an undo or a fix may have come while it was written
		return ActionRecord{}, err
	}
	rec.Status, rec.Error = store.ActionFailed, store.ActionInterrupted
	if rec.ID == 0 {
		if rec.ID, err = x.Store.InsertAction(recCtx, rec); err != nil {
			return rec, err
		}
	}
	saved, err := mb.FindByMessageID(ctx, rec.Folder, reply.MessageID)
	if errors.Is(err, mail.ErrNotFound) {
		saved, err = mb.Append(ctx, rec.Folder, reply.Raw, DraftFlags, time.Unix(x.now(), 0))
	}
	if err != nil {
		// Not tried again: the reply would be paid for twice, and what failed is on record.
		rec.Error = fmt.Sprintf("could not save the draft: %v", err)
		ferr := x.Store.FinishAction(recCtx, rec, store.FollowUps{})
		x.published(recCtx, j.TenantID, msg)
		return rec, ferr
	}
	after := rec.Before
	rec.Status, rec.Error, rec.After = store.ActionDone, "", &after
	rec.Draft = &store.DraftRef{UIDValidity: saved.UIDValidity, UID: saved.UID, MessageID: reply.MessageID}
	if err := x.Store.FinishAction(recCtx, rec, store.FollowUps{}); err != nil {
		return rec, err
	}
	x.published(recCtx, j.TenantID, msg)
	return rec, nil
}

// inEffect is ErrDraftSettled when what the job's rule did to its email no longer stands.
func (x *Exec) inEffect(ctx context.Context, j store.DraftJob) error {
	ok, err := x.Store.RunInEffect(ctx, j.TenantID, j.DecisionID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDraftSettled
	}
	return nil
}

// published sends the email's row as it is now, so an open Activity shows the draft.
func (x *Exec) published(ctx context.Context, tenantID int64, msg store.Message) {
	if row, err := x.Store.MessageActivity(ctx, msg.ID); err == nil {
		x.Hub.Publish(tenantID, msg.AccountID, events.MessageProcessed, row)
	}
}

// undoDraft deletes the reply a done draft action saved: exactly that message, found at the
// UID the server gave it, or by its Message-ID when the folder's UIDVALIDITY changed. One
// that is no longer there (the person edited, sent, moved or deleted it) is ErrGone, and
// nothing is deleted.
func (x *Exec) undoDraft(ctx context.Context, a store.Action) error {
	if a.Draft == nil {
		return fmt.Errorf("undo draft: action %d has no saved draft on record", a.ID)
	}
	mb, err := x.Accounts.Mailbox(a.AccountID)
	if err != nil {
		return err
	}
	want := mail.MsgRef{AccountID: a.AccountID, Folder: a.Folder, UIDValidity: a.Draft.UIDValidity, UID: a.Draft.UID}
	at, err := find(ctx, mb, want, a.Draft.MessageID, want)
	if err != nil {
		return err
	}
	if err := mb.Delete(ctx, at); err != nil {
		return fmt.Errorf("undo draft: %w", err)
	}
	return nil
}
