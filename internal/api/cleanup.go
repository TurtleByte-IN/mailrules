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

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// scope checks a selection of mail and fills in what it leaves out, answering 400 itself
// when it cannot be used.
func (s *server) scope(w http.ResponseWriter, r *http.Request, in ext.ScopeInput) (worker.Cleanup, bool) {
	// Every check covers at most the newest composer.MaxLimit emails of its range (MAI-48):
	// a request that names no limit gets the most, so no client can ask for an unbounded run.
	c := worker.Cleanup{AccountID: in.AccountID, Folder: in.Folder, Limit: composer.MaxLimit}
	if c.Folder == "" {
		c.Folder = "INBOX"
	}
	switch _, err := s.store.VisibleAccount(r.Context(), viewer(r), in.AccountID); {
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

// ruleSet is the rules a cleanup check works with. Walk are the rules checked, in their usual
// order. RouteOnly are enabled rules that are not walked but that a sender rule routes to:
// sender rules always apply, whatever rules a run was limited to (MAI-43), so a sender rule
// that sends one sender's mail to an unpicked rule still does.
type ruleSet struct {
	Walk, RouteOnly []rules.Rule
}

// selectRules picks the rules a check works with out of all the user's rules, which come in
// their usual order. ids nil picks every one (RouteOnly is then empty: the walk has them all).
func selectRules(all []rules.Rule, senders []rules.SenderRule, ids []int64) ruleSet {
	if ids == nil {
		return ruleSet{Walk: all}
	}
	var set ruleSet
	routed := map[int64]bool{}
	for _, sr := range senders {
		if sr.Verdict == rules.VerdictRoute && !slices.Contains(ids, sr.RuleID) {
			routed[sr.RuleID] = true
		}
	}
	for _, r := range all {
		switch {
		case slices.Contains(ids, r.ID):
			set.Walk = append(set.Walk, r)
		case routed[r.ID] && r.Enabled:
			set.RouteOnly = append(set.RouteOnly, r)
		}
	}
	return set
}

// pickedRules checks the rule_ids of a request against the user's rules and answers 400
// itself when they cannot be used: none named (nil, leaving it out, picks every enabled
// rule, an empty list picks none), a rule that does not exist or is switched off. It
// returns the ids without repeats, in the order given; nil when every rule is picked.
func pickedRules(w http.ResponseWriter, all []rules.Rule, ids []int64) ([]int64, bool) {
	if ids == nil {
		return nil, true
	}
	if len(ids) == 0 {
		invalid(w, "rule_ids", "Pick at least one rule, or leave rule_ids out to use every rule.")
		return nil, false
	}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		i := slices.IndexFunc(all, func(r rules.Rule) bool { return r.ID == id })
		switch {
		case i < 0:
			invalid(w, "rule_ids", "No such rule.")
			return nil, false
		case !all[i].Enabled:
			invalid(w, "rule_ids", "A rule that is switched off cannot be checked. Switch it on first.")
			return nil, false
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, true
}

// rulesFingerprint is the signature of the rules a check was run against: the enabled rules
// it walked in evaluation order, the rules only a sender rule routes to, and the sender
// rules. A cleanup Sort compares it with the rules in force; when they differ the check is
// stale and Sort is refused, so mail is never sorted by a rule the user has since rewritten
// (MAI-44, Tilak's option A). A check limited to some rules is stale only when one of those,
// or a sender rule, changes.
func rulesFingerprint(set ruleSet, senders []rules.SenderRule) string {
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
		Type, Value, Verdict, Folder string
		RuleID                       int64
	}
	fps := func(rs []rules.Rule) []ruleFP {
		var out []ruleFP
		for _, x := range rs {
			if x.Enabled {
				out = append(out, ruleFP{x.ID, x.Priority, x.Name, x.Intent, x.Conditions, x.Exceptions, x.Actions, x.Stack, x.Model, x.MinConfidence, x.AccountID})
			}
		}
		return out
	}
	sfp := make([]senderFP, len(senders))
	for i, x := range senders {
		sfp[i] = senderFP{x.MatchType, x.Value, x.Verdict, x.Folder, x.RuleID}
	}
	slices.SortFunc(sfp, func(a, b senderFP) int {
		if a.Type != b.Type {
			return cmpStr(a.Type, b.Type)
		}
		return cmpStr(a.Value, b.Value)
	})
	b, _ := json.Marshal(struct {
		R []ruleFP
		O []ruleFP
		S []senderFP
	}{fps(set.Walk), fps(set.RouteOnly), sfp})
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

// currentFingerprint is the fingerprint of the rules a check limited to ruleIDs (nil = every
// rule) would be made with right now.
func (s *server) currentFingerprint(ctx context.Context, tenantID int64, ruleIDs []int64) (string, error) {
	rs, err := s.store.Rules(ctx, tenantID)
	if err != nil {
		return "", err
	}
	senders, err := s.store.SenderRules(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return rulesFingerprint(selectRules(rs, senders, ruleIDs), senders), nil
}

// cleanupMailboxes is which mailboxes a check request names: the account_id of the original
// shape, or the account_ids of a manual run over several (not both), without repeats. It
// answers 400 itself when it cannot say. A single account_id is not looked up here: scope does.
func (s *server) cleanupMailboxes(w http.ResponseWriter, r *http.Request, in cleanupStart) ([]int64, bool) {
	switch {
	case in.AccountIDs == nil:
		return []int64{in.AccountID}, true
	case in.AccountID != 0:
		invalid(w, "account_ids", "Give account_id or account_ids, not both.")
		return nil, false
	case len(in.AccountIDs) == 0:
		invalid(w, "account_ids", "Pick at least one mailbox.")
		return nil, false
	}
	var ids []int64
	for _, id := range in.AccountIDs {
		if _, err := s.store.VisibleAccount(r.Context(), viewer(r), id); err != nil {
			invalid(w, "account_ids", "No such account.")
			return nil, false
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, true
}

// cleanupStart is the body of a cleanup check: the contract's CleanupCheckRequest.
type cleanupStart struct {
	ext.ScopeInput
	AccountIDs []int64 `json:"account_ids"` // mailboxes to check together; the original shape names one in account_id
	RuleIDs    []int64 `json:"rule_ids"`    // check only these rules; nil = every enabled rule
}

// handleCleanupCheckStart runs one real check of the selected mail of each mailbox named —
// the whole flow, sender rules then conditions then the decision model, as live processing
// decides — moving nothing and recording nothing, but really asking the model and paying for
// it (the calls are booked under the "cleanup" purpose). It answers at once with the checks,
// running; their rows, progress and cost arrive through check.progress events and GET
// /api/cleanup/check. The checks run in the background and survive the browser dropping this
// request. Mailboxes are checked together, each on its own; rule_ids limits every check to
// those rules, still in their usual order, with sender rules applying as always (MAI-43).
func (s *server) handleCleanupCheckStart(w http.ResponseWriter, r *http.Request) {
	var in cleanupStart
	if !readJSON(w, r, &in) {
		return
	}
	ids, ok := s.cleanupMailboxes(w, r, in)
	if !ok {
		return
	}
	c, ok := s.scope(w, r, ext.ScopeInput{AccountID: ids[0], Folder: in.Folder, Since: in.Since, Limit: in.Limit})
	if !ok {
		return
	}
	ctx, v := r.Context(), viewer(r)
	rs, err := s.store.Rules(ctx, v.TenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	senders, err := s.store.SenderRules(ctx, v.TenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if c.RuleIDs, ok = pickedRules(w, rs, in.RuleIDs); !ok {
		return
	}
	set := selectRules(rs, senders, c.RuleIDs)
	fp := rulesFingerprint(set, senders)

	// Every mailbox is made ready before any check starts, so the run begins on all or none.
	runs := make([]worker.CheckFunc, len(ids))
	for i, id := range ids {
		mb, err := s.reader(id)
		if err != nil {
			fail(w, r, err, "account")
			return
		}
		acct, err := s.store.VisibleAccount(ctx, v, id)
		if err != nil {
			internalError(w, r, err)
			return
		}
		decider, err := s.cleanupDecider(ctx, v, acct)
		if err != nil {
			internalError(w, r, err)
			return
		}
		decider.RouteOnly = set.RouteOnly
		t := composer.Tester{Store: s.store, Mailbox: mb, AccountID: id, Decider: decider, BodyChars: s.Settings.Env.BodyChars}
		runs[i] = func(ctx context.Context, report func(composer.CheckProgress)) ([]composer.CheckRow, error) {
			return t.Check(ctx, set.Walk, senders, c.Folder, c.Since, c.Limit, report)
		}
	}
	started := make([]cleanupCheckJSON, len(ids))
	for i, id := range ids {
		ci := c
		ci.AccountID = id
		chk, err := s.StartCheck(ci, fp, runs[i])
		if err != nil { // not connected, in the moment since it was looked at
			for _, done := range ids[:i] {
				s.Checks.Discard(done)
			}
			fail(w, r, err, "account")
			return
		}
		started[i] = s.checkJSON(chk.Started())
	}
	resp := map[string]any{"checks": started}
	if in.AccountIDs == nil {
		resp["check"] = started[0]
	}
	writeJSON(w, http.StatusAccepted, resp)
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
	st, err := s.freshState(r.Context(), viewer(r).TenantID, chk)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"check": s.checkJSON(st)})
}

// handleCleanupChecks returns every account's current check, in account order: what the
// screen asks for when it opens, to show a manual run over several mailboxes as it stands.
func (s *server) handleCleanupChecks(w http.ResponseWriter, r *http.Request) {
	out := []cleanupCheckJSON{}
	for _, chk := range s.Checks.All() {
		if !s.seesAccount(r, chk.AccountID) {
			continue // the account was deleted since, or is not one the viewer sees
		}
		st, err := s.freshState(r.Context(), viewer(r).TenantID, chk)
		if err != nil {
			internalError(w, r, err)
			return
		}
		out = append(out, s.checkJSON(st))
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": out})
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
	if _, err := s.store.VisibleAccount(r.Context(), viewer(r), id); err != nil {
		invalid(w, "account_id", "No such account.")
		return 0, false
	}
	return id, true
}

// freshState reads a check's state, first moving a ready check whose rules have changed
// since it ran to stale: its rows were settled by rules that are no longer in force.
func (s *server) freshState(ctx context.Context, tenantID int64, chk *worker.Check) (worker.CheckState, error) {
	st := chk.State()
	if st.Status != worker.CheckReady {
		return st, nil
	}
	fp, err := s.currentFingerprint(ctx, tenantID, st.RuleIDs)
	if err != nil {
		return worker.CheckState{}, err
	}
	if fp != st.Fingerprint {
		chk.MarkStale()
		st = chk.State()
	}
	return st, nil
}

// readyCheck returns the account's current check when it is the one named, ready and still
// in step with the rules, or answers why not (409 preview_stale for a replaced, gone or
// stale check, 409 check_not_ready while it runs or after it failed, both naming path when
// it is not empty) and returns false.
func (s *server) readyCheck(w http.ResponseWriter, r *http.Request, accountID int64, checkID, path string) (*worker.Check, worker.CheckState, bool) {
	chk := s.Checks.Get(accountID)
	if chk == nil || chk.State().ID != checkID {
		writeError(w, http.StatusConflict, "preview_stale", "This check is no longer current. Run a new check, then sort.", path)
		return nil, worker.CheckState{}, false
	}
	// A ready check may have gone stale: the rules changed since it ran.
	st, err := s.freshState(r.Context(), viewer(r).TenantID, chk)
	if err != nil {
		internalError(w, r, err)
		return nil, worker.CheckState{}, false
	}
	switch st.Status {
	case worker.CheckReady:
		return chk, st, true
	case worker.CheckStale:
		writeError(w, http.StatusConflict, "preview_stale", "The rules changed since this check. Run a new check, then sort.", path)
	default: // running or failed
		writeError(w, http.StatusConflict, "check_not_ready", "This check is not ready to sort yet.", path)
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
	if _, err := s.store.VisibleAccount(r.Context(), viewer(r), in.AccountID); err != nil {
		invalid(w, "account_id", "No such account.")
		return
	}
	if in.CheckID == "" {
		invalid(w, "check_id", "Say which check this selection is for.")
		return
	}
	chk, st, ok := s.readyCheck(w, r, in.AccountID, in.CheckID, "")
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

// cleanupRunItem is one mailbox's part of a run: the check to sort and the rows unticked.
type cleanupRunItem struct {
	AccountID int64  `json:"account_id"`
	CheckID   string `json:"check_id"`
	Exclude   []int  `json:"exclude"`
}

// handleCleanupRun applies finished checks' kept rows, one undoable cleanup batch per
// mailbox. The body names one check (account_id, check_id, exclude: the original shape,
// answered with `batch`) or several (runs, each of that shape), which are started together
// as one manual run (MAI-43): every one is checked and refused before any starts, so the
// run begins on every mailbox or on none. Each check's selectable rows are applied minus
// the ones the user unticked (exclude, row indices), making no model calls. A run is refused
// when a check is not its account's current one, is not ready (still running, failed, or
// stale because the rules changed since), or a Sort is already going for the account.
func (s *server) handleCleanupRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		cleanupRunItem
		Runs []cleanupRunItem `json:"runs"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	multi := in.Runs != nil
	items := in.Runs
	switch {
	case multi && (in.AccountID != 0 || in.CheckID != "" || in.Exclude != nil):
		invalid(w, "runs", "Give runs, or account_id and check_id, not both.")
		return
	case multi && len(in.Runs) == 0:
		invalid(w, "runs", "Name at least one check to sort.")
		return
	case !multi:
		items = []cleanupRunItem{in.cleanupRunItem}
	}
	// at names a field of item i the way the contract does: runs[i].check_id for a run.
	at := func(i int, field string) string {
		if multi {
			return "runs[" + strconv.Itoa(i) + "]." + field
		}
		return field
	}

	ctx := r.Context()
	sorts := make([]worker.SortRun, len(items))
	seen := map[int64]bool{}
	for i, it := range items {
		if _, err := s.store.VisibleAccount(ctx, viewer(r), it.AccountID); err != nil {
			invalid(w, at(i, "account_id"), "No such account.")
			return
		}
		if it.CheckID == "" {
			invalid(w, at(i, "check_id"), "Say which check to sort: run a check first.")
			return
		}
		if seen[it.AccountID] {
			invalid(w, at(i, "account_id"), "A mailbox can be sorted once in a run.")
			return
		}
		seen[it.AccountID] = true
		stalePath := "" // a refused check is named only when there are several
		if multi {
			stalePath = at(i, "check_id")
		}
		_, st, ok := s.readyCheck(w, r, it.AccountID, it.CheckID, stalePath)
		if !ok {
			return
		}
		exclude := map[int]bool{}
		for _, x := range it.Exclude {
			exclude[x] = true
		}
		var rows []composer.CheckRow
		for j, row := range st.Rows {
			if selectableRow(row) && !exclude[j] {
				rows = append(rows, row)
			}
		}
		sorts[i] = worker.SortRun{AccountID: it.AccountID, Folder: st.Folder, Since: st.Since, Limit: st.Limit, Matched: st.Matched, Rows: rows}
	}

	batches, err := s.SortAll(ctx, sorts)
	if errors.Is(err, worker.ErrCleanupRunning) {
		writeError(w, http.StatusConflict, "cleanup_running", "A cleanup is already running for this account. Wait for it to finish.", "")
		return
	}
	if err != nil {
		fail(w, r, err, "account")
		return
	}
	out := make([]batchJSON, len(batches))
	for i, it := range items {
		s.Checks.Take(it.AccountID, it.CheckID) // used: delete it, unless a newer check replaced it
		if out[i], err = s.batchJSON(ctx, viewer(r), batches[i]); err != nil {
			internalError(w, r, err)
			return
		}
	}
	resp := map[string]any{"batches": out}
	if !multi {
		resp["batch"] = out[0]
	}
	writeJSON(w, http.StatusAccepted, resp)
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
	Limit      int            `json:"limit"`    // the most emails the check covers: the newest of its range
	RuleIDs    []int64        `json:"rule_ids"` // the rules the check was limited to; null = every enabled rule
	Status     string         `json:"status"`
	Done       int            `json:"done"`
	Total      int            `json:"total"`
	Matched    int            `json:"matched"` // emails in the range before the limit
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
		Done: st.Done, Total: st.Total, Matched: st.Matched, ModelCalls: st.ModelCalls, Tokens: st.Tokens, CostUSD: st.CostUSD, Error: st.Error, Rows: []checkRowJSON{}, Exclude: append([]int{}, st.Exclude...)}
	out.Limit = st.Limit
	if st.RuleIDs != nil {
		out.RuleIDs = slices.Clone(st.RuleIDs)
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
			j.RuleName = o.Reason // "Sender rule: keep" / "Sender rule: trash" / `Sender rule: move to "Receipts"`
		}
		if o.Asked {
			c := o.Confidence
			j.Confidence = &c
		}
		out.Rows = append(out.Rows, j)
	}
	return out
}

// cleanupDecider is the deciding step of a cleanup check of acct's mail: the routers in
// force, with their calls booked under the "cleanup" purpose so Usage can tell them from
// live sorting, leaving the account's own mail alone as live sorting does.
func (s *server) cleanupDecider(ctx context.Context, v store.Viewer, acct store.Account) (pipeline.Decider, error) {
	src := s.modelSource()
	router, minConfidence := src.Live(ctx, acct.TenantID)
	own, err := pipeline.OwnMail(ctx, s.store, acct)
	return pipeline.Decider{Router: router.For("cleanup"), MinConfidence: minConfidence, Now: s.now(), Own: own,
		Examples: pipeline.Corrections(s.store, v),
		Override: func(ctx context.Context, spec string) *models.Router {
			return src.RouterFor(ctx, acct.TenantID, spec).For("cleanup")
		}}, err
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
	rows, err := s.store.Batches(r.Context(), viewer(r), kind, before, limit+1)
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
		if items[i], err = s.batchJSON(r.Context(), viewer(r), b); err != nil {
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
