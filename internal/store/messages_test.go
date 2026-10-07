package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

func TestMessagesAndDecisions(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	a := newAccount(t, s, "me@icloud.com", "pw")
	ref := mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 9, UID: 4}

	m, fresh, err := s.IngestMessage(ctx, ref, 100)
	if err != nil || !fresh || m.ID == 0 || m.State != StateNew || m.Location() != ref {
		t.Fatalf("ingest = %+v, %v, %v", m, fresh, err)
	}
	if again, fresh, err := s.IngestMessage(ctx, ref, 200); err != nil || fresh || again.ID != m.ID || again.CreatedAt != 100 {
		t.Fatalf("second ingest = %+v, %v, %v", again, fresh, err)
	}
	m.MessageID, m.FromAddr, m.ToAddrs, m.Subject, m.Signals = "a@b", "x@y.example", []string{"me@icloud.com"}, "Hi", Signals{Bulk: true, DMARC: "pass"}
	if err := s.SaveMessageSummary(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Message(ctx, m.ID); err != nil || got.FromAddr != "x@y.example" || got.ToAddrs[0] != "me@icloud.com" || !got.Signals.Bulk || got.Signals.DMARC != "pass" {
		t.Fatalf("message = %+v, %v", got, err)
	}
	if _, err := s.Message(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing message: %v", err)
	}

	// The retry queue: only failed messages whose time has come.
	if err := s.SetMessageState(ctx, m.ID, StateError, 2, 500); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueMessages(ctx, a.ID, 499); len(due) != 0 {
		t.Errorf("due too early: %+v", due)
	}
	if due, err := s.DueMessages(ctx, a.ID, 500); err != nil || len(due) != 1 || due[0].Attempts != 2 || due[0].NextAttemptAt != 500 {
		t.Errorf("due = %+v, %v", due, err)
	}

	u, _ := s.FirstUser(ctx)
	rule, err := s.CreateRule(ctx, rules.Rule{UserID: u.ID, Name: "Food", Intent: "food", Actions: []rules.Action{{Type: rules.ActKeep}}, Enabled: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "decider", RuleID: rule.ID, RuleVersion: 1, Confidence: 0.9, Reason: "r", Model: "jev", TokensIn: 7, CreatedAt: 300}, StateDecided)
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.ActivityFor(ctx, m.ID)
	if err != nil || row.Decision == nil || row.Decision.ID != id || row.Decision.RuleID != rule.ID || row.Message.State != StateDecided || row.Message.NextAttemptAt != 0 {
		t.Fatalf("activity row = %+v / %+v, %v", row.Message, row.Decision, err)
	}
	if _, err := s.ActivityFor(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing activity row: %v", err)
	}
	for name, f := range map[string]ActivityFilter{
		"account": {AccountID: a.ID}, "rule": {RuleID: rule.ID}, "stage": {Stage: "decider"}, "state": {State: StateDecided}, "cursor": {After: FeedCursor{300, m.ID + 1}},
	} {
		if rows, err := s.Activity(ctx, f); err != nil || len(rows) != 1 {
			t.Errorf("filter by %s: %d rows, %v", name, len(rows), err)
		}
	}
	for name, f := range map[string]ActivityFilter{
		"account": {AccountID: a.ID + 1}, "rule": {RuleID: rule.ID + 1}, "stage": {Stage: "sender"}, "state": {State: StateReview}, "cursor": {After: FeedCursor{300, m.ID}},
	} {
		if rows, err := s.Activity(ctx, f); err != nil || len(rows) != 0 {
			t.Errorf("filter by another %s: %d rows, %v", name, len(rows), err)
		}
	}

	// The folder position only moves forward, except across a UIDVALIDITY change.
	for _, step := range []struct{ validity, uid, want uint32 }{{9, 4, 4}, {9, 2, 4}, {9, 6, 6}, {10, 1, 1}} {
		if err := s.AdvanceFolder(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: step.validity, UID: step.uid}); err != nil {
			t.Fatal(err)
		}
		if f, _ := s.Folder(ctx, a.ID, "INBOX"); f.LastUID != step.want || f.UIDValidity != step.validity {
			t.Errorf("after uid %d (validity %d): %+v, want last_uid %d", step.uid, step.validity, f, step.want)
		}
	}

	// History outlives its rule; nothing outlives its account.
	if _, err := s.AddCorrection(ctx, u.ID, Correction{MessageID: m.ID, WrongRuleID: rule.ID, Example: "{}", CreatedAt: 400}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddCorrection(ctx, u.ID, Correction{MessageID: 999, Example: "{}"}, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("correcting a missing message: %v", err)
	}
	if err := s.DeleteRule(ctx, u.ID, rule.ID); err != nil {
		t.Fatalf("deleting a rule that has decisions and corrections: %v", err)
	}
	if row, _ := s.ActivityFor(ctx, m.ID); row.Decision == nil || row.Decision.RuleID != 0 || row.Decision.RuleVersion != 1 {
		t.Errorf("decision after its rule was deleted = %+v", row.Decision)
	}
	scoped, err := s.CreateRule(ctx, rules.Rule{UserID: u.ID, AccountID: a.ID, Name: "Scoped", Intent: "x", Actions: []rules.Action{{Type: rules.ActKeep}}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rule(ctx, u.ID, scoped.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a rule scoped to a deleted account is still there: %v", err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM messages) + (SELECT COUNT(*) FROM decisions) + (SELECT COUNT(*) FROM corrections)`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows left behind, %v", left, err)
	}
}

// The feed is in the order MailRules acted (MAI-74): by the time of each email's latest
// decision, or when it was first seen while undecided, newest first, however long ago the
// email arrived or was first seen. Same-second rows come newest row first, and paging
// through the cursor one row at a time lists every row once, in that order.
func TestActivityOrder(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	a := newAccount(t, s, "me@icloud.com", "pw")
	ingest := func(uid uint32, seen int64) Message {
		m, _, err := s.IngestMessage(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: uid}, seen)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	decide := func(m Message, at int64) {
		if _, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "none", CreatedAt: at}, StateSkipped); err != nil {
			t.Fatal(err)
		}
	}
	old, mid, undecided, same := ingest(1, 100), ingest(2, 200), ingest(3, 300), ingest(4, 400)
	if undecided.ActedAt != 300 {
		t.Errorf("an undecided email acted at %d, want when it was seen (300)", undecided.ActedAt)
	}
	decide(old, 150)
	decide(mid, 250)
	decide(same, 900)
	decide(old, 900) // decided again today, by a cleanup: it comes first again
	want := []int64{same.ID, old.ID, undecided.ID, mid.ID}

	all, err := s.Activity(ctx, ActivityFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, r := range all {
		got = append(got, r.Message.ID)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("feed = %v, want %v", got, want)
	}
	if all[1].Message.ActedAt != 900 || all[1].Decision.CreatedAt != 900 {
		t.Errorf("the re-decided email acted at %d, decision at %d; want 900", all[1].Message.ActedAt, all[1].Decision.CreatedAt)
	}

	got = nil
	for after := (FeedCursor{}); ; {
		page, err := s.Activity(ctx, ActivityFilter{After: after, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page[0].Message.ID)
		after = page[0].Cursor()
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("paged one at a time = %v, want %v", got, want)
	}
}

// Migration 0006: every email gets acted_at, the time of its latest decision, or when it
// was first seen if it has none.
func TestMigrationActedAt(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 5)
	s := New(db)
	a := newAccount(t, s, "me@icloud.com", "pw")
	exec := func(q string, args ...any) int64 {
		res, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	addMessage := func(uid int, seen int64) int64 {
		return exec(`INSERT INTO messages (account_id, folder, uidvalidity, uid, state, created_at) VALUES (?, 'INBOX', 1, ?, 'new', ?)`, a.ID, uid, seen)
	}
	twice, never := addMessage(1, 100), addMessage(2, 200)
	exec(`INSERT INTO decisions (message_id, stage, created_at) VALUES (?, 'none', 500)`, twice)
	exec(`INSERT INTO decisions (message_id, stage, created_at) VALUES (?, 'none', 700)`, twice)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]int64{twice: 700, never: 200} {
		if m, err := s.Message(ctx, id); err != nil || m.ActedAt != want {
			t.Errorf("message %d acted at %d, %v; want %d", id, m.ActedAt, err, want)
		}
	}
}

// migratedTo opens a database migrated up to version n only.
func migratedTo(t *testing.T, n int64) *sql.DB {
	t.Helper()
	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	return db
}

// An email that came back under a new UID keeps its one row (MAI-77): the row MailRules
// made for the new UID goes, and the known row records where the email is now. A row with
// any history is never removed.
func TestRebind(t *testing.T) {
	ctx := t.Context()
	for name, tc := range map[string]struct {
		history func(s *Store, fresh Message) error
		rebound bool
	}{
		"a row nothing was done with": {rebound: true},
		"a row that has a decision": {history: func(s *Store, m Message) error {
			_, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "none", CreatedAt: 1}, StateDecided)
			return err
		}},
		"a row that has an action": {history: func(s *Store, m Message) error {
			_, err := s.InsertAction(ctx, Action{MessageID: m.ID, AccountID: m.AccountID, Kind: "keep", Status: ActionDone, CreatedAt: 1})
			return err
		}},
		"a row that has a correction": {history: func(s *Store, m Message) error {
			_, err := s.AddCorrection(ctx, 1, Correction{MessageID: m.ID, Example: "{}", CreatedAt: 1}, "")
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := open(t)
			a := newAccount(t, s, "me@icloud.com", "pw")
			ref := func(uid uint32) mail.MsgRef {
				return mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: uid}
			}
			ingest := func(uid uint32) Message {
				m, _, err := s.IngestMessage(ctx, ref(uid), 100)
				if err != nil {
					t.Fatal(err)
				}
				m.MessageID = "same@example.test"
				if err := s.SaveMessageSummary(ctx, m); err != nil {
					t.Fatal(err)
				}
				return m
			}
			known, fresh := ingest(10), ingest(18)
			if got, err := s.MessageByMessageID(ctx, a.ID, "same@example.test", fresh.ID); err != nil || got.ID != known.ID {
				t.Fatalf("by message-id = %+v, %v", got, err)
			}
			if _, err := s.MessageByMessageID(ctx, a.ID, "other@example.test", 0); !errors.Is(err, ErrNotFound) {
				t.Errorf("unknown message-id: %v", err)
			}
			if tc.history != nil {
				if err := tc.history(s, fresh); err != nil {
					t.Fatal(err)
				}
			}

			rebound, err := s.Rebind(ctx, fresh.ID, known.ID, ref(18))
			if err != nil || rebound != tc.rebound {
				t.Fatalf("Rebind = %v, %v; want %v", rebound, err, tc.rebound)
			}
			_, freshErr := s.Message(ctx, fresh.ID)
			got, _ := s.Message(ctx, known.ID)
			again, isNew, _ := s.IngestMessage(ctx, ref(18), 200)
			if tc.rebound {
				if !errors.Is(freshErr, ErrNotFound) || got.Location() != ref(18) || again.ID != known.ID || isNew {
					t.Errorf("after rebind: fresh %v, known at %+v, uid 18 is row %d (new %v)", freshErr, got.Location(), again.ID, isNew)
				}
				return
			}
			if freshErr != nil || got.Location() != ref(10) || again.ID != fresh.ID {
				t.Errorf("a row with history was changed: fresh %v, known at %+v, uid 18 is row %d", freshErr, got.Location(), again.ID)
			}
		})
	}
}

// Migration 0005: an email that waits twice in Needs review (two rows, one Message-ID)
// waits there once; the older row stays, as skipped, with its decision.
func TestMigrationOneReviewRowPerEmail(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 4)
	s := New(db)
	a := newAccount(t, s, "me@icloud.com", "pw")
	rows := []struct {
		messageID, state, want string
	}{
		{"twice@example.test", StateReview, StateSkipped}, // the older of two rows in review
		{"twice@example.test", StateReview, StateReview},
		{"sorted@example.test", StateReview, StateReview}, // the newer row is not in review
		{"sorted@example.test", StateActed, StateActed},
		{"once@example.test", StateReview, StateReview},
		{"", StateReview, StateReview}, // no Message-ID: nothing to match on
		{"", StateReview, StateReview},
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		// The rows are written as the schema of version 4 has them.
		res, err := db.ExecContext(ctx, `INSERT INTO messages (account_id, folder, uidvalidity, uid, message_id, state, created_at) VALUES (?, 'INBOX', 1, ?, ?, ?, 100)`,
			a.ID, i+1, r.messageID, r.state)
		if err != nil {
			t.Fatal(err)
		}
		ids[i], _ = res.LastInsertId()
		if _, err := db.ExecContext(ctx, `INSERT INTO decisions (message_id, stage, created_at) VALUES (?, 'decider', 100)`, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		m, err := s.Message(ctx, ids[i])
		if err != nil || m.State != r.want {
			t.Errorf("row %d (%q, %s) = %s, %v; want %s", i, r.messageID, r.state, m.State, err, r.want)
		}
		if ds, _ := s.MessageDecisions(ctx, ids[i]); len(ds) != 1 {
			t.Errorf("row %d lost its decision", i)
		}
	}
}
