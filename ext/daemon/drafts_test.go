package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	mailimap "github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// fakeDrafter is a module that writes replies with the model it is handed: it asks for
// {"body"} and keeps every request.
type fakeDrafter struct {
	mu   sync.Mutex
	got  []ext.DraftRequest
	fail int // the first fail calls fail, for now
}

func (f *fakeDrafter) write(ctx context.Context, r ext.DraftRequest) (string, error) {
	f.mu.Lock()
	f.got = append(f.got, r)
	fail := f.fail > 0
	if fail {
		f.fail--
	}
	f.mu.Unlock()
	if fail {
		return "", errors.New("the model is busy")
	}
	var out struct{ Body string }
	if _, err := r.Model.Generate(ctx, "Write a reply. "+r.Instruction, r.Email.Text, json.RawMessage(`{"type":"object"}`), &out); err != nil {
		return "", err
	}
	return out.Body, nil
}

func (f *fakeDrafter) requests() []ext.DraftRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func (f *fakeDrafter) module() ext.Module {
	return ext.Module{Name: "draftstest", Drafts: &ext.Drafts{Write: f.write}}
}

// ollama answers every chat with a reply body, as an Ollama server would, and counts calls.
func ollama(t *testing.T, body string) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		content, _ := json.Marshal(map[string]string{"body": body})
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": string(content)},
			"prompt_eval_count": 120, "eval_count": 30})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// End to end: a rule that drafts replies acts on an email live; within seconds the module
// writes the reply on the tenant's model (booked as "draft"), the daemon saves it in the
// mailbox's Drafts folder as a read draft threaded under the email, Activity lists it, and
// undoing it deletes that reply. In dry-run nothing is written.
func TestDraftsEndToEnd(t *testing.T) {
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}, imap.CapSpecialUse: {}})
	if err := srv.User.Create("Receipts", nil); err != nil {
		t.Fatal(err)
	}
	srv.CreateSpecial(t, "Drafts", imap.MailboxAttrDrafts)
	model, _ := ollama(t, "Thanks, got it.")
	oauth := &fakeOAuth{srv: srv, rotate: map[string]string{}, revoked: map[string]bool{}}
	f := &fakeDrafter{fail: 1}
	d := serveWithEnv(t, srv.TLS, map[string]string{"MAILRULES_COMPOSER_MODEL": "ollama:llama3.2", "OLLAMA_URL": model.URL},
		oauth.module(), f.module())
	c := d.c
	if _, body := c.do(http.MethodGet, "/api/settings", nil, http.StatusOK); !strings.Contains(string(body), `"draft_replies":true`) {
		t.Fatalf("features.draft_replies is not on: %s", body)
	}
	hdr, _ := c.do(http.MethodGet, "/api/oauthtest/connect?provider=gmail", nil, http.StatusSeeOther)
	var account int64
	if _, err := fmt.Sscanf(hdr.Get("Location"), "/#/accounts?added=%d", &account); err != nil {
		t.Fatalf("added: %q", hdr.Get("Location"))
	}
	eventually(t, "the mailbox goes live", func() (bool, string) { a := c.account(account); return a.Status == "live", a.Status })
	c.do(http.MethodPost, "/api/rules/batch", map[string]any{"rules": []map[string]any{{"name": "Receipts",
		"conditions": map[string]any{"field": "subject", "op": "contains_any", "value": []string{"Receipt"}},
		"actions":    []map[string]any{{"type": "move", "folder": "Receipts"}},
		"draft":      map[string]any{"instruction": "say thanks", "read_previous": false}}}}, http.StatusCreated)

	srv.Append(t, "INBOX", receipt(1)) // dry-run, on until the person turns it off
	eventually(t, "the first receipt is recorded", func() (bool, string) {
		_, body := c.do(http.MethodGet, "/api/activity", nil, http.StatusOK)
		return strings.Contains(string(body), "Receipt 1"), string(body)
	})
	time.Sleep(2*draftEvery + 200*time.Millisecond)
	if got := f.requests(); len(got) != 0 {
		t.Fatalf("dry-run asked for %d replies", len(got))
	}

	c.do(http.MethodPatch, "/api/settings", map[string]any{"dry_run": false}, http.StatusOK)
	srv.Append(t, "INBOX", receipt(2))
	type actionView struct {
		ID     int64
		Kind   string
		Status string
		Folder string
		Error  string
	}
	var draft actionView
	// The module fails once; the job is tried again a few seconds later.
	eventually(t, "Activity lists the reply", func() (bool, string) {
		_, body := c.do(http.MethodGet, "/api/activity?action=draft", nil, http.StatusOK)
		var out struct {
			Items []struct {
				Subject string
				Outcome string
				Actions []actionView
			}
		}
		_ = json.Unmarshal(body, &out)
		for _, it := range out.Items {
			for _, a := range it.Actions {
				if a.Kind == "draft" {
					draft = a
					return it.Subject == "Receipt 2" && it.Outcome == "Moved to Receipts · reply drafted", string(body)
				}
			}
		}
		return false, string(body)
	})
	if draft.Status != "done" || draft.Folder != "Drafts" {
		t.Fatalf("draft action = %+v", draft)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[1].ID != reqs[0].ID {
		t.Fatalf("requests = %+v, want the same one twice", reqs)
	}
	r := reqs[1]
	if r.RuleName != "Receipts" || r.Instruction != "say thanks" || r.Mailbox != imaptest.Username || r.Email.From != "orders@shop.example" ||
		r.Email.FromName != "Shop" || r.Email.Subject != "Receipt 2" || r.Email.Text != "Thanks for your order." || r.Previous != nil {
		t.Errorf("request = %+v", r)
	}

	// On the server: one read draft in Drafts, a reply to the receipt, from the mailbox.
	mb, err := mailimap.Open(t.Context(), mailimap.Config{AccountID: account, Host: srv.Host, Port: srv.Port, TLSMode: presets.TLSImplicit,
		Username: imaptest.Username, Password: imaptest.Password, TLSConfig: srv.TLS, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mb.Close() })
	refs, _, err := mb.FetchSince(t.Context(), "Drafts", time.Time{}, 0)
	if err != nil || len(refs) != 1 {
		t.Fatalf("Drafts holds %v, %v", refs, err)
	}
	raw, err := mb.Fetch(t.Context(), refs[0], 0)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := message.Parse(raw, account, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(raw.Flags, []string{`\Draft`, `\Seen`}) || sum.From != imaptest.Username || !slices.Equal(sum.To, []string{"orders@shop.example"}) ||
		sum.Subject != "Re: Receipt 2" || sum.Headers["In-Reply-To"][0] != "<r2@shop.example>" || sum.Headers["References"][0] != "<r2@shop.example>" ||
		sum.Body != "Thanks, got it." {
		t.Errorf("saved draft: flags %v, %+v", raw.Flags, sum)
	}

	_, body := c.do(http.MethodGet, "/api/stats/usage", nil, http.StatusOK)
	if !strings.Contains(string(body), `"purpose":"draft"`) {
		t.Errorf("the drafting model's calls are not booked as draft: %s", body)
	}

	c.do(http.MethodPost, fmt.Sprintf("/api/actions/%d/undo", draft.ID), nil, http.StatusOK)
	if refs, _, err := mb.FetchSince(t.Context(), "Drafts", time.Time{}, 0); err != nil || len(refs) != 0 {
		t.Errorf("Drafts after the undo holds %v, %v", refs, err)
	}
	if refs, _, _ := mb.FetchSince(t.Context(), "Receipts", time.Time{}, 0); len(refs) != 1 {
		t.Errorf("Receipts holds %v: undoing the reply moved the email", refs)
	}
}

// draftAccounts is actions.Accounts with one fake mailbox.
type draftAccounts struct{ mb mail.Mailbox }

func (a draftAccounts) Mailbox(int64) (mail.Mailbox, error) { return a.mb, nil }
func (a draftAccounts) Lock(int64) func()                   { return func() {} }

// draftWorld is a live run of a rule that drafts replies, on a fake mailbox with a Drafts
// folder, and a worker to write its reply.
type draftWorld struct {
	st  *store.Store
	mb  *mailtest.Mailbox
	job store.DraftJob
	msg int64
	w   *draftWorker
}

func newDraftWorld(t *testing.T, module func(context.Context, ext.DraftRequest) (string, error)) draftWorld {
	t.Helper()
	ctx := t.Context()
	st, _ := storetest.New(t)
	u, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := st.CreateAccount(ctx, store.Account{UserID: u.ID, Label: "me", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit",
		Username: "Me@Example.test"}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveFolders(ctx, acct.ID, []store.Folder{{Name: "INBOX"}, {Name: "Drafts", SpecialUse: mail.RoleDrafts}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDryRun(ctx, u.TenantID, false); err != nil {
		t.Fatal(err)
	}
	mb := mailtest.New(acct.ID)
	mb.AddFolder("Drafts", mail.RoleDrafts)
	rule, err := st.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "Receipts", Intent: "receipts", Enabled: true,
		Actions: []rules.Action{{Type: rules.ActFlag}}, Draft: &rules.Draft{}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := mb.Deliver("INBOX", receipt(1))
	m, _, err := st.IngestMessage(ctx, ref, 1)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := st.AddDecision(ctx, store.Decision{MessageID: m.ID, Stage: "condition", RuleID: rule.ID, RuleName: rule.Name, CreatedAt: 1}, store.StateDecided)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	exec := &actions.Exec{Store: st, Accounts: draftAccounts{mb}, Hub: events.NewHub(), Now: func() time.Time { return now },
		Drafts: func(context.Context, int64) bool { return true }}
	batch, _ := st.LiveBatch(ctx, u.TenantID, now)
	if _, err := exec.Apply(ctx, actions.DecisionRecord{DecisionID: dec, MessageID: m.ID, Ref: ref}, rule.Actions, batch); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.WaitingDrafts(ctx, now.Unix(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	gen := &models.Fake{GenerateFunc: func(_, _ string, _ json.RawMessage, out any) (models.Usage, error) {
		return models.Usage{Provider: "anthropic", Model: "claude", TokensIn: 100, TokensOut: 20, CostUSD: 0.001}, json.Unmarshal([]byte(`{"body":"Thanks!"}`), out)
	}}
	w := &draftWorker{store: st, exec: exec, drafts: &ext.Drafts{Write: module}, hub: events.NewHub(), bodyChars: 2000,
		now: func() time.Time { return now },
		model: func(context.Context, int64) (models.Generator, store.Ledger, bool, error) {
			return gen, store.Ledger{Store: st, TenantID: u.TenantID, Operator: func(string) bool { return true }}, true, nil
		}}
	return draftWorld{st: st, mb: mb, job: jobs[0], msg: m.ID, w: w}
}

// draftAction is the email's draft action, and how many replies Drafts holds.
func (d draftWorld) draftAction(t *testing.T) (store.Action, int) {
	t.Helper()
	refs, _, _ := d.mb.FetchSince(t.Context(), "Drafts", time.Time{}, 0)
	acts, err := d.st.MessageActions(t.Context(), d.msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range acts {
		if a.Kind == store.ActionDraft {
			return a, len(refs)
		}
	}
	return store.Action{}, len(refs)
}

// The worker writes the reply with the tenant's model, books it as "draft" and counts it as
// one AI-sorted email on the operator's keys; it refuses, with the reason Activity shows,
// when the allowance is used up, no model is set, or the module declines; a module that
// keeps failing is retried, then given up.
func TestDraftWorker(t *testing.T) {
	write := func(ctx context.Context, r ext.DraftRequest) (string, error) {
		var out struct{ Body string }
		_, err := r.Model.Generate(ctx, "s", r.Email.Text, nil, &out)
		return out.Body, err
	}
	tests := []struct {
		name       string
		module     func(context.Context, ext.DraftRequest) (string, error)
		setup      func(w *draftWorker)
		later      time.Duration // how long after the rule acted the job is handled
		wantStatus string        // "" = no draft action yet, the job tried again
		wantReason string
		wantSaved  int
		wantSpent  int
	}{
		{name: "written and saved", module: write, wantStatus: store.ActionDone, wantSaved: 1, wantSpent: 1},
		{name: "on the tenant's own key", module: write, setup: func(w *draftWorker) {
			model := w.model
			w.model = func(ctx context.Context, tenantID int64) (models.Generator, store.Ledger, bool, error) {
				g, l, _, err := model(ctx, tenantID)
				return g, l, false, err
			}
			w.allowance = func(context.Context, int64, time.Time) (bool, error) { return true, nil } // used up, but not counted
		}, wantStatus: store.ActionDone, wantSaved: 1},
		{name: "the allowance is used up", module: write, setup: func(w *draftWorker) {
			w.allowance = func(context.Context, int64, time.Time) (bool, error) { return true, nil }
		}, wantStatus: store.ActionFailed, wantReason: "this month's AI-sorted emails are used up, so no reply was drafted"},
		{name: "no model is set up", module: write, setup: func(w *draftWorker) {
			w.model = func(context.Context, int64) (models.Generator, store.Ledger, bool, error) {
				return nil, store.Ledger{}, false, settings.ErrNoComposer
			}
		}, wantStatus: store.ActionFailed, wantReason: "no AI model is set up to write replies: choose the rule composer model in Settings"},
		{name: "the module declines", module: func(context.Context, ext.DraftRequest) (string, error) {
			return "", fmt.Errorf("model said: %w", &ext.DraftRefused{Reason: "Nothing\nto answer."})
		}, wantStatus: store.ActionFailed, wantReason: "Nothing to answer."},
		{name: "the module writes nothing", module: func(context.Context, ext.DraftRequest) (string, error) { return "  ", nil },
			wantStatus: store.ActionFailed, wantReason: "the drafting module wrote no reply"},
		{name: "the module fails for now", module: func(context.Context, ext.DraftRequest) (string, error) { return "", errors.New("busy") }},
		{name: "the module fails for a day", module: func(context.Context, ext.DraftRequest) (string, error) { return "", errors.New("busy") },
			later: ext.DraftWait, wantStatus: store.ActionFailed, wantReason: "the drafting module could not write a reply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDraftWorld(t, tt.module)
			ctx := t.Context()
			if tt.setup != nil {
				tt.setup(d.w)
			}
			at := time.Unix(1_800_000_000, 0).Add(tt.later)
			d.w.now = func() time.Time { return at }
			if err := d.w.once(ctx); err != nil {
				t.Fatal(err)
			}
			a, saved := d.draftAction(t)
			if a.Status != tt.wantStatus || a.Error != tt.wantReason || saved != tt.wantSaved {
				t.Errorf("draft action %+v, %d saved; want %q (%q), %d saved", a, saved, tt.wantStatus, tt.wantReason, tt.wantSaved)
			}
			waiting, _ := d.st.WaitingDrafts(ctx, 2_000_000_000, 10)
			if (tt.wantStatus == "") != (len(waiting) == 1) {
				t.Errorf("waiting jobs = %+v", waiting)
			}
			if tt.wantStatus == "" && (waiting[0].Attempts != 1 || waiting[0].ID != d.job.ID) {
				t.Errorf("retried job = %+v", waiting[0])
			}
			if spent, _ := d.st.AIEmails(ctx, 1, store.Month(at)); spent != tt.wantSpent {
				t.Errorf("AI-sorted emails counted = %d, want %d", spent, tt.wantSpent)
			}
			if tt.wantStatus == store.ActionDone {
				usage, err := d.st.Usage(ctx, 1, "2000-01-01")
				if err != nil || len(usage) != 1 || usage[0].Purpose != "draft" || !usage[0].Operator {
					t.Errorf("usage = %+v, %v", usage, err)
				}
			}
		})
	}
}

func TestOwnAddress(t *testing.T) {
	for _, tt := range []struct {
		acct store.Account
		want string
	}{
		{store.Account{Username: "Jane@Me.com", Preset: "icloud"}, "jane@me.com"},
		{store.Account{Username: "jane", Preset: "icloud"}, "jane@icloud.com"},
		{store.Account{Username: "jane", Preset: "generic"}, ""},
	} {
		if got := ownAddress(tt.acct); got != tt.want {
			t.Errorf("ownAddress(%+v) = %q, want %q", tt.acct, got, tt.want)
		}
	}
}

func TestParentID(t *testing.T) {
	for _, tt := range []struct {
		h    map[string][]string
		want string
	}{
		{map[string][]string{"In-Reply-To": {"<a@x>"}, "References": {"<z@x> <b@x>"}}, "a@x"},
		{map[string][]string{"References": {"<z@x> <b@x>"}}, "b@x"},
		{map[string][]string{"In-Reply-To": {"<>"}}, ""},
		{map[string][]string{}, ""},
	} {
		if got := parentID(tt.h); got != tt.want {
			t.Errorf("parentID(%v) = %q, want %q", tt.h, got, tt.want)
		}
	}
}
