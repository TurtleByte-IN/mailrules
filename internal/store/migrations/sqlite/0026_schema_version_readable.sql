-- +goose Up
-- Nothing to do on SQLite: one process owns the file. On Postgres (postgres/0026) the roles
-- the daemon runs as may read which migrations ran, so it can refuse to start on a schema
-- it was not built for (store.CheckSchema). Kept so that both sets list the same versions.
SELECT 1;

-- +goose Down
SELECT 1;
