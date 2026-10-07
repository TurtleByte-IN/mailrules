package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// folderCount is how many emails a folder of the fake mailbox holds; -1 when it does not exist.
func (e *env) folderCount(name string) int {
	e.t.Helper()
	refs, _, err := e.mb.FetchSince(e.t.Context(), name, time.Time{}, 0)
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

// cleanupRules is the Food condition rule (no model) and the Reading rule, which has an
// intent and a condition that makes it a candidate only for news.example mail.
const cleanupRules = `{"rules":[
	{"name":"Food","conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"}]},
	{"name":"Reading","intent":"Newsletters and promotions","conditions":{"field":"from_domain","op":"eq","value":"news.example"},"actions":[{"type":"move","folder":"Reading"}]}]}`

// cleanupDeciderFunc picks the candidate named in the subject; "maybe" makes it unsure, so
// the email waits in Needs review.
func cleanupDeciderFunc(req models.DecideRequest) (models.Decision, models.Usage, error) {
	u := models.Usage{Provider: "fake", Model: "fake-1", TokensIn: 100, TokensOut: 5, CostUSD: 0.001}
	subj := strings.ToLower(req.Email.Subject)
	for _, c := range req.Candidates {
		if strings.Contains(subj, strings.ToLower(c.Name)) {
			conf := 0.95
			if strings.Contains(subj, "maybe") {
				conf = 0.4
			}
			return models.Decision{RuleID: c.RuleID, Confidence: conf, Reason: "the subject mentions " + c.Name}, u, nil
		}
	}
	return models.Decision{Confidence: 0.9}, u, nil // none of them
}

// startCheck POSTs a cleanup check and returns the started (running) check.
func (e *env) startCheck(body string) map[string]any {
	e.t.Helper()
	return e.call(http.MethodPost, "/api/cleanup/check", body, http.StatusAccepted)["check"].(map[string]any)
}

// getCheck GETs the account's current check, or nil.
func (e *env) getCheck(accountID int64) map[string]any {
	e.t.Helper()
	got := e.call(http.MethodGet, fmt.Sprintf("/api/cleanup/check?account_id=%d", accountID), "", http.StatusOK)["check"]
	if got == nil {
		return nil
	}
	return got.(map[string]any)
}

// checkReady runs a check and waits until it is ready, stale or failed.
func (e *env) checkReady(body string) map[string]any {
	e.t.Helper()
	acct := id(e.startCheck(body)["account_id"])
	var chk map[string]any
	eventually(e.t, "the check to settle", func() bool {
		chk = e.getCheck(acct)
		s := chk["status"]
		return s == "ready" || s == "stale" || s == "failed"
	})
	return chk
}

// rowsOf returns a check's rows as maps.
func rowsOf(chk map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range chk["rows"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

// The milestone's demo, on the new contract: a few hundred emails are checked for real (one
// paid run), the rows carry each outcome, then the kept rows are sorted — replaying the
// check's answers with no new model calls — and one batch undo puts everything back.
func TestCleanupCheckThenSortThenUndo(t *testing.T) {
	e := newEnv(t)
	// Deliver oldest first; the check returns them newest first.
	const food, reading, review, none, block = 60, 50, 10, 20, 10
	for i := range food {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("food order %d", i))
	}
	for i := range reading {
		e.deliver("hello@news.example", fmt.Sprintf("reading issue %d", i))
	}
	for i := range review {
		e.deliver("hello@news.example", fmt.Sprintf("maybe reading %d", i))
	}
	for i := range none {
		e.deliver("friend@example.org", fmt.Sprintf("lunch %d", i))
	}
	for i := range block {
		e.deliver("spam@junk.example", fmt.Sprintf("deal %d", i))
	}
	total := food + reading + review + none + block

	e.connect() // signs in, dry-run off, account 1 live
	e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)
	e.call(http.MethodPut, "/api/senders/domain/junk.example", `{"verdict":"block"}`, http.StatusOK)
	e.decider.DecideFunc = cleanupDeciderFunc

	// Follow the stream as a browser would, keeping the check and batch progress events.
	_, live, cancel := e.hub.Subscribe(0)
	defer cancel()
	checks := make(chan worker.CheckState, 2000)
	batches := make(chan store.Batch, 2000)
	go func() {
		for ev := range live {
			switch ev.Name {
			case events.CheckProgress:
				checks <- ev.Data.(worker.CheckState)
			case events.BatchProgress:
				batches <- ev.Data.(store.Batch)
			}
		}
	}()

	// Start the check: it answers at once, running, with no rows.
	started := e.startCheck(`{"account_id":1}`)
	conform(t, e.doc, "CleanupCheck", started)
	if started["status"] != "running" || id(started["account_id"]) != 1 || started["folder"] != "INBOX" ||
		len(started["rows"].([]any)) != 0 || started["since"] != nil || started["limit"] != float64(composer.MaxLimit) || started["matched"] != float64(0) {
		t.Fatalf("started check = %v", started)
	}

	// Drain the check progress: first 0 of N, last N of N ready, with calls and cost growing.
	var seen []worker.CheckState
	for {
		select {
		case st := <-checks:
			seen = append(seen, st)
			if st.Status != worker.CheckRunning {
				goto checked
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("the check did not finish after %d progress events", len(seen))
		}
	}
checked:
	first, last := seen[0], seen[len(seen)-1]
	const calls = reading + review // only news.example mail reaches the model
	if first.Done != 0 || first.Total != total || first.ModelCalls != 0 {
		t.Fatalf("first progress = %+v", first)
	}
	if last.Status != worker.CheckReady || last.Done != total || last.Total != total || last.ModelCalls != calls ||
		last.Tokens != calls*105 || last.CostUSD < 0.0599 || last.CostUSD > 0.0601 || len(last.Rows) != 0 {
		t.Fatalf("last progress = %+v", last)
	}
	if !slices.IsSortedFunc(seen, func(a, b worker.CheckState) int { return a.ModelCalls - b.ModelCalls }) {
		t.Fatalf("model calls did not grow monotonically across %d events", len(seen))
	}

	// GET the ready check: its rows carry every outcome.
	chk := e.getCheck(1)
	conform(t, e.doc, "CleanupCheck", chk)
	if chk["status"] != "ready" || id(chk["model_calls"]) != calls || id(chk["tokens"]) != calls*105 {
		t.Fatalf("ready check = %v", chk)
	}
	rows := rowsOf(chk)
	if len(rows) != total {
		t.Fatalf("%d rows, want %d", len(rows), total)
	}
	foodID := id(e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any)[0].(map[string]any)["id"])
	readingID := id(e.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any)[1].(map[string]any)["id"])

	byStage := map[string][]map[string]any{}
	selectable, excludeReading := 0, []int{}
	for _, r := range rows {
		kind := r["stage"].(string)
		if r["selectable"].(bool) {
			selectable++
		}
		switch {
		case kind == "sender":
			byStage["sender"] = append(byStage["sender"], r)
		case kind == "condition":
			byStage["condition"] = append(byStage["condition"], r)
		case kind == "decider" && r["selectable"].(bool):
			byStage["decider"] = append(byStage["decider"], r)
			if len(excludeReading) < 20 {
				excludeReading = append(excludeReading, int(id(r["index"])))
			}
		case kind == "decider": // below the threshold: Needs review
			byStage["review"] = append(byStage["review"], r)
		case kind == "none":
			byStage["none"] = append(byStage["none"], r)
		}
	}
	if selectable != food+reading+block {
		t.Errorf("%d selectable rows, want %d", selectable, food+reading+block)
	}
	if len(byStage["sender"]) != block || len(byStage["condition"]) != food || len(byStage["decider"]) != reading ||
		len(byStage["review"]) != review || len(byStage["none"]) != none {
		t.Fatalf("row stages = %d sender, %d condition, %d decider, %d review, %d none",
			len(byStage["sender"]), len(byStage["condition"]), len(byStage["decider"]), len(byStage["review"]), len(byStage["none"]))
	}
	// Sender block: a trash action, no model, selectable.
	if r := byStage["sender"][0]; r["rule_id"] != nil || r["rule_name"] != "Sender rule: trash" || r["confidence"] != nil ||
		len(r["actions"].([]any)) != 1 || r["actions"].([]any)[0].(map[string]any)["type"] != "trash" || r["reason"] != "Sender rule: trash" {
		t.Errorf("sender row = %v", r)
	}
	// Condition: the Food rule, no model asked, so confidence is null.
	if r := byStage["condition"][0]; id(r["rule_id"]) != foodID || r["rule_name"] != "Food" || r["confidence"] != nil ||
		r["actions"].([]any)[0].(map[string]any)["folder"] != "Food" || !strings.Contains(r["reason"].(string), `Matched "Food"`) {
		t.Errorf("condition row = %v", r)
	}
	// Decider: the model picked Reading with a confidence.
	if r := byStage["decider"][0]; id(r["rule_id"]) != readingID || r["rule_name"] != "Reading" || r["confidence"].(float64) != 0.95 ||
		r["actions"].([]any)[0].(map[string]any)["folder"] != "Reading" || !strings.Contains(r["reason"].(string), "Reading") {
		t.Errorf("decider row = %v", r)
	}
	// Review: a model confidence, but not selectable and no action.
	if r := byStage["review"][0]; r["selectable"].(bool) || r["review"] != true || r["confidence"].(float64) != 0.4 || len(r["actions"].([]any)) != 0 {
		t.Errorf("review row = %v", r)
	}
	// None: no model, not selectable, no rule.
	if r := byStage["none"][0]; r["selectable"].(bool) || r["review"] != false || r["rule_id"] != nil || r["rule_name"] != "" || r["confidence"] != nil ||
		len(r["actions"].([]any)) != 0 || r["reason"] != "No rule matched" {
		t.Errorf("none row = %v", r)
	}

	// Sorting makes no model calls: record the count the check left.
	callsBefore := len(e.decider.Requests())
	if callsBefore != calls {
		t.Fatalf("the check made %d model calls, want %d", callsBefore, calls)
	}
	usageRowsBefore := e.count(`SELECT COUNT(*) FROM usage_daily`)

	// Run the kept rows, unticking 20 of the Reading rows.
	body, _ := json.Marshal(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": excludeReading})
	run := e.call(http.MethodPost, "/api/cleanup/run", string(body), http.StatusAccepted)["batch"].(map[string]any)
	conform(t, e.doc, "Batch", run)
	// The batch remembers what the check covered, so its label can be truthful (MAI-48).
	if id(run["limit"]) != composer.MaxLimit || id(run["matched"]) != int64(total) || run["since"] != nil {
		t.Errorf("batch covered limit %v, matched %v, since %v; want %d, %d, null", run["limit"], run["matched"], run["since"], composer.MaxLimit, total)
	}
	batchID := id(run["id"])
	sorted := food + (reading - 20) + block
	if run["kind"] != "cleanup" || run["status"] != "running" || id(run["total"]) != int64(sorted) || id(run["account_id"]) != 1 {
		t.Fatalf("run = %v", run)
	}
	// Drain the batch progress; the last event ends it.
	var bseen []store.Batch
	for {
		select {
		case b := <-batches:
			if b.ID != batchID {
				continue
			}
			bseen = append(bseen, b)
			if b.Status != store.BatchRunning {
				goto ran
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("the sort did not finish after %d events", len(bseen))
		}
	}
ran:
	done := bseen[len(bseen)-1]
	if done.Status != store.BatchDone || done.Done != sorted || done.Skipped != 0 {
		t.Fatalf("sort ended %+v", done)
	}
	// The used check is gone, and no model call was made by the Sort.
	if e.getCheck(1) != nil {
		t.Error("the check was not cleared after the sort")
	}
	if n := len(e.decider.Requests()); n != callsBefore {
		t.Errorf("the sort made %d model calls", n-callsBefore)
	}
	if n := e.count(`SELECT COUNT(*) FROM usage_daily`); n != usageRowsBefore {
		t.Errorf("the sort added %d usage_daily rows", n-usageRowsBefore)
	}

	// Only the selected rows moved: the 20 unticked Reading emails stayed.
	if e.folderCount("Food") != food || e.folderCount("Reading") != reading-20 || e.folderCount("Trash") != block ||
		e.folderCount("INBOX") != total-food-(reading-20)-block {
		t.Fatalf("after the sort: Food %d, Reading %d, Trash %d, INBOX %d",
			e.folderCount("Food"), e.folderCount("Reading"), e.folderCount("Trash"), e.folderCount("INBOX"))
	}

	// The check booked its calls once, under "cleanup" — not "decide" — and the Sort added none.
	var cleanupCalls, decideCalls int
	if err := e.db.QueryRowContext(t.Context(), `SELECT COALESCE(SUM(CASE WHEN purpose='cleanup' THEN calls END),0),
		COALESCE(SUM(CASE WHEN purpose='decide' THEN calls END),0) FROM usage_daily`).Scan(&cleanupCalls, &decideCalls); err != nil {
		t.Fatal(err)
	}
	if cleanupCalls != calls || decideCalls != 0 {
		t.Errorf("ledger = %d cleanup calls, %d decide calls; want %d, 0", cleanupCalls, decideCalls, calls)
	}
	// A model-decided email counts as such; a condition- or sender-decided one counts as free.
	tot, err := e.st.StatsTotals(t.Context(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tot.Processed != sorted || tot.WithoutModel != food+block || tot.Processed-tot.WithoutModel != reading-20 {
		t.Fatalf("totals = processed %d, without model %d; want %d, %d", tot.Processed, tot.WithoutModel, sorted, food+block)
	}

	// The batch is a cleanup batch and undoes as one: everything goes back.
	undo := e.call(http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", batchID), "", http.StatusOK)
	conform(t, e.doc, "UndoResult", undo)
	if undo["undone"] != float64(sorted) || undo["failed"] != float64(0) || undo["batch"].(map[string]any)["status"] != "undone" {
		t.Fatalf("undo = %v", undo)
	}
	if e.folderCount("INBOX") != total || e.folderCount("Food") != 0 || e.folderCount("Reading") != 0 || e.folderCount("Trash") != 0 {
		t.Fatalf("after the undo: INBOX %d, Food %d, Reading %d, Trash %d",
			e.folderCount("INBOX"), e.folderCount("Food"), e.folderCount("Reading"), e.folderCount("Trash"))
	}
}

// A sort refuses a check that is no longer current, not ready, or run against rules that
// have changed since.
func TestCleanupRunRefusals(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
	}
	for i := range 3 {
		e.deliver("hello@news.example", fmt.Sprintf("reading digest %d", i)) // these reach the model
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)
	e.decider.DecideFunc = cleanupDeciderFunc

	// The body is validated before anything else.
	e.refuse(http.MethodPost, "/api/cleanup/run", `{"account_id":9,"check_id":"1"}`, http.StatusBadRequest, "invalid_input", "account_id")
	e.refuse(http.MethodPost, "/api/cleanup/run", `{"account_id":1}`, http.StatusBadRequest, "invalid_input", "check_id")
	// No check at all, or an id that is not the current one: preview_stale.
	e.refuse(http.MethodPost, "/api/cleanup/run", `{"account_id":1,"check_id":"nope"}`, http.StatusConflict, "preview_stale", "")

	chk := e.checkReady(`{"account_id":1}`)
	// A newer check replaces the old one: the old id is refused.
	newer := e.checkReady(`{"account_id":1}`)
	body, _ := json.Marshal(map[string]any{"account_id": 1, "check_id": chk["id"]})
	e.refuse(http.MethodPost, "/api/cleanup/run", string(body), http.StatusConflict, "preview_stale", "")

	// Editing a rule makes the current check stale: GET says so, and the run is refused.
	e.call(http.MethodPatch, "/api/rules/1", `{"actions":[{"type":"archive"}]}`, http.StatusOK)
	stale := e.getCheck(1)
	if stale["status"] != "stale" {
		t.Fatalf("after a rule edit the check is %v, want stale", stale["status"])
	}
	body, _ = json.Marshal(map[string]any{"account_id": 1, "check_id": newer["id"]})
	e.refuse(http.MethodPost, "/api/cleanup/run", string(body), http.StatusConflict, "preview_stale", "")

	// A sender-rule change also makes a fresh check stale.
	fresh := e.checkReady(`{"account_id":1}`)
	e.call(http.MethodPut, "/api/senders/domain/junk.example", `{"verdict":"block"}`, http.StatusOK)
	if s := e.getCheck(1)["status"]; s != "stale" {
		t.Errorf("after a sender change the check is %v, want stale", s)
	}
	body, _ = json.Marshal(map[string]any{"account_id": 1, "check_id": fresh["id"]})
	e.refuse(http.MethodPost, "/api/cleanup/run", string(body), http.StatusConflict, "preview_stale", "")

	// A failed check is not ready to sort.
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{}, models.Usage{}, fmt.Errorf("the model is down")
	}
	failed := e.checkReady(`{"account_id":1}`)
	if failed["status"] != "failed" || failed["error"] == "" {
		t.Fatalf("check with a broken model = %v", failed)
	}
	body, _ = json.Marshal(map[string]any{"account_id": 1, "check_id": failed["id"]})
	e.refuse(http.MethodPost, "/api/cleanup/run", string(body), http.StatusConflict, "check_not_ready", "")

	// Discarding the check leaves none.
	e.call(http.MethodDelete, "/api/cleanup/check?account_id=1", "", http.StatusNoContent)
	if e.getCheck(1) != nil {
		t.Error("the check was not discarded")
	}
}

// The user's ticks are kept with the check, in the daemon's memory, so a reload shows the
// same table: saved, returned by GET, validated, and gone when the check is replaced,
// discarded or used by Sort. Sort takes its own explicit list, never the saved one.
func TestCleanupSelectionIsKeptWithTheCheck(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
	}
	for i := range 3 {
		e.deliver("hello@news.example", fmt.Sprintf("reading digest %d", i))
	}
	for i := range 2 {
		e.deliver("friend@example.org", fmt.Sprintf("lunch %d", i)) // no rule: not selectable
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)
	e.decider.DecideFunc = cleanupDeciderFunc

	put := func(body map[string]any, status int) {
		e.t.Helper()
		b, _ := json.Marshal(body)
		e.call(http.MethodPut, "/api/cleanup/check/selection", string(b), status)
	}
	refusePut := func(body map[string]any, status int, code, path string) {
		e.t.Helper()
		b, _ := json.Marshal(body)
		e.refuse(http.MethodPut, "/api/cleanup/check/selection", string(b), status, code, path)
	}
	excludeOf := func(chk map[string]any) []int {
		e.t.Helper()
		got := []int{}
		for _, v := range chk["exclude"].([]any) {
			got = append(got, int(id(v)))
		}
		return got
	}
	indices := func(chk map[string]any) (selectable []int, unselectable int) {
		unselectable = -1
		for _, r := range rowsOf(chk) {
			if r["selectable"].(bool) {
				selectable = append(selectable, int(id(r["index"])))
			} else {
				unselectable = int(id(r["index"]))
			}
		}
		return selectable, unselectable
	}

	// A fresh check has nothing unticked.
	chk := e.checkReady(`{"account_id":1}`)
	conform(t, e.doc, "CleanupCheck", chk)
	selectable, unselectable := indices(chk)
	if len(selectable) != 8 || unselectable < 0 || len(excludeOf(chk)) != 0 {
		t.Fatalf("fresh check: %d selectable, exclude %v", len(selectable), chk["exclude"])
	}
	a, b := selectable[1], selectable[5]

	// Saved in any order, returned ascending, the same on every GET (a reload).
	put(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{b, a, b}}, http.StatusNoContent)
	for range 2 {
		got := e.getCheck(1)
		conform(t, e.doc, "CleanupCheck", got)
		if ex := excludeOf(got); !slices.Equal(ex, []int{a, b}) || got["id"] != chk["id"] {
			t.Fatalf("GET after saving: exclude %v, want %v", ex, []int{a, b})
		}
	}
	// Saving again replaces the list; an empty list ticks everything.
	put(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{a}}, http.StatusNoContent)
	if ex := excludeOf(e.getCheck(1)); !slices.Equal(ex, []int{a}) {
		t.Fatalf("replaced selection = %v", ex)
	}

	// Refusals: a body that names no check, an unknown account, a foreign check, a row that
	// cannot be ticked or does not exist.
	refusePut(map[string]any{"account_id": 9, "check_id": chk["id"], "exclude": []int{}}, http.StatusBadRequest, "invalid_input", "account_id")
	refusePut(map[string]any{"account_id": 1, "exclude": []int{}}, http.StatusBadRequest, "invalid_input", "check_id")
	refusePut(map[string]any{"account_id": 1, "check_id": "nope", "exclude": []int{}}, http.StatusConflict, "preview_stale", "")
	for _, bad := range []int{unselectable, -1, 1000} {
		refusePut(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{a, bad}}, http.StatusBadRequest, "invalid_input", "exclude")
	}
	if ex := excludeOf(e.getCheck(1)); !slices.Equal(ex, []int{a}) {
		t.Errorf("a refused save changed the selection to %v", ex)
	}

	// A new check replaces the old one and starts with everything ticked; the old id is refused.
	newer := e.checkReady(`{"account_id":1}`)
	if len(excludeOf(newer)) != 0 || newer["id"] == chk["id"] {
		t.Fatalf("new check kept the selection: %v", newer["exclude"])
	}
	refusePut(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{a}}, http.StatusConflict, "preview_stale", "")

	// Discard clears it: the id is gone, and the next check starts clean.
	put(map[string]any{"account_id": 1, "check_id": newer["id"], "exclude": []int{a}}, http.StatusNoContent)
	e.call(http.MethodDelete, "/api/cleanup/check?account_id=1", "", http.StatusNoContent)
	refusePut(map[string]any{"account_id": 1, "check_id": newer["id"], "exclude": []int{a}}, http.StatusConflict, "preview_stale", "")
	chk = e.checkReady(`{"account_id":1}`)
	if len(excludeOf(chk)) != 0 {
		t.Fatalf("a check after Discard has selection %v", chk["exclude"])
	}

	// A rule edit makes the check stale, and a stale check takes no selection.
	put(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{a}}, http.StatusNoContent)
	e.call(http.MethodPatch, "/api/rules/1", `{"actions":[{"type":"archive"}]}`, http.StatusOK)
	refusePut(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{a}}, http.StatusConflict, "preview_stale", "")

	// A check that failed is not ready either.
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{}, models.Usage{}, fmt.Errorf("the model is down")
	}
	failed := e.checkReady(`{"account_id":1}`)
	refusePut(map[string]any{"account_id": 1, "check_id": failed["id"], "exclude": []int{}}, http.StatusConflict, "check_not_ready", "")
	e.decider.DecideFunc = cleanupDeciderFunc

	// Sort uses its own explicit list, not the saved one, and deletes the check with its selection.
	chk = e.checkReady(`{"account_id":1}`)
	selectable, _ = indices(chk)
	put(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": selectable[:3]}, http.StatusNoContent)
	run, _ := json.Marshal(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{}})
	batch := e.call(http.MethodPost, "/api/cleanup/run", string(run), http.StatusAccepted)["batch"].(map[string]any)
	e.batchDone(id(batch["id"]))
	if got := e.folderCount("INBOX"); got != 2 { // all 8 selectable rows were sorted; only the two unmatched stay
		t.Errorf("INBOX holds %d after Sort, want 2: the saved selection must not decide what Sort does", got)
	}
	if e.getCheck(1) != nil {
		t.Error("the check, and its selection, outlived the Sort")
	}
	refusePut(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{}}, http.StatusConflict, "preview_stale", "")
}

// A check says how many emails the range held before its limit, and a request that names no
// limit still covers at most the daemon's cap.
func TestCleanupCheckReportsMatchedBeforeTheLimit(t *testing.T) {
	e := newEnv(t)
	for i := range 12 {
		e.deliver("friend@example.org", fmt.Sprintf("lunch %d", i))
	}
	e.connect()
	for _, tc := range []struct {
		name, body            string
		limit, total, matched int
	}{
		{"no limit named: the cap, the whole small range", `{"account_id":1}`, composer.MaxLimit, 12, 12},
		{"newest 5 of 12", `{"account_id":1,"limit":5}`, 5, 5, 12},
		{"a start time of an hour ago with the cap", fmt.Sprintf(`{"account_id":1,"since":%d,"limit":2000}`, time.Now().Add(-time.Hour).Unix()), 2000, 12, 12},
		{"a start time in the future", fmt.Sprintf(`{"account_id":1,"since":%d,"limit":2000}`, time.Now().Add(48*time.Hour).Unix()), 2000, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chk := e.checkReady(tc.body)
			conform(t, e.doc, "CleanupCheck", chk)
			if chk["status"] != "ready" || chk["limit"] != float64(tc.limit) || chk["total"] != float64(tc.total) ||
				chk["matched"] != float64(tc.matched) || len(chk["rows"].([]any)) != tc.total {
				t.Errorf("check = status %v, limit %v, total %v, matched %v, %d rows; want limit %d, total %d, matched %d",
					chk["status"], chk["limit"], chk["total"], chk["matched"], len(chk["rows"].([]any)), tc.limit, tc.total, tc.matched)
			}
		})
	}
	e.refuse(http.MethodPost, "/api/cleanup/check", `{"account_id":1,"limit":2001}`, http.StatusBadRequest, "invalid_input", "limit")
}

// A batch says how much mail its check covered: the newest 5 of 12 reads limit 5, matched 12,
// and one made before that was recorded says nothing (null), never a made-up number.
func TestCleanupBatchRecordsWhatTheCheckCovered(t *testing.T) {
	e := newEnv(t)
	for i := range 12 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)

	chk := e.checkReady(`{"account_id":1,"limit":5}`)
	run, _ := json.Marshal(map[string]any{"account_id": 1, "check_id": chk["id"], "exclude": []int{}})
	started := e.call(http.MethodPost, "/api/cleanup/run", string(run), http.StatusAccepted)["batch"].(map[string]any)
	b := e.batchDone(id(started["id"]))
	conform(t, e.doc, "Batch", b)
	if id(b["limit"]) != 5 || id(b["matched"]) != 12 || b["total"] != float64(5) || b["since"] != nil {
		t.Errorf("batch = limit %v, matched %v, total %v, since %v; want 5, 12, 5, null", b["limit"], b["matched"], b["total"], b["since"])
	}

	old, err := e.st.CreateCleanupBatch(t.Context(), 1, "INBOX", 0, 0, 0, 3, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	got := e.call(http.MethodGet, fmt.Sprintf("/api/batches/%d", old.ID), "", http.StatusOK)["batch"].(map[string]any)
	conform(t, e.doc, "Batch", got)
	if got["limit"] != nil || got["matched"] != nil {
		t.Errorf("a batch with no record of its check says limit %v, matched %v; want null", got["limit"], got["matched"])
	}
}

// A check of an offline account is refused; the scope is validated like the old preview was.
func TestCleanupCheckScope(t *testing.T) {
	e := newEnv(t)
	e.connect()
	for body, path := range map[string]string{
		`{"account_id":9}`:            "account_id",
		`{"account_id":1,"limit":0}`:  "limit",
		`{"account_id":1,"since":-5}`: "since",
	} {
		e.refuse(http.MethodPost, "/api/cleanup/check", body, http.StatusBadRequest, "invalid_input", path)
	}
	// A paused account is offline: no check can run.
	e.call(http.MethodPatch, "/api/accounts/1", `{"paused":true}`, http.StatusOK)
	e.refuse(http.MethodPost, "/api/cleanup/check", `{"account_id":1}`, http.StatusConflict, "account_offline", "")
}

// An email that moved after the check is passed over by the Sort, counted as skipped, and
// not acted on.
func TestCleanupSortSkipsMovedMail(t *testing.T) {
	e := newEnv(t)
	for i := range 4 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)
	e.decider.DecideFunc = cleanupDeciderFunc

	_, live, cancel := e.hub.Subscribe(0)
	defer cancel()
	batches := make(chan store.Batch, 1000)
	go func() {
		for ev := range live {
			if ev.Name == events.BatchProgress {
				batches <- ev.Data.(store.Batch)
			}
		}
	}()

	chk := e.checkReady(`{"account_id":1}`)
	rows := rowsOf(chk)
	// Move the email of the first row out of the inbox: its uid no longer matches.
	gone := rows[0]
	ref := refOf(gone)
	if _, err := e.mb.Move(t.Context(), ref, "Archive"); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"account_id": 1, "check_id": chk["id"]})
	run := e.call(http.MethodPost, "/api/cleanup/run", string(body), http.StatusAccepted)["batch"].(map[string]any)
	batchID := id(run["id"])
	var last store.Batch
	for {
		select {
		case b := <-batches:
			if b.ID != batchID {
				continue
			}
			last = b
			if b.Status != store.BatchRunning {
				goto done
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the sort did not finish")
		}
	}
done:
	if last.Skipped != 1 {
		t.Fatalf("batch skipped %d, want 1", last.Skipped)
	}
	// The moved email is still in Archive, not sorted into Food.
	if e.folderOf(gone["subject"].(string)) != "Archive" {
		t.Errorf("the moved email is in %q, want Archive", e.folderOf(gone["subject"].(string)))
	}
	b := e.call(http.MethodGet, fmt.Sprintf("/api/batches/%d", batchID), "", http.StatusOK)["batch"].(map[string]any)
	if id(b["skipped"]) != 1 {
		t.Errorf("batch json skipped = %v, want 1", b["skipped"])
	}
}

// refOf is a check row's mail reference.
func refOf(row map[string]any) mail.MsgRef {
	return mail.MsgRef{AccountID: 1, Folder: row["folder"].(string), UID: uint32(id(row["uid"])), UIDValidity: uint32(id(row["uidvalidity"]))}
}

// lockedBuf is a log sink safe for the daemon's goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Neither a check nor a sort ever logs a subject, a sender or a body, even at debug level.
func TestCleanupLogsNoSecrets(t *testing.T) {
	buf := &lockedBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	e := newEnv(t)
	const subject, sender, bodyTok = "CONFIDENTIALSUBJECT", "topsecretsender@leak.example", "SECRETBODYTOKEN"
	for i := range 3 {
		e.mb.Deliver("INBOX", fmt.Sprintf("From: %s\r\nSubject: %s %d\r\nMessage-ID: <leak-%d@example.test>\r\n\r\n%s\r\n", sender, subject, i, i, bodyTok))
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)
	e.decider.DecideFunc = cleanupDeciderFunc

	chk := e.checkReady(`{"account_id":1}`)
	body, _ := json.Marshal(map[string]any{"account_id": 1, "check_id": chk["id"]})
	e.call(http.MethodPost, "/api/cleanup/run", string(body), http.StatusAccepted)
	eventually(t, "a log line from the sort", func() bool { return strings.Contains(buf.String(), "cleanup sort") })

	for _, leak := range []string{subject, sender, bodyTok} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("the log leaked %q", leak)
		}
	}
}

// Dry-run is honoured: the Sort records its actions as dry-run and moves nothing, and its
// undo is a no-op; with dry-run off the same selection moves and undo restores it.
func TestCleanupSortHonoursDryRun(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
	}
	e.signIn() // dry-run is on by default
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	e.live()
	e.call(http.MethodPost, "/api/rules/batch", cleanupRules, http.StatusCreated)
	e.decider.DecideFunc = cleanupDeciderFunc

	// Dry-run: five actions recorded as dry-run, nothing moved.
	chk := e.checkReady(`{"account_id":1}`)
	body, _ := json.Marshal(map[string]any{"account_id": 1, "check_id": chk["id"]})
	dry := id(e.call(http.MethodPost, "/api/cleanup/run", string(body), http.StatusAccepted)["batch"].(map[string]any)["id"])
	b := e.batchDone(dry)
	if acts := b["actions"].(map[string]any); b["status"] != "done" || acts["dry_run"] != float64(5) || acts["done"] != float64(0) {
		t.Fatalf("dry-run batch = %v", b)
	}
	if e.folderCount("INBOX") != 5 || e.hasFolder("Food") {
		t.Fatalf("dry-run moved mail: INBOX holds %d", e.folderCount("INBOX"))
	}
	if undo := e.call(http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", dry), "", http.StatusOK); undo["undone"] != float64(0) {
		t.Errorf("undo of a dry-run batch = %v, want 0 undone", undo)
	}

	// Dry-run off: the same selection moves, and one undo restores it.
	e.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	chk = e.checkReady(`{"account_id":1}`)
	body, _ = json.Marshal(map[string]any{"account_id": 1, "check_id": chk["id"]})
	live := id(e.call(http.MethodPost, "/api/cleanup/run", string(body), http.StatusAccepted)["batch"].(map[string]any)["id"])
	b = e.batchDone(live)
	if acts := b["actions"].(map[string]any); acts["done"] != float64(5) || acts["dry_run"] != float64(0) {
		t.Fatalf("live batch = %v", b)
	}
	if e.folderCount("INBOX") != 0 || e.folderCount("Food") != 5 {
		t.Fatalf("after the live run: INBOX %d, Food %d", e.folderCount("INBOX"), e.folderCount("Food"))
	}
	if undo := e.call(http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", live), "", http.StatusOK); undo["undone"] != float64(5) ||
		e.folderCount("INBOX") != 5 || e.folderCount("Food") != 0 {
		t.Fatalf("after the undo: %v; INBOX %d, Food %d", undo, e.folderCount("INBOX"), e.folderCount("Food"))
	}
}
