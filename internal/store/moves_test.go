package store

import (
	"context"
	"errors"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// movesWorld is one mailbox with a rule that files receipts.
type movesWorld struct {
	s      *Store
	ctx    context.Context
	user   User
	acct   Account
	rule   rules.Rule
	nextID uint32
}

func newMovesWorld(t *testing.T) *movesWorld {
	t.Helper()
	s, _ := open(t)
	ctx := t.Context()
	u, err := s.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAccount(ctx, Account{UserID: u.ID, Label: "me", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit",
		Username: "me@example.test", WatchFolder: "INBOX"}, "pw")

	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "Receipts", Intent: "receipts", Enabled: true,
		Actions: []rules.Action{{Type: rules.ActMove, Folder: "Receipts"}}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return &movesWorld{s: s, ctx: ctx, user: u, acct: a, rule: r}
}

// file records an email from sender that the rule moved to Receipts at now, and returns
// its row and the filing action.
func (w *movesWorld) file(t *testing.T, sender string, now int64) (Message, Action) {
	t.Helper()
	w.nextID++
	m, _, err := w.s.IngestMessage(w.ctx, mail.MsgRef{AccountID: w.acct.ID, Folder: "INBOX", UIDValidity: 1, UID: w.nextID}, now)
	if err != nil {
		t.Fatal(err)
	}
	m.MessageID, m.FromAddr, m.FromDomain = "id"+string(rune('a'+w.nextID))+"@shop.test", sender, "shop.test"
	if err := w.s.SaveMessageSummary(w.ctx, m); err != nil {
		t.Fatal(err)
	}
	dec, err := w.s.AddDecision(w.ctx, Decision{MessageID: m.ID, Stage: "condition", RuleID: w.rule.ID, RuleName: "Receipts", CreatedAt: now}, StateActed)
	if err != nil {
		t.Fatal(err)
	}
	a := Action{DecisionID: dec, MessageID: m.ID, AccountID: w.acct.ID, Kind: rules.ActMove, Folder: "Receipts",
		Before: Snapshot{Folder: "INBOX", UIDValidity: 1, UID: w.nextID}, Status: ActionFailed, CreatedAt: now}
	if a.ID, err = w.s.InsertAction(w.ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Status, a.After = ActionDone, &Snapshot{Folder: "Receipts", UIDValidity: 9, UID: 100 + w.nextID}
	if err := w.s.FinishAction(w.ctx, a, FollowUps{Notices: true}); err != nil {
		t.Fatal(err)
	}
	return m, a
}

func (w *movesWorld) move(f Filed, chosen string, inbox, person bool, now int64) Move {
	return Move{TenantID: w.user.TenantID, AccountID: w.acct.ID, MessageID: f.MessageID, ActionID: f.ActionID, From: f.From,
		RuleID: f.RuleID, RuleName: f.RuleName, Filed: f.After.Folder, Chosen: chosen, Inbox: inbox, Person: person,
		FiledAt: f.FiledAt, CreatedAt: now}
}

// FiledSince lists what MailRules filed and has not moved on: not what an undo took back,
// not an action a later move replaced, not one recorded already, not one outside the
// window.
func TestFiledSince(t *testing.T) {
	w := newMovesWorld(t)
	_, old := w.file(t, "a@shop.test", 10)
	_, kept := w.file(t, "b@shop.test", 1000)
	_, undone := w.file(t, "c@shop.test", 1000)
	if err := w.s.MarkActionUndone(w.ctx, undone, undone.Before.Ref(w.acct.ID), 1001); err != nil {
		t.Fatal(err)
	}
	m4, replaced := w.file(t, "d@shop.test", 1000)
	later := Action{MessageID: m4.ID, AccountID: w.acct.ID, Kind: rules.ActArchive, Before: *replaced.After, Status: ActionFailed, CreatedAt: 1002}
	var err error
	if later.ID, err = w.s.InsertAction(w.ctx, later); err != nil {
		t.Fatal(err)
	}
	later.Status, later.After = ActionDone, &Snapshot{Folder: "Archive", UIDValidity: 3, UID: 7}
	if err := w.s.FinishAction(w.ctx, later, FollowUps{Notices: true}); err != nil {
		t.Fatal(err)
	}

	got, err := w.s.FiledSince(w.ctx, w.acct.ID, 500, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(fs []Filed) (out []int64) {
		for _, f := range fs {
			out = append(out, f.ActionID)
		}
		return out
	}
	if want := []int64{kept.ID, later.ID}; !equalIDs(ids(got), want) {
		t.Fatalf("FiledSince = %v, want %v (old %d, undone %d, replaced %d)", ids(got), want, old.ID, undone.ID, replaced.ID)
	}
	f := got[0]
	if f.From != "b@shop.test" || f.MsgID == "" || f.RuleID != w.rule.ID || f.RuleName != "Receipts" || f.After.Folder != "Receipts" || f.FiledAt != 1000 {
		t.Errorf("filed = %+v", f)
	}
	if got[1].RuleID != 0 || got[1].After.Folder != "Archive" {
		t.Errorf("an archive with no decision = %+v", got[1])
	}
	if got, _ := w.s.FiledSince(w.ctx, w.acct.ID, 500, kept.ID, 10); !equalIDs(ids(got), []int64{later.ID}) {
		t.Errorf("after the cursor = %v", ids(got))
	}

	if id, err := w.s.RecordMove(w.ctx, w.move(f, "INBOX", true, true, 2000)); err != nil || id == 0 {
		t.Fatalf("RecordMove = %d, %v", id, err)
	}
	if id, err := w.s.RecordMove(w.ctx, w.move(f, "INBOX", true, true, 2001)); err != nil || id != 0 {
		t.Errorf("recording the same move again = %d, %v", id, err)
	}
	if got, _ := w.s.FiledSince(w.ctx, w.acct.ID, 500, 0, 10); !equalIDs(ids(got), []int64{later.ID}) {
		t.Errorf("after recording a move = %v", ids(got))
	}
	// An action undone between the look and the record leaves no move.
	if err := w.s.MarkActionUndone(w.ctx, later, later.Before.Ref(w.acct.ID), 2002); err != nil {
		t.Fatal(err)
	}
	if id, err := w.s.RecordMove(w.ctx, w.move(got[1], "INBOX", true, true, 2003)); err != nil || id != 0 {
		t.Errorf("recording a move of an undone action = %d, %v", id, err)
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Only the person's moves wait for the module, each counted with the earlier ones of the
// same sender to the same destination; one handed over waits no more, one refused waits
// until its next time.
func TestWaitingMoves(t *testing.T) {
	w := newMovesWorld(t)
	for range 4 {
		w.file(t, "news@shop.test", 1000)
	}
	fs, err := w.s.FiledSince(w.ctx, w.acct.ID, 0, 0, 10)
	if err != nil || len(fs) != 4 {
		t.Fatalf("filed = %v, %v", fs, err)
	}
	ids := make([]int64, 4)
	for i, mv := range []Move{w.move(fs[0], "INBOX", true, true, 2000), w.move(fs[1], "Trash", false, false, 2001),
		w.move(fs[2], "INBOX", true, true, 2002), w.move(fs[3], "Later", false, true, 2003)} {
		if ids[i], err = w.s.RecordMove(w.ctx, mv); err != nil {
			t.Fatal(err)
		}
	}
	got, err := w.s.WaitingMoves(w.ctx, 5000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != ids[0] || got[1].ID != ids[2] || got[2].ID != ids[3] {
		t.Fatalf("waiting = %+v", got)
	}
	if got[0].Count != 1 || got[1].Count != 2 || got[2].Count != 1 {
		t.Errorf("counts = %d %d %d, want 1 2 1", got[0].Count, got[1].Count, got[2].Count)
	}
	if m := got[1]; m.Mailbox != "me@example.test" || m.From != "news@shop.test" || m.Filed != "Receipts" || m.Chosen != "INBOX" ||
		!m.Inbox || m.RuleName != "Receipts" || m.RuleID != w.rule.ID || m.FiledAt != 1000 || m.CreatedAt != 2002 {
		t.Errorf("move = %+v", m)
	}
	if err := w.s.MoveHanded(w.ctx, ids[0], 5000); err != nil {
		t.Fatal(err)
	}
	if err := w.s.MoveRetry(w.ctx, ids[2], 9000); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.s.WaitingMoves(w.ctx, 5000, 10); len(got) != 1 || got[0].ID != ids[3] {
		t.Errorf("waiting after one handed and one refused = %+v", got)
	}
	if got, _ := w.s.WaitingMoves(w.ctx, 9000, 10); len(got) != 2 || got[0].ID != ids[3] || got[1].ID != ids[2] || got[1].Attempts != 1 {
		t.Errorf("waiting once the refused one is due = %+v", got)
	}
	// Retention takes moves older than MoveDays.
	if _, _, err := w.s.Retain(w.ctx, w.user.TenantID, 2001+MoveDays*day, 30); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.s.WaitingMoves(w.ctx, 1<<40, 10); len(got) != 2 {
		t.Errorf("waiting after retention = %+v", got)
	}
}

// One question per sender and destination: asking again counts the moves since; Yes stores
// the sender rule and No stores none; an answered question is never asked again.
func TestQuestions(t *testing.T) {
	w := newMovesWorld(t)
	for range 4 {
		w.file(t, "news@shop.test", 1000)
	}
	fs, _ := w.s.FiledSince(w.ctx, w.acct.ID, 0, 0, 10)
	record := func(f Filed, chosen string, inbox, person bool) int64 {
		t.Helper()
		id, err := w.s.RecordMove(w.ctx, w.move(f, chosen, inbox, person, 2000))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, notPerson := record(fs[0], "INBOX", true, true), record(fs[1], "Trash", false, false)
	if _, _, err := w.s.AskMove(w.ctx, notPerson, 3000); !errors.Is(err, ErrNotFound) {
		t.Errorf("asking about a move that is not the person's: %v", err)
	}
	q, changed, err := w.s.AskMove(w.ctx, first, 3000)
	if err != nil || !changed {
		t.Fatalf("AskMove = %+v, %v, %v", q, changed, err)
	}
	if !q.Inbox || q.Folder != "INBOX" || q.Emails != 1 || q.From != "news@shop.test" || q.Mailbox != "me@example.test" || q.Status != QuestionOpen {
		t.Errorf("question = %+v", q)
	}
	if _, changed, _ := w.s.AskMove(w.ctx, first, 3001); changed {
		t.Error("asking the same again changed it")
	}
	second := record(fs[2], "INBOX", true, true)
	again, changed, err := w.s.AskMove(w.ctx, second, 3002)
	if err != nil || !changed || again.ID != q.ID || again.Emails != 2 {
		t.Errorf("asking after a second move = %+v, %v, %v", again, changed, err)
	}
	folder := record(fs[3], "Later", false, true)
	other, _, err := w.s.AskMove(w.ctx, folder, 3003)
	if err != nil || other.ID == q.ID || other.Inbox || other.Folder != "Later" || other.Emails != 1 {
		t.Fatalf("a question about another folder = %+v, %v", other, err)
	}

	me := w.user.Viewer()
	if open, err := w.s.OpenQuestions(w.ctx, me); err != nil || len(open) != 2 || open[0].ID != other.ID {
		t.Errorf("open = %+v, %v", open, err)
	}
	stranger := Viewer{UserID: me.UserID + 100, TenantID: me.TenantID + 100}
	if open, _ := w.s.OpenQuestions(w.ctx, stranger); len(open) != 0 {
		t.Errorf("another tenant sees %+v", open)
	}
	if _, err := w.s.AnswerQuestion(w.ctx, stranger, q.ID, true, 4000); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant answers: %v", err)
	}

	// No: nothing learned, and never asked again.
	if _, err := w.s.AnswerQuestion(w.ctx, me, other.ID, false, 4000); err != nil {
		t.Fatal(err)
	}
	if srs, _ := w.s.SenderRules(w.ctx, me.TenantID); len(srs) != 0 {
		t.Errorf("No stored %+v", srs)
	}
	if _, _, err := w.s.AskMove(w.ctx, folder, 4001); !errors.Is(err, ErrAnswered) {
		t.Errorf("asking after No: %v", err)
	}
	if _, err := w.s.AnswerQuestion(w.ctx, me, other.ID, true, 4002); !errors.Is(err, ErrNotFound) {
		t.Errorf("answering twice: %v", err)
	}

	// Yes over a learned rule: the person's rule replaces it.
	if _, err := w.s.AddLearnedSenderRule(w.ctx, me.TenantID, me.UserID, "news@shop.test", w.rule.ID, 3500); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.AnswerQuestion(w.ctx, me, q.ID, true, 4003); err != nil {
		t.Fatal(err)
	}
	srs, _ := w.s.SenderRules(w.ctx, me.TenantID)
	if len(srs) != 1 || srs[0].Value != "news@shop.test" || srs[0].MatchType != rules.MatchAddress || srs[0].Verdict != rules.VerdictKeep ||
		srs[0].Source != "user" || srs[0].UserID != me.UserID || srs[0].RuleID != 0 {
		t.Errorf("sender rules after Yes = %+v", srs)
	}
	if open, _ := w.s.OpenQuestions(w.ctx, me); len(open) != 0 {
		t.Errorf("open after both answered = %+v", open)
	}
	if _, _, err := w.s.AskMove(w.ctx, second, 4004); !errors.Is(err, ErrAnswered) {
		t.Errorf("asking after Yes: %v", err)
	}
}

// Yes to a question about an ordinary folder moves the sender's mail there.
func TestAnswerYesToAFolder(t *testing.T) {
	w := newMovesWorld(t)
	w.file(t, "news@shop.test", 1000)
	fs, _ := w.s.FiledSince(w.ctx, w.acct.ID, 0, 0, 10)
	id, err := w.s.RecordMove(w.ctx, w.move(fs[0], "Reading", false, true, 2000))
	if err != nil {
		t.Fatal(err)
	}
	q, _, err := w.s.AskMove(w.ctx, id, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.AnswerQuestion(w.ctx, w.user.Viewer(), q.ID, true, 4000); err != nil {
		t.Fatal(err)
	}
	srs, _ := w.s.SenderRules(w.ctx, w.user.TenantID)
	if len(srs) != 1 || srs[0].Verdict != rules.VerdictMove || srs[0].Folder != "Reading" || srs[0].Source != "user" {
		t.Errorf("sender rules = %+v", srs)
	}
}

// A cleanup that sorted the same email under a row of its own left it where it is now:
// MailRules, not the person.
func TestLeftByMailRules(t *testing.T) {
	w := newMovesWorld(t)
	m, a := w.file(t, "x@shop.test", 1000)
	at := a.After
	if left, err := w.s.LeftByMailRules(w.ctx, w.acct.ID, at.Folder, at.UIDValidity, at.UID, m.ID); err != nil || left {
		t.Errorf("its own row = %v, %v", left, err)
	}
	if left, err := w.s.LeftByMailRules(w.ctx, w.acct.ID, at.Folder, at.UIDValidity, at.UID, m.ID+1); err != nil || !left {
		t.Errorf("another row's place = %v, %v", left, err)
	}
}
