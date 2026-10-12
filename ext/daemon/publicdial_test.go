package daemon

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// watchedPort is a port on 127.0.0.1 that counts the connections made to it.
func watchedPort(t *testing.T) (port int, accepted *atomic.Int32) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	accepted = &atomic.Int32{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, accepted
}

// In the hosted build every connection the daemon makes to a server a person named, for
// the connection test, a supervisor or the command line, refuses an address off the public
// internet before connecting. A mailbox a module added is not checked, and self-host never.
func TestCloudModeConnectsOnlyToPublicServers(t *testing.T) {
	ctx := t.Context()
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	port, accepted := watchedPort(t)
	named := store.Account{Preset: "generic", Host: "127.0.0.1", Port: port, TLSMode: presets.TLSImplicit, Username: imaptest.Username}

	st, _ := storetest.New(t)
	user, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	named.UserID = user.ID
	stored, err := st.CreateAccount(ctx, named, imaptest.Password)
	if err != nil {
		t.Fatal(err)
	}
	logins := &mailLogins{st: st, locks: map[int64]*sync.Mutex{}, cloud: true}
	oneClick := stored
	oneClick.OAuth = true

	for name, connect := range map[string]func() error{
		"the connection test and create": func() error {
			_, _, err := dial(ctx, named, imaptest.Password, nil, true)
			return err
		},
		"a supervisor": func() error { _, err := openAccount(logins, stored)(ctx); return err },
		"a one-click sign-in": func() error {
			_, err := logins.open(ctx, oneClick, nil)
			return err
		},
		"accounts test": func() error {
			_, err := accountsCLI{cloud: true}.open(ctx, named, presets.Preset{}, imaptest.Username, imaptest.Password)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := connect(); !errors.Is(err, mail.ErrNotPublic) {
				t.Fatalf("connect to 127.0.0.1 in cloud mode = %v, want ErrNotPublic", err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Errorf("the private server saw %d connections", n)
	}

	// The same server is reached in self-host, and in cloud mode when a module added the
	// mailbox: the test server is on 127.0.0.1 too.
	onServer := named
	onServer.Host, onServer.Port = srv.Host, srv.Port
	for name, acct := range map[string]struct {
		cloud  bool
		module bool
	}{"self-host": {false, false}, "cloud, a module's mailbox": {true, true}} {
		t.Run(name, func(t *testing.T) {
			a := onServer
			a.Module = acct.module
			mb, _, err := dial(ctx, a, imaptest.Password, srv.TLS, acct.cloud)
			if err != nil {
				t.Fatalf("dial = %v", err)
			}
			_ = mb.Close()
		})
	}
}

// The command line in cloud mode refuses to add a mailbox on a private server, and to
// test one stored before.
func TestAccountsCLIInCloudMode(t *testing.T) {
	ctx := t.Context()
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	dir := t.TempDir()
	env := map[string]string{"MAILRULES_DATA_DIR": dir, "MAILRULES_MASTER_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))}
	run := func(args ...string) (string, error) {
		out := &syncBuffer{}
		err := accountsCLI{stdin: strings.NewReader(imaptest.Password + "\n"), stdout: out, getenv: func(k string) string { return env[k] }, tlsConfig: srv.TLS}.run(ctx, args)
		return out.String(), err
	}
	st, _ := storetest.InDir(t, dir)
	if _, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	add := []string{"add", "--preset", "generic", "--host", srv.Host, "--port", strconv.Itoa(srv.Port), "--username", imaptest.Username}

	// Added in self-host, where any server goes.
	if _, err := run(add...); err != nil {
		t.Fatal(err)
	}
	env["MAILRULES_MODE"] = "cloud"
	if _, err := run("test", "1"); !errors.Is(err, mail.ErrNotPublic) {
		t.Errorf("accounts test in cloud mode = %v, want ErrNotPublic", err)
	}
	if _, err := st.DeleteAccount(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := run(add...); !errors.Is(err, mail.ErrNotPublic) {
		t.Errorf("accounts add in cloud mode = %v, want ErrNotPublic", err)
	}
	if list, _ := st.Accounts(ctx); len(list) != 0 {
		t.Errorf("a refused add stored %+v", list)
	}
}
