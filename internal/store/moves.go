package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// MoveDays is how long a move the watcher found is kept: what the module was told, and what
// a question counts.
const MoveDays = 30

// Question statuses, as stored in move_questions.status.
const (
	QuestionOpen = "open"
	QuestionYes  = "yes"
	QuestionNo   = "no"
)

// ErrAnswered is what AskMove returns when the person already answered the question about
// that sender and destination. Nothing changed.
var ErrAnswered = errors.New("the person already answered the question about this sender and folder")

// movedKinds are the action kinds that file an email somewhere: the ones whose result the
// move watcher looks after.
const movedKinds = `('move', 'archive', 'trash', 'junk')`

// Filed is an email a done move-type action of MailRules left somewhere and that no later
// MailRules action moved on: where the move watcher expects it.
type Filed struct {
	ActionID  int64
	MessageID int64
	MsgID     string // the RFC 5322 Message-ID, without angle brackets
	From      string
	RuleID    int64  // the rule that decided; 0 = none, or deleted since
	RuleName  string // its name when it decided
	Before    Snapshot
	After     Snapshot
	FiledAt   int64
}

// FiledSince lists, oldest first, the account's emails a MailRules action filed at or after
// since, with an action id above afterID, that no later action moved on and that have no
// move recorded yet: at most limit. Dry-run, failed and undone actions file nothing. An
// email without a Message-ID or a sender cannot be followed and is left out. The rule is
// the decision's, or, for a fix made in MailRules, the rule it was fixed to.
func (s *Store) FiledSince(ctx context.Context, accountID, since, afterID int64, limit int) ([]Filed, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.message_id, m.message_id, m.from_addr, a.before, a.after, a.created_at,
		        COALESCE(d.rule_id, c.right_rule_id, 0), COALESCE(d.rule_name, r.name, '')
		 FROM actions a
		 JOIN messages m ON m.id = a.message_id
		 LEFT JOIN decisions d ON d.id = a.decision_id
		 LEFT JOIN corrections c ON a.decision_id IS NULL AND c.id = (SELECT MAX(id) FROM corrections WHERE message_id = a.message_id)
		 LEFT JOIN rules r ON r.id = c.right_rule_id
		 WHERE a.account_id = ? AND a.created_at >= ? AND a.id > ? AND a.status = 'done' AND a.kind IN `+movedKinds+`
		   AND a.after IS NOT NULL AND COALESCE(m.message_id, '') <> '' AND COALESCE(m.from_addr, '') <> ''
		   AND NOT EXISTS (SELECT 1 FROM actions x WHERE x.message_id = a.message_id AND x.id > a.id AND x.status = 'done' AND x.kind IN `+movedKinds+`)
		   AND NOT EXISTS (SELECT 1 FROM moves v WHERE v.action_id = a.id)
		 ORDER BY a.id LIMIT ?`, accountID, since, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list filed mail: %w", err)
	}
	defer rows.Close()
	var out []Filed
	for rows.Next() {
		var f Filed
		var before, after string
		if err := rows.Scan(&f.ActionID, &f.MessageID, &f.MsgID, &f.From, &before, &after, &f.FiledAt, &f.RuleID, &f.RuleName); err != nil {
			return nil, fmt.Errorf("list filed mail: %w", err)
		}
		if err := json.Unmarshal([]byte(before), &f.Before); err != nil {
			return nil, fmt.Errorf("decode action %d: %w", f.ActionID, err)
		}
		if err := json.Unmarshal([]byte(after), &f.After); err != nil {
			return nil, fmt.Errorf("decode action %d: %w", f.ActionID, err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list filed mail: %w", err)
	}
	return out, nil
}

// LeftByMailRules reports whether MailRules itself left an email at (folder, uidValidity,
// uid) of the account: it is where the executor last put a row other than exceptMessage
// (a cleanup that sorted the same email under a row of its own).
func (s *Store) LeftByMailRules(ctx context.Context, accountID int64, folder string, uidValidity, uid uint32, exceptMessage int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE account_id = ? AND cur_folder = ? AND cur_uidvalidity = ? AND cur_uid = ? AND id <> ?`,
		accountID, folder, uidValidity, uid, exceptMessage).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("look up where mailrules left mail: %w", err)
	}
	return n > 0, nil
}

// Move is an email the person moved out of the folder a MailRules action filed it in.
type Move struct {
	ID        int64
	TenantID  int64
	AccountID int64
	MessageID int64
	ActionID  int64 // the filing action
	From      string
	RuleID    int64
	RuleName  string
	Filed     string // where MailRules filed it
	Chosen    string // where it was found; "" = in no folder
	Inbox     bool   // Chosen is the mailbox's watched folder
	// Person: Chosen is the Inbox or an ordinary folder, so the person filed it there; not
	// deleted, archived or put there by MailRules itself. Only such moves reach the module.
	Person    bool
	FiledAt   int64
	CreatedAt int64 // when the watcher found it moved
	Attempts  int

	// Read back only.
	Mailbox string // the mailbox's address, as notices name it
	Count   int    // the person's moves of this sender to Chosen kept so far, this one included
}

// target is the destination a question about m is keyed by: "" for the Inbox.
func (m Move) target() string {
	if m.Inbox {
		return ""
	}
	return m.Chosen
}

// RecordMove writes a move the watcher found, once per filing action, provided the action
// is still in effect (nothing undid it meanwhile). A move that is not the person's is
// written as already handed over: the module never gets it. It returns the move's id, or
// 0 when nothing was written.
func (s *Store) RecordMove(ctx context.Context, m Move) (int64, error) {
	var handed any
	if !m.Person {
		handed = m.CreatedAt
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO moves (tenant_id, account_id, message_id, action_id, from_addr, rule_id, rule_name, filed_folder,
		   chosen_folder, inbox, person, filed_at, created_at, next_at, handed_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM actions WHERE id = ? AND status = 'done')`,
		m.TenantID, m.AccountID, m.MessageID, m.ActionID, m.From, null(m.RuleID), m.RuleName, m.Filed, m.Chosen, m.Inbox, m.Person,
		m.FiledAt, m.CreatedAt, m.CreatedAt, handed, m.ActionID)
	if err != nil {
		return 0, fmt.Errorf("record move: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, nil
	}
	id, _ := res.LastInsertId()
	return id, nil
}

const moveCols = `v.id, v.tenant_id, v.account_id, v.message_id, v.action_id, v.from_addr, COALESCE(v.rule_id, 0), v.rule_name,
	v.filed_folder, v.chosen_folder, v.inbox, v.person, v.filed_at, v.created_at, v.attempts, acc.preset, acc.username, acc.label,
	(SELECT COUNT(*) FROM moves o WHERE o.tenant_id = v.tenant_id AND o.from_addr = v.from_addr AND o.person = 1
	   AND o.inbox = v.inbox AND (v.inbox = 1 OR o.chosen_folder = v.chosen_folder) AND o.id <= v.id)`

func scanMove(row interface{ Scan(...any) error }) (Move, error) {
	var m Move
	var acct Account
	if err := row.Scan(&m.ID, &m.TenantID, &m.AccountID, &m.MessageID, &m.ActionID, &m.From, &m.RuleID, &m.RuleName,
		&m.Filed, &m.Chosen, &m.Inbox, &m.Person, &m.FiledAt, &m.CreatedAt, &m.Attempts, &acct.Preset, &acct.Username, &acct.Label,
		&m.Count); err != nil {
		return Move{}, err
	}
	m.Mailbox = acct.Label
	if own := acct.OwnAddresses(); len(own) > 0 {
		m.Mailbox = own[0]
	}
	return m, nil
}

// WaitingMoves lists the person's moves the module has not taken yet that are due at now,
// oldest first: at most limit.
func (s *Store) WaitingMoves(ctx context.Context, now int64, limit int) ([]Move, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+moveCols+` FROM moves v JOIN accounts acc ON acc.id = v.account_id
		 WHERE v.handed_at IS NULL AND v.next_at <= ? ORDER BY v.next_at, v.id LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list waiting moves: %w", err)
	}
	defer rows.Close()
	var out []Move
	for rows.Next() {
		m, err := scanMove(rows)
		if err != nil {
			return nil, fmt.Errorf("list waiting moves: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list waiting moves: %w", err)
	}
	return out, nil
}

// MoveHanded records that the module took a move.
func (s *Store) MoveHanded(ctx context.Context, id, now int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE moves SET handed_at = ? WHERE id = ?`, now, id); err != nil {
		return fmt.Errorf("move handed: %w", err)
	}
	return nil
}

// MoveRetry records that the module refused a move, to be handed over again at next.
func (s *Store) MoveRetry(ctx context.Context, id, next int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE moves SET attempts = attempts + 1, next_at = ? WHERE id = ?`, next, id); err != nil {
		return fmt.Errorf("move retry: %w", err)
	}
	return nil
}

// Question is one question Activity asks about the person's moves of one sender to one
// destination.
type Question struct {
	ID        int64
	TenantID  int64
	AccountID int64  // the mailbox of the latest move asked about
	Mailbox   string // its address
	From      string
	Inbox     bool   // the destination is the Inbox (the mailbox's watched folder)
	Folder    string // the destination's folder; the watched folder's name for the Inbox
	Emails    int    // how many of the sender's emails the person moved there
	Status    string // QuestionOpen | QuestionYes | QuestionNo
	AskedAt   int64
}

// AskMove puts, or keeps, the question about the move's sender and destination open,
// counting the person's moves kept for them: one question per sender and destination,
// whichever mailboxes the moves were in. It returns the question and whether it is new or
// changed. A question already answered is ErrAnswered; a move that is not kept, or not the
// person's, is ErrNotFound.
func (s *Store) AskMove(ctx context.Context, moveID, now int64) (Question, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Question{}, false, fmt.Errorf("ask about move: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	m, err := scanMove(tx.QueryRowContext(ctx, `SELECT `+moveCols+` FROM moves v JOIN accounts acc ON acc.id = v.account_id WHERE v.id = ? AND v.person = 1`, moveID))
	if errors.Is(err, sql.ErrNoRows) {
		return Question{}, false, ErrNotFound
	}
	if err != nil {
		return Question{}, false, fmt.Errorf("ask about move: %w", err)
	}
	var emails int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM moves WHERE tenant_id = ? AND from_addr = ? AND person = 1 AND inbox = ? AND (inbox = 1 OR chosen_folder = ?)`,
		m.TenantID, m.From, m.Inbox, m.Chosen).Scan(&emails); err != nil {
		return Question{}, false, fmt.Errorf("ask about move: %w", err)
	}
	var id, account int64
	var status string
	var had int
	err = tx.QueryRowContext(ctx, `SELECT id, status, emails, account_id FROM move_questions WHERE tenant_id = ? AND from_addr = ? AND target = ?`,
		m.TenantID, m.From, m.target()).Scan(&id, &status, &had, &account)
	changed := true
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx,
			`INSERT INTO move_questions (tenant_id, account_id, move_id, from_addr, target, emails, asked_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.TenantID, m.AccountID, m.ID, m.From, m.target(), emails, now)
		if err != nil {
			return Question{}, false, fmt.Errorf("ask about move: %w", err)
		}
		id, _ = res.LastInsertId()
	case err != nil:
		return Question{}, false, fmt.Errorf("ask about move: %w", err)
	case status != QuestionOpen:
		return Question{}, false, ErrAnswered
	default:
		changed = had != emails || account != m.AccountID
		if _, err := tx.ExecContext(ctx, `UPDATE move_questions SET account_id = ?, move_id = ?, emails = ? WHERE id = ?`,
			m.AccountID, m.ID, emails, id); err != nil {
			return Question{}, false, fmt.Errorf("ask about move: %w", err)
		}
	}
	q, err := scanQuestion(tx.QueryRowContext(ctx, `SELECT `+questionCols+` WHERE q.id = ?`, id))
	if err != nil {
		return Question{}, false, fmt.Errorf("ask about move: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Question{}, false, fmt.Errorf("ask about move: %w", err)
	}
	return q, changed, nil
}

const questionCols = `q.id, q.tenant_id, q.account_id, q.from_addr, q.target, acc.watch_folder, q.emails, q.status, q.asked_at,
	acc.preset, acc.username, acc.label
	FROM move_questions q JOIN accounts acc ON acc.id = q.account_id`

func scanQuestion(row interface{ Scan(...any) error }) (Question, error) {
	var q Question
	var target, watch string
	var acct Account
	if err := row.Scan(&q.ID, &q.TenantID, &q.AccountID, &q.From, &target, &watch, &q.Emails, &q.Status, &q.AskedAt,
		&acct.Preset, &acct.Username, &acct.Label); err != nil {
		return Question{}, err
	}
	q.Inbox, q.Folder = target == "", target
	if q.Inbox {
		q.Folder = watch
	}
	q.Mailbox = acct.Label
	if own := acct.OwnAddresses(); len(own) > 0 {
		q.Mailbox = own[0]
	}
	return q, nil
}

// OpenQuestions lists the open questions about mailboxes the viewer sees, newest first.
func (s *Store) OpenQuestions(ctx context.Context, v Viewer) ([]Question, error) {
	//nolint:gosec // G202: inVisible prints ids as numbers; values are bound arguments
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+questionCols+` WHERE q.tenant_id = ? AND q.status = ? AND `+inVisible("q.account_id", v)+` ORDER BY q.asked_at DESC, q.id DESC`,
		v.TenantID, QuestionOpen)
	if err != nil {
		return nil, fmt.Errorf("list questions: %w", err)
	}
	defer rows.Close()
	var out []Question
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, fmt.Errorf("list questions: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list questions: %w", err)
	}
	return out, nil
}

// AnswerQuestion records the viewer's answer to an open question about a mailbox they see,
// in one transaction. Yes stores the sender rule a correction's "Always do this for
// <sender>" stores (putUserSenderRule), made by the viewer: keep the sender's mail in the
// Inbox, or move it to the folder; it replaces whatever the tenant had for that address.
// No stores nothing more. Either way the question is not asked again. A question that is
// not open, or not one the viewer sees, is ErrNotFound.
func (s *Store) AnswerQuestion(ctx context.Context, v Viewer, id int64, yes bool, now int64) (Question, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Question{}, fmt.Errorf("answer question: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	//nolint:gosec // G202: inVisible prints ids as numbers; values are bound arguments
	q, err := scanQuestion(tx.QueryRowContext(ctx,
		`SELECT `+questionCols+` WHERE q.id = ? AND q.tenant_id = ? AND q.status = ? AND `+inVisible("q.account_id", v), id, v.TenantID, QuestionOpen))
	if errors.Is(err, sql.ErrNoRows) {
		return Question{}, ErrNotFound
	}
	if err != nil {
		return Question{}, fmt.Errorf("answer question: %w", err)
	}
	q.Status = QuestionNo
	if yes {
		q.Status = QuestionYes
		sr := rules.SenderRule{UserID: v.UserID, MatchType: rules.MatchAddress, Value: q.From, Verdict: rules.VerdictKeep}
		if !q.Inbox {
			sr.Verdict, sr.Folder = rules.VerdictMove, q.Folder
		}
		if err := putUserSenderRule(ctx, tx, v.TenantID, sr, now); err != nil {
			return Question{}, fmt.Errorf("answer question: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE move_questions SET status = ?, answered_at = ?, answered_by = ? WHERE id = ?`,
		q.Status, now, v.UserID, id); err != nil {
		return Question{}, fmt.Errorf("answer question: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Question{}, fmt.Errorf("answer question: %w", err)
	}
	return q, nil
}

// retainMoves deletes, in the tenant, the moves found more than MoveDays before now.
// Questions stay: an answered one is what keeps the same question from being asked again.
func (s *Store) retainMoves(ctx context.Context, tenantID, now int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM moves WHERE tenant_id = ? AND created_at < ?`, tenantID, now-MoveDays*day); err != nil {
		return fmt.Errorf("delete old moves: %w", err)
	}
	return nil
}
