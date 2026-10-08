package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// MAI-53: trash_to_folder is on for an install that predates it, sends what a rule or a
// Needs review answer trashes to MailRules Trash, keeps counting it as trashed, and
// switches back to the server's Trash when turned off. An action follows the folder it
// recorded, whatever the setting says later.
func TestTrashToFolderSetting(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	// An existing install: settings were saved before trash_to_folder existed.
	for key, value := range map[string]string{"dry_run": "false", "retention_days": "90"} {
		if err := e.st.SetSetting(ctx, 1, key, value); err != nil {
			t.Fatal(err)
		}
	}
	e.connect()
	if got := e.call(http.MethodGet, "/api/settings", "", http.StatusOK); got["trash_to_folder"] != true || got["retention_days"] != float64(90) {
		t.Fatalf("an existing install reads trash_to_folder %v (retention %v), want true", got["trash_to_folder"], got["retention_days"])
	}

	e.call(http.MethodPost, "/api/rules/batch", `{"rules":[
		{"name":"Spam","conditions":{"field":"from_domain","op":"eq","value":"junk.example"},"actions":[{"type":"trash"}]},
		{"name":"Later","intent":"Things for later","actions":[{"type":"archive"}]}]}`, http.StatusCreated)
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{RuleID: 2, Confidence: 0.4}, models.Usage{Provider: "fake", Model: "fake-1"}, nil
	}

	trashed := func(subject, folder, outcome string) {
		t.Helper()
		it := e.item(subject, "acted")
		acts := it["actions"].([]any)
		last := acts[len(acts)-1].(map[string]any)
		if e.folderOf(subject) != folder || it["outcome"] != outcome || last["kind"] != "trash" || last["to_folder"] != folder {
			t.Errorf("%s: in %q, outcome %q, action %v; want %q, %q, a trash", subject, e.folderOf(subject), it["outcome"], last, folder, outcome)
		}
	}

	// A rule trashes: MailRules Trash is made and the email goes there, named as such.
	e.deliver("spam@junk.example", "spam one")
	trashed("spam one", actions.TrashFolder, "Moved to MailRules Trash")
	if f, err := e.st.Folders(ctx, 1); err != nil || !slices.ContainsFunc(f, func(f store.Folder) bool { return f.Name == actions.TrashFolder && f.SpecialUse == "" }) {
		t.Errorf("MailRules Trash is not in the stored folder list: %+v, %v", f, err)
	}

	// A Needs review answer that trashes goes the same way.
	e.deliver("someone@else.example", "unsure")
	unsure := e.item("unsure", "review")
	e.call(http.MethodPost, fmt.Sprintf("/api/review/%d/resolve", id(unsure["id"])), `{"rule_id":1}`, http.StatusOK)
	trashed("unsure", actions.TrashFolder, "Moved to MailRules Trash")

	// Both still count as trashed, and the Trashed filter lists both.
	stats := e.call(http.MethodGet, "/api/stats/summary", "", http.StatusOK)
	if stats["counts"].(map[string]any)["trashed"] != float64(2) {
		t.Errorf("counts = %v, want 2 trashed", stats["counts"])
	}
	if n := len(e.call(http.MethodGet, "/api/activity?outcome=trashed", "", http.StatusOK)["items"].([]any)); n != 2 {
		t.Errorf("?outcome=trashed lists %d emails, want 2", n)
	}

	// Off: trash goes to the server's Trash again, and says so.
	got := e.call(http.MethodPatch, "/api/settings", `{"trash_to_folder":false}`, http.StatusOK)
	conform(t, e.doc, "Settings", got)
	if v, err := e.st.Setting(ctx, 1, store.SettingTrashToFolder); got["trash_to_folder"] != false || err != nil || v != "false" {
		t.Fatalf("after patch: %v, stored %q %v", got["trash_to_folder"], v, err)
	}
	e.deliver("spam@junk.example", "spam two")
	trashed("spam two", "Trash", "Moved to Trash")

	// Undo of the email in MailRules Trash, with the setting now off: back from where it went.
	spam := e.item("spam one", "acted")
	e.call(http.MethodPost, fmt.Sprintf("/api/messages/%d/undo", id(spam["id"])), "", http.StatusOK)
	if e.folderOf("spam one") != "INBOX" {
		t.Errorf("after undo the email is in %q, want INBOX", e.folderOf("spam one"))
	}

	// null forgets the stored value: the default, on, is back.
	if got := e.call(http.MethodPatch, "/api/settings", `{"trash_to_folder":null}`, http.StatusOK); got["trash_to_folder"] != true {
		t.Errorf("after null: trash_to_folder %v, want true", got["trash_to_folder"])
	}
	if _, err := e.st.Setting(ctx, 1, store.SettingTrashToFolder); err == nil {
		t.Error("null left a stored trash_to_folder")
	}
}

// A trash says where the email went: the folder its row recorded, or Trash for one that
// went to the server's Trash (with the setting off, or before it existed).
func TestTrashWording(t *testing.T) {
	for _, tt := range []struct {
		name, folder, status, outcome, detail string
	}{
		{"to MailRules Trash", actions.TrashFolder, store.ActionDone, "Moved to MailRules Trash", "Moved to MailRules Trash"},
		{"to MailRules Trash, dry-run", actions.TrashFolder, store.ActionDryRun, "Would move to MailRules Trash", "Moved to MailRules Trash (dry run: nothing was changed)"},
		{"to the server's Trash", "", store.ActionDone, "Moved to Trash", "Moved to Trash"},
		{"to the server's Trash, dry-run", "", store.ActionDryRun, "Would move to Trash", "Moved to Trash (dry run: nothing was changed)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := store.Action{Kind: "trash", Folder: tt.folder, Status: tt.status}
			if got := outcome(store.ActivityRow{Actions: []store.Action{a}}); got != tt.outcome {
				t.Errorf("outcome = %q, want %q", got, tt.outcome)
			}
			if got := actionDetail(a); got != tt.detail {
				t.Errorf("trace detail = %q, want %q", got, tt.detail)
			}
		})
	}
}
