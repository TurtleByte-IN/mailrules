package worker

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// removalWorld is a database in dir with two hosted people, each the only member of their
// tenant with one mailbox: gone, whom the sign-in module forgets, and kept.
type removalWorld struct {
	dir                string
	gone, kept         store.User
	goneAcct, keptAcct store.Account
}

func openAt(t *testing.T, dir string) (*store.Store, *sql.DB) {
	t.Helper()
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	return store.New(db), db
}

func newRemovalWorld(t *testing.T) (removalWorld, *store.Store, *sql.DB) {
	t.Helper()
	ctx := t.Context()
	w := removalWorld{dir: t.TempDir()}
	st, db := openAt(t, w.dir)
	person := func(sub, email, org string) (store.User, store.Account) {
		u, err := st.SignInIdentity(ctx, store.Identity{Provider: "workos", Subject: sub, Email: email, Tenant: org}, 1)
		if err != nil {
			t.Fatal(err)
		}
		a, err := st.CreateAccount(ctx, make([]byte, 32), store.Account{UserID: u.ID, Label: sub, Preset: "generic",
			Host: "h", Port: 993, TLSMode: "implicit", Username: email}, "pw")
		if err != nil {
			t.Fatal(err)
		}
		return u, a
	}
	w.gone, w.goneAcct = person("user_gone", "gone@example.com", "org_gone")
	w.kept, w.keptAcct = person("user_kept", "kept@example.com", "org_kept")
	return w, st, db
}

// supervise starts a supervisor for acct on a fake mailbox.
func supervise(ctx context.Context, m *Manager, st *store.Store, acct store.Account) {
	mb := mailtest.New(acct.ID)
	hub := events.NewHub()
	m.Start(ctx, &Supervisor{
		Account: acct, Store: st, Hub: hub,
		Open:       func(context.Context) (mail.Mailbox, error) { return mb, nil },
		Pipeline:   pipeline.Pipeline{Store: st, Hub: hub, MinConfidence: 0.75, BodyChars: 2000},
		BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond, RetryEvery: 10 * time.Millisecond,
	})
}

// A person forgotten before the daemon stopped is removed by the daemon that starts next,
// once the wait is over: their mailbox, its secret deleted when they were forgotten, is not
// started (no login) and is stopped before it goes; nobody else's is touched.
func TestRemovalJobSurvivesARestart(t *testing.T) {
	w, st, db := newRemovalWorld(t)
	ctx := t.Context()
	now := time.Unix(1_800_000_000, 0)
	if _, err := st.ForgetIdentity(ctx, "workos", "user_gone", now.Unix()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close() // the daemon stops in the middle of the wait

	st, db = openAt(t, w.dir)
	t.Cleanup(func() { _ = db.Close() })
	m := &Manager{}
	runCtx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); m.Wait() }()
	gone, err := st.Account(ctx, w.goneAcct.ID) // as the daemon reads it when it starts
	if err != nil || !gone.SecretGone {
		t.Fatalf("the forgotten mailbox = %+v, %v; want its secret gone", gone, err)
	}
	supervise(runCtx, m, st, gone)
	supervise(runCtx, m, st, w.keptAcct)
	if m.supervisor(w.goneAcct.ID) != nil || m.supervisor(w.keptAcct.ID) == nil {
		t.Fatal("a mailbox without its secret was started, or the other one was not")
	}

	var stopped []int64
	job := Removal{Store: st, Hub: events.NewHub(),
		Stop:  func(id int64) { stopped = append(stopped, id); m.Stop(id) },
		Start: func(a store.Account) { t.Errorf("started account %d again", a.ID) },
		Now:   func() time.Time { return now },
	}
	for _, c := range []struct {
		at   time.Time
		want int
	}{
		{now.Add(store.ForgetDays*24*time.Hour - time.Second), 0},
		{now.Add(store.ForgetDays * 24 * time.Hour), 1},
		{now.Add(store.ForgetDays * 48 * time.Hour), 0}, // done already
	} {
		now = c.at
		removed, err := job.Once(ctx)
		if err != nil || removed != c.want {
			t.Errorf("at %v: removed %d, %v; want %d", c.at, removed, err, c.want)
		}
	}
	if len(stopped) != 1 || stopped[0] != w.goneAcct.ID {
		t.Errorf("stopped %v, want only the forgotten person's mailbox %d", stopped, w.goneAcct.ID)
	}
	if m.supervisor(w.goneAcct.ID) != nil || m.supervisor(w.keptAcct.ID) == nil {
		t.Error("the wrong supervisors are running")
	}
	if _, err := st.Account(ctx, w.goneAcct.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the forgotten mailbox: %v", err)
	}
	if _, err := st.User(ctx, w.gone.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the forgotten user: %v", err)
	}
	if _, err := st.Account(ctx, w.keptAcct.ID); err != nil {
		t.Errorf("the other mailbox: %v", err)
	}
}

// A person who signs in again after their mailbox was stopped for the removal, and before
// it happened, keeps everything; their mailbox, whose secret went when they were
// forgotten, stays stopped until they enter a new password.
func TestRemovalCalledOffKeepsTheMailboxStopped(t *testing.T) {
	w, st, db := newRemovalWorld(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	now := time.Unix(1_800_000_000, 0)
	if _, err := st.ForgetIdentity(ctx, "workos", "user_gone", now.Unix()); err != nil {
		t.Fatal(err)
	}
	var started []int64
	job := Removal{Store: st,
		Stop: func(int64) {
			if _, err := st.SignInIdentity(ctx, store.Identity{Provider: "workos", Subject: "user_gone",
				Email: "gone@example.com", Tenant: "org_gone"}, now.Unix()); err != nil {
				t.Fatal(err)
			}
		},
		Start: func(a store.Account) { started = append(started, a.ID) },
		Now:   func() time.Time { return now.Add(store.ForgetDays * 24 * time.Hour) },
	}
	if removed, err := job.Once(ctx); err != nil || removed != 0 {
		t.Errorf("removed %d, %v; want 0", removed, err)
	}
	if len(started) != 0 {
		t.Errorf("started %v, want none: the mailbox has no secret", started)
	}
	if a, err := st.Account(ctx, w.goneAcct.ID); err != nil || a.Status != StatusAuthFailed || !a.SecretGone {
		t.Errorf("the mailbox of the person who came back = %+v, %v; want it kept, auth_failed", a, err)
	}
}
