package settings

import (
	"context"
	"fmt"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// trash_to_folder and leave_own_mail are on unless they were turned off, on a new install
// and on one whose settings were saved before they existed; the screen and the step that
// acts on each (the executor, the pipeline) read the same value, and a reset puts the
// default back.
func TestOwnSwitches(t *testing.T) {
	off, on := false, true
	switches := []struct {
		key   string
		patch func(*bool) Patch
		view  func(View) bool
		read  func(*store.Store, context.Context, int64) (bool, error)
	}{
		{store.SettingTrashToFolder, func(b *bool) Patch { return Patch{TrashToFolder: b} }, func(v View) bool { return v.TrashToFolder }, (*store.Store).TrashToFolder},
		{store.SettingLeaveOwnMail, func(b *bool) Patch { return Patch{LeaveOwnMail: b} }, func(v View) bool { return v.LeaveOwnMail }, (*store.Store).LeaveOwnMail},
	}
	tests := []struct {
		name   string
		other  bool  // other settings were saved before, not this one
		stored *bool // this switch as stored before
		set    *bool // the change
		reset  bool
		want   bool
	}{
		{name: "a new install", want: true},
		{name: "an existing install, saved without it", other: true, want: true},
		{name: "turned off", set: &off, want: false},
		{name: "turned off, then on", stored: &off, set: &on, want: true},
		{name: "reset after being turned off", stored: &off, reset: true, want: true},
	}
	for _, sw := range switches {
		for _, tt := range tests {
			t.Run(sw.key+"/"+tt.name, func(t *testing.T) {
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
				rows := map[string]string{}
				if tt.other {
					rows["dry_run"], rows["retention_days"] = "false", "90"
				}
				if tt.stored != nil {
					rows[sw.key] = fmt.Sprint(*tt.stored)
				}
				for k, v := range rows {
					if err := st.SetSetting(ctx, 1, k, v); err != nil {
						t.Fatal(err)
					}
				}
				env, err := config.Load(nil, func(string) string { return "" })
				if err != nil {
					t.Fatal(err)
				}
				s := &Settings{Store: st, Master: make([]byte, 32), Env: env}
				p := sw.patch(tt.set)
				if tt.reset {
					p.Reset = []string{sw.key}
				}
				if err := s.Apply(ctx, 1, p); err != nil {
					t.Fatal(err)
				}
				v, err := s.View(ctx, 1)
				if err != nil {
					t.Fatal(err)
				}
				acting, err := sw.read(st, ctx, 1)
				if err != nil || sw.view(v) != tt.want || acting != tt.want {
					t.Errorf("the screen reads %v, the step that acts on it %v (%v); want %v", sw.view(v), acting, err, tt.want)
				}
			})
		}
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
		for _, n := range providerNeeds[decider] {
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
