package settings

import (
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
)

// The warnings the API shows and the readiness check the daemon logs at startup name the
// same needs: two lists that must agree, so this fails when they part ways.
func TestWarningsAgreeWithDeciderReady(t *testing.T) {
	for _, decider := range Deciders {
		empty := config.Config{Decider: decider}
		var missing int
		if joined, ok := empty.DeciderReady().(interface{ Unwrap() []error }); ok {
			missing = len(joined.Unwrap())
		}
		ws := warnings(empty)
		if len(ws) != missing || missing == 0 {
			t.Errorf("%s with nothing set: %d warnings, DeciderReady misses %d (want the same, and at least one)", decider, len(ws), missing)
		}
		full := empty
		for _, n := range deciderNeeds[decider] {
			if n.path == "" || n.what == "" {
				t.Errorf("%s: a need without a path or a name", decider)
			}
		}
		full.OpenRouterAPIKey, full.CloudflareAccountID, full.CloudflareAPIToken, full.AnthropicAPIKey, full.OpenAIAPIKey, full.OllamaURL = "k", "k", "k", "k", "k", "http://o"
		if ws := warnings(full); len(ws) != 0 || full.DeciderReady() != nil {
			t.Errorf("%s with everything set: warnings %v, DeciderReady %v", decider, ws, full.DeciderReady())
		}
	}
}
