package worker

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// imapWorld is a live account over the real IMAP connector and go-imap's in-memory server,
// with the supervisor watching INBOX and the executor acting live, plus a second
// connection standing in for the user's own mail client.
type imapWorld struct {
	t    *testing.T
	srv  *imaptest.Server
	st   *store.Store
	acct store.Account
	mgr  *Manager
	exec *actions.Exec
	user mail.Mailbox // the user's mail client: changes made outside MailRules
}

func newIMAPWorld(t *testing.T) *imapWorld {
	t.Helper()
	ctx := t.Context()
	srv := imaptest.Start(t, goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapMove: {}, goimap.CapUIDPlus: {}})
	st, _ := storetest.New(t)
	u, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := st.CreateAccount(ctx, store.Account{UserID: u.ID, Label: "x", Preset: "generic", Host: srv.Host, Port: srv.Port, TLSMode: presets.TLSImplicit, Username: imaptest.Username}, imaptest.Password)
	if err != nil {
		t.Fatal(err)
	}
	// Mail from food.example is sorted into Food. Anything else is for the rule with an
	// intent, and with no decision model set it waits in Needs review.
	for _, r := range []rules.Rule{
		{UserID: u.ID, Name: "Food", Enabled: true, Priority: 1, Conditions: rules.Cond{Field: "from_domain", Op: rules.OpEq, Value: "food.example"},
			Actions: []rules.Action{{Type: rules.ActMove, Folder: "Food"}}},
		{UserID: u.ID, Name: "Later", Enabled: true, Priority: 2, Intent: "Things to read later", Actions: []rules.Action{{Type: rules.ActMove, Folder: "Later"}}},
	} {
		if _, err := st.CreateRule(ctx, 1, r, 1); err != nil {
			t.Fatal(err)
		}
	}
	cfg := imap.Config{AccountID: acct.ID, Host: srv.Host, Port: srv.Port, TLSMode: presets.TLSImplicit,
		Username: imaptest.Username, Password: imaptest.Password, TLSConfig: srv.TLS, Logger: slog.New(slog.DiscardHandler),
		IdleRestart: 40 * time.Millisecond, PollInterval: 10 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond}
	hub := events.NewHub()
	mgr := &Manager{}
	exec := &actions.Exec{Store: st, Accounts: mgr, Hub: hub}
	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	t.Cleanup(func() { stop(); mgr.Wait() }) // before the database closes
	mgr.Start(runCtx, &Supervisor{
		Account: acct, Store: st, Hub: hub,
		Open:       func(ctx context.Context) (mail.Mailbox, error) { return imap.Open(ctx, cfg) },
		Pipeline:   pipeline.Pipeline{Store: st, Hub: hub, MinConfidence: 0.75, BodyChars: 2000, Exec: exec},
		BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond, DrainTimeout: 50 * time.Millisecond,
	})
	eventually(t, "the account to be live", func() bool {
		a, _ := st.Account(ctx, acct.ID)
		return a.Status == StatusLive
	})
	user, err := imap.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = user.Close() })
	return &imapWorld{t: t, srv: srv, st: st, acct: acct, mgr: mgr, exec: exec, user: user}
}

// deliver puts an email in INBOX and waits until MailRules has handled it.
func (w *imapWorld) deliver(eml string) mail.MsgRef {
	w.t.Helper()
	return w.arrived(w.srv.Append(w.t, "INBOX", eml))
}

// arrived waits until the watcher has handled INBOX up to uid and returns its reference.
func (w *imapWorld) arrived(uid uint32) mail.MsgRef {
	w.t.Helper()
	st, err := w.user.Status(w.t.Context(), "INBOX")
	if err != nil {
		w.t.Fatal(err)
	}
	eventually(w.t, "the watcher to reach the new mail", func() bool {
		f, _ := w.st.Folder(w.t.Context(), w.acct.ID, "INBOX")
		return f.LastUID >= uid
	})
	return mail.MsgRef{AccountID: w.acct.ID, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: uid}
}

// rows lists MailRules' rows for the email with this Message-ID.
func (w *imapWorld) rows(messageID string) []store.Message {
	w.t.Helper()
	all, err := w.st.Activity(w.t.Context(), store.Viewer{UserID: 1, TenantID: 1}, store.ActivityFilter{})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []store.Message
	for _, r := range all {
		if r.Message.MessageID == messageID {
			out = append(out, r.Message)
		}
	}
	return out
}

// MAI-77: an email that comes back into INBOX outside MailRules (moved out and back by the
// user's mail client, or brought back from where a rule put it) has a new UID. It is the
// same email: it keeps its one row, now pointing at where it is, and is not decided again,
// so it never waits twice in Needs review and is not sorted away again. A second copy that
// arrives while the first is still in place is new mail.
func TestEmailBackUnderANewUID(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		from  string
		again func(w *imapWorld, eml string, now mail.MsgRef) mail.MsgRef // returns where the email now is in INBOX
		rows  int
		state string
	}{
		{"waiting in Needs review, moved out and back", "friend@people.example", func(w *imapWorld, _ string, now mail.MsgRef) mail.MsgRef {
			if err := w.user.EnsureFolder(ctx, "Archive"); err != nil {
				t.Fatal(err)
			}
			out, err := w.user.Move(ctx, now, "Archive")
			if err != nil {
				t.Fatal(err)
			}
			back, err := w.user.Move(ctx, out, "INBOX")
			if err != nil {
				t.Fatal(err)
			}
			return w.arrived(back.UID)
		}, 1, store.StateReview},
		{"sorted into Food, brought back by the user", "orders@food.example", func(w *imapWorld, _ string, now mail.MsgRef) mail.MsgRef {
			back, err := w.user.Move(ctx, now, "INBOX")
			if err != nil {
				t.Fatal(err)
			}
			return w.arrived(back.UID)
		}, 1, store.StateActed},
		{"a second copy while the first is still there", "friend@people.example", func(w *imapWorld, eml string, _ mail.MsgRef) mail.MsgRef {
			return w.deliver(eml)
		}, 2, store.StateReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newIMAPWorld(t)
			const id = "the-email@example.test"
			eml := "From: " + tc.from + "\r\nTo: me@example.test\r\nSubject: Your application\r\nMessage-ID: <" + id + ">\r\n\r\nThank you for applying.\r\n"
			w.deliver(eml)
			first := w.rows(id)
			if len(first) != 1 || first[0].State != tc.state {
				t.Fatalf("on arrival: %+v, want one row %s", first, tc.state)
			}

			now := tc.again(w, eml, first[0].Location())

			got := w.rows(id)
			if len(got) != tc.rows {
				t.Fatalf("%d rows for one email, want %d: %+v", len(got), tc.rows, got)
			}
			for _, m := range got {
				if m.State != tc.state {
					t.Errorf("row %d is %s, want %s", m.ID, m.State, tc.state)
				}
			}
			if tc.rows == 1 && (got[0].ID != first[0].ID || got[0].Location() != now) {
				t.Errorf("the row is %d at %+v, want row %d at %+v", got[0].ID, got[0].Location(), first[0].ID, now)
			}
			// The email stays where the user put it, and is still there to act on.
			if _, err := w.user.Flags(ctx, now); err != nil {
				t.Errorf("the email is no longer at %+v: %v", now, err)
			}
			if n, _ := w.st.CountMessages(ctx, store.Viewer{UserID: 1, TenantID: 1}, store.StateReview); tc.state == store.StateReview && n != tc.rows {
				t.Errorf("Needs review holds %d, want %d", n, tc.rows)
			}
		})
	}
}

// MAI-262: a cleanup Sort over a real IMAP connection moves its emails in chunks of
// actions.MoveChunk, each email still on record as its own undoable action, and the batch's
// undo puts every one back.
func TestCleanupSortOverIMAP(t *testing.T) {
	w := newIMAPWorld(t)
	ctx := t.Context()
	if err := w.user.EnsureFolder(ctx, "Old"); err != nil {
		t.Fatal(err)
	}
	const n = actions.MoveChunk + 50 // two chunks
	for i := range n {
		w.srv.Append(t, "Old", fmt.Sprintf("From: orders@food.example\r\nTo: me@example.test\r\nSubject: order %d\r\nMessage-ID: <order-%d@food.example>\r\n\r\nbody\r\n", i, i))
	}
	rows := checkRows(t, w.st, w.user, w.acct.ID, 1, "Old")
	if len(rows) != n {
		t.Fatalf("check found %d rows, want %d", len(rows), n)
	}
	b, err := w.mgr.Sort(ctx, SortRun{AccountID: w.acct.ID, Folder: "Old", Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the run to finish", func() bool {
		got, _ := w.st.Batch(ctx, b.ID)
		return got.Status != store.BatchRunning
	})
	if got, _ := w.st.Batch(ctx, b.ID); got.Status != store.BatchDone || got.Done != n || got.Skipped != 0 {
		t.Fatalf("batch = %+v", got)
	}
	count := func(folder string) int {
		refs, _, err := w.user.FetchSince(ctx, folder, time.Time{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(refs)
	}
	if old, food := count("Old"), count("Food"); old != 0 || food != n {
		t.Fatalf("after the sort: Old %d, Food %d", old, food)
	}
	acts, err := w.st.BatchActions(ctx, b.ID)
	if err != nil || len(acts) != n {
		t.Fatalf("%d actions on record, %v", len(acts), err)
	}
	for _, a := range acts {
		if a.Status != store.ActionDone || a.After == nil || a.After.Folder != "Food" || a.After.UID == 0 {
			t.Fatalf("action = %+v", a)
		}
	}
	u, err := w.exec.UndoBatch(ctx, store.Viewer{UserID: 1, TenantID: 1}, b.ID)
	if err != nil || u.Actions != n || u.Emails != n {
		t.Fatalf("UndoBatch = %+v, %v", u, err)
	}
	if old, food := count("Old"), count("Food"); old != n || food != 0 {
		t.Errorf("after the undo: Old %d, Food %d", old, food)
	}
}
