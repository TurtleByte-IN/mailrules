package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// fakeUndoer is a module's MailboxRemoved: it refuses the first `refuse` hand-overs, then
// keeps what it is told.
type fakeUndoer struct {
	mu     sync.Mutex
	refuse int
	calls  []int64 // the RemovedMailbox.ID of every hand-over, refused or not
	got    []ext.RemovedMailbox
	before func() // when set, runs at each hand-over, before it is answered
}

func (f *fakeUndoer) removed(_ context.Context, m ext.RemovedMailbox) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, m.ID)
	if f.before != nil {
		f.before()
	}
	if f.refuse > 0 {
		f.refuse--
		return errors.New("bridge is restarting")
	}
	f.got = append(f.got, m)
	return nil
}

func (f *fakeUndoer) told() ([]int64, []ext.RemovedMailbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), slices.Clone(f.got)
}

// removedWorld is a database with one hosted person who has a one-click Gmail mailbox, a
// Proton one a sign-in form connected, and one of their own with an app password.
type removedWorld struct {
	dir                     string
	db                      *sql.DB
	st                      *store.Store
	gmail, proton, password store.Account
}

func newRemovedWorld(t *testing.T) removedWorld {
	t.Helper()
	ctx := t.Context()
	w := removedWorld{dir: t.TempDir()}
	w.st, w.db = storetest.InDir(t, w.dir)
	u, err := w.st.SignInIdentity(ctx, store.Identity{Provider: "workos", Subject: "user_jo", Email: "jo@example.test", Tenant: "org_jo"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	add := func(a store.Account, secret string) store.Account {
		a.UserID, a.Host, a.Port, a.TLSMode = u.ID, "127.0.0.1", 993, "implicit"
		a.Label = a.Username
		out, err := w.st.CreateAccount(ctx, a, secret)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	w.gmail = add(store.Account{Preset: "gmail", Username: "jo@gmail.test", OAuth: true, Module: true}, "refresh-1")
	w.proton = add(store.Account{Preset: "proton", Username: "jo@proton.test", Module: true}, "bridge-password")
	w.password = add(store.Account{Preset: "generic", Username: "jo@example.test"}, "app-password")
	return w
}

// undoModules are a one-click module for Gmail and Outlook and a sign-in form module for
// Proton, told through oneClick and form.
func undoModules(oneClick, form *fakeUndoer) []ext.Module {
	return []ext.Module{
		{Name: "oneclick", Mailboxes: &ext.MailboxSignIn{Providers: []string{"gmail", "outlook"}}, MailboxRemoved: oneClick.removed},
		{Name: "proton", MailboxForms: []ext.MailboxForm{{Provider: "proton"}}, MailboxRemoved: form.removed},
	}
}

// Removing a mailbox and forgetting its owner each tell the module that connected it its
// provider, address and secret, whatever the module answers: the removal is done first. A
// refused one is handed over again later, the same, until it is taken, then never again; a
// mailbox the person connected with their own password is told to nobody.
func TestRemovedMailboxesReachTheirModule(t *testing.T) {
	w := newRemovedWorld(t)
	ctx := t.Context()
	oneClick, form := &fakeUndoer{refuse: 1}, &fakeUndoer{}
	now := time.Unix(1_800_000_000, 0)
	rw := &removedWorker{store: w.st, modules: undoModules(oneClick, form), now: func() time.Time { return now }}

	for _, id := range []int64{w.gmail.ID, w.password.ID} {
		if _, err := w.st.DeleteAccount(ctx, id, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.st.ForgetIdentity(ctx, "workos", "user_jo", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.Account(ctx, w.proton.ID); err != nil {
		t.Fatal(err) // forgotten, not yet removed
	}

	if err := rw.once(ctx); err != nil {
		t.Fatal(err)
	}
	calls, got := oneClick.told()
	if len(calls) != 1 || len(got) != 0 {
		t.Fatalf("one-click module after a refusal: calls %v, taken %+v", calls, got)
	}
	if _, got := form.told(); len(got) != 1 || got[0] != (ext.RemovedMailbox{ID: got[0].ID, AccountID: w.proton.ID, Provider: "proton",
		Address: "jo@proton.test", Secret: "bridge-password"}) {
		t.Fatalf("form module got %+v", got)
	}

	now = now.Add(noticeRetryFirst - time.Second)
	if err := rw.once(ctx); err != nil {
		t.Fatal(err)
	}
	if calls, _ := oneClick.told(); len(calls) != 1 {
		t.Fatalf("handed over again too soon: %v", calls)
	}
	now = now.Add(time.Second)
	if err := rw.once(ctx); err != nil {
		t.Fatal(err)
	}
	calls, got = oneClick.told()
	want := []ext.RemovedMailbox{{ID: calls[0], AccountID: w.gmail.ID, Provider: "gmail", Address: "jo@gmail.test", Secret: "refresh-1"}}
	if len(calls) != 2 || calls[1] != calls[0] || !slices.Equal(got, want) {
		t.Fatalf("one-click module: calls %v, taken %+v; want %+v handed over again and taken", calls, got, want)
	}

	now = now.Add(24 * time.Hour)
	if err := rw.once(ctx); err != nil {
		t.Fatal(err)
	}
	if c1, _ := oneClick.told(); len(c1) != 2 {
		t.Errorf("one-click module told again: %v", c1)
	}
	if c2, _ := form.told(); len(c2) != 1 {
		t.Errorf("form module told again: %v", c2)
	}
	var left int
	if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM removed_mailboxes`).Scan(&left); err != nil || left != 0 {
		t.Errorf("secrets kept after they were taken: %d, %v", left, err)
	}
}

// A removal is told after a restart: when the daemon stopped between removing the mailbox
// and telling the module, and when it stopped after the module took it but before that was
// recorded, which hands the same one (the same ID) over again.
func TestRemovedMailboxSurvivesRestart(t *testing.T) {
	w := newRemovedWorld(t)
	ctx := t.Context()
	now := time.Unix(1_800_000_000, 0)
	if _, err := w.st.DeleteAccount(ctx, w.gmail.ID, now.Unix()); err != nil {
		t.Fatal(err)
	}
	_ = w.db.Close() // the daemon stops before the worker ran

	st, db := storetest.InDir(t, w.dir)
	oneClick := &fakeUndoer{}
	oneClick.before = func() { _ = db.Close() } // and stops again as the module takes it
	rw := &removedWorker{store: st, modules: undoModules(oneClick, &fakeUndoer{}), now: func() time.Time { return now }}
	if err := rw.once(ctx); err == nil {
		t.Fatal("recording the hand-over on a closed database did not fail")
	}

	st, _ = storetest.InDir(t, w.dir)
	again := &fakeUndoer{}
	rw = &removedWorker{store: st, modules: undoModules(again, &fakeUndoer{}), now: func() time.Time { return now }}
	if err := rw.once(ctx); err != nil {
		t.Fatal(err)
	}
	_, first := oneClick.told()
	_, second := again.told()
	if len(first) != 1 || !slices.Equal(second, first) || first[0].Secret != "refresh-1" || first[0].AccountID != w.gmail.ID {
		t.Errorf("before the restart %+v, after it %+v; want the same mailbox told again", first, second)
	}
}

// A module that takes no removals is never called and its mailboxes are dropped; one whose
// module is not in the build waits for it, and is dropped with its secret after
// RemovedMailboxDays; a secret the master key cannot open is told as empty.
func TestRemovedMailboxWithoutItsModule(t *testing.T) {
	w := newRemovedWorld(t)
	ctx := t.Context()
	now := time.Unix(1_800_000_000, 0)
	if _, err := w.st.ForgetIdentity(ctx, "workos", "user_jo", now.Unix()); err != nil {
		t.Fatal(err)
	}
	form := &fakeUndoer{}
	// The build has the one-click module, without MailboxRemoved, and no Proton module.
	modules := []ext.Module{{Name: "oneclick", Mailboxes: &ext.MailboxSignIn{Providers: []string{"gmail"}}}}
	rw := &removedWorker{store: w.st, modules: modules, now: func() time.Time { return now }}
	if err := rw.once(ctx); err != nil {
		t.Fatal(err)
	}
	waiting, err := w.st.WaitingRemovedMailboxes(ctx, now.Add(time.Hour).Unix(), 10)
	if err != nil || len(waiting) != 1 || waiting[0].AccountID != w.proton.ID || waiting[0].Attempts != 1 {
		t.Fatalf("waiting = %+v, %v; want only the Proton one, an hour later", waiting, err)
	}

	// The Proton module comes back, with another master key.
	now = now.Add(time.Hour)
	rw = &removedWorker{store: store.New(w.db).WithKeys(testKey(9)), modules: undoModules(&fakeUndoer{}, form), now: func() time.Time { return now }}
	if err := rw.once(ctx); err != nil {
		t.Fatal(err)
	}
	if _, got := form.told(); len(got) != 1 || got[0].Secret != "" || got[0].Address != "jo@proton.test" {
		t.Errorf("told %+v; want the address without the secret", got)
	}

	// One no module ever takes is dropped after RemovedMailboxDays.
	if _, err := w.st.DeleteAccount(ctx, w.proton.ID, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := w.st.SetAccountSecret(ctx, w.gmail.ID, "refresh-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.DeleteAccount(ctx, w.gmail.ID, now.Unix()); err != nil {
		t.Fatal(err)
	}
	rw = &removedWorker{store: w.st, modules: nil, now: func() time.Time { return now }}
	for range 2 {
		if err := rw.once(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(store.RemovedMailboxDays * 24 * time.Hour)
	}
	var left int
	if err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM removed_mailboxes`).Scan(&left); err != nil || left != 0 {
		t.Errorf("rows left: %d, %v", left, err)
	}
}

// End to end: removing a one-click mailbox in Mailboxes answers at once, and within seconds
// the module is told the refresh token it gave.
func TestMailboxRemovedEndToEnd(t *testing.T) {
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	oauth := &fakeOAuth{srv: srv, rotate: map[string]string{}, revoked: map[string]bool{}}
	undo := &fakeUndoer{}
	m := oauth.module()
	m.MailboxRemoved = undo.removed
	c := serveForTest(t, srv.TLS, m).c

	hdr, _ := c.do(http.MethodGet, "/api/oauthtest/connect?provider=gmail", nil, http.StatusSeeOther)
	var account int64
	if _, err := fmt.Sscanf(hdr.Get("Location"), "/#/accounts?added=%d", &account); err != nil {
		t.Fatalf("added: %q", hdr.Get("Location"))
	}
	c.do(http.MethodDelete, "/api/accounts/"+strconv.FormatInt(account, 10), nil, http.StatusNoContent)
	c.do(http.MethodGet, "/api/accounts/"+strconv.FormatInt(account, 10), nil, http.StatusNotFound)
	eventually(t, "the module takes it", func() (bool, string) {
		_, got := undo.told()
		return len(got) == 1, fmt.Sprint(got)
	})
	if _, got := undo.told(); got[0].AccountID != account || got[0].Provider != "gmail" || got[0].Address != imaptest.Username || got[0].Secret != "refresh-1" {
		t.Errorf("told %+v", got[0])
	}
}
