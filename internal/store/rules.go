package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// null stores a zero value as NULL, for the optional rule columns.
func null[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

// ruleJSON encodes the three JSON columns of a rule.
func ruleJSON(r rules.Rule) (conditions, exceptions, actions []byte, err error) {
	if conditions, err = json.Marshal(r.Conditions); err != nil {
		return nil, nil, nil, fmt.Errorf("encode conditions: %w", err)
	}
	if exceptions, err = json.Marshal(r.Exceptions); err != nil {
		return nil, nil, nil, fmt.Errorf("encode exceptions: %w", err)
	}
	if actions, err = json.Marshal(r.Actions); err != nil {
		return nil, nil, nil, fmt.Errorf("encode actions: %w", err)
	}
	return conditions, exceptions, actions, nil
}

// CreateRule inserts a rule at version 1 and returns it with its id. The
// caller validates the rule first.
func (s *Store) CreateRule(ctx context.Context, r rules.Rule, now int64) (rules.Rule, error) {
	conditions, exceptions, actions, err := ruleJSON(r)
	if err != nil {
		return rules.Rule{}, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO rules (user_id, account_id, name, said, intent, conditions, exceptions, actions,
		                    priority, stack, model, min_confidence, enabled, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		r.UserID, null(r.AccountID), r.Name, null(r.Said), null(r.Intent), string(conditions), string(exceptions), string(actions),
		r.Priority, r.Stack, null(r.Model), r.MinConfidence, r.Enabled, now, now)
	if err != nil {
		return rules.Rule{}, fmt.Errorf("create rule: %w", err)
	}
	r.ID, _ = res.LastInsertId()
	r.Version, r.CreatedAt, r.UpdatedAt = 1, now, now
	return r, nil
}

// UpdateRule rewrites a rule's editable columns and bumps its version, so
// past decisions keep pointing at the wording they were made under.
func (s *Store) UpdateRule(ctx context.Context, r rules.Rule, now int64) error {
	conditions, exceptions, actions, err := ruleJSON(r)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE rules SET account_id = ?, name = ?, said = ?, intent = ?, conditions = ?, exceptions = ?, actions = ?,
		                  priority = ?, stack = ?, model = ?, min_confidence = ?, enabled = ?,
		                  version = version + 1, updated_at = ?
		 WHERE id = ? AND user_id = ?`,
		null(r.AccountID), r.Name, null(r.Said), null(r.Intent), string(conditions), string(exceptions), string(actions),
		r.Priority, r.Stack, null(r.Model), r.MinConfidence, r.Enabled, now, r.ID, r.UserID)
	if err != nil {
		return fmt.Errorf("update rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const ruleColumns = `id, user_id, account_id, name, said, intent, conditions, exceptions, actions,
	priority, stack, model, min_confidence, enabled, version, created_at, updated_at`

func scanRule(row interface{ Scan(...any) error }) (rules.Rule, error) {
	var r rules.Rule
	var accountID sql.NullInt64
	var said, intent, model sql.NullString
	var minConfidence sql.NullFloat64
	var conditions, exceptions, actions string
	if err := row.Scan(&r.ID, &r.UserID, &accountID, &r.Name, &said, &intent, &conditions, &exceptions, &actions,
		&r.Priority, &r.Stack, &model, &minConfidence, &r.Enabled, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return rules.Rule{}, err
	}
	r.AccountID, r.Said, r.Intent, r.Model = accountID.Int64, said.String, intent.String, model.String
	if minConfidence.Valid {
		r.MinConfidence = &minConfidence.Float64
	}
	if err := errors.Join(
		json.Unmarshal([]byte(conditions), &r.Conditions),
		json.Unmarshal([]byte(exceptions), &r.Exceptions),
		json.Unmarshal([]byte(actions), &r.Actions),
	); err != nil {
		return rules.Rule{}, fmt.Errorf("decode rule %d: %w", r.ID, err)
	}
	return r, nil
}

// Rule returns one of the user's rules, or ErrNotFound.
func (s *Store) Rule(ctx context.Context, userID, id int64) (rules.Rule, error) {
	r, err := scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return rules.Rule{}, ErrNotFound
	}
	if err != nil {
		return rules.Rule{}, fmt.Errorf("get rule: %w", err)
	}
	return r, nil
}

// Rules returns all of the user's rules, disabled ones included, in priority order.
func (s *Store) Rules(ctx context.Context, userID int64) ([]rules.Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE user_id = ? ORDER BY priority, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()
	var out []rules.Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("list rules: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	return out, nil
}

// DeleteRule removes a rule. The schema takes the sender rules that route to it along
// (ON DELETE CASCADE) and keeps its decisions and corrections, with their rule set to NULL.
func (s *Store) DeleteRule(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM rules WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PutSenderRule stores the verdict for a sender address or domain, replacing
// any earlier one for the same sender, and returns it with its id.
func (s *Store) PutSenderRule(ctx context.Context, sr rules.SenderRule, now int64) (rules.SenderRule, error) {
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO sender_rules (user_id, match_type, value, rule_id, verdict, source, hits, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?)
		 ON CONFLICT (user_id, match_type, value) DO UPDATE
		 SET rule_id = excluded.rule_id, verdict = excluded.verdict, source = excluded.source
		 RETURNING id, hits, created_at`,
		sr.UserID, sr.MatchType, sr.Value, null(sr.RuleID), sr.Verdict, sr.Source, now).
		Scan(&sr.ID, &sr.Hits, &sr.CreatedAt)
	if err != nil {
		return rules.SenderRule{}, fmt.Errorf("put sender rule: %w", err)
	}
	return sr, nil
}

// SenderRules returns all of the user's sender rules.
func (s *Store) SenderRules(ctx context.Context, userID int64) ([]rules.SenderRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, match_type, value, rule_id, verdict, source, hits, created_at
		 FROM sender_rules WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sender rules: %w", err)
	}
	defer rows.Close()
	var out []rules.SenderRule
	for rows.Next() {
		var sr rules.SenderRule
		var ruleID sql.NullInt64
		if err := rows.Scan(&sr.ID, &sr.UserID, &sr.MatchType, &sr.Value, &ruleID, &sr.Verdict, &sr.Source, &sr.Hits, &sr.CreatedAt); err != nil {
			return nil, fmt.Errorf("list sender rules: %w", err)
		}
		sr.RuleID = ruleID.Int64
		out = append(out, sr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sender rules: %w", err)
	}
	return out, nil
}

// DeleteSenderRule removes a sender's verdict; deleting a missing one is not an error.
func (s *Store) DeleteSenderRule(ctx context.Context, userID int64, matchType, value string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sender_rules WHERE user_id = ? AND match_type = ? AND value = ?`, userID, matchType, value); err != nil {
		return fmt.Errorf("delete sender rule: %w", err)
	}
	return nil
}
