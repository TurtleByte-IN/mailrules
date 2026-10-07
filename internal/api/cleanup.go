package api

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// cleanupScope reads the body a cleanup check takes: which mail to check. It is the same
// selection the old preview took (mailbox, folder, "emails from" range, optional limit).
func (s *server) cleanupScope(w http.ResponseWriter, r *http.Request) (worker.Cleanup, bool) {
	var in struct {
		AccountID int64  `json:"account_id"`
		Folder    string `json:"folder"`
		Since     *int64 `json:"since"`
		Limit     *int   `json:"limit"`
	}
	if !readJSON(w, r, &in) {
		return worker.Cleanup{}, false
	}
	c := worker.Cleanup{AccountID: in.AccountID, Folder: in.Folder}
	if c.Folder == "" {
		c.Folder = "INBOX"
	}
	switch _, err := s.store.Account(r.Context(), in.AccountID); {
	case err != nil:
		invalid(w, "account_id", "No such account.")
	case in.Since != nil && *in.Since < 0:
		invalid(w, "since", "Give the start as a unix time in seconds, or null for all mail.")
	case in.Limit != nil && (*in.Limit < 1 || *in.Limit > composer.MaxLimit):
		invalid(w, "limit", "The limit must be between 1 and "+strconv.Itoa(composer.MaxLimit)+".")
	default:
		if in.Since != nil && *in.Since > 0 {
			c.Since = time.Unix(*in.Since, 0)
		}
		if in.Limit != nil {
			c.Limit = *in.Limit
		}
		return c, true
	}
	return worker.Cleanup{}, false
}

// rulesFingerprint is the signature of the rules a check was run against: the enabled rules
// in evaluation order and the sender rules. A cleanup Sort compares it with the rules in
// force; when they differ the check is stale and Sort is refused, so mail is never sorted
// by a rule the user has since rewritten (MAI-44, Tilak's option A).
func rulesFingerprint(rs []rules.Rule, senders []rules.SenderRule) string {
	type ruleFP struct {
		ID         int64
		Priority   int
		Name       string
		Intent     string
		Conditions rules.Cond
		Exceptions rules.Cond
		Actions    []rules.Action
		Stack      bool
		Model      string
		Min        *float64
		Account    int64
	}
	type senderFP struct {
		Type, Value, Verdict string
		RuleID               int64
	}
	var rfp []ruleFP
	for _, x := range rs {
		if !x.Enabled {
			continue
		}
		rfp = append(rfp, ruleFP{x.ID, x.Priority, x.Name, x.Intent, x.Conditions, x.Exceptions, x.Actions, x.Stack, x.Model, x.MinConfidence, x.AccountID})
	}
	sfp := make([]senderFP, len(senders))
	for i, x := range senders {
		sfp[i] = senderFP{x.MatchType, x.Value, x.Verdict, x.RuleID}
	}
	slices.SortFunc(sfp, func(a, b senderFP) int {
		if a.Type != b.Type {
			return cmpStr(a.Type, b.Type)
		}
		return cmpStr(a.Value, b.Value)
	})
	b, _ := json.Marshal(struct {
		R []ruleFP
		S []senderFP
	}{rfp, sfp})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// currentFingerprint is the fingerprint of the user's rules right now.
func (s *server) currentFingerprint(ctx context.Context, userID int64) (string, error) {
	rs, err := s.store.Rules(ctx, userID)
	if err != nil {
		return "", err
	}
	senders, err := s.store.SenderRules(ctx, userID)
	if err != nil {
		return "", err
	}
	return rulesFingerprint(rs, senders), nil
}

// handleCleanupCheckStart runs one real check of the selected mail — the whole flow, sender
// rules then conditions then the decision model, as live processing decides — moving
// nothing and recording nothing, but really asking the model and paying for it (the calls
// are booked under the "cleanup" purpose). It answers at once with the check, running; its
// rows, progress and cost arrive through check.progress events and GET /api/cleanup/check.
// The check runs in the background and survives the browser dropping this request.
func (s *server) handleCleanupCheckStart(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cleanupScope(w, r)
	if !ok {
		return
	}
	ctx, uid := r.Context(), user(r).ID
	mb, err := s.reader(c.AccountID)
	if err != nil {
		fail(w, r, err, "account")
		return
	}
	rs, err := s.store.Rules(ctx, uid)
	if err != nil {
		internalError(w, r, err)
		return
	}
	senders, err := s.store.SenderRules(ctx, uid)
	if err != nil {
		internalError(w, r, err)
		return
	}
	fp := rulesFingerprint(rs, senders)
	t := composer.Tester{Store: s.store, Mailbox: mb, AccountID: c.AccountID, Decider: s.cleanupDecider(ctx, uid), BodyChars: s.Settings.Env.BodyChars}
	run := func(ctx context.Context, report func(composer.CheckProgress)) ([]composer.CheckRow, error) {
		return t.Check(ctx, rs, senders, c.Folder, c.Since, c.Limit, report)
	}
	chk, err := s.StartCheck(c, fp, run)
	if err != nil {
		fail(w, r, err, "account") // not connected
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"check": s.checkJSON(chk.State())})
}

// handleCleanupCheckGet returns the account's current check, or null when there is none. A
// ready check whose rules have changed since is reported (and kept) stale.
func (s *server) handleCleanupCheckGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.checkAccount(w, r)
	if !ok {
		return
	}
	chk := s.Checks.Get(id)
	if chk == nil {
		writeJSON(w, http.StatusOK, map[string]any{"check": nil})
		return
	}
	st := chk.State()
	if st.Status == worker.CheckReady {
		fp, err := s.currentFingerprint(r.Context(), user(r).ID)
		if err != nil {
			internalError(w, r, err)
			return
		}
		if fp != st.Fingerprint {
			chk.MarkStale()
			st = chk.State()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"check": s.checkJSON(st)})
}

// handleCleanupCheckDelete discards the account's current check: the user pressed Discard.
func (s *server) handleCleanupCheckDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.checkAccount(w, r)
	if !ok {
		return
	}
	s.Checks.Discard(id)
	w.WriteHeader(http.StatusNoContent)
}

// checkAccount reads ?account_id= and checks the account exists.
func (s *server) checkAccount(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get("account_id"), 10, 64)
	if err != nil || id < 1 {
		invalid(w, "account_id", "Give account_id as a positive integer.")
		return 0, false
	}
	if _, err := s.store.Account(r.Context(), id); err != nil {
		invalid(w, "account_id", "No such account.")
		return 0, false
	}
	return id, true
}

// readyCheck returns the account's current check when it is the one named, ready and still
// in step with the rules, or answers why not (409 preview_stale for a replaced, gone or
// stale check, 409 check_not_ready while it runs or after it failed) and returns false.
func (s *server) readyCheck(w http.ResponseWriter, r *http.Request, accountID int64, checkID string) (*worker.Check, worker.CheckState, bool) {
	chk := s.Checks.Get(accountID)
	if chk == nil || chk.State().ID != checkID {
		writeError(w, http.StatusConflict, "preview_stale", "This check is no longer current. Run a new check, then sort.", "")
		return nil, worker.CheckState{}, false
	}
	st := chk.State()
	// A ready check may have gone stale: the rules changed since it ran.
	if st.Status == worker.CheckReady {
		fp, err := s.currentFingerprint(r.Context(), user(r).ID)
		if err != nil {
			internalError(w, r, err)
			return nil, worker.CheckState{}, false
		}
		if fp != st.Fingerprint {
			chk.MarkStale()
			st = chk.State()
		}
	}
	switch st.Status {
	case worker.CheckReady:
		return chk, st, true
	case worker.CheckStale:
		writeError(w, http.StatusConflict, "preview_stale", "The rules changed since this check. Run a new check, then sort.", "")
	default: // running or failed
		writeError(w, http.StatusConflict, "check_not_ready", "This check is not ready to sort yet.", "")
	}
	return nil, worker.CheckState{}, false
}

// handleCleanupSelection saves which rows of the current check the user has unticked, so a
// page reload or a return to the screen shows the same ticks. It lives only as long as the
// check, in the daemon's memory (MAI-44): Sort, Discard and a new check all drop it. Sort
// still takes its own explicit exclude list; this one only restores the screen.
func (s *server) handleCleanupSelection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID int64  `json:"account_id"`
		CheckID   string `json:"check_id"`
		Exclude   []int  `json:"exclude"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if _, err := s.store.Account(r.Context(), in.AccountID); err != nil {
		invalid(w, "account_id", "No such account.")
		return
	}
	if in.CheckID == "" {
		invalid(w, "check_id", "Say which check this selection is for.")
		return
	}
	chk, st, ok := s.readyCheck(w, r, in.AccountID, in.CheckID)
	if !ok {
		return
	}
	for _, i := range in.Exclude {
		if i < 0 || i >= len(st.Rows) || !selectableRow(st.Rows[i]) {
			invalid(w, "exclude", "Only rows that can be ticked can be left out.")
			return
		}
	}
	if !chk.SetExclude(in.Exclude) { // a Sort or a new check got in between
		writeError(w, http.StatusConflict, "check_not_ready", "This check is not ready to sort yet.", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCleanupRun applies a finished check's kept rows, as one undoable cleanup batch,
// making no model calls. The body names the check and, as an exclude list of row indices,
// the selectable rows the user unticked. The run is refused when the check is not this
// account's current one, is not ready (still running, failed, or stale because the rules
// changed since), or a Sort is already going for the account.
func (s *server) handleCleanupRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID int64  `json:"account_id"`
		CheckID   string `json:"check_id"`
		Exclude   []int  `json:"exclude"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	if _, err := s.store.Account(ctx, in.AccountID); err != nil {
		invalid(w, "account_id", "No such account.")
		return
	}
	if in.CheckID == "" {
		invalid(w, "check_id", "Say which check to sort: run a check first.")
		return
	}
	_, st, ok := s.readyCheck(w, r, in.AccountID, in.CheckID)
	if !ok {
		return
	}

	exclude := map[int]bool{}
	for _, i := range in.Exclude {
		exclude[i] = true
	}
	var rows []composer.CheckRow
	for i, row := range st.Rows {
		if selectableRow(row) && !exclude[i] {
			rows = append(rows, row)
		}
	}
	b, err := s.Sort(ctx, worker.SortRun{AccountID: in.AccountID, Folder: st.Folder, Since: st.Since, Rows: rows})
	if errors.Is(err, worker.ErrCleanupRunning) {
		writeError(w, http.StatusConflict, "cleanup_running", "A cleanup is already running for this account. Wait for it to finish.", "")
		return
	}
	if err != nil {
		fail(w, r, err, "account")
		return
	}
	s.Checks.Take(in.AccountID, in.CheckID) // used: delete it, unless a newer check replaced it
	bj, err := s.batchJSON(ctx, b)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"batch": bj})
}

// selectableRow reports whether a checked email can be ticked for Sort: a rule settled it
// with an action, and it would not wait in Needs review.
func selectableRow(row composer.CheckRow) bool {
	return len(row.Outcome.Actions) > 0 && !row.Outcome.Review
}

// checkRowJSON is one row of the check table: its JSON is the contract's CleanupCheckRow.
type checkRowJSON struct {
	Index       int            `json:"index"`
	From        string         `json:"from"`
	Subject     string         `json:"subject"`
	ReceivedAt  *int64         `json:"received_at"`
	Folder      string         `json:"folder"`
	UID         uint32         `json:"uid"`
	UIDValidity uint32         `json:"uidvalidity"`
	Stage       string         `json:"stage"`
	RuleID      *int64         `json:"rule_id"`
	RuleName    string         `json:"rule_name"`
	Actions     []rules.Action `json:"actions"`
	Confidence  *float64       `json:"confidence"` // null where no model was asked
	Selectable  bool           `json:"selectable"`
	Review      bool           `json:"review"` // it would wait in Needs review
	Reason      string         `json:"reason"`
}

// cleanupCheckJSON is a check read back: its JSON is the contract's CleanupCheck.
type cleanupCheckJSON struct {
	ID         string         `json:"id"`
	AccountID  int64          `json:"account_id"`
	Folder     string         `json:"folder"`
	Since      *int64         `json:"since"`
	Limit      *int           `json:"limit"`
	Status     string         `json:"status"`
	Done       int            `json:"done"`
	Total      int            `json:"total"`
	ModelCalls int            `json:"model_calls"`
	Tokens     int            `json:"tokens"`
	CostUSD    float64        `json:"cost_usd"`
	Error      string         `json:"error"`
	Rows       []checkRowJSON `json:"rows"`
	// Exclude is the saved selection: the unticked selectable rows' indices, ascending.
	// Empty means every selectable row is ticked.
	Exclude []int `json:"exclude"`
}

// checkJSON shapes a check for the browser. Rows are present only when it is ready or stale.
func (s *server) checkJSON(st worker.CheckState) cleanupCheckJSON {
	out := cleanupCheckJSON{ID: st.ID, AccountID: st.AccountID, Folder: st.Folder, Since: ts(st.Since), Status: st.Status,
		Done: st.Done, Total: st.Total, ModelCalls: st.ModelCalls, Tokens: st.Tokens, CostUSD: st.CostUSD, Error: st.Error, Rows: []checkRowJSON{}, Exclude: append([]int{}, st.Exclude...)}
	if st.Limit > 0 {
		out.Limit = &st.Limit
	}
	for i, row := range st.Rows {
		o := row.Outcome
		j := checkRowJSON{Index: i, From: row.From, Subject: row.Subject, Folder: row.Ref.Folder, UID: row.Ref.UID,
			UIDValidity: row.Ref.UIDValidity, Stage: string(o.Stage), RuleName: o.RuleName,
			Actions: append([]rules.Action{}, o.Actions...), Selectable: selectableRow(row), Review: o.Review, Reason: o.Reason}
		if row.ReceivedAt != 0 {
			at := row.ReceivedAt
			j.ReceivedAt = &at
		}
		if o.RuleID > 0 {
			id := o.RuleID
			j.RuleID = &id
		}
		if j.RuleName == "" && o.Stage == rules.StageSender {
			j.RuleName = o.Reason // "Sender rule: keep" / "Sender rule: trash"
		}
		if o.Asked {
			c := o.Confidence
			j.Confidence = &c
		}
		out.Rows = append(out.Rows, j)
	}
	return out
}

// cleanupDecider is the deciding step of a cleanup check: the routers in force, with their
// calls booked under the "cleanup" purpose so Usage can tell them from live sorting.
func (s *server) cleanupDecider(ctx context.Context, userID int64) pipeline.Decider {
	src := s.modelSource()
	router, minConfidence := src.Live(ctx)
	return pipeline.Decider{Router: router.For("cleanup"), MinConfidence: minConfidence, Now: s.now(),
		Examples: pipeline.Corrections(s.store, userID),
		Override: func(ctx context.Context, spec string) *models.Router { return src.RouterFor(ctx, spec).For("cleanup") }}
}

var batchKinds = []string{store.BatchLive, store.BatchCleanup, "review", store.BatchCorrection, store.BatchUndo}

// handleBatches lists past batches, newest first: the Cleanup screen's "batches you can undo".
func (s *server) handleBatches(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, limit := q.Get("kind"), 50
	var before int64
	if kind != "" && !slices.Contains(batchKinds, kind) {
		invalid(w, "kind", "Unknown kind of batch.")
		return
	}
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			invalid(w, "cursor", "The cursor must be a positive integer.")
			return
		}
		before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			invalid(w, "limit", "The limit must be between 1 and 100.")
			return
		}
		limit = n
	}
	rows, err := s.store.Batches(r.Context(), kind, before, limit+1)
	if err != nil {
		internalError(w, r, err)
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := strconv.FormatInt(rows[limit-1].ID, 10)
		next = &c
	}
	items := make([]batchJSON, len(rows))
	for i, b := range rows {
		if items[i], err = s.batchJSON(r.Context(), b); err != nil {
			internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// templates is the rule template gallery, as served: {"items": [...]}. A test saves every
// one of its rules through POST /api/rules/batch.
//
//go:embed templates.json
var templates []byte

func (s *server) handleTemplates(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(templates)
}
