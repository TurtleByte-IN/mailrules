package settings

import (
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// trash_to_folder is on unless it was turned off, on a new install and on one whose
// settings were saved before the setting existed; the screen and the executor read the
// same value, and a reset puts the default back.
func TestTrashToFolder(t *testing.T) {
	off, on := false, true
	tests := []struct {
		name  string
		rows  map[string]string // stored before the daemon reads them
		patch Patch
		want  bool
	}{
		{"a new install", nil, Patch{}, true},
		{"an existing install, saved without it", map[string]string{"dry_run": "false", "retention_days": "90"}, Patch{}, true},
		{"turned off", nil, Patch{TrashToFolder: &off}, false},
		{"turned off, then on", map[string]string{store.SettingTrashToFolder: "false"}, Patch{TrashToFolder: &on}, true},
		{"reset after being turned off", map[string]string{store.SettingTrashToFolder: "false"}, Patch{Reset: []string{store.SettingTrashToFolder}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			db, err := store.Open(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := store.Migrate(ctx, db); err != nil {
				t.Fatal(err)
			}
			st := store.New(db)
			for k, v := range tt.rows {
				if err := st.SetSetting(ctx, k, v); err != nil {
					t.Fatal(err)
				}
			}
			env, err := config.Load(nil, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			s := &Settings{Store: st, Master: make([]byte, 32), Env: env}
			if err := s.Apply(ctx, tt.patch); err != nil {
				t.Fatal(err)
			}
			v, err := s.View(ctx)
			if err != nil {
				t.Fatal(err)
			}
			exec, err := st.TrashToFolder(ctx)
			if err != nil || v.TrashToFolder != tt.want || exec != tt.want {
				t.Errorf("the screen reads %v, the executor %v (%v); want %v", v.TrashToFolder, exec, err, tt.want)
			}
		})
	}
}

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
