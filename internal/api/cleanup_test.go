package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// folderCount is how many emails a folder of the fake mailbox holds; -1 when it does not exist.
func (e *env) folderCount(name string) int {
	e.t.Helper()
	refs, err := e.mb.FetchSince(e.t.Context(), name, time.Time{}, 0)
	if err != nil {
		return -1
	}
	return len(refs)
}

// batchDone waits until a cleanup batch has ended and returns it.
func (e *env) batchDone(batchID int64) map[string]any {
	e.t.Helper()
	var b map[string]any
	eventually(e.t, "the cleanup to finish", func() bool {
		b = e.call(http.MethodGet, fmt.Sprintf("/api/batches/%d", batchID), "", http.StatusOK)["batch"].(map[string]any)
		return b["status"] != "running"
	})
	return b
}

// The milestone's demo: a thousand emails that were already there are previewed, sorted in
// dry-run (nothing moves), sorted for real (they move), and put back with one batch undo.
func TestCleanupDryRunThenLiveThenUndo(t *testing.T) {
	e := newEnv(t)
	for i := range 1000 {
		switch {
		case i%2 == 0:
			e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i)) // 500: the Food rule's conditions
		case i%10 < 7:
			e.deliver("hello@news.example", fmt.Sprintf("Reading issue %d", i)) // 300: the model says Reading
		default:
			e.deliver("friend@example.org", fmt.Sprintf("lunch %d", i)) // 200: the model says none
		}
	}
	// Dry-run is the default: the account is connected and nothing may move yet.
	e.signIn()
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	e.live()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	e.decider.DecideFunc = bySubject("The subject says ")

	// Preview: counts per rule and samples, with no model asked.
	prev := e.call(http.MethodPost, "/api/cleanup/preview", `{"account_id":1}`, http.StatusOK)
	conform(t, e.doc, "CleanupPreview", prev)
	groups := prev["groups"].([]any)
	if prev["total"] != float64(1000) || len(groups) != 2 || prev["estimated_model_calls"] != float64(500) || prev["estimated_cost_usd"] != float64(0) {
		t.Fatalf("preview = total %v, %d groups, %v calls, cost %v", prev["total"], len(groups), prev["estimated_model_calls"], prev["estimated_cost_usd"])
	}
	for _, g := range groups {
		g := g.(map[string]any)
		switch g["outcome"] {
		case "rule":
			if id(g["rule_id"]) != 1 || g["rule_name"] != "Food" || g["count"] != float64(500) || len(g["samples"].([]any)) != 5 ||
				g["samples"].([]any)[0].(map[string]any)["subject"] != "order 998" {
				t.Errorf("rule group = %v", g)
			}
		case "model":
			if g["rule_id"] != nil || g["count"] != float64(500) || len(g["samples"].([]any)) != 5 {
				t.Errorf("model group = %v", g)
			}
		default:
			t.Errorf("unexpected group %v", g)
		}
	}
	if small := e.call(http.MethodPost, "/api/cleanup/preview", `{"account_id":1,"limit":10,"folder":"INBOX","since":null}`, http.StatusOK); small["total"] != float64(10) {
		t.Errorf("preview of the newest 10 = %v", small["total"])
	}
	if none := e.call(http.MethodPost, "/api/cleanup/preview", fmt.Sprintf(`{"account_id":1,"since":%d}`, time.Now().Add(48*time.Hour).Unix()), http.StatusOK); none["total"] != float64(0) || len(none["groups"].([]any)) != 0 {
		t.Errorf("preview of mail from the future = %v", none)
	}
	if e.count(`SELECT COUNT(*) FROM messages`)+e.count(`SELECT COUNT(*) FROM usage_daily`) != 0 {
		t.Error("the preview recorded messages or model calls")
	}
	for body, path := range map[string]string{`{"account_id":9}`: "account_id", `{"account_id":1,"limit":0}`: "limit", `{"account_id":1,"since":-5}`: "since", `{"account_id":1,"folder":"Nope"}`: "folder"} {
		e.refuse(http.MethodPost, "/api/cleanup/preview", body, http.StatusBadRequest, "invalid_input", path)
		e.refuse(http.MethodPost, "/api/cleanup/run", body, http.StatusBadRequest, "invalid_input", path)
	}

	// Follow the event stream as a browser would, keeping the progress events.
	_, live, cancel := e.hub.Subscribe(0)
	defer cancel()
	progress := make(chan store.Batch, 1000)
	go func() {
		for ev := range live {
			if b, ok := ev.Data.(store.Batch); ok && ev.Name == events.BatchProgress {
				progress <- b
			}
		}
	}()
	// drain returns the progress events of one run, up to the one that ends it.
	drain := func(batchID int64) []store.Batch {
		t.Helper()
		var got []store.Batch
		for {
			select {
			case b := <-progress:
				if b.ID != batchID {
					continue
				}
				if got = append(got, b); b.Status != store.BatchRunning {
					return got
				}
			case <-time.After(60 * time.Second):
				t.Fatalf("no end of batch %d after %d progress events", batchID, len(got))
			}
		}
	}

	// Run 1, in dry-run: every email is decided and recorded, and nothing moves.
	run := e.call(http.MethodPost, "/api/cleanup/run", `{"account_id":1}`, http.StatusAccepted)["batch"].(map[string]any)
	conform(t, e.doc, "Batch", run)
	dry := id(run["id"])
	if run["kind"] != "cleanup" || run["status"] != "running" || run["total"] != float64(1000) || id(run["account_id"]) != 1 || run["folder"] != "INBOX" || run["since"] != nil {
		t.Fatalf("started = %v", run)
	}
	e.refuse(http.MethodPost, "/api/cleanup/run", `{"account_id":1}`, http.StatusConflict, "cleanup_running", "")
	seen := drain(dry)
	last := seen[len(seen)-1]
	// The model is asked about every email from the friend, but only about the first three
	// newsletters: after three confident, identical decisions the sender is learned.
	const calls = 200 + 3
	if len(seen) != 100 || last.Status != store.BatchDone || last.Done != 1000 || last.Total != 1000 || last.Tokens != calls*105 || last.CostUSD < 0.2029 || last.CostUSD > 0.2031 ||
		!slices.IsSortedFunc(seen, func(a, b store.Batch) int { return a.Done - b.Done }) || seen[0].Done != 10 || seen[0].Total != 1000 {
		t.Fatalf("%d progress events; first %+v, last %+v", len(seen), seen[0], last)
	}
	b := e.batchDone(dry)
	conform(t, e.doc, "Batch", b)
	if acts := b["actions"].(map[string]any); b["status"] != "done" || b["done"] != float64(1000) || b["tokens"] != float64(calls*105) ||
		acts["dry_run"] != float64(500*2+300) || acts["done"] != float64(0) {
		t.Fatalf("dry-run batch = %v", b)
	}
	if e.folderCount("INBOX") != 1000 || e.hasFolder("Food") || e.hasFolder("Reading") {
		t.Fatalf("dry-run moved mail: INBOX holds %d", e.folderCount("INBOX"))
	}

	// Run 2, live: the same mail is decided afresh and now moves.
	e.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	liveRun := id(e.call(http.MethodPost, "/api/cleanup/run", `{"account_id":1,"folder":"INBOX"}`, http.StatusAccepted)["batch"].(map[string]any)["id"])
	if seen = drain(liveRun); seen[len(seen)-1].Status != store.BatchDone {
		t.Fatalf("live run ended %+v", seen[len(seen)-1])
	}
	b = e.batchDone(liveRun)
	if acts := b["actions"].(map[string]any); b["status"] != "done" || acts["done"] != float64(1300) || acts["failed"] != float64(0) {
		t.Fatalf("live batch = %v", b)
	}
	if e.folderCount("INBOX") != 200 || e.folderCount("Food") != 500 || e.folderCount("Reading") != 300 {
		t.Fatalf("after the live run: INBOX %d, Food %d, Reading %d", e.folderCount("INBOX"), e.folderCount("Food"), e.folderCount("Reading"))
	}
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 1000 {
		t.Errorf("%d message rows after two runs over the same 1,000 emails", n)
	}
	// With history on the ledger the preview can price what is left.
	if left := e.call(http.MethodPost, "/api/cleanup/preview", `{"account_id":1}`, http.StatusOK); left["total"] != float64(200) ||
		left["estimated_model_calls"] != float64(200) || left["estimated_cost_usd"].(float64) < 0.19 || left["estimated_cost_usd"].(float64) > 0.21 {
		t.Errorf("preview of what is left = %v calls, %v USD", left["estimated_model_calls"], left["estimated_cost_usd"])
	}

	// The Cleanup screen lists both runs, newest first.
	list := e.call(http.MethodGet, "/api/batches?kind=cleanup", "", http.StatusOK)
	items := list["items"].([]any)
	conform(t, e.doc, "Batch", items[0])
	if len(items) != 2 || id(items[0].(map[string]any)["id"]) != liveRun || list["next_cursor"] != nil {
		t.Fatalf("batches = %v", list)
	}
	page := e.call(http.MethodGet, "/api/batches?limit=1", "", http.StatusOK)
	if cursor, _ := page["next_cursor"].(string); cursor != fmt.Sprint(liveRun) ||
		id(e.call(http.MethodGet, "/api/batches?limit=1&cursor="+cursor, "", http.StatusOK)["items"].([]any)[0].(map[string]any)["id"]) != dry {
		t.Errorf("paging batches: %v", page)
	}
	if got := e.call(http.MethodGet, "/api/batches?kind=undo", "", http.StatusOK)["items"].([]any); len(got) != 0 {
		t.Errorf("undo batches = %v", got)
	}
	for query, path := range map[string]string{"kind=big": "kind", "limit=0": "limit", "cursor=x": "cursor"} {
		e.refuse(http.MethodGet, "/api/batches?"+query, "", http.StatusBadRequest, "invalid_input", path)
	}

	// One batch undo puts every email back where it was, unread.
	undo := e.call(http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", liveRun), "", http.StatusOK)
	conform(t, e.doc, "UndoResult", undo)
	if undo["undone"] != float64(1300) || undo["failed"] != float64(0) || undo["batch"].(map[string]any)["status"] != "undone" {
		t.Fatalf("undo = %v", undo)
	}
	if e.folderCount("INBOX") != 1000 || e.folderCount("Food") != 0 || e.folderCount("Reading") != 0 {
		t.Fatalf("after the undo: INBOX %d, Food %d, Reading %d", e.folderCount("INBOX"), e.folderCount("Food"), e.folderCount("Reading"))
	}
	refs, _ := e.mb.FetchSince(t.Context(), "INBOX", time.Time{}, 0)
	for _, ref := range refs {
		if flags, _ := e.mb.Flags(t.Context(), ref); len(flags) != 0 {
			t.Fatalf("email %d still has flags %v after the undo", ref.UID, flags)
		}
	}
	// The emails that came back are known: when the next email arrives, live sorting passes
	// over them and sorts only the new one.
	e.deliver("noreply@swiggy.in", "a new order")
	e.item("a new order", "acted")
	eventually(t, "the returned mail to be passed over", func() bool {
		f, err := e.st.Folder(t.Context(), 1, "INBOX")
		return err == nil && f.LastUID >= 1801
	})
	if n := e.count(`SELECT COUNT(*) FROM messages`); n != 1001 || e.folderCount("INBOX") != 1000 || e.folderCount("Food") != 1 {
		t.Errorf("after the undo settled: %d message rows, INBOX holds %d, Food %d", n, e.folderCount("INBOX"), e.folderCount("Food"))
	}

	// An offline account cannot be previewed or cleaned.
	e.call(http.MethodPatch, "/api/accounts/1", `{"paused":true}`, http.StatusOK)
	e.refuse(http.MethodPost, "/api/cleanup/preview", `{"account_id":1}`, http.StatusConflict, "account_offline", "")
	e.refuse(http.MethodPost, "/api/cleanup/run", `{"account_id":1}`, http.StatusConflict, "account_offline", "")
}

// Every template in the gallery is a rule the batch endpoint accepts as it is.
func TestTemplates(t *testing.T) {
	e := newEnv(t)
	e.noDecider = true // adding a template needs no model
	e.connect()
	r := e.do(http.MethodGet, "/api/templates", "")
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("templates = %d %s", r.status, r.header.Get("Content-Type"))
	}
	items := r.object(t)["items"].([]any)
	var ids []string
	for _, it := range items {
		tpl := it.(map[string]any)
		conform(t, e.doc, "Template", tpl)
		ids = append(ids, tpl["id"].(string))
		body, _ := json.Marshal(map[string]any{"rules": []any{tpl["rule"]}})
		saved := e.call(http.MethodPost, "/api/rules/batch", string(body), http.StatusCreated)["items"].([]any)
		if saved[0].(map[string]any)["name"] != tpl["name"] || tpl["description"] == "" {
			t.Errorf("template %v saved as %v", tpl["id"], saved[0])
		}
	}
	want := []string{"newsletters", "receipts", "login-codes", "cold-sales", "travel", "social-notifications", "bank-statements", "calendar-invites"}
	if !slices.Equal(ids, want) {
		t.Errorf("templates = %v, want %v", ids, want)
	}
	if n := len(e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any)); n != 8 {
		t.Errorf("%d rules after adding every template", n)
	}
	for _, folder := range []string{"Reading", "Receipts", "Travel", "Finance"} {
		if !e.hasFolder(folder) {
			t.Errorf("adding the templates did not create %s", folder)
		}
	}
}
