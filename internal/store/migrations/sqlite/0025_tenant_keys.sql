-- +goose Up
-- Each tenant's data key (MAI-150): a random key that seals the tenant's secrets (mailbox
-- passwords and tokens, model keys, notification addresses), stored only wrapped by the
-- key-encryption key that kek_id names (crypto.KeyProvider; the master key is "master").
-- NULL until the tenant's first secret is sealed. Deleting the tenant deletes it, and with
-- it every secret sealed under it. Secrets sealed before this (envelope 1, under the master
-- key itself) still open, and the daemon reseals them under their tenant's key at start.
ALTER TABLE tenants ADD COLUMN dek_enc BLOB;
ALTER TABLE tenants ADD COLUMN kek_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tenants DROP COLUMN kek_id;
ALTER TABLE tenants DROP COLUMN dek_enc;
