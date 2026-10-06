// Package models puts every model provider behind two interfaces: Decider
// (which rule does this email match?) and Generator (free-form structured
// output). Nothing outside this package imports a provider SDK.
package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/learn"
	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// ErrAuth means the provider rejected the API key or token.
var ErrAuth = errors.New("model provider rejected the credentials")

// ErrBadOutput means the provider answered, but not with something usable.
var ErrBadOutput = errors.New("model output is not usable")

// StatusError is a non-2xx answer from a provider. It deliberately carries
// only the status: provider error bodies can echo parts of the request.
type StatusError struct {
	Provider string
	Code     int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d %s", e.Provider, e.Code, http.StatusText(e.Code))
}

// Is makes errors.Is(err, ErrAuth) true for 401 and 403.
func (e *StatusError) Is(target error) bool {
	return target == ErrAuth && (e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden)
}

// Candidate is one intent rule the email might match.
type Candidate struct {
	RuleID     int64
	Name       string // the rule's name, shown to models and in the synthesized reason
	Intent     string // plain-English intent
	Exceptions string // rendered "unless" text, may be empty
}

// DecideRequest is one email and the rules it may match.
type DecideRequest struct {
	Email      message.Summary // headers, signals, text <= BODY_CHARS
	Candidates []Candidate     // eligible intent rules in priority order
	Examples   []learn.Example // few-shot corrections (fallback model only)
}

// Decision is a model's answer.
type Decision struct {
	RuleID     int64   // 0 = none of the rules
	Confidence float64 // calibrated 0..1
	Reason     string  // one sentence; synthesized when the model gives none
}

// Usage is what one model call cost.
type Usage struct {
	Provider, Model     string
	TokensIn, TokensOut int
	CostUSD             float64
	Latency             time.Duration
}

// Decider picks the rule an email matches.
type Decider interface {
	Name() string
	Decide(ctx context.Context, req DecideRequest) (Decision, Usage, error)
}

// Generator is for free-form structured output (composer, AI rule builder).
type Generator interface {
	Generate(ctx context.Context, system, user string, schema json.RawMessage, out any) (Usage, error)
}

// Deps are what every adapter shares.
type Deps struct {
	HTTP   *http.Client // nil = http.DefaultClient
	Caller *Caller      // the global concurrency cap, timeout and retries
	Prices Prices
}

func (d Deps) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return http.DefaultClient
}
