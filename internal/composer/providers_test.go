package composer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/models"
)

// spy remembers the schema the composer asked a real adapter for.
type spy struct {
	models.Generator
	schema json.RawMessage
}

func (s *spy) Generate(ctx context.Context, system, user string, schema json.RawMessage, out any) (models.Usage, error) {
	s.schema = schema
	return s.Generator.Generate(ctx, system, user, schema, out)
}

// wire is one provider as its HTTP API looks: how a structured answer comes back, and where
// in the request the answer's schema and the forced choice go.
type wire struct {
	name   string
	build  func(url string) models.Generator
	path   string
	reply  func(answer string) string
	schema func(req map[string]any) any // the schema as sent
	forced func(req map[string]any) any // how the answer is forced, nil when the schema itself does it
	want   any
}

var wireDeps = models.Deps{Caller: models.NewCaller(1), Prices: models.DefaultPrices()}

var wires = []wire{
	{
		name: "anthropic",
		build: func(url string) models.Generator {
			d := wireDeps
			d.AnthropicURL = url
			return models.NewAnthropic(models.AnthropicAuth{APIKey: "sk-ant-test"}, "claude-haiku-4-5", d)
		},
		path: "/v1/messages",
		reply: func(answer string) string {
			return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"tool_use","id":"toolu_1","name":"answer","input":` +
				answer + `}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":900,"output_tokens":400}}`
		},
		schema: func(req map[string]any) any { return req["tools"].([]any)[0].(map[string]any)["input_schema"] },
		forced: func(req map[string]any) any { return req["tool_choice"] },
		want:   map[string]any{"type": "tool", "name": "answer"},
	},
	{
		name:  "openai",
		build: func(url string) models.Generator { return models.NewOpenAI(url, "sk-test", "test-model", wireDeps) },
		path:  "/chat/completions",
		reply: func(answer string) string {
			args, _ := json.Marshal(answer)
			return `{"id":"c1","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"answer","arguments":` +
				string(args) + `}}]}}],"usage":{"prompt_tokens":900,"completion_tokens":400,"total_tokens":1300}}`
		},
		schema: func(req map[string]any) any {
			return req["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"]
		},
		forced: func(req map[string]any) any { return req["tool_choice"] },
		want:   map[string]any{"type": "function", "function": map[string]any{"name": "answer"}},
	},
	{
		name:  "ollama",
		build: func(url string) models.Generator { return models.NewOllama(url, "test-local", wireDeps) },
		path:  "/api/chat",
		reply: func(answer string) string {
			c, _ := json.Marshal(answer)
			return `{"model":"test-local","message":{"role":"assistant","content":` + string(c) + `},"done":true,"prompt_eval_count":900,"eval_count":400}`
		},
		schema: func(req map[string]any) any { return req["format"] },
	},
}

// serve answers every request with reply and keeps the last request body.
func serve(t *testing.T, reply string) (url string, last func() (path string, body map[string]any)) {
	t.Helper()
	var mu sync.Mutex
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		path, body = r.URL.Path, nil
		_ = json.Unmarshal(raw, &body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() (string, map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		return path, body
	}
}

// checkRequest checks that the schema went where the provider reads it, unchanged, and
// that the answer was forced the provider's way.
func checkRequest(t *testing.T, w wire, path string, req map[string]any, asked json.RawMessage) {
	t.Helper()
	if path != w.path {
		t.Errorf("path = %q, want %q", path, w.path)
	}
	var want any
	if err := json.Unmarshal(asked, &want); err != nil {
		t.Fatal(err)
	}
	if got := w.schema(req); !reflect.DeepEqual(got, want) {
		t.Errorf("the schema sent differs from the one asked for:\n got %v\nwant %v", got, want)
	}
	if w.forced != nil && !reflect.DeepEqual(w.forced(req), w.want) {
		t.Errorf("the answer is not forced: %v", w.forced(req))
	}
}

// The composer writes rules through each provider's adapter: the draft schema reaches the
// provider in its own wire format, the drafts come back, and the call is on the ledger
// under the provider's name and the compose purpose.
func TestComposeOnEveryProvider(t *testing.T) {
	for _, w := range wires {
		t.Run(w.name, func(t *testing.T) {
			e := newEnv(t)
			e.deliver("noreply@swiggy.in", "order one")
			url, last := serve(t, w.reply(fiveDrafts))
			gen := &spy{Generator: w.build(url)}
			now := time.Unix(1_800_000_000, 0)
			out, err := Composer{Store: e.st, Gen: gen, Now: func() time.Time { return now }, BodyChars: 2000}.Compose(t.Context(),
				Request{Viewer: e.user.Viewer(), Text: paragraph, Account: &e.acct, Mailbox: e.mb})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Drafts) != 5 || out.Drafts[0].Name != "Food" || out.Drafts[0].MatchCount != 1 {
				t.Fatalf("drafts = %+v", out.Drafts)
			}
			path, req := last()
			checkRequest(t, w, path, req, gen.schema)
			if n := e.count(t, `SELECT COALESCE(SUM(calls), 0) FROM usage_daily WHERE purpose = 'compose' AND provider = '`+w.name+`' AND tokens_in = 900`); n != 1 {
				t.Errorf("%d compose calls on the ledger for %s", n, w.name)
			}
		})
	}
}
