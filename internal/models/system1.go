package models

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Jev (TypeSafe, served by OpenRouter) and Clef (Cloudflare Workers AI) are
// decision models that share the "System One" wire format: a state, a map of
// typed questions, and an answer with a probability per option. Shapes follow
// https://openrouter.ai/docs/guides/community/jev-tutorial and
// https://developers.cloudflare.com/workers-ai/models/clef/ (read 2026-10-06).

const (
	// DefaultJevURL and DefaultClefURL are the providers' public hosts.
	DefaultJevURL  = "https://openrouter.ai"
	DefaultClefURL = "https://api.cloudflare.com"
	// DefaultJevModel is pinned so tuned thresholds do not move under us.
	DefaultJevModel  = "typesafe/jev-1.13"
	DefaultClefModel = "clef"

	s1Question = "rule"
	s1None     = "none"
)

type system1 struct {
	name, provider string
	model          string // ledger and price-table id
	wireModel      string // the "model" field of the request
	url            string
	header         http.Header // carries the credential
	deps           Deps
}

// NewJev decides with Jev through OpenRouter's Decisions API.
func NewJev(baseURL, apiKey, model string, deps Deps) Decider {
	return &system1{
		name: "jev", provider: "openrouter", model: model, wireModel: model,
		url:    strings.TrimRight(baseURL, "/") + "/api/alpha/decisions",
		header: http.Header{"Authorization": {"Bearer " + apiKey}},
		deps:   deps,
	}
}

// NewClef decides with Clef on Workers AI. model is "clef" or "clef-flash".
func NewClef(baseURL, accountID, apiToken, model string, deps Deps) Decider {
	model = strings.TrimPrefix(model, "@cf/cloudflare/")
	id := "@cf/cloudflare/" + model
	return &system1{
		name: "clef", provider: "cloudflare", model: id, wireModel: model,
		url:    strings.TrimRight(baseURL, "/") + "/client/v4/accounts/" + accountID + "/ai/run/" + id,
		header: http.Header{"Authorization": {"Bearer " + apiToken}},
		deps:   deps,
	}
}

func (s *system1) Name() string { return s.name }

type s1Choice struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type s1Request struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]s1Choice `json:"questions"`
}

type s1Response struct {
	Answers map[string]struct {
		Choice        string             `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
	} `json:"answers"`
	Usage struct {
		InputTokens  int      `json:"input_tokens"`
		OutputTokens int      `json:"output_tokens"`
		Cost         *float64 `json:"cost"` // USD; OpenRouter reports it, Workers AI does not
	} `json:"usage"`
}

func (s *system1) Decide(ctx context.Context, req DecideRequest) (Decision, Usage, error) {
	u := Usage{Provider: s.provider, Model: s.model}
	criteria := map[string]string{s1None: "None of these"}
	ids := map[string]int64{s1None: 0}
	for _, c := range req.Candidates {
		key := fmt.Sprintf("rule_%d", c.RuleID)
		criteria[key] = ruleText(c)
		ids[key] = c.RuleID
	}
	body := s1Request{
		Model: s.wireModel,
		State: RenderEmail(req.Email),
		Questions: map[string]s1Choice{s1Question: {
			Type: "choice", Instructions: "Which of these rules does this email match?", Criteria: criteria,
		}},
	}

	// Workers AI's REST API wraps a model's output in {"result": ...}; the
	// Clef page documents only the inner object, so both shapes are accepted.
	var env struct {
		Result json.RawMessage `json:"result"`
		s1Response
	}
	start := time.Now()
	err := s.deps.Caller.Do(ctx, func(ctx context.Context) error {
		return postJSON(ctx, s.deps.client(), s.name, s.url, s.header, body, &env)
	})
	u.Latency = time.Since(start)
	if err != nil {
		return Decision{}, u, err
	}
	resp := env.s1Response
	if len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			return Decision{}, u, fmt.Errorf("%s: %w: result is not the expected JSON", s.name, ErrBadOutput)
		}
	}

	u.TokensIn, u.TokensOut = resp.Usage.InputTokens, resp.Usage.OutputTokens
	if resp.Usage.Cost != nil {
		u.CostUSD = *resp.Usage.Cost
	} else {
		u.CostUSD = s.deps.Prices.Cost(s.provider, s.model, u.TokensIn, u.TokensOut, 0, 0)
	}
	ans, ok := resp.Answers[s1Question]
	if !ok || ans.Choice == "" {
		return Decision{}, u, fmt.Errorf("%s: %w: no answer to the question", s.name, ErrBadOutput)
	}
	id, ok := ids[ans.Choice]
	if !ok {
		id = -1 // not an option we offered; allow turns it into "none"
	}
	// Confidence is the probability of the chosen option (backend plan), not
	// the provider's "confidence" field, which measures how peaked the spread is.
	return allow(req.Candidates, Decision{RuleID: id, Confidence: ans.Probabilities[ans.Choice]}), u, nil
}
