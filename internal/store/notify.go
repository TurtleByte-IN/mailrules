package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"strconv"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// Notification destination kinds, as stored in notify_destinations.kind.
const (
	DestinationWebhook = "webhook"
	DestinationSlack   = "slack"
)

// Delivery statuses, as stored in notify_deliveries.status.
const (
	DeliverySent   = "sent"
	DeliveryFailed = "failed"
)

// NoticeDays is how long a notice, and what its module reported of delivering it, is kept.
const NoticeDays = 30

// Destination is one place a tenant's rule notifications go. Its address and signing secret
// are never part of it: read them with DestinationSecret when a notice is handed over.
type Destination struct {
	ID        int64
	TenantID  int64
	Name      string
	Kind      string // DestinationWebhook | DestinationSlack
	URLHint   string // the address cut short (URLHint), the only form of it shown
	CreatedAt int64
	// AddressDeleted: the address and secret were deleted with the sign-in of the person who
	// was the tenant's only member (ForgetIdentity); nothing can be delivered to it.
	AddressDeleted bool
}

// DestinationSecret is what is sealed for a destination: where notices go and, for a
// webhook, the secret deliveries are signed with.
type DestinationSecret struct {
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

// URLHint is an address as Settings shows it: its scheme and host, then the last four
// characters when it has a path or query, so a Slack webhook's token stays unread.
func URLHint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "…"
	}
	hint := u.Scheme + "://" + u.Host
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		r := []rune(raw)
		hint += "/…" + string(r[max(len(r)-4, 0):])
	}
	return hint
}

// destinationAAD binds a destination's ciphertext to its row, apart from the accounts rows
// that seal with their own ids.
func destinationAAD(id int64) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("notify_destination:" + strconv.FormatInt(id, 10)))
	return int64(h.Sum64()) // #nosec G115 -- bit pattern only
}

// CreateDestination stores a destination of the tenant with its address and secret sealed
// under the master key, and returns it with its id.
func (s *Store) CreateDestination(ctx context.Context, master []byte, d Destination, sec DestinationSecret, now int64) (Destination, error) {
	plain, err := json.Marshal(sec) //nolint:gosec // G117: the secret is sealed right below, never stored or sent in clear
	if err != nil {
		return Destination{}, fmt.Errorf("encode destination: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Destination{}, fmt.Errorf("create destination: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	d.URLHint, d.CreatedAt, d.AddressDeleted = URLHint(sec.URL), now, false
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO notify_destinations (tenant_id, name, kind, url_hint, secret_enc, dek_enc, created_at)
		 VALUES (?, ?, ?, ?, x'', x'', ?) RETURNING id`,
		d.TenantID, d.Name, d.Kind, d.URLHint, now).Scan(&d.ID); err != nil {
		return Destination{}, fmt.Errorf("create destination: %w", err)
	}
	secretEnc, dekEnc, err := crypto.Seal(master, destinationAAD(d.ID), plain)
	if err != nil {
		return Destination{}, fmt.Errorf("encrypt destination: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notify_destinations SET secret_enc = ?, dek_enc = ? WHERE id = ?`, secretEnc, dekEnc, d.ID); err != nil {
		return Destination{}, fmt.Errorf("create destination: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Destination{}, fmt.Errorf("create destination: %w", err)
	}
	return d, nil
}

const destinationCols = `id, tenant_id, name, kind, url_hint, created_at, length(secret_enc) = 0`

func scanDestination(row interface{ Scan(...any) error }) (Destination, error) {
	var d Destination
	err := row.Scan(&d.ID, &d.TenantID, &d.Name, &d.Kind, &d.URLHint, &d.CreatedAt, &d.AddressDeleted)
	return d, err
}

// Destinations lists the tenant's destinations, oldest first.
func (s *Store) Destinations(ctx context.Context, tenantID int64) ([]Destination, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+destinationCols+` FROM notify_destinations WHERE tenant_id = ? ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list destinations: %w", err)
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, fmt.Errorf("list destinations: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Destination returns one destination, or ErrNotFound. It does not look at who asks: it is
// for the delivery worker.
func (s *Store) Destination(ctx context.Context, id int64) (Destination, error) {
	d, err := scanDestination(s.db.QueryRowContext(ctx, `SELECT `+destinationCols+` FROM notify_destinations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Destination{}, ErrNotFound
	}
	if err != nil {
		return Destination{}, fmt.Errorf("get destination: %w", err)
	}
	return d, nil
}

// DestinationSecret opens a destination's address and secret. ErrNoSecret when they were
// deleted (Destination.AddressDeleted), ErrNotFound when there is no such destination.
func (s *Store) DestinationSecret(ctx context.Context, master []byte, id int64) (DestinationSecret, error) {
	var secretEnc, dekEnc []byte
	err := s.db.QueryRowContext(ctx, `SELECT secret_enc, dek_enc FROM notify_destinations WHERE id = ?`, id).Scan(&secretEnc, &dekEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return DestinationSecret{}, ErrNotFound
	}
	if err != nil {
		return DestinationSecret{}, fmt.Errorf("get destination secret: %w", err)
	}
	if len(secretEnc) == 0 {
		return DestinationSecret{}, ErrNoSecret
	}
	plain, err := crypto.Open(master, destinationAAD(id), secretEnc, dekEnc)
	if err != nil {
		return DestinationSecret{}, fmt.Errorf("decrypt destination: %w", err)
	}
	var sec DestinationSecret
	if err := json.Unmarshal(plain, &sec); err != nil {
		return DestinationSecret{}, fmt.Errorf("decode destination: %w", err)
	}
	return sec, nil
}

// SealedDestinations lists every tenant's destinations whose address is stored (not
// deleted), for checking the master key opens them.
func (s *Store) SealedDestinations(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM notify_destinations WHERE length(secret_enc) > 0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list sealed destinations: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list sealed destinations: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteDestination removes one of the tenant's destinations with its delivery log and the
// notices still waiting for it, and takes it off every rule of the tenant that named it,
// in one transaction. It returns the rules it changed, as saved (an edit: their versions
// go up). A destination of another tenant is ErrNotFound.
func (s *Store) DeleteDestination(ctx context.Context, tenantID, id, now int64) ([]rules.Rule, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("delete destination: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	res, err := tx.ExecContext(ctx, `DELETE FROM notify_destinations WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return nil, fmt.Errorf("delete destination: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	named, err := rulesNaming(ctx, tx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("delete destination: %w", err)
	}
	for i := range named {
		r := &named[i]
		r.Notify = removeID(r.Notify, id)
		if err := updateRule(ctx, tx, tenantID, *r, now); err != nil {
			return nil, err
		}
		r.Version, r.UpdatedAt = r.Version+1, now
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("delete destination: %w", err)
	}
	return named, nil
}

// rulesNaming lists, in tx, the tenant's rules that notify the destination.
func rulesNaming(ctx context.Context, tx *sql.Tx, tenantID, destinationID int64) ([]rules.Rule, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+ruleColumns+` FROM rules
		WHERE tenant_id = ? AND EXISTS (SELECT 1 FROM json_each(rules.notify) WHERE value = ?) ORDER BY id`, tenantID, destinationID)
	if err != nil {
		return nil, fmt.Errorf("rules naming it: %w", err)
	}
	defer rows.Close()
	var named []rules.Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		named = append(named, r)
	}
	return named, rows.Err()
}

func removeID(ids []int64, id int64) []int64 {
	var out []int64
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// Delivery is one result a module reported of delivering a notice.
type Delivery struct {
	ID            int64
	DestinationID int64
	NoticeID      int64
	RuleID        int64 // 0 once the rule was deleted
	RuleName      string
	Status        string // DeliverySent | DeliveryFailed
	Reason        string // why it failed; empty when sent
	CreatedAt     int64
}

// RecordDelivery stores what a module reported of delivering a notice: sent, or failed and
// why. It returns the notice's tenant. A notice that is no longer kept (its destination or
// mailbox was removed, or it is older than NoticeDays) is ErrNotFound.
func (s *Store) RecordDelivery(ctx context.Context, noticeID int64, sent bool, reason string, at int64) (tenantID int64, err error) {
	status := DeliveryFailed
	if sent {
		status, reason = DeliverySent, ""
	}
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO notify_deliveries (tenant_id, destination_id, notice_id, rule_id, rule_name, status, reason, created_at)
		 SELECT tenant_id, destination_id, id, rule_id, rule_name, ?, ?, ? FROM notices WHERE id = ?
		 RETURNING tenant_id`, status, reason, at, noticeID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("record delivery: %w", err)
	}
	return tenantID, nil
}

// RecentDeliveries lists, for each of the tenant's destinations, the last perDestination
// deliveries reported, newest first.
func (s *Store) RecentDeliveries(ctx context.Context, tenantID int64, perDestination int) (map[int64][]Delivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, destination_id, notice_id, COALESCE(rule_id, 0), rule_name, status, reason, created_at FROM (
		   SELECT *, ROW_NUMBER() OVER (PARTITION BY destination_id ORDER BY created_at DESC, id DESC) AS n
		   FROM notify_deliveries WHERE tenant_id = ?)
		 WHERE n <= ? ORDER BY destination_id, created_at DESC, id DESC`, tenantID, perDestination)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()
	out := map[int64][]Delivery{}
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.DestinationID, &d.NoticeID, &d.RuleID, &d.RuleName, &d.Status, &d.Reason, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("list deliveries: %w", err)
		}
		out[d.DestinationID] = append(out[d.DestinationID], d)
	}
	return out, rows.Err()
}

// Notice is one waiting notice: what a rule did to one email, for one destination.
type Notice struct {
	ID            int64
	TenantID      int64
	DestinationID int64
	AccountID     int64
	MessageID     int64 // the Activity id
	RuleID        int64 // 0 once the rule was deleted
	RuleName      string
	CreatedAt     int64 // when the rule acted
	Attempts      int   // hand-overs the module refused so far
	NoticePayload
}

// NoticePayload is the part of a notice that holds the email's own details, kept only until
// the module takes the notice.
type NoticePayload struct {
	Mailbox  string   `json:"mailbox"`
	Actions  []string `json:"actions"`
	Folder   string   `json:"folder"`
	From     string   `json:"from"`
	FromName string   `json:"from_name"`
	Subject  string   `json:"subject"`
}

// queueNotices writes, in tx, the notices of a rule's run on one email once its last action
// is recorded (a, just finished): one per destination the rule names, when the decision was
// a rule's (not a sender rule's), in the live batch, and at least one of its actions was
// done. Dry-run, cleanup, correction and undo runs queue none.
func queueNotices(ctx context.Context, tx *sql.Tx, a Action) error {
	if a.DecisionID == 0 || a.BatchID == 0 {
		return nil
	}
	var stage, ruleName, notify, kind string
	var ruleID sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT d.stage, d.rule_id, COALESCE(d.rule_name, r.name, ''), COALESCE(r.notify, '[]'), b.kind
		 FROM decisions d JOIN batches b ON b.id = ? LEFT JOIN rules r ON r.id = d.rule_id
		 WHERE d.id = ?`, a.BatchID, a.DecisionID).Scan(&stage, &ruleID, &ruleName, &notify, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("queue notices: %w", err)
	}
	if kind != BatchLive || stage == string(rules.StageSender) || !ruleID.Valid || notify == "[]" {
		return nil
	}
	var p NoticePayload
	if p.Actions, err = doneKinds(ctx, tx, a.DecisionID); err != nil || len(p.Actions) == 0 {
		return err
	}
	var acct Account
	var tenantID, messageID int64
	err = tx.QueryRowContext(ctx,
		`SELECT m.id, COALESCE(m.cur_folder, m.folder), COALESCE(m.from_addr, ''), COALESCE(m.from_name, ''), COALESCE(m.subject, ''),
		        acc.tenant_id, acc.preset, acc.username, acc.label
		 FROM messages m JOIN accounts acc ON acc.id = m.account_id WHERE m.id = ?`, a.MessageID).
		Scan(&messageID, &p.Folder, &p.From, &p.FromName, &p.Subject, &tenantID, &acct.Preset, &acct.Username, &acct.Label)
	if err != nil {
		return fmt.Errorf("queue notices: message: %w", err)
	}
	p.Mailbox = acct.Label
	if own := acct.OwnAddresses(); len(own) > 0 {
		p.Mailbox = own[0]
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("queue notices: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO notices (tenant_id, destination_id, account_id, message_id, rule_id, rule_name, payload, created_at, next_at)
		 SELECT ?, id, ?, ?, ?, ?, ?, ?, ? FROM notify_destinations
		 WHERE tenant_id = ? AND id IN (SELECT value FROM json_each(?)) ORDER BY id`,
		tenantID, a.AccountID, messageID, ruleID.Int64, ruleName, string(payload), a.CreatedAt, a.CreatedAt, tenantID, notify); err != nil {
		return fmt.Errorf("queue notices: %w", err)
	}
	return nil
}

// doneKinds lists, in tx, the kinds of the decision's actions that were done, in order.
func doneKinds(ctx context.Context, tx *sql.Tx, decisionID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT kind FROM actions WHERE decision_id = ? AND status = ? ORDER BY id`, decisionID, ActionDone)
	if err != nil {
		return nil, fmt.Errorf("queue notices: actions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("queue notices: actions: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// WaitingNotices lists up to limit notices the module has not taken yet that are due by
// now, oldest first.
func (s *Store) WaitingNotices(ctx context.Context, now int64, limit int) ([]Notice, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, destination_id, account_id, message_id, COALESCE(rule_id, 0), rule_name, created_at, attempts, COALESCE(payload, '{}')
		 FROM notices WHERE handed_at IS NULL AND next_at <= ? ORDER BY next_at, id LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list waiting notices: %w", err)
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		var payload string
		if err := rows.Scan(&n.ID, &n.TenantID, &n.DestinationID, &n.AccountID, &n.MessageID, &n.RuleID, &n.RuleName, &n.CreatedAt, &n.Attempts, &payload); err != nil {
			return nil, fmt.Errorf("list waiting notices: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &n.NoticePayload); err != nil {
			return nil, fmt.Errorf("decode notice %d: %w", n.ID, err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NoticeHanded records that the module took a notice: it is not handed over again, and the
// email's details it held are blanked.
func (s *Store) NoticeHanded(ctx context.Context, id, now int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE notices SET handed_at = ?, payload = NULL WHERE id = ?`, now, id); err != nil {
		return fmt.Errorf("mark notice handed over: %w", err)
	}
	return nil
}

// NoticeRetry records that the module refused a notice; it is handed over again at next.
func (s *Store) NoticeRetry(ctx context.Context, id, next int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE notices SET attempts = attempts + 1, next_at = ? WHERE id = ?`, next, id); err != nil {
		return fmt.Errorf("retry notice: %w", err)
	}
	return nil
}

// retainNotices deletes the tenant's notices, handed over or not, and delivery records
// older than NoticeDays.
func (s *Store) retainNotices(ctx context.Context, tenantID, now int64) error {
	cut := now - NoticeDays*day
	if _, err := s.db.ExecContext(ctx, `DELETE FROM notices WHERE tenant_id = ? AND created_at < ?`, tenantID, cut); err != nil {
		return fmt.Errorf("delete old notices: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM notify_deliveries WHERE tenant_id = ? AND created_at < ?`, tenantID, cut); err != nil {
		return fmt.Errorf("delete old deliveries: %w", err)
	}
	return nil
}
