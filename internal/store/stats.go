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
	// OneClick is set when, on at least one mailbox, one of them offers an RFC 8058
	// one-click unsubscribe link.
	OneClick bool
	// UnsubscribedAt is when MailRules last unsubscribed the address, once every such mailbox
	// is unsubscribed (unsubscribes); 0 while one is not.
	UnsubscribedAt int64
}

// SendersSeen lists every address the mailboxes the viewer sees have had mail from since
// `since` (by when MailRules first saw the email).
func (s *Store) SendersSeen(ctx context.Context, v Viewer, since int64) ([]SenderSeen, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.addr, g.n, g.last_seen, g.unsub, COALESCE((SELECT from_name FROM messages WHERE id = g.last_id), ''),
		        g.links > 0, CASE WHEN g.links > 0 AND g.pending = 0
		                     THEN COALESCE((SELECT MAX(u.created_at) FROM unsubscribes u WHERE u.address = g.addr AND `+inVisible("u.account_id")+`), 0)
		                     ELSE 0 END
		 FROM (SELECT m.from_addr AS addr, COUNT(*) AS n, MAX(m.id) AS last_id,
		              MAX(COALESCE(m.received_at, m.created_at)) AS last_seen,
		              MAX(COALESCE(json_extract(m.signals, '$.list_unsubscribe'), 0)) AS unsub,
		              COUNT(m.unsubscribe_url) AS links,
		              COUNT(CASE WHEN m.unsubscribe_url IS NOT NULL AND NOT EXISTS
		                (SELECT 1 FROM unsubscribes u WHERE u.account_id = m.account_id AND u.address = m.from_addr) THEN 1 END) AS pending
		       FROM messages m
		       WHERE `+inVisible("m.account_id")+` AND m.created_at >= ? AND COALESCE(m.from_addr, '') <> ''
		       GROUP BY m.from_addr) g`, since, v.tenantArg(), v.userArg())
	if err != nil {
		return nil, fmt.Errorf("list senders: %w", err)
	}
	defer rows.Close()
	var out []SenderSeen
	for rows.Next() {
		var v SenderSeen
		if err := rows.Scan(&v.Address, &v.Messages, &v.LastSeenAt, &v.ListUnsubscribe, &v.Name, &v.OneClick, &v.UnsubscribedAt); err != nil {
			return nil, fmt.Errorf("list senders: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list senders: %w", err)
	}
	return out, nil
}

// latest is every message of the mailboxes the viewer sees whose latest decision was made
// since ?2, with that decision: the unit the stats count. A message decided twice (a retry,
// a cleanup run) counts once, as it stands now. It leaves ?1 to the query that follows, and
// binds the viewer as visibleAccounts does.
func latest() string {
	return `WITH latest AS (
	SELECT m.id AS message_id, m.state AS state, d.stage AS stage, d.rule_id AS rule_id, COALESCE(d.model, '') AS model,
	       COALESCE((SELECT name FROM rules WHERE id = d.rule_id), d.rule_name, '') AS rule_name
	FROM messages m
	JOIN decisions d ON d.id = (SELECT MAX(id) FROM decisions WHERE message_id = m.id)
	WHERE ` + inVisible("m.account_id") + ` AND d.created_at >= ?2) `
}

// countWhere is the SQL that counts the rows where cond holds: a sum of a condition taken
// as a number is SQLite's alone.
func countWhere(cond string) string {
	return `COALESCE(SUM(CASE WHEN ` + cond + ` THEN 1 ELSE 0 END), 0)`
}

// withoutModelSQL is the one definition of "decided without a model", over the latest CTE:
// a sender rule or a condition rule settled the email, no model was asked, and the rule's
// actions ran. Stage "none" (no rule matched, or no model could be asked and the email
// waits in Needs review) is not a decision by anyone, so it never counts.
const withoutModelSQL = `(state = 'acted' AND stage IN ('sender', 'condition') AND model = '')`

// Totals are the counts of the stats tiles.
type Totals struct {
	Processed    int // emails decided
	Sorted       int // of those, a rule or sender rule was applied (or recorded, in dry-run) and not undone since
	WithoutModel int // of those, a sender rule or condition rule acted on, with no model asked; never more than Processed
	Trashed      int // emails with a trash action that is in effect (or recorded, in dry-run)

	// Where the processed emails ended up; the four add up to Processed.
	WentSorted  int // a rule was applied and it did not trash the email
	WentTrash   int // a rule was applied and it trashed the email
	WentReview  int // waiting in Needs review
	WentNowhere int // left in the inbox: no rule matched, it was the mailbox's own mail, it could not be handled, or all that was done to it was undone
}

// StatsTotals counts the emails of the mailboxes the viewer sees decided since `since`.
func (s *Store) StatsTotals(ctx context.Context, v Viewer, since int64) (Totals, error) {
	var t Totals
	// The four groups come from outcomeSQL, which the feed's ?outcome= filter is built from too.
	went := outcomeSQL("state", "latest.message_id")
	err := s.db.QueryRowContext(ctx,
		latest()+`SELECT COUNT(*), `+countWhere(withoutModelSQL)+`,
		          (SELECT COUNT(DISTINCT x.message_id) FROM actions x
		           WHERE `+inVisible("x.account_id")+` AND x.kind = 'trash' AND x.status IN ('done', 'dry_run') AND x.created_at >= ?2),
		          `+countWhere(went["sorted"])+`, `+countWhere(went["trashed"])+`, `+countWhere(went["review"])+`, `+countWhere(went["inbox"])+`
		        FROM latest`, 0, since, v.tenantArg(), v.userArg()).
		Scan(&t.Processed, &t.WithoutModel, &t.Trashed, &t.WentSorted, &t.WentTrash, &t.WentReview, &t.WentNowhere)
	if err != nil {
		return Totals{}, fmt.Errorf("stats totals: %w", err)
	}
	t.Sorted = t.WentSorted + t.WentTrash
	return t, nil
}

// QuietRules counts the enabled rules of the viewer's tenant that were applied to no email
// of the mailboxes the viewer sees since `since`.
func (s *Store) QuietRules(ctx context.Context, v Viewer, since int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		latest()+`SELECT COUNT(*) FROM rules WHERE tenant_id = ?1 AND enabled = 1
		        AND id NOT IN (SELECT rule_id FROM latest WHERE rule_id IS NOT NULL AND state = 'acted')`, v.TenantID, since, v.tenantArg(), v.userArg()).Scan(&n)
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

// TopRules lists the rules applied to the most emails of the mailboxes the viewer sees
// since `since`, most first.
func (s *Store) TopRules(ctx context.Context, v Viewer, since int64, limit int) ([]RuleUse, error) {
	return s.ruleUses(ctx, "top rules",
		latest()+`SELECT rule_id, rule_name, COUNT(*), 0, 0 FROM latest WHERE state = 'acted' AND rule_name <> ''
		        GROUP BY rule_id, rule_name ORDER BY 3 DESC, rule_name LIMIT ?3`, 0, since, limit, v.tenantArg(), v.userArg())
}

// RuleCosts lists, per rule, the emails of the mailboxes the viewer sees decided for it
// since `since` and what the model decisions among them cost, dearest first. Every
// decision counts here, a retry too: it was paid for.
func (s *Store) RuleCosts(ctx context.Context, v Viewer, since int64) ([]RuleUse, error) {
	return s.ruleUses(ctx, "rule costs",
		`SELECT d.rule_id, COALESCE((SELECT name FROM rules WHERE id = d.rule_id), d.rule_name, '') AS name,
		        COUNT(DISTINCT d.message_id), `+countWhere(`COALESCE(d.model, '') <> ''`)+`, COALESCE(SUM(d.cost_usd), 0)
		 FROM decisions d JOIN messages m ON m.id = d.message_id
		 WHERE `+inVisible("m.account_id")+` AND d.created_at >= ?
		 GROUP BY d.rule_id, name ORDER BY 5 DESC, 3 DESC, name`, since, v.tenantArg(), v.userArg())
}

// UsageRow is one row of the cost ledger.
type UsageRow struct {
	Day       string // YYYY-MM-DD, UTC
	Provider  string
	Model     string
	Purpose   string // decide | escalate | compose | test | cleanup | suggest
	Operator  bool   // made with a key the operator holds (MAILRULES_MODE=cloud)
	Calls     int
	TokensIn  int
	TokensOut int
	CostUSD   float64
}

// Usage lists the tenant's cost ledger from sinceDay (YYYY-MM-DD) on, oldest day first.
func (s *Store) Usage(ctx context.Context, tenantID int64, sinceDay string) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT day, provider, model, purpose, operator, calls, tokens_in, tokens_out, cost_usd FROM usage_daily
		 WHERE tenant_id = ? AND day >= ? ORDER BY day, provider, model, purpose, operator`, tenantID, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("list usage: %w", err)
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Day, &u.Provider, &u.Model, &u.Purpose, &u.Operator, &u.Calls, &u.TokensIn, &u.TokensOut, &u.CostUSD); err != nil {
			return nil, fmt.Errorf("list usage: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list usage: %w", err)
	}
	return out, nil
}
