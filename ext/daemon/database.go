package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// The daemon's database (MAI-150). Self-hosting keeps it in SQLite in the data directory,
// migrated by whatever opens it, as always. The hosted service runs on Postgres:
//
//   - MAILRULES_DATABASE_URL signs in as a member of mailrules_app, for the web app and the
//     daemon's jobs; MAILRULES_WORKER_DATABASE_URL (when set) as a member of
//     mailrules_worker, for the mailbox workers and the jobs that hand work to modules,
//     which never read a password hash or a session. Both run in one process, each on its
//     own pool; a later split into separate processes needs no change here.
//   - Neither role owns a table, so neither migrates: `mailrules migrate` does, signed in
//     as the owner through MAILRULES_MIGRATE_DATABASE_URL, which nothing else reads. The
//     owner's password then lives only where that one command runs, never in a daemon's
//     environment. A daemon refuses to start on a schema that is not the one it was built
//     for, rather than fail at its first query.

// databases are the stores the daemon runs on, and how to close them.
type databases struct {
	app    *store.Store // the web app, the daemon's jobs and the command line
	worker *store.Store // the mailbox workers and the module hand-overs; app on SQLite
	dbs    []*sql.DB
}

func (d *databases) Close() {
	for _, db := range d.dbs {
		_ = db.Close()
	}
}

// openDatabases opens the daemon's database: SQLite in cfg.DataDir, migrated, or Postgres
// at cfg.DatabaseURL (and cfg.WorkerDatabaseURL), checked to be migrated.
func openDatabases(ctx context.Context, cfg *config.Config) (*databases, error) {
	if err := cfg.ValidateDatabase(); err != nil {
		return nil, err
	}
	if !cfg.Postgres() {
		db, err := store.Open(ctx, cfg.DataDir)
		if err != nil {
			return nil, err
		}
		if err := store.Migrate(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
		st := store.New(db)
		return &databases{app: st, worker: st, dbs: []*sql.DB{db}}, nil
	}
	d := &databases{}
	app, err := openPostgres(ctx, "MAILRULES_DATABASE_URL", cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	d.dbs = append(d.dbs, app)
	if err := store.CheckSchema(ctx, app); err != nil {
		d.Close()
		return nil, err
	}
	d.app, d.worker = store.New(app), store.New(app)
	if cfg.WorkerDatabaseURL != "" && cfg.WorkerDatabaseURL != cfg.DatabaseURL {
		worker, err := openPostgres(ctx, "MAILRULES_WORKER_DATABASE_URL", cfg.WorkerDatabaseURL)
		if err != nil {
			d.Close()
			return nil, err
		}
		d.dbs = append(d.dbs, worker)
		d.worker = store.New(worker)
	}
	return d, nil
}

// openStore is openDatabases' app store, for a command that needs no workers.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, func(), error) {
	d, err := openDatabases(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return d.app, d.Close, nil
}

// openPostgres connects with the URL that env names; an error never quotes the URL, which
// holds a password.
func openPostgres(ctx context.Context, env, url string) (*sql.DB, error) {
	db, err := store.OpenPostgres(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to the database %s names: %w", env, err)
	}
	return db, nil
}

// migrate is `mailrules migrate`: SQLite in the data directory, or the Postgres database
// MAILRULES_MIGRATE_DATABASE_URL names, signed in as the owner of the tables.
func migrate(ctx context.Context, cfg *config.Config) error {
	if err := cfg.ValidateDatabase(); err != nil {
		return err
	}
	if cfg.MigrateDatabaseURL == "" {
		if cfg.Postgres() {
			return errors.New("migrations run as the owner of the tables, never as the app's role: set MAILRULES_MIGRATE_DATABASE_URL for `mailrules migrate`")
		}
		db, err := store.Open(ctx, cfg.DataDir)
		if err != nil {
			return err
		}
		defer db.Close()
		return store.Migrate(ctx, db)
	}
	db, err := openPostgres(ctx, "MAILRULES_MIGRATE_DATABASE_URL", cfg.MigrateDatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	return store.Migrate(ctx, db)
}
