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
	"regexp"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/learn"
	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// ErrAuth means the provider rejected the API key or token.
var ErrAuth = errors.New("model provider rejected the credentials")

// ErrBadOutput means the provider answered, but not with something usable.
var ErrBadOutput = errors.New("model output is not usable")

// StatusError is a non-2xx answer from a provider. Type, Detail and RequestID come from
// the provider's error reply so the log can say why a request was refused (MAI-56).
// Provider error bodies can echo parts of the request, so Detail is scrubbed: everything
// quoted and every email address is cut out, and it is capped (see scrubDetail).
type StatusError struct {
	Provider  string
	Code      int
	Type      string // the provider's error type, such as "invalid_request_error"; may be empty
	Detail    string // the provider's message, scrubbed; may be empty
	RequestID string // the provider's id for the request, for its support; may be empty
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("%s: HTTP %d %s", e.Provider, e.Code, http.StatusText(e.Code))
	for _, part := range []string{e.Type, e.Detail} {
		if part != "" {
			s += ": " + part
		}
	}
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

// newStatusError reads a provider's error reply into a StatusError.
func newStatusError(provider string, code int, body []byte, requestID string) *StatusError {
	typ, msg := errorReply(body)
	return &StatusError{Provider: provider, Code: code, Type: scrubDetail(typ), Detail: scrubDetail(msg), RequestID: requestID}
}

// errorReply reads the error type and message out of a provider's error body, in the
// shapes the providers use: {"error": {"type", "message"}} (Anthropic, OpenAI),
// {"error": "message"} (Ollama) and {"errors": [{"message"}]} (Cloudflare). Anything
// else gives nothing: an unknown body is never logged as it is.
func errorReply(body []byte) (typ, msg string) {
	var reply struct {
		Error  json.RawMessage `json:"error"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return "", ""
	}
	var nested struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	switch {
	case json.Unmarshal(reply.Error, &nested) == nil && (nested.Type != "" || nested.Message != ""):
		return nested.Type, nested.Message
	case json.Unmarshal(reply.Error, &msg) == nil:
		return "", msg
	case len(reply.Errors) > 0:
		return "", reply.Errors[0].Message
	}
	return "", ""
}

// echoed matches what a provider's error message could have copied from the request or
// its credentials: text in double quotes or back quotes; text in single quotes that opens
// after a space (an apostrophe inside a word does not); email addresses; and a word that
// starts like a key (sk-, pk-, rk-, with - or _) or whatever follows "Bearer".
var echoed = regexp.MustCompile("\"[^\"]*\"|`[^`]*`|(?:^|\\s)'[^']*'|[\\w.+-]+@[\\w-]+(?:\\.[\\w-]+)+" +
	"|(?i:\\b(?:sk|pk|rk)[-_]\\S+|bearer\\s+\\S+)")

// longRun is an unbroken run of 20 or more letters, digits and key characters. One with a
// digit in it is how a key, a token or a masked key looks, and is cut; one without, such
// as invalid_request_error, is a name and is kept. Dotted field paths such as
// messages.0.content.0.text are not runs: they say what was wrong and are kept.
var longRun = regexp.MustCompile(`[A-Za-z0-9_+/=*-]{20,}`)

// maxDetail caps a provider's message in the log.
const maxDetail = 300

// scrubDetail makes a provider's error text fit for the log: echoes of the request and
// anything like a credential cut out, whitespace collapsed to single spaces, at most
// maxDetail characters.
func scrubDetail(s string) string {
	s = echoed.ReplaceAllString(s, " …")
	s = longRun.ReplaceAllStringFunc(s, func(run string) string {
		if strings.ContainsAny(run, "0123456789") {
			return " …"
		}
		return run
	})
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxDetail {
		s = string(r[:maxDetail]) + "…"
	}
	return s
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

// Spreader is a Decider that can also say how sure it was of every option: the
// probability it gave each candidate, by rule id, with 0 for "none of these". The decision
// models (Jev, Clef) are; a generative model asked for one answer is not.
type Spreader interface {
	Decider
	DecideSpread(ctx context.Context, req DecideRequest) (Decision, map[int64]float64, Usage, error)
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
	// AnthropicURL is where the Claude adapters and the workspace lookup send their
	// requests; empty = the public API. Only tests point it elsewhere.
	AnthropicURL string
	// WorkspaceNeeded is asked when Anthropic refuses a Claude request because its key covers
	// a whole organisation and no workspace was named. It answers the workspace to retry the
	// request in, or "" when there is none to use. nil = the refusal stands.
	WorkspaceNeeded func(ctx context.Context, apiKey string) string
}

func (d Deps) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return http.DefaultClient
}
