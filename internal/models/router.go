package models

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/config"
)

// UsageStore is the cost ledger (store.Store implements it): it adds to the
// usage_daily row for that day, provider, model and purpose.
type UsageStore interface {
	AddUsage(ctx context.Context, day, provider, model, purpose string, calls, tokensIn, tokensOut int, costUSD float64) error
}

// Router is the Decider the pipeline uses. It asks the primary decider and,
// when that answer's confidence is below EscalateBelow, asks the fallback with
// the few-shot examples and uses the fallback's answer.
type Router struct {
	Primary       Decider
	Fallback      Decider // nil = never escalate
	EscalateBelow float64
	Usage         UsageStore       // nil = do not record
	Now           func() time.Time // nil = time.Now
	// Purpose, when set, is the ledger purpose of every call ("test" for the rule tester);
	// empty records the primary's calls as "decide" and the fallback's as "escalate".
	Purpose string
}

// For returns a copy of the router whose calls are recorded under purpose.
func (r *Router) For(purpose string) *Router {
	if r == nil {
		return nil
	}
	c := *r
	c.Purpose = purpose
	return &c
}

// Result is a routed decision with what each model call cost.
type Result struct {
	Decision
	Escalated   bool   // the fallback was asked
	Primary     Usage  // always set
	Fallback    *Usage // set when the fallback answered
	FallbackErr error  // set when the fallback was asked and failed; Decision is then the primary's
	// Probabilities is what the primary gave each candidate, by rule id (0 = none of
	// them), when it is a Spreader; nil otherwise. It stays the primary's when the
	// fallback answers: it is why the fallback was asked.
	Probabilities map[int64]float64
}

// Name reports the primary decider's name.
func (r *Router) Name() string { return r.Primary.Name() }

// Decide implements Decider. The usage it returns names the model whose answer
// was used and adds up tokens, cost and latency over both calls.
func (r *Router) Decide(ctx context.Context, req DecideRequest) (Decision, Usage, error) {
	res, err := r.Route(ctx, req)
	u := res.Primary
	if res.Fallback != nil {
		u = *res.Fallback
		u.TokensIn += res.Primary.TokensIn
		u.TokensOut += res.Primary.TokensOut
		u.CostUSD += res.Primary.CostUSD
		u.Latency += res.Primary.Latency
	}
	return res.Decision, u, err
}

// Route is Decide with the escalation detail kept apart.
func (r *Router) Route(ctx context.Context, req DecideRequest) (Result, error) {
	if len(req.Candidates) == 0 {
		return Result{Decision: Decision{Confidence: 1, Reason: "No candidate rules"}}, nil
	}
	primaryReq := req
	primaryReq.Examples = nil // few-shot examples are for the fallback only
	var d Decision
	var u Usage
	var spread map[int64]float64
	var err error
	if sp, ok := r.Primary.(Spreader); ok {
		d, spread, u, err = sp.DecideSpread(ctx, primaryReq)
	} else {
		d, u, err = r.Primary.Decide(ctx, primaryReq)
	}
	if err != nil {
		return Result{Primary: u}, fmt.Errorf("primary decider %s: %w", r.Primary.Name(), err)
	}
	r.record(ctx, "decide", u)
	res := Result{Decision: allow(req.Candidates, d), Primary: u, Probabilities: spread}
	if r.Fallback == nil || res.Confidence >= r.EscalateBelow {
		return res, nil
	}

	res.Escalated = true
	fd, fu, err := r.Fallback.Decide(ctx, req)
	if err != nil {
		// Keep the primary's answer; its confidence is already below the
		// escalation threshold, and the reason says nobody double-checked it.
		slog.WarnContext(ctx, "fallback model failed; keeping the primary answer", "fallback", r.Fallback.Name(), "error", err.Error())
		res.FallbackErr = err
		res.Reason = strings.TrimSuffix(res.Reason, ".") + " (low confidence; fallback failed)"
		return res, nil
	}
	r.record(ctx, "escalate", fu)
	res.Decision, res.Fallback = allow(req.Candidates, fd), &fu
	return res, nil
}

// record writes one call to the ledger. A ledger failure must not lose a decision.
func (r *Router) record(ctx context.Context, purpose string, u Usage) {
	if r.Usage == nil {
		return
	}
	if r.Purpose != "" {
		purpose = r.Purpose
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	day := now().UTC().Format("2006-01-02")
	if err := r.Usage.AddUsage(ctx, day, u.Provider, u.Model, purpose, 1, u.TokensIn, u.TokensOut, u.CostUSD); err != nil {
		slog.WarnContext(ctx, "could not record model usage", "error", err.Error())
	}
}

// NewDecider builds one decider from config. spec is a MAILRULES_DECIDER name,
// optionally followed by ":model" (for example "clef:clef-flash" or
// "ollama:llama3.2"). jev, clef and anthropic have default models; openai and
// ollama need one named (MAILRULES_DECIDER_MODEL, see config.DeciderSpec).
func NewDecider(cfg *config.Config, spec string, deps Deps) (Decider, error) {
	name, model, _ := strings.Cut(spec, ":")
	ready := *cfg
	ready.Decider = name
	if err := ready.DeciderReady(); err != nil {
		return nil, err
	}
	orDefault := func(def string) string {
		if model == "" {
			return def
		}
		return model
	}
	switch name {
	case "jev":
		return NewJev(DefaultJevURL, cfg.OpenRouterAPIKey, orDefault(DefaultJevModel), deps), nil
	case "clef":
		return NewClef(DefaultClefURL, cfg.CloudflareAccountID, cfg.CloudflareAPIToken, orDefault(DefaultClefModel), deps), nil
	case "anthropic":
		return NewLLMDecider(name, NewAnthropic("", cfg.AnthropicAPIKey, orDefault(DefaultAnthropicModel), deps)), nil
	case "openai", "ollama":
		if model == "" {
			return nil, fmt.Errorf("decider %s needs a model: write it as %s:<model>", name, name)
		}
		if name == "ollama" {
			return NewLLMDecider(name, NewOllama(cfg.OllamaURL, model, deps)), nil
		}
		return NewLLMDecider(name, NewOpenAI(cfg.OpenAIBaseURL, cfg.OpenAIAPIKey, model, deps)), nil
	}
	return nil, fmt.Errorf("unknown decider %q: must be jev, clef, anthropic, openai or ollama", name)
}

// NewRouter builds the Router for a decider spec. The fallback is Anthropic's
// MAILRULES_FALLBACK_MODEL; it is left out when that model is empty, when
// there is no Anthropic key, or when the primary already is that model.
func NewRouter(cfg *config.Config, spec string, deps Deps, usage UsageStore) (*Router, error) {
	primary, err := NewDecider(cfg, spec, deps)
	if err != nil {
		return nil, err
	}
	r := &Router{Primary: primary, EscalateBelow: cfg.EscalateBelow, Usage: usage}
	name, model, _ := strings.Cut(spec, ":")
	if model == "" {
		model = DefaultAnthropicModel
	}
	samePrimary := name == "anthropic" && model == cfg.FallbackModel
	if cfg.FallbackModel != "" && cfg.AnthropicAPIKey != "" && !samePrimary {
		r.Fallback = NewLLMDecider("anthropic", NewAnthropic("", cfg.AnthropicAPIKey, cfg.FallbackModel, deps))
	}
	return r, nil
}
