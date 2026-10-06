package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// week is the window of a rule's hit count.
const week = 7 * 24 * 3600

type ruleJSON struct {
	ID            int64          `json:"id"`
	AccountID     *int64         `json:"account_id"` // null = every account
	Name          string         `json:"name"`
	Said          string         `json:"said"`
	Intent        string         `json:"intent"`
	Conditions    rules.Cond     `json:"conditions"`
	Exceptions    rules.Cond     `json:"exceptions"`
	Actions       []rules.Action `json:"actions"`
	Priority      int            `json:"priority"`
	Stack         bool           `json:"stack"`
	Model         string         `json:"model"`
	MinConfidence *float64       `json:"min_confidence"`
	Enabled       bool           `json:"enabled"`
	Version       int            `json:"version"`
	CreatedAt     int64          `json:"created_at"`
	UpdatedAt     int64          `json:"updated_at"`
	HitsWeek      int            `json:"hits_week"`
	LastMatchAt   *int64         `json:"last_match_at"`
}

func toRuleJSON(r rules.Rule, st store.RuleStat) ruleJSON {
	return ruleJSON{ID: r.ID, AccountID: ts(r.AccountID), Name: r.Name, Said: r.Said, Intent: r.Intent,
		Conditions: r.Conditions, Exceptions: r.Exceptions, Actions: r.Actions, Priority: r.Priority, Stack: r.Stack,
		Model: r.Model, MinConfidence: r.MinConfidence, Enabled: r.Enabled, Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, HitsWeek: st.Hits, LastMatchAt: ts(st.LastMatchAt)}
}

// rulesJSON lists the user's rules in priority order with this week's hit counts.
func (s *server) rulesJSON(r *http.Request) ([]ruleJSON, error) {
	rs, err := s.store.Rules(r.Context(), user(r).ID)
	if err != nil {
		return nil, err
	}
	stats, err := s.store.RuleStats(r.Context(), user(r).ID, s.now().Unix()-week)
	if err != nil {
		return nil, err
	}
	out := make([]ruleJSON, len(rs))
	for i, rule := range rs {
		out[i] = toRuleJSON(rule, stats[rule.ID])
	}
	return out, nil
}

func (s *server) writeRules(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	items, err := s.rulesJSON(r)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if extra == nil {
		extra = map[string]any{}
	}
	extra["items"] = items
	writeJSON(w, http.StatusOK, extra)
}

func (s *server) handleRules(w http.ResponseWriter, r *http.Request) { s.writeRules(w, r, nil) }

// rule loads the rule a path names, answering 404 itself.
func (s *server) rule(w http.ResponseWriter, r *http.Request) (rules.Rule, bool) {
	id, ok := pathID(w, r, "id", "rule")
	if !ok {
		return rules.Rule{}, false
	}
	rule, err := s.store.Rule(r.Context(), user(r).ID, id)
	if err != nil {
		fail(w, r, err, "rule")
		return rules.Rule{}, false
	}
	return rule, true
}

func (s *server) writeRule(w http.ResponseWriter, r *http.Request, id int64) {
	rule, err := s.store.Rule(r.Context(), user(r).ID, id)
	if err != nil {
		fail(w, r, err, "rule")
		return
	}
	stats, err := s.store.RuleStats(r.Context(), user(r).ID, s.now().Unix()-week)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rule": toRuleJSON(rule, stats[id])})
}

func (s *server) handleRule(w http.ResponseWriter, r *http.Request) {
	if rule, ok := s.rule(w, r); ok {
		s.writeRule(w, r, rule.ID)
	}
}

// ruleInvalid answers for a rule that does not pass validation, with the path of the problem.
func ruleInvalid(w http.ResponseWriter, err error) {
	var ve *rules.ValidationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, "rule_invalid", err.Error(), ve.Path)
		return
	}
	writeError(w, http.StatusBadRequest, "rule_invalid", err.Error(), "")
}

func (s *server) handleRulePatch(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.rule(w, r)
	if !ok {
		return
	}
	accountID := ts(rule.AccountID)
	sent, ok := readPatch(w, r, map[string]any{
		"name": &rule.Name, "said": &rule.Said, "intent": &rule.Intent, "conditions": &rule.Conditions,
		"exceptions": &rule.Exceptions, "actions": &rule.Actions, "account_id": &accountID, "stack": &rule.Stack,
		"model": &rule.Model, "min_confidence": &rule.MinConfidence, "enabled": &rule.Enabled,
	})
	if !ok {
		return
	}
	if rule.AccountID = 0; accountID != nil {
		rule.AccountID = *accountID
	}
	if sent["account_id"] && rule.AccountID != 0 {
		if _, err := s.store.Account(r.Context(), rule.AccountID); err != nil {
			invalid(w, "account_id", "No such account.")
			return
		}
	}
	rule.Name = strings.TrimSpace(rule.Name)
	if err := rule.Validate(); err != nil {
		ruleInvalid(w, err)
		return
	}
	if err := s.store.UpdateRule(r.Context(), rule, s.now().Unix()); err != nil {
		fail(w, r, err, "rule")
		return
	}
	s.Hub.Publish(events.RulesChanged, nil)
	s.writeRule(w, r, rule.ID)
}

// handleRuleDelete removes a rule. What it decided in the past stays in the activity
// feed under the name it had.
func (s *server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.rule(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteRule(r.Context(), user(r).ID, rule.ID); err != nil {
		fail(w, r, err, "rule")
		return
	}
	s.Hub.Publish(events.RulesChanged, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRulesReorder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	err := s.store.ReorderRules(r.Context(), user(r).ID, in.IDs)
	if errors.Is(err, store.ErrRuleSet) {
		invalid(w, "ids", "List every rule's id exactly once, in the new order.")
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.Hub.Publish(events.RulesChanged, nil)
	s.writeRules(w, r, nil)
}

func (s *server) handleRulesExport(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.Rules(r.Context(), user(r).ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	out, err := rules.MarshalYAML(rules.File{Rules: rs})
	if err != nil {
		internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="mailrules-rules.yaml"`)
	_, _ = w.Write(out)
}

// handleRulesImport takes a rules YAML file as the request body. Nothing is stored unless
// every rule in it is valid.
func (s *server) handleRulesImport(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "A rules file can be at most 1 MB.", "")
		return
	}
	f, err := rules.ParseYAML(data)
	if err != nil {
		ruleInvalid(w, err)
		return
	}
	created, updated, err := s.store.ImportRules(r.Context(), user(r).ID, f.Rules, s.now().Unix())
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.Hub.Publish(events.RulesChanged, nil)
	s.writeRules(w, r, map[string]any{"created": created, "updated": updated})
}

// since reads the required ?since= unix time of the undo-since endpoints.
func since(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil || v <= 0 {
		invalid(w, "since", "Give ?since= as a unix time in seconds: everything from then on is undone.")
		return 0, false
	}
	return v, true
}

// handleRuleUndo undoes everything this rule did since ?since=, as one undo batch.
func (s *server) handleRuleUndo(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.rule(w, r)
	if !ok {
		return
	}
	if from, ok := since(w, r); ok {
		s.undoSince(w, r, rule.ID, from)
	}
}
