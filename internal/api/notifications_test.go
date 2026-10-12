package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// notifier is a module that delivers notifications of the kinds given; the daemon's worker
// is not running in these tests, so it is never called.
func notifier(kinds ...string) ext.Module {
	return ext.Module{Name: "notify", Notifications: &ext.Notifications{Kinds: kinds,
		Notify: func(context.Context, ext.Notice, ext.Deliveries) error { return nil }}}
}

// The free build has no module that delivers notifications: the feature is off, the routes
// refuse with not_available, and a rule cannot name a destination.
func TestFreeBuildHasNoNotifications(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["notifications"]; on != false {
		t.Errorf("features.notifications = %v without the module", on)
	}
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/notifications", ""},
		{http.MethodPost, "/api/notifications/destinations", `{"name":"x","kind":"webhook","url":"https://x.test"}`},
		{http.MethodDelete, "/api/notifications/destinations/1", ""},
	} {
		got := e.do(r.method, r.path, r.body)
		if got.status != http.StatusNotFound || got.body.Error.Code != "not_available" ||
			got.body.Error.Message != "Notifications and webhooks are not included in this build of MailRules." {
			t.Errorf("%s %s = %d %s", r.method, r.path, got.status, got.raw)
		}
	}
	e.refuse(http.MethodPatch, "/api/rules/1", `{"notify":[1]}`, http.StatusBadRequest, "invalid_input", "notify")
	if rule := e.call(http.MethodPatch, "/api/rules/1", `{"notify":[]}`, http.StatusOK)["rule"].(map[string]any); len(rule["notify"].([]any)) != 0 {
		t.Errorf("notify = %v", rule["notify"])
	}
}

// With the module: destinations are added, listed with only their address cut short, named
// by rules, and removed, which takes them off the rules.
func TestNotificationDestinations(t *testing.T) {
	e := newEnv(t, notifier(ext.DestinationSlack, ext.DestinationWebhook))
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["notifications"]; on != true {
		t.Errorf("features.notifications = %v with the module", on)
	}
	list := e.call(http.MethodGet, "/api/notifications", "", http.StatusOK)
	conform(t, e.doc, "NotificationList", list)
	if fmt.Sprint(list["kinds"]) != "[webhook slack]" || len(list["destinations"].([]any)) != 0 {
		t.Errorf("list = %v", list)
	}

	const hookURL, slackURL = "https://example.com/hooks/mailrules-7f3a", "https://hooks.slack.com/services/T0/B0/sEcReTtOkEn"
	for _, c := range []struct{ name, body, path string }{
		{"no name", `{"name":" ","kind":"webhook","url":"https://x.test/a"}`, "name"},
		{"a long name", `{"name":"` + strings.Repeat("n", 101) + `","kind":"webhook","url":"https://x.test/a"}`, "name"},
		{"an unknown kind", `{"name":"x","kind":"pager","url":"https://x.test/a"}`, "kind"},
		{"not http", `{"name":"x","kind":"webhook","url":"ftp://x.test/a"}`, "url"},
		{"no host", `{"name":"x","kind":"webhook","url":"https:///a"}`, "url"},
		{"a password in it", `{"name":"x","kind":"webhook","url":"https://u:p@x.test/a"}`, "url"},
		{"slack elsewhere", `{"name":"x","kind":"slack","url":"https://example.com/services/x"}`, "url"},
		{"slack over http", `{"name":"x","kind":"slack","url":"http://hooks.slack.com/services/x"}`, "url"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.refuse(http.MethodPost, "/api/notifications/destinations", c.body, http.StatusBadRequest, "invalid_input", c.path)
		})
	}
	e.refuse(http.MethodPost, "/api/notifications/destinations", `{"name":"x","kind":"webhook","url":"https://x.test","secret":"mine"}`,
		http.StatusBadRequest, "invalid_json", "")

	hook := e.call(http.MethodPost, "/api/notifications/destinations", `{"name":" Ops ","kind":"webhook","url":"`+hookURL+`"}`, http.StatusCreated)
	conform(t, e.doc, "NotificationDestinationCreated", hook)
	secret, _ := hook["secret"].(string)
	hd := hook["destination"].(map[string]any)
	if !strings.HasPrefix(secret, "whsec_") || len(secret) < 40 || hd["name"] != "Ops" || hd["url"] != "https://example.com/…7f3a" || hd["address_deleted"] != false {
		t.Errorf("webhook = %v", hook)
	}
	slack := e.call(http.MethodPost, "/api/notifications/destinations", `{"name":"Team","kind":"slack","url":"`+slackURL+`"}`, http.StatusCreated)
	if slack["secret"] != nil || slack["destination"].(map[string]any)["url"] != "https://hooks.slack.com/…OkEn" {
		t.Errorf("slack = %v", slack)
	}
	hookID, slackID := id(hd["id"]), id(slack["destination"].(map[string]any)["id"])
	if sec, err := e.st.DestinationSecret(t.Context(), hookID); err != nil || sec.URL != hookURL || sec.Secret != secret {
		t.Errorf("stored = %+v, %v", sec, err)
	}

	// The address and secret never come back.
	got := e.do(http.MethodGet, "/api/notifications", "")
	for _, s := range []string{hookURL, slackURL, secret, "sEcReT"} {
		if strings.Contains(string(got.raw), s) {
			t.Errorf("the list shows %q: %s", s, got.raw)
		}
	}
	if ds := got.object(t)["destinations"].([]any); len(ds) != 2 {
		t.Fatalf("destinations = %v", ds)
	}

	// A rule names them; one it cannot name is refused.
	both := fmt.Sprintf(`{"notify":[%d,%d]}`, hookID, slackID)
	rule := e.call(http.MethodPatch, "/api/rules/1", both, http.StatusOK)["rule"].(map[string]any)
	conform(t, e.doc, "Rule", rule)
	if fmt.Sprint(rule["notify"]) != fmt.Sprint([]any{float64(hookID), float64(slackID)}) {
		t.Errorf("notify = %v", rule["notify"])
	}
	e.refuse(http.MethodPatch, "/api/rules/1", `{"notify":[999]}`, http.StatusBadRequest, "invalid_input", "notify")
	e.refuse(http.MethodPatch, "/api/rules/1", fmt.Sprintf(`{"notify":[%d,%d]}`, hookID, hookID), http.StatusBadRequest, "invalid_input", "notify")
	created := e.call(http.MethodPost, "/api/rules/batch", fmt.Sprintf(`{"rules":[{"name":"Bills","conditions":{"field":"subject","op":"contains_any","value":["bill"]},
		"actions":[{"type":"archive"}],"notify":[%d]}]}`, slackID), http.StatusCreated)["items"].([]any)[0].(map[string]any)
	if fmt.Sprint(created["notify"]) != fmt.Sprint([]any{float64(slackID)}) {
		t.Errorf("created notify = %v", created["notify"])
	}
	e.refuse(http.MethodPost, "/api/rules/batch", `{"rules":[{"name":"Bad","intent":"x","actions":[{"type":"archive"}],"notify":[999]}]}`,
		http.StatusBadRequest, "invalid_input", "rules[0].notify")
	// Importing the rules again keeps what they notify: a rules file names no destinations.
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	if r := e.call(http.MethodGet, "/api/rules/1", "", http.StatusOK)["rule"].(map[string]any); len(r["notify"].([]any)) != 2 {
		t.Errorf("after an import, notify = %v", r["notify"])
	}

	// Removing one takes it off the rules.
	e.call(http.MethodDelete, fmt.Sprintf("/api/notifications/destinations/%d", slackID), "", http.StatusNoContent)
	e.refuse(http.MethodDelete, fmt.Sprintf("/api/notifications/destinations/%d", slackID), "", http.StatusNotFound, "not_found", "")
	if r := e.call(http.MethodGet, "/api/rules/1", "", http.StatusOK)["rule"].(map[string]any); fmt.Sprint(r["notify"]) != fmt.Sprint([]any{float64(hookID)}) {
		t.Errorf("after removing Team, notify = %v", r["notify"])
	}
	if r := e.call(http.MethodGet, fmt.Sprintf("/api/rules/%d", id(created["id"])), "", http.StatusOK)["rule"].(map[string]any); len(r["notify"].([]any)) != 0 {
		t.Errorf("Bills still notifies %v", r["notify"])
	}
}

// A rule that names a destination queues a notice when it sorts an email live, and the
// delivery the module reports is listed under the destination; in dry-run nothing is queued.
func TestLiveRuleQueuesNotices(t *testing.T) {
	e := newEnv(t, notifier(ext.DestinationWebhook))
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	dest := id(e.call(http.MethodPost, "/api/notifications/destinations", `{"name":"Ops","kind":"webhook","url":"https://x.test/h"}`,
		http.StatusCreated)["destination"].(map[string]any)["id"])
	e.call(http.MethodPatch, "/api/rules/1", fmt.Sprintf(`{"notify":[%d]}`, dest), http.StatusOK)

	e.call(http.MethodPatch, "/api/settings", `{"dry_run":true}`, http.StatusOK)
	e.deliver("noreply@swiggy.in", "dry order")
	e.item("dry order", "acted")
	if got, _ := e.st.WaitingNotices(t.Context(), 2_000_000_000, 10); len(got) != 0 {
		t.Fatalf("dry-run queued %+v", got)
	}

	e.call(http.MethodPatch, "/api/settings", `{"dry_run":false}`, http.StatusOK)
	e.deliver("Swiggy <noreply@swiggy.in>", "your order")
	row := e.item("your order", "acted")
	got, err := e.st.WaitingNotices(t.Context(), 2_000_000_000, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("notices = %+v, %v", got, err)
	}
	n := got[0]
	if n.DestinationID != dest || n.RuleID != 1 || n.RuleName != "Food" || !slices.Equal(n.Actions, []string{"move", "read"}) ||
		n.Folder != "Food" || n.From != "noreply@swiggy.in" || n.FromName != "Swiggy" || n.Subject != "your order" ||
		n.Mailbox != "me@example.test" || n.MessageID != id(row["id"]) {
		t.Errorf("notice = %+v", n)
	}

	// The module reports the delivery; Settings lists it.
	if _, err := e.st.RecordDelivery(t.Context(), n.ID, false, "Timed out after 10 seconds.", 1_800_000_100); err != nil {
		t.Fatal(err)
	}
	list := e.call(http.MethodGet, "/api/notifications", "", http.StatusOK)
	conform(t, e.doc, "NotificationList", list)
	ds := list["destinations"].([]any)[0].(map[string]any)["deliveries"].([]any)
	if len(ds) != 1 {
		t.Fatalf("deliveries = %v", ds)
	}
	if d := ds[0].(map[string]any); d["status"] != "failed" || d["reason"] != "Timed out after 10 seconds." || d["rule_name"] != "Food" || id(d["rule_id"]) != 1 {
		t.Errorf("delivery = %v", d)
	}
}

func TestCheckModulesNotifications(t *testing.T) {
	notify := func(context.Context, ext.Notice, ext.Deliveries) error { return nil }
	mod := func(name string, n *ext.Notifications) ext.Module { return ext.Module{Name: name, Notifications: n} }
	for _, c := range []struct {
		name    string
		modules []ext.Module
		want    string
	}{
		{"webhook and slack", []ext.Module{mod("n", &ext.Notifications{Kinds: []string{"webhook", "slack"}, Notify: notify})}, ""},
		{"no Notify", []ext.Module{mod("n", &ext.Notifications{Kinds: []string{"webhook"}})}, "needs Notify"},
		{"no kind", []ext.Module{mod("n", &ext.Notifications{Notify: notify})}, "at least one kind"},
		{"another kind", []ext.Module{mod("n", &ext.Notifications{Kinds: []string{"pager"}, Notify: notify})}, `kind "pager"`},
		{"two modules", []ext.Module{mod("a", &ext.Notifications{Kinds: []string{"webhook"}, Notify: notify}),
			mod("b", &ext.Notifications{Kinds: []string{"slack"}, Notify: notify})}, "more than one module delivers"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckModules("selfhost", c.modules)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want one saying %q", err, c.want)
			}
		})
	}
}

// Every build takes https addresses only, so a notice's sender and subject never cross the
// network in clear, a server on the same network included.
func TestDestinationURLHTTPSOnly(t *testing.T) {
	const sentence = "Use an https address, so the email's sender and subject are encrypted on the way."
	for _, c := range []struct{ mode, url, want string }{
		{"selfhost", "http://192.168.1.5:5678/webhook", sentence},
		{"cloud", "http://example.com/hook", sentence},
		{"selfhost", "https://192.168.1.5:5678/webhook", ""},
		{"cloud", "https://example.com/hook", ""},
	} {
		s := &server{Options: Options{Settings: &settings.Settings{Env: &config.Config{Mode: c.mode}}}}
		if got := s.destinationURLProblem(store.DestinationWebhook, c.url); got != c.want {
			t.Errorf("%s %s: problem = %q, want %q", c.mode, c.url, got, c.want)
		}
	}
}
