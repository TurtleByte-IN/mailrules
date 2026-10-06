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

	"github.com/TurtleByte-IN/mailrules/internal/api"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: mailrules <command> [flags]

  serve     run the daemon and web UI
  migrate   apply database migrations
  eval      measure decider accuracy on labeled mail:
            eval --labels testdata/labeled.jsonl --decider jev,clef:clef-flash
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
	case "eval":
		return eval(ctx, args[1:], os.Stdout)
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

	if err := cfg.DeciderReady(); err != nil {
		slog.Warn("no decision model yet; new mail waits in Needs review until one is set", "missing", err.Error())
	}

	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}

	// Loaded here so a bad key stops startup; account credentials use it from M2.
	if _, err := crypto.LoadMasterKey(cfg.MasterKey, cfg.MasterKeyFile, cfg.DataDir); err != nil {
		return err
	}

	handler := api.NewHandler(api.Options{Store: store.New(db), SecureCookies: !cfg.ListensLocally()})
	srv := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "listen", cfg.Listen, "version", version, "dry_run", cfg.DryRun)

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
	deciders := fs.String("decider", cfg.Decider, "comma-separated deciders, each name or name:model")
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
