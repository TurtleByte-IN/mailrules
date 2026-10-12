-- +goose Up
-- The daemon never migrates on Postgres: `mailrules migrate` does, as the owner. The roles it
-- runs as read which migrations ran, so a daemon refuses to start on a schema it was not
-- built for (store.CheckSchema). They change nothing there.
GRANT SELECT ON goose_db_version TO mailrules_app, mailrules_worker;

-- +goose Down
REVOKE SELECT ON goose_db_version FROM mailrules_app, mailrules_worker;
