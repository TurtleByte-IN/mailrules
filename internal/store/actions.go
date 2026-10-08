package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	BatchCleanup    = "cleanup"
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
	Folder     string // params.folder: the destination of a move, or of a trash sent to MailRules' own folder (trash_to_folder)
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

// Action returns one action, or ErrNotFound. It does not look at who asks: a request
// reads VisibleAction.
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

// VisibleAction returns one action on a mailbox the viewer sees, or ErrNotFound.
func (s *Store) VisibleAction(ctx context.Context, v Viewer, id int64) (Action, error) {
	a, err := scanAction(s.db.QueryRowContext(ctx,
		`SELECT `+actionCols+` FROM actions WHERE id = ? AND `+inVisible("account_id", v), id))
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

// BatchActions lists a batch's actions, oldest first, whoever's mailbox they are on: for
// internal work. A request reads VisibleBatchActions.
func (s *Store) BatchActions(ctx context.Context, batchID int64) ([]Action, error) {
	return s.listActions(ctx, actionsOfBatch, batchID)
}

// VisibleBatchActions lists a batch's actions on the mailboxes the viewer sees, oldest
// first. A tenant's batch without a mailbox (the day's live batch, a correction or an undo)
// can hold actions on mailboxes of several people; each sees only theirs and shared ones.
func (s *Store) VisibleBatchActions(ctx context.Context, v Viewer, batchID int64) ([]Action, error) {
	return s.listActions(ctx, `SELECT `+actionCols+` FROM actions WHERE batch_id = ? AND `+inVisible("account_id", v)+` ORDER BY id`, batchID)
}

// Batch is one row of batches: a group of actions that can be undone together.
type Batch struct {
	ID        int64
	TenantID  int64
	Kind      string // live | cleanup | review | correction | undo
	Status    string // running | done | failed | undone
	Total     int    // how many items the batch set out to handle; 0 = not counted (live)
	Done      int    // how many it has handled
	CreatedAt int64

	// What a cleanup batch sorted, and what its model calls have cost so far. Zero for
	// the other kinds.
	AccountID int64
	Folder    string
	Since     int64 // only mail received from this time on; 0 = all of it
	Tokens    int   // model tokens, in and out
	CostUSD   float64
	// Skipped is how many selected emails a cleanup Sort passed over because they were no
	// longer where its check found them (MAI-44). 0 for the other kinds.
	Skipped int
	// ScanLimit is the most emails the cleanup's check was allowed to cover, and ScanMatched
	// how many the folder held in the range before that cut (MAI-48). ScanLimit 0 = not
	// recorded (a batch made before this), and then ScanMatched means nothing.
	ScanLimit   int
	ScanMatched int
	// RunID joins the batches of one manual run over several mailboxes, one per mailbox
	// (MAI-43): it is the id of the run's first batch, the same on every one. 0 = not part of
	// such a run, which is every other batch and a cleanup of one mailbox.
	RunID int64
}

const batchCols = `id, kind, status, COALESCE(total, 0), done, created_at,
	COALESCE(account_id, 0), COALESCE(folder, ''), COALESCE(since, 0), tokens, cost_usd, skipped,
	COALESCE(scan_limit, 0), COALESCE(scan_matched, 0), COALESCE(run_id, 0), tenant_id`

func scanBatch(row interface{ Scan(...any) error }) (Batch, error) {
	var b Batch
	err := row.Scan(&b.ID, &b.Kind, &b.Status, &b.Total, &b.Done, &b.CreatedAt, &b.AccountID, &b.Folder, &b.Since, &b.Tokens, &b.CostUSD, &b.Skipped, &b.ScanLimit, &b.ScanMatched, &b.RunID, &b.TenantID)
	return b, err
}

// visibleBatch is the condition that the batches row (unaliased) is one the viewer sees: a
// batch of a mailbox when they see the mailbox, a batch of no mailbox when it is their
// tenant's.
func visibleBatch(v Viewer) string {
	return `(tenant_id = ` + strconv.FormatInt(v.TenantID, 10) + ` AND (account_id IS NULL OR ` + inVisible("account_id", v) + `))`
}

// CleanupBatch is what a cleanup batch is opened with.
type CleanupBatch struct {
	AccountID int64
	Folder    string
	Since     int64 // only mail received from this time on; 0 = all of it
	// ScanLimit and ScanMatched say how much mail the check behind the batch covered: the
	// most emails it was allowed and how many the range held before that cut. ScanLimit 0
	// records nothing.
	ScanLimit, ScanMatched int
	Total                  int // emails the batch sorts
}

func insertCleanupBatch(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, c CleanupBatch, runID any, now int64) (int64, error) {
	var matched any // an empty range (0) is a fact when a limit was recorded
	if c.ScanLimit > 0 {
		matched = c.ScanMatched
	}
	// The batch belongs to the tenant of the mailbox it sorts.
	res, err := db.ExecContext(ctx,
		`INSERT INTO batches (tenant_id, kind, status, total, account_id, folder, since, scan_limit, scan_matched, run_id, created_at)
		 SELECT tenant_id, ?, ?, ?, id, ?, ?, ?, ?, ?, ? FROM accounts WHERE id = ?`,
		BatchCleanup, BatchRunning, c.Total, c.Folder, null(c.Since), null(c.ScanLimit), matched, runID, now, c.AccountID)
	if err != nil {
		return 0, fmt.Errorf("create cleanup batch: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("create cleanup batch: account %d: %w", c.AccountID, ErrNotFound)
	}
	return res.LastInsertId()
}

// CreateCleanupBatch opens the batch of one cleanup run over total emails, running.
//
// scanLimit and scanMatched say how much mail the check behind it covered: the most emails it
// was allowed and how many the range held before that cut. scanLimit 0 records nothing.
func (s *Store) CreateCleanupBatch(ctx context.Context, accountID int64, folder string, since int64, scanLimit, scanMatched, total int, now int64) (Batch, error) {
	id, err := insertCleanupBatch(ctx, s.db, CleanupBatch{AccountID: accountID, Folder: folder, Since: since, ScanLimit: scanLimit, ScanMatched: scanMatched, Total: total}, nil, now)
	if err != nil {
		return Batch{}, err
	}
	return s.Batch(ctx, id)
}

// CreateCleanupRun opens one running cleanup batch per mailbox for a manual run over several
// mailboxes, all or none: they share a RunID, the id of the first, so they read back as one
// run. A run of one mailbox is a plain cleanup batch with no RunID.
func (s *Store) CreateCleanupRun(ctx context.Context, cs []CleanupBatch, now int64) ([]Batch, error) {
	if len(cs) == 1 {
		b, err := s.CreateCleanupBatch(ctx, cs[0].AccountID, cs[0].Folder, cs[0].Since, cs[0].ScanLimit, cs[0].ScanMatched, cs[0].Total, now)
		return []Batch{b}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create cleanup run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ids := make([]int64, len(cs))
	var runID any
	for i, c := range cs {
		if ids[i], err = insertCleanupBatch(ctx, tx, c, runID, now); err != nil {
			return nil, err
		}
		if i == 0 {
			runID = ids[0]
			if _, err = tx.ExecContext(ctx, `UPDATE batches SET run_id = id WHERE id = ?`, ids[0]); err != nil {
				return nil, fmt.Errorf("create cleanup run: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create cleanup run: %w", err)
	}
	out := make([]Batch, len(ids))
	for i, id := range ids {
		if out[i], err = s.Batch(ctx, id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AddBatchProgress records that a cleanup batch handled more emails, passed over some that
// had moved, and what their model calls cost.
func (s *Store) AddBatchProgress(ctx context.Context, id int64, done, skipped, tokens int, costUSD float64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE batches SET done = done + ?, skipped = skipped + ?, tokens = tokens + ?, cost_usd = cost_usd + ? WHERE id = ?`,
		done, skipped, tokens, costUSD, id); err != nil {
		return fmt.Errorf("add batch progress: %w", err)
	}
	return nil
}

// FailRunningBatches marks every batch still running as failed. The daemon calls it at
// startup: a batch that was running when the daemon stopped will never finish.
func (s *Store) FailRunningBatches(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE batches SET status = ? WHERE status = ?`, BatchFailed, BatchRunning); err != nil {
		return fmt.Errorf("fail running batches: %w", err)
	}
	return nil
}

// Batches lists the batches the viewer sees, newest first: of one kind, or of every kind
// when kind is empty. before is the cursor (only batches with a smaller id; 0 = from the
// newest).
func (s *Store) Batches(ctx context.Context, v Viewer, kind string, before int64, limit int) ([]Batch, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+batchCols+` FROM batches WHERE `+visibleBatch(v)+` AND (?1 = '' OR kind = ?1) AND (?2 = 0 OR id < ?2) ORDER BY id DESC LIMIT ?3`, kind, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list batches: %w", err)
	}
	defer rows.Close()
	var out []Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("list batches: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list batches: %w", err)
	}
	return out, nil
}

// BatchActionCounts counts a batch's own actions on the mailboxes the viewer sees, by status.
func (s *Store) BatchActionCounts(ctx context.Context, v Viewer, batchID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM actions WHERE batch_id = ? AND `+inVisible("account_id", v)+` GROUP BY status`, batchID)
	if err != nil {
		return nil, fmt.Errorf("count batch actions: %w", err)
	}
	defer rows.Close()
	out := map[string]int{ActionDone: 0, ActionDryRun: 0, ActionFailed: 0, ActionUndone: 0}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("count batch actions: %w", err)
		}
		out[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count batch actions: %w", err)
	}
	return out, nil
}

// CreateBatch opens a batch of the tenant, with no mailbox, and returns its id.
func (s *Store) CreateBatch(ctx context.Context, tenantID int64, kind, status string, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO batches (tenant_id, kind, status, created_at) VALUES (?, ?, ?, ?)`, tenantID, kind, status, now)
	if err != nil {
		return 0, fmt.Errorf("create batch: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// Batch returns one batch, or ErrNotFound. It does not look at who asks: a request reads
// VisibleBatch.
func (s *Store) Batch(ctx context.Context, id int64) (Batch, error) {
	b, err := scanBatch(s.db.QueryRowContext(ctx, `SELECT `+batchCols+` FROM batches WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Batch{}, ErrNotFound
	}
	if err != nil {
		return Batch{}, fmt.Errorf("get batch: %w", err)
	}
	return b, nil
}

// VisibleBatch returns one batch the viewer sees, or ErrNotFound.
func (s *Store) VisibleBatch(ctx context.Context, v Viewer, id int64) (Batch, error) {
	b, err := scanBatch(s.db.QueryRowContext(ctx, `SELECT `+batchCols+` FROM batches WHERE id = ? AND `+visibleBatch(v), id))
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

// LiveBatch returns the batch that collects the tenant's actions of live processing for
// now's day (UTC), creating it on the day's first action, so "undo today" is one batch
// undo. Once a day's batch has been undone, later mail that day starts a new one.
func (s *Store) LiveBatch(ctx context.Context, tenantID int64, now time.Time) (int64, error) {
	day := now.UTC().Truncate(24 * time.Hour).Unix()
	const open = `tenant_id = ?1 AND kind = 'live' AND status <> 'undone' AND created_at >= ?2`
	// One statement, so two accounts' first actions of the day cannot both create it.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO batches (tenant_id, kind, status, created_at) SELECT ?1, 'live', 'done', ?3 WHERE NOT EXISTS (SELECT 1 FROM batches WHERE `+open+`)`,
		tenantID, day, now.Unix()); err != nil {
		return 0, fmt.Errorf("open live batch: %w", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM batches WHERE `+open+` ORDER BY id DESC LIMIT 1`, tenantID, day).Scan(&id); err != nil {
		return 0, fmt.Errorf("open live batch: %w", err)
	}
	return id, nil
}

// settingDryRun is the settings key of a tenant's dry-run switch.
const settingDryRun = "dry_run"

// SettingTrashToFolder is the settings key of the switch that sends trashed mail to an
// ordinary folder of MailRules' own instead of the server's Trash, which providers empty
// on their own. DefaultTrashToFolder is in force until it is set, for a new install and
// an existing one alike.
const (
	SettingTrashToFolder = "trash_to_folder"
	DefaultTrashToFolder = true
)

// DryRun reports whether the tenant's dry-run switch is on. def is used until the switch
// has been set (MAILRULES_DRY_RUN).
func (s *Store) DryRun(ctx context.Context, tenantID int64, def bool) (bool, error) {
	return s.boolSetting(ctx, tenantID, settingDryRun, def)
}

// SetDryRun turns the tenant's dry-run switch on or off. A running daemon reads it before
// every action, so it takes effect at once.
func (s *Store) SetDryRun(ctx context.Context, tenantID int64, on bool) error {
	return s.SetSetting(ctx, tenantID, settingDryRun, fmt.Sprint(on))
}

// TrashToFolder reports whether the tenant's trashed mail goes to MailRules' own folder
// rather than the server's Trash. The executor reads it before every trash action.
func (s *Store) TrashToFolder(ctx context.Context, tenantID int64) (bool, error) {
	return s.boolSetting(ctx, tenantID, SettingTrashToFolder, DefaultTrashToFolder)
}

// SetTrashToFolder turns the tenant's trash_to_folder switch on or off.
func (s *Store) SetTrashToFolder(ctx context.Context, tenantID int64, on bool) error {
	return s.SetSetting(ctx, tenantID, SettingTrashToFolder, fmt.Sprint(on))
}

// SettingLeaveOwnMail is the settings key of the switch that leaves mail sent from the
// mailbox's own address alone (Account.OwnAddresses): no rule, model or action touches it.
// DefaultLeaveOwnMail is in force until it is set.
const (
	SettingLeaveOwnMail = "leave_own_mail"
	DefaultLeaveOwnMail = true
)

// LeaveOwnMail reports whether, in the tenant, mail from the mailbox's own address is left
// alone. The pipeline reads it for every email.
func (s *Store) LeaveOwnMail(ctx context.Context, tenantID int64) (bool, error) {
	return s.boolSetting(ctx, tenantID, SettingLeaveOwnMail, DefaultLeaveOwnMail)
}

// boolSetting reads one of the tenant's stored switches; def is used until it has been set.
func (s *Store) boolSetting(ctx context.Context, tenantID int64, key string, def bool) (bool, error) {
	v, err := s.Setting(ctx, tenantID, key)
	if errors.Is(err, ErrNotFound) {
		return def, nil
	}
	if err != nil {
		return false, err
	}
	var on bool
	if err := json.Unmarshal([]byte(v), &on); err != nil {
		return false, fmt.Errorf("decode the %s setting %q: %w", key, v, err)
	}
	return on, nil
}

// SetBatchProgress records how far a batch has come.
func (s *Store) SetBatchProgress(ctx context.Context, id int64, total, done int) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE batches SET total = ?, done = ? WHERE id = ?`, total, done, id); err != nil {
		return fmt.Errorf("set batch progress: %w", err)
	}
	return nil
}

// DoneActionsSince lists the actions on the mailboxes the viewer sees made at or after
// since that are still in effect, newest first: the ones "undo everything since" reverses.
// With ruleID, only those a decision for that rule led to.
func (s *Store) DoneActionsSince(ctx context.Context, v Viewer, ruleID, since int64) ([]Action, error) {
	return s.listActions(ctx,
		`SELECT `+actionCols+` FROM actions
		 WHERE status = 'done' AND created_at >= ?1 AND `+inVisible("account_id", v)+`
		   AND (?2 = 0 OR decision_id IN (SELECT id FROM decisions WHERE rule_id = ?2))
		 ORDER BY id DESC`, since, ruleID)
}
