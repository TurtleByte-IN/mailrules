// Package daemon runs the MailRules command line: the daemon and web UI (serve), and the
// migrate, accounts, users, eval, rules, dry-run and version commands. cmd/mailrules, the free
// self-host build, runs it with no modules; a build that adds modules (ext.Module) calls
// Run from its own main with them.
package daemon

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/api"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/mailer"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/summary"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

const usage = `usage: mailrules <command> [flags]

  serve     run the daemon and web UI
  migrate   apply database migrations
  accounts  add, list or test mail accounts
  users     reset-password: set a new admin password from the host, when the
            old one is lost; it ends every session
  eval      measure decider accuracy on labeled mail:
            eval --labels testdata/labeled.jsonl --decider jev,clef:clef-flash
  rules     import or export the rules as YAML; validate a rules file, or
            test it over .eml files
  dry-run   on | off: switch the global dry-run; with no argument, show it.
            While it is on, decisions are logged and no mailbox is changed.
  version   print the version
`

// Run runs the command args name, as `mailrules <command> [flags]` would. version is the
// build's version; modules are the features compiled in from outside this repository,
// served by `serve`. It stops on SIGINT or SIGTERM.
func Run(args []string, version string, modules ...ext.Module) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "migrate":
		cfg, err := config.Load(args[1:], os.Getenv)
		if err != nil {
			return err
		}
		db, err := store.Open(ctx, cfg.DataDir)
		if err != nil {
			return err
		}
		defer db.Close()
		return store.Migrate(ctx, db)
	case "accounts":
		return accountsCLI{stdin: os.Stdin, stdout: os.Stdout, getenv: os.Getenv}.run(ctx, args[1:])
	case "users":
		return usersCLI{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}.run(ctx, args[1:])
	case "eval":
		return eval(ctx, args[1:], os.Stdout)
	case "rules":
		return rulesCmd(ctx, args[1:], os.Getenv, os.Stdout)
	case "dry-run":
		return dryRunCmd(ctx, args[1:], os.Getenv, os.Stdout)
	case "serve":
		cfg, err := config.Load(args[1:], os.Getenv)
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		return serve(ctx, cfg, version, modules)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(ctx context.Context, cfg *config.Config, version string, modules []ext.Module) error {
	slog.SetDefault(telemetry.NewLogger(os.Stderr, cfg.LogLevel))
	if !cfg.ListensLocally() {
		slog.Warn("web UI is reachable from other machines; put it behind a reverse proxy with TLS", "listen", cfg.Listen)
	}

	proxies, err := cfg.TrustedProxyPrefixes()
	if err != nil {
		return err
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

	// The decider, its models, the thresholds and the provider keys can be changed in the
	// browser, so the pipeline asks for them per message (sett.Live) instead of holding a router.
	prices, err := models.LoadPrices(cfg.PricesFile)
	if err != nil {
		return err
	}
	deps := models.Deps{Caller: models.NewCaller(cfg.ModelConcurrency), Prices: prices}
	sett := &settings.Settings{Store: st, Master: master, Env: cfg, Deps: deps,
		Workspaces: models.AnthropicWorkspaces{Deps: deps}} // finds the workspace a Claude key that covers a whole organisation needs
	sett.Live(ctx) // logs now if the decider is not ready, rather than at the first email

	// One supervisor per account. They stop with ctx and get a few seconds to finish
	// queued mail; the database closes only after they have.
	accounts, err := st.Accounts(ctx)
	if err != nil {
		return err
	}
	hub := events.NewHub()
	ctx, stopAll := context.WithCancel(ctx)
	supervisors := &worker.Manager{}
	retention := make(chan struct{})
	defer supervisors.Wait()
	defer func() { <-retention }()
	defer stopAll() // runs first, so the waits above return even when the HTTP server is what failed
	// The retention job forgets old snippets and old messages: now, and then every hour.
	go func() {
		defer close(retention)
		worker.Retention{Store: st, Days: sett.RetentionDays}.Run(ctx)
	}()
	// A cleanup that was running when the daemon last stopped will never finish.
	if err := st.FailRunningBatches(ctx); err != nil {
		return err
	}
	// The executor is the one place that changes a mailbox; the HTTP layer reaches it for
	// undo and corrections.
	exec := &actions.Exec{Store: st, Accounts: supervisors, Hub: hub, DryRunDefault: cfg.DryRun}
	// start is also how the HTTP layer starts an account added, resumed or reconnected at runtime.
	start := func(acct store.Account) {
		supervisors.Start(ctx, &worker.Supervisor{
			Account: acct, Store: st, Hub: hub, Open: openAccount(st, master, acct),
			Pipeline: pipeline.Pipeline{Store: st, Live: sett.Live, Override: sett.RouterFor, Exec: exec, Hub: hub, BodyChars: cfg.BodyChars},
		})
	}
	watcher := newAccountWatcher(st, start)
	watcher.Seed(accounts)
	for _, acct := range accounts {
		if acct.Status != worker.StatusPaused {
			start(acct)
		}
	}
	// Mailboxes added with `mailrules accounts add` while this runs are picked up here.
	watching := make(chan struct{})
	defer func() { stopAll(); <-watching }()
	go func() {
		defer close(watching)
		watcher.Run(ctx, accountPollInterval)
	}()
	dryRun, err := st.DryRun(ctx, cfg.DryRun)
	if err != nil {
		return err
	}
	sum, err := newSummary(st, cfg)
	if err != nil {
		return err
	}
	// The summary email's loop checks every minute whether one is due. The defer stops it
	// before waiting for it, like the other defers.
	summaries := make(chan struct{})
	defer func() { stopAll(); <-summaries }()
	go func() {
		defer close(summaries)
		sum.Run(ctx)
	}()

	handler := api.NewHandler(api.Options{
		Store: st, SecureCookies: cfg.SecureCookies(), TrustedProxies: proxies, Hub: hub, Exec: exec, Settings: sett, Master: master,
		Metrics: telemetry.NewMetrics(version), Version: version,
		Connect: func(ctx context.Context, acct store.Account, password string) (mail.Mailbox, string, error) {
			mb, username, err := dial(ctx, acct, password, nil)
			if err != nil {
				return nil, "", err // not mb: a nil *imap.Mailbox is not a nil mail.Mailbox
			}
			return mb, username, nil
		},
		StartAccount: watcher.Start, StopAccount: supervisors.Stop, Summary: sum,
		StartCheck: supervisors.StartCheck, Checks: supervisors.Checks(), Sort: supervisors.Sort,
		Modules: modules,
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "listen", cfg.Listen, "version", version, "dry_run", dryRun, "accounts", len(accounts))

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down http server: %w", err)
	}
	return nil
}

// newSummary returns the summary email's service, sending through the mail server the
// environment sets, or through none while MAILRULES_SMTP_* lack something.
func newSummary(st *store.Store, cfg *config.Config) (*summary.Service, error) {
	if missing := cfg.SMTPMissing(); len(missing) > 0 {
		slog.Info("summary email cannot be sent until these are set", "missing", strings.Join(missing, ", "))
		return summary.New(st, cfg, nil), nil
	}
	password, err := cfg.SMTPSecret()
	if err != nil {
		return nil, err
	}
	return summary.New(st, cfg, &mailer.SMTP{Addr: cfg.SMTPAddr(), TLS: cfg.SMTPTLS, Username: cfg.SMTPUser, Password: password, From: cfg.SMTPFrom}), nil
}

// dryRunCmd shows or sets the global dry-run switch. It lives in the database, so a
// running daemon follows it from its next action on.
func dryRunCmd(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "on" && args[0] != "off") {
		return errors.New("usage: mailrules dry-run [on|off]")
	}
	cfg, err := config.Load(nil, getenv)
	if err != nil {
		return err
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
	if len(args) == 1 {
		if err := st.SetDryRun(ctx, args[0] == "on"); err != nil {
			return err
		}
	}
	on, err := st.DryRun(ctx, cfg.DryRun)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "dry-run is %s\n", map[bool]string{true: "on: decisions are logged, no mailbox is changed", false: "off: rules change mailboxes"}[on])
	return err
}

// openAccount returns how a supervisor connects to an account. The password is decrypted
// for each connection and kept nowhere else.
func openAccount(st *store.Store, master []byte, acct store.Account) func(context.Context) (mail.Mailbox, error) {
	return func(ctx context.Context) (mail.Mailbox, error) {
		password, err := st.AccountSecret(ctx, master, acct.ID)
		if err != nil {
			return nil, err
		}
		preset, _ := presets.Get(acct.Preset)
		mb, err := imap.Open(ctx, imap.Config{
			AccountID: acct.ID, Host: acct.Host, Port: acct.Port, TLSMode: acct.TLSMode,
			Username: acct.Username, Password: password, Preset: preset,
		})
		if err != nil {
			return nil, err
		}
		return mb, nil
	}
}

// eval runs each named decider over a labeled file and prints its report.
// Settings come from the environment; nothing is written to the database.
func eval(ctx context.Context, args []string, out io.Writer) error {
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	labelsPath := fs.String("labels", "testdata/labeled.jsonl", "labeled emails and their rule set")
	deciders := fs.String("decider", cfg.DeciderSpec(), "comma-separated deciders, each name or name:model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := os.Open(*labelsPath)
	if err != nil {
		return fmt.Errorf("open labels: %w", err)
	}
	defer f.Close()
	labels, err := models.LoadLabels(f)
	if err != nil {
		return err
	}
	prices, err := models.LoadPrices(cfg.PricesFile)
	if err != nil {
		return err
	}
	deps := models.Deps{Caller: models.NewCaller(cfg.ModelConcurrency), Prices: prices}
	for _, spec := range strings.Split(*deciders, ",") {
		router, err := models.NewRouter(cfg, strings.TrimSpace(spec), deps, nil)
		if err != nil {
			return err
		}
		models.Evaluate(ctx, router, labels).Write(out)
	}
	return nil
}
