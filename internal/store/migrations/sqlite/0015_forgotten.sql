-- +goose Up
-- People a sign-in module forgot (MAI-167): the sign-in service deleted them, so their
-- sessions were ended and their email freed at once (users.email became a placeholder), and
-- what MailRules kept of them is removed at remove_at unless the same identity signs in
-- again first, which deletes the row. The removal job reads this table, so a restart does
-- not lose a removal that is waiting.
CREATE TABLE forgotten (
  user_id      INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  forgotten_at INTEGER NOT NULL,
  remove_at    INTEGER NOT NULL
);
CREATE INDEX forgotten_due ON forgotten(remove_at);

-- +goose Down
DROP TABLE forgotten;
