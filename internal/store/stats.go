package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SenderSeen is what the messages table knows about one sender address.
type SenderSeen struct {
	Address         string
	Name            string // the From display name of the newest email; may be empty
	Messages        int    // emails seen since the time asked for
	LastSeenAt      int64
	ListUnsubscribe bool // at least one of them carried a List-Unsubscribe header
}

// SendersSeen lists every address the user's accounts have seen mail from since `since`
// (by when MailRules first saw the email).
func (s *Store) SendersSeen(ctx context.Context, userID, since int64) ([]SenderSeen, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.addr, g.n, g.last_seen, g.unsub, COALESCE((SELECT from_name FROM messages WHERE id = g.last_id), '')
		 FROM (SELECT m.from_addr AS addr, COUNT(*) AS n, MAX(m.id) AS last_id,
		              MAX(COALESCE(m.received_at, m.created_at)) AS last_seen,
		              MAX(COALESCE(json_extract(m.signals, '$.list_unsubscribe'), 0)) AS unsub
		       FROM messages m JOIN accounts a ON a.id = m.account_id
		       WHERE a.user_id = ? AND m.created_at >= ? AND COALESCE(m.from_addr, '') <> ''
		       GROUP BY m.from_addr) g`, userID, since)
	if err != nil {
		return nil, fmt.Errorf("list senders: %w", err)
	}
	defer rows.Close()
	var out []SenderSeen
	for rows.Next() {
		var v SenderSeen
		if err := rows.Scan(&v.Address, &v.Messages, &v.LastSeenAt, &v.ListUnsubscribe, &v.Name); err != nil {
			return nil, fmt.Errorf("list senders: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list senders: %w", err)
	}
	return out, nil
}

// latest is every message of the user whose latest decision was made since ?2, with that
// decision: the unit the stats count. A message decided twice (a retry, a cleanup run)
// counts once, as it stands now.
const latest = `WITH latest AS (
	SELECT m.id AS message_id, m.state AS state, d.rule_id AS rule_id, COALESCE(d.model, '') AS model,
	       COALESCE((SELECT name FROM rules WHERE id = d.rule_id), d.rule_name, '') AS rule_name
	FROM messages m JOIN accounts a ON a.id = m.account_id
	JOIN decisions d ON d.id = (SELECT MAX(id) FROM decisions WHERE message_id = m.id)
	WHERE a.user_id = ?1 AND d.created_at >= ?2) `

// Totals are the counts of the stats tiles.
type Totals struct {
	Processed    int // emails decided
	Sorted       int // of those, a rule or sender rule was applied (or recorded, in dry-run)
	WithoutModel int // of those, settled without asking a model
	Trashed      int // emails with a trash action that is in effect (or recorded, in dry-run)

	// Where the processed emails ended up; the four add up to Processed.
	WentSorted  int // a rule was applied and it did not trash the email
	WentTrash   int // a rule was applied and it trashed the email
	WentReview  int // waiting in Needs review
	WentNowhere int // left in the inbox: no rule matched, or it could not be handled
}

// StatsTotals counts the user's emails decided since `since`.
func (s *Store) StatsTotals(ctx context.Context, userID, since int64) (Totals, error) {
	var t Totals
	const trash = `x.kind = 'trash' AND x.status IN ('done', 'dry_run') AND x.created_at >= ?2`
	err := s.db.QueryRowContext(ctx,
		latest+`SELECT COUNT(*), COALESCE(SUM(state = 'acted'), 0), COALESCE(SUM(model = ''), 0),
		          (SELECT COUNT(DISTINCT x.message_id) FROM actions x JOIN accounts a ON a.id = x.account_id
		           WHERE a.user_id = ?1 AND `+trash+`),
		          COALESCE(SUM(state = 'acted' AND EXISTS (SELECT 1 FROM actions x WHERE x.message_id = latest.message_id AND `+trash+`)), 0),
		          COALESCE(SUM(state = 'review'), 0), COALESCE(SUM(state NOT IN ('acted', 'review')), 0)
		        FROM latest`, userID, since).
		Scan(&t.Processed, &t.Sorted, &t.WithoutModel, &t.Trashed, &t.WentTrash, &t.WentReview, &t.WentNowhere)
	if err != nil {
		return Totals{}, fmt.Errorf("stats totals: %w", err)
	}
	t.WentSorted = t.Sorted - t.WentTrash
	return t, nil
}

// QuietRules counts the user's enabled rules that were applied to no email since `since`.
func (s *Store) QuietRules(ctx context.Context, userID, since int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		latest+`SELECT COUNT(*) FROM rules WHERE user_id = ?1 AND enabled = 1
		        AND id NOT IN (SELECT rule_id FROM latest WHERE rule_id IS NOT NULL AND state = 'acted')`, userID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("quiet rules: %w", err)
	}
	return n, nil
}

// RuleUse is what one rule did, or cost. RuleID is 0 with a name for a rule that has been
// deleted since, and 0 without one for "no rule".
type RuleUse struct {
	RuleID   int64
	RuleName string
	Emails   int
	Calls    int // decisions a model was asked for
	CostUSD  float64
}

func (s *Store) ruleUses(ctx context.Context, what, query string, args ...any) ([]RuleUse, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []RuleUse
	for rows.Next() {
		var u RuleUse
		var id sql.NullInt64
		if err := rows.Scan(&id, &u.RuleName, &u.Emails, &u.Calls, &u.CostUSD); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		u.RuleID = id.Int64
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// TopRules lists the rules applied to the most emails since `since`, most first.
func (s *Store) TopRules(ctx context.Context, userID, since int64, limit int) ([]RuleUse, error) {
	return s.ruleUses(ctx, "top rules",
		latest+`SELECT rule_id, rule_name, COUNT(*), 0, 0 FROM latest WHERE state = 'acted' AND rule_name <> ''
		        GROUP BY rule_id, rule_name ORDER BY 3 DESC, rule_name LIMIT ?3`, userID, since, limit)
}

// RuleCosts lists, per rule, the emails decided for it since `since` and what the model
// decisions among them cost, dearest first. Every decision counts here, a retry too: it
// was paid for.
func (s *Store) RuleCosts(ctx context.Context, userID, since int64) ([]RuleUse, error) {
	return s.ruleUses(ctx, "rule costs",
		`SELECT d.rule_id, COALESCE((SELECT name FROM rules WHERE id = d.rule_id), d.rule_name, '') AS name,
		        COUNT(DISTINCT d.message_id), COALESCE(SUM(COALESCE(d.model, '') <> ''), 0), COALESCE(SUM(d.cost_usd), 0)
		 FROM decisions d JOIN messages m ON m.id = d.message_id JOIN accounts a ON a.id = m.account_id
		 WHERE a.user_id = ? AND d.created_at >= ?
		 GROUP BY d.rule_id, name ORDER BY 5 DESC, 3 DESC, name`, userID, since)
}

// UsageRow is one row of the cost ledger.
type UsageRow struct {
	Day       string // YYYY-MM-DD, UTC
	Provider  string
	Model     string
	Purpose   string // decide | escalate | compose | test
	Calls     int
	TokensIn  int
	TokensOut int
	CostUSD   float64
}

// Usage lists the cost ledger from sinceDay (YYYY-MM-DD) on, oldest day first.
func (s *Store) Usage(ctx context.Context, sinceDay string) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT day, provider, model, purpose, calls, tokens_in, tokens_out, cost_usd FROM usage_daily
		 WHERE day >= ? ORDER BY day, provider, model, purpose`, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("list usage: %w", err)
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Day, &u.Provider, &u.Model, &u.Purpose, &u.Calls, &u.TokensIn, &u.TokensOut, &u.CostUSD); err != nil {
			return nil, fmt.Errorf("list usage: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list usage: %w", err)
	}
	return out, nil
}

// DecideCost is what one live decision has cost on average so far, the fallback's share
// included: the ledger's "decide" and "escalate" cost over its "decide" calls. 0 until
// there is history.
func (s *Store) DecideCost(ctx context.Context) (float64, error) {
	var cost float64
	var calls int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0), COALESCE(SUM(CASE WHEN purpose = 'decide' THEN calls END), 0)
		 FROM usage_daily WHERE purpose IN ('decide', 'escalate')`).Scan(&cost, &calls)
	if err != nil {
		return 0, fmt.Errorf("average decision cost: %w", err)
	}
	if calls == 0 {
		return 0, nil
	}
	return cost / float64(calls), nil
}
