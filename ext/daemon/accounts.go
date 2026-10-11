package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/contacts"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const accountsUsage = `usage: mailrules accounts <add|list|test> [flags]

  add   --preset icloud|gmail|fastmail|yahoo|zoho|proton|generic --username NAME
        [--label TEXT] [--host HOST] [--port 993] [--tls implicit|starttls]
        [--watch-folder INBOX] [--password-file PATH] [--accept-cert SHA256]
        The app password comes from --password-file, else MAILRULES_ACCOUNT_PASSWORD,
        else one line on standard input. It is never accepted as a flag.
        --accept-cert trusts the server's own certificate with that SHA-256 fingerprint,
        and only it, for a server the system does not trust (such as Proton Mail Bridge).
        Without it, such a server is refused and its fingerprint printed.
  list  show every account
  test  ID [--watch]   log in and list folders; --watch also prints new mail as it arrives

Every command also takes --data-dir (default MAILRULES_DATA_DIR).
`

// accountsCLI holds what the accounts commands touch outside the database, so tests can
// swap them.
type accountsCLI struct {
	stdin     io.Reader
	stdout    io.Writer
	getenv    func(string) string
	tlsConfig *tls.Config // nil outside tests: verify against the system roots
}

func (a accountsCLI) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, accountsUsage)
		return errors.New("accounts: no subcommand given")
	}
	cfg, err := config.Load(nil, a.getenv)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("accounts "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "")
	var (
		presetName   = fs.String("preset", "", "")
		username     = fs.String("username", "", "")
		label        = fs.String("label", "", "")
		host         = fs.String("host", "", "")
		port         = fs.Int("port", 0, "")
		tlsMode      = fs.String("tls", "", "")
		watchFolder  = fs.String("watch-folder", "INBOX", "")
		passwordFile = fs.String("password-file", "", "")
		watch        = fs.Bool("watch", false, "")
		acceptCert   = fs.String("accept-cert", "", "")
	)
	// Allow "test 3 --watch" as well as "test --watch 3".
	var positional []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return fmt.Errorf("accounts %s: %w", args[0], err)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
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
	master, err := openMasterKey(ctx, cfg, st)
	if err != nil {
		return err
	}

	switch args[0] {
	case "list":
		return a.list(ctx, st)
	case "add":
		preset, ok := presets.Get(*presetName)
		if !ok || preset.OAuthOnly {
			return fmt.Errorf("accounts add: --preset %q must be icloud, gmail, fastmail, yahoo, zoho, proton or generic", *presetName)
		}
		acct := store.Account{
			Label: *label, Preset: preset.Name, Host: preset.Host, Port: preset.Port, TLSMode: preset.TLSMode,
			Username: *username, WatchFolder: *watchFolder, CreatedAt: time.Now().Unix(),
		}
		if *host != "" {
			acct.Host = *host
		}
		if *port != 0 {
			acct.Port = *port
		}
		if *tlsMode != "" {
			acct.TLSMode = *tlsMode
		}
		if acct.Label == "" {
			acct.Label = acct.Username
		}
		if acct.Username == "" || acct.Host == "" {
			return errors.New("accounts add: --username is required, and --host too with the generic preset")
		}
		if *acceptCert != "" {
			if acct.CertFingerprint, err = mail.ParseFingerprint(*acceptCert); err != nil {
				return fmt.Errorf("accounts add: --accept-cert: %w", err)
			}
		}
		password, err := a.password(*passwordFile, preset)
		if err != nil {
			return err
		}
		return a.add(ctx, st, master, acct, preset, password)
	case "test":
		if len(positional) != 1 {
			return errors.New("accounts test: give one account id (see `mailrules accounts list`)")
		}
		id, err := strconv.ParseInt(positional[0], 10, 64)
		if err != nil {
			return fmt.Errorf("accounts test: %q is not an account id", positional[0])
		}
		return a.test(ctx, st, master, id, *watch)
	default:
		fmt.Fprint(os.Stderr, accountsUsage)
		return fmt.Errorf("accounts: unknown subcommand %q", args[0])
	}
}

// password reads the app password without it ever being an argument, where `ps` would show it.
func (a accountsCLI) password(file string, preset presets.Preset) (string, error) {
	var pw string
	switch {
	case file != "":
		b, err := os.ReadFile(file) // #nosec G304 G703 -- operator-chosen path
		if err != nil {
			return "", fmt.Errorf("read password file: %w", err)
		}
		pw = string(b)
	case a.getenv("MAILRULES_ACCOUNT_PASSWORD") != "":
		pw = a.getenv("MAILRULES_ACCOUNT_PASSWORD")
	default:
		if preset.HelpURL != "" {
			fmt.Fprintf(os.Stderr, "Create an app password first: %s\n", preset.HelpURL)
		}
		fmt.Fprint(os.Stderr, "App password (input is not hidden): ")
		line, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read password: %w", err)
		}
		pw = line
	}
	if pw = strings.TrimSpace(pw); pw == "" {
		return "", errors.New("the app password is empty")
	}
	return pw, nil
}

// dial logs in with credentials that are not stored yet, trying each username the
// account's preset allows, and returns the connection with the username that worked. The
// CLI's `accounts add` and the HTTP API's connection test and account creation share it.
func dial(ctx context.Context, acct store.Account, password string, tlsConfig *tls.Config) (*imap.Mailbox, string, error) {
	preset, _ := presets.Get(acct.Preset)
	var err error
	for _, name := range preset.Usernames(acct.Username) {
		var mb *imap.Mailbox
		mb, err = imap.Open(ctx, imap.Config{
			AccountID: acct.ID, Host: acct.Host, Port: acct.Port, TLSMode: acct.TLSMode,
			Username: name, Password: password, Preset: preset, TLSConfig: tlsConfig, CertFingerprint: acct.CertFingerprint,
		})
		if err == nil {
			return mb, name, nil
		}
		if !errors.Is(err, mail.ErrAuth) {
			break
		}
	}
	return nil, "", err
}

func (a accountsCLI) open(ctx context.Context, acct store.Account, preset presets.Preset, username, password string) (*imap.Mailbox, error) {
	return imap.Open(ctx, imap.Config{
		AccountID: acct.ID, Host: acct.Host, Port: acct.Port, TLSMode: acct.TLSMode,
		Username: username, Password: password, Preset: preset, TLSConfig: a.tlsConfig, CertFingerprint: acct.CertFingerprint,
	})
}

// certHelp adds to a certificate error what the certificate is and how to accept it.
func certHelp(err error, hint string) error {
	var ce *mail.CertError
	if !errors.As(err, &ce) {
		return err
	}
	c := ce.Cert
	return fmt.Errorf("%w\n  subject %s, issued by %s, valid %s to %s\n%s", err, c.Subject, c.Issuer,
		c.NotBefore.Format(time.DateOnly), c.NotAfter.Format(time.DateOnly), strings.ReplaceAll(hint, "FINGERPRINT", c.Fingerprint))
}

// add checks the account against the server before anything is stored, then does the
// first-connect work: folder discovery, the watch baseline and the contacts scan.
func (a accountsCLI) add(ctx context.Context, st *store.Store, master []byte, acct store.Account, preset presets.Preset, password string) error {
	user, err := st.FirstUser(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("accounts add: no admin user yet; run `mailrules serve`, finish first-run setup in the browser, then add accounts")
	}
	if err != nil {
		return err
	}
	acct.UserID = user.ID

	mb, username, err := dial(ctx, acct, password, a.tlsConfig)
	if err != nil {
		if errors.Is(err, mail.ErrAuth) && preset.HelpURL != "" {
			return fmt.Errorf("%w\nthis provider needs an app password, not your account password: %s", err, preset.HelpURL)
		}
		return certHelp(err, "if this is your server's own certificate (Proton Mail Bridge makes its own), check the fingerprint and add the mailbox again with --accept-cert FINGERPRINT")
	}
	defer mb.Close()
	acct.Username = username

	folders, err := mb.Folders(ctx)
	if err != nil {
		return err
	}
	status, err := mb.Status(ctx, acct.WatchFolder)
	if err != nil {
		return err
	}
	acct.Capabilities = mb.Capabilities().All
	if acct, err = st.CreateAccount(ctx, master, acct, password); err != nil {
		return err
	}
	rows := make([]store.Folder, len(folders))
	sent := ""
	for i, f := range folders {
		rows[i] = store.Folder{Name: f.Name, Delimiter: f.Delimiter, SpecialUse: f.SpecialUse}
		if f.SpecialUse == mail.RoleSent {
			sent = f.Name
		}
	}
	if err := st.SaveFolders(ctx, acct.ID, rows); err != nil {
		return err
	}
	// Baseline: only mail that arrives from now on is sorted.
	if err := st.SetFolderPosition(ctx, acct.ID, acct.WatchFolder, status.UIDValidity, status.UIDNext-1); err != nil {
		return err
	}
	nContacts := 0
	if sent != "" {
		if nContacts, err = contacts.Sync(ctx, mb, st, acct.ID, sent); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.stdout, "added account %d (%s as %s): %d folders, %d contacts, watching %s for mail after UID %d\n",
		acct.ID, acct.Host, acct.Username, len(folders), nContacts, acct.WatchFolder, status.UIDNext-1)
	if sent == "" {
		fmt.Fprintln(a.stdout, "warning: no Sent folder found, so the contacts index is empty")
	}
	a.warnCaps(mb.Capabilities())
	fmt.Fprintln(a.stdout, "a running mailrules serve starts watching it within a few seconds; no restart is needed")
	return nil
}

func (a accountsCLI) warnCaps(caps mail.Caps) {
	if !caps.CanMove() {
		fmt.Fprintln(a.stdout, "warning: this server has neither MOVE nor UIDPLUS; rules can flag and mark mail here but not move it")
	}
	if !caps.Idle {
		fmt.Fprintln(a.stdout, "note: this server has no IDLE; new mail is checked for once a minute")
	}
}

func (a accountsCLI) list(ctx context.Context, st *store.Store) error {
	accounts, err := st.Accounts(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tLABEL\tPRESET\tUSERNAME\tHOST\tWATCHING\tSTATUS")
	for _, acct := range accounts {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s:%d\t%s\t%s\n", acct.ID, acct.Label, acct.Preset, acct.Username, acct.Host, acct.Port, acct.WatchFolder, acct.Status)
	}
	return w.Flush()
}

// test logs in with the stored credentials and changes nothing, in the database or the mailbox.
func (a accountsCLI) test(ctx context.Context, st *store.Store, master []byte, id int64, watch bool) error {
	acct, err := st.Account(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("accounts test: no account with id %d", id)
	}
	if err != nil {
		return err
	}
	if acct.OAuth {
		// Only the module that connected it can sign in to it, and the command line runs none.
		return fmt.Errorf("accounts test: account %d signs in with one-click sign-in; test it in the web UI: Mailboxes, then Test", id)
	}
	password, err := st.AccountSecret(ctx, master, id)
	if err != nil {
		return err
	}
	preset, _ := presets.Get(acct.Preset)
	mb, err := a.open(ctx, acct, preset, acct.Username, password)
	if err != nil {
		return certHelp(err, "if you replaced the server's certificate, accept the new one in the web UI: Mailboxes, then Check certificate on this mailbox")
	}
	defer mb.Close()
	folders, err := mb.Folders(ctx)
	if err != nil {
		return err
	}
	status, err := mb.Status(ctx, acct.WatchFolder)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "ok: logged in to %s as %s; %d folders; %s is at UID %d\n", acct.Host, acct.Username, len(folders), acct.WatchFolder, status.UIDNext-1)
	for _, f := range folders {
		if f.SpecialUse != "" {
			fmt.Fprintf(a.stdout, "  %s -> %s\n", f.SpecialUse, f.Name)
		}
	}
	a.warnCaps(mb.Capabilities())
	if !watch {
		return nil
	}

	fmt.Fprintf(a.stdout, "watching %s; press Ctrl-C to stop\n", acct.WatchFolder)
	out := make(chan mail.NewMail)
	done := make(chan error, 1)
	go func() { done <- mb.Watch(ctx, acct.WatchFolder, status.UIDNext-1, out) }()
	for {
		select {
		case err := <-done:
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case nm := <-out:
			raw, err := mb.Fetch(ctx, nm.Ref, 0)
			if err != nil {
				fmt.Fprintf(a.stdout, "new mail uid=%d (could not be read: %v)\n", nm.Ref.UID, err)
				continue
			}
			// Sender and subject only: bodies are never printed or logged.
			if sum, err := message.Parse(raw, acct.ID, 0); err == nil {
				fmt.Fprintf(a.stdout, "new mail uid=%d from=%s subject=%q\n", nm.Ref.UID, sum.From, sum.Subject)
			} else {
				fmt.Fprintf(a.stdout, "new mail uid=%d (could not be parsed: %v)\n", nm.Ref.UID, err)
			}
		}
	}
}
