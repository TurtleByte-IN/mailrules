package api

import (
	"net/http"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
)

// While leave_own_mail is on (the default), mail from the account's own address
// (me@example.test, in any case) is left alone by live sorting, the rule tester and a
// cleanup check alike: no model call, no rule, nothing to sort. Switched off, the same
// email is decided like any other; null puts the default back.
func TestOwnMailLeftAlone(t *testing.T) {
	e := newEnv(t)
	e.deliver("Me@Example.TEST", "Reading my reply")
	e.deliver("hello@news.example", "Reading issue 1")
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK) // 2 Reading: by intent, a candidate for every email
	e.decider.DecideFunc = bySubject("The subject says ")

	if got := e.call(http.MethodGet, "/api/settings", "", http.StatusOK); got["leave_own_mail"] != true {
		t.Fatalf("leave_own_mail = %v, want true by default", got["leave_own_mail"])
	}
	bySubj := func(rows []any, subject string) map[string]any {
		t.Helper()
		for _, r := range rows {
			if m := r.(map[string]any); m["subject"] == subject {
				return m
			}
		}
		t.Fatalf("no row for %q in %v", subject, rows)
		return nil
	}

	// The rule tester.
	res := e.call(http.MethodPost, "/api/rules/test", `{"account_id":1}`, http.StatusOK)
	own := bySubj(res["results"].([]any), "Reading my reply")
	if own["stage"] != "none" || own["reason"] != pipeline.ReasonOwnMail || own["rule_id"] != nil || len(own["actions"].([]any)) != 0 ||
		res["model_calls"] != float64(1) || res["matched"] != float64(1) {
		t.Errorf("tester: own row %v, %v model calls, %v matched", own, res["model_calls"], res["matched"])
	}

	// A cleanup check: the row is there, with nothing to sort.
	chk := e.checkReady(`{"account_id":1}`)
	var ownRow map[string]any
	for _, r := range rowsOf(chk) {
		if r["subject"] == "Reading my reply" {
			ownRow = r
		}
	}
	if ownRow == nil || ownRow["stage"] != "none" || ownRow["reason"] != pipeline.ReasonOwnMail || ownRow["selectable"] != false ||
		ownRow["review"] != false || chk["model_calls"] != float64(1) {
		t.Errorf("check: own row %v, %v model calls", ownRow, chk["model_calls"])
	}

	// Live sorting.
	e.deliver("me@example.test", "Reading my second reply")
	if it := e.item("Reading my second reply", "skipped"); it["decision"].(map[string]any)["reason"] != pipeline.ReasonOwnMail {
		t.Errorf("live: %v", it)
	}

	// Switched off, the same email is decided like any other.
	if got := e.call(http.MethodPatch, "/api/settings", `{"leave_own_mail":false}`, http.StatusOK); got["leave_own_mail"] != false {
		t.Fatalf("after turning it off: %v", got["leave_own_mail"])
	}
	res = e.call(http.MethodPost, "/api/rules/test", `{"account_id":1}`, http.StatusOK)
	if own := bySubj(res["results"].([]any), "Reading my reply"); own["stage"] != "decider" || own["rule_name"] != "Reading" {
		t.Errorf("tester with the setting off: %v", own)
	}
	e.refuse(http.MethodPatch, "/api/settings", `{"leave_own_mail":"no"}`, http.StatusBadRequest, "invalid_input", "leave_own_mail")
	if got := e.call(http.MethodPatch, "/api/settings", `{"leave_own_mail":null}`, http.StatusOK); got["leave_own_mail"] != true {
		t.Errorf("null did not put the default back: %v", got["leave_own_mail"])
	}
}
