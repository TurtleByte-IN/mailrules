-- +goose Up
-- A cleanup batch records how much mail its check covered (MAI-48): the most emails the check
-- was allowed (scan_limit) and how many the folder held in the range before that cut
-- (scan_matched). With the batch's `since` they let the screen say "newest 25 emails" or "all
-- time" truthfully. NULL on a batch made before this: nothing was recorded, so nothing is known.
ALTER TABLE batches ADD COLUMN scan_limit INTEGER;
ALTER TABLE batches ADD COLUMN scan_matched INTEGER;

-- +goose Down
ALTER TABLE batches DROP COLUMN scan_matched;
ALTER TABLE batches DROP COLUMN scan_limit;
