package models

import (
	"context"
	"errors"
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
// the few-shot examples and uses the fallback's answer. A model whose answer cannot
// be read (ErrBadOutput) is asked again, up to ReadTries calls in all.
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

// ReadTries is how many times one model is asked about one email while its answer cannot
// be read: the first call and two retries (PRD R5).
const ReadTries = 3

// Result is a routed decision with what each model call cost.
type Result struct {
	Decision
	Escalated   bool   // the fallback was asked
	Primary     Usage  // always set; summed over the primary's calls
	Fallback    *Usage // set when the fallback answered; summed over its calls
	FallbackErr error  // set when the fallback was asked and failed; Decision is then the primary's
	Calls       int    // model calls made, counting each retry of an unreadable answer
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

// Route is Decide with the escalation detail kept apart. When the primary's answer still
// cannot be read after ReadTries calls, the error wraps ErrBadOutput and the Result carries
// what those calls cost.
func (r *Router) Route(ctx context.Context, req DecideRequest) (Result, error) {
	if len(req.Candidates) == 0 {
		return Result{Decision: Decision{Confidence: 1, Reason: "No candidate rules"}}, nil
	}
	primaryReq := req
	primaryReq.Examples = nil // few-shot examples are for the fallback only
	d, spread, u, calls, err := r.ask(ctx, r.Primary, primaryReq, "decide")
	if err != nil {
		return Result{Primary: u, Calls: calls}, fmt.Errorf("primary decider %s: %w", r.Primary.Name(), err)
	}
	res := Result{Decision: allow(req.Candidates, d), Primary: u, Calls: calls, Probabilities: spread}
	if r.Fallback == nil || res.Confidence >= r.EscalateBelow {
		return res, nil
	}

	res.Escalated = true
	fd, _, fu, calls, err := r.ask(ctx, r.Fallback, req, "escalate")
	res.Calls += calls
	if err != nil {
		// Keep the primary's answer; its confidence is already below the
		// escalation threshold, and the reason says nobody double-checked it.
		slog.WarnContext(ctx, "fallback model failed; keeping the primary answer", "fallback", r.Fallback.Name(), "error", err.Error())
		res.FallbackErr = err
		res.Reason = strings.TrimSuffix(res.Reason, ".") + " (low confidence; fallback failed)"
		return res, nil
	}
	res.Decision, res.Fallback = allow(req.Candidates, fd), &fu
	return res, nil
}

// ask puts one question to one model, asking again while its answer cannot be read, at
// most ReadTries calls in all. Every answered call goes on the ledger under purpose; the
// usage returned is summed over them. Any other error ends it at once: the Caller has
// already retried what retrying can help.
func (r *Router) ask(ctx context.Context, dec Decider, req DecideRequest, purpose string) (Decision, map[int64]float64, Usage, int, error) {
	pctx := WithPurpose(ctx, r.purpose(purpose))
	var total Usage
	for call := 1; ; call++ {
		var d Decision
		var u Usage
		var spread map[int64]float64
		var err error
		if sp, ok := dec.(Spreader); ok {
			d, spread, u, err = sp.DecideSpread(pctx, req)
		} else {
			d, u, err = dec.Decide(pctx, req)
		}
		total = addUsage(total, u)
		bad := errors.Is(err, ErrBadOutput)
		if err == nil || bad {
			r.record(ctx, purpose, u)
		}
		if !bad || call >= ReadTries {
			return d, spread, total, call, err
		}
		slog.WarnContext(ctx, "model answer could not be read; asking again", "model", dec.Name(), "call", call, "error", err.Error())
	}
}

// addUsage adds b's tokens, cost and latency to a, naming b's provider and model.
func addUsage(a, b Usage) Usage {
	b.TokensIn += a.TokensIn
	b.TokensOut += a.TokensOut
	b.CostUSD += a.CostUSD
	b.Latency += a.Latency
	return b
}

// purpose is the ledger purpose of a call that would be recorded as def.
func (r *Router) purpose(def string) string {
	if r.Purpose != "" {
		return r.Purpose
	}
	return def
}

// record writes one call to the ledger. A ledger failure must not lose a decision, and a
// call that was made is paid for even if the caller has since gone, so a cancelled ctx
// does not keep it off the ledger.
func (r *Router) record(ctx context.Context, purpose string, u Usage) {
	ctx = context.WithoutCancel(ctx)
	if r.Usage == nil {
		return
	}
	purpose = r.purpose(purpose)
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
		return NewLLMDecider(name, generator(cfg, name, orDefault(DefaultAnthropicModel), deps)), nil
	case "openai", "ollama":
		if model == "" {
			return nil, config.MissingModel("decider", name)
		}
		return NewLLMDecider(name, generator(cfg, name, model, deps)), nil
	}
	return nil, fmt.Errorf("unknown decider %q: must be jev, clef, anthropic, openai or ollama", name)
}

// NewGenerator builds the generative model a composer_model spec names (see
// config.SplitComposerModel): a bare name is a Claude model on Anthropic, otherwise
// anthropic:, openai: or ollama: and the model. It does not check that the provider's key
// or URL is set (config.ComposerReady does); a call without them fails at the provider.
func NewGenerator(cfg *config.Config, spec string, deps Deps) (Generator, error) {
	provider, model, err := config.SplitComposerModel(spec)
	if err != nil {
		return nil, err
	}
	return generator(cfg, provider, model, deps), nil
}

// generator builds the adapter of a generative provider: anthropic, openai or ollama.
func generator(cfg *config.Config, provider, model string, deps Deps) Generator {
	switch provider {
	case "openai":
		return NewOpenAI(cfg.OpenAIBaseURL, cfg.OpenAIAPIKey, model, deps)
	case "ollama":
		return NewOllama(cfg.OllamaURL, model, deps)
	}
	return NewAnthropic(AnthropicAuthOf(cfg), model, deps)
}

// AnthropicAuthOf is the Anthropic credentials of a configuration: every Claude adapter is
// built from it, so none runs without the workspace the settings name.
func AnthropicAuthOf(cfg *config.Config) AnthropicAuth {
	return AnthropicAuth{APIKey: cfg.AnthropicAPIKey, WorkspaceID: cfg.AnthropicWorkspaceID}
}

// Why a configured fallback model is not asked. FallbackNotSkipped ("") means it is, or none
// is configured.
const (
	FallbackNotSkipped = ""
	FallbackNeedsKey   = "needs_key"  // the fallback is a Claude model and there is no Anthropic key
	FallbackIsPrimary  = "is_primary" // the decision model already is that Claude model
)

// FallbackSkipped says why NewRouter leaves out the configured fallback for a decider spec:
// FallbackNeedsKey, FallbackIsPrimary, or FallbackNotSkipped when the fallback is built or
// none is set (FallbackModel empty). It is the one place that decides, so the startup log
// and the Settings screen cannot disagree with the router.
func FallbackSkipped(cfg *config.Config, spec string) string {
	if cfg.FallbackModel == "" {
		return FallbackNotSkipped
	}
	name, model, _ := strings.Cut(spec, ":")
	if model == "" {
		model = DefaultAnthropicModel
	}
	switch {
	case name == "anthropic" && model == cfg.FallbackModel:
		return FallbackIsPrimary
	case cfg.AnthropicAPIKey == "":
		return FallbackNeedsKey
	}
	return FallbackNotSkipped
}

// NewRouter builds the Router for a decider spec. The fallback is Anthropic's
// MAILRULES_FALLBACK_MODEL; it is left out when that model is empty, when
// there is no Anthropic key, or when the primary already is that model
// (see FallbackSkipped).
func NewRouter(cfg *config.Config, spec string, deps Deps, usage UsageStore) (*Router, error) {
	primary, err := NewDecider(cfg, spec, deps)
	if err != nil {
		return nil, err
	}
	r := &Router{Primary: primary, EscalateBelow: cfg.EscalateBelow, Usage: usage}
	if cfg.FallbackModel != "" && FallbackSkipped(cfg, spec) == FallbackNotSkipped {
		r.Fallback = NewLLMDecider("anthropic", NewAnthropic(AnthropicAuthOf(cfg), cfg.FallbackModel, deps))
	}
	return r, nil
}
