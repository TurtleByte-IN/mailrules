package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// ErrGone means an action cannot be undone because its message is no longer where
// MailRules left it. The HTTP layer answers 409 with this text.
var ErrGone = errors.New("message was moved or deleted outside MailRules")

// Accounts is how the executor reaches a mail account (worker.Manager implements it).
type Accounts interface {
	// Mailbox returns the account's open connection.
	Mailbox(accountID int64) (mail.Mailbox, error)
	// Lock waits until the account's processor is between messages and holds it there
	// until unlock is called. Undo and corrections take it, so mail they move back into
	// the watched folder is on record before the processor can mistake it for new mail.
	Lock(accountID int64) (unlock func())
}

// Exec is the executor: the only code that changes a mailbox.
type Exec struct {
	Store    *store.Store
	Accounts Accounts
	Hub      *events.Hub // nil = publish nothing
	// DryRunDefault is used until the dry_run setting is stored (MAILRULES_DRY_RUN).
	DryRunDefault bool
	Now           func() time.Time // nil = time.Now
}

var _ Executor = (*Exec)(nil)

func (x *Exec) now() int64 {
	if x.Now != nil {
		return x.Now().Unix()
	}
	return time.Now().Unix()
}

// moveRole is the special-use folder each move-type action other than "move" goes to.
var moveRole = map[string]string{rules.ActArchive: mail.RoleArchive, rules.ActTrash: mail.RoleTrash, rules.ActJunk: mail.RoleJunk}

// flagOf is the one flag each flag-type action sets (or clears).
var flagOf = map[string]struct {
	flag string
	set  bool
}{
	rules.ActFlag: {`\Flagged`, true}, rules.ActUnflag: {`\Flagged`, false},
	rules.ActRead: {`\Seen`, true}, rules.ActUnread: {`\Seen`, false},
	KindReview: {ReviewKeyword, true},
}

func isMove(kind string) bool { return kind == rules.ActMove || moveRole[kind] != "" }

// Apply runs acts in order on one message. In dry-run (checked here and nowhere else) it
// only writes a dry_run row per action and never contacts the server. Otherwise it reads
// the message's flags once, and for each action writes its row with the before snapshot
// first, then changes the mailbox, then stores the outcome, so there is no change without
// a row. A failure stops the remaining actions and is returned; whoever asked decides
// about a retry.
func (x *Exec) Apply(ctx context.Context, d DecisionRecord, acts []rules.Action, batchID int64) ([]ActionRecord, error) {
	dry, err := x.Store.DryRun(ctx, x.DryRunDefault)
	if err != nil {
		return nil, err
	}
	cur := store.Snapshot{Folder: d.Ref.Folder, UIDValidity: d.Ref.UIDValidity, UID: d.Ref.UID}
	row := func(a rules.Action, status string) ActionRecord {
		return ActionRecord{DecisionID: d.DecisionID, BatchID: batchID, MessageID: d.MessageID, AccountID: d.Ref.AccountID,
			Kind: a.Type, Folder: a.Folder, Before: cur, Status: status, CreatedAt: x.now()}
	}
	var recs []ActionRecord
	if dry {
		for _, a := range acts {
			rec := row(a, store.ActionDryRun)
			if rec.ID, err = x.Store.InsertAction(ctx, rec); err != nil {
				return recs, err
			}
			recs = append(recs, rec)
		}
		return recs, nil
	}

	mb, snapErr := x.Accounts.Mailbox(d.Ref.AccountID)
	if snapErr == nil {
		if cur.Flags, snapErr = mb.Flags(ctx, d.Ref); snapErr != nil {
			snapErr = fmt.Errorf("read flags: %w", snapErr)
		}
	}
	// What follows a mailbox change must be recorded even if the daemon is stopping.
	recCtx := context.WithoutCancel(ctx)
	for _, a := range acts {
		rec := row(a, store.ActionFailed)
		rec.Error = "interrupted before it finished"
		if snapErr != nil {
			rec.Error = snapErr.Error() // no snapshot, so no change: the attempt is still on record
		}
		if rec.ID, err = x.Store.InsertAction(recCtx, rec); err != nil {
			return recs, err
		}
		if snapErr != nil {
			return append(recs, rec), snapErr
		}
		after, err := x.do(ctx, mb, d.Ref.AccountID, a, cur)
		if err != nil {
			rec.Error = err.Error()
			return append(recs, rec), errors.Join(fmt.Errorf("%s: %w", a.Type, err), x.Store.FinishAction(recCtx, rec))
		}
		rec.Status, rec.Error, rec.After = store.ActionDone, "", &after
		if err := x.Store.FinishAction(recCtx, rec); err != nil {
			return append(recs, rec), err
		}
		recs = append(recs, rec)
		cur = after
	}
	return recs, nil
}

// do performs one action on the message at cur and returns its state afterwards.
func (x *Exec) do(ctx context.Context, mb mail.Mailbox, accountID int64, a rules.Action, cur store.Snapshot) (store.Snapshot, error) {
	after := cur
	after.Flags = slices.Clone(cur.Flags)
	switch {
	case a.Type == rules.ActKeep:
		return after, nil // no IMAP call; recorded for the activity feed
	case isMove(a.Type):
		dest := a.Folder
		if role := moveRole[a.Type]; role != "" {
			// Never guess a folder, and never create one: trash, junk and archive only
			// ever move to the folder the server marks for that use.
			var err error
			if dest, err = x.roleFolder(ctx, accountID, role); err != nil {
				return after, err
			}
		} else if err := mb.EnsureFolder(ctx, dest); err != nil {
			return after, err
		}
		if dest == cur.Folder {
			return after, nil // already there, e.g. on a retry after a later action failed
		}
		moved, err := mb.Move(ctx, cur.Ref(accountID), dest)
		if err != nil {
			return after, err
		}
		after.Folder, after.UIDValidity, after.UID = moved.Folder, moved.UIDValidity, moved.UID
		return after, nil
	}
	f, ok := flagOf[a.Type]
	if !ok {
		return after, fmt.Errorf("unknown action %q", a.Type)
	}
	return setFlag(after, f.flag, f.set), setFlagOn(ctx, mb, cur.Ref(accountID), f.flag, f.set)
}

func setFlagOn(ctx context.Context, mb mail.Mailbox, ref mail.MsgRef, flag string, set bool) error {
	if set {
		return mb.SetFlags(ctx, ref, []string{flag}, nil)
	}
	return mb.SetFlags(ctx, ref, nil, []string{flag})
}

func setFlag(s store.Snapshot, flag string, set bool) store.Snapshot {
	s.Flags = slices.DeleteFunc(s.Flags, func(f string) bool { return f == flag })
	if set {
		s.Flags = append(s.Flags, flag)
		slices.Sort(s.Flags)
	}
	return s
}

// roleFolder returns the account's folder with a special-use role, from folder discovery.
func (x *Exec) roleFolder(ctx context.Context, accountID int64, role string) (string, error) {
	folders, err := x.Store.Folders(ctx, accountID)
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if f.SpecialUse == role {
			return f.Name, nil
		}
	}
	return "", fmt.Errorf("no %s folder", strings.TrimPrefix(role, `\`))
}

// EnsureFolders creates folders on an account's server, ahead of the rules that will move
// mail into them. It is a mailbox change like any other: in dry-run it does nothing, and
// the first live move creates the folder then.
func (x *Exec) EnsureFolders(ctx context.Context, accountID int64, names []string) error {
	dry, err := x.Store.DryRun(ctx, x.DryRunDefault)
	if err != nil || dry || len(names) == 0 {
		return err
	}
	mb, err := x.Accounts.Mailbox(accountID)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := mb.EnsureFolder(ctx, name); err != nil {
			return fmt.Errorf("create folder %q: %w", name, err)
		}
		if err := x.Store.AddFolder(context.WithoutCancel(ctx), accountID, name); err != nil {
			return err
		}
	}
	return nil
}

// Undo reverses one action: a move goes back to the folder it came from, a flag change
// goes back to what the before snapshot says. Undoing an action that is already undone,
// or that never changed the mailbox (dry-run, failed), does nothing. When the message is
// not where the action left it, the error is ErrGone and the action stays as it was.
// Undo does not look at dry-run: it only restores what MailRules itself changed, and
// only when asked.
func (x *Exec) Undo(ctx context.Context, actionID int64) error {
	a, err := x.Store.Action(ctx, actionID)
	if err != nil {
		return err
	}
	defer x.Accounts.Lock(a.AccountID)()
	return x.undo(ctx, actionID)
}

// undo is Undo for callers that hold the account's lock.
func (x *Exec) undo(ctx context.Context, actionID int64) error {
	a, err := x.Store.Action(ctx, actionID)
	if err != nil || a.Status != store.ActionDone || a.After == nil {
		return err
	}
	msg, err := x.Store.Message(ctx, a.MessageID)
	if err != nil {
		return err
	}
	restored := msg.Location()
	f, isFlag := flagOf[a.Kind]
	if moved := a.After.Folder != a.Before.Folder; moved || isFlag {
		mb, err := x.Accounts.Mailbox(a.AccountID)
		if err != nil {
			return err
		}
		if restored, err = locate(ctx, mb, a, msg); err != nil {
			return err
		}
		if moved {
			restored, err = mb.Move(ctx, restored, a.Before.Folder)
		} else {
			err = setFlagOn(ctx, mb, restored, f.flag, slices.Contains(a.Before.Flags, f.flag))
		}
		if err != nil {
			return fmt.Errorf("undo %s: %w", a.Kind, err)
		}
	}
	a.UndoneAt = x.now()
	if err := x.Store.MarkActionUndone(context.WithoutCancel(ctx), a, restored, a.UndoneAt); err != nil {
		return err
	}
	a.Status = store.ActionUndone
	x.Hub.Publish(events.ActionUndone, a)
	return nil
}

// locate finds the message an action left at a.After: by UID, or, when that folder's
// UIDVALIDITY has changed since, by Message-ID. A flag action's message may also have
// been moved on by a later MailRules action, so its last known location is tried too.
func locate(ctx context.Context, mb mail.Mailbox, a store.Action, msg store.Message) (mail.MsgRef, error) {
	missing := func(err error) bool { return errors.Is(err, mail.ErrNotFound) || errors.Is(err, mail.ErrNoFolder) }
	want := a.After.Ref(a.AccountID)
	tries := []mail.MsgRef{want}
	if !isMove(a.Kind) && msg.Location() != want {
		tries = append(tries, msg.Location())
	}
	for _, ref := range tries {
		if _, err := mb.Flags(ctx, ref); err == nil {
			return ref, nil
		} else if !missing(err) {
			return mail.MsgRef{}, err
		}
	}
	st, err := mb.Status(ctx, want.Folder)
	if err == nil && st.UIDValidity != want.UIDValidity && msg.MessageID != "" {
		var ref mail.MsgRef
		if ref, err = mb.FindByMessageID(ctx, want.Folder, msg.MessageID); err == nil {
			return ref, nil
		}
	}
	if err != nil && !missing(err) {
		return mail.MsgRef{}, err
	}
	return mail.MsgRef{}, ErrGone
}

// UndoBatch undoes a batch's actions, newest first, and returns how many it undid. An
// action that cannot be undone does not stop the rest: its error is joined into the one
// returned, and the batch is marked undone only when every action was.
func (x *Exec) UndoBatch(ctx context.Context, batchID int64) (int, error) {
	if _, err := x.Store.Batch(ctx, batchID); err != nil {
		return 0, err
	}
	acts, err := x.Store.BatchActions(ctx, batchID)
	if err != nil {
		return 0, err
	}
	undone := 0
	var errs []error
	for _, a := range slices.Backward(acts) {
		if a.Status != store.ActionDone {
			continue
		}
		if err := x.Undo(ctx, a.ID); err != nil {
			errs = append(errs, fmt.Errorf("action %d: %w", a.ID, err))
			continue
		}
		undone++
	}
	if len(errs) > 0 {
		return undone, errors.Join(errs...)
	}
	return undone, x.Store.SetBatchStatus(ctx, batchID, store.BatchUndone)
}

// Correction is the user saying a message belongs to another rule.
type Correction struct {
	MessageID       int64
	RightRuleID     int64 // 0 = keep in the inbox
	AlwaysForSender bool  // also store a user sender rule for the message's sender
}

// Correct fixes one message: it undoes what was done to it (newest first), applies the
// right rule's actions, or none for "keep", and records the correction, which deletes the
// learned sender rule for that sender and, with AlwaysForSender, stores a user sender rule
// instead. The new actions are one batch, whose id is returned. It also resolves a message
// waiting in Needs review.
func (x *Exec) Correct(ctx context.Context, c Correction) (batchID int64, err error) {
	msg, err := x.Store.Message(ctx, c.MessageID)
	if err != nil {
		return 0, err
	}
	acct, err := x.Store.Account(ctx, msg.AccountID)
	if err != nil {
		return 0, err
	}
	acts := []rules.Action{{Type: rules.ActKeep}}
	if c.RightRuleID != 0 {
		rule, err := x.Store.Rule(ctx, acct.UserID, c.RightRuleID)
		if err != nil {
			return 0, err
		}
		acts = rule.Actions
	}
	defer x.Accounts.Lock(msg.AccountID)()

	row, err := x.Store.ActivityFor(ctx, msg.ID)
	if err != nil {
		return 0, err
	}
	if batchID, err = x.Store.CreateBatch(ctx, store.BatchCorrection, store.BatchRunning, x.now()); err != nil {
		return 0, err
	}
	status := store.BatchFailed
	defer func() { err = errors.Join(err, x.Store.SetBatchStatus(context.WithoutCancel(ctx), batchID, status)) }()

	for _, a := range slices.Backward(row.Actions) {
		if err := x.undo(ctx, a.ID); err != nil {
			return batchID, fmt.Errorf("undo action %d: %w", a.ID, err)
		}
	}
	if msg, err = x.Store.Message(ctx, msg.ID); err != nil { // the undo may have moved it
		return batchID, err
	}
	if _, err := x.Apply(ctx, DecisionRecord{MessageID: msg.ID, Ref: msg.Location()}, acts, batchID); err != nil {
		return batchID, err
	}

	example, err := json.Marshal(message.Summary{
		AccountID: msg.AccountID, From: msg.FromAddr, FromDomain: msg.FromDomain, To: msg.ToAddrs, Subject: msg.Subject,
		Body: msg.Snippet, ListID: msg.ListID, HasAttachment: msg.HasAttachment, SizeKB: float64(msg.Size) / 1024,
		ReceivedAt: time.Unix(msg.ReceivedAt, 0).UTC(), IsContact: msg.Signals.Contact, RepliedBefore: msg.Signals.RepliedBefore,
		IsBulk: msg.Signals.Bulk, IsNoreply: msg.Signals.Noreply, DMARC: msg.Signals.DMARC,
	})
	if err != nil {
		return batchID, fmt.Errorf("encode example: %w", err)
	}
	corr := store.Correction{MessageID: msg.ID, RightRuleID: c.RightRuleID, Example: string(example), CreatedAt: x.now()}
	if row.Decision != nil {
		corr.WrongRuleID = row.Decision.RuleID
	}
	if _, err := x.Store.AddCorrection(ctx, acct.UserID, corr, c.AlwaysForSender); err != nil {
		return batchID, err
	}
	status = store.BatchDone
	if row, err := x.Store.ActivityFor(ctx, msg.ID); err == nil {
		x.Hub.Publish(events.MessageProcessed, row)
	}
	return batchID, nil
}

// UndoSince reverses every action made at or after since that is still in effect, newest
// first: "undo the last hour". With ruleID it only takes the actions that rule led to. The
// run is recorded as one batch of kind undo, whose id is returned with how many actions
// were undone and how many could not be (their message is gone, or its account is not
// connected). One that cannot be undone does not stop the rest.
func (x *Exec) UndoSince(ctx context.Context, ruleID, since int64) (batchID int64, undone, failed int, err error) {
	acts, err := x.Store.DoneActionsSince(ctx, ruleID, since)
	if err != nil {
		return 0, 0, 0, err
	}
	if batchID, err = x.Store.CreateBatch(ctx, store.BatchUndo, store.BatchRunning, x.now()); err != nil {
		return 0, 0, 0, err
	}
	for _, a := range acts {
		if err := x.Undo(ctx, a.ID); err != nil {
			failed++
			continue
		}
		undone++
	}
	status := store.BatchDone
	if failed > 0 {
		status = store.BatchFailed
	}
	recCtx := context.WithoutCancel(ctx)
	return batchID, undone, failed, errors.Join(
		x.Store.SetBatchProgress(recCtx, batchID, len(acts), undone), x.Store.SetBatchStatus(recCtx, batchID, status))
}
