package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const oldAdminPassword = "the old admin password"

// adminDir makes a data directory with an admin account and one signed-in session.
func adminDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	hash, err := crypto.HashPassword(oldAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateFirstUser(t.Context(), "me@example.test", hash, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(t.Context(), "token-hash", u.ID, 100, 1<<40); err != nil {
		t.Fatal(err)
	}
	return dir
}

// adminState reads back whether password opens the account and whether the session is alive.
func adminState(t *testing.T, dir, password string) (passwordWorks, sessionAlive bool) {
	t.Helper()
	db, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := store.New(db)
	u, err := st.UserByEmail(t.Context(), "me@example.test")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.SessionUser(t.Context(), "token-hash", 200)
	return crypto.VerifyPassword(u.PasswordHash, password), err == nil
}

func TestUsersResetPassword(t *testing.T) {
	const next = "a brand new password"
	tests := []struct {
		name  string
		stdin string
		env   map[string]string
		file  string // contents of a --password-file; empty = none
		want  string // the password that works afterwards
		fail  string // substring of the error; empty = success
	}{
		{name: "one line on standard input", stdin: next + "\n", want: next},
		{name: "windows line ending", stdin: next + "\r\n", want: next},
		{name: "no trailing newline", stdin: next, want: next},
		{name: "spaces are part of the password", stdin: "  " + next + "  \n", want: "  " + next + "  "},
		{name: "environment", env: map[string]string{"MAILRULES_USER_PASSWORD": next}, want: next},
		{name: "environment beats standard input", stdin: "stdin is ignored here\n", env: map[string]string{"MAILRULES_USER_PASSWORD": next}, want: next},
		{name: "password file", file: next + "\n", want: next},
		{name: "too short", stdin: "short\n", want: oldAdminPassword, fail: "at least 12 characters"},
		{name: "exactly twelve", stdin: "twelve chars\n", want: "twelve chars"},
		{name: "eleven", stdin: "eleven char\n", want: oldAdminPassword, fail: "at least 12 characters"},
		{name: "nothing on standard input", stdin: "", want: oldAdminPassword, fail: "read password"},
		{name: "empty line", stdin: "\n", want: oldAdminPassword, fail: "at least 12 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := adminDir(t)
			args := []string{"reset-password", "--data-dir", dir}
			if tt.file != "" {
				f := filepath.Join(t.TempDir(), "pw")
				if err := os.WriteFile(f, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--password-file", f)
			}
			var out, errOut bytes.Buffer
			err := usersCLI{stdin: strings.NewReader(tt.stdin), stdout: &out, stderr: &errOut, getenv: func(k string) string { return tt.env[k] }}.run(t.Context(), args)
			works, alive := adminState(t, dir, tt.want)
			if !works {
				t.Errorf("password %q does not work afterwards", tt.want)
			}
			if tt.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tt.fail) {
					t.Fatalf("error = %v, want one with %q", err, tt.fail)
				}
				if !alive {
					t.Error("a refused reset ended the session")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if alive {
				t.Error("the session survived the reset")
			}
			if !strings.Contains(out.String(), "me@example.test") {
				t.Errorf("output %q does not say whose password changed", out.String())
			}
			for _, w := range []string{out.String(), errOut.String()} {
				if strings.Contains(w, strings.TrimSpace(tt.want)) || strings.Contains(w, oldAdminPassword) {
					t.Errorf("a password was printed: %q", w)
				}
			}
			if ok, _ := adminState(t, dir, oldAdminPassword); ok {
				t.Error("the old password still works")
			}
		})
	}
}

func TestUsersDataDirFromEnvironment(t *testing.T) {
	dir := adminDir(t)
	env := map[string]string{"MAILRULES_DATA_DIR": dir}
	var out bytes.Buffer
	err := usersCLI{stdin: strings.NewReader("password from env dir\n"), stdout: &out, stderr: &out, getenv: func(k string) string { return env[k] }}.run(t.Context(), []string{"reset-password"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := adminState(t, dir, "password from env dir"); !ok {
		t.Error("password was not reset in MAILRULES_DATA_DIR")
	}
}

func TestUsersErrors(t *testing.T) {
	empty := t.TempDir() // a directory without a database
	noAdmin := t.TempDir()
	db, err := store.Open(t.Context(), noAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	db.Close()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", nil, "no subcommand"},
		{"unknown subcommand", []string{"delete"}, `unknown subcommand "delete"`},
		{"a password as an argument", []string{"reset-password", "--data-dir", noAdmin, "hunter2hunter2"}, "never an argument"},
		{"a password as a flag", []string{"reset-password", "--password", "hunter2hunter2"}, "flag provided but not defined"},
		{"no database there", []string{"reset-password", "--data-dir", empty}, "no MailRules database in " + empty},
		{"no admin yet", []string{"reset-password", "--data-dir", noAdmin}, "no admin account yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := usersCLI{stdin: strings.NewReader("a long enough password\n"), stdout: &out, stderr: &out, getenv: func(string) string { return "" }}.run(t.Context(), tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one with %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error echoes the password: %v", err)
			}
		})
	}
	// A missing database stays missing: the command did not create one.
	if _, err := os.Stat(filepath.Join(empty, "mailrules.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reset-password created a database in a directory that had none: %v", err)
	}
}
