// Package moved notices when the person moves an email MailRules filed out of the folder
// MailRules put it in, in another mail app: back to the Inbox, or to another folder. It
// runs only for a build whose module asks to know (ext.Moves); the free build never starts
// it.
//
// It costs no connection of its own. Each mailbox's supervisor calls Sweep now and then on
// the mailbox's one worker connection, between two emails, so nothing MailRules does to the
// mailbox (sorting, undo, a fix, a cleanup) happens while it looks. A sweep reads what
// MailRules filed in the last WatchDays and is still in effect, at most SweepLimit emails,
// grouped by the folder they were filed in: one UID SEARCH per folder tells which are still
// there (mail.Mailbox.Present). Only an email found gone costs more: a Message-ID search of
// the mailbox's folders, Inbox first, until it turns up, for at most LocateLimit emails a
// sweep. Each gone email is recorded once (store.RecordMove), so it is never searched for
// again. At most Concurrent sweeps run at a time in the process; a mailbox whose turn finds
// them all busy waits for its next one. Nothing here changes a mailbox: SEARCH reads no
// message and sets no flag.
package moved

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	// WatchDays is how long after MailRules filed an email a move of it is noticed.
	WatchDays = 7
	// SweepLimit is how many filed emails one sweep of a mailbox checks at most; the next
	// sweep goes on from there.
	SweepLimit = 500
	// LocateLimit is how many gone emails one sweep searches the folders for at most.
	LocateLimit = 20
	// Concurrent is how many sweeps run at once in the process.
	Concurrent = 4
	// SweepTimeout bounds one sweep, during which the mailbox's new mail waits.
	SweepTimeout = time.Minute
)

// Watcher sweeps mailboxes for emails the person moved. One serves every mailbox of the
// process; its zero value is not ready, use New.
type Watcher struct {
	Store *store.Store
	// Serves, when set, says whether a tenant's mailboxes are swept; nil = every mailbox.
	Serves func(ctx context.Context, tenantID int64) bool
	// Found, when set, is called once for each move of the person recorded.
	Found func(m store.Move)
	Now   func() time.Time // nil = time.Now

	slots  chan struct{}
	mu     sync.Mutex
	cursor map[int64]int64 // per mailbox: the last filing action a sweep checked
}

// New returns a watcher over st.
func New(st *store.Store) *Watcher {
	return &Watcher{Store: st, slots: make(chan struct{}, Concurrent), cursor: map[int64]int64{}}
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Sweep looks at what MailRules filed in acct, on mb, and records each email it finds gone
// from where MailRules left it. The caller holds the mailbox's processor between emails
// (worker.Supervisor), so no MailRules action moves mail meanwhile.
func (w *Watcher) Sweep(ctx context.Context, mb mail.Mailbox, acct store.Account) error {
	if w.Serves != nil && !w.Serves(ctx, acct.TenantID) {
		return nil
	}
	select {
	case w.slots <- struct{}{}:
		defer func() { <-w.slots }()
	default:
		return nil // busy: this mailbox's next turn
	}
	ctx, cancel := context.WithTimeout(ctx, SweepTimeout)
	defer cancel()

	w.mu.Lock()
	after := w.cursor[acct.ID]
	w.mu.Unlock()
	now := w.now()
	filed, err := w.Store.FiledSince(ctx, acct.ID, now.Add(-WatchDays*24*time.Hour).Unix(), after, SweepLimit)
	if err != nil {
		return err
	}
	next := int64(0) // fewer than a full sweep's worth: start again from the oldest next time
	if len(filed) == SweepLimit {
		next = filed[len(filed)-1].ActionID
	}
	filed = slices.DeleteFunc(filed, func(f store.Filed) bool {
		return f.After.Folder == f.Before.Folder || f.After.Folder == acct.WatchFolder
	})

	gone, err := w.gone(ctx, mb, filed)
	if err != nil {
		return err
	}
	var folders []store.Folder
	if len(gone) > 0 {
		if folders, err = w.Store.Folders(ctx, acct.ID); err != nil {
			return err
		}
	}
	for i, f := range gone {
		if i == LocateLimit {
			// The rest are looked for on the next sweep, which starts before them.
			next = f.ActionID - 1
			break
		}
		if err := w.record(ctx, mb, acct, folders, f, now); err != nil {
			return err
		}
	}
	w.mu.Lock()
	w.cursor[acct.ID] = next
	w.mu.Unlock()
	return nil
}

// gone returns the filed emails no longer where MailRules left them, oldest first.
func (w *Watcher) gone(ctx context.Context, mb mail.Mailbox, filed []store.Filed) ([]store.Filed, error) {
	type place struct {
		folder   string
		validity uint32
	}
	byPlace := map[place][]store.Filed{}
	var places []place
	for _, f := range filed {
		p := place{f.After.Folder, f.After.UIDValidity}
		if byPlace[p] == nil {
			places = append(places, p)
		}
		byPlace[p] = append(byPlace[p], f)
	}
	var out []store.Filed
	for _, p := range places {
		fs := byPlace[p]
		uids := make([]uint32, len(fs))
		for i, f := range fs {
			uids[i] = f.After.UID
		}
		present, err := mb.Present(ctx, p.folder, p.validity, uids)
		switch {
		case errors.Is(err, mail.ErrNoFolder):
			out = append(out, fs...) // the folder is gone, and the mail with it
		case errors.Is(err, mail.ErrNotFound):
			// The folder was rebuilt on the server: its UIDs are new, so look by Message-ID.
			for _, f := range fs {
				if _, err := mb.FindByMessageID(ctx, p.folder, f.MsgID); errors.Is(err, mail.ErrNotFound) {
					out = append(out, f)
				} else if err != nil {
					return nil, fmt.Errorf("look for a filed email in %q: %w", p.folder, err)
				}
			}
		case err != nil:
			return nil, fmt.Errorf("check filed mail in %q: %w", p.folder, err)
		default:
			for _, f := range fs {
				if !slices.Contains(present, f.After.UID) {
					out = append(out, f)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b store.Filed) int { return cmp.Compare(a.ActionID, b.ActionID) })
	return out, nil
}

// searchOrder is the order gone mail is looked for in: the watched folder, then ordinary
// folders, then Junk, Archive and the trash. Sent and Drafts never take received mail.
// The folder it was filed in is left out: it is not there.
func searchOrder(acct store.Account, folders []store.Folder, filed string) []store.Folder {
	rank := func(f store.Folder) int {
		switch {
		case f.Name == acct.WatchFolder:
			return 0
		case f.SpecialUse == "" && f.Name != actions.TrashFolder:
			return 1
		case f.SpecialUse == mail.RoleJunk:
			return 2
		case f.SpecialUse == mail.RoleArchive:
			return 3
		default:
			return 4
		}
	}
	out := []store.Folder{{Name: acct.WatchFolder}}
	for _, f := range folders {
		if f.Name != acct.WatchFolder && f.Name != filed && f.SpecialUse != mail.RoleSent && f.SpecialUse != mail.RoleDrafts {
			out = append(out, f)
		}
	}
	slices.SortStableFunc(out, func(a, b store.Folder) int { return cmp.Compare(rank(a), rank(b)) })
	return out
}

// record finds where a gone email is and writes its move. It is the person's move when it
// is in the watched folder or an ordinary folder, and not where MailRules itself left a
// row of the same email; in Junk, an archive, the trash or no folder at all, the person
// threw it away or was done with it, and nothing is learned from that.
func (w *Watcher) record(ctx context.Context, mb mail.Mailbox, acct store.Account, folders []store.Folder, f store.Filed, now time.Time) error {
	m := store.Move{TenantID: acct.TenantID, AccountID: acct.ID, MessageID: f.MessageID, ActionID: f.ActionID, From: f.From,
		RuleID: f.RuleID, RuleName: f.RuleName, Filed: f.After.Folder, FiledAt: f.FiledAt, CreatedAt: now.Unix()}
	for _, folder := range searchOrder(acct, folders, f.After.Folder) {
		ref, err := mb.FindByMessageID(ctx, folder.Name, f.MsgID)
		if errors.Is(err, mail.ErrNotFound) || errors.Is(err, mail.ErrNoFolder) {
			continue
		}
		if err != nil {
			return fmt.Errorf("look for a moved email in %q: %w", folder.Name, err)
		}
		m.Chosen, m.Inbox = folder.Name, folder.Name == acct.WatchFolder
		ours, err := w.Store.LeftByMailRules(ctx, acct.ID, ref.Folder, ref.UIDValidity, ref.UID, f.MessageID)
		if err != nil {
			return err
		}
		m.Person = !ours && (m.Inbox || (folder.SpecialUse == "" && folder.Name != actions.TrashFolder))
		break
	}
	id, err := w.Store.RecordMove(ctx, m)
	if err != nil || id == 0 {
		return err
	}
	slog.InfoContext(ctx, "an email MailRules filed was moved outside MailRules", "account", acct.ID, "action", f.ActionID,
		"from_folder", m.Filed, "to_folder", m.Chosen, "learnable", m.Person)
	if m.Person && w.Found != nil {
		m.ID = id
		w.Found(m)
	}
	return nil
}
