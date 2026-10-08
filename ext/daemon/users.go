package daemon

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const usersUsage = `usage: mailrules users reset-password [flags]

  reset-password  set a new password for the admin account, without the old one, and
                  end every session. Run it on the machine that holds the data
                  directory; having that access is what proves you may.
                  The new password comes from --password-file, else
                  MAILRULES_USER_PASSWORD, else standard input: typed twice without
                  echo at a terminal, or one line when piped. It is never accepted as
                  a flag, and never printed.

Every command also takes --data-dir (default MAILRULES_DATA_DIR).
`

// usersCLI holds what the users commands touch outside the database, so tests can swap them.
type usersCLI struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
}

func (u usersCLI) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(u.stderr, usersUsage)
		return errors.New("users: no subcommand given")
	}
	if args[0] != "reset-password" {
		fmt.Fprint(u.stderr, usersUsage)
		return fmt.Errorf("users: unknown subcommand %q", args[0])
	}
	cfg, err := config.Load(nil, u.getenv)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("users "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "")
	passwordFile := fs.String("password-file", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("users %s: %w", args[0], err)
	}
	if fs.NArg() != 0 {
		// Not echoed: whatever was typed there may be the password.
		return fmt.Errorf("users %s: unexpected argument; the password is never an argument (see --password-file)", args[0])
	}

	// Opening a data directory that is not there would create an empty database in it.
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "mailrules.db")); err != nil {
		return fmt.Errorf("users reset-password: no MailRules database in %s; check --data-dir or MAILRULES_DATA_DIR", cfg.DataDir)
	}
	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}
	st := store.New(db)
	user, err := st.FirstUser(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("users reset-password: no admin account yet; open MailRules in the browser and create it")
	}
	if err != nil {
		return err
	}

	password, err := u.newPassword(*passwordFile)
	if err != nil {
		return err
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return err
	}
	if err := st.SetPassword(ctx, user.ID, hash); err != nil {
		return err
	}
	fmt.Fprintf(u.stdout, "password changed for %s; everyone signed in was signed out\n", user.Email)
	return nil
}

// newPassword reads the new password without it ever being an argument, where `ps` would
// show it, and checks it. At a terminal it asks twice and shows nothing as it is typed.
func (u usersCLI) newPassword(file string) (string, error) {
	var pw string
	switch {
	case file != "":
		b, err := os.ReadFile(file) // #nosec G304 G703 -- operator-chosen path
		if err != nil {
			return "", fmt.Errorf("read password file: %w", err)
		}
		pw = string(b)
	case u.getenv("MAILRULES_USER_PASSWORD") != "":
		pw = u.getenv("MAILRULES_USER_PASSWORD")
	default:
		var err error
		if pw, err = u.prompt(); err != nil {
			return "", err
		}
	}
	// A trailing line ending is not part of the password; spaces inside or around it are.
	pw = strings.TrimRight(pw, "\r\n")
	if len(pw) < crypto.MinPasswordLen {
		return "", fmt.Errorf("the password must be at least %d characters", crypto.MinPasswordLen)
	}
	return pw, nil
}

func (u usersCLI) prompt() (string, error) {
	if f, ok := u.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) { // #nosec G115 -- a file descriptor fits an int
		read := func(label string) (string, error) {
			fmt.Fprint(u.stderr, label)
			b, err := term.ReadPassword(int(f.Fd())) // #nosec G115
			fmt.Fprintln(u.stderr)
			return string(b), err
		}
		first, err := read("New password: ")
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		second, err := read("New password again: ")
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if first != second {
			return "", errors.New("the two passwords differ; nothing was changed")
		}
		return first, nil
	}
	line, err := bufio.NewReader(u.stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read password: %w", err)
	}
	return line, nil
}
