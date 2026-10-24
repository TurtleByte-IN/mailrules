-- +goose Up
-- A rule made in Build with conditions saved "Built with conditions" as `said`, which is for
-- the owner's own words (MAI-261): the rule editor showed "You said: Built with conditions",
-- and Rewrite with AI sent it to the composer as their words and kept it as the first line
-- above the new ones. Such a rule has no words of the owner's (NULL, as a rule saved with
-- none has); a rewritten one keeps the words under the placeholder. There is no rule
-- history table: `said` lives only here.
UPDATE rules SET said = NULL WHERE said = 'Built with conditions';
UPDATE rules SET said = NULLIF(substr(said, 23), '') WHERE substr(said, 1, 22) = 'Built with conditions' || char(10);

-- +goose Down
-- Nothing to restore: the placeholder was never the owner's words.
