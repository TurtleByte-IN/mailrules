package api

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// statsRanges is how many UTC days each range covers, today included.
var statsRanges = map[string]int{"day": 1, "week": 7, "month": 30}

// statsSince reads ?range= and returns it with the start of its first UTC day. The cost
// ledger is kept per UTC day, so ranges start on a day boundary and the email counts and
// the costs cover exactly the same time.
func (s *server) statsSince(w http.ResponseWriter, r *http.Request, def string, allowed ...string) (name string, since time.Time, ok bool) {
	name = cmp.Or(r.URL.Query().Get("range"), def)
	if !slices.Contains(allowed, name) {
		invalid(w, "range", "Unknown range.")
		return "", time.Time{}, false
	}
	today := s.now().UTC().Truncate(24 * time.Hour)
	return name, today.AddDate(0, 0, 1-statsRanges[name]), true
}

type modelUsageJSON struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	Purpose   string  `json:"purpose"`
	Calls     int     `json:"calls"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}

// byModel adds the ledger rows up per provider, model and purpose, dearest first.
func byModel(rows []store.UsageRow) (out []modelUsageJSON, calls int, cost float64) {
	out = []modelUsageJSON{}
	for _, u := range rows {
		calls, cost = calls+u.Calls, cost+u.CostUSD
		i := slices.IndexFunc(out, func(m modelUsageJSON) bool {
			return m.Provider == u.Provider && m.Model == u.Model && m.Purpose == u.Purpose
		})
		if i < 0 {
			i, out = len(out), append(out, modelUsageJSON{Provider: u.Provider, Model: u.Model, Purpose: u.Purpose})
		}
		out[i].Calls, out[i].TokensIn, out[i].TokensOut, out[i].CostUSD = out[i].Calls+u.Calls, out[i].TokensIn+u.TokensIn, out[i].TokensOut+u.TokensOut, out[i].CostUSD+u.CostUSD
	}
	slices.SortStableFunc(out, func(a, b modelUsageJSON) int { return cmp.Compare(b.CostUSD, a.CostUSD) })
	return out, calls, cost
}

// handleStatsSummary is the Activity screen's tiles: what was sorted, what it cost and
// whether the accounts are well.
func (s *server) handleStatsSummary(w http.ResponseWriter, r *http.Request) {
	name, since, ok := s.statsSince(w, r, "day", "day", "week", "month")
	if !ok {
		return
	}
	ctx, uid := r.Context(), user(r).ID
	totals, err := s.store.StatsTotals(ctx, uid, since.Unix())
	if err != nil {
		internalError(w, r, err)
		return
	}
	review, err := s.store.CountMessages(ctx, store.StateReview)
	if err != nil {
		internalError(w, r, err)
		return
	}
	top, err := s.store.TopRules(ctx, uid, since.Unix(), 5)
	if err != nil {
		internalError(w, r, err)
		return
	}
	ledger, err := s.store.Usage(ctx, since.Format(time.DateOnly))
	if err != nil {
		internalError(w, r, err)
		return
	}
	accounts, err := s.store.Accounts(ctx)
	if err != nil {
		internalError(w, r, err)
		return
	}
	quiet, err := s.store.QuietRules(ctx, uid, since.Unix())
	if err != nil {
		internalError(w, r, err)
		return
	}
	models, _, cost := byModel(ledger)
	free := 0.0
	if totals.Processed > 0 {
		free = float64(totals.WithoutModel) / float64(totals.Processed)
	}
	topRules := make([]map[string]any, len(top))
	for i, t := range top {
		topRules[i] = map[string]any{"rule_id": ts(t.RuleID), "rule_name": t.RuleName, "hits": t.Emails}
	}
	health := make([]map[string]any, len(accounts))
	for i, a := range accounts {
		folders, err := s.store.Folders(ctx, a.ID)
		if err != nil {
			internalError(w, r, err)
			return
		}
		health[i] = map[string]any{"account_id": a.ID, "label": a.Label, "preset": a.Preset, "username": a.Username, "folder_count": len(folders),
			"status": a.Status, "last_event_at": ts(a.LastEventAt), "last_error": a.LastError}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": name, "since": since.Unix(),
		"counts":                map[string]int{"processed": totals.Processed, "sorted": totals.Sorted, "trashed": totals.Trashed, "review": review},
		"went":                  map[string]int{"sorted": totals.WentSorted, "inbox": totals.WentNowhere, "review": totals.WentReview, "trashed": totals.WentTrash},
		"decided_without_model": free, "cost_usd": cost, "calls_by_model": models, "top_rules": topRules, "quiet_rules": quiet, "accounts": health,
	})
}

// handleStatsUsage is the Usage screen: model calls and cost by day, by rule and by model.
// The days and the models come from the cost ledger (usage_daily), which counts every
// call; the rules come from the decisions, which know which rule a call was for.
func (s *server) handleStatsUsage(w http.ResponseWriter, r *http.Request) {
	name, since, ok := s.statsSince(w, r, "month", "month")
	if !ok {
		return
	}
	ctx, uid := r.Context(), user(r).ID
	totals, err := s.store.StatsTotals(ctx, uid, since.Unix())
	if err != nil {
		internalError(w, r, err)
		return
	}
	ledger, err := s.store.Usage(ctx, since.Format(time.DateOnly))
	if err != nil {
		internalError(w, r, err)
		return
	}
	costs, err := s.store.RuleCosts(ctx, uid, since.Unix())
	if err != nil {
		internalError(w, r, err)
		return
	}
	type dayModel struct {
		Provider string  `json:"provider"`
		Model    string  `json:"model"`
		Purpose  string  `json:"purpose"`
		Calls    int     `json:"calls"`
		CostUSD  float64 `json:"cost_usd"`
	}
	type dayJSON struct {
		Day    string     `json:"day"`
		Models []dayModel `json:"models"`
	}
	// One entry per day of the range, the quiet ones too, so a chart needs no gap filling.
	days := make([]dayJSON, statsRanges[name])
	for i := range days {
		days[i] = dayJSON{Day: since.AddDate(0, 0, i).Format(time.DateOnly), Models: []dayModel{}}
	}
	for _, u := range ledger {
		i := slices.IndexFunc(days, func(d dayJSON) bool { return d.Day == u.Day })
		if i < 0 {
			continue
		}
		ms := days[i].Models
		j := slices.IndexFunc(ms, func(m dayModel) bool { return m.Provider == u.Provider && m.Model == u.Model && m.Purpose == u.Purpose })
		if j < 0 {
			j, ms = len(ms), append(ms, dayModel{Provider: u.Provider, Model: u.Model, Purpose: u.Purpose})
		}
		ms[j].Calls, ms[j].CostUSD = ms[j].Calls+u.Calls, ms[j].CostUSD+u.CostUSD
		days[i].Models = ms
	}
	byRule := make([]map[string]any, len(costs))
	for i, c := range costs {
		byRule[i] = map[string]any{"rule_id": ts(c.RuleID), "rule_name": c.RuleName, "emails": c.Emails, "calls": c.Calls, "cost_usd": c.CostUSD}
	}
	models, calls, cost := byModel(ledger)
	writeJSON(w, http.StatusOK, map[string]any{
		"range": name, "since": since.Unix(), "processed": totals.Processed, "emails": totals.Sorted, "calls": calls, "cost_usd": cost,
		"days": days, "by_rule": byRule, "by_model": models, "without_model": totals.WithoutModel,
	})
}
