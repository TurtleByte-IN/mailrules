package models

import (
	"encoding/json"
	"fmt"
	"os"
)

// Price is what a model costs, in USD per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`  // input tokens served from a prompt cache
	CacheWrite float64 `json:"cache_write"` // input tokens written to a prompt cache
}

// Prices maps provider, then model id, to a price. A model that is missing
// costs 0, which is also the right answer for local Ollama models.
type Prices map[string]map[string]Price

// DefaultPrices is the built-in table. Sources, read on 2026-10-06:
// Jev from the worked example in the OpenRouter tutorial (476 input tokens
// cost $0.000019992; output is free), Clef from the Workers AI model pages,
// Haiku 4.5 from Anthropic's model table ($1 in, $5 out) with the cache rates
// OpenRouter lists for the same model. OpenAI-compatible endpoints differ
// per host, so they have no built-in entry: add them in MAILRULES_PRICES_FILE.
func DefaultPrices() Prices {
	return Prices{
		"openrouter": {"typesafe/jev-1.13": {Input: 0.042}},
		"cloudflare": {
			"@cf/cloudflare/clef":       {Input: 0.24},
			"@cf/cloudflare/clef-flash": {Input: 0.09},
		},
		"anthropic": {"claude-haiku-4-5": {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25}},
	}
}

// LoadPrices returns the built-in table with the entries of the JSON file at
// path (MAILRULES_PRICES_FILE) laid over it. An empty path means built-in only.
// The file looks like {"openai": {"gpt-x": {"input": 0.25, "output": 2}}}.
func LoadPrices(path string) (Prices, error) {
	p := DefaultPrices()
	if path == "" {
		return p, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator chooses this path
	if err != nil {
		return nil, fmt.Errorf("read prices file: %w", err)
	}
	var over Prices
	if err := json.Unmarshal(raw, &over); err != nil {
		return nil, fmt.Errorf("parse prices file %s: %w", path, err)
	}
	for provider, models := range over {
		if p[provider] == nil {
			p[provider] = map[string]Price{}
		}
		for model, price := range models {
			p[provider][model] = price
		}
	}
	return p, nil
}

// Cost prices one call. in is uncached input tokens; cacheRead and cacheWrite
// are counted separately because providers bill them at different rates.
func (p Prices) Cost(provider, model string, in, out, cacheRead, cacheWrite int) float64 {
	pr := p[provider][model]
	return (float64(in)*pr.Input + float64(out)*pr.Output +
		float64(cacheRead)*pr.CacheRead + float64(cacheWrite)*pr.CacheWrite) / 1e6
}
