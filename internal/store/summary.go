package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Summary statuses, as stored in summaries.status.
const (
	SummarySending = "sending"
	SummarySent    = "sent"
	SummaryFailed  = "failed"
)

// SettingSummary holds the summary email's settings, one JSON object (internal/summary).
const SettingSummary = "summary"

// Summary is one row of summaries: a scheduled summary email.
type Summary struct {
	ID            int64
	UserID        int64
	DueAt         int64
	Status        string
	Attempts      int
	LastAttemptAt int64
	PeriodStart   int64 // 0 until sent
	PeriodEnd     int64
	SentAt        int64
}

// ClaimSummary records an attempt at the summary due at dueAt and reports whether this
// caller is to make it, and which attempt it is. It is one statement, so of two callers
// at once only one gets it. It is not to be made when it was sent, when an attempt is
// running or was cut off (status sending), after maxAttempts, or while the last failed
// attempt is younger than its back-off: a minute after the first, doubling each time.
func (s *Store) ClaimSummary(ctx context.Context, userID, dueAt, now int64, maxAttempts int) (id int64, attempt int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO summaries (user_id, due_at, status, attempts, last_attempt_at) VALUES (?1, ?2, 'sending', 1, ?3)
		 ON CONFLICT (user_id, due_at) DO UPDATE SET status = 'sending', attempts = attempts + 1, last_attempt_at = ?3
		 WHERE summaries.status = 'failed' AND summaries.attempts < ?4
		   AND summaries.last_attempt_at + (60 << (summaries.attempts - 1)) <= ?3
		 RETURNING id, attempts`, userID, dueAt, now, maxAttempts).Scan(&id, &attempt)
	if errors.Is(err, sql.ErrNoRows) { // the conflict's WHERE left the row as it was
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("claim summary: %w", err)
	}
	return id, attempt, true, nil
}

// FinishSummary records how a claimed attempt ended: sent, with the time it covered, or failed.
func (s *Store) FinishSummary(ctx context.Context, id int64, sent bool, periodStart, periodEnd, now int64) error {
	var err error
	if sent {
		_, err = s.db.ExecContext(ctx, `UPDATE summaries SET status = 'sent', period_start = ?, period_end = ?, sent_at = ? WHERE id = ?`,
			periodStart, periodEnd, now, id)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE summaries SET status = 'failed' WHERE id = ?`, id)
	}
	if err != nil {
		return fmt.Errorf("finish summary: %w", err)
	}
	return nil
}

// LastSentSummary returns the user's latest summary that went out, or ErrNotFound.
func (s *Store) LastSentSummary(ctx context.Context, userID int64) (Summary, error) {
	var v Summary
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, due_at, status, attempts, last_attempt_at, period_start, period_end, sent_at
		 FROM summaries WHERE user_id = ? AND status = 'sent' ORDER BY sent_at DESC, id DESC LIMIT 1`, userID).
		Scan(&v.ID, &v.UserID, &v.DueAt, &v.Status, &v.Attempts, &v.LastAttemptAt, &v.PeriodStart, &v.PeriodEnd, &v.SentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Summary{}, ErrNotFound
	}
	if err != nil {
		return Summary{}, fmt.Errorf("last summary: %w", err)
	}
	return v, nil
}

// SummaryEmail is one email as a summary lists it: who sent it, its subject and, for a
// trashed one, where it went. Never its text.
type SummaryEmail struct {
	MessageID int64
	FromName  string
	FromAddr  string
	Subject   string
	Folder    string // a trashed email: the folder the trash moved it to; "" = the server's Trash
	DryRun    bool   // a trashed email: the trash was only recorded, in dry-run
}

// windowLatest is latest (stats.go) with an end: every message of the mailboxes the viewer
// sees whose latest decision was made in [?1, ?2).
func windowLatest(v Viewer) string {
	return `WITH latest AS (
	SELECT m.id AS message_id, m.state AS state, d.rule_id AS rule_id,
	       COALESCE((SELECT name FROM rules WHERE id = d.rule_id), d.rule_name, '') AS rule_name
	FROM messages m
	JOIN decisions d ON d.id = (SELECT MAX(id) FROM decisions WHERE message_id = m.id)
	WHERE ` + inVisible("m.account_id", v) + ` AND d.created_at >= ?1 AND d.created_at < ?2) `
}

// SummaryRules lists, per rule, the emails of the mailboxes the viewer sees a rule was
// applied to (sorted or trashed, as the Overview counts them) whose latest decision was
// made in [from, to), most first.
func (s *Store) SummaryRules(ctx context.Context, v Viewer, from, to int64) ([]RuleUse, error) {
	went := outcomeSQL("state", "latest.message_id")
	return s.ruleUses(ctx, "summary rules",
		windowLatest(v)+`SELECT rule_id, rule_name, COUNT(*), 0, 0 FROM latest
		 WHERE rule_name <> '' AND ((`+went["sorted"]+`) OR (`+went["trashed"]+`))
		 GROUP BY rule_id, rule_name ORDER BY 3 DESC, rule_name`, from, to)
}

// SummaryTrashed lists the emails of the mailboxes the viewer sees trashed (or, in
// dry-run, recorded as trashed) in [from, to) and not undone since, newest first, at most
// limit of them, and how many there are.
func (s *Store) SummaryTrashed(ctx context.Context, v Viewer, from, to int64, limit int) ([]SummaryEmail, int, error) {
	where := ` FROM actions x JOIN messages m ON m.id = x.message_id
		 WHERE ` + inVisible("x.account_id", v) + ` AND x.kind = 'trash' AND x.status IN ('done', 'dry_run') AND x.created_at >= ?1 AND x.created_at < ?2`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT x.message_id)`+where, from, to).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("summary trashed: %w", err)
	}
	out, err := s.summaryEmails(ctx, "summary trashed",
		`SELECT m.id, COALESCE(m.from_name, ''), COALESCE(m.from_addr, ''), COALESCE(m.subject, ''),
		        COALESCE(json_extract(x.params, '$.folder'), json_extract(x.after, '$.folder'), ''), x.status = 'dry_run'`+where+`
		 GROUP BY m.id ORDER BY MAX(x.id) DESC LIMIT ?3`, from, to, limit)
	return out, total, err
}

// SummaryReview lists the emails of the mailboxes the viewer sees waiting in Needs review
// now, newest first, at most limit of them, and how many there are.
func (s *Store) SummaryReview(ctx context.Context, v Viewer, limit int) ([]SummaryEmail, int, error) {
	where := ` FROM messages m WHERE ` + inVisible("m.account_id", v) + ` AND m.state = 'review'`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+where).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("summary review: %w", err)
	}
	out, err := s.summaryEmails(ctx, "summary review",
		`SELECT m.id, COALESCE(m.from_name, ''), COALESCE(m.from_addr, ''), COALESCE(m.subject, ''), '', 0`+where+`
		 ORDER BY m.acted_at DESC, m.id DESC LIMIT ?`, limit)
	return out, total, err
}

func (s *Store) summaryEmails(ctx context.Context, what, query string, args ...any) ([]SummaryEmail, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	out := []SummaryEmail{}
	for rows.Next() {
		var e SummaryEmail
		if err := rows.Scan(&e.MessageID, &e.FromName, &e.FromAddr, &e.Subject, &e.Folder, &e.DryRun); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// SummaryCost is what the model decisions made in [from, to) on the mail of the mailboxes
// the viewer sees cost, and how many asked a model.
func (s *Store) SummaryCost(ctx context.Context, v Viewer, from, to int64) (costUSD float64, calls int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(d.cost_usd), 0), COALESCE(SUM(COALESCE(d.model, '') <> ''), 0)
		 FROM decisions d JOIN messages m ON m.id = d.message_id
		 WHERE `+inVisible("m.account_id", v)+` AND d.created_at >= ? AND d.created_at < ?`, from, to).Scan(&costUSD, &calls)
	if err != nil {
		return 0, 0, fmt.Errorf("summary cost: %w", err)
	}
	return costUSD, calls, nil
}
