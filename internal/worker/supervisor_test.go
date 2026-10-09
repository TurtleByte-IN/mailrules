package worker

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

func eml(subject string) string {
	return "From: weekly@news.example\r\nTo: me@example.test\r\nSubject: " + subject + "\r\n\r\nbody\r\n"
}

type env struct {
	st  *store.Store
	mb  *mailtest.Mailbox
	sup *Supervisor
}

func newEnv(t *testing.T) *env {
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
	st := store.New(db)
	u, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := st.CreateAccount(ctx, make([]byte, 32), store.Account{UserID: u.ID, Label: "x", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "me"}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRule(ctx, 1, rules.Rule{UserID: u.ID, Name: "Reading", Enabled: true,
		Conditions: rules.Cond{Field: "from_domain", Op: rules.OpEq, Value: "news.example"},
		Actions:    []rules.Action{{Type: rules.ActKeep}}}, 1); err != nil {
		t.Fatal(err)
	}
	mb := mailtest.New(acct.ID)
	mb.AddFolder("Sent", mail.RoleSent)
	mb.Deliver("Sent", "From: me@example.test\r\nTo: Friend@People.example\r\nSubject: hi\r\n\r\nhello\r\n")
	hub := events.NewHub()
	return &env{st: st, mb: mb, sup: &Supervisor{
		Account: acct, Store: st, Hub: hub,
		Open:       func(context.Context) (mail.Mailbox, error) { return mb, nil },
		Pipeline:   pipeline.Pipeline{Store: st, Hub: hub, MinConfidence: 0.75, BodyChars: 2000},
		BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	}}
}

// run starts the supervisor and returns a function that stops it and waits for it.
func (e *env) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.sup.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the supervisor did not stop")
		}
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *env) state(t *testing.T, ref mail.MsgRef) string {
	t.Helper()
	rows, err := e.st.Activity(t.Context(), store.Viewer{UserID: 1, TenantID: 1}, store.ActivityFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Message.Location() == ref {
			return r.Message.State
		}
	}
	return ""
}

func TestSupervisorWatchesAndProcesses(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	old := e.mb.Deliver("INBOX", eml("already here"))
	_, live, cancel := e.sup.Hub.Subscribe(0)
	defer cancel()
	stop := e.run(t)

	eventually(t, "status live", func() bool {
		a, _ := e.st.Account(ctx, e.sup.Account.ID)
		return a.Status == StatusLive
	})
	if ev := <-live; ev.Name != events.AccountStatus || ev.Data.(store.Account).Status != StatusLive {
		t.Errorf("first event = %+v", ev)
	}
	// First connect: folders discovered, contacts scanned, and the watch starts after the
	// mail that was already there.
	folders, _ := e.st.Folders(ctx, e.sup.Account.ID)
	isContact, _ := e.st.IsContact(ctx, e.sup.Account.ID, "friend@people.example")
	if len(folders) != 2 || !isContact || e.sup.Mailbox() == nil {
		t.Errorf("folders = %+v, contact = %v, mailbox = %v", folders, isContact, e.sup.Mailbox())
	}
	if f, _ := e.st.Folder(ctx, e.sup.Account.ID, "INBOX"); f.LastUID != old.UID || f.UIDValidity != old.UIDValidity {
		t.Errorf("baseline = %+v, want uid %d", f, old.UID)
	}

	ref := e.mb.Deliver("INBOX", eml("new"))
	eventually(t, "the new message to be processed", func() bool { return e.state(t, ref) == store.StateActed })
	if got := e.state(t, old); got != "" {
		t.Errorf("mail from before the first connect was processed (state %s)", got)
	}
	stop()
	if e.sup.Mailbox() != nil {
		t.Error("mailbox still exposed after the supervisor stopped")
	}

	// A restart resumes from the stored position: mail that arrived meanwhile is caught up.
	missed := e.mb.Deliver("INBOX", eml("while down"))
	stop = e.run(t)
	eventually(t, "catch-up after a restart", func() bool { return e.state(t, missed) == store.StateActed })
	stop()

	// The folder is rebuilt on the server (new UIDVALIDITY): start from its newest message.
	e.mb.AddFolder("INBOX", "")
	rebuilt := e.mb.Deliver("INBOX", eml("restored from backup"))
	stop = e.run(t)
	eventually(t, "the new baseline", func() bool {
		f, _ := e.st.Folder(ctx, e.sup.Account.ID, "INBOX")
		return f.UIDValidity == rebuilt.UIDValidity && f.LastUID == rebuilt.UID
	})
	after := e.mb.Deliver("INBOX", eml("after rebuild"))
	eventually(t, "mail after the rebuild", func() bool { return e.state(t, after) == store.StateActed })
	if got := e.state(t, rebuilt); got != "" {
		t.Errorf("a rebuilt folder was re-sorted (state %s)", got)
	}
	stop()
}

func TestSupervisorStatuses(t *testing.T) {
	tests := []struct {
		name string
		open func(e *env, calls int) (mail.Mailbox, error)
		want string
		stay bool // Run returns on its own
	}{
		{"wrong password stops for good", func(*env, int) (mail.Mailbox, error) { return nil, mail.ErrAuth }, StatusAuthFailed, true},
		{"tls failure stops for good", func(*env, int) (mail.Mailbox, error) { return nil, mail.ErrTLS }, StatusError, true},
		{"a changed certificate stops for good", func(*env, int) (mail.Mailbox, error) {
			return nil, fmt.Errorf("connect: %w", &mail.CertError{Host: "127.0.0.1", Cert: mail.Cert{Fingerprint: "AA"}, Pinned: "BB"})
		}, StatusCertChanged, true},
		{"an untrusted certificate stops for good", func(*env, int) (mail.Mailbox, error) {
			return nil, &mail.CertError{Host: "127.0.0.1", Cert: mail.Cert{Fingerprint: "AA"}, Reason: "self-made"}
		}, StatusCertChanged, true},
		{"missing watch folder stops for good", func(e *env, _ int) (mail.Mailbox, error) {
			e.sup.Account.WatchFolder = "Gone"
			return e.mb, nil
		}, StatusError, true},
		{"a dropped connection is retried", func(e *env, calls int) (mail.Mailbox, error) {
			if calls < 3 {
				return nil, mail.ErrConnection
			}
			return e.mb, nil
		}, StatusLive, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			var calls atomic.Int32
			e.sup.Open = func(context.Context) (mail.Mailbox, error) { return tt.open(e, int(calls.Add(1))) }
			_, live, cancel := e.sup.Hub.Subscribe(0)
			defer cancel()
			stop := e.run(t)
			var a store.Account
			eventually(t, "status "+tt.want, func() bool {
				a, _ = e.st.Account(t.Context(), e.sup.Account.ID)
				return a.Status == tt.want
			})
			if (a.LastError == "") != (tt.want == StatusLive) {
				t.Errorf("last error = %q in status %s", a.LastError, a.Status)
			}
			if tt.stay {
				time.Sleep(20 * time.Millisecond)
				if n := calls.Load(); n != 1 {
					t.Errorf("connected %d times; a fatal error must not be retried", n)
				}
			} else if ev := <-live; ev.Data.(store.Account).Status != StatusReconnecting {
				t.Errorf("first event = %+v, want reconnecting", ev)
			}
			stop()
		})
	}
}

// gated blocks the first Fetch until released, so mail piles up in the queue.
type gated struct {
	*mailtest.Mailbox
	started chan struct{}
	release chan struct{}
}

func (g *gated) Fetch(ctx context.Context, ref mail.MsgRef, maxBody int) (*message.Raw, error) {
	select {
	case g.started <- struct{}{}:
		<-g.release
	default:
	}
	return g.Mailbox.Fetch(ctx, ref, maxBody)
}

func TestSupervisorDrainsQueuedMailOnShutdown(t *testing.T) {
	e := newEnv(t)
	g := &gated{Mailbox: e.mb, started: make(chan struct{}), release: make(chan struct{})}
	e.sup.Open = func(context.Context) (mail.Mailbox, error) { return g, nil }
	stop := e.run(t)
	eventually(t, "status live", func() bool {
		a, _ := e.st.Account(t.Context(), e.sup.Account.ID)
		return a.Status == StatusLive
	})
	refs := []mail.MsgRef{e.mb.Deliver("INBOX", eml("one")), e.mb.Deliver("INBOX", eml("two")), e.mb.Deliver("INBOX", eml("three"))}
	<-g.started // the processor holds message one; two and three wait in the queue
	eventually(t, "the watcher to queue the rest", func() bool {
		f, _ := e.st.Activity(t.Context(), store.Viewer{UserID: 1, TenantID: 1}, store.ActivityFilter{})
		return len(f) == 1
	})
	time.Sleep(20 * time.Millisecond)
	go func() { time.Sleep(20 * time.Millisecond); close(g.release) }()
	stop() // returns only after the queue is drained
	for _, ref := range refs {
		if got := e.state(t, ref); got != store.StateActed {
			t.Errorf("uid %d left in state %q at shutdown", ref.UID, got)
		}
	}

	// With no time to drain, shutdown still returns promptly and loses nothing: the
	// position stays behind the unprocessed mail.
	e.sup.DrainTimeout = time.Nanosecond
	g.started, g.release = make(chan struct{}), make(chan struct{})
	stop = e.run(t)
	late := e.mb.Deliver("INBOX", eml("four"))
	<-g.started
	go func() { time.Sleep(20 * time.Millisecond); close(g.release) }()
	stop()
	if f, _ := e.st.Folder(t.Context(), e.sup.Account.ID, "INBOX"); f.LastUID != refs[2].UID {
		t.Errorf("last_uid = %d, want %d (uid %d was cut off)", f.LastUID, refs[2].UID, late.UID)
	}
	if errors.Is(t.Context().Err(), context.Canceled) {
		t.Fatal("test context ended early")
	}
}
