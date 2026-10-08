package daemon

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// A mailbox added with the command line while the daemon runs is started by the next poll,
// once; accounts the daemon already knew, and paused ones, are left alone.
func TestAccountWatcherPicksUpCLIAccounts(t *testing.T) {
	ctx := t.Context()
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	dir := t.TempDir()
	env := map[string]string{
		"MAILRULES_DATA_DIR":   dir,
		"MAILRULES_MASTER_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
	}
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	if _, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	add := func(label string) string {
		out := &syncBuffer{}
		err := accountsCLI{stdin: strings.NewReader(imaptest.Password + "\n"), stdout: out, getenv: func(k string) string { return env[k] }, tlsConfig: srv.TLS}.run(ctx,
			[]string{"add", "--preset", "generic", "--host", srv.Host, "--port", strconv.Itoa(srv.Port), "--username", imaptest.Username, "--label", label})
		if err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	add("before")
	existing, err := st.Accounts(ctx)
	if err != nil || len(existing) != 1 {
		t.Fatalf("accounts: %v %v", existing, err)
	}
	var started []int64
	w := newAccountWatcher(st, func(a store.Account) { started = append(started, a.ID) })
	w.Seed(existing)

	if n, err := w.Poll(ctx); err != nil || n != 0 {
		t.Fatalf("poll with nothing new: %d, %v", n, err)
	}
	out := add("after")
	if !strings.Contains(out, "no restart is needed") {
		t.Errorf("add output does not say a restart is unnecessary: %q", out)
	}
	if n, err := w.Poll(ctx); err != nil || n != 1 {
		t.Fatalf("poll after add: %d, %v", n, err)
	}
	all, _ := st.Accounts(ctx)
	if len(started) != 1 || started[0] != all[1].ID {
		t.Fatalf("started %v, want [%d]", started, all[1].ID)
	}
	if n, _ := w.Poll(ctx); n != 0 {
		t.Errorf("an account must be started once, got %d more", n)
	}

	// An account added paused is recorded but not started.
	add("paused")
	all, _ = st.Accounts(ctx)
	if err := st.SetAccountStatus(ctx, all[2].ID, worker.StatusPaused, "", 1); err != nil {
		t.Fatal(err)
	}
	if n, _ := w.Poll(ctx); n != 0 {
		t.Errorf("paused account started")
	}
	// The HTTP layer's start marks an account seen too.
	w.Start(all[1])
	if len(started) != 2 {
		t.Errorf("Start did not call through: %v", started)
	}
}
