package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	openaiopt "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// The three generative adapters. Each implements Generator by forcing the
// model to answer in the caller's JSON schema; NewLLMDecider turns any of them
// into a Decider. SDK retries are off because Caller owns retrying.

const (
	// DefaultAnthropicModel is the plan's fallback and composer model.
	DefaultAnthropicModel = "claude-haiku-4-5"

	answerTool     = "answer"
	answerToolDesc = "Record the answer."
	// maxOutputTokens is a ceiling, not a target: a decision is ~60 tokens, a composer draft more.
	maxOutputTokens = 4096
)

// schemaMap decodes a JSON schema for the SDKs that want it as a map.
func schemaMap(provider string, schema json.RawMessage) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(schema, &m); err != nil {
		return nil, fmt.Errorf("%s: output schema is not a JSON object: %w", provider, err)
	}
	return m, nil
}

// decodeAnswer reads the model's structured answer into out.
func decodeAnswer(provider, raw string, out any) error {
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("%s: %w: answer is not the expected JSON", provider, ErrBadOutput)
	}
	return nil
}

// Anthropic talks to the Messages API through the official SDK.
type Anthropic struct {
	client anthropic.Client
	model  string
	deps   Deps
}

// NewAnthropic builds the adapter. An empty baseURL means the public API.
func NewAnthropic(baseURL, apiKey, model string, deps Deps) *Anthropic {
	opts := []anthropicopt.RequestOption{
		anthropicopt.WithoutEnvironmentDefaults(), // config is the only source of credentials
		anthropicopt.WithAPIKey(apiKey),
		anthropicopt.WithHTTPClient(deps.client()),
		anthropicopt.WithMaxRetries(0),
	}
	if baseURL != "" {
		opts = append(opts, anthropicopt.WithBaseURL(baseURL))
	}
	return &Anthropic{client: anthropic.NewClient(opts...), model: model, deps: deps}
}

// Generate caches the system prompt and forces one call of a tool whose input
// schema is schema, at temperature 0. Forced tool choice and temperature are
// accepted by Haiku 4.5; newer Opus and Sonnet models reject both.
func (a *Anthropic) Generate(ctx context.Context, system, user string, schema json.RawMessage, out any) (Usage, error) {
	u := Usage{Provider: "anthropic", Model: a.model}
	input, err := schemaMap("anthropic", schema)
	if err != nil {
		return u, err
	}
	delete(input, "type") // the SDK writes "type": "object" itself
	params := anthropic.MessageNewParams{
		Model:       anthropic.Model(a.model),
		MaxTokens:   maxOutputTokens,
		Temperature: anthropic.Float(0),
		System: []anthropic.TextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
		Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name:        answerTool,
			Description: anthropic.String(answerToolDesc),
			InputSchema: anthropic.ToolInputSchemaParam{ExtraFields: input},
		}}},
		ToolChoice: anthropic.ToolChoiceParamOfTool(answerTool),
	}

	var resp *anthropic.Message
	start := time.Now()
	err = a.deps.Caller.Do(ctx, Call{"anthropic", a.model}, func(ctx context.Context) error {
		var err error
		resp, err = a.client.Messages.New(ctx, params)
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return &StatusError{Provider: "anthropic", Code: apiErr.StatusCode}
		}
		if err != nil {
			return fmt.Errorf("anthropic: %w", err)
		}
		return nil
	})
	u.Latency = time.Since(start)
	if err != nil {
		return u, err
	}

	us := resp.Usage
	u.TokensIn = int(us.InputTokens + us.CacheReadInputTokens + us.CacheCreationInputTokens)
	u.TokensOut = int(us.OutputTokens)
	u.CostUSD = a.deps.Prices.Cost("anthropic", a.model, int(us.InputTokens), u.TokensOut,
		int(us.CacheReadInputTokens), int(us.CacheCreationInputTokens))
	for _, block := range resp.Content {
		if tool, ok := block.AsAny().(anthropic.ToolUseBlock); ok && tool.Name == answerTool {
			return u, decodeAnswer("anthropic", string(tool.Input), out)
		}
	}
	return u, fmt.Errorf("anthropic: %w: no tool call in the answer (stop reason %s)", ErrBadOutput, resp.StopReason)
}

// OpenAI talks to any OpenAI-compatible chat completions endpoint (OpenAI,
// OpenRouter, Groq, DeepSeek) through the official SDK.
type OpenAI struct {
	client openai.Client
	model  string
	deps   Deps
}

// NewOpenAI builds the adapter. An empty baseURL means api.openai.com.
func NewOpenAI(baseURL, apiKey, model string, deps Deps) *OpenAI {
	opts := []openaiopt.RequestOption{
		openaiopt.WithAPIKey(apiKey),
		openaiopt.WithHTTPClient(deps.client()),
		openaiopt.WithMaxRetries(0),
	}
	if baseURL != "" {
		opts = append(opts, openaiopt.WithBaseURL(baseURL))
	}
	return &OpenAI{client: openai.NewClient(opts...), model: model, deps: deps}
}

// Generate forces one function call whose parameters are schema, at temperature 0.
func (o *OpenAI) Generate(ctx context.Context, system, user string, schema json.RawMessage, out any) (Usage, error) {
	u := Usage{Provider: "openai", Model: o.model}
	parameters, err := schemaMap("openai", schema)
	if err != nil {
		return u, err
	}
	params := openai.ChatCompletionNewParams{
		Model:       o.model,
		Temperature: openai.Float(0),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
		Tools: []openai.ChatCompletionToolUnionParam{openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        answerTool,
			Description: openai.String(answerToolDesc),
			Parameters:  parameters,
		})},
		ToolChoice: openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{Name: answerTool}),
	}

	var resp *openai.ChatCompletion
	start := time.Now()
	err = o.deps.Caller.Do(ctx, Call{"openai", o.model}, func(ctx context.Context) error {
		var err error
		resp, err = o.client.Chat.Completions.New(ctx, params)
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			return &StatusError{Provider: "openai", Code: apiErr.StatusCode}
		}
		if err != nil {
			return fmt.Errorf("openai: %w", err)
		}
		return nil
	})
	u.Latency = time.Since(start)
	if err != nil {
		return u, err
	}

	u.TokensIn, u.TokensOut = int(resp.Usage.PromptTokens), int(resp.Usage.CompletionTokens)
	u.CostUSD = o.deps.Prices.Cost("openai", o.model, u.TokensIn, u.TokensOut, 0, 0)
	if len(resp.Choices) == 0 {
		return u, fmt.Errorf("openai: %w: no choices in the answer", ErrBadOutput)
	}
	msg := resp.Choices[0].Message
	if len(msg.ToolCalls) > 0 {
		return u, decodeAnswer("openai", msg.ToolCalls[0].Function.Arguments, out)
	}
	// Some compatible servers ignore tool_choice and answer with the JSON as text.
	return u, decodeAnswer("openai", msg.Content, out)
}

// Ollama talks to a local Ollama server's /api/chat with plain net/http
// (https://docs.ollama.com/api/chat, read 2026-10-06).
type Ollama struct {
	url   string
	model string
	deps  Deps
}

// NewOllama builds the adapter; baseURL is OLLAMA_URL, e.g. http://localhost:11434.
func NewOllama(baseURL, model string, deps Deps) *Ollama {
	return &Ollama{url: strings.TrimRight(baseURL, "/") + "/api/chat", model: model, deps: deps}
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Generate asks for a non-streamed answer constrained to schema ("format"), at temperature 0.
func (o *Ollama) Generate(ctx context.Context, system, user string, schema json.RawMessage, out any) (Usage, error) {
	u := Usage{Provider: "ollama", Model: o.model}
	body := struct {
		Model    string          `json:"model"`
		Messages []ollamaMessage `json:"messages"`
		Stream   bool            `json:"stream"`
		Format   json.RawMessage `json:"format"`
		Options  map[string]any  `json:"options"`
	}{o.model, []ollamaMessage{{"system", system}, {"user", user}}, false, schema, map[string]any{"temperature": 0}}
	var resp struct {
		Message         ollamaMessage `json:"message"`
		PromptEvalCount int           `json:"prompt_eval_count"`
		EvalCount       int           `json:"eval_count"`
	}
	start := time.Now()
	err := o.deps.Caller.Do(ctx, Call{"ollama", o.model}, func(ctx context.Context) error {
		return postJSON(ctx, o.deps.client(), "ollama", o.url, nil, body, &resp)
	})
	u.Latency = time.Since(start)
	if err != nil {
		return u, err
	}
	u.TokensIn, u.TokensOut = resp.PromptEvalCount, resp.EvalCount
	u.CostUSD = o.deps.Prices.Cost("ollama", o.model, u.TokensIn, u.TokensOut, 0, 0)
	return u, decodeAnswer("ollama", resp.Message.Content, out)
}
