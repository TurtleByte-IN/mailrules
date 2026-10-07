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

// NoFolderError means an archive, trash or junk action has nowhere to go: the account has
// no folder the server marks for that use, and MailRules never guesses one or creates one.
// Its text is what the action's row records ("no Archive folder"). Trash only meets it
// with trash_to_folder off: otherwise it goes to TrashFolder, which MailRules makes.
type NoFolderError struct{ Role string } // Archive | Trash | Junk

func (e *NoFolderError) Error() string { return "no " + e.Role + " folder" }

// TrashFolder is where a trash action moves mail while the trash_to_folder setting is on:
// an ordinary folder, made on first use and given no special-use role, so the provider
// never empties it the way it empties Trash.
const TrashFolder = "MailRules Trash"

// ErrGone means an action cannot be undone because its message is no longer where
// MailRules left it. The HTTP layer answers 409 with this text.
var ErrGone = errors.New("message was moved or deleted outside MailRules")

// ErrTooOld means an action was made more than store.UndoDays ago and can no longer be
// undone. The HTTP layer answers 409 too_old.
var ErrTooOld = errors.New("the action is too old to undo")

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
}

func isMove(kind string) bool { return kind == rules.ActMove || moveRole[kind] != "" }

// Apply runs acts in order on one message. In dry-run (checked here and nowhere else) it
// only writes a dry_run row per action and never contacts the server. Otherwise it reads
// the message's flags once, and for each action writes its row with the before snapshot
// first, then changes the mailbox, then stores the outcome, so there is no change without
// a row. A failure stops the remaining actions and is returned; whoever asked decides
// about a retry. With trash_to_folder on, a trash row names TrashFolder as its folder,
// in dry-run too, so what it says it did (or would do) is where the mail went.
func (x *Exec) Apply(ctx context.Context, d DecisionRecord, acts []rules.Action, batchID int64) ([]ActionRecord, error) {
	dry, err := x.Store.DryRun(ctx, x.DryRunDefault)
	if err != nil {
		return nil, err
	}
	if acts, err = x.trashDestination(ctx, acts); err != nil {
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

// trashDestination returns acts with every trash action pointed at TrashFolder while
// trash_to_folder is on, and acts as they are otherwise. The setting is read only when
// there is something to trash; one that cannot be read stops the actions, as dry-run does.
func (x *Exec) trashDestination(ctx context.Context, acts []rules.Action) ([]rules.Action, error) {
	if !slices.ContainsFunc(acts, func(a rules.Action) bool { return a.Type == rules.ActTrash }) {
		return acts, nil
	}
	on, err := x.Store.TrashToFolder(ctx)
	if err != nil || !on {
		return acts, err
	}
	out := slices.Clone(acts)
	for i := range out {
		if out[i].Type == rules.ActTrash {
			out[i].Folder = TrashFolder
		}
	}
	return out, nil
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
		var err error
		switch role := moveRole[a.Type]; {
		case a.Type == rules.ActTrash && dest != "":
			// trash_to_folder: MailRules' own folder, made on first use. When it cannot be
			// made the action fails: falling back to Trash would defeat the setting.
			err = x.ensureFolder(ctx, mb, accountID, dest)
		case role != "":
			// Never guess a folder, and never create one: junk, archive, and trash with
			// trash_to_folder off, only ever move to the folder the server marks for that use.
			dest, err = x.roleFolder(ctx, accountID, role)
		default:
			err = mb.EnsureFolder(ctx, dest)
		}
		if err != nil {
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
	return "", &NoFolderError{Role: strings.TrimPrefix(role, `\`)}
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
		if err := x.ensureFolder(ctx, mb, accountID, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureFolder creates a folder on the server if it is missing and puts it on record in
// the account's folder list, as discovery would, with no special-use role.
func (x *Exec) ensureFolder(ctx context.Context, mb mail.Mailbox, accountID int64, name string) error {
	if err := mb.EnsureFolder(ctx, name); err != nil {
		return fmt.Errorf("create folder %q: %w", name, err)
	}
	return x.Store.AddFolder(context.WithoutCancel(ctx), accountID, name)
}

// Undo reverses one action: a move goes back to the folder it came from, a flag change
// goes back to what the before snapshot says. Undoing an action that is already undone,
// or that never changed the mailbox (dry-run, failed), does nothing. An action made more
// than store.UndoDays ago is refused with ErrTooOld. When the message is
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
	if x.now()-a.CreatedAt > store.UndoDays*24*3600 {
		return ErrTooOld
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
// returned, and the batch is marked undone only when every action was. A batch that never
// changed a mailbox (all its actions were recorded in dry-run, or failed) has nothing to
// undo: that is a no-op, and the batch stays as it is.
func (x *Exec) UndoBatch(ctx context.Context, batchID int64) (int, error) {
	if _, err := x.Store.Batch(ctx, batchID); err != nil {
		return 0, err
	}
	acts, err := x.Store.BatchActions(ctx, batchID)
	if err != nil {
		return 0, err
	}
	undone := 0
	changed := false // the batch changed a mailbox at some point
	var errs []error
	for _, a := range slices.Backward(acts) {
		changed = changed || a.Status == store.ActionDone || a.Status == store.ActionUndone
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
	if !changed {
		return 0, nil
	}
	return undone, x.Store.SetBatchStatus(ctx, batchID, store.BatchUndone)
}

// Correction is the user saying a message belongs to another rule.
type Correction struct {
	MessageID   int64
	RightRuleID int64 // 0 = keep in the inbox
	// Always also stores a user sender rule, so future mail goes the same way: for the
	// message's sender (rules.MatchAddress) or its whole domain (rules.MatchDomain). "" = no.
	Always string
	Review bool // an answer given in Needs review, as opposed to a fix made from the feed
}

// Correct fixes one message: it undoes what was done to it (newest first), applies the
// right rule's actions, or none for "keep", and records the correction, which deletes the
// learned sender rule for that sender and, with Always, stores a user sender rule
// instead. The new actions are one batch, whose id is returned. It also resolves a message
// waiting in Needs review. When the new actions cannot be applied, what was undone stays
// undone; the row as it now is goes out as message.processed all the same, after the
// action.undone events, so a screen that shows the email does not keep the old picture.
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
	defer func() {
		recCtx := context.WithoutCancel(ctx)
		err = errors.Join(err, x.Store.SetBatchStatus(recCtx, batchID, status))
		if row, rowErr := x.Store.ActivityFor(recCtx, msg.ID); rowErr == nil {
			x.Hub.Publish(events.MessageProcessed, row)
		}
	}()

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
	if c.Review {
		corr.Kind = store.CorrectionReview
	}
	if _, err := x.Store.AddCorrection(ctx, acct.UserID, corr, c.Always); err != nil {
		return batchID, err
	}
	status = store.BatchDone
	return batchID, nil
}

// UndoSince reverses every action made at or after since that is still in effect, newest
// first: "undo the last hour". With ruleID it only takes the actions that rule led to. The
// run is recorded as one batch of kind undo, whose id is returned with how many actions
// were undone and how many could not be (their message is gone, or its account is not
// connected). One that cannot be undone does not stop the rest.
func (x *Exec) UndoSince(ctx context.Context, ruleID, since int64) (batchID int64, undone, failed int, err error) {
	// What is older than the undo window is not looked at: it could only fail as too old.
	acts, err := x.Store.DoneActionsSince(ctx, ruleID, max(since, x.now()-store.UndoDays*24*3600))
	if err != nil {
		return 0, 0, 0, err
	}
	batchID, undone, failed, _, err = x.undoAll(ctx, acts)
	return batchID, undone, failed, err
}

// UndoMessage reverses everything still in effect on one message, newest first, as one
// batch of kind undo: "undo this email" in one step. An action that cannot be undone does
// not stop the others; why is the first such failure (ErrGone when the message is gone),
// nil when there was none.
func (x *Exec) UndoMessage(ctx context.Context, messageID int64) (batchID int64, undone, failed int, why, err error) {
	all, err := x.Store.MessageActions(ctx, messageID)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	var acts []store.Action
	for _, a := range slices.Backward(all) {
		if a.Status == store.ActionDone { // as in "undo everything since"
			acts = append(acts, a)
		}
	}
	return x.undoAll(ctx, acts)
}

// undoAll undoes acts in the order given and records the run as one undo batch.
func (x *Exec) undoAll(ctx context.Context, acts []store.Action) (batchID int64, undone, failed int, why, err error) {
	if batchID, err = x.Store.CreateBatch(ctx, store.BatchUndo, store.BatchRunning, x.now()); err != nil {
		return 0, 0, 0, nil, err
	}
	for _, a := range acts {
		if err := x.Undo(ctx, a.ID); err != nil {
			failed++
			if why == nil {
				why = err
			}
			continue
		}
		undone++
	}
	status := store.BatchDone
	if failed > 0 {
		status = store.BatchFailed
	}
	recCtx := context.WithoutCancel(ctx)
	return batchID, undone, failed, why, errors.Join(
		x.Store.SetBatchProgress(recCtx, batchID, len(acts), undone), x.Store.SetBatchStatus(recCtx, batchID, status))
}
