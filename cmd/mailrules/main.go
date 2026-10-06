// Command mailrules is the MailRules daemon and CLI.
package main

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

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/api"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: mailrules <command> [flags]

  serve     run the daemon and web UI
  migrate   apply database migrations
  accounts  add, list or test mail accounts
  eval      measure decider accuracy on labeled mail:
            eval --labels testdata/labeled.jsonl --decider jev,clef:clef-flash
  rules     validate a rules file, or test it over .eml files
  dry-run   on | off: switch the global dry-run; with no argument, show it.
            While it is on, decisions are logged and no mailbox is changed.
  version   print the version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mailrules:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
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
	case "eval":
		return eval(ctx, args[1:], os.Stdout)
	case "rules":
		return rulesCmd(args[1:], os.Stdout)
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
		return serve(ctx, cfg)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(ctx context.Context, cfg *config.Config) error {
	slog.SetDefault(telemetry.NewLogger(os.Stderr, cfg.LogLevel))
	if !cfg.ListensLocally() {
		slog.Warn("web UI is reachable from other machines; put it behind a reverse proxy with TLS", "listen", cfg.Listen)
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
	master, err := crypto.LoadMasterKey(cfg.MasterKey, cfg.MasterKeyFile, cfg.DataDir)
	if err != nil {
		return err
	}

	var router *models.Router // nil = mail that needs a model waits in Needs review
	if err := cfg.DeciderReady(); err != nil {
		slog.Warn("no decision model yet; new mail that needs one waits in Needs review until it is set", "missing", err.Error())
	} else {
		prices, err := models.LoadPrices(cfg.PricesFile)
		if err != nil {
			return err
		}
		deps := models.Deps{Caller: models.NewCaller(cfg.ModelConcurrency), Prices: prices}
		if router, err = models.NewRouter(cfg, cfg.DeciderSpec(), deps, st); err != nil {
			return err
		}
	}

	// One supervisor per account. They stop with ctx and get a few seconds to finish
	// queued mail; the database closes only after they have.
	accounts, err := st.Accounts(ctx)
	if err != nil {
		return err
	}
	hub := events.NewHub()
	ctx, stopAll := context.WithCancel(ctx)
	supervisors := &worker.Manager{}
	defer supervisors.Wait()
	defer stopAll() // runs first, so Wait returns even when the HTTP server is what failed
	// The executor is the one place that changes a mailbox. The HTTP layer (M7) will call
	// its Undo, UndoBatch and Correct.
	exec := &actions.Exec{Store: st, Accounts: supervisors, Hub: hub, DryRunDefault: cfg.DryRun}
	for _, acct := range accounts {
		if acct.Status == worker.StatusPaused {
			continue
		}
		supervisors.Start(ctx, &worker.Supervisor{
			Account: acct, Store: st, Hub: hub, Open: openAccount(st, master, acct),
			Pipeline: pipeline.Pipeline{Store: st, Router: router, Exec: exec, Hub: hub, MinConfidence: cfg.MinConfidence, BodyChars: cfg.BodyChars},
		})
	}
	dryRun, err := st.DryRun(ctx, cfg.DryRun)
	if err != nil {
		return err
	}

	handler := api.NewHandler(api.Options{Store: st, SecureCookies: !cfg.ListensLocally()})
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
