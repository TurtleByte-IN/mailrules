-- +goose Up
-- A mailbox whose server presents a certificate the system does not trust, such as Proton
-- Mail Bridge's own, connects once the person accepts that certificate (MAI-159). Its SHA-256
-- fingerprint is kept here, and from then on the server must present exactly that
-- certificate. Empty: the system's trust store decides, as before.
ALTER TABLE accounts ADD COLUMN cert_fingerprint TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE accounts DROP COLUMN cert_fingerprint;
