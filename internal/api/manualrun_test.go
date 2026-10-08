package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// mailboxes is the daemon with two accounts, each over the real IMAP connector and its own
// in-memory IMAP server, signed in with dry-run off. Mail is put in before the accounts are
// started, so what is there is for a manual run to sort, not for live sorting.
type mailboxes struct {
	*env
	srv  [2]*imaptest.Server
	cfg  [2]imap.Config
	acct [2]store.Account
	user [2]mail.Mailbox // the user's own mail client: reads what is really in each mailbox
}

func newMailboxes(t *testing.T) *mailboxes {
	t.Helper()
	e := newEnv(t)
	e.signIn()
	e.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	u, err := e.st.FirstUser(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m := &mailboxes{env: e}
	for i := range m.srv {
		m.srv[i] = imaptest.Start(t, goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapMove: {}, goimap.CapUIDPlus: {}})
		m.acct[i], err = e.st.CreateAccount(t.Context(), make([]byte, 32), store.Account{UserID: u.ID, Label: fmt.Sprintf("Box %d", i+1), Preset: "generic",
			Host: m.srv[i].Host, Port: m.srv[i].Port, TLSMode: presets.TLSImplicit, Username: imaptest.Username}, imaptest.Password)
		if err != nil {
			t.Fatal(err)
		}
		m.cfg[i] = imap.Config{AccountID: m.acct[i].ID, Host: m.srv[i].Host, Port: m.srv[i].Port, TLSMode: presets.TLSImplicit,
			Username: imaptest.Username, Password: imaptest.Password, TLSConfig: m.srv[i].TLS, Logger: slog.New(slog.DiscardHandler),
			IdleRestart: 40 * time.Millisecond, PollInterval: 10 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond}
		if m.user[i], err = imap.Open(t.Context(), m.cfg[i]); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.user[i].Close() })
	}
	return m
}

// put delivers an email to a mailbox's INBOX.
func (m *mailboxes) put(box int, from, subject string) {
	m.t.Helper()
	m.srv[box].Append(m.t, "INBOX", "From: "+from+"\r\nTo: me@example.test\r\nSubject: "+subject+
		"\r\nMessage-ID: <"+strings.ReplaceAll(subject, " ", "-")+"@example.test>\r\n\r\n"+strings.Repeat("The body of "+subject+". ", 20)+"\r\n")
}

// start runs both accounts, watching INBOX from where it stands now, and waits until they are live.
func (m *mailboxes) start() {
	m.t.Helper()
	exec := &actions.Exec{Store: m.st, Accounts: m.mgr, Hub: m.hub, DryRunDefault: false, Now: m.ck.now}
	runCtx, stop := context.WithCancel(context.WithoutCancel(m.t.Context()))
	m.t.Cleanup(func() { stop(); m.mgr.Wait() }) // runs before the database closes
	for i := range m.acct {
		cfg := m.cfg[i]
		m.mgr.Start(runCtx, &worker.Supervisor{
			Account: m.acct[i], Store: m.st, Hub: m.hub,
			Open:       func(ctx context.Context) (mail.Mailbox, error) { return imap.Open(ctx, cfg) },
			Pipeline:   pipeline.Pipeline{Store: m.st, Exec: exec, Hub: m.hub, BodyChars: 2000, Now: m.ck.now, Live: m.Live, Override: m.RouterFor},
			BackoffMin: time.Millisecond, DrainTimeout: 50 * time.Millisecond,
		})
	}
	eventually(m.t, "both accounts to be live", func() bool {
		for _, a := range m.acct {
			got, err := m.st.Account(m.t.Context(), a.ID)
			if _, mbErr := m.mgr.Mailbox(a.ID); err != nil || got.Status != worker.StatusLive || mbErr != nil {
				return false
			}
		}
		return true
	})
}

// count is how many emails a folder of a mailbox really holds; 0 when it does not exist.
func (m *mailboxes) count(box int, folder string) int {
	m.t.Helper()
	refs, _, err := m.user[box].FetchSince(m.t.Context(), folder, time.Time{}, 0)
	if err != nil {
		return 0
	}
	return len(refs)
}

// holds says what is really in each mailbox: INBOX, Food, Reading, Banking and MailRules Trash.
func (m *mailboxes) holds(box int) string {
	var parts []string
	for _, f := range []string{"INBOX", "Food", "Reading", "Banking", "MailRules Trash"} {
		parts = append(parts, fmt.Sprintf("%s %d", f, m.count(box, f)))
	}
	return strings.Join(parts, ", ")
}

// checksReady starts a check with body and waits until every check it started has settled,
// returning them by account id.
func (m *mailboxes) checksReady(body string) map[int64]map[string]any {
	m.t.Helper()
	started := m.call(http.MethodPost, "/api/cleanup/check", body, http.StatusAccepted)["checks"].([]any)
	out := map[int64]map[string]any{}
	for _, c := range started {
		acct := id(c.(map[string]any)["account_id"])
		eventually(m.t, "the check to settle", func() bool {
			chk := m.getCheck(acct)
			out[acct] = chk
			return chk["status"] == "ready" || chk["status"] == "stale" || chk["status"] == "failed"
		})
	}
	return out
}

// runBody is the body of a run that sorts every ticked row of these checks.
func runBody(t *testing.T, checks ...map[string]any) string {
	t.Helper()
	var runs []map[string]any
	for _, c := range checks {
		runs = append(runs, map[string]any{"account_id": c["account_id"], "check_id": c["id"]})
	}
	b, err := json.Marshal(map[string]any{"runs": runs})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const manualRules = `{"rules":[
	{"name":"Food","conditions":{"field":"from_domain","op":"eq","value":"swiggy.in"},"actions":[{"type":"move","folder":"Food"}]},
	{"name":"Bank","conditions":{"field":"from_domain","op":"eq","value":"bank.example"},"actions":[{"type":"move","folder":"Banking"}]},
	{"name":"Reading","intent":"Newsletters and promotions","conditions":{"field":"from_domain","op":"eq","value":"news.example"},"actions":[{"type":"move","folder":"Reading"}]}]}`

// MAI-43: a manual run over two mailboxes and some of the rules, against real IMAP servers.
// Only the picked rules act and in their usual order, sender rules still act (a block, and a
// route to a rule that was not picked), each mailbox gets its own batch and the batches
// share a run, a dry-run run changes nothing in either mailbox, undoing the run puts every
// email back, and a mailbox that is already being sorted refuses a second run.
func TestManualRunOverTwoMailboxesAndSomeRules(t *testing.T) {
	m := newMailboxes(t)
	for box := range 2 {
		tag := fmt.Sprintf("box%d ", box+1)
		m.put(box, "noreply@swiggy.in", tag+"food order 1")
		m.put(box, "noreply@swiggy.in", tag+"food order 2")
		m.put(box, "statements@bank.example", tag+"statement")
		m.put(box, "vip@bank.example", tag+"vip statement") // a sender rule routes this one to Bank
		m.put(box, "hello@news.example", tag+"reading issue")
		m.put(box, "spam@junk.example", tag+"deal") // a sender rule blocks this one
		m.put(box, "friend@example.org", tag+"lunch")
	}
	const each = 7
	m.start()
	m.call(http.MethodPost, "/api/rules/batch", manualRules, http.StatusCreated)
	items := m.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any)
	food, bank, reading := id(items[0].(map[string]any)["id"]), id(items[1].(map[string]any)["id"]), id(items[2].(map[string]any)["id"])
	m.call(http.MethodPut, "/api/senders/domain/junk.example", `{"verdict":"block"}`, http.StatusOK)
	m.call(http.MethodPut, "/api/senders/address/vip@bank.example", fmt.Sprintf(`{"verdict":"route","rule_id":%d}`, bank), http.StatusOK)
	m.decider.DecideFunc = cleanupDeciderFunc
	a1, a2 := m.acct[0].ID, m.acct[1].ID
	pick := fmt.Sprintf(`"account_ids":[%d,%d],"rule_ids":[%d,%d]`, a1, a2, food, reading) // Bank is left out
	for box := range 2 {
		if got, want := m.holds(box), fmt.Sprintf("INBOX %d, Food 0, Reading 0, Banking 0, MailRules Trash 0", each); got != want {
			t.Fatalf("box %d before the run: %s, want %s", box+1, got, want)
		}
	}

	// The request is checked before anything starts.
	for _, tc := range []struct{ body, path string }{
		{`{"account_id":1,"account_ids":[1]}`, "account_ids"},
		{`{"account_ids":[]}`, "account_ids"},
		{fmt.Sprintf(`{"account_ids":[%d,99]}`, a1), "account_ids"},
		{fmt.Sprintf(`{"account_ids":[%d],"rule_ids":[]}`, a1), "rule_ids"},
		{fmt.Sprintf(`{"account_ids":[%d],"rule_ids":[999]}`, a1), "rule_ids"},
	} {
		m.refuse(http.MethodPost, "/api/cleanup/check", tc.body, http.StatusBadRequest, "invalid_input", tc.path)
	}

	// Dry-run first: the picked rules are checked and sorted, nothing moves in either mailbox.
	m.call(http.MethodPatch, "/api/settings", `{"dry_run":true}`, http.StatusOK)
	checks := m.checksReady("{" + pick + "}")
	if len(checks) != 2 {
		t.Fatalf("%d checks, want one per mailbox", len(checks))
	}
	for acct, chk := range checks {
		conform(t, m.doc, "CleanupCheck", chk)
		if chk["status"] != "ready" || len(rowsOf(chk)) != each || len(chk["rule_ids"].([]any)) != 2 {
			t.Fatalf("check of account %d = %v", acct, chk)
		}
		byRule := map[string]int{}
		for _, r := range rowsOf(chk) {
			if r["selectable"] == true {
				byRule[r["rule_name"].(string)]++
			}
			if r["rule_name"] == "Bank" && r["stage"] != "sender" {
				t.Errorf("an unpicked rule took a row: %v", r)
			}
		}
		// Food x2, Reading x1 and the two sender rows: the block and the route to the unpicked Bank rule.
		if byRule["Food"] != 2 || byRule["Reading"] != 1 || byRule["Sender rule: trash"] != 1 || byRule["Bank"] != 1 || len(byRule) != 4 {
			t.Errorf("account %d would act on %v", acct, byRule)
		}
	}
	dry := m.call(http.MethodPost, "/api/cleanup/run", runBody(t, checks[a1], checks[a2]), http.StatusAccepted)
	if _, single := dry["batch"]; single {
		t.Error("a run of several checks answered `batch`")
	}
	dryBatches := dry["batches"].([]any)
	if len(dryBatches) != 2 {
		t.Fatalf("batches = %v", dryBatches)
	}
	for i, b := range dryBatches {
		conform(t, m.doc, "Batch", b)
		done := m.batchDone(id(b.(map[string]any)["id"]))
		if acts := done["actions"].(map[string]any); done["status"] != "done" || acts["dry_run"] != float64(5) || acts["done"] != float64(0) {
			t.Errorf("dry-run batch of box %d = %v", i+1, done)
		}
		if got, want := m.holds(i), fmt.Sprintf("INBOX %d, Food 0, Reading 0, Banking 0, MailRules Trash 0", each); got != want {
			t.Errorf("a dry run changed box %d: %s", i+1, got)
		}
	}

	// Live: the same run moves mail. The batches are one run, one per mailbox.
	m.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	checks = m.checksReady("{" + pick + "}")
	live := m.call(http.MethodPost, "/api/cleanup/run", runBody(t, checks[a1], checks[a2]), http.StatusAccepted)["batches"].([]any)
	var ids []int64
	for i, b := range live {
		bj := b.(map[string]any)
		ids = append(ids, id(bj["id"]))
		conform(t, m.doc, "Batch", bj)
		if id(bj["account_id"]) != m.acct[i].ID || bj["kind"] != "cleanup" || id(bj["total"]) != 5 || id(bj["run_id"]) != ids[0] {
			t.Errorf("batch %d = %v", i, bj)
		}
	}
	if ids[0] == ids[1] {
		t.Fatalf("one batch for two mailboxes: %v", ids)
	}
	for _, bid := range ids {
		if b := m.batchDone(bid); b["status"] != "done" || b["actions"].(map[string]any)["done"] != float64(5) {
			t.Errorf("batch %d = %v", bid, b)
		}
	}
	for box := range 2 {
		// Only the picked rules acted: Food (2) and Reading (1). The sender rules acted whatever was
		// picked: the block went to MailRules Trash and the vip's mail to Banking, by a rule that was
		// not picked. The other bank email and the stranger's stayed, so did nothing else move.
		if got, want := m.holds(box), "INBOX 2, Food 2, Reading 1, Banking 1, MailRules Trash 1"; got != want {
			t.Errorf("box %d after the run: %s, want %s", box+1, got, want)
		}
	}
	listed := m.call(http.MethodGet, "/api/batches?kind=cleanup", "", http.StatusOK)["items"].([]any)
	var inRun int
	for _, b := range listed {
		if r := b.(map[string]any)["run_id"]; r != nil && id(r) == ids[0] {
			inRun++
		}
	}
	if inRun != 2 {
		t.Errorf("the listing shows %d batches of the run, want 2", inRun)
	}

	// Undoing the run, mailbox by mailbox, puts every email back where it was.
	for _, bid := range ids {
		if undo := m.call(http.MethodPost, fmt.Sprintf("/api/batches/%d/undo", bid), "", http.StatusOK); undo["undone"] != float64(5) || undo["failed"] != float64(0) {
			t.Fatalf("undo of batch %d = %v", bid, undo)
		}
	}
	for box := range 2 {
		if got, want := m.holds(box), fmt.Sprintf("INBOX %d, Food 0, Reading 0, Banking 0, MailRules Trash 0", each); got != want {
			t.Errorf("box %d after the undo: %s, want %s", box+1, got, want)
		}
	}

	// A mailbox that is being sorted refuses a second run, and a run over both mailboxes is
	// refused whole: the other mailbox is not started either.
	unlock := m.mgr.Lock(a1) // holds the first mailbox between two emails
	checks = m.checksReady("{" + pick + "}")
	first := m.call(http.MethodPost, "/api/cleanup/run", runBody(t, checks[a1], checks[a2]), http.StatusAccepted)["batches"].([]any)
	again := m.checksReady("{" + pick + "}")
	before := len(m.call(http.MethodGet, "/api/batches?kind=cleanup", "", http.StatusOK)["items"].([]any))
	m.refuse(http.MethodPost, "/api/cleanup/run", runBody(t, again[a1], again[a2]), http.StatusConflict, "cleanup_running", "")
	single, _ := json.Marshal(map[string]any{"account_id": a1, "check_id": again[a1]["id"]})
	m.refuse(http.MethodPost, "/api/cleanup/run", string(single), http.StatusConflict, "cleanup_running", "")
	if after := len(m.call(http.MethodGet, "/api/batches?kind=cleanup", "", http.StatusOK)["items"].([]any)); after != before {
		t.Errorf("a refused run made %d batches", after-before)
	}
	unlock()
	for _, b := range first {
		m.batchDone(id(b.(map[string]any)["id"]))
	}
	// Nothing was left claimed by the refused run: the second mailbox is free again.
	if again = m.checksReady(fmt.Sprintf(`{"account_id":%d}`, a2)); again[a2]["status"] != "ready" {
		t.Fatalf("check = %v", again)
	}
	body, _ := json.Marshal(map[string]any{"account_id": a2, "check_id": again[a2]["id"]})
	m.batchDone(id(m.call(http.MethodPost, "/api/cleanup/run", string(body), http.StatusAccepted)["batch"].(map[string]any)["id"]))
}

// The original request shapes keep working next to the new ones: one mailbox in account_id is
// answered with `check` and `batch` as before, every rule is used when none are picked, and
// GET /api/cleanup/checks lists each mailbox's current check.
func TestManualRunKeepsTheOriginalShapes(t *testing.T) {
	m := newMailboxes(t)
	m.put(0, "noreply@swiggy.in", "food order")
	m.put(1, "statements@bank.example", "statement")
	m.start()
	m.call(http.MethodPost, "/api/rules/batch", manualRules, http.StatusCreated)
	m.decider.DecideFunc = cleanupDeciderFunc
	a1, a2 := m.acct[0].ID, m.acct[1].ID

	if got := m.call(http.MethodGet, "/api/cleanup/checks", "", http.StatusOK)["checks"].([]any); len(got) != 0 {
		t.Fatalf("checks before any = %v", got)
	}
	started := m.call(http.MethodPost, "/api/cleanup/check", fmt.Sprintf(`{"account_id":%d}`, a1), http.StatusAccepted)
	one, checks := started["check"].(map[string]any), started["checks"].([]any)
	if len(checks) != 1 || id(one["account_id"]) != a1 || one["rule_ids"] != nil {
		t.Fatalf("started = %v", started)
	}
	m.checksReady(fmt.Sprintf(`{"account_ids":[%d]}`, a2)) // the other mailbox, in the new shape
	both := m.call(http.MethodGet, "/api/cleanup/checks", "", http.StatusOK)["checks"].([]any)
	if len(both) != 2 || id(both[0].(map[string]any)["account_id"]) != a1 || id(both[1].(map[string]any)["account_id"]) != a2 {
		t.Fatalf("checks = %v", both)
	}
	for _, c := range both {
		conform(t, m.doc, "CleanupCheck", c)
	}

	// The original run shape answers `batch`, and `batches` beside it.
	chk := m.getCheck(a1)
	body, _ := json.Marshal(map[string]any{"account_id": a1, "check_id": chk["id"]})
	ran := m.call(http.MethodPost, "/api/cleanup/run", string(body), http.StatusAccepted)
	b := ran["batch"].(map[string]any)
	if got := ran["batches"].([]any); len(got) != 1 || id(got[0].(map[string]any)["id"]) != id(b["id"]) || b["run_id"] != nil {
		t.Fatalf("run = %v", ran)
	}
	m.batchDone(id(b["id"]))

	// The two shapes are not mixed.
	chk = m.getCheck(a2)
	mixed, _ := json.Marshal(map[string]any{"account_id": a2, "check_id": chk["id"], "runs": []map[string]any{{"account_id": a2, "check_id": chk["id"]}}})
	m.refuse(http.MethodPost, "/api/cleanup/run", string(mixed), http.StatusBadRequest, "invalid_input", "runs")
	m.refuse(http.MethodPost, "/api/cleanup/run", `{"runs":[]}`, http.StatusBadRequest, "invalid_input", "runs")
	m.refuse(http.MethodPost, "/api/cleanup/run", fmt.Sprintf(`{"runs":[{"account_id":%d,"check_id":"nope"}]}`, a2), http.StatusConflict, "preview_stale", "runs[0].check_id")
	dup := fmt.Sprintf(`{"runs":[{"account_id":%d,"check_id":%q},{"account_id":%d,"check_id":%q}]}`, a2, chk["id"], a2, chk["id"])
	m.refuse(http.MethodPost, "/api/cleanup/run", dup, http.StatusBadRequest, "invalid_input", "runs[1].account_id")
}

// A check limited to some rules goes stale only when one of those rules, or a sender rule,
// changes: editing a rule that was left out does not spoil it.
func TestManualRunStaleOnlyForPickedRules(t *testing.T) {
	m := newMailboxes(t)
	m.put(0, "noreply@swiggy.in", "food order")
	m.start()
	m.call(http.MethodPost, "/api/rules/batch", manualRules, http.StatusCreated)
	m.decider.DecideFunc = cleanupDeciderFunc
	items := m.call(http.MethodGet, "/api/rules", "", http.StatusOK)["items"].([]any)
	food, bank := id(items[0].(map[string]any)["id"]), id(items[1].(map[string]any)["id"])
	a1 := m.acct[0].ID

	chk := m.checksReady(fmt.Sprintf(`{"account_ids":[%d],"rule_ids":[%d]}`, a1, food))[a1]
	m.call(http.MethodPatch, fmt.Sprintf("/api/rules/%d", bank), `{"actions":[{"type":"archive"}]}`, http.StatusOK)
	if s := m.getCheck(a1)["status"]; s != "ready" {
		t.Fatalf("after editing a rule that was left out the check is %v, want ready", s)
	}
	m.call(http.MethodPatch, fmt.Sprintf("/api/rules/%d", food), `{"actions":[{"type":"archive"}]}`, http.StatusOK)
	if s := m.getCheck(a1)["status"]; s != "stale" {
		t.Fatalf("after editing a picked rule the check is %v, want stale", s)
	}
	body, _ := json.Marshal(map[string]any{"account_id": a1, "check_id": chk["id"]})
	m.refuse(http.MethodPost, "/api/cleanup/run", string(body), http.StatusConflict, "preview_stale", "")

	// A rule that is switched off cannot be picked.
	m.call(http.MethodPatch, fmt.Sprintf("/api/rules/%d", bank), `{"enabled":false}`, http.StatusOK)
	m.refuse(http.MethodPost, "/api/cleanup/check", fmt.Sprintf(`{"account_ids":[%d],"rule_ids":[%d]}`, a1, bank), http.StatusBadRequest, "invalid_input", "rule_ids")
}
