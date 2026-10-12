package worker

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
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
	st, _ := storetest.New(t)
	u, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := st.CreateAccount(ctx, store.Account{UserID: u.ID, Label: "x", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "me"}, "pw")
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
		{"a revoked one-click sign-in stops for good", func(*env, int) (mail.Mailbox, error) {
			return nil, fmt.Errorf("sign in: %w", mail.ErrReconnect)
		}, StatusReconnectNeeded, true},
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
		{"a server now off the public internet stops for good", func(*env, int) (mail.Mailbox, error) {
			return nil, fmt.Errorf("connect to imap.example.test:993: imap.example.test is at 10.0.0.1: %w", mail.ErrNotPublic)
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

// A password a module gave the daemon (Bridge's, for a mailbox its sign-in form added) is
// one the person never saw: when the server refuses it, the mailbox waits for them to sign
// in through the module again (reconnect_needed), not for a new password.
func TestSupervisorModuleMailboxRefused(t *testing.T) {
	e := newEnv(t)
	e.sup.Account.Module = true
	var calls atomic.Int32
	e.sup.Open = func(context.Context) (mail.Mailbox, error) {
		calls.Add(1)
		return nil, fmt.Errorf("log in to 127.0.0.1:1143: %w", mail.ErrAuth)
	}
	stop := e.run(t)
	defer stop()
	eventually(t, "status reconnect_needed", func() bool {
		a, _ := e.st.Account(t.Context(), e.sup.Account.ID)
		return a.Status == StatusReconnectNeeded && a.LastError != ""
	})
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("connected %d times; a refused password must not be retried", n)
	}
}

// A server that resolves off the public internet in the hosted build stops the account in
// error without retrying, and the person reads only the sentence, not the address.
func TestSupervisorServerNotPublic(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	e.sup.Open = func(context.Context) (mail.Mailbox, error) {
		calls.Add(1)
		return nil, fmt.Errorf("connect to imap.example.test:993: imap.example.test is at 10.0.0.1: %w", mail.ErrNotPublic)
	}
	stop := e.run(t)
	defer stop()
	var a store.Account
	eventually(t, "status error", func() bool {
		a, _ = e.st.Account(t.Context(), e.sup.Account.ID)
		return a.Status == StatusError
	})
	if a.LastError != "the hosted version of MailRules can only connect to mail servers on the public internet" {
		t.Errorf("last error = %q", a.LastError)
	}
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("connected %d times; it must not be retried", n)
	}
}

// A mailbox whose password the key service cannot open right now waits in key_unavailable,
// saying so rather than the provider's error, and is retried until the service answers:
// nothing is wrong with its sign-in, also for one a module connected, whose token comes
// from the key service's tenant key in the middle of logging in.
func TestSupervisorKeyServiceUnreachable(t *testing.T) {
	for name, failure := range map[string]error{
		"password": fmt.Errorf("decrypt account secret: open the key of tenant 1: %w: rpc error: code = Unavailable", crypto.ErrKeyUnavailable),
		"one-click token": fmt.Errorf("sign in to imap.gmail.com:993: get an access token: %w: %w", mail.ErrConnection,
			fmt.Errorf("open the key of tenant 1: %w: deadline exceeded", crypto.ErrKeyUnavailable)),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.sup.Account.OAuth, e.sup.Account.Module = name != "password", name != "password"
			e.sup.BackoffMin, e.sup.BackoffMax = 10*time.Millisecond, 10*time.Millisecond
			var calls atomic.Int32
			release := make(chan struct{})
			e.sup.Open = func(context.Context) (mail.Mailbox, error) {
				calls.Add(1)
				select {
				case <-release:
					return e.mb, nil
				default:
					return nil, failure
				}
			}
			_, evs, cancel := e.sup.Hub.Subscribe(0)
			defer cancel()
			stop := e.run(t)
			defer stop()
			var a store.Account
			eventually(t, "status key_unavailable", func() bool {
				a, _ = e.st.Account(t.Context(), e.sup.Account.ID)
				return a.Status == StatusKeyUnavailable
			})
			if a.LastError != errKeyUnavailable.Error() {
				t.Errorf("last error = %q", a.LastError)
			}
			eventually(t, "retries", func() bool { return calls.Load() >= 3 })
			close(release)
			eventually(t, "status live", func() bool {
				a, _ = e.st.Account(t.Context(), e.sup.Account.ID)
				return a.Status == StatusLive
			})
			// One status change for the outage however often it was retried, then live.
			for _, want := range []string{StatusKeyUnavailable, StatusLive} {
				if ev := <-evs; ev.Data.(store.Account).Status != want {
					t.Fatalf("event = %+v, want %s", ev.Data, want)
				}
			}
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
