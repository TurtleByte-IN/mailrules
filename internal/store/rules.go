package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

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
	return createRule(ctx, s.db, r, now)
}

// execer is a database or a transaction on it.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func createRule(ctx context.Context, q execer, r rules.Rule, now int64) (rules.Rule, error) {
	conditions, exceptions, actions, err := ruleJSON(r)
	if err != nil {
		return rules.Rule{}, err
	}
	res, err := q.ExecContext(ctx,
		`INSERT INTO rules (user_id, account_id, name, said, template, intent, conditions, exceptions, actions,
		                    priority, stack, model, min_confidence, enabled, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		r.UserID, null(r.AccountID), r.Name, null(r.Said), null(r.Template), null(r.Intent), string(conditions), string(exceptions), string(actions),
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
	return updateRule(ctx, s.db, r, now)
}

func updateRule(ctx context.Context, q execer, r rules.Rule, now int64) error {
	conditions, exceptions, actions, err := ruleJSON(r)
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx,
		`UPDATE rules SET account_id = ?, name = ?, said = ?, template = ?, intent = ?, conditions = ?, exceptions = ?, actions = ?,
		                  priority = ?, stack = ?, model = ?, min_confidence = ?, enabled = ?,
		                  version = version + 1, updated_at = ?
		 WHERE id = ? AND user_id = ?`,
		null(r.AccountID), r.Name, null(r.Said), null(r.Template), null(r.Intent), string(conditions), string(exceptions), string(actions),
		r.Priority, r.Stack, null(r.Model), r.MinConfidence, r.Enabled, now, r.ID, r.UserID)
	if err != nil {
		return fmt.Errorf("update rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const ruleColumns = `id, user_id, account_id, name, said, template, intent, conditions, exceptions, actions,
	priority, stack, model, min_confidence, enabled, version, created_at, updated_at`

func scanRule(row interface{ Scan(...any) error }) (rules.Rule, error) {
	var r rules.Rule
	var accountID sql.NullInt64
	var said, template, intent, model sql.NullString
	var minConfidence sql.NullFloat64
	var conditions, exceptions, actions string
	if err := row.Scan(&r.ID, &r.UserID, &accountID, &r.Name, &said, &template, &intent, &conditions, &exceptions, &actions,
		&r.Priority, &r.Stack, &model, &minConfidence, &r.Enabled, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return rules.Rule{}, err
	}
	r.AccountID, r.Said, r.Template, r.Intent, r.Model = accountID.Int64, said.String, template.String, intent.String, model.String
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

// HitSenderRule counts one more email settled by a sender rule.
func (s *Store) HitSenderRule(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sender_rules SET hits = hits + 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("count sender rule hit: %w", err)
	}
	return nil
}

// DeleteSenderRule removes a sender's verdict; deleting a missing one is not an error.
func (s *Store) DeleteSenderRule(ctx context.Context, userID int64, matchType, value string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM sender_rules WHERE user_id = ? AND match_type = ? AND value = ?`, userID, matchType, value); err != nil {
		return fmt.Errorf("delete sender rule: %w", err)
	}
	return nil
}

// RuleStat is how often a rule has been applied.
type RuleStat struct {
	Hits        int   // messages it was applied to since the time asked for
	LastMatchAt int64 // 0 = never
}

// RuleStats counts, per rule, the messages it was applied to since `since`, with the time
// of the latest one ever. Mail waiting in Needs review does not count: nothing was applied.
func (s *Store) RuleStats(ctx context.Context, userID, since int64) (map[int64]RuleStat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.rule_id, COALESCE(SUM(d.created_at >= ?), 0), MAX(d.created_at)
		 FROM decisions d JOIN messages m ON m.id = d.message_id JOIN rules r ON r.id = d.rule_id
		 WHERE r.user_id = ? AND m.state = 'acted' AND d.id = (SELECT MAX(id) FROM decisions WHERE message_id = m.id)
		 GROUP BY d.rule_id`, since, userID)
	if err != nil {
		return nil, fmt.Errorf("rule stats: %w", err)
	}
	defer rows.Close()
	out := map[int64]RuleStat{}
	for rows.Next() {
		var id int64
		var st RuleStat
		if err := rows.Scan(&id, &st.Hits, &st.LastMatchAt); err != nil {
			return nil, fmt.Errorf("rule stats: %w", err)
		}
		out[id] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rule stats: %w", err)
	}
	return out, nil
}

// ErrRuleSet means a reorder did not name each of the user's rules exactly once.
var ErrRuleSet = errors.New("the list must name every rule exactly once")

// ReorderRules sets the rules' priorities to the order of ids, in one transaction. A
// reorder is not an edit, so versions stay as they are.
func (s *Store) ReorderRules(ctx context.Context, userID int64, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reorder rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var have int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules WHERE user_id = ?`, userID).Scan(&have); err != nil {
		return fmt.Errorf("reorder rules: %w", err)
	}
	moved := 0
	for i, id := range ids {
		// The negative pass marks a row as placed, so an id given twice is caught below.
		res, err := tx.ExecContext(ctx, `UPDATE rules SET priority = ? WHERE id = ? AND user_id = ? AND priority >= 0`, -(i + 1), id, userID)
		if err != nil {
			return fmt.Errorf("reorder rules: %w", err)
		}
		n, _ := res.RowsAffected()
		moved += int(n)
	}
	if moved != have || len(ids) != have {
		return ErrRuleSet
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rules SET priority = -priority WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("reorder rules: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reorder rules: %w", err)
	}
	return nil
}

// NewRule is a rule to create and where in the priority order to put it.
type NewRule struct {
	Rule     rules.Rule
	Position *int // 0 = first; nil, or past the end = last
}

// CreateRules inserts rules in one transaction and returns them as saved, in the order
// given. Each lands at its position in the priority order, or at the end; the priorities of
// all the user's rules are then renumbered from 1, which is not an edit, so versions stay.
// The caller validates the rules first.
func (s *Store) CreateRules(ctx context.Context, userID int64, added []NewRule, now int64) ([]rules.Rule, error) {
	existing, err := s.Rules(ctx, userID)
	if err != nil {
		return nil, err
	}
	order := make([]int64, 0, len(existing)+len(added))
	for _, r := range existing {
		order = append(order, r.ID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	out := make([]rules.Rule, len(added))
	for i, n := range added {
		n.Rule.UserID = userID
		if out[i], err = createRule(ctx, tx, n.Rule, now); err != nil {
			return nil, err
		}
		at := len(order)
		if n.Position != nil {
			at = min(max(*n.Position, 0), len(order))
		}
		order = slices.Insert(order, at, out[i].ID)
	}
	for i, id := range order {
		if _, err := tx.ExecContext(ctx, `UPDATE rules SET priority = ? WHERE id = ?`, i+1, id); err != nil {
			return nil, fmt.Errorf("create rules: %w", err)
		}
		if j := slices.IndexFunc(out, func(r rules.Rule) bool { return r.ID == id }); j >= 0 {
			out[j].Priority = i + 1
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create rules: %w", err)
	}
	return out, nil
}

// ImportRules stores the rules of a YAML file in one transaction. A rule whose name is
// already in use replaces that rule, including its mailbox (AccountID, which the caller
// has set to the one to keep when the file says nothing), and keeps its place and id; the
// others are added after the existing rules, in file order. Rules the file does not name
// are left alone. The caller validates the rules first.
func (s *Store) ImportRules(ctx context.Context, userID int64, imported []rules.Rule, now int64) (created, updated int, err error) {
	existing, err := s.Rules(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	byName := map[string]rules.Rule{}
	last := 0
	for _, r := range existing {
		byName[r.Name] = r
		last = max(last, r.Priority)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("import rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	for _, r := range imported {
		r.UserID = userID
		if old, ok := byName[r.Name]; ok {
			r.ID, r.Priority = old.ID, old.Priority
			if err := updateRule(ctx, tx, r, now); err != nil {
				return 0, 0, err
			}
			updated++
			continue
		}
		last++
		r.Priority = last
		if _, err := createRule(ctx, tx, r, now); err != nil {
			return 0, 0, err
		}
		created++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("import rules: %w", err)
	}
	return created, updated, nil
}
