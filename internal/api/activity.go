package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// maxSnippet is the most message text the API ever returns.
const maxSnippet = 200

type decisionJSON struct {
	ID         int64   `json:"id"`
	Stage      string  `json:"stage"`
	RuleID     *int64  `json:"rule_id"` // null = no rule, or the rule has been deleted (rule_name still says which)
	RuleName   string  `json:"rule_name"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	Model      string  `json:"model"`
	TokensIn   int     `json:"tokens_in"`
	TokensOut  int     `json:"tokens_out"`
	CostUSD    float64 `json:"cost_usd"`
	LatencyMS  int64   `json:"latency_ms"`
	CreatedAt  int64   `json:"created_at"`
}

type actionJSON struct {
	ID         int64  `json:"id"`
	MessageID  int64  `json:"message_id"`
	AccountID  int64  `json:"account_id"`
	DecisionID *int64 `json:"decision_id"`
	BatchID    *int64 `json:"batch_id"`
	Kind       string `json:"kind"`
	Folder     string `json:"folder"` // the destination a move names; empty otherwise
	FromFolder string `json:"from_folder"`
	ToFolder   string `json:"to_folder"` // where the message was afterwards; empty unless the action was done
	Status     string `json:"status"`
	Error      string `json:"error"`
	CreatedAt  int64  `json:"created_at"`
	UndoneAt   *int64 `json:"undone_at"`
}

// activityJSON is one row of the feed: an email (never its body: at most a snippet), the
// decision made for it and what was done.
type activityJSON struct {
	ID            int64         `json:"id"`
	AccountID     int64         `json:"account_id"`
	From          string        `json:"from"`
	FromDomain    string        `json:"from_domain"`
	Subject       string        `json:"subject"`
	Snippet       string        `json:"snippet"`
	ReceivedAt    *int64        `json:"received_at"`
	CreatedAt     int64         `json:"created_at"`
	HasAttachment bool          `json:"has_attachment"`
	State         string        `json:"state"`
	Decision      *decisionJSON `json:"decision"`
	Actions       []actionJSON  `json:"actions"`
	Undoable      bool          `json:"undoable"` // at least one action is in effect and can be undone
	// Correction is the user's latest word on this email, which overrules Decision.
	Correction *correctionJSON `json:"correction"`
}

type correctionJSON struct {
	RuleID    *int64 `json:"rule_id"` // null = keep in the inbox
	RuleName  string `json:"rule_name"`
	CreatedAt int64  `json:"created_at"`
}

func toDecisionJSON(d store.Decision) decisionJSON {
	name := d.RuleName
	if d.RuleID == 0 && d.RuleVersion != 0 && name == "" {
		name = "Deleted rule" // decided before rule names were kept with the decision
	}
	return decisionJSON{ID: d.ID, Stage: d.Stage, RuleID: ts(d.RuleID), RuleName: name, Confidence: d.Confidence, Reason: d.Reason,
		Model: d.Model, TokensIn: d.TokensIn, TokensOut: d.TokensOut, CostUSD: d.CostUSD, LatencyMS: d.LatencyMS, CreatedAt: d.CreatedAt}
}

func toActionJSON(a store.Action) actionJSON {
	out := actionJSON{ID: a.ID, MessageID: a.MessageID, AccountID: a.AccountID, DecisionID: ts(a.DecisionID), BatchID: ts(a.BatchID),
		Kind: a.Kind, Folder: a.Folder, FromFolder: a.Before.Folder, Status: a.Status, Error: a.Error,
		CreatedAt: a.CreatedAt, UndoneAt: ts(a.UndoneAt)}
	if a.After != nil {
		out.ToFolder = a.After.Folder
	}
	return out
}

// activityJSON shapes a feed row. A row the user corrected has an action no decision led
// to; only then is the correction looked up.
func (s *server) activityJSON(ctx context.Context, row store.ActivityRow) activityJSON {
	m := row.Message
	out := activityJSON{ID: m.ID, AccountID: m.AccountID, From: m.FromAddr, FromDomain: m.FromDomain, Subject: m.Subject,
		Snippet: m.Snippet, ReceivedAt: ts(m.ReceivedAt), CreatedAt: m.CreatedAt, HasAttachment: m.HasAttachment, State: m.State,
		Actions: make([]actionJSON, 0, len(row.Actions))}
	if r := []rune(out.Snippet); len(r) > maxSnippet {
		out.Snippet = string(r[:maxSnippet])
	}
	if row.Decision != nil {
		d := toDecisionJSON(*row.Decision)
		out.Decision = &d
	}
	for _, a := range row.Actions {
		out.Actions = append(out.Actions, toActionJSON(a))
		out.Undoable = out.Undoable || a.Status == store.ActionDone && a.Kind != actions.KindReview
	}
	if slices.ContainsFunc(row.Actions, func(a store.Action) bool { return a.DecisionID == 0 }) {
		if cs, err := s.store.MessageCorrections(ctx, m.ID); err == nil && len(cs) > 0 {
			c := cs[len(cs)-1]
			out.Correction = &correctionJSON{RuleID: ts(c.RightRuleID), RuleName: c.RightRule, CreatedAt: c.CreatedAt}
		}
	}
	return out
}

var (
	stages = []string{"sender", "condition", "decider", "fallback", "none"}
	states = []string{store.StateNew, store.StateDecided, store.StateActed, store.StateReview, store.StateSkipped, store.StateError}
	kinds  = []string{rules.ActMove, rules.ActArchive, rules.ActTrash, rules.ActJunk, rules.ActFlag, rules.ActUnflag,
		rules.ActRead, rules.ActUnread, rules.ActKeep, actions.KindReview}
)

// activityFilter reads the list parameters shared by the feed and Needs review:
// ?account=&rule=&stage=&status=&action=&cursor=&limit=.
func activityFilter(w http.ResponseWriter, r *http.Request) (store.ActivityFilter, bool) {
	q := r.URL.Query()
	f := store.ActivityFilter{Stage: q.Get("stage"), State: q.Get("status"), Action: q.Get("action"), Limit: 50}
	for name, dst := range map[string]*int64{"account": &f.AccountID, "rule": &f.RuleID, "cursor": &f.Before} {
		if v := q.Get(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				invalid(w, name, "The "+name+" must be a positive integer.")
				return f, false
			}
			*dst = n
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			invalid(w, "limit", "The limit must be between 1 and 100.")
			return f, false
		}
		f.Limit = n
	}
	for name, c := range map[string]struct {
		got  string
		want []string
	}{"stage": {f.Stage, stages}, "status": {f.State, states}, "action": {f.Action, kinds}} {
		if c.got != "" && !slices.Contains(c.want, c.got) {
			invalid(w, name, "Unknown "+name+".")
			return f, false
		}
	}
	return f, true
}

// page lists one page of the feed. It asks for one row more than the page holds: when that
// row exists there is a next page, and the cursor is the id of the last row shown.
func (s *server) page(w http.ResponseWriter, r *http.Request, f store.ActivityFilter) (map[string]any, bool) {
	limit := f.Limit
	f.Limit++
	rows, err := s.store.Activity(r.Context(), f)
	if err != nil {
		internalError(w, r, err)
		return nil, false
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := strconv.FormatInt(rows[limit-1].Message.ID, 10)
		next = &c
	}
	items := make([]activityJSON, len(rows))
	for i, row := range rows {
		items[i] = s.activityJSON(r.Context(), row)
	}
	return map[string]any{"items": items, "next_cursor": next}, true
}

func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	f, ok := activityFilter(w, r)
	if !ok {
		return
	}
	if body, ok := s.page(w, r, f); ok {
		writeJSON(w, http.StatusOK, body)
	}
}

// handleReview lists the mail waiting in Needs review, newest first; each row's decision
// names the rule the model would have picked. total is the whole queue, for the nav badge.
func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	f, ok := activityFilter(w, r)
	if !ok {
		return
	}
	f.State = store.StateReview
	body, ok := s.page(w, r, f)
	if !ok {
		return
	}
	total, err := s.store.CountMessages(r.Context(), store.StateReview)
	if err != nil {
		internalError(w, r, err)
		return
	}
	body["total"] = total
	writeJSON(w, http.StatusOK, body)
}

type signalsJSON struct {
	Bulk          bool   `json:"bulk"`
	Noreply       bool   `json:"noreply"`
	IsContact     bool   `json:"is_contact"`
	RepliedBefore bool   `json:"replied_before"`
	DMARC         string `json:"dmarc"`
}

// traceStep is one line of the "Why this happened" panel.
type traceStep struct {
	Kind       string   `json:"kind"` // a decision stage, or action | correction
	Label      string   `json:"label"`
	Detail     string   `json:"detail"`
	RuleID     *int64   `json:"rule_id"`
	RuleName   string   `json:"rule_name"`
	Confidence *float64 `json:"confidence"`
	Model      string   `json:"model"`
	TokensIn   int      `json:"tokens_in"`
	TokensOut  int      `json:"tokens_out"`
	CostUSD    float64  `json:"cost_usd"`
	LatencyMS  int64    `json:"latency_ms"`
	Status     string   `json:"status"` // an action's status; empty for other kinds
	At         int64    `json:"at"`
	Active     bool     `json:"active"` // the step that settled where the email is now
	// Candidates is what the decision model gave each rule it chose between, likeliest
	// first; empty when it gives no such spread, and for steps that are not a decision.
	Candidates []candidateJSON `json:"candidates"`
}

type candidateJSON struct {
	RuleID      *int64  `json:"rule_id"` // null = none of the rules
	RuleName    string  `json:"rule_name"`
	Probability float64 `json:"probability"`
}

// candidates lists a decision's per-rule probabilities, likeliest first.
func (s *server) candidates(ctx context.Context, userID int64, d store.Decision) []candidateJSON {
	out := make([]candidateJSON, 0, len(d.Probabilities))
	if len(d.Probabilities) == 0 {
		return out
	}
	names := map[int64]string{}
	rs, _ := s.store.Rules(ctx, userID) // without the names the ids still say which rule
	for _, r := range rs {
		names[r.ID] = r.Name
	}
	for id, p := range d.Probabilities {
		out = append(out, candidateJSON{RuleID: ts(id), RuleName: names[id], Probability: p})
	}
	slices.SortFunc(out, func(a, b candidateJSON) int {
		return cmp.Or(cmp.Compare(b.Probability, a.Probability), cmp.Compare(a.RuleName, b.RuleName))
	})
	return out
}

type messageJSON struct {
	activityJSON
	To            []string    `json:"to"`
	ListID        string      `json:"list_id"`
	Size          int64       `json:"size"`
	Signals       signalsJSON `json:"signals"`
	Folder        string      `json:"folder"`         // where it arrived
	CurrentFolder string      `json:"current_folder"` // where MailRules last left it
	Attempts      int         `json:"attempts"`
	NextAttemptAt *int64      `json:"next_attempt_at"`
	Trace         []traceStep `json:"trace"`
}

var stageLabels = map[string]string{"sender": "Sender rules", "condition": "Conditions", "decider": "Decision model",
	"fallback": "Fallback model", "none": "No rule"}

var actionWords = map[string]string{rules.ActArchive: "Archived", rules.ActTrash: "Moved to Trash", rules.ActJunk: "Moved to Junk",
	rules.ActFlag: "Flagged", rules.ActUnflag: "Unflagged", rules.ActRead: "Marked read", rules.ActUnread: "Marked unread",
	rules.ActKeep: "Kept in the inbox", actions.KindReview: "Tagged for review"}

func actionDetail(a store.Action) string {
	text := actionWords[a.Kind]
	if a.Kind == rules.ActMove {
		text = "Moved to " + a.Folder
	}
	switch a.Status {
	case store.ActionDryRun:
		text += " (dry run: nothing was changed)"
	case store.ActionFailed:
		text += " (failed: " + a.Error + ")"
	case store.ActionUndone:
		text += " (undone)"
	}
	return text
}

// trace lays out, oldest first, every decision made for a message, every action taken on
// it and every correction the user made.
func (s *server) trace(ctx context.Context, userID int64, row store.ActivityRow) ([]traceStep, error) {
	decisions, err := s.store.MessageDecisions(ctx, row.Message.ID)
	if err != nil {
		return nil, err
	}
	corrections, err := s.store.MessageCorrections(ctx, row.Message.ID)
	if err != nil {
		return nil, err
	}
	steps := []traceStep{}
	for _, d := range decisions {
		dj := toDecisionJSON(d)
		step := traceStep{Kind: d.Stage, Label: stageLabels[d.Stage], Detail: d.Reason, RuleID: dj.RuleID, RuleName: dj.RuleName,
			Model: d.Model, TokensIn: d.TokensIn, TokensOut: d.TokensOut, CostUSD: d.CostUSD, LatencyMS: d.LatencyMS, At: d.CreatedAt,
			Candidates: s.candidates(ctx, userID, d)}
		if d.Stage != "none" || d.Model != "" {
			step.Confidence = &d.Confidence
		}
		steps = append(steps, step)
	}
	for _, a := range row.Actions {
		steps = append(steps, traceStep{Kind: "action", Label: "Action", Detail: actionDetail(a), Status: a.Status, At: a.CreatedAt, Candidates: []candidateJSON{}})
	}
	for _, c := range corrections {
		step := traceStep{Kind: "correction", Label: "Your correction", Detail: "Kept in the inbox", RuleID: ts(c.RightRuleID), At: c.CreatedAt,
			Candidates: []candidateJSON{}}
		if c.RightRuleID != 0 {
			step.RuleName = c.RightRule
			step.Detail = "You chose " + c.RightRule
		}
		steps = append(steps, step)
	}
	// A correction is recorded after the actions it led to, so on equal times it stays last.
	slices.SortStableFunc(steps, func(a, b traceStep) int { return cmp.Compare(a.At, b.At) })
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Kind != "action" {
			steps[i].Active = true
			break
		}
	}
	return steps, nil
}

func (s *server) handleMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "message")
	if !ok {
		return
	}
	row, err := s.store.ActivityFor(r.Context(), id)
	if err != nil {
		fail(w, r, err, "message")
		return
	}
	trace, err := s.trace(r.Context(), user(r).ID, row)
	if err != nil {
		internalError(w, r, err)
		return
	}
	m := row.Message
	writeJSON(w, http.StatusOK, map[string]any{"message": messageJSON{
		activityJSON: s.activityJSON(r.Context(), row), To: append([]string{}, m.ToAddrs...), ListID: m.ListID, Size: m.Size,
		Signals: signalsJSON{m.Signals.Bulk, m.Signals.Noreply, m.Signals.Contact, m.Signals.RepliedBefore, m.Signals.DMARC},
		Folder:  m.Folder, CurrentFolder: m.Location().Folder, Attempts: m.Attempts, NextAttemptAt: ts(m.NextAttemptAt), Trace: trace,
	}})
}

// correct is the body of both fix endpoints: file the message under another rule, or keep
// it in the inbox (rule_id null).
func (s *server) correct(w http.ResponseWriter, r *http.Request, messageID int64) {
	var in struct {
		RuleID          *int64 `json:"rule_id"`
		AlwaysForSender bool   `json:"always_for_sender"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	c := actions.Correction{MessageID: messageID, AlwaysForSender: in.AlwaysForSender}
	if in.RuleID != nil {
		if _, err := s.store.Rule(r.Context(), user(r).ID, *in.RuleID); err != nil {
			invalid(w, "rule_id", "No such rule.")
			return
		}
		c.RightRuleID = *in.RuleID
	}
	batchID, err := s.Exec.Correct(r.Context(), c)
	if err != nil {
		fail(w, r, err, "message")
		return
	}
	row, err := s.store.ActivityFor(r.Context(), messageID)
	if err != nil {
		fail(w, r, err, "message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": batchID, "item": s.activityJSON(r.Context(), row)})
}

// handleMessageUndo undoes everything still in effect on one email, newest first, as one
// undo batch, and answers with the row as it is now. An action that cannot be undone does
// not stop the others; only when none could be undone is the request refused, with the
// reason of the first (409 message_gone when the email is gone).
func (s *server) handleMessageUndo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "message")
	if !ok {
		return
	}
	if _, err := s.store.Message(r.Context(), id); err != nil {
		fail(w, r, err, "message")
		return
	}
	batchID, undone, failed, why, err := s.Exec.UndoMessage(r.Context(), id)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if undone == 0 && failed > 0 {
		fail(w, r, why, "message")
		return
	}
	row, err := s.store.ActivityFor(r.Context(), id)
	if err != nil {
		fail(w, r, err, "message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": batchID, "item": s.activityJSON(r.Context(), row), "undone": undone, "failed": failed})
}

func (s *server) handleCorrect(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "message")
	if !ok {
		return
	}
	if _, err := s.store.Message(r.Context(), id); err != nil {
		fail(w, r, err, "message")
		return
	}
	s.correct(w, r, id)
}

// handleReviewResolve settles a message waiting in Needs review: approve the suggested
// rule, choose another, or keep it in the inbox.
func (s *server) handleReviewResolve(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "message_id", "message")
	if !ok {
		return
	}
	m, err := s.store.Message(r.Context(), id)
	if err != nil {
		fail(w, r, err, "message")
		return
	}
	if m.State != store.StateReview {
		writeError(w, http.StatusConflict, "not_in_review", "This message is not waiting in Needs review.", "")
		return
	}
	s.correct(w, r, id)
}

func (s *server) handleActionUndo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "action")
	if !ok {
		return
	}
	if err := s.Exec.Undo(r.Context(), id); err != nil {
		fail(w, r, err, "action")
		return
	}
	a, err := s.store.Action(r.Context(), id)
	if err != nil {
		fail(w, r, err, "action")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": toActionJSON(a)})
}

type batchJSON struct {
	ID        int64          `json:"id"`
	Kind      string         `json:"kind"`
	Status    string         `json:"status"`
	Total     *int64         `json:"total"` // null = not counted up front (live batches)
	Done      int            `json:"done"`
	CreatedAt int64          `json:"created_at"`
	Actions   map[string]int `json:"actions"` // how many of the batch's own actions have each status
	// What a cleanup batch sorted and what its model calls have cost so far; null, empty
	// and zero for the other kinds.
	AccountID *int64  `json:"account_id"`
	Folder    string  `json:"folder"`
	Since     *int64  `json:"since"`
	Tokens    int     `json:"tokens"`
	CostUSD   float64 `json:"cost_usd"`
}

// batchJSON adds the status counts of the batch's actions.
func (s *server) batchJSON(ctx context.Context, b store.Batch) (batchJSON, error) {
	out := batchJSON{ID: b.ID, Kind: b.Kind, Status: b.Status, Total: ts(int64(b.Total)), Done: b.Done, CreatedAt: b.CreatedAt,
		AccountID: ts(b.AccountID), Folder: b.Folder, Since: ts(b.Since), Tokens: b.Tokens, CostUSD: b.CostUSD}
	if b.Kind == store.BatchCleanup {
		total := int64(b.Total) // counted up front, so 0 means an empty selection
		out.Total = &total
	}
	var err error
	out.Actions, err = s.store.BatchActionCounts(ctx, b.ID)
	return out, err
}

// writeBatch answers with a batch, and after an undo with how it went.
func (s *server) writeBatch(w http.ResponseWriter, r *http.Request, id int64, extra map[string]any) {
	b, err := s.store.Batch(r.Context(), id)
	if err != nil {
		fail(w, r, err, "batch")
		return
	}
	bj, err := s.batchJSON(r.Context(), b)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if extra == nil {
		extra = map[string]any{}
	}
	extra["batch"] = bj
	writeJSON(w, http.StatusOK, extra)
}

func (s *server) handleBatch(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r, "id", "batch"); ok {
		s.writeBatch(w, r, id, nil)
	}
}

// handleBatchUndo undoes a batch's actions, newest first. One that cannot be undone (its
// message is gone) does not stop the rest: the answer counts both.
func (s *server) handleBatchUndo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "batch")
	if !ok {
		return
	}
	if _, err := s.store.Batch(r.Context(), id); err != nil {
		fail(w, r, err, "batch")
		return
	}
	undone, undoErr := s.Exec.UndoBatch(r.Context(), id)
	acts, err := s.store.BatchActions(r.Context(), id)
	if err != nil {
		internalError(w, r, err)
		return
	}
	failed := 0
	for _, a := range acts {
		if a.Status == store.ActionDone {
			failed++
		}
	}
	if undoErr != nil && failed == 0 { // not an action that would not undo: the database, say
		internalError(w, r, undoErr)
		return
	}
	s.writeBatch(w, r, id, map[string]any{"undone": undone, "failed": failed})
}

// undoSince undoes everything done since a time, by one rule or (ruleID 0) by all of
// them, and answers with the undo batch it was recorded as.
func (s *server) undoSince(w http.ResponseWriter, r *http.Request, ruleID, from int64) {
	batchID, undone, failed, err := s.Exec.UndoSince(r.Context(), ruleID, from)
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.writeBatch(w, r, batchID, map[string]any{"undone": undone, "failed": failed})
}

// handleUndoSince is "undo the last hour": everything done since ?since=.
func (s *server) handleUndoSince(w http.ResponseWriter, r *http.Request) {
	if from, ok := since(w, r); ok {
		s.undoSince(w, r, 0, from)
	}
}
