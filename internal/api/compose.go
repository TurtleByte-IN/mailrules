package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// ModelSource is how this package reaches the models (settings.Settings implements it):
// the decision router in force, the router of a rule's own model, and the generative model
// the composer writes with.
type ModelSource interface {
	Live(ctx context.Context) (router *models.Router, minConfidence float64)
	RouterFor(ctx context.Context, spec string) *models.Router
	Composer(ctx context.Context) (models.Generator, error)
}

func (s *server) modelSource() ModelSource {
	if s.Models != nil {
		return s.Models
	}
	return s.Settings
}

// noModel answers for a feature that needs a model nobody has configured yet.
func noModel(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "no_composer_model",
		"This needs an AI model, and none is set up yet. Add a Claude (Anthropic) key in Settings, then try again. Rules built from conditions work without one.", "")
}

// modelFail answers for an error from the composer or the tester.
func (s *server) modelFail(w http.ResponseWriter, r *http.Request, err error) {
	if clientGone(w, r) {
		return
	}
	switch {
	case errors.Is(err, settings.ErrNoComposer):
		noModel(w)
	case errors.Is(err, mail.ErrNoFolder):
		invalid(w, "folder", "The mail account has no such folder.")
	case errors.Is(err, composer.ErrModel), errors.Is(err, pipeline.ErrModel):
		slog.WarnContext(r.Context(), "model error", "method", r.Method, "path", r.URL.Path, "error", err.Error())
		writeError(w, http.StatusBadGateway, "model_error", "The AI model could not be reached or gave an answer that cannot be used. Try again.", "")
	default:
		fail(w, r, err, "account")
	}
}

// testDecider is the deciding step for a test run: the routers in force, with their calls
// booked as tests.
func (s *server) testDecider(r *http.Request) pipeline.Decider {
	src := s.modelSource()
	router, minConfidence := src.Live(r.Context())
	return pipeline.Decider{Router: router.For("test"), MinConfidence: minConfidence, Now: s.now(),
		Examples: pipeline.Corrections(s.store, user(r).ID),
		Override: func(ctx context.Context, spec string) *models.Router { return src.RouterFor(ctx, spec).For("test") }}
}

// reader returns the account's connection as the composer and tester may use it: read-only.
func (s *server) reader(accountID int64) (composer.Reader, error) {
	return s.Exec.Accounts.Mailbox(accountID)
}

// composeAccount picks the mailbox drafts are checked against: the one asked for, or else
// the first that is connected. Both results are nil when there is none; ok is false when
// the account asked for does not exist, which has been answered.
func (s *server) composeAccount(w http.ResponseWriter, r *http.Request, id int64) (*store.Account, composer.Reader, bool) {
	if id != 0 {
		a, err := s.store.Account(r.Context(), id)
		if err != nil {
			invalid(w, "account_id", "No such account.")
			return nil, nil, false
		}
		mb, err := s.reader(id)
		if err != nil {
			return &a, nil, true // offline: its folders are still known, the drafts go untested
		}
		return &a, mb, true
	}
	all, err := s.store.Accounts(r.Context())
	if err != nil {
		internalError(w, r, err)
		return nil, nil, false
	}
	for _, a := range all {
		if mb, err := s.reader(a.ID); err == nil {
			return &a, mb, true
		}
	}
	return nil, nil, true
}

// compose runs the composer for a new text, or for one rule to re-optimize.
func (s *server) compose(w http.ResponseWriter, r *http.Request, text string, accountID int64, rule *rules.Rule) (composer.Output, bool) {
	if msg := composer.CheckText(text); msg != "" {
		invalid(w, "text", msg)
		return composer.Output{}, false
	}
	acct, mb, ok := s.composeAccount(w, r, accountID)
	if !ok {
		return composer.Output{}, false
	}
	gen, err := s.modelSource().Composer(r.Context())
	if err != nil {
		s.modelFail(w, r, err)
		return composer.Output{}, false
	}
	c := composer.Composer{Store: s.store, Gen: gen, Now: s.now, BodyChars: s.Settings.Env.BodyChars}
	began := time.Now()
	slog.InfoContext(r.Context(), "compose started", "account", accountID, "reoptimize", rule != nil, "text_chars", len([]rune(text)))
	out, err := c.Compose(r.Context(), composer.Request{UserID: user(r).ID, Text: strings.TrimSpace(text), Rule: rule,
		Account: acct, Mailbox: mb, Decider: s.testDecider(r)})
	s.Hub.Publish(events.UsageUpdated, nil)
	if err != nil {
		if r.Context().Err() == nil {
			slog.WarnContext(r.Context(), "compose failed", "account", accountID, "duration_ms", time.Since(began).Milliseconds(), "error", err.Error())
		}
		s.modelFail(w, r, err)
		return composer.Output{}, false
	}
	slog.InfoContext(r.Context(), "compose finished", "account", accountID, "drafts", len(out.Drafts), "unparsed", len(out.Unparsed),
		"duration_ms", time.Since(began).Milliseconds())
	return out, true
}

// handleCompose turns free text into draft rules to review. Nothing is saved.
func (s *server) handleCompose(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text      string `json:"text"`
		AccountID *int64 `json:"account_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var accountID int64
	if in.AccountID != nil {
		accountID = *in.AccountID
	}
	if out, ok := s.compose(w, r, in.Text, accountID, nil); ok {
		if out.Drafts == nil {
			out.Drafts = []composer.Draft{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": out.Drafts, "unparsed": out.Unparsed})
	}
}

// handleRecompose re-optimizes one rule from its original wording plus new text, and
// answers with a draft to replace it. Nothing is saved.
func (s *server) handleRecompose(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.rule(w, r)
	if !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if out, ok := s.compose(w, r, in.Text, rule.AccountID, &rule); ok {
		writeJSON(w, http.StatusOK, map[string]any{"rule": out.Drafts[0]})
	}
}

// ruleInput is a rule as a client sends it to create or test one (the contract's RuleInput).
type ruleInput struct {
	Name          string         `json:"name"`
	Said          string         `json:"said"`
	Intent        *string        `json:"intent"`
	Conditions    rules.Cond     `json:"conditions"`
	Exceptions    rules.Cond     `json:"exceptions"`
	Actions       []rules.Action `json:"actions"`
	AccountID     *int64         `json:"account_id"`
	Stack         bool           `json:"stack"`
	Model         string         `json:"model"`
	MinConfidence *float64       `json:"min_confidence"`
	Enabled       *bool          `json:"enabled"`
	NewFolders    []string       `json:"new_folders"`
	Position      *int           `json:"position"`
}

// readRules decodes and validates a list of rule inputs, answering 400 itself with a
// sentence for the first problem and its location in path (rules[1].conditions.all[0].op).
func (s *server) readRules(w http.ResponseWriter, r *http.Request, raws []json.RawMessage) ([]ruleInput, []rules.Rule, bool) {
	ins, out := make([]ruleInput, len(raws)), make([]rules.Rule, len(raws))
	for i, raw := range raws {
		at := fmt.Sprintf("rules[%d]", i)
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ins[i]); err != nil {
			invalid(w, at, "This rule has a field of the wrong type or shape, or one that is not known.")
			return nil, nil, false
		}
		in := ins[i]
		rule := rules.Rule{UserID: user(r).ID, Name: strings.TrimSpace(in.Name), Said: in.Said, Conditions: in.Conditions,
			Exceptions: in.Exceptions, Actions: in.Actions, Stack: in.Stack, Model: strings.TrimSpace(in.Model),
			MinConfidence: in.MinConfidence, Enabled: in.Enabled == nil || *in.Enabled}
		if in.Intent != nil {
			rule.Intent = strings.TrimSpace(*in.Intent)
		}
		if in.AccountID != nil {
			if _, err := s.store.Account(r.Context(), *in.AccountID); err != nil {
				invalid(w, at+".account_id", "No such account.")
				return nil, nil, false
			}
			rule.AccountID = *in.AccountID
		}
		if path, msg := ruleProblem(rule); msg != "" {
			writeError(w, http.StatusBadRequest, "rule_invalid", msg, at+"."+path)
			return nil, nil, false
		}
		for j, name := range in.NewFolders {
			if msg := rules.FolderProblem(name); msg != "" {
				writeError(w, http.StatusBadRequest, "rule_invalid", msg, fmt.Sprintf("%s.new_folders[%d]", at, j))
				return nil, nil, false
			}
		}
		out[i] = rule
	}
	return ins, out, true
}

// handleRulesBatch saves rules: approved composer drafts, a rule built by hand in the
// condition builder, or a template. Everything is validated before anything is done; the
// new folders are then created through the executor and the rules inserted in one
// transaction, so one bad rule saves none.
func (s *server) handleRulesBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Rules []json.RawMessage `json:"rules"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Rules) == 0 {
		invalid(w, "rules", "Send at least one rule.")
		return
	}
	ins, rs, ok := s.readRules(w, r, in.Rules)
	if !ok {
		return
	}
	// A rule is known by its name (an import replaces the rule of the same name), so a
	// batch may not bring a name twice, nor one a saved rule has.
	saved, err := s.store.Rules(r.Context(), user(r).ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	for i, rule := range rs {
		at := fmt.Sprintf("rules[%d].name", i)
		switch same := func(x rules.Rule) bool { return x.Name == rule.Name }; {
		case slices.ContainsFunc(rs[:i], same):
			writeError(w, http.StatusBadRequest, "rule_invalid", fmt.Sprintf("Two of these rules are named %q. Give each rule its own name.", rule.Name), at)
			return
		case slices.ContainsFunc(saved, same):
			writeError(w, http.StatusBadRequest, "rule_invalid", fmt.Sprintf("There is already a rule named %q. Choose another name.", rule.Name), at)
			return
		}
	}
	accounts, err := s.store.Accounts(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	added := make([]store.NewRule, len(rs))
	for i, rule := range rs {
		added[i] = store.NewRule{Rule: rule, Position: ins[i].Position}
		// A rule for every account needs its folders on each of them. An account that is
		// offline is passed over: the executor creates a missing folder on the first move.
		for _, a := range accounts {
			if rule.AccountID != 0 && rule.AccountID != a.ID {
				continue
			}
			if err := s.Exec.EnsureFolders(r.Context(), a.ID, ins[i].NewFolders); err != nil && !errors.Is(err, worker.ErrNotConnected) {
				fail(w, r, err, "account")
				return
			}
		}
	}
	created, err := s.store.CreateRules(r.Context(), user(r).ID, added, s.now().Unix())
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.Hub.Publish(events.RulesChanged, nil)
	items := make([]ruleJSON, len(created))
	for i, rule := range created {
		items[i] = toRuleJSON(rule, store.RuleStat{})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"items": items})
}

// progressSteps is about how many progress events a test sends, however many emails it
// reads: a 20-email run reports every email, a 2,000-email run every 20th.
const progressSteps = 100

// wantsStream reports whether the client asked for an event stream: its Accept header
// names text/event-stream. A client that does not gets one JSON body.
func wantsStream(r *http.Request) bool {
	for _, part := range strings.Split(strings.Join(r.Header.Values("Accept"), ","), ",") {
		if mediaType, _, _ := strings.Cut(part, ";"); strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream") {
			return true
		}
	}
	return false
}

// eventStream writes server-sent events. Its headers go out with the first event, so a run
// that fails before it has anything to report is still answered as plain JSON.
type eventStream struct {
	w       http.ResponseWriter
	started bool
}

// send writes one event and flushes it.
func (e *eventStream) send(event string, v any) {
	if !e.started {
		h := e.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		e.started = true
	}
	data, _ := json.Marshal(v) // the values sent here always encode
	_, _ = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, data)
	_ = http.NewResponseController(e.w).Flush()
}

// handleRulesTest runs saved or draft rules over an account's recent mail and reports
// what each email would get. It reads the mailbox and changes nothing. A client that
// accepts text/event-stream gets progress events, from 0 of N once the mail is listed,
// then the result; any other client gets the result as one JSON body.
func (s *server) handleRulesTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID int64             `json:"account_id"`
		RuleIDs   []int64           `json:"rule_ids"`
		Rules     []json.RawMessage `json:"rules"`
		Folder    string            `json:"folder"`
		Limit     *int              `json:"limit"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	if in.AccountID == 0 {
		invalid(w, "account_id", "An account is required: say which mailbox to test the rules on.")
		return
	}
	acct, err := s.store.Account(ctx, in.AccountID)
	if err != nil {
		invalid(w, "account_id", "No such account.")
		return
	}
	limit := composer.DefaultLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	if limit < 1 || limit > composer.MaxLimit {
		invalid(w, "limit", fmt.Sprintf("The limit must be between 1 and %d.", composer.MaxLimit))
		return
	}
	if in.Folder == "" {
		in.Folder = "INBOX"
	}

	// The rule set. A request that names rules (saved ones by id, drafts inline, or both)
	// tests exactly those and nothing else, each switched on so a rule can be tried before
	// it is enabled. Only a request that names none tests the saved set as it is, sender
	// rules included.
	saved, err := s.store.Rules(ctx, user(r).ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	var senders []rules.SenderRule
	if in.RuleIDs == nil && len(in.Rules) == 0 {
		if senders, err = s.store.SenderRules(ctx, user(r).ID); err != nil {
			internalError(w, r, err)
			return
		}
	} else {
		for _, id := range in.RuleIDs {
			if !slices.ContainsFunc(saved, func(x rules.Rule) bool { return x.ID == id }) {
				invalid(w, "rule_ids", fmt.Sprintf("There is no rule %d.", id))
				return
			}
		}
		saved = slices.DeleteFunc(saved, func(x rules.Rule) bool { return !slices.Contains(in.RuleIDs, x.ID) })
		for i := range saved {
			saved[i].Enabled = true
		}
	}
	_, drafts, ok := s.readRules(w, r, in.Rules)
	if !ok {
		return
	}
	last := 0
	for _, x := range saved {
		last = max(last, x.Priority)
	}
	for i := range drafts {
		drafts[i].ID, drafts[i].Priority, drafts[i].Enabled = -int64(i+1), last+i+1, true
	}
	set := append(saved, drafts...)

	decider := s.testDecider(r)
	if decider.Router == nil && slices.ContainsFunc(set, func(x rules.Rule) bool { return x.Enabled && x.Intent != "" }) {
		noModel(w)
		return
	}
	mb, err := s.reader(acct.ID)
	if err != nil {
		fail(w, r, err, "account")
		return
	}
	t := composer.Tester{Store: s.store, Mailbox: mb, AccountID: acct.ID, Decider: decider, BodyChars: s.Settings.Env.BodyChars}
	stream := wantsStream(r)
	began := time.Now()
	slog.InfoContext(ctx, "rule test started", "account", acct.ID, "folder", in.Folder, "limit", limit,
		"rules", len(set), "sender_rules", len(senders), "stream", stream)

	// The headers of a stream go out with the first event, so a run that fails before the
	// mail is listed (no such folder) is still answered as plain JSON.
	es := &eventStream{w: w}
	var seen composer.Progress
	tenth := -1
	res, err := t.Run(ctx, set, senders, in.Folder, limit, func(p composer.Progress) {
		seen = p
		if stream && (p.Done%max(1, p.Total/progressSteps) == 0 || p.Done == p.Total) {
			es.send("progress", map[string]int{"done": p.Done, "total": p.Total, "model_calls": p.ModelCalls})
		}
		if at := p.Done * 10 / max(1, p.Total); at != tenth {
			tenth = at
			slog.DebugContext(ctx, "rule test progress", "account", acct.ID, "done", p.Done, "total", p.Total, "model_calls", p.ModelCalls,
				"duration_ms", time.Since(began).Milliseconds())
		}
	})
	if seen.ModelCalls > 0 {
		s.Hub.Publish(events.UsageUpdated, nil) // the stats screens may show the calls the test booked
	}
	took := time.Since(began).Milliseconds()
	switch {
	case err == nil:
		slog.InfoContext(ctx, "rule test finished", "account", acct.ID, "folder", in.Folder, "limit", limit, "rules", len(set),
			"tested", res.Tested, "matched", res.Matched, "model_calls", res.ModelCalls, "cost_usd", res.CostUSD, "duration_ms", took)
		if stream {
			es.send("done", res)
		} else {
			writeJSON(w, http.StatusOK, res)
		}
	case ctx.Err() != nil: // the client dropped the request, and nobody is left to answer
		slog.InfoContext(ctx, fmt.Sprintf("rule test cancelled by the client after %d of %d", seen.Done, seen.Total),
			"account", acct.ID, "folder", in.Folder, "limit", limit, "done", seen.Done, "total", seen.Total,
			"model_calls", seen.ModelCalls, "duration_ms", took)
		if !es.started {
			w.WriteHeader(statusClientClosed)
		}
	default:
		slog.WarnContext(ctx, "rule test failed", "account", acct.ID, "folder", in.Folder, "limit", limit, "done", seen.Done,
			"total", seen.Total, "model_calls", seen.ModelCalls, "duration_ms", took, "error", err.Error())
		if es.started {
			es.send("error", map[string]apiError{"error": {Code: "test_failed", Message: "The test stopped: the mail server or the AI model failed. Try again."}})
		} else {
			s.modelFail(w, r, err)
		}
	}
}
