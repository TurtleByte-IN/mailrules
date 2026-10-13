package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

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

// ruleJSON encodes the four JSON columns of a rule.
func ruleJSON(r rules.Rule) (conditions, exceptions, actions, notify []byte, err error) {
	if conditions, err = json.Marshal(r.Conditions); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("encode conditions: %w", err)
	}
	if exceptions, err = json.Marshal(r.Exceptions); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("encode exceptions: %w", err)
	}
	if actions, err = json.Marshal(r.Actions); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("encode actions: %w", err)
	}
	if notify, err = json.Marshal(append([]int64{}, r.Notify...)); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("encode notify: %w", err)
	}
	return conditions, exceptions, actions, notify, nil
}

// CreateRule inserts a rule of the tenant at version 1 and returns it with its id.
// r.UserID is its author. The caller validates the rule first.
func (s *Store) CreateRule(ctx context.Context, tenantID int64, r rules.Rule, now int64) (rules.Rule, error) {
	err := s.change(ctx, tenantID, "create rule", func(ctx context.Context, tx *txn) error {
		var err error
		r, err = createRule(ctx, tx, tenantID, r, now)
		return err
	})
	if err != nil {
		return rules.Rule{}, err
	}
	return r, nil
}

// createRule inserts the rule in q, a transaction, and records it in the audit log.
func createRule(ctx context.Context, q querier, tenantID int64, r rules.Rule, now int64) (rules.Rule, error) {
	conditions, exceptions, actions, notify, err := ruleJSON(r)
	if err != nil {
		return rules.Rule{}, err
	}
	if err := q.QueryRowContext(ctx,
		`INSERT INTO rules (tenant_id, user_id, account_id, name, said, template, intent, conditions, exceptions, actions,
		                    priority, stack, model, min_confidence, enabled, mailbox_removed, notify, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?) RETURNING id`,
		tenantID, r.UserID, null(r.AccountID), r.Name, null(r.Said), null(r.Template), null(r.Intent), string(conditions), string(exceptions), string(actions),
		r.Priority, r.Stack, null(r.Model), r.MinConfidence, r.Enabled, r.MailboxRemoved, string(notify), now, now).Scan(&r.ID); err != nil {
		return rules.Rule{}, fmt.Errorf("create rule: %w", err)
	}
	r.Version, r.CreatedAt, r.UpdatedAt = 1, now, now
	detail := "on"
	if !r.Enabled {
		detail = "off"
	}
	if err := record(ctx, q, tenantID, now, AuditRuleCreated, r.Name, detail); err != nil {
		return rules.Rule{}, err
	}
	return r, nil
}

// UpdateRule rewrites the editable columns of one of the tenant's rules and bumps its
// version, so past decisions keep pointing at the wording they were made under. The
// author (UserID) stays as it is. A rule of another tenant is ErrNotFound.
func (s *Store) UpdateRule(ctx context.Context, tenantID int64, r rules.Rule, now int64) error {
	return s.change(ctx, tenantID, "update rule", func(ctx context.Context, tx *txn) error {
		return updateRule(ctx, tx, tenantID, r, now)
	})
}

// updateRule rewrites the rule in q, a transaction, and records in the audit log what of it
// changed.
func updateRule(ctx context.Context, q querier, tenantID int64, r rules.Rule, now int64) error {
	conditions, exceptions, actions, notify, err := ruleJSON(r)
	if err != nil {
		return err
	}
	var old rules.Rule
	if _, audited := ActorOf(ctx); audited {
		old, err = scanRule(q.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE id = ? AND tenant_id = ?`, r.ID, tenantID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("update rule: %w", err)
		}
	}
	res, err := q.ExecContext(ctx,
		`UPDATE rules SET account_id = ?, name = ?, said = ?, template = ?, intent = ?, conditions = ?, exceptions = ?, actions = ?,
		                  priority = ?, stack = ?, model = ?, min_confidence = ?, enabled = ?, mailbox_removed = ?, notify = ?,
		                  version = version + 1, updated_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		null(r.AccountID), r.Name, null(r.Said), null(r.Template), null(r.Intent), string(conditions), string(exceptions), string(actions),
		r.Priority, r.Stack, null(r.Model), r.MinConfidence, r.Enabled, r.MailboxRemoved, string(notify), now, r.ID, tenantID)
	if err != nil {
		return fmt.Errorf("update rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if old.ID == 0 {
		return nil
	}
	return record(ctx, q, tenantID, now, AuditRuleChanged, r.Name, ruleChanges(old, r))
}

// ruleChanges says, for the audit log, what of a rule an edit changed: "renamed from
// Newsletters, conditions, turned off". Never the conditions themselves.
func ruleChanges(old, r rules.Rule) string {
	var out []string
	if old.Name != r.Name {
		out = append(out, "renamed from "+old.Name)
	}
	same := func(a, b any) bool {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return string(x) == string(y)
	}
	for _, f := range []struct {
		what string
		same bool
	}{
		{"description", old.Said == r.Said && old.Intent == r.Intent},
		{"conditions", same(old.Conditions, r.Conditions)},
		{"exceptions", same(old.Exceptions, r.Exceptions)},
		{"actions", same(old.Actions, r.Actions)},
		{"mailbox", old.AccountID == r.AccountID && old.MailboxRemoved == r.MailboxRemoved},
		{"model", old.Model == r.Model && same(old.MinConfidence, r.MinConfidence)},
		{"stack", old.Stack == r.Stack},
		{"notifications", same(old.Notify, r.Notify)},
	} {
		if !f.same {
			out = append(out, f.what)
		}
	}
	switch {
	case old.Enabled && !r.Enabled:
		out = append(out, "turned off")
	case !old.Enabled && r.Enabled:
		out = append(out, "turned on")
	}
	return strings.Join(out, ", ")
}

const ruleColumns = `id, user_id, account_id, name, said, template, intent, conditions, exceptions, actions,
	priority, stack, model, min_confidence, enabled, mailbox_removed, notify, version, created_at, updated_at`

// scanRule reads ruleColumns, then into more whatever the query selects after them.
func scanRule(row interface{ Scan(...any) error }, more ...any) (rules.Rule, error) {
	var r rules.Rule
	var accountID sql.NullInt64
	var said, template, intent, model sql.NullString
	var minConfidence sql.NullFloat64
	var conditions, exceptions, actions, notify string
	if err := row.Scan(append([]any{&r.ID, &r.UserID, &accountID, &r.Name, &said, &template, &intent, &conditions, &exceptions, &actions,
		&r.Priority, &r.Stack, &model, &minConfidence, &r.Enabled, &r.MailboxRemoved, &notify, &r.Version, &r.CreatedAt, &r.UpdatedAt}, more...)...); err != nil {
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
		json.Unmarshal([]byte(notify), &r.Notify),
	); err != nil {
		return rules.Rule{}, fmt.Errorf("decode rule %d: %w", r.ID, err)
	}
	if len(r.Notify) == 0 {
		r.Notify = nil
	}
	return r, nil
}

// Rule returns one of the tenant's rules, or ErrNotFound.
func (s *Store) Rule(ctx context.Context, tenantID, id int64) (rules.Rule, error) {
	ctx = WithTenant(ctx, tenantID)
	r, err := scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE id = ? AND tenant_id = ?`, id, tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return rules.Rule{}, ErrNotFound
	}
	if err != nil {
		return rules.Rule{}, fmt.Errorf("get rule: %w", err)
	}
	return r, nil
}

// Rules returns all of the tenant's rules, disabled ones included, in priority order.
func (s *Store) Rules(ctx context.Context, tenantID int64) ([]rules.Rule, error) {
	ctx = WithTenant(ctx, tenantID)
	return listRules(ctx, s.db, tenantID)
}

// listRules reads, through q, all of the tenant's rules in priority order.
func listRules(ctx context.Context, q querier, tenantID int64) ([]rules.Rule, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+ruleColumns+` FROM rules WHERE tenant_id = ? ORDER BY priority, id`, tenantID)
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

// DeleteRule removes one of the tenant's rules. The schema takes the sender rules that
// route to it along (ON DELETE CASCADE) and keeps its decisions and corrections, with their
// rule set to NULL.
func (s *Store) DeleteRule(ctx context.Context, tenantID, id int64) error {
	return s.change(ctx, tenantID, "delete rule", func(ctx context.Context, tx *txn) error {
		var name string
		err := tx.QueryRowContext(ctx, `DELETE FROM rules WHERE id = ? AND tenant_id = ? RETURNING name`, id, tenantID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete rule: %w", err)
		}
		return record(ctx, tx, tenantID, 0, AuditRuleDeleted, name, "")
	})
}

// tenantRule is a rule with the tenant it belongs to.
type tenantRule struct {
	rules.Rule
	tenantID int64
}

// rulesNamingMailboxes returns, for every tenant, the rules that may name a mailbox: those
// limited to one and those with an account condition. It reads q, a transaction.
func rulesNamingMailboxes(ctx context.Context, q *txn) ([]tenantRule, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+ruleColumns+`, tenant_id FROM rules
		WHERE account_id IS NOT NULL OR conditions LIKE '%"field":"account"%' OR exceptions LIKE '%"field":"account"%'
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list rules naming mailboxes: %w", err)
	}
	defer rows.Close()
	var out []tenantRule
	for rows.Next() {
		var t tenantRule
		if t.Rule, err = scanRule(rows, &t.tenantID); err != nil {
			return nil, fmt.Errorf("list rules naming mailboxes: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rules naming mailboxes: %w", err)
	}
	return out, nil
}

// dropMailboxes rewrites, in tx, every rule naming one of the mailboxes gone reports as
// gone, as rules.Rule.DropAccount has it, as an edit (the version goes up). It returns the
// rules it changed, as saved.
func dropMailboxes(ctx context.Context, tx *txn, gone func(id int64) bool, now int64) ([]rules.Rule, error) {
	rs, err := rulesNamingMailboxes(ctx, tx)
	if err != nil {
		return nil, err
	}
	var changed []rules.Rule
	for _, t := range rs {
		r, edited := t.Rule, false
		for _, id := range r.AccountIDs() {
			if gone(id) {
				var ch bool
				r, ch = r.DropAccount(id)
				edited = edited || ch
			}
		}
		if !edited {
			continue
		}
		if err := updateRule(ctx, tx, t.tenantID, r, now); err != nil {
			return nil, err
		}
		r.Version, r.UpdatedAt = r.Version+1, now
		changed = append(changed, r)
	}
	return changed, nil
}

// ReconcileRemovedMailboxes fixes the rules that still name a mailbox removed before
// DeleteAccount kept them (MAI-132): every mailbox a rule names that has no account any
// more is dropped from it as DeleteAccount would have, for every tenant, each in one
// transaction of its own. It returns the rules it changed, by id; a second run changes
// nothing.
func (s *Store) ReconcileRemovedMailboxes(ctx context.Context, now int64) ([]rules.Rule, error) {
	var changed []rules.Rule
	if err := s.eachTenant(ctx, tenantsAll, func(ctx context.Context, _ int64) error {
		ch, err := reconcileRemovedMailboxes(ctx, s.db, now)
		changed = append(changed, ch...)
		return err
	}); err != nil {
		return nil, err
	}
	slices.SortStableFunc(changed, func(a, b rules.Rule) int { return cmp.Compare(a.ID, b.ID) })
	return changed, nil
}

// reconcileRemovedMailboxes is ReconcileRemovedMailboxes over the rules and mailboxes ctx
// sees, in one transaction.
func reconcileRemovedMailboxes(ctx context.Context, db *conn, now int64) ([]rules.Rule, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reconcile removed mailboxes: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	have, err := accountIDs(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("reconcile removed mailboxes: %w", err)
	}
	changed, err := dropMailboxes(ctx, tx, func(id int64) bool { return !have[id] }, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reconcile removed mailboxes: %w", err)
	}
	return changed, nil
}

// accountIDs is the set of every account's id, read in tx.
func accountIDs(ctx context.Context, tx *txn) (map[int64]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		have[id] = true
	}
	return have, rows.Err()
}

// PutSenderRule stores the tenant's verdict for a sender address or domain, replacing any
// earlier one for the same sender, and returns it with its id. sr.UserID is who made it.
// A rule_id that is not one of the tenant's rules is refused as ErrNotFound.
func (s *Store) PutSenderRule(ctx context.Context, tenantID int64, sr rules.SenderRule, now int64) (rules.SenderRule, error) {
	ctx = WithTenant(ctx, tenantID)
	if sr.RuleID != 0 {
		if _, err := s.Rule(ctx, tenantID, sr.RuleID); err != nil {
			return rules.SenderRule{}, err
		}
	}
	err := s.change(ctx, tenantID, "put sender rule", func(ctx context.Context, tx *txn) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO sender_rules (tenant_id, user_id, match_type, value, rule_id, verdict, folder, source, hits, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
			 ON CONFLICT (tenant_id, match_type, value) DO UPDATE
			 SET rule_id = excluded.rule_id, verdict = excluded.verdict, folder = excluded.folder, source = excluded.source
			 RETURNING id, hits, created_at`,
			tenantID, sr.UserID, sr.MatchType, sr.Value, null(sr.RuleID), sr.Verdict, null(sr.Folder), sr.Source, now).
			Scan(&sr.ID, &sr.Hits, &sr.CreatedAt); err != nil {
			return fmt.Errorf("put sender rule: %w", err)
		}
		return record(ctx, tx, tenantID, now, AuditSenderRuleSet, sr.Value, senderVerdict(sr))
	})
	if err != nil {
		return rules.SenderRule{}, err
	}
	return sr, nil
}

// senderVerdict is a sender rule's verdict for the audit log: "move to Receipts, learned".
func senderVerdict(sr rules.SenderRule) string {
	out := sr.Verdict
	if sr.Folder != "" {
		out += " to " + sr.Folder
	}
	if sr.Source != "" && sr.Source != "user" {
		out += ", " + sr.Source
	}
	return out
}

// SenderRules returns all of the tenant's sender rules.
func (s *Store) SenderRules(ctx context.Context, tenantID int64) ([]rules.SenderRule, error) {
	ctx = WithTenant(ctx, tenantID)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, match_type, value, rule_id, verdict, folder, source, hits, created_at
		 FROM sender_rules WHERE tenant_id = ? ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list sender rules: %w", err)
	}
	defer rows.Close()
	var out []rules.SenderRule
	for rows.Next() {
		var sr rules.SenderRule
		var ruleID sql.NullInt64
		var folder sql.NullString
		if err := rows.Scan(&sr.ID, &sr.UserID, &sr.MatchType, &sr.Value, &ruleID, &sr.Verdict, &folder, &sr.Source, &sr.Hits, &sr.CreatedAt); err != nil {
			return nil, fmt.Errorf("list sender rules: %w", err)
		}
		sr.RuleID, sr.Folder = ruleID.Int64, folder.String
		out = append(out, sr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sender rules: %w", err)
	}
	return out, nil
}

// HitSenderRule counts one more email settled by a sender rule.
func (s *Store) HitSenderRule(ctx context.Context, id int64) error {
	ctx = s.tenantFor(ctx, ofSenderRule, id)
	if _, err := s.db.ExecContext(ctx, `UPDATE sender_rules SET hits = hits + 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("count sender rule hit: %w", err)
	}
	return nil
}

// DeleteSenderRule removes one of the tenant's sender verdicts; deleting a missing one is
// not an error.
func (s *Store) DeleteSenderRule(ctx context.Context, tenantID int64, matchType, value string) error {
	return s.change(ctx, tenantID, "delete sender rule", func(ctx context.Context, tx *txn) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM sender_rules WHERE tenant_id = ? AND match_type = ? AND value = ?`, tenantID, matchType, value)
		if err != nil {
			return fmt.Errorf("delete sender rule: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return record(ctx, tx, tenantID, 0, AuditSenderRuleDeleted, value, "")
	})
}

// RuleStat is how often a rule has been applied.
type RuleStat struct {
	Hits        int   // messages it was applied to since the time asked for
	LastMatchAt int64 // 0 = never
}

// RuleStats counts, per rule of the viewer's tenant, the messages it was applied to since
// `since` in the mailboxes the viewer sees, with the time of the latest one ever. Mail
// waiting in Needs review does not count: nothing was applied.
func (s *Store) RuleStats(ctx context.Context, v Viewer, since int64) (map[int64]RuleStat, error) {
	ctx = WithTenant(ctx, v.TenantID)
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.rule_id, COALESCE(SUM(CASE WHEN d.created_at >= ? THEN 1 ELSE 0 END), 0), MAX(d.created_at)
		 FROM decisions d JOIN messages m ON m.id = d.message_id JOIN rules r ON r.id = d.rule_id
		 WHERE r.tenant_id = ? AND `+inVisible("m.account_id")+` AND m.state = 'acted'
		   AND d.id = (SELECT MAX(id) FROM decisions WHERE message_id = m.id)
		 GROUP BY d.rule_id`, since, v.TenantID, v.tenantArg(), v.userArg())
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

// ErrRuleSet means a reorder did not name each of the tenant's rules exactly once.
var ErrRuleSet = errors.New("the list must name every rule exactly once")

// ReorderRules sets the priorities of the tenant's rules to the order of ids, in one
// transaction. A reorder is not an edit, so versions stay as they are.
func (s *Store) ReorderRules(ctx context.Context, tenantID int64, ids []int64) error {
	ctx = WithTenant(ctx, tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reorder rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var have int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules WHERE tenant_id = ?`, tenantID).Scan(&have); err != nil {
		return fmt.Errorf("reorder rules: %w", err)
	}
	moved := 0
	for i, id := range ids {
		// The negative pass marks a row as placed, so an id given twice is caught below.
		res, err := tx.ExecContext(ctx, `UPDATE rules SET priority = ? WHERE id = ? AND tenant_id = ? AND priority >= 0`, -(i + 1), id, tenantID)
		if err != nil {
			return fmt.Errorf("reorder rules: %w", err)
		}
		n, _ := res.RowsAffected()
		moved += int(n)
	}
	if moved != have || len(ids) != have {
		return ErrRuleSet
	}
	// Only the rows placed above: a rule another connection added meanwhile (no single
	// writer serialises them on every database) keeps its priority rather than losing it.
	if _, err := tx.ExecContext(ctx, `UPDATE rules SET priority = -priority WHERE tenant_id = ? AND priority < 0`, tenantID); err != nil {
		return fmt.Errorf("reorder rules: %w", err)
	}
	if err := record(ctx, tx, tenantID, 0, AuditRulesReordered, "", fmt.Sprintf("%d rules", have)); err != nil {
		return err
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

// CreateRules inserts rules of the tenant, written by userID, in one transaction and
// returns them as saved, in the order given. Each lands at its position in the priority
// order, or at the end; the priorities of all the tenant's rules are then renumbered from
// 1, which is not an edit, so versions stay. The caller validates the rules first.
func (s *Store) CreateRules(ctx context.Context, tenantID, userID int64, added []NewRule, now int64) ([]rules.Rule, error) {
	ctx = WithTenant(ctx, tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	existing, err := listRules(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	order := make([]int64, 0, len(existing)+len(added))
	for _, r := range existing {
		order = append(order, r.ID)
	}
	out := make([]rules.Rule, len(added))
	for i, n := range added {
		n.Rule.UserID = userID
		if out[i], err = createRule(ctx, tx, tenantID, n.Rule, now); err != nil {
			return nil, err
		}
		at := len(order)
		if n.Position != nil {
			at = min(max(*n.Position, 0), len(order))
		}
		order = slices.Insert(order, at, out[i].ID)
	}
	for i, id := range order {
		if _, err := tx.ExecContext(ctx, `UPDATE rules SET priority = ? WHERE id = ? AND tenant_id = ?`, i+1, id, tenantID); err != nil {
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

// ImportRules stores the rules of a YAML file in the tenant in one transaction; userID is
// the author of the rules it adds. A rule whose name is already in use replaces that rule,
// including its mailbox (AccountID, which the caller has set to the one to keep when the
// file says nothing), and keeps its place, id and author; the others are added after the
// existing rules, in file order. Rules the file does not name are left alone. The caller
// validates the rules first.
func (s *Store) ImportRules(ctx context.Context, tenantID, userID int64, imported []rules.Rule, now int64) (created, updated int, err error) {
	ctx = WithTenant(ctx, tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("import rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	existing, err := listRules(ctx, tx, tenantID)
	if err != nil {
		return 0, 0, err
	}
	byName := map[string]rules.Rule{}
	last := 0
	for _, r := range existing {
		byName[r.Name] = r
		last = max(last, r.Priority)
	}
	for _, r := range imported {
		r.UserID = userID
		if old, ok := byName[r.Name]; ok {
			r.ID, r.Priority, r.UserID, r.Notify = old.ID, old.Priority, old.UserID, old.Notify // a rules file carries no destinations
			if err := updateRule(ctx, tx, tenantID, r, now); err != nil {
				return 0, 0, err
			}
			updated++
			continue
		}
		last++
		r.Priority = last
		if _, err := createRule(ctx, tx, tenantID, r, now); err != nil {
			return 0, 0, err
		}
		created++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("import rules: %w", err)
	}
	return created, updated, nil
}
