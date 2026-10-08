package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// syncBuffer lets the test read output while a command is still writing it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAccountsAddListTest(t *testing.T) {
	ctx := t.Context()
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	for _, name := range []string{"Sent Messages", "Junk"} {
		if err := srv.User.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	srv.Append(t, "INBOX", "From: old@example.test\r\nSubject: already here\r\n\r\nx\r\n")
	srv.Append(t, "Sent Messages", "To: friend@example.test\r\nSubject: hi\r\n\r\nx\r\n")

	dir := t.TempDir()
	env := map[string]string{
		"MAILRULES_DATA_DIR":   dir,
		"MAILRULES_MASTER_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
	}
	run := func(ctx context.Context, stdin string, args ...string) (string, error) {
		out := &syncBuffer{}
		err := accountsCLI{stdin: strings.NewReader(stdin), stdout: out, getenv: func(k string) string { return env[k] }, tlsConfig: srv.TLS}.run(ctx, args)
		return out.String(), err
	}
	add := []string{"add", "--preset", "generic", "--host", srv.Host, "--port", strconv.Itoa(srv.Port), "--username", imaptest.Username, "--label", "Test box"}

	if _, err := run(ctx, imaptest.Password+"\n", add...); err == nil || !strings.Contains(err.Error(), "first-run setup") {
		t.Fatalf("add before setup: %v", err)
	}
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := store.New(db)
	if _, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}

	// A wrong password stores nothing and does not echo the password back.
	_, err = run(ctx, "not-the-password\n", add...)
	if !errors.Is(err, mail.ErrAuth) || strings.Contains(err.Error(), "not-the-password") {
		t.Fatalf("add with a wrong password: %v", err)
	}
	if list, _ := st.Accounts(ctx); len(list) != 0 {
		t.Fatalf("a failed add stored %+v", list)
	}
	for _, bad := range [][]string{
		{"add", "--preset", "gmail", "--username", "x"},
		{"add", "--preset", "generic", "--username", "x"}, // generic needs --host
		{"add", "--preset", "icloud"},
		{"add", "--password", "x"}, // never a flag
		{"test"}, {"test", "abc"}, {"test", "99"}, {"remove"}, {},
	} {
		if _, err := run(ctx, "pw\n", bad...); err == nil {
			t.Errorf("accounts %v: want an error", bad)
		}
	}

	out, err := run(ctx, imaptest.Password+"\n", add...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added account 1") || !strings.Contains(out, "3 folders, 1 contacts") || !strings.Contains(out, "after UID 1") {
		t.Errorf("add output: %s", out)
	}
	acct, err := st.Account(ctx, 1)
	if err != nil || acct.Label != "Test box" || acct.Username != imaptest.Username || acct.TLSMode != "implicit" || len(acct.Capabilities) == 0 {
		t.Fatalf("stored account = %+v, %v", acct, err)
	}
	// First connect baselines the inbox, records roles and indexes sent mail.
	if f, err := st.Folder(ctx, 1, "INBOX"); err != nil || f.LastUID != 1 || f.UIDValidity == 0 {
		t.Errorf("INBOX = %+v, %v", f, err)
	}
	if f, err := st.Folder(ctx, 1, "Sent Messages"); err != nil || f.SpecialUse != mail.RoleSent {
		t.Errorf("Sent Messages = %+v, %v", f, err)
	}
	if ok, err := st.IsContact(ctx, 1, "friend@example.test"); err != nil || !ok {
		t.Errorf("contact not indexed: %v, %v", ok, err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		if data, _ := os.ReadFile(f); bytes.Contains(data, []byte(imaptest.Password)) {
			t.Errorf("%s holds the password in plaintext", filepath.Base(f))
		}
	}

	// The password can also come from a file or the environment.
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte(imaptest.Password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(ctx, "", append(add, "--password-file", pwFile)...); err != nil || !strings.Contains(out, "added account 2") {
		t.Errorf("add with --password-file: %s, %v", out, err)
	}
	env["MAILRULES_ACCOUNT_PASSWORD"] = imaptest.Password
	if out, err := run(ctx, "", append(add, "--watch-folder", "Nowhere")...); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("add with a missing watch folder: %s, %v", out, err)
	}
	delete(env, "MAILRULES_ACCOUNT_PASSWORD")

	out, err = run(ctx, "", "list")
	if err != nil || !strings.Contains(out, "Test box") || !strings.Contains(out, "new") || strings.Count(out, "\n") != 3 || strings.Contains(out, imaptest.Password) {
		t.Errorf("list: %s, %v", out, err)
	}

	out, err = run(ctx, "", "test", "1")
	if err != nil || !strings.Contains(out, "ok: logged in") || !strings.Contains(out, `\Sent -> Sent Messages`) {
		t.Errorf("test: %s, %v", out, err)
	}

	// test --watch prints sender and subject of new mail, never the body, and marks nothing read.
	wctx, cancel := context.WithCancel(ctx)
	watchOut := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- accountsCLI{stdin: strings.NewReader(""), stdout: watchOut, getenv: func(k string) string { return env[k] }, tlsConfig: srv.TLS}.
			run(wctx, []string{"test", "1", "--watch"})
	}()
	waitFor(t, func() bool { return strings.Contains(watchOut.String(), "watching INBOX") })
	srv.Append(t, "INBOX", "From: Shop <news@shop.example>\r\nSubject: Big sale\r\n\r\nSECRET-BODY-TEXT\r\n")
	waitFor(t, func() bool {
		return strings.Contains(watchOut.String(), `new mail uid=2 from=news@shop.example subject="Big sale"`)
	})
	cancel()
	if err := <-done; err != nil {
		t.Errorf("test --watch returned %v", err)
	}
	if got := watchOut.String(); strings.Contains(got, "SECRET-BODY-TEXT") || strings.Contains(got, "already here") {
		t.Errorf("watch output leaked a body or reported old mail: %s", got)
	}
	if st, err := srv.User.Status("INBOX", &imap.StatusOptions{NumUnseen: true}); err != nil || *st.NumUnseen != 2 {
		t.Errorf("unseen after watching = %v, %v, want 2", st.NumUnseen, err)
	}
}

// waitFor polls for up to 15 s: IMAP push can take several seconds on a loaded CI runner
// with the race detector on, and a passing condition returns at once.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 1500 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 15 s")
}
