package moved

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// world is one mailbox, in the fake server and in the store, with the folders a person
// files into and the ones they throw mail away in.
type world struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	mb    *mailtest.Mailbox
	acct  store.Account
	rule  rules.Rule
	w     *Watcher
	found []store.Move
	n     int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := t.Context()
	st, _ := storetest.New(t)
	u, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := st.CreateAccount(ctx, store.Account{UserID: u.ID, Label: "me", Preset: "generic", Host: "h", Port: 993,
		TLSMode: "implicit", Username: "me@example.test", WatchFolder: "INBOX"}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := st.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "Receipts", Intent: "receipts", Enabled: true,
		Actions: []rules.Action{{Type: rules.ActMove, Folder: "Receipts"}}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	mb := mailtest.New(acct.ID)
	folders := []store.Folder{{Name: "INBOX"}, {Name: "Receipts"}, {Name: "Later"}, {Name: actions.TrashFolder},
		{Name: "Trash", SpecialUse: mail.RoleTrash}, {Name: "Archive", SpecialUse: mail.RoleArchive},
		{Name: "Junk", SpecialUse: mail.RoleJunk}, {Name: "Sent", SpecialUse: mail.RoleSent}}
	for _, f := range folders[1:] {
		mb.AddFolder(f.Name, f.SpecialUse)
	}
	if err := st.SaveFolders(ctx, acct.ID, folders); err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ctx: ctx, st: st, mb: mb, acct: acct, rule: rule}
	w.w = New(st)
	w.w.Found = func(m store.Move) { w.found = append(w.found, m) }
	return w
}

// file has the rule file one email from sender into Receipts, as the executor does, and
// returns where it is now and its row.
func (w *world) file(sender string) (mail.MsgRef, store.Message) {
	w.t.Helper()
	w.n++
	eml := fmt.Sprintf("From: %s\r\nSubject: Receipt %d\r\nMessage-ID: <r%d@shop.test>\r\n\r\nthanks\r\n", sender, w.n, w.n)
	inbox := w.mb.Deliver("INBOX", eml)
	m, _, err := w.st.IngestMessage(w.ctx, inbox, time.Now().Unix())
	if err != nil {
		w.t.Fatal(err)
	}
	m.MessageID, m.FromAddr = fmt.Sprintf("r%d@shop.test", w.n), sender
	if err := w.st.SaveMessageSummary(w.ctx, m); err != nil {
		w.t.Fatal(err)
	}
	dec, err := w.st.AddDecision(w.ctx, store.Decision{MessageID: m.ID, Stage: "condition", RuleID: w.rule.ID, RuleName: "Receipts",
		CreatedAt: time.Now().Unix()}, store.StateActed)
	if err != nil {
		w.t.Fatal(err)
	}
	a := store.Action{DecisionID: dec, MessageID: m.ID, AccountID: w.acct.ID, Kind: rules.ActMove, Folder: "Receipts",
		Before: store.Snapshot{Folder: "INBOX", UIDValidity: inbox.UIDValidity, UID: inbox.UID}, Status: store.ActionFailed, CreatedAt: time.Now().Unix()}
	if a.ID, err = w.st.InsertAction(w.ctx, a); err != nil {
		w.t.Fatal(err)
	}
	to, err := w.mb.Move(w.ctx, inbox, "Receipts")
	if err != nil {
		w.t.Fatal(err)
	}
	a.Status, a.After = store.ActionDone, &store.Snapshot{Folder: to.Folder, UIDValidity: to.UIDValidity, UID: to.UID}
	if err := w.st.FinishAction(w.ctx, a, store.FollowUps{Notices: true}); err != nil {
		w.t.Fatal(err)
	}
	return to, m
}

func (w *world) sweep() {
	w.t.Helper()
	if err := w.w.Sweep(w.ctx, w.mb, w.acct); err != nil {
		w.t.Fatal(err)
	}
}

// recorded is every move the store holds, the person's or not.
func (w *world) recorded() []store.Move {
	w.t.Helper()
	all, err := w.st.WaitingMoves(w.ctx, 1<<40, 1000)
	if err != nil {
		w.t.Fatal(err)
	}
	return all
}

// What a sweep makes of an email the person moved somewhere, or threw away.
func TestSweep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		act    func(w *world, at mail.MsgRef)
		chosen string // "" with person false: recorded as in no folder; "-" = nothing recorded
		inbox  bool
		person bool
	}{
		{name: "still where MailRules filed it", act: func(*world, mail.MsgRef) {}, chosen: "-"},
		{name: "moved back to the Inbox", act: moveTo("INBOX"), chosen: "INBOX", inbox: true, person: true},
		{name: "moved to another folder", act: moveTo("Later"), chosen: "Later", person: true},
		{name: "deleted", act: moveTo("Trash"), chosen: "Trash"},
		{name: "put in MailRules' own trash folder", act: moveTo(actions.TrashFolder), chosen: actions.TrashFolder},
		{name: "archived", act: moveTo("Archive"), chosen: "Archive"},
		{name: "marked as junk", act: moveTo("Junk"), chosen: "Junk"},
		{name: "expunged", act: func(w *world, at mail.MsgRef) {
			if err := w.mb.SetFlags(w.ctx, at, []string{`\Deleted`}, nil); err != nil {
				w.t.Fatal(err)
			}
		}, chosen: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			at, m := w.file("orders@shop.test")
			tc.act(w, at)
			w.sweep()
			w.sweep() // a second sweep finds nothing more
			if tc.chosen == "-" {
				if got, _ := w.st.FiledSince(w.ctx, w.acct.ID, 0, 0, 10); len(got) != 1 || len(w.found) != 0 {
					t.Errorf("still filed = %v; found %v", got, w.found)
				}
				return
			}
			got, err := w.st.FiledSince(w.ctx, w.acct.ID, 0, 0, 10)
			if err != nil || len(got) != 0 {
				t.Errorf("still to check after the sweep: %v, %v", got, err)
			}
			told := w.recorded() // the moves waiting for the module: the person's only
			if !tc.person {
				if len(told) != 0 || len(w.found) != 0 {
					t.Errorf("the module would be told %+v (found %+v)", told, w.found)
				}
				return
			}
			if len(told) != 1 || len(w.found) != 1 {
				t.Fatalf("told %+v, found %+v", told, w.found)
			}
			mv := told[0]
			if mv.MessageID != m.ID || mv.From != "orders@shop.test" || mv.Filed != "Receipts" || mv.Chosen != tc.chosen ||
				mv.Inbox != tc.inbox || mv.RuleID != w.rule.ID || mv.RuleName != "Receipts" || mv.Mailbox != "me@example.test" ||
				mv.Count != 1 || w.found[0].ID != mv.ID {
				t.Errorf("move = %+v", mv)
			}
		})
	}
}

func moveTo(folder string) func(w *world, at mail.MsgRef) {
	return func(w *world, at mail.MsgRef) {
		if _, err := w.mb.Move(w.ctx, at, folder); err != nil {
			w.t.Fatal(err)
		}
	}
}

// An email MailRules itself moved on under another row (a cleanup sorting the folder) is
// not the person's move.
func TestSweepLeavesMailRulesOwnMovesAlone(t *testing.T) {
	w := newWorld(t)
	at, _ := w.file("orders@shop.test")
	to, err := w.mb.Move(w.ctx, at, "Later")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := w.st.IngestMessage(w.ctx, at, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	a := store.Action{MessageID: other.ID, AccountID: w.acct.ID, Kind: rules.ActMove, Folder: "Later",
		Before: store.Snapshot{Folder: at.Folder, UIDValidity: at.UIDValidity, UID: at.UID}, Status: store.ActionFailed, CreatedAt: time.Now().Unix()}
	if a.ID, err = w.st.InsertAction(w.ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Status, a.After = store.ActionDone, &store.Snapshot{Folder: to.Folder, UIDValidity: to.UIDValidity, UID: to.UID}
	if err := w.st.FinishAction(w.ctx, a, store.FollowUps{Notices: true}); err != nil {
		t.Fatal(err)
	}
	w.sweep()
	if len(w.found) != 0 || len(w.recorded()) != 0 {
		t.Errorf("found %+v", w.found)
	}
}

// A folder rebuilt on the server gives its mail new UIDs: what is still there by
// Message-ID has not moved.
func TestSweepRebuiltFolder(t *testing.T) {
	w := newWorld(t)
	w.file("orders@shop.test")
	w.mb.AddFolder("Receipts", "") // rebuilt empty, with a new UIDVALIDITY
	w.mb.Deliver("Receipts", "From: orders@shop.test\r\nSubject: Receipt 1\r\nMessage-ID: <r1@shop.test>\r\n\r\nthanks\r\n")
	w.sweep()
	if got, _ := w.st.FiledSince(w.ctx, w.acct.ID, 0, 0, 10); len(got) != 1 || len(w.found) != 0 {
		t.Errorf("filed = %v, found %v", got, w.found)
	}
}

// A sweep searches the folders for at most LocateLimit gone emails; the next one finds
// the rest.
func TestSweepLocatesALimitedNumber(t *testing.T) {
	w := newWorld(t)
	for range LocateLimit + 2 {
		at, _ := w.file("orders@shop.test")
		moveTo("INBOX")(w, at)
	}
	w.sweep()
	if len(w.found) != LocateLimit {
		t.Fatalf("first sweep found %d, want %d", len(w.found), LocateLimit)
	}
	w.sweep()
	if len(w.found) != LocateLimit+2 {
		t.Fatalf("second sweep found %d in all, want %d", len(w.found), LocateLimit+2)
	}
	counts := make([]int, len(w.found))
	for i, m := range w.recorded() {
		counts[i] = m.Count
	}
	if counts[len(counts)-1] != LocateLimit+2 {
		t.Errorf("counts = %v", counts)
	}
}

// Nothing is looked at for a tenant the module does not serve, nor while every sweep slot
// of the process is taken.
func TestSweepSkips(t *testing.T) {
	w := newWorld(t)
	at, _ := w.file("orders@shop.test")
	moveTo("INBOX")(w, at)
	var asked []int64
	w.w.Serves = func(_ context.Context, tenant int64) bool { asked = append(asked, tenant); return false }
	w.sweep()
	if len(w.found) != 0 || !slices.Equal(asked, []int64{w.acct.TenantID}) {
		t.Fatalf("not served: found %v, asked %v", w.found, asked)
	}
	w.w.Serves = nil
	for range Concurrent {
		w.w.slots <- struct{}{}
	}
	w.sweep()
	if len(w.found) != 0 {
		t.Fatalf("busy: found %v", w.found)
	}
	for range Concurrent {
		<-w.w.slots
	}
	w.sweep()
	if len(w.found) != 1 {
		t.Errorf("found %v", w.found)
	}
}
