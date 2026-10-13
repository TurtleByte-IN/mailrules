package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// DraftWait is how long a draft job is tried, in seconds from when its rule acted: one not
// written by then is given up and recorded as a failed draft action. A reply drafted a day
// late is of little use, and the email may be answered by then.
const DraftWait = day

// DraftJob is one reply waiting to be drafted: a rule with Draft set acted on an email live.
type DraftJob struct {
	ID         int64
	TenantID   int64
	AccountID  int64
	MessageID  int64 // the Activity id
	DecisionID int64
	BatchID    int64
	RuleID     int64 // 0 once the rule was deleted
	RuleName   string
	// Instruction and ReadPrevious are the rule's Draft as it stood when it acted.
	Instruction  string
	ReadPrevious bool
	// Key is the job's own, the same on every attempt: the draft's Message-ID is made from
	// it, so a draft saved before a crash is found again instead of saved twice.
	Key       string
	CreatedAt int64 // when the rule acted
	Attempts  int   // tries that failed so far
}

// queueDraft writes, in tx, the draft job of a rule's run on one email once its last action
// is recorded done (a, just finished): when the decision was a rule's (not a sender rule's),
// in the live batch, and the rule drafts replies. Dry-run, cleanup, correction and undo runs
// queue none, nor does a run a failure cut short.
func queueDraft(ctx context.Context, tx *txn, a Action) error {
	if a.DecisionID == 0 || a.BatchID == 0 || a.Status != ActionDone || a.Kind == ActionDraft {
		return nil
	}
	var stage, ruleName, kind string
	var ruleID sql.NullInt64
	var draft sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT d.stage, d.rule_id, COALESCE(d.rule_name, r.name, ''), r.draft, b.kind
		 FROM decisions d JOIN batches b ON b.id = ? LEFT JOIN rules r ON r.id = d.rule_id
		 WHERE d.id = ?`, a.BatchID, a.DecisionID).Scan(&stage, &ruleID, &ruleName, &draft, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("queue draft: %w", err)
	}
	if kind != BatchLive || stage == string(rules.StageSender) || !ruleID.Valid || !draft.Valid {
		return nil
	}
	var d rules.Draft
	if err := json.Unmarshal([]byte(draft.String), &d); err != nil {
		return fmt.Errorf("queue draft: rule %d: %w", ruleID.Int64, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO draft_jobs (tenant_id, account_id, message_id, decision_id, batch_id, rule_id, rule_name, instruction, read_previous,
		                         draft_key, created_at, next_at)
		 SELECT tenant_id, CAST(? AS BIGINT), CAST(? AS BIGINT), CAST(? AS BIGINT), CAST(? AS BIGINT), CAST(? AS BIGINT), CAST(? AS TEXT),
		        CAST(? AS TEXT), CAST(? AS BOOLEAN), CAST(? AS TEXT), CAST(? AS BIGINT), CAST(? AS BIGINT)
		 FROM accounts WHERE id = ?`,
		a.AccountID, a.MessageID, a.DecisionID, a.BatchID, ruleID.Int64, ruleName, d.Instruction, d.ReadPrevious,
		rand.Text(), a.CreatedAt, a.CreatedAt, a.AccountID); err != nil {
		return fmt.Errorf("queue draft: %w", err)
	}
	return nil
}

const draftJobCols = `id, tenant_id, account_id, message_id, decision_id, batch_id, COALESCE(rule_id, 0), rule_name, instruction,
	read_previous, draft_key, created_at, attempts, next_at`

// WaitingDrafts lists up to limit draft jobs due by now, oldest first.
func (s *Store) WaitingDrafts(ctx context.Context, now int64, limit int) ([]DraftJob, error) {
	var waiting []outboxRow[DraftJob]
	err := s.eachTenant(ctx, tenantsDrafts, func(ctx context.Context, _ int64) error {
		rows, err := s.db.QueryContext(ctx,
			`SELECT `+draftJobCols+` FROM draft_jobs WHERE next_at <= ? ORDER BY next_at, id LIMIT ?`, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j DraftJob
			var next int64
			if err := rows.Scan(&j.ID, &j.TenantID, &j.AccountID, &j.MessageID, &j.DecisionID, &j.BatchID, &j.RuleID, &j.RuleName,
				&j.Instruction, &j.ReadPrevious, &j.Key, &j.CreatedAt, &j.Attempts, &next); err != nil {
				return err
			}
			waiting = append(waiting, outboxRow[DraftJob]{row: j, next: next, id: j.ID})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list waiting drafts: %w", err)
	}
	return firstDue(waiting, limit), nil
}

// DraftRetry records that a draft job could not be done now; it is tried again at next.
func (s *Store) DraftRetry(ctx context.Context, j DraftJob, next int64) error {
	ctx = WithTenant(ctx, j.TenantID)
	if _, err := s.db.ExecContext(ctx, `UPDATE draft_jobs SET attempts = attempts + 1, next_at = ? WHERE id = ?`, next, j.ID); err != nil {
		return fmt.Errorf("retry draft: %w", err)
	}
	return nil
}

// DraftDone deletes a draft job that is settled: its draft action records how.
func (s *Store) DraftDone(ctx context.Context, j DraftJob) error {
	ctx = WithTenant(ctx, j.TenantID)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM draft_jobs WHERE id = ?`, j.ID); err != nil {
		return fmt.Errorf("delete draft job: %w", err)
	}
	return nil
}

// DecisionDraft returns the latest draft action recorded for a decision; ok is false when
// there is none.
func (s *Store) DecisionDraft(ctx context.Context, tenantID, decisionID int64) (a Action, ok bool, err error) {
	ctx = WithTenant(ctx, tenantID)
	a, err = scanAction(s.db.QueryRowContext(ctx,
		`SELECT `+s.actionCols()+` FROM actions WHERE decision_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, decisionID, ActionDraft))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, fmt.Errorf("get draft action: %w", err)
	}
	return a, true, nil
}

// RunInEffect reports whether what a decision's rule did to its email still stands: at least
// one of its actions (a draft aside) is done and none was undone since.
func (s *Store) RunInEffect(ctx context.Context, tenantID, decisionID int64) (bool, error) {
	ctx = WithTenant(ctx, tenantID)
	var ok bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM actions WHERE decision_id = ? AND kind <> ? AND status = ?)
		    AND NOT EXISTS (SELECT 1 FROM actions WHERE decision_id = ? AND kind <> ? AND status = ?)`,
		decisionID, ActionDraft, ActionDone, decisionID, ActionDraft, ActionUndone).Scan(&ok); err != nil {
		return false, fmt.Errorf("check the rule's actions: %w", err)
	}
	return ok, nil
}
