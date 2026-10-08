package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	// ListUnsubscribe is set when the email carries a List-Unsubscribe header.
	ListUnsubscribe bool `json:"list_unsubscribe"`
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
	FromName      string // the From display name; may be empty
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
	// ActedAt is when MailRules last decided the message, or CreatedAt until a decision is
	// made. The feed is ordered by it.
	ActedAt int64

	// Where the executor last left the message; zero while it is still where it arrived.
	CurFolder      string
	CurUIDValidity uint32
	CurUID         uint32
}

// Location is where the message is on the server, as far as MailRules knows: where the
// executor last left it, or else where it arrived.
func (m Message) Location() mail.MsgRef {
	if m.CurUID != 0 {
		return mail.MsgRef{AccountID: m.AccountID, Folder: m.CurFolder, UIDValidity: m.CurUIDValidity, UID: m.CurUID}
	}
	return mail.MsgRef{AccountID: m.AccountID, Folder: m.Folder, UIDValidity: m.UIDValidity, UID: m.UID}
}

const messageCols = `m.id, m.account_id, m.folder, m.uidvalidity, m.uid, COALESCE(m.message_id, ''),
	COALESCE(m.from_addr, ''), COALESCE(m.from_domain, ''), COALESCE(m.to_addrs, '[]'), COALESCE(m.subject, ''),
	COALESCE(m.snippet, ''), COALESCE(m.received_at, 0), COALESCE(m.list_id, ''), m.has_attachment, COALESCE(m.size, 0),
	COALESCE(m.signals, '{}'), m.state, m.attempts, COALESCE(m.next_attempt_at, 0), m.created_at,
	COALESCE(m.cur_folder, ''), COALESCE(m.cur_uidvalidity, 0), COALESCE(m.cur_uid, 0), COALESCE(m.from_name, ''), m.acted_at`

func messageDest(m *Message, to, signals *string) []any {
	return []any{&m.ID, &m.AccountID, &m.Folder, &m.UIDValidity, &m.UID, &m.MessageID, &m.FromAddr, &m.FromDomain, to,
		&m.Subject, &m.Snippet, &m.ReceivedAt, &m.ListID, &m.HasAttachment, &m.Size, signals, &m.State, &m.Attempts,
		&m.NextAttemptAt, &m.CreatedAt, &m.CurFolder, &m.CurUIDValidity, &m.CurUID, &m.FromName, &m.ActedAt}
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
// either way. A known message that the executor itself put at ref (an undo back into the
// watched folder gets a new UID) is not new mail either: its existing row is returned.
func (s *Store) IngestMessage(ctx context.Context, ref mail.MsgRef, now int64) (m Message, fresh bool, err error) {
	m, err = scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages m
		 WHERE m.account_id = ? AND m.cur_folder = ? AND m.cur_uidvalidity = ? AND m.cur_uid = ?`,
		ref.AccountID, ref.Folder, ref.UIDValidity, ref.UID))
	if err == nil {
		return m, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, fmt.Errorf("ingest message: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (account_id, folder, uidvalidity, uid, created_at, acted_at) VALUES (?, ?, ?, ?, ?, ?)`,
		ref.AccountID, ref.Folder, ref.UIDValidity, ref.UID, now, now)
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

// MessageByMessageID returns the account's newest row, other than except, for the email
// with this Message-ID header, or ErrNotFound.
func (s *Store) MessageByMessageID(ctx context.Context, accountID int64, messageID string, except int64) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages m WHERE m.account_id = ? AND m.message_id = ? AND m.id <> ? ORDER BY m.id DESC LIMIT 1`,
		accountID, messageID, except))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("find message by message-id: %w", err)
	}
	return m, nil
}

// Rebind records that the email of row known is now at ref, where it was just ingested as
// row fresh, and removes fresh, so the email keeps one row. fresh must be a row MailRules
// has done nothing with yet: when it has a decision, an action or a correction, Rebind
// changes nothing and reports false.
func (s *Store) Rebind(ctx context.Context, fresh, known int64, ref mail.MsgRef) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("rebind message: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	res, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE id = ?1
		   AND NOT EXISTS (SELECT 1 FROM decisions WHERE message_id = ?1)
		   AND NOT EXISTS (SELECT 1 FROM actions WHERE message_id = ?1)
		   AND NOT EXISTS (SELECT 1 FROM corrections WHERE message_id = ?1)`, fresh)
	if err != nil {
		return false, fmt.Errorf("rebind message: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	if err := setLocation(ctx, tx, known, ref); err != nil {
		return false, fmt.Errorf("rebind message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("rebind message: %w", err)
	}
	return true, nil
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
		`UPDATE messages SET message_id = ?, from_addr = ?, from_name = ?, from_domain = ?, to_addrs = ?, subject = ?, snippet = ?,
		                     received_at = ?, list_id = ?, has_attachment = ?, size = ?, signals = ?
		 WHERE id = ?`,
		null(m.MessageID), m.FromAddr, null(m.FromName), m.FromDomain, string(to), m.Subject, m.Snippet,
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
	RuleName    string // the rule's name: as it is now, or as it was when decided if the rule is gone
	RuleVersion int
	Confidence  float64
	Reason      string
	Model       string
	TokensIn    int
	TokensOut   int
	CostUSD     float64
	LatencyMS   int64
	CreatedAt   int64
	// Probabilities is what the decision model gave each candidate rule, by rule id, with
	// 0 for "none of these". nil when the model gives no such spread, or none was asked.
	Probabilities map[int64]float64
}

// The columns tolerate a missing row, so a LEFT JOIN scans into a zero Decision.
const decisionCols = `COALESCE(d.id, 0), COALESCE(d.message_id, 0), COALESCE(d.stage, ''), COALESCE(d.rule_id, 0),
	COALESCE((SELECT name FROM rules WHERE id = d.rule_id), d.rule_name, ''),
	COALESCE(d.rule_version, 0), COALESCE(d.confidence, 0), COALESCE(d.reason, ''), COALESCE(d.model, ''),
	COALESCE(d.tokens_in, 0), COALESCE(d.tokens_out, 0), COALESCE(d.cost_usd, 0), COALESCE(d.latency_ms, 0), COALESCE(d.created_at, 0),
	COALESCE(d.probabilities, '')`

// probabilities scans decisions.probabilities, a JSON object keyed by rule id.
type probabilities map[int64]float64

func (p *probabilities) Scan(v any) error {
	if s, _ := v.(string); s != "" {
		return json.Unmarshal([]byte(s), p)
	}
	return nil
}

func decisionDest(d *Decision) []any {
	return []any{&d.ID, &d.MessageID, &d.Stage, &d.RuleID, &d.RuleName, &d.RuleVersion, &d.Confidence, &d.Reason, &d.Model,
		&d.TokensIn, &d.TokensOut, &d.CostUSD, &d.LatencyMS, &d.CreatedAt, (*probabilities)(&d.Probabilities)}
}

// AddDecision records a decision and moves its message to state in one transaction, and
// returns the decision's id. The message's ActedAt becomes the decision's time.
func (s *Store) AddDecision(ctx context.Context, d Decision, state string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add decision: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var probs any
	if len(d.Probabilities) > 0 {
		b, err := json.Marshal(d.Probabilities)
		if err != nil {
			return 0, fmt.Errorf("encode probabilities: %w", err)
		}
		probs = string(b)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO decisions (message_id, stage, rule_id, rule_name, rule_version, confidence, reason, model,
		                        tokens_in, tokens_out, cost_usd, latency_ms, created_at, probabilities)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.MessageID, d.Stage, null(d.RuleID), null(d.RuleName), null(d.RuleVersion), d.Confidence, d.Reason, null(d.Model),
		d.TokensIn, d.TokensOut, d.CostUSD, d.LatencyMS, d.CreatedAt, probs)
	if err != nil {
		return 0, fmt.Errorf("add decision: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET state = ?, next_attempt_at = NULL, acted_at = ? WHERE id = ?`, state, d.CreatedAt, d.MessageID); err != nil {
		return 0, fmt.Errorf("add decision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("add decision: %w", err)
	}
	return id, nil
}

// ActivityFilter narrows Activity. Zero fields do not filter.
type ActivityFilter struct {
	ID        int64 // only this message
	AccountID int64
	RuleID    int64
	Stage     string
	State     string // a message state; StateReview lists Needs review
	Action    string // an action kind: only messages with such an action
	Outcome   string // one of Outcomes: where the message ended up, as the stats count it
	After     FeedCursor
	Limit     int // 0 = 50
}

// FeedCursor is where a page of Activity ends: the ActedAt and ID of its last row. The
// next page starts after it in the feed's order. The zero value is the first page.
type FeedCursor struct {
	ActedAt int64
	ID      int64
}

// Cursor is the position of this row in the feed, for the page after it.
func (r ActivityRow) Cursor() FeedCursor {
	return FeedCursor{ActedAt: r.Message.ActedAt, ID: r.Message.ID}
}

// ActivityRow is a message with its latest decision and every action taken on it, oldest
// first; Decision is nil until one is made.
type ActivityRow struct {
	Message  Message
	Decision *Decision
	Actions  []Action
}

// Outcomes are the four places a processed message ends up: the groups of the Overview bar
// (Totals.Went*) and the values of ActivityFilter.Outcome.
var Outcomes = []string{"sorted", "trashed", "inbox", "review"}

// outcomeSQL is the condition for each of Outcomes on one message, given the SQL for its
// state and its id. The stats and the feed's filter are both built from it, so a group's
// count and the list behind it cannot disagree. An action is in effect when it is done, or
// recorded in dry-run; one that was undone is not, so a message whose actions were all
// undone is back in the inbox.
func outcomeSQL(state, id string) map[string]string {
	inEffect := func(more string) string {
		return `EXISTS (SELECT 1 FROM actions x WHERE x.message_id = ` + id + ` AND x.status IN ('done', 'dry_run')` + more + `)`
	}
	acted, trash := state+` = 'acted' AND `+inEffect(""), inEffect(` AND x.kind = 'trash'`)
	return map[string]string{
		"review":  state + ` = 'review'`,
		"trashed": acted + ` AND ` + trash,
		"sorted":  acted + ` AND NOT ` + trash,
		"inbox":   state + ` <> 'review' AND NOT (` + acted + `)`,
	}
}

// Activity lists the messages of the mailboxes the viewer sees, each with its latest
// decision, by when MailRules acted on them (ActedAt), newest first; messages acted on in
// the same second come newest row first.
func (s *Store) Activity(ctx context.Context, v Viewer, f ActivityFilter) ([]ActivityRow, error) {
	return s.activity(ctx, &v, f)
}

// activity is Activity; a nil viewer lists every tenant's mail, for internal work.
func (s *Store) activity(ctx context.Context, v *Viewer, f ActivityFilter) ([]ActivityRow, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	// Only the filters in force go into the query, so the planner can walk messages_acted
	// from the cursor, or look one message up by its id.
	where, args := []string{"1"}, []any{}
	add := func(cond string, arg ...any) { where, args = append(where, cond), append(args, arg...) }
	if v != nil {
		add(inVisible("m.account_id", *v))
	}
	if f.ID != 0 {
		add(`m.id = ?`, f.ID)
	}
	if f.AccountID != 0 {
		add(`m.account_id = ?`, f.AccountID)
	}
	if f.RuleID != 0 {
		add(`d.rule_id = ?`, f.RuleID)
	}
	if f.Stage != "" {
		add(`d.stage = ?`, f.Stage)
	}
	if f.State != "" {
		add(`m.state = ?`, f.State)
	}
	if f.After != (FeedCursor{}) {
		add(`(m.acted_at, m.id) < (?, ?)`, f.After.ActedAt, f.After.ID)
	}
	if f.Action != "" {
		add(`EXISTS (SELECT 1 FROM actions a WHERE a.message_id = m.id AND a.kind = ?)`, f.Action)
	}
	if f.Outcome != "" {
		outcome, ok := outcomeSQL("m.state", "m.id")[f.Outcome]
		if !ok {
			return nil, fmt.Errorf("list activity: unknown outcome %q", f.Outcome)
		}
		add(outcome)
	}
	//nolint:gosec // G202: every condition is built in this package; values are bound arguments or ids printed as numbers
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageCols+`, `+decisionCols+`
		 FROM messages m LEFT JOIN decisions d ON d.id = (SELECT MAX(id) FROM decisions WHERE message_id = m.id)
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY m.acted_at DESC, m.id DESC LIMIT ?`,
		append(args, f.Limit)...)
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
	// ponytail: one query per row for its actions; pages are at most 100 rows. Join if it shows up in a profile.
	for i := range out {
		if out[i].Actions, err = s.MessageActions(ctx, out[i].Message.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ActivityFor returns the activity row of one message the viewer sees, or ErrNotFound.
func (s *Store) ActivityFor(ctx context.Context, v Viewer, messageID int64) (ActivityRow, error) {
	return s.activityFor(ctx, &v, messageID)
}

// MessageActivity returns one message's activity row, whoever's it is, or ErrNotFound: for
// internal work on a known message (the executor, the event publishers).
func (s *Store) MessageActivity(ctx context.Context, messageID int64) (ActivityRow, error) {
	return s.activityFor(ctx, nil, messageID)
}

func (s *Store) activityFor(ctx context.Context, v *Viewer, messageID int64) (ActivityRow, error) {
	rows, err := s.activity(ctx, v, ActivityFilter{ID: messageID, Limit: 1})
	if err != nil {
		return ActivityRow{}, err
	}
	if len(rows) == 0 {
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

// RecentSenderDecisions returns the decisions of the newest n decided emails from one
// address across the mailboxes the viewer sees, newest first. An email decided more than
// once (a retry, a cleanup run) counts once, by its latest decision. A decision whose
// actions were recorded in dry-run is left out, so a dry-run period teaches the learner
// nothing, then or once MailRules is live.
func (s *Store) RecentSenderDecisions(ctx context.Context, v Viewer, address string, n int) ([]SenderDecision, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.stage, COALESCE(d.rule_id, 0), COALESCE(d.confidence, 0),
		        EXISTS (SELECT 1 FROM corrections c WHERE c.message_id = m.id)
		 FROM decisions d JOIN messages m ON m.id = d.message_id
		 WHERE `+inVisible("m.account_id", v)+` AND m.from_addr = ? AND d.id = (SELECT MAX(id) FROM decisions WHERE message_id = m.id)
		   AND NOT EXISTS (SELECT 1 FROM actions x WHERE x.message_id = m.id AND x.decision_id = d.id AND x.status = 'dry_run')
		 ORDER BY d.id DESC LIMIT ?`, address, n)
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

// AddLearnedSenderRule routes an address to one of the tenant's rules unless the address
// already has a sender rule there, which is never overwritten. userID is who it is learnt
// for. It reports whether a rule was created.
func (s *Store) AddLearnedSenderRule(ctx context.Context, tenantID, userID int64, address string, ruleID, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sender_rules (tenant_id, user_id, match_type, value, rule_id, verdict, source, hits, created_at)
		 SELECT ?, ?, ?, ?, id, ?, 'learned', 0, ? FROM rules WHERE id = ? AND tenant_id = ?
		 ON CONFLICT (tenant_id, match_type, value) DO NOTHING`,
		tenantID, userID, rules.MatchAddress, address, rules.VerdictRoute, now, ruleID, tenantID)
	if err != nil {
		return false, fmt.Errorf("add learned sender rule: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// What a correction was: a fix made from the feed, or an answer given in Needs review.
const (
	CorrectionFix    = "correction"
	CorrectionReview = "review"
)

// Correction is one row of corrections: the user said a message belongs to another rule.
type Correction struct {
	ID          int64
	MessageID   int64
	WrongRuleID int64  // 0 = no rule had been applied
	RightRuleID int64  // 0 = keep in the inbox
	RightRule   string // that rule's name, when read back; empty for keep, or once the rule is deleted
	Kind        string // CorrectionFix | CorrectionReview; empty is stored as CorrectionFix
	Example     string // JSON message.Summary, the few-shot example
	CreatedAt   int64
}

// AddCorrection records a correction in one transaction: it inserts the row, forgets the
// tenant's learned sender rule for that message's sender and marks the message acted. With
// always (rules.MatchAddress or rules.MatchDomain; "" = neither) it also stores a user
// sender rule of the tenant, made by userID, that sends the address, or its whole domain,
// to the right rule (or keeps it in the inbox), replacing whatever that address or domain
// had. The caller checks that the message and the right rule are the tenant's.
func (s *Store) AddCorrection(ctx context.Context, tenantID, userID int64, c Correction, always string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var sender, domain string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(from_addr, ''), COALESCE(from_domain, '') FROM messages WHERE id = ?`, c.MessageID).Scan(&sender, &domain)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO corrections (message_id, wrong_rule_id, right_rule_id, example, kind, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		c.MessageID, null(c.WrongRuleID), null(c.RightRuleID), c.Example, cmp.Or(c.Kind, CorrectionFix), c.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sender_rules WHERE tenant_id = ? AND match_type = ? AND value = ? AND (source = 'learned' OR ?)`,
		tenantID, rules.MatchAddress, sender, always == rules.MatchAddress); err != nil {
		return 0, fmt.Errorf("add correction: %w", err)
	}
	value := sender
	if always == rules.MatchDomain {
		value = domain
	}
	if always != "" && value != "" {
		verdict := rules.VerdictRoute
		if c.RightRuleID == 0 {
			verdict = rules.VerdictKeep
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sender_rules (tenant_id, user_id, match_type, value, rule_id, verdict, source, hits, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'user', 0, ?)
			 ON CONFLICT (tenant_id, match_type, value) DO UPDATE SET rule_id = excluded.rule_id, verdict = excluded.verdict, source = 'user', hits = 0`,
			tenantID, userID, always, value, null(c.RightRuleID), verdict, c.CreatedAt); err != nil {
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

// Corrections lists the newest corrections made to mail in the mailboxes the viewer sees,
// newest first, with the example each stored: the pool few-shot retrieval ranks.
func (s *Store) Corrections(ctx context.Context, v Viewer, limit int) ([]Correction, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.message_id, COALESCE(c.wrong_rule_id, 0), COALESCE(c.right_rule_id, 0), c.example, c.created_at
		 FROM corrections c JOIN messages m ON m.id = c.message_id
		 WHERE `+inVisible("m.account_id", v)+` ORDER BY c.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list corrections: %w", err)
	}
	defer rows.Close()
	var out []Correction
	for rows.Next() {
		var c Correction
		if err := rows.Scan(&c.ID, &c.MessageID, &c.WrongRuleID, &c.RightRuleID, &c.Example, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("list corrections: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list corrections: %w", err)
	}
	return out, nil
}

// MessageDecisions lists every decision made for a message, oldest first. A message has
// more than one when the retry job ran it again.
func (s *Store) MessageDecisions(ctx context.Context, messageID int64) ([]Decision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+decisionCols+` FROM decisions d WHERE d.message_id = ? ORDER BY d.id`, messageID)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(decisionDest(&d)...); err != nil {
			return nil, fmt.Errorf("list decisions: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	return out, nil
}

// MessageCorrections lists the corrections the user made to a message, oldest first.
func (s *Store) MessageCorrections(ctx context.Context, messageID int64) ([]Correction, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, message_id, COALESCE(wrong_rule_id, 0), COALESCE(right_rule_id, 0),
		        COALESCE((SELECT name FROM rules WHERE id = right_rule_id), ''), kind, created_at
		 FROM corrections WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return nil, fmt.Errorf("list corrections: %w", err)
	}
	defer rows.Close()
	var out []Correction
	for rows.Next() {
		var c Correction
		if err := rows.Scan(&c.ID, &c.MessageID, &c.WrongRuleID, &c.RightRuleID, &c.RightRule, &c.Kind, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("list corrections: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list corrections: %w", err)
	}
	return out, nil
}

// CountMessages counts the messages in a state in the mailboxes the viewer sees, e.g.
// StateReview for the Needs review badge.
func (s *Store) CountMessages(ctx context.Context, v Viewer, state string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages m WHERE m.state = ? AND `+inVisible("m.account_id", v), state).Scan(&n); err != nil {
		return 0, fmt.Errorf("count messages: %w", err)
	}
	return n, nil
}

// VisibleMessage returns one message of a mailbox the viewer sees, or ErrNotFound.
func (s *Store) VisibleMessage(ctx context.Context, v Viewer, id int64) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages m WHERE m.id = ? AND `+inVisible("m.account_id", v), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("get message: %w", err)
	}
	return m, nil
}
