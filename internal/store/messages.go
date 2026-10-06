package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// Message states, as stored in messages.state.
const (
	StateNew     = "new"     // ingested, not yet decided
	StateDecided = "decided" // a decision is recorded, its actions are not finished
	StateActed   = "acted"   // the decision's actions ran (or were recorded as dry-run)
	StateReview  = "review"  // waiting in Needs review
	StateSkipped = "skipped" // nothing to do: no rule matched, or the message was gone
	StateError   = "error"   // failed; the retry job runs it again at NextAttemptAt
)

// Signals is the JSON in messages.signals.
type Signals struct {
	Bulk          bool   `json:"bulk"`
	Noreply       bool   `json:"noreply"`
	Contact       bool   `json:"is_contact"`
	RepliedBefore bool   `json:"replied_before"`
	DMARC         string `json:"dmarc"`
}

// Message is one row of messages: what is kept of an email. The body never is.
type Message struct {
	ID            int64
	AccountID     int64
	Folder        string // where it arrived; with UIDValidity and UID, the dedupe key
	UIDValidity   uint32
	UID           uint32
	MessageID     string // RFC 5322 Message-ID, without angle brackets
	FromAddr      string
	FromDomain    string
	ToAddrs       []string
	Subject       string
	Snippet       string
	ReceivedAt    int64
	ListID        string
	HasAttachment bool
	Size          int64
	Signals       Signals
	State         string
	Attempts      int   // retries made after a failure
	NextAttemptAt int64 // 0 = not waiting for the retry job
	CreatedAt     int64
}

// Location is where the message is on the server.
func (m Message) Location() mail.MsgRef {
	return mail.MsgRef{AccountID: m.AccountID, Folder: m.Folder, UIDValidity: m.UIDValidity, UID: m.UID}
}

const messageCols = `m.id, m.account_id, m.folder, m.uidvalidity, m.uid, COALESCE(m.message_id, ''),
	COALESCE(m.from_addr, ''), COALESCE(m.from_domain, ''), COALESCE(m.to_addrs, '[]'), COALESCE(m.subject, ''),
	COALESCE(m.snippet, ''), COALESCE(m.received_at, 0), COALESCE(m.list_id, ''), m.has_attachment, COALESCE(m.size, 0),
	COALESCE(m.signals, '{}'), m.state, m.attempts, COALESCE(m.next_attempt_at, 0), m.created_at`

func messageDest(m *Message, to, signals *string) []any {
	return []any{&m.ID, &m.AccountID, &m.Folder, &m.UIDValidity, &m.UID, &m.MessageID, &m.FromAddr, &m.FromDomain, to,
		&m.Subject, &m.Snippet, &m.ReceivedAt, &m.ListID, &m.HasAttachment, &m.Size, signals, &m.State, &m.Attempts,
		&m.NextAttemptAt, &m.CreatedAt}
}

func (m *Message) decode(to, signals string) error {
	if err := errors.Join(json.Unmarshal([]byte(to), &m.ToAddrs), json.Unmarshal([]byte(signals), &m.Signals)); err != nil {
		return fmt.Errorf("decode message %d: %w", m.ID, err)
	}
	return nil
}

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var to, signals string
	if err := row.Scan(messageDest(&m, &to, &signals)...); err != nil {
		return Message{}, err
	}
	return m, m.decode(to, signals)
}

// IngestMessage records that a message arrived, once. fresh is false when the row was
// already there (the watcher repeats a message after a crash); the stored row is returned
// either way.
func (s *Store) IngestMessage(ctx context.Context, ref mail.MsgRef, now int64) (m Message, fresh bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (account_id, folder, uidvalidity, uid, created_at) VALUES (?, ?, ?, ?, ?)`,
		ref.AccountID, ref.Folder, ref.UIDValidity, ref.UID, now)
	if err != nil {
		return Message{}, false, fmt.Errorf("ingest message: %w", err)
	}
	n, _ := res.RowsAffected()
	m, err = scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages m WHERE m.account_id = ? AND m.folder = ? AND m.uidvalidity = ? AND m.uid = ?`,
		ref.AccountID, ref.Folder, ref.UIDValidity, ref.UID))
	if err != nil {
		return Message{}, false, fmt.Errorf("ingest message: %w", err)
	}
	return m, n == 1, nil
}

// Message returns one message, or ErrNotFound.
func (s *Store) Message(ctx context.Context, id int64) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages m WHERE m.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("get message: %w", err)
	}
	return m, nil
}

// SaveMessageSummary stores the parsed header fields, snippet and signals of a message.
func (s *Store) SaveMessageSummary(ctx context.Context, m Message) error {
	to, err := json.Marshal(append([]string{}, m.ToAddrs...))
	if err != nil {
		return fmt.Errorf("encode recipients: %w", err)
	}
	signals, err := json.Marshal(m.Signals)
	if err != nil {
		return fmt.Errorf("encode signals: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE messages SET message_id = ?, from_addr = ?, from_domain = ?, to_addrs = ?, subject = ?, snippet = ?,
		                     received_at = ?, list_id = ?, has_attachment = ?, size = ?, signals = ?
		 WHERE id = ?`,
		null(m.MessageID), m.FromAddr, m.FromDomain, string(to), m.Subject, m.Snippet,
		m.ReceivedAt, null(m.ListID), m.HasAttachment, m.Size, string(signals), m.ID); err != nil {
		return fmt.Errorf("save message summary: %w", err)
	}
	return nil
}

// SetMessageState moves a message to a state. nextAttemptAt is when the retry job should
// run it again; 0 means it is not waiting.
func (s *Store) SetMessageState(ctx context.Context, id int64, state string, attempts int, nextAttemptAt int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE messages SET state = ?, attempts = ?, next_attempt_at = ? WHERE id = ?`,
		state, attempts, null(nextAttemptAt), id); err != nil {
		return fmt.Errorf("set message state: %w", err)
	}
	return nil
}

// DueMessages lists an account's failed messages whose retry time has come, oldest first.
func (s *Store) DueMessages(ctx context.Context, accountID, now int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageCols+` FROM messages m
		 WHERE m.account_id = ? AND m.state = 'error' AND m.next_attempt_at <= ? ORDER BY m.id`, accountID, now)
	if err != nil {
		return nil, fmt.Errorf("list due messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("list due messages: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due messages: %w", err)
	}
	return out, nil
}

// AdvanceFolder raises a watched folder's last processed UID to ref's. It never lowers it,
// except when the folder's UIDVALIDITY changed, where the old position means nothing.
func (s *Store) AdvanceFolder(ctx context.Context, ref mail.MsgRef) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO folders (account_id, name, uidvalidity, last_uid) VALUES (?, ?, ?, ?)
		 ON CONFLICT (account_id, name) DO UPDATE SET
		   last_uid = CASE WHEN folders.uidvalidity IS excluded.uidvalidity
		                   THEN MAX(folders.last_uid, excluded.last_uid) ELSE excluded.last_uid END,
		   uidvalidity = excluded.uidvalidity`,
		ref.AccountID, ref.Folder, ref.UIDValidity, ref.UID); err != nil {
		return fmt.Errorf("advance folder: %w", err)
	}
	return nil
}

// Decision is one row of decisions: what was decided for a message, and what it cost.
type Decision struct {
	ID          int64
	MessageID   int64
	Stage       string // sender | condition | decider | fallback | none
	RuleID      int64  // 0 = no rule; in Needs review, the rule the model suggested
	RuleVersion int
	Confidence  float64
	Reason      string
	Model       string
	TokensIn    int
	TokensOut   int
	CostUSD     float64
	LatencyMS   int64
	CreatedAt   int64
}

// The columns tolerate a missing row, so a LEFT JOIN scans into a zero Decision.
const decisionCols = `COALESCE(d.id, 0), COALESCE(d.message_id, 0), COALESCE(d.stage, ''), COALESCE(d.rule_id, 0),
	COALESCE(d.rule_version, 0), COALESCE(d.confidence, 0), COALESCE(d.reason, ''), COALESCE(d.model, ''),
	COALESCE(d.tokens_in, 0), COALESCE(d.tokens_out, 0), COALESCE(d.cost_usd, 0), COALESCE(d.latency_ms, 0), COALESCE(d.created_at, 0)`

func decisionDest(d *Decision) []any {
	return []any{&d.ID, &d.MessageID, &d.Stage, &d.RuleID, &d.RuleVersion, &d.Confidence, &d.Reason, &d.Model,
		&d.TokensIn, &d.TokensOut, &d.CostUSD, &d.LatencyMS, &d.CreatedAt}
}

// AddDecision records a decision and moves its message to state in one transaction, and
// returns the decision's id.
func (s *Store) AddDecision(ctx context.Context, d Decision, state string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add decision: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	res, err := tx.ExecContext(ctx,
		`INSERT INTO decisions (message_id, stage, rule_id, rule_version, confidence, reason, model,
		                        tokens_in, tokens_out, cost_usd, latency_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.MessageID, d.Stage, null(d.RuleID), null(d.RuleVersion), d.Confidence, d.Reason, null(d.Model),
		d.TokensIn, d.TokensOut, d.CostUSD, d.LatencyMS, d.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("add decision: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET state = ?, next_attempt_at = NULL WHERE id = ?`, state, d.MessageID); err != nil {
		return 0, fmt.Errorf("add decision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("add decision: %w", err)
	}
	return id, nil
}

// ActivityFilter narrows Activity. Zero fields do not filter.
type ActivityFilter struct {
	AccountID int64
	RuleID    int64
	Stage     string
	State     string // a message state; StateReview lists Needs review
	Before    int64  // cursor: only messages with a smaller id
	Limit     int    // 0 = 50
}

// ActivityRow is a message with its latest decision; Decision is nil until one is made.
type ActivityRow struct {
	Message  Message
	Decision *Decision
}

// Activity lists messages, newest first, each with its latest decision.
func (s *Store) Activity(ctx context.Context, f ActivityFilter) ([]ActivityRow, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageCols+`, `+decisionCols+`
		 FROM messages m LEFT JOIN decisions d ON d.id = (SELECT MAX(id) FROM decisions WHERE message_id = m.id)
		 WHERE (?1 = 0 OR m.account_id = ?1) AND (?2 = 0 OR d.rule_id = ?2) AND (?3 = '' OR d.stage = ?3)
		   AND (?4 = '' OR m.state = ?4) AND (?5 = 0 OR m.id < ?5)
		 ORDER BY m.id DESC LIMIT ?6`,
		f.AccountID, f.RuleID, f.Stage, f.State, f.Before, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	defer rows.Close()
	var out []ActivityRow
	for rows.Next() {
		var row ActivityRow
		var d Decision
		var to, signals string
		if err := rows.Scan(append(messageDest(&row.Message, &to, &signals), decisionDest(&d)...)...); err != nil {
			return nil, fmt.Errorf("list activity: %w", err)
		}
		if err := row.Message.decode(to, signals); err != nil {
			return nil, err
		}
		if d.ID != 0 {
			row.Decision = &d
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	return out, nil
}

// ActivityFor returns one message's activity row, or ErrNotFound.
func (s *Store) ActivityFor(ctx context.Context, messageID int64) (ActivityRow, error) {
	rows, err := s.Activity(ctx, ActivityFilter{Before: messageID + 1, Limit: 1})
	if err != nil {
		return ActivityRow{}, err
	}
	if len(rows) == 0 || rows[0].Message.ID != messageID {
		return ActivityRow{}, ErrNotFound
	}
	return rows[0], nil
}

// SenderDecision is one past decision for a sender, as the learner reads it.
type SenderDecision struct {
	Stage      string
	RuleID     int64
	Confidence float64
	Corrected  bool // the user corrected that message afterwards
}

// RecentSenderDecisions returns the newest n decisions for mail from one address across
// the user's accounts, newest first.
func (s *Store) RecentSenderDecisions(ctx context.Context, userID int64, address string, n int) ([]SenderDecision, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.stage, COALESCE(d.rule_id, 0), COALESCE(d.confidence, 0),
		        EXISTS (SELECT 1 FROM corrections c WHERE c.message_id = m.id)
		 FROM decisions d JOIN messages m ON m.id = d.message_id JOIN accounts a ON a.id = m.account_id
		 WHERE a.user_id = ? AND m.from_addr = ?
		 ORDER BY d.id DESC LIMIT ?`, userID, address, n)
	if err != nil {
		return nil, fmt.Errorf("list sender decisions: %w", err)
	}
	defer rows.Close()
	var out []SenderDecision
	for rows.Next() {
		var d SenderDecision
		if err := rows.Scan(&d.Stage, &d.RuleID, &d.Confidence, &d.Corrected); err != nil {
			return nil, fmt.Errorf("list sender decisions: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sender decisions: %w", err)
	}
	return out, nil
}

// AddLearnedSenderRule routes an address to a rule unless the address already has a sender
// rule, which is never overwritten. It reports whether a rule was created.
func (s *Store) AddLearnedSenderRule(ctx context.Context, userID int64, address string, ruleID, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sender_rules (user_id, match_type, value, rule_id, verdict, source, hits, created_at)
		 VALUES (?, ?, ?, ?, ?, 'learned', 0, ?) ON CONFLICT (user_id, match_type, value) DO NOTHING`,
		userID, rules.MatchAddress, address, ruleID, rules.VerdictRoute, now)
	if err != nil {
		return false, fmt.Errorf("add learned sender rule: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Correction is one row of corrections: the user said a message belongs to another rule.
type Correction struct {
	ID          int64
	MessageID   int64
	WrongRuleID int64  // 0 = no rule had been applied
	RightRuleID int64  // 0 = keep in the inbox
	Example     string // JSON message.Summary, the few-shot example
	CreatedAt   int64
}

// AddCorrection records a correction in one transaction: it inserts the row, forgets the
// learned sender rule for that message's sender and marks the message acted. With
// alwaysForSender it instead stores a user sender rule that sends the address to the right
// rule (or keeps it in the inbox), replacing whatever the address had.
func (s *Store) AddCorrection(ctx context.Context, userID int64, c Correction, alwaysForSender bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var sender string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(from_addr, '') FROM messages WHERE id = ?`, c.MessageID).Scan(&sender)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO corrections (message_id, wrong_rule_id, right_rule_id, example, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.MessageID, null(c.WrongRuleID), null(c.RightRuleID), c.Example, c.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sender_rules WHERE user_id = ? AND match_type = ? AND value = ? AND (source = 'learned' OR ?)`,
		userID, rules.MatchAddress, sender, alwaysForSender); err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	if alwaysForSender && sender != "" {
		verdict := rules.VerdictRoute
		if c.RightRuleID == 0 {
			verdict = rules.VerdictKeep
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sender_rules (user_id, match_type, value, rule_id, verdict, source, hits, created_at)
			 VALUES (?, ?, ?, ?, ?, 'user', 0, ?)`,
			userID, rules.MatchAddress, sender, null(c.RightRuleID), verdict, c.CreatedAt); err != nil {
			return 0, fmt.Errorf("add correction: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET state = ?, next_attempt_at = NULL WHERE id = ?`, StateActed, c.MessageID); err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	return id, nil
}

// Action statuses, as stored in actions.status.
const (
	ActionDone   = "done"
	ActionDryRun = "dry_run"
	ActionFailed = "failed"
	ActionUndone = "undone"
)

// Snapshot is the JSON in actions.before and actions.after: where a message was and
// which flags it had.
type Snapshot struct {
	Folder      string   `json:"folder"`
	UIDValidity uint32   `json:"uidvalidity"`
	UID         uint32   `json:"uid"`
	Flags       []string `json:"flags"`
}

// Action is one row of actions: one step taken (or, in dry-run, not taken) on a message.
type Action struct {
	ID         int64
	DecisionID int64 // 0 = none
	BatchID    int64 // 0 = none
	AccountID  int64
	Kind       string // move | trash | archive | junk | flag | unflag | read | unread | keep | review
	Folder     string // params.folder: the destination of a move
	Before     Snapshot
	After      *Snapshot // nil until the action is done
	Status     string
	Error      string
	CreatedAt  int64
	UndoneAt   int64
}
