package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// Action statuses, as stored in actions.status.
const (
	ActionDone   = "done"
	ActionDryRun = "dry_run"
	ActionFailed = "failed"
	ActionUndone = "undone"
)

// Batch kinds and statuses, as stored in batches.
const (
	BatchLive       = "live"
	BatchCorrection = "correction"
	BatchUndo       = "undo"

	BatchRunning = "running"
	BatchDone    = "done"
	BatchFailed  = "failed"
	BatchUndone  = "undone"
)

// Snapshot is the JSON in actions.before and actions.after: where a message was and
// which flags it had.
type Snapshot struct {
	Folder      string   `json:"folder"`
	UIDValidity uint32   `json:"uidvalidity"`
	UID         uint32   `json:"uid"`
	Flags       []string `json:"flags"`
}

// Ref is the snapshot's location in an account.
func (s Snapshot) Ref(accountID int64) mail.MsgRef {
	return mail.MsgRef{AccountID: accountID, Folder: s.Folder, UIDValidity: s.UIDValidity, UID: s.UID}
}

// Action is one row of actions: one step taken (or, in dry-run, not taken) on a message.
type Action struct {
	ID         int64
	DecisionID int64 // 0 = none
	BatchID    int64 // 0 = none
	MessageID  int64
	AccountID  int64
	Kind       string // move | trash | archive | junk | flag | unflag | read | unread | keep | review
	Folder     string // params.folder: the destination of a move
	Before     Snapshot
	After      *Snapshot // nil unless the action is done (or was, before an undo)
	Status     string
	Error      string
	CreatedAt  int64
	UndoneAt   int64
}

// InsertAction writes an action row and returns its id.
func (s *Store) InsertAction(ctx context.Context, a Action) (int64, error) {
	before, err := json.Marshal(a.Before)
	if err != nil {
		return 0, fmt.Errorf("encode snapshot: %w", err)
	}
	var params any
	if a.Folder != "" {
		b, err := json.Marshal(map[string]string{"folder": a.Folder})
		if err != nil {
			return 0, fmt.Errorf("encode params: %w", err)
		}
		params = string(b)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO actions (decision_id, batch_id, message_id, account_id, kind, params, before, status, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		null(a.DecisionID), null(a.BatchID), a.MessageID, a.AccountID, a.Kind, params, string(before), a.Status, null(a.Error), a.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert action: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// setLocation records where a message now is, inside tx.
func setLocation(ctx context.Context, tx *sql.Tx, messageID int64, ref mail.MsgRef) error {
	_, err := tx.ExecContext(ctx, `UPDATE messages SET cur_folder = ?, cur_uidvalidity = ?, cur_uid = ? WHERE id = ?`,
		ref.Folder, ref.UIDValidity, ref.UID, messageID)
	return err
}

// FinishAction stores how an action ended. For a done action, after is the message's
// state afterwards, and the message's location is updated with it in the same transaction.
func (s *Store) FinishAction(ctx context.Context, a Action) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish action: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var after any
	if a.After != nil {
		b, err := json.Marshal(a.After)
		if err != nil {
			return fmt.Errorf("encode snapshot: %w", err)
		}
		after = string(b)
		if err := setLocation(ctx, tx, a.MessageID, a.After.Ref(a.AccountID)); err != nil {
			return fmt.Errorf("finish action: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = ?, error = ?, after = ? WHERE id = ?`, a.Status, null(a.Error), after, a.ID); err != nil {
		return fmt.Errorf("finish action: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish action: %w", err)
	}
	return nil
}

// MarkActionUndone marks a done action undone and records where its message is now.
func (s *Store) MarkActionUndone(ctx context.Context, a Action, restored mail.MsgRef, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mark action undone: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	if err := setLocation(ctx, tx, a.MessageID, restored); err != nil {
		return fmt.Errorf("mark action undone: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = ?, undone_at = ? WHERE id = ?`, ActionUndone, now, a.ID); err != nil {
		return fmt.Errorf("mark action undone: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark action undone: %w", err)
	}
	return nil
}

const actionCols = `id, COALESCE(decision_id, 0), COALESCE(batch_id, 0), message_id, account_id, kind,
	COALESCE(json_extract(params, '$.folder'), ''), before, COALESCE(after, ''), status, COALESCE(error, ''),
	created_at, COALESCE(undone_at, 0)`

func scanAction(row interface{ Scan(...any) error }) (Action, error) {
	var a Action
	var before, after string
	if err := row.Scan(&a.ID, &a.DecisionID, &a.BatchID, &a.MessageID, &a.AccountID, &a.Kind, &a.Folder, &before, &after,
		&a.Status, &a.Error, &a.CreatedAt, &a.UndoneAt); err != nil {
		return Action{}, err
	}
	if err := json.Unmarshal([]byte(before), &a.Before); err != nil {
		return Action{}, fmt.Errorf("decode action %d: %w", a.ID, err)
	}
	if after != "" {
		a.After = &Snapshot{}
		if err := json.Unmarshal([]byte(after), a.After); err != nil {
			return Action{}, fmt.Errorf("decode action %d: %w", a.ID, err)
		}
	}
	return a, nil
}

// Action returns one action, or ErrNotFound.
func (s *Store) Action(ctx context.Context, id int64) (Action, error) {
	a, err := scanAction(s.db.QueryRowContext(ctx, `SELECT `+actionCols+` FROM actions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, ErrNotFound
	}
	if err != nil {
		return Action{}, fmt.Errorf("get action: %w", err)
	}
	return a, nil
}

const (
	actionsOfMessage = `SELECT ` + actionCols + ` FROM actions WHERE message_id = ? ORDER BY id`
	actionsOfBatch   = `SELECT ` + actionCols + ` FROM actions WHERE batch_id = ? ORDER BY id`
)

func (s *Store) listActions(ctx context.Context, query string, args ...any) ([]Action, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, fmt.Errorf("list actions: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	return out, nil
}

// MessageActions lists every action taken on a message, oldest first.
func (s *Store) MessageActions(ctx context.Context, messageID int64) ([]Action, error) {
	return s.listActions(ctx, actionsOfMessage, messageID)
}

// BatchActions lists a batch's actions, oldest first.
func (s *Store) BatchActions(ctx context.Context, batchID int64) ([]Action, error) {
	return s.listActions(ctx, actionsOfBatch, batchID)
}

// Batch is one row of batches: a group of actions that can be undone together.
type Batch struct {
	ID        int64
	Kind      string // live | cleanup | review | correction | undo
	Status    string // running | done | failed | undone
	Total     int    // how many items the batch set out to handle; 0 = not counted (live)
	Done      int    // how many it has handled
	CreatedAt int64
}

// CreateBatch opens a batch and returns its id.
func (s *Store) CreateBatch(ctx context.Context, kind, status string, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO batches (kind, status, created_at) VALUES (?, ?, ?)`, kind, status, now)
	if err != nil {
		return 0, fmt.Errorf("create batch: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// Batch returns one batch, or ErrNotFound.
func (s *Store) Batch(ctx context.Context, id int64) (Batch, error) {
	b := Batch{ID: id}
	err := s.db.QueryRowContext(ctx, `SELECT kind, status, COALESCE(total, 0), done, created_at FROM batches WHERE id = ?`, id).
		Scan(&b.Kind, &b.Status, &b.Total, &b.Done, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Batch{}, ErrNotFound
	}
	if err != nil {
		return Batch{}, fmt.Errorf("get batch: %w", err)
	}
	return b, nil
}

// SetBatchStatus moves a batch to a status.
func (s *Store) SetBatchStatus(ctx context.Context, id int64, status string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE batches SET status = ? WHERE id = ?`, status, id); err != nil {
		return fmt.Errorf("set batch status: %w", err)
	}
	return nil
}

// LiveBatch returns the batch that collects the actions of live processing for now's day
// (UTC), creating it on the day's first action, so "undo today" is one batch undo. Once a
// day's batch has been undone, later mail that day starts a new one.
func (s *Store) LiveBatch(ctx context.Context, now time.Time) (int64, error) {
	day := now.UTC().Truncate(24 * time.Hour).Unix()
	const open = `kind = 'live' AND status <> 'undone' AND created_at >= ?`
	// One statement, so two accounts' first actions of the day cannot both create it.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO batches (kind, status, created_at) SELECT 'live', 'done', ? WHERE NOT EXISTS (SELECT 1 FROM batches WHERE `+open+`)`,
		now.Unix(), day); err != nil {
		return 0, fmt.Errorf("open live batch: %w", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM batches WHERE `+open+` ORDER BY id DESC LIMIT 1`, day).Scan(&id); err != nil {
		return 0, fmt.Errorf("open live batch: %w", err)
	}
	return id, nil
}

// settingDryRun is the settings key of the global dry-run switch.
const settingDryRun = "dry_run"

// DryRun reports whether the global dry-run switch is on. def is used until the switch
// has been set (MAILRULES_DRY_RUN).
func (s *Store) DryRun(ctx context.Context, def bool) (bool, error) {
	v, err := s.Setting(ctx, settingDryRun)
	if errors.Is(err, ErrNotFound) {
		return def, nil
	}
	if err != nil {
		return false, err
	}
	var on bool
	if err := json.Unmarshal([]byte(v), &on); err != nil {
		return false, fmt.Errorf("decode the dry_run setting %q: %w", v, err)
	}
	return on, nil
}

// SetDryRun turns the global dry-run switch on or off. A running daemon reads it before
// every action, so it takes effect at once.
func (s *Store) SetDryRun(ctx context.Context, on bool) error {
	return s.SetSetting(ctx, settingDryRun, fmt.Sprint(on))
}

// SetBatchProgress records how far a batch has come.
func (s *Store) SetBatchProgress(ctx context.Context, id int64, total, done int) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE batches SET total = ?, done = ? WHERE id = ?`, total, done, id); err != nil {
		return fmt.Errorf("set batch progress: %w", err)
	}
	return nil
}

// DoneActionsSince lists the actions made at or after since that are still in effect,
// newest first: the ones "undo everything since" reverses. With ruleID, only those a
// decision for that rule led to. The Needs review tag is left out: taking it off would not
// take the message out of review.
func (s *Store) DoneActionsSince(ctx context.Context, ruleID, since int64) ([]Action, error) {
	return s.listActions(ctx,
		`SELECT `+actionCols+` FROM actions
		 WHERE status = 'done' AND kind <> 'review' AND created_at >= ?1
		   AND (?2 = 0 OR decision_id IN (SELECT id FROM decisions WHERE rule_id = ?2))
		 ORDER BY id DESC`, since, ruleID)
}
