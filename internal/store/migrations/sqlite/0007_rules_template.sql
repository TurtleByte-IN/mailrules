-- +goose Up
-- A rule added from the template gallery names its template in its own column (MAI-73)
-- instead of carrying "Template: <name>" in `said`, which is for the user's own words: the
-- rule editor showed "You said: Template: Receipts". A rule rewritten with AI since then
-- kept the marker on its first line and the user's words on the lines under it; those words
-- stay in `said`.
ALTER TABLE rules ADD COLUMN template TEXT;
UPDATE rules SET
  template = CASE WHEN instr(said, char(10)) > 0 THEN substr(said, 11, instr(said, char(10)) - 11) ELSE substr(said, 11) END,
  said = CASE WHEN instr(said, char(10)) > 0 THEN substr(said, instr(said, char(10)) + 1) END
WHERE substr(said, 1, 10) = 'Template: ';

-- +goose Down
UPDATE rules SET said = 'Template: ' || template || COALESCE(char(10) || said, '') WHERE template IS NOT NULL;
ALTER TABLE rules DROP COLUMN template;
