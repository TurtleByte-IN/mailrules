package actions

import (
	"context"
	"errors"
	netmail "net/mail"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// draftEnv is newEnv with a Drafts folder the server marks \Drafts, on the server and on record.
func draftEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.mb.AddFolder("Drafts", mail.RoleDrafts)
	if err := e.st.SaveFolders(t.Context(), e.acct.ID, append(slices.Clone(roleFolders), store.Folder{Name: "Drafts", SpecialUse: mail.RoleDrafts})); err != nil {
		t.Fatal(err)
	}
	e.x.Drafts = func(context.Context, int64) bool { return true }
	return e
}

// liveRun has a rule that drafts replies act on a delivered email live and returns the
// email and the draft job the run queued.
func (e *env) liveRun(acts ...string) (DecisionRecord, store.DraftJob) {
	e.t.Helper()
	ctx := e.t.Context()
	rule, err := e.st.CreateRule(ctx, 1, rules.Rule{UserID: e.user.ID, Name: "Shops", Intent: "shops", Actions: act(acts...), Enabled: true,
		Draft: &rules.Draft{Instruction: "say thanks", ReadPrevious: true}}, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	d := e.deliver("1")
	if d.DecisionID, err = e.st.AddDecision(ctx, store.Decision{MessageID: d.MessageID, Stage: "condition", RuleID: rule.ID, RuleName: rule.Name, CreatedAt: 1}, store.StateDecided); err != nil {
		e.t.Fatal(err)
	}
	batch, err := e.st.LiveBatch(ctx, 1, time.Unix(1_800_000_000, 0))
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.x.Apply(ctx, d, rule.Actions, batch); err != nil {
		e.t.Fatal(err)
	}
	jobs, err := e.st.WaitingDrafts(ctx, 2_000_000_000, 10)
	if err != nil || len(jobs) != 1 {
		e.t.Fatalf("draft jobs = %+v, %v", jobs, err)
	}
	return d, jobs[0]
}

// writer is a DraftWriter that reads the email and replies "Thanks!", counting its calls.
func writer(calls *int) DraftWriter {
	return func(ctx context.Context, mb mail.Mailbox, email mail.MsgRef, _ store.Message) (Reply, error) {
		*calls++
		raw, err := mb.Fetch(ctx, email, 0)
		if err != nil {
			return Reply{}, err
		}
		sum, err := message.Parse(raw, email.AccountID, 2000)
		if err != nil {
			return Reply{}, err
		}
		b, id := message.Reply(sum, netmail.Address{Address: "me@example.test"}, "Thanks!", "K1", time.Unix(1_800_000_000, 0))
		return Reply{Raw: b, MessageID: id}, nil
	}
}

// A rule that drafts replies queues one draft job for every email it acts on live, in the
// transaction of its last action, and none for anything else.
func TestApplyQueuesDraft(t *testing.T) {
	tests := []struct {
		name        string
		stage       string // the decision's; "" = no decision (a correction)
		batch       string
		dryRun      bool
		draft       bool // the rule drafts replies
		gate        int  // 0: the tenant may draft; 1: its plan does not include it; 2: the build has no module
		failOnFlags bool
		want        bool
	}{
		{name: "a live run of a rule", stage: "condition", batch: store.BatchLive, draft: true, want: true},
		{name: "a model's pick", stage: "decider", batch: store.BatchLive, draft: true, want: true},
		{name: "a run a failure cut short", stage: "condition", batch: store.BatchLive, draft: true, failOnFlags: true},
		{name: "dry-run", stage: "condition", batch: store.BatchLive, dryRun: true, draft: true},
		{name: "a cleanup", stage: "condition", batch: store.BatchCleanup, draft: true},
		{name: "a sender rule", stage: "sender", batch: store.BatchLive, draft: true},
		{name: "a correction", batch: store.BatchLive, draft: true},
		{name: "a rule that drafts nothing", stage: "condition", batch: store.BatchLive},
		{name: "a plan without draft replies", stage: "condition", batch: store.BatchLive, draft: true, gate: 1},
		{name: "a build without the module", stage: "condition", batch: store.BatchLive, draft: true, gate: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := draftEnv(t)
			ctx := t.Context()
			switch tt.gate {
			case 1:
				e.x.Drafts = func(context.Context, int64) bool { return false }
			case 2:
				e.x.Drafts = nil
			}
			r := rules.Rule{UserID: e.user.ID, Name: "Shops", Intent: "shops", Actions: act("move:Food", "flag"), Enabled: true}
			if tt.draft {
				r.Draft = &rules.Draft{Instruction: "say thanks", ReadPrevious: true}
			}
			rule, err := e.st.CreateRule(ctx, 1, r, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.st.SetDryRun(ctx, 1, tt.dryRun); err != nil {
				t.Fatal(err)
			}
			if tt.failOnFlags {
				e.mb.storeErr = errors.New("NO [CANNOT] refused")
			}
			d := e.deliver("1")
			if tt.stage != "" {
				if d.DecisionID, err = e.st.AddDecision(ctx, store.Decision{MessageID: d.MessageID, Stage: tt.stage, RuleID: rule.ID,
					RuleName: rule.Name, CreatedAt: 1}, store.StateDecided); err != nil {
					t.Fatal(err)
				}
			}
			batch, err := e.st.LiveBatch(ctx, 1, time.Unix(1_800_000_000, 0))
			if tt.batch == store.BatchCleanup {
				var b store.Batch
				b, err = e.st.CreateCleanupBatch(ctx, e.acct.ID, "INBOX", 0, 0, 0, 1, 1)
				batch = b.ID
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.x.Apply(ctx, d, rule.Actions, batch); (err != nil) != tt.failOnFlags {
				t.Fatalf("Apply = %v", err)
			}
			got, err := e.st.WaitingDrafts(ctx, 2_000_000_000, 10)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.want {
				if len(got) != 0 {
					t.Fatalf("draft jobs = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("draft jobs = %+v, want one", got)
			}
			j := got[0]
			if j.TenantID != 1 || j.AccountID != e.acct.ID || j.MessageID != d.MessageID || j.DecisionID != d.DecisionID || j.BatchID != batch ||
				j.RuleID != rule.ID || j.RuleName != "Shops" || j.Instruction != "say thanks" || !j.ReadPrevious || j.Key == "" ||
				j.CreatedAt != 1_800_000_000 || j.Attempts != 0 {
				t.Errorf("draft job = %+v", j)
			}
		})
	}
}

// A live run's reply is saved in Drafts as a draft, marked read, threaded under the email,
// which stays where the rule put it; the action that records it can be undone, and undoing
// it deletes that reply and nothing else.
func TestDraft(t *testing.T) {
	e := draftEnv(t)
	ctx := t.Context()
	d, j := e.liveRun("move:Food")
	calls := 0
	rec, err := e.x.Draft(ctx, j, writer(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != store.ActionDraft || rec.Status != store.ActionDone || rec.Folder != "Drafts" || rec.Draft == nil ||
		rec.Draft.MessageID != "mailrules-draft-K1@example.test" || rec.DecisionID != d.DecisionID || rec.BatchID != j.BatchID || calls != 1 {
		t.Fatalf("draft action = %+v (writer called %d times)", rec, calls)
	}
	saved := mail.MsgRef{AccountID: e.acct.ID, Folder: "Drafts", UIDValidity: rec.Draft.UIDValidity, UID: rec.Draft.UID}
	raw, flags, err := e.mb.Message(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(flags, []string{`\Draft`, `\Seen`}) || !strings.Contains(raw, "In-Reply-To: <1@shop.example>") ||
		!strings.Contains(raw, "Subject: Re: order 1") || !strings.Contains(raw, "To: \"Shop\" <orders@shop.example>") {
		t.Errorf("saved draft (flags %v):\n%s", flags, raw)
	}
	if ref, _ := e.where(d.MessageID); ref.Folder != "Food" {
		t.Errorf("the email moved to %s", ref.Folder)
	}
	if got := e.actions(d.MessageID); len(got) != 2 || got[1].ID != rec.ID || *got[1].Draft != *rec.Draft {
		t.Errorf("actions = %+v", got)
	}

	// Handed over again (the job outlived a crash): nothing more is written or saved.
	if again, err := e.x.Draft(ctx, j, writer(&calls)); !errors.Is(err, ErrDraftSettled) || again.ID != rec.ID || calls != 1 {
		t.Errorf("a second hand-over = %+v, %v (writer called %d times)", again, err, calls)
	}

	// Another draft of the person's own in Drafts must survive the undo.
	own := e.mb.Deliver("Drafts", "From: me@example.test\r\nSubject: my own\r\nMessage-ID: <own@example.test>\r\n\r\nhi\r\n", `\Draft`)
	if err := e.x.Undo(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.mb.Message(saved); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("the reply is still in Drafts: %v", err)
	}
	if _, _, err := e.mb.Message(own); err != nil {
		t.Errorf("the person's own draft went: %v", err)
	}
	if got := e.actions(d.MessageID); got[1].Status != store.ActionUndone || got[0].Status != store.ActionDone {
		t.Errorf("after undoing the draft: %+v", got)
	}
	if ref, _ := e.where(d.MessageID); ref.Folder != "Food" {
		t.Errorf("undoing the draft moved the email to %s", ref.Folder)
	}
	if !slices.Equal(e.mb.changes(), []string{"move Food", "append Drafts", "delete Drafts/1"}) {
		t.Errorf("mailbox changes = %v", e.mb.changes())
	}
}

// Undoing the day's batch undoes the reply with the rule's actions, the reply first.
func TestDraftUndoneWithItsBatch(t *testing.T) {
	e := draftEnv(t)
	ctx := t.Context()
	d, j := e.liveRun("move:Food")
	calls := 0
	if _, err := e.x.Draft(ctx, j, writer(&calls)); err != nil {
		t.Fatal(err)
	}
	u, err := e.x.UndoBatch(ctx, e.user.Viewer(), j.BatchID)
	if err != nil || u != (Undid{Actions: 2, Emails: 1}) {
		t.Fatalf("UndoBatch = %+v, %v", u, err)
	}
	if got := e.mb.changes(); !slices.Equal(got, []string{"move Food", "append Drafts", "delete Drafts/1", "move INBOX"}) {
		t.Errorf("mailbox changes = %v", got)
	}
	if ref, _ := e.where(d.MessageID); ref.Folder != "INBOX" {
		t.Errorf("email in %s after the undo", ref.Folder)
	}
}

// A reply the person edited, sent or deleted since is not MailRules' to delete: the undo
// fails as for an email gone, and the action stays done.
func TestDraftUndoOfAReplyGone(t *testing.T) {
	e := draftEnv(t)
	ctx := t.Context()
	_, j := e.liveRun("move:Food")
	calls := 0
	rec, err := e.x.Draft(ctx, j, writer(&calls))
	if err != nil {
		t.Fatal(err)
	}
	saved := mail.MsgRef{AccountID: e.acct.ID, Folder: "Drafts", UIDValidity: rec.Draft.UIDValidity, UID: rec.Draft.UID}
	if err := e.mb.Mailbox.Delete(ctx, saved); err != nil { // the person's mail app replaced it with their edit
		t.Fatal(err)
	}
	edited := e.mb.Deliver("Drafts", "Message-ID: <"+rec.Draft.MessageID+">\r\nSubject: Re: order 1\r\n\r\nThanks, edited\r\n", `\Draft`)
	if err := e.x.Undo(ctx, rec.ID); !errors.Is(err, ErrGone) {
		t.Fatalf("undo = %v, want ErrGone", err)
	}
	if _, _, err := e.mb.Message(edited); err != nil {
		t.Errorf("the person's edit was deleted: %v", err)
	}
	if a, _ := e.st.Action(ctx, rec.ID); a.Status != store.ActionDone {
		t.Errorf("action = %+v", a)
	}
}

// What stops a reply being saved is recorded on the draft action, with a reason Activity
// shows, or left for another try when it may pass; dry-run records what would happen and
// pays for no model.
func TestDraftOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(e *env, j store.DraftJob)
		write      func(calls *int) DraftWriter
		wantErr    error // ErrDraftSettled, or errAny for one to try again
		wantStatus string
		wantReason string
		wantCalls  int
	}{
		{name: "dry-run", setup: func(e *env, _ store.DraftJob) { _ = e.st.SetDryRun(e.t.Context(), 1, true) }, wantStatus: store.ActionDryRun},
		{name: "no Drafts folder", setup: func(e *env, _ store.DraftJob) { _ = e.st.SaveFolders(e.t.Context(), e.acct.ID, roleFolders) },
			wantStatus: store.ActionFailed, wantReason: "no Drafts folder"},
		{name: "the email is gone", setup: func(e *env, j store.DraftJob) {
			m, _ := e.st.Message(e.t.Context(), j.MessageID)
			_, _ = e.mb.Mailbox.Move(e.t.Context(), m.Location(), "Trash")
		}, wantStatus: store.ActionFailed, wantReason: "the email was moved or deleted before a reply was drafted"},
		{name: "the writer will not", write: func(calls *int) DraftWriter {
			return func(context.Context, mail.Mailbox, mail.MsgRef, store.Message) (Reply, error) {
				*calls++
				return Reply{}, &SkipDraft{Reason: "this month's AI-sorted emails are used up, so no reply was drafted"}
			}
		}, wantStatus: store.ActionFailed, wantReason: "this month's AI-sorted emails are used up, so no reply was drafted", wantCalls: 1},
		{name: "the writer fails for now", write: func(calls *int) DraftWriter {
			return func(context.Context, mail.Mailbox, mail.MsgRef, store.Message) (Reply, error) {
				*calls++
				return Reply{}, errors.New("model timed out")
			}
		}, wantErr: errAny, wantCalls: 1},
		{name: "not connected", setup: func(e *env, _ store.DraftJob) { e.accs.mb = nil }, wantErr: errAny},
		{name: "the rule's actions were undone", setup: func(e *env, j store.DraftJob) {
			acts, _ := e.st.MessageActions(e.t.Context(), j.MessageID)
			_ = e.x.Undo(e.t.Context(), acts[0].ID)
		}, wantErr: ErrDraftSettled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := draftEnv(t)
			d, j := e.liveRun("move:Food")
			if tt.setup != nil {
				tt.setup(e, j)
			}
			write := writer
			if tt.write != nil {
				write = tt.write
			}
			calls := 0
			rec, err := e.x.Draft(t.Context(), j, write(&calls))
			switch {
			case tt.wantErr == errAny && (err == nil || errors.Is(err, ErrDraftSettled)),
				tt.wantErr != errAny && !errors.Is(err, tt.wantErr):
				t.Fatalf("Draft = %+v, %v; want %v", rec, err, tt.wantErr)
			}
			if calls != tt.wantCalls {
				t.Errorf("writer called %d times, want %d", calls, tt.wantCalls)
			}
			var drafts []store.Action
			for _, a := range e.actions(d.MessageID) {
				if a.Kind == store.ActionDraft {
					drafts = append(drafts, a)
				}
			}
			if tt.wantStatus == "" {
				if len(drafts) != 0 {
					t.Errorf("draft actions = %+v, want none", drafts)
				}
			} else if len(drafts) != 1 || drafts[0].Status != tt.wantStatus || drafts[0].Error != tt.wantReason || drafts[0].Draft != nil {
				t.Errorf("draft actions = %+v, want one %s (%q)", drafts, tt.wantStatus, tt.wantReason)
			}
			if slices.ContainsFunc(e.mb.changes(), func(c string) bool { return strings.HasPrefix(c, "append") }) {
				t.Errorf("something was saved: %v", e.mb.changes())
			}
		})
	}
}

// errAny stands for an error to try again: any but ErrDraftSettled.
var errAny = errors.New("any error")

// A reply saved just before the daemon stopped, with its row still saying so, is found by
// its Message-ID and recorded on that row: never saved twice. A job given up records why.
func TestDraftAfterACrash(t *testing.T) {
	e := draftEnv(t)
	ctx := t.Context()
	d, j := e.liveRun("move:Food")
	calls := 0
	reply, err := writer(&calls)(ctx, e.mb, mail.MsgRef{}, store.Message{})
	if err == nil {
		t.Fatal("the writer read nothing")
	}
	m, _ := e.st.Message(ctx, d.MessageID)
	reply, err = writer(&calls)(ctx, e.mb, m.Location(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.mb.Mailbox.Append(ctx, "Drafts", reply.Raw, DraftFlags, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	loc := m.Location()
	half, err := e.st.InsertAction(ctx, store.Action{DecisionID: j.DecisionID, BatchID: j.BatchID, MessageID: d.MessageID, AccountID: e.acct.ID,
		Kind: store.ActionDraft, Folder: "Drafts", Before: store.Snapshot{Folder: loc.Folder, UIDValidity: loc.UIDValidity, UID: loc.UID},
		Status: store.ActionFailed, Error: store.ActionInterrupted, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := e.x.Draft(ctx, j, writer(&calls))
	if err != nil || rec.ID != half || rec.Status != store.ActionDone || rec.Draft == nil {
		t.Fatalf("Draft after a crash = %+v, %v", rec, err)
	}
	if refs, _, _ := e.raw.FetchSince(ctx, "Drafts", time.Time{}, 0); len(refs) != 1 {
		t.Errorf("%d drafts saved, want 1", len(refs))
	}

	_, j2 := e.liveRunAgain(t)
	rec, err = e.x.GiveUpDraft(ctx, j2, "the mailbox could not be reached in time to draft a reply")
	if err != nil || rec.Status != store.ActionFailed || rec.Error != "the mailbox could not be reached in time to draft a reply" {
		t.Errorf("GiveUpDraft = %+v, %v", rec, err)
	}
}

// liveRunAgain is liveRun for a second email in the same env.
func (e *env) liveRunAgain(t *testing.T) (DecisionRecord, store.DraftJob) {
	t.Helper()
	ctx := t.Context()
	rs, err := e.st.Rules(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	d := e.deliver("2")
	if d.DecisionID, err = e.st.AddDecision(ctx, store.Decision{MessageID: d.MessageID, Stage: "condition", RuleID: rs[0].ID, RuleName: rs[0].Name, CreatedAt: 1}, store.StateDecided); err != nil {
		t.Fatal(err)
	}
	batch, _ := e.st.LiveBatch(ctx, 1, time.Unix(1_800_000_000, 0))
	if _, err := e.x.Apply(ctx, d, rs[0].Actions, batch); err != nil {
		t.Fatal(err)
	}
	jobs, _ := e.st.WaitingDrafts(ctx, 2_000_000_000, 10)
	for _, j := range jobs {
		if j.MessageID == d.MessageID {
			return d, j
		}
	}
	t.Fatalf("no job for the second email: %+v", jobs)
	return d, store.DraftJob{}
}
