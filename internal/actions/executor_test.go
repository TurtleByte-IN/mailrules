package actions

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// accounts is the test Accounts: one mailbox for every account.
type accounts struct {
	mb    mail.Mailbox
	locks int
}

func (a *accounts) Mailbox(int64) (mail.Mailbox, error) {
	if a.mb == nil {
		return nil, errors.New("account is not connected")
	}
	return a.mb, nil
}

func (a *accounts) Lock(int64) func() { a.locks++; return func() {} }

// spy records every call the executor makes on the mailbox; a call that would change it
// is recorded only when the server accepted it.
type spy struct {
	*mailtest.Mailbox
	calls     []string
	storeErr  error // makes SetFlags fail
	ensureErr error // makes EnsureFolder fail, as a server that refuses CREATE would
}

func (s *spy) Flags(ctx context.Context, ref mail.MsgRef) ([]string, error) {
	s.calls = append(s.calls, "flags")
	return s.Mailbox.Flags(ctx, ref)
}

func (s *spy) EnsureFolder(ctx context.Context, name string) error {
	s.calls = append(s.calls, "ensure "+name)
	if s.ensureErr != nil {
		return s.ensureErr
	}
	return s.Mailbox.EnsureFolder(ctx, name)
}

func (s *spy) Move(ctx context.Context, ref mail.MsgRef, dest string) (mail.MsgRef, error) {
	moved, err := s.Mailbox.Move(ctx, ref, dest)
	if err == nil {
		s.calls = append(s.calls, "move "+dest)
	}
	return moved, err
}

func (s *spy) SetFlags(ctx context.Context, ref mail.MsgRef, add, remove []string) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	for _, f := range add {
		s.calls = append(s.calls, "store +"+f)
	}
	for _, f := range remove {
		s.calls = append(s.calls, "store -"+f)
	}
	return s.Mailbox.SetFlags(ctx, ref, add, remove)
}

// changes lists the calls that changed the mailbox.
func (s *spy) changes() []string {
	return slices.DeleteFunc(slices.Clone(s.calls), func(c string) bool {
		return !strings.HasPrefix(c, "move ") && !strings.HasPrefix(c, "store ")
	})
}

type env struct {
	t    *testing.T
	db   *sql.DB
	st   *store.Store
	user store.User
	acct store.Account
	mb   *spy
	raw  mail.Mailbox // the mailbox itself, for checking state without leaving a trace in the spy
	accs *accounts
	x    *Exec
}

var roleFolders = []store.Folder{{Name: "INBOX"}, {Name: "Trash", SpecialUse: mail.RoleTrash},
	{Name: "Archive", SpecialUse: mail.RoleArchive}, {Name: "Junk", SpecialUse: mail.RoleJunk}}

// newEnv returns an executor over a fake mailbox with live mode on (DryRunDefault false).
func newEnv(t *testing.T) *env {
	t.Helper()
	e := newStoreEnv(t)
	e.mb = &spy{Mailbox: mailtest.New(e.acct.ID)}
	for _, f := range roleFolders[1:] {
		e.mb.AddFolder(f.Name, f.SpecialUse)
	}
	e.accs.mb, e.raw = e.mb, e.mb.Mailbox
	return e
}

// newStoreEnv is newEnv without a mailbox, for tests that bring their own.
func newStoreEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, st: store.New(db), accs: &accounts{}}
	if e.user, err = e.st.CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	if e.acct, err = e.st.CreateAccount(ctx, make([]byte, 32), store.Account{UserID: e.user.ID, Label: "x", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "me"}, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SaveFolders(ctx, e.acct.ID, roleFolders); err != nil {
		t.Fatal(err)
	}
	e.x = &Exec{Store: e.st, Accounts: e.accs, Hub: events.NewHub(), Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	return e
}

func eml(id string) string {
	return "From: Shop <orders@shop.example>\r\nTo: me@example.test\r\nSubject: order " + id + "\r\nMessage-ID: <" + id + "@shop.example>\r\n\r\nbody\r\n"
}

// track creates the messages row for a delivered email, as the pipeline would.
func (e *env) track(ref mail.MsgRef, id string) DecisionRecord {
	e.t.Helper()
	m, _, err := e.st.IngestMessage(e.t.Context(), ref, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	m.MessageID, m.FromAddr, m.FromDomain, m.Subject = id+"@shop.example", "orders@shop.example", "shop.example", "order "+id
	if err := e.st.SaveMessageSummary(e.t.Context(), m); err != nil {
		e.t.Fatal(err)
	}
	return DecisionRecord{MessageID: m.ID, Ref: ref}
}

func (e *env) deliver(id string, flags ...string) DecisionRecord {
	e.t.Helper()
	return e.track(e.mb.Deliver("INBOX", eml(id), flags...), id)
}

// where returns the message's location as stored, and its flags there on the server.
func (e *env) where(messageID int64) (mail.MsgRef, []string) {
	e.t.Helper()
	m, err := e.st.Message(e.t.Context(), messageID)
	if err != nil {
		e.t.Fatal(err)
	}
	flags, err := e.raw.Flags(e.t.Context(), m.Location())
	if err != nil {
		e.t.Fatalf("the message is not where the store says (%+v): %v", m.Location(), err)
	}
	return m.Location(), flags
}

func (e *env) actions(messageID int64) []store.Action {
	e.t.Helper()
	acts, err := e.st.MessageActions(e.t.Context(), messageID)
	if err != nil {
		e.t.Fatal(err)
	}
	return acts
}

func act(spec ...string) []rules.Action {
	out := make([]rules.Action, len(spec))
	for i, s := range spec {
		out[i].Type, out[i].Folder, _ = strings.Cut(s, ":")
	}
	return out
}

// checkInvariants asserts the four safety invariants on what one Apply did.
func (e *env) checkInvariants(d DecisionRecord, initialFlags []string) {
	e.t.Helper()
	rows := e.actions(d.MessageID)
	// 1. No action without a before snapshot, taken before anything changed.
	if len(e.mb.calls) > 0 && (e.mb.calls[0] != "flags" || len(e.mb.changes()) > 0 && !slices.Equal(rows[0].Before.Flags, initialFlags)) {
		e.t.Errorf("the flags were not snapshotted first: calls %v, first before %+v", e.mb.calls, rows[0].Before)
	}
	changed := 0
	for _, r := range rows {
		if r.Before.Folder == "" || r.Before.UID == 0 || r.Before.UIDValidity == 0 {
			e.t.Errorf("action %d (%s, %s) has no before snapshot: %+v", r.ID, r.Kind, r.Status, r.Before)
		}
		if r.After != nil && !reflect.DeepEqual(*r.After, r.Before) {
			changed++
			if r.Status != store.ActionDone {
				e.t.Errorf("action %d changed the mailbox but is %s", r.ID, r.Status)
			}
		}
	}
	// 4. Every mailbox change has an actions row (and no row claims a change that did not happen).
	if got := len(e.mb.changes()); got != changed {
		e.t.Errorf("%d mailbox changes (%v) but %d action rows record one", got, e.mb.changes(), changed)
	}
	for _, c := range e.mb.calls {
		// 2. The executor never deletes: it has no way to expunge, and never sets \Deleted.
		if strings.Contains(c, `\Deleted`) {
			e.t.Errorf("the executor set \\Deleted: %v", e.mb.calls)
		}
	}
	for _, r := range rows {
		// 3. Trash is always a move: to MailRules Trash when the row names it, otherwise to
		// the folder the server marks as trash.
		want := "Trash"
		if r.Folder != "" {
			want = TrashFolder
		}
		if r.Kind == rules.ActTrash && r.Status == store.ActionDone && (r.Folder != "" && r.Folder != TrashFolder ||
			r.After.Folder != want || !slices.Contains(e.mb.calls, "move "+want)) {
			e.t.Errorf("trash did not move to %s: %+v, calls %v", want, r.After, e.mb.calls)
		}
	}
}

func TestApply(t *testing.T) {
	tests := []struct {
		name       string
		trashOff   bool // trash_to_folder off: trash goes to the server's Trash
		flags      []string
		acts       []rules.Action
		wantCalls  []string
		wantFolder string
		wantFlags  []string
	}{
		{"move creates the folder and moves", false, nil, act("move:Food"), []string{"flags", "ensure Food", "move Food"}, "Food", nil},
		{"trash makes MailRules Trash and moves there", false, nil, act("trash"), []string{"flags", "ensure " + TrashFolder, "move " + TrashFolder}, TrashFolder, nil},
		{"with the setting off, trash is a move to the trash folder", true, nil, act("trash"), []string{"flags", "move Trash"}, "Trash", nil},
		{"archive", false, nil, act("archive"), []string{"flags", "move Archive"}, "Archive", nil},
		{"junk goes to Junk with the setting on", false, nil, act("junk"), []string{"flags", "move Junk"}, "Junk", nil},
		{"junk goes to Junk with the setting off", true, nil, act("junk"), []string{"flags", "move Junk"}, "Junk", nil},
		{"flag and read", false, nil, act("flag", "read"), []string{"flags", `store +\Flagged`, `store +\Seen`}, "INBOX", []string{`\Flagged`, `\Seen`}},
		{"unflag and unread", false, []string{`\Flagged`, `\Seen`, `\Answered`}, act("unflag", "unread"), []string{"flags", `store -\Flagged`, `store -\Seen`}, "INBOX", []string{`\Answered`}},
		{"keep changes nothing", false, []string{`\Seen`}, act("keep"), []string{"flags"}, "INBOX", []string{`\Seen`}},
		{"flags after a move land on the moved message", false, nil, act("move:Food", "read", "flag"),
			[]string{"flags", "ensure Food", "move Food", `store +\Seen`, `store +\Flagged`}, "Food", []string{`\Flagged`, `\Seen`}},
		{"a move to where it already is does nothing", false, nil, act("move:INBOX", "read"), []string{"flags", "ensure INBOX", `store +\Seen`}, "INBOX", []string{`\Seen`}},
		{"review adds the keyword", false, nil, act(KindReview), []string{"flags", "store +" + ReviewKeyword}, "INBOX", []string{ReviewKeyword}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.trashOff {
				if err := e.st.SetTrashToFolder(t.Context(), false); err != nil {
					t.Fatal(err)
				}
			}
			d := e.deliver("1", tt.flags...)
			d.DecisionID = 0
			recs, err := e.x.Apply(t.Context(), d, tt.acts, 0)
			if err != nil || len(recs) != len(tt.acts) {
				t.Fatalf("Apply = %d records, %v", len(recs), err)
			}
			if !slices.Equal(e.mb.calls, tt.wantCalls) {
				t.Errorf("mailbox calls = %v, want %v", e.mb.calls, tt.wantCalls)
			}
			ref, flags := e.where(d.MessageID)
			if ref.Folder != tt.wantFolder || !slices.Equal(flags, tt.wantFlags) {
				t.Errorf("message is in %s with %v, want %s with %v", ref.Folder, flags, tt.wantFolder, tt.wantFlags)
			}
			rows := e.actions(d.MessageID)
			for i, r := range rows {
				// The row keeps the kind; a trash to MailRules Trash names that folder.
				wantRowFolder := tt.acts[i].Folder
				if tt.acts[i].Type == rules.ActTrash && !tt.trashOff {
					wantRowFolder = TrashFolder
				}
				if r.Status != store.ActionDone || r.Kind != tt.acts[i].Type || r.Folder != wantRowFolder || r.After == nil || r.Error != "" || r.ID != recs[i].ID {
					t.Errorf("row %d = %+v", i, r)
				}
			}
			// The last after snapshot is the real state; each action starts where the previous ended.
			last := rows[len(rows)-1].After
			if last.Ref(ref.AccountID) != ref || !slices.Equal(last.Flags, flags) {
				t.Errorf("last after = %+v, real state %+v %v", last, ref, flags)
			}
			for i := 1; i < len(rows); i++ {
				if !reflect.DeepEqual(rows[i].Before, *rows[i-1].After) {
					t.Errorf("row %d starts at %+v, the one before ended at %+v", i, rows[i].Before, rows[i-1].After)
				}
			}
			initial := slices.Clone(tt.flags)
			slices.Sort(initial)
			e.checkInvariants(d, initial)
		})
	}
}

func TestApplyFailures(t *testing.T) {
	tests := []struct {
		name         string
		setup        func(e *env, d *DecisionRecord)
		acts         []rules.Action
		wantErr      error  // matched with errors.Is; nil = any error
		wantErrText  string // substring of the failed row's error
		wantStatuses []string
		wantFolder   string
	}{
		{"a failure stops the remaining actions", func(e *env, _ *DecisionRecord) { e.mb.storeErr = mail.ErrConnection },
			act("move:Food", "read", "flag"), mail.ErrConnection, "connection lost", []string{"done", "failed"}, "Food"},
		{"no trash folder with the setting off: fail, never guess or delete", func(e *env, _ *DecisionRecord) {
			if err := e.st.SaveFolders(e.t.Context(), e.acct.ID, roleFolders[:1]); err != nil {
				e.t.Fatal(err)
			}
			if err := e.st.SetTrashToFolder(e.t.Context(), false); err != nil {
				e.t.Fatal(err)
			}
		}, act("trash", "read"), nil, "no Trash folder", []string{"failed"}, "INBOX"},
		{"MailRules Trash cannot be made: fail, never fall back to Trash", func(e *env, _ *DecisionRecord) { e.mb.ensureErr = mail.ErrUnsupported },
			act("trash", "read"), mail.ErrUnsupported, `create folder "MailRules Trash"`, []string{"failed"}, "INBOX"},
		{"a server that cannot move", func(e *env, _ *DecisionRecord) { e.mb.Caps = mail.Caps{} },
			act("archive"), mail.ErrUnsupported, "unsupported capability", []string{"failed"}, "INBOX"},
		{"unknown action", nil, act("shred"), nil, `unknown action "shred"`, []string{"failed"}, "INBOX"},
		{"the message is gone: nothing to snapshot, nothing changed", func(_ *env, d *DecisionRecord) { d.Ref.UID = 99 },
			act("flag", "read"), mail.ErrNotFound, "read flags", []string{"failed"}, "INBOX"},
		{"the account is not connected", func(e *env, _ *DecisionRecord) { e.accs.mb = nil },
			act("flag"), nil, "not connected", []string{"failed"}, "INBOX"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			d := e.deliver("1")
			if tt.setup != nil {
				tt.setup(e, &d)
			}
			recs, err := e.x.Apply(t.Context(), d, tt.acts, 0)
			if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("Apply error = %v, want %v", err, tt.wantErr)
			}
			rows := e.actions(d.MessageID)
			var statuses []string
			for _, r := range rows {
				statuses = append(statuses, r.Status)
			}
			if !slices.Equal(statuses, tt.wantStatuses) || len(recs) != len(rows) {
				t.Fatalf("statuses = %v (%d records returned), want %v", statuses, len(recs), tt.wantStatuses)
			}
			if failed := rows[len(rows)-1]; !strings.Contains(failed.Error, tt.wantErrText) || failed.After != nil {
				t.Errorf("failed row = %+v, want error containing %q", failed, tt.wantErrText)
			}
			m, _ := e.st.Message(t.Context(), d.MessageID)
			if m.Location().Folder != tt.wantFolder {
				t.Errorf("message is in %s, want %s", m.Location().Folder, tt.wantFolder)
			}
			e.checkInvariants(d, nil)
		})
	}
}

func TestDryRun(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.x.DryRunDefault = true // MAILRULES_DRY_RUN's default; nothing stored yet
	d := e.deliver("1")
	all := act("move:Food", "trash", "archive", "junk", "flag", "unflag", "read", "unread", "keep", KindReview)

	batch, err := e.st.CreateBatch(ctx, store.BatchLive, store.BatchDone, 1)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := e.x.Apply(ctx, d, all, batch)
	if err != nil || len(recs) != len(all) {
		t.Fatalf("Apply = %d records, %v", len(recs), err)
	}
	for i, r := range e.actions(d.MessageID) {
		if r.Status != store.ActionDryRun || r.Kind != all[i].Type || r.After != nil || r.BatchID != batch || r.Before.Ref(e.acct.ID) != d.Ref {
			t.Errorf("row %d = %+v", i, r)
		}
	}
	// Nothing is touched: not one call reaches the server, not even a read.
	if len(e.mb.calls) != 0 {
		t.Errorf("dry-run called the mailbox: %v", e.mb.calls)
	}
	if ref, flags := e.where(d.MessageID); ref != d.Ref || len(flags) != 0 {
		t.Errorf("dry-run changed the message: %+v %v", ref, flags)
	}
	if folders, _ := e.mb.Folders(ctx); len(folders) != 4 {
		t.Errorf("dry-run created a folder: %+v", folders)
	}
	// trash_to_folder is on by default: the dry-run trash row says where it would have gone,
	// and MailRules Trash is neither made nor put on record.
	if r := e.actions(d.MessageID)[1]; r.Kind != rules.ActTrash || r.Folder != TrashFolder {
		t.Errorf("dry-run trash row = %+v, want folder %q", r, TrashFolder)
	}
	if stored, _ := e.st.Folders(ctx, e.acct.ID); len(stored) != len(roleFolders) {
		t.Errorf("dry-run put a folder on record: %+v", stored)
	}
	// Undoing a dry-run row has nothing to undo.
	if err := e.x.Undo(ctx, recs[0].ID); err != nil || e.actions(d.MessageID)[0].Status != store.ActionDryRun || len(e.mb.calls) != 0 {
		t.Errorf("undo of a dry-run row: %v, calls %v", err, e.mb.calls)
	}

	// The stored switch wins over the default, in both directions, without a restart.
	if err := e.st.SetDryRun(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.x.Apply(ctx, d, act("read"), 0); err != nil {
		t.Fatal(err)
	}
	if _, flags := e.where(d.MessageID); !slices.Equal(flags, []string{`\Seen`}) {
		t.Errorf("with dry-run off the action did not run: %v", flags)
	}
	e.x.DryRunDefault = false
	if err := e.st.SetDryRun(ctx, true); err != nil {
		t.Fatal(err)
	}
	e.mb.calls = nil
	if _, err := e.x.Apply(ctx, d, act("flag"), 0); err != nil || len(e.mb.calls) != 0 {
		t.Errorf("with dry-run switched on: %v, calls %v", err, e.mb.calls)
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE settings SET value = 'maybe' WHERE key = 'dry_run'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.x.Apply(ctx, d, act("flag"), 0); err == nil || len(e.mb.calls) != 0 {
		t.Errorf("an unreadable dry-run switch must stop the action, got %v, calls %v", err, e.mb.calls)
	}
}

// The Mailbox interface gives the executor no way to delete or expunge: the only
// mailbox-changing methods are the three the invariants are checked against.
func TestMailboxOffersNoDelete(t *testing.T) {
	readOnly := []string{"Capabilities", "Close", "Fetch", "FetchMany", "FetchSince", "FindByMessageID", "Flags", "Folders", "SentRecipients", "Status", "Watch"}
	changing := []string{"EnsureFolder", "Move", "SetFlags"}
	typ := reflect.TypeFor[mail.Mailbox]()
	for i := range typ.NumMethod() {
		if name := typ.Method(i).Name; !slices.Contains(readOnly, name) && !slices.Contains(changing, name) {
			t.Errorf("mail.Mailbox gained %s: decide whether it changes a mailbox, and if so cover it in the safety invariants", name)
		}
	}
}

func TestUndoBatch(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	batch, err := e.st.CreateBatch(ctx, store.BatchLive, store.BatchDone, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, live, cancel := e.x.Hub.Subscribe(0)
	defer cancel()
	a, b := e.deliver("a"), e.deliver("b", `\Seen`)
	for _, d := range []DecisionRecord{a, b} {
		if _, err := e.x.Apply(ctx, d, act("flag", "move:Food", "read", "keep"), batch); err != nil {
			t.Fatal(err)
		}
	}
	other := e.deliver("c")
	if _, err := e.x.Apply(ctx, other, act("flag"), 0); err != nil { // not in the batch
		t.Fatal(err)
	}

	n, err := e.x.UndoBatch(ctx, batch)
	if n != 8 || err != nil {
		t.Fatalf("UndoBatch = %d, %v", n, err)
	}
	for d, want := range map[DecisionRecord][]string{a: nil, b: {`\Seen`}} {
		ref, flags := e.where(d.MessageID)
		if ref.Folder != "INBOX" || !slices.Equal(flags, want) {
			t.Errorf("after batch undo: in %s with %v, want INBOX with %v", ref.Folder, flags, want)
		}
		for _, r := range e.actions(d.MessageID) {
			if r.Status != store.ActionUndone || r.UndoneAt == 0 {
				t.Errorf("action %+v not marked undone", r)
			}
		}
	}
	if _, flags := e.where(other.MessageID); !slices.Equal(flags, []string{`\Flagged`}) {
		t.Errorf("an action outside the batch was undone: %v", flags)
	}
	if got, _ := e.st.Batch(ctx, batch); got.Status != store.BatchUndone {
		t.Errorf("batch status = %s", got.Status)
	}
	if len(live) != 8 {
		t.Errorf("%d action.undone events, want 8", len(live))
	} else if ev := <-live; ev.Name != events.ActionUndone || ev.Data.(store.Action).Status != store.ActionUndone {
		t.Errorf("event = %+v", ev)
	}
	// Newest first: the last four mailbox calls restore message a, ending with its first action.
	if got := e.mb.changes(); got[len(got)-1] != `store -\Flagged` || got[len(got)-2] != "move INBOX" {
		t.Errorf("undo order = %v", got[len(got)-4:])
	}
	if n, err := e.x.UndoBatch(ctx, batch); n != 0 || err != nil {
		t.Errorf("undoing a batch twice = %d, %v", n, err)
	}
	if _, err := e.x.UndoBatch(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown batch: %v", err)
	}
	if e.accs.locks == 0 {
		t.Error("undo did not take the account lock")
	}

	// One message was deleted by the user: the others are still undone, the error says
	// which action could not be, and the batch is not marked undone.
	batch2, _ := e.st.CreateBatch(ctx, store.BatchLive, store.BatchDone, 2)
	gone, kept := e.deliver("gone"), e.deliver("kept")
	for _, d := range []DecisionRecord{gone, kept} {
		if _, err := e.x.Apply(ctx, d, act("move:Food"), batch2); err != nil {
			t.Fatal(err)
		}
	}
	ref, _ := e.where(gone.MessageID)
	if _, err := e.mb.Mailbox.Move(ctx, ref, "Trash"); err != nil { // not through the executor: "outside MailRules"
		t.Fatal(err)
	}
	n, err = e.x.UndoBatch(ctx, batch2)
	if n != 1 || !errors.Is(err, ErrGone) {
		t.Fatalf("UndoBatch with a missing message = %d, %v", n, err)
	}
	if ref, _ := e.where(kept.MessageID); ref.Folder != "INBOX" {
		t.Errorf("the other message was not restored: %+v", ref)
	}
	if got, _ := e.st.Batch(ctx, batch2); got.Status != store.BatchDone || e.actions(gone.MessageID)[0].Status != store.ActionDone {
		t.Errorf("batch %s, action %s; both must stay as they were", got.Status, e.actions(gone.MessageID)[0].Status)
	}
}

func TestUndoSingleActions(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	d := e.deliver("1")
	recs, err := e.x.Apply(ctx, d, act("flag", "move:Food"), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The flag action alone, although MailRules has since moved the message: it is found
	// at its last known location.
	if err := e.x.Undo(ctx, recs[0].ID); err != nil {
		t.Fatal(err)
	}
	if ref, flags := e.where(d.MessageID); ref.Folder != "Food" || len(flags) != 0 {
		t.Errorf("after undoing the flag: %s %v", ref.Folder, flags)
	}
	if err := e.x.Undo(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown action: %v", err)
	}
	// Not connected: the undo fails and the action stays done.
	e.accs.mb = nil
	if err := e.x.Undo(ctx, recs[1].ID); err == nil || e.actions(d.MessageID)[1].Status != store.ActionDone {
		t.Errorf("undo without a connection = %v", err)
	}
}

func TestCorrect(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	rule := func(name string, acts ...string) rules.Rule {
		r, err := e.st.CreateRule(ctx, rules.Rule{UserID: e.user.ID, Name: name, Intent: name, Actions: act(acts...), Enabled: true}, 1)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	food, receipts := rule("Food", "move:Food"), rule("Receipts", "move:Receipts", "read")

	// The pipeline sorted the message into Food and has learned the sender.
	d := e.deliver("1")
	d.DecisionID, _ = e.st.AddDecision(ctx, store.Decision{MessageID: d.MessageID, Stage: "decider", RuleID: food.ID, Confidence: 0.95, CreatedAt: 1}, store.StateDecided)
	if _, err := e.x.Apply(ctx, d, food.Actions, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.st.AddLearnedSenderRule(ctx, e.user.ID, "orders@shop.example", food.ID, 1); !ok || err != nil {
		t.Fatal(ok, err)
	}
	_, live, cancel := e.x.Hub.Subscribe(0)
	defer cancel()

	batch, err := e.x.Correct(ctx, Correction{MessageID: d.MessageID, RightRuleID: receipts.ID})
	if err != nil {
		t.Fatal(err)
	}
	if ref, flags := e.where(d.MessageID); ref.Folder != "Receipts" || !slices.Equal(flags, []string{`\Seen`}) {
		t.Errorf("after the correction: in %s with %v", ref.Folder, flags)
	}
	rows := e.actions(d.MessageID)
	if len(rows) != 3 || rows[0].Status != store.ActionUndone || rows[1].BatchID != batch || rows[2].BatchID != batch ||
		rows[1].Status != store.ActionDone || rows[1].DecisionID != 0 || rows[1].Before.Folder != "INBOX" {
		t.Errorf("actions = %+v", rows)
	}
	if b, _ := e.st.Batch(ctx, batch); b.Kind != store.BatchCorrection || b.Status != store.BatchDone {
		t.Errorf("batch = %+v", b)
	}
	var wrong, right int64
	var example string
	if err := e.db.QueryRowContext(ctx, `SELECT wrong_rule_id, right_rule_id, example FROM corrections WHERE message_id = ?`, d.MessageID).Scan(&wrong, &right, &example); err != nil ||
		wrong != food.ID || right != receipts.ID || !strings.Contains(example, `"Subject":"order 1"`) || !strings.Contains(example, "orders@shop.example") {
		t.Errorf("correction = wrong %d right %d example %s, %v", wrong, right, example, err)
	}
	if srs, _ := e.st.SenderRules(ctx, e.user.ID); len(srs) != 0 {
		t.Errorf("the learned sender rule survived: %+v", srs)
	}
	if m, _ := e.st.Message(ctx, d.MessageID); m.State != store.StateActed {
		t.Errorf("state = %s", m.State)
	}
	var names []string
	for len(live) > 0 {
		names = append(names, (<-live).Name)
	}
	if !slices.Equal(names, []string{events.ActionUndone, events.MessageProcessed}) || e.accs.locks != 1 {
		t.Errorf("events = %v, locks = %d", names, e.accs.locks)
	}

	// The correction is itself one batch: undoing it puts the mail back in the inbox, unread.
	if n, err := e.x.UndoBatch(ctx, batch); n != 2 || err != nil {
		t.Fatalf("undo of the correction batch = %d, %v", n, err)
	}
	if ref, flags := e.where(d.MessageID); ref.Folder != "INBOX" || len(flags) != 0 {
		t.Errorf("after undoing the correction: in %s with %v", ref.Folder, flags)
	}

	// "Keep in the inbox, always for this sender".
	batch, err = e.x.Correct(ctx, Correction{MessageID: d.MessageID, Always: rules.MatchAddress})
	if err != nil {
		t.Fatal(err)
	}
	rows = e.actions(d.MessageID)
	if last := rows[len(rows)-1]; last.Kind != rules.ActKeep || last.BatchID != batch || last.Status != store.ActionDone {
		t.Errorf("keep action = %+v", last)
	}
	srs, _ := e.st.SenderRules(ctx, e.user.ID)
	if len(srs) != 1 || srs[0].Verdict != rules.VerdictKeep || srs[0].Source != "user" || srs[0].Value != "orders@shop.example" {
		t.Errorf("sender rules = %+v", srs)
	}

	// Errors: unknown message, unknown rule, and mail the user has since deleted.
	if _, err := e.x.Correct(ctx, Correction{MessageID: 999}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown message: %v", err)
	}
	if _, err := e.x.Correct(ctx, Correction{MessageID: d.MessageID, RightRuleID: 999}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown rule: %v", err)
	}
	if _, err := e.x.Correct(ctx, Correction{MessageID: d.MessageID, RightRuleID: food.ID}); err != nil {
		t.Fatal(err)
	}
	ref, _ := e.where(d.MessageID)
	if _, err := e.mb.Mailbox.Move(ctx, ref, "Trash"); err != nil {
		t.Fatal(err)
	}
	batch, err = e.x.Correct(ctx, Correction{MessageID: d.MessageID, RightRuleID: receipts.ID})
	if b, _ := e.st.Batch(ctx, batch); !errors.Is(err, ErrGone) || b.Status != store.BatchFailed {
		t.Errorf("correcting mail that is gone: %v, batch %+v", err, b)
	}
}

// "Undo the last hour" takes what was done since a time and leaves older actions alone.
func TestUndoSince(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	now := time.Unix(1000, 0)
	e.x.Now = func() time.Time { return now }
	old, recent, gone := e.deliver("old"), e.deliver("recent"), e.deliver("gone")
	if _, err := e.x.Apply(ctx, old, act("move:Food"), 0); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(5000, 0)
	for _, d := range []DecisionRecord{recent, gone} {
		if _, err := e.x.Apply(ctx, d, act("move:Food", "flag"), 0); err != nil {
			t.Fatal(err)
		}
	}
	ref, _ := e.where(recent.MessageID)
	tag := DecisionRecord{MessageID: recent.MessageID, Ref: ref}
	if _, err := e.x.Apply(ctx, tag, []rules.Action{{Type: KindReview}}, 0); err != nil { // the review tag is not undone
		t.Fatal(err)
	}
	ref, _ = e.where(gone.MessageID)
	if _, err := e.raw.Move(ctx, ref, "Archive"); err != nil { // filed by hand since
		t.Fatal(err)
	}

	batch, undone, failed, err := e.x.UndoSince(ctx, 0, 4000)
	if err != nil || undone != 2 || failed != 2 {
		t.Fatalf("UndoSince = %d undone, %d failed, %v", undone, failed, err)
	}
	if b, _ := e.st.Batch(ctx, batch); b.Kind != store.BatchUndo || b.Status != store.BatchFailed || b.Total != 4 || b.Done != 2 {
		t.Errorf("undo batch = %+v", b)
	}
	if ref, flags := e.where(recent.MessageID); ref.Folder != "INBOX" || !slices.Equal(flags, []string{ReviewKeyword}) {
		t.Errorf("the recent message is in %s with %v", ref.Folder, flags)
	}
	if ref, _ := e.where(old.MessageID); ref.Folder != "Food" {
		t.Errorf("an action from before the time was undone: the message is in %s", ref.Folder)
	}
	if batch, undone, failed, err := e.x.UndoSince(ctx, 77, 0); err != nil || undone != 0 || failed != 0 || batch == 0 {
		t.Errorf("UndoSince for a rule with no actions = batch %d, %d, %d, %v", batch, undone, failed, err)
	}
}
