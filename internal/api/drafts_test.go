package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// drafter is a module that writes draft replies; the daemon's worker is not running in these
// tests, so it is never called.
func drafter() ext.Module {
	return ext.Module{Name: "drafts", Drafts: &ext.Drafts{Write: func(context.Context, ext.DraftRequest) (string, error) { return "ok", nil }}}
}

const draftOn = `{"draft":{"instruction":"polite no","read_previous":true}}`

// The free build drafts nothing: the feature is off, a rule cannot start drafting, and a draft
// setting a rules file brought is kept, may be sent back as it is, and may be switched off.
func TestFreeBuildDraftsNothing(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML+"    draft: {instruction: say thanks}\n", http.StatusOK)
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["draft_replies"]; on != false {
		t.Errorf("features.draft_replies = %v without the module", on)
	}
	rule := e.call(http.MethodGet, "/api/rules/2", "", http.StatusOK)
	conform(t, e.doc, "RuleEnvelope", rule)
	if d := rule["rule"].(map[string]any)["draft"]; d == nil || d.(map[string]any)["instruction"] != "say thanks" {
		t.Fatalf("imported draft = %v", d)
	}
	e.refuse(http.MethodPatch, "/api/rules/1", draftOn, http.StatusBadRequest, "invalid_input", "draft")
	e.refuse(http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"x","intent":"x","actions":[{"type":"keep"}],"draft":{"instruction":"","read_previous":false}}]}`,
		http.StatusBadRequest, "invalid_input", "rules[0].draft")
	e.call(http.MethodPatch, "/api/rules/2", `{"draft":{"instruction":"say thanks","read_previous":false},"name":"Reading"}`, http.StatusOK)
	if d := e.call(http.MethodPatch, "/api/rules/2", `{"draft":null}`, http.StatusOK)["rule"].(map[string]any)["draft"]; d != nil {
		t.Errorf("draft after switching it off = %v", d)
	}
	if out := e.do(http.MethodGet, "/api/rules/export", ""); strings.Contains(string(out.raw), "draft") {
		t.Errorf("export still drafts:\n%s", out.raw)
	}
}

// With the module: a rule drafts replies, set by an edit or a batch save, and every email it
// acts on live (not in dry-run) queues one reply to write.
func TestDraftReplies(t *testing.T) {
	e := newEnv(t, drafter())
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["draft_replies"]; on != true {
		t.Errorf("features.draft_replies = %v with the module", on)
	}
	rule := e.call(http.MethodPatch, "/api/rules/1", `{"draft":{"instruction":"  polite no  ","read_previous":true}}`, http.StatusOK)
	conform(t, e.doc, "RuleEnvelope", rule)
	if d := rule["rule"].(map[string]any)["draft"].(map[string]any); d["instruction"] != "polite no" || d["read_previous"] != true {
		t.Errorf("draft = %v", d)
	}
	e.refuse(http.MethodPatch, "/api/rules/1", `{"draft":{"instruction":"`+strings.Repeat("a", 301)+`","read_previous":false}}`,
		http.StatusBadRequest, "rule_invalid", "draft.instruction")
	e.refuse(http.MethodPatch, "/api/rules/1", `{"draft":{"tone":"warm"}}`, http.StatusBadRequest, "invalid_input", "draft")
	items := e.call(http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"Recruiters","intent":"Recruiter outreach","actions":[{"type":"keep"}],
		"draft":{"instruction":"","read_previous":false}}]}`, http.StatusCreated)["items"].([]any)
	if d := items[len(items)-1].(map[string]any)["draft"]; d == nil {
		t.Errorf("batch-saved rule drafts nothing: %v", items[len(items)-1])
	}

	e.call(http.MethodPatch, "/api/settings", `{"dry_run":true}`, http.StatusOK)
	e.deliver("noreply@swiggy.in", "dry order")
	e.item("dry order", "acted")
	if got, _ := e.st.WaitingDrafts(t.Context(), 2_000_000_000, 10); len(got) != 0 {
		t.Fatalf("dry-run queued %+v", got)
	}
	e.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	e.deliver("Swiggy <noreply@swiggy.in>", "your order")
	row := e.item("your order", "acted")
	got, err := e.st.WaitingDrafts(t.Context(), 2_000_000_000, 10)
	if err != nil || len(got) != 1 || got[0].MessageID != id(row["id"]) || got[0].RuleID != 1 || got[0].Instruction != "polite no" || !got[0].ReadPrevious {
		t.Fatalf("draft jobs = %+v, %v", got, err)
	}
	e.call(http.MethodGet, "/api/activity?action=draft", "", http.StatusOK)
}

// A plan without draft replies hides the feature, and a rule cannot start drafting.
func TestPlanGatesDraftReplies(t *testing.T) {
	plans := salePlans()
	pro := plans[store.PlanPro]
	pro.Features = append(pro.Features, "draft_replies")
	plans[store.PlanPro] = pro
	e := newEnv(t, billingModule(&fakeBilling{}, plans), ext.Module{Name: "suggest"}, notifier(ext.DestinationWebhook), drafter())
	e.signIn()
	e.onPlan(store.PlanPersonal)
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["draft_replies"]; on != false {
		t.Errorf("features.draft_replies on Personal = %v", on)
	}
	r := e.do(http.MethodPatch, "/api/rules/1", draftOn)
	if r.status != http.StatusForbidden || r.body.Error.Code != "plan_required" || r.body.Error.Message != "Draft replies is part of Pro. Choose it on Plan and billing to use it." {
		t.Errorf("PATCH on Personal = %d %s", r.status, r.raw)
	}
	e.onPlan(store.PlanPro)
	e.call(http.MethodPatch, "/api/rules/1", draftOn, http.StatusOK)
}

func TestCheckModulesDrafts(t *testing.T) {
	for _, c := range []struct {
		name    string
		modules []ext.Module
		want    string
	}{
		{"one", []ext.Module{drafter()}, ""},
		{"no Write", []ext.Module{{Name: "d", Drafts: &ext.Drafts{}}}, "needs Write"},
		{"two modules", []ext.Module{drafter(), func() ext.Module { m := drafter(); m.Name = "other"; return m }()}, "more than one module writes draft replies"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckModules("selfhost", c.modules)
			if (c.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.want) {
				t.Errorf("CheckModules = %v, want %q", err, c.want)
			}
		})
	}
}

// What a reply came to reads in the email's outcome after what the rule did, and a reply
// that could not be written does not make the email's outcome a failure.
func TestOutcomeWithADraft(t *testing.T) {
	move := store.Action{ID: 1, DecisionID: 7, BatchID: 3, Kind: "move", Folder: "Food", Status: store.ActionDone}
	draft := store.Action{ID: 2, DecisionID: 7, BatchID: 3, Kind: store.ActionDraft, Folder: "Drafts", Status: store.ActionDone}
	with := func(d store.Action) store.ActivityRow { return store.ActivityRow{Actions: []store.Action{move, d}} }
	failed, undone, dry := draft, draft, draft
	failed.Status, failed.Error = store.ActionFailed, "no Drafts folder"
	undone.Status = store.ActionUndone
	dry.Status = store.ActionDryRun
	dryMove := move
	dryMove.Status = store.ActionDryRun
	for _, tt := range []struct {
		row  store.ActivityRow
		want string
	}{
		{with(draft), "Moved to Food · reply drafted"},
		{with(failed), "Moved to Food · no reply drafted"},
		{with(undone), "Moved to Food"},
		{store.ActivityRow{Actions: []store.Action{dryMove, dry}}, "Would move to Food · draft a reply"},
	} {
		if got := outcome(tt.row); got != tt.want {
			t.Errorf("outcome = %q, want %q", got, tt.want)
		}
	}
	if got := actionDetail(draft); got != "Drafted a reply in Drafts" {
		t.Errorf("detail = %q", got)
	}
	if got := toActionJSON(store.Action{Kind: store.ActionDraft, Folder: "Drafts", Status: store.ActionDone, After: &store.Snapshot{Folder: "Food"}}); got.ToFolder != "Drafts" {
		t.Errorf("to_folder = %q", got.ToFolder)
	}
}
