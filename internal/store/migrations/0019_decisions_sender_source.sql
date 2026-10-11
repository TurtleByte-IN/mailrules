-- +goose Up
-- Who made the sender rule that settled an email (MAI-169): user (set on Senders or with
-- "Always do this for this sender") or learned (MailRules learned it from the model's own
-- decisions). NULL for every other stage.
ALTER TABLE decisions ADD COLUMN sender_source TEXT;

-- Earlier sender decisions did not record it, and the rule itself may have been replaced or
-- forgotten since. Only one case is certain: MailRules learns nothing but routes to a rule,
-- so a sender decision that named no rule at all (kept, trashed or moved to a folder) came
-- from a rule the user set. A sender decision that routed to a rule stays NULL: unknown.
UPDATE decisions SET sender_source = 'user'
WHERE stage = 'sender' AND rule_id IS NULL AND rule_name IS NULL AND COALESCE(rule_version, 0) = 0;

-- +goose Down
ALTER TABLE decisions DROP COLUMN sender_source;
