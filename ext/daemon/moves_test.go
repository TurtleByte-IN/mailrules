package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"

	mrimap "github.com/TurtleByte-IN/mailrules/internal/mail/imap"
)

// fakeMover is a module that learns from the person's moves the way the hosted one does:
// it asks about every move it is told of, and keeps what it was told and what Ask said.
type fakeMover struct {
	mu     sync.Mutex
	refuse int // hand-overs to refuse first
	moves  []ext.Move
	asked  []error
}

func (f *fakeMover) moved(ctx context.Context, m ext.Move, q ext.Questions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse > 0 {
		f.refuse--
		return errors.New("busy")
	}
	f.moves = append(f.moves, m)
	f.asked = append(f.asked, q.Ask(ctx, m.ID))
	return nil
}

func (f *fakeMover) told() ([]ext.Move, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.moves), slices.Clone(f.asked)
}

func (f *fakeMover) module(every time.Duration) ext.Module {
	return ext.Module{Name: "movetest", Moves: &ext.Moves{Every: every, Moved: f.moved}}
}

// person is another mail app on the same mailbox, signed in with the password.
func person(t *testing.T, srv *imaptest.Server) *mrimap.Mailbox {
	t.Helper()
	m, err := mrimap.Open(t.Context(), mrimap.Config{AccountID: 99, Host: srv.Host, Port: srv.Port, TLSMode: presets.TLSImplicit,
		Username: imaptest.Username, Password: imaptest.Password, TLSConfig: srv.TLS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// moveMail moves the receipt n, wherever it is in from, to dest, as the person would.
func moveMail(t *testing.T, mb *mrimap.Mailbox, n int, from, dest string) {
	t.Helper()
	ref, err := mb.FindByMessageID(t.Context(), from, fmt.Sprintf("r%d@shop.example", n))
	if err != nil {
		t.Fatalf("receipt %d in %s: %v", n, from, err)
	}
	if _, err := mb.Move(t.Context(), ref, dest); err != nil {
		t.Fatal(err)
	}
}

type activityItem struct {
	ID      int64  `json:"id"`
	Subject string `json:"subject"`
}

// filed waits until the receipt n is sorted live into Receipts, and returns its Activity id.
func filed(t *testing.T, c *client, srv *imaptest.Server, n int) int64 {
	t.Helper()
	srv.Append(t, "INBOX", receipt(n))
	var id int64
	eventually(t, fmt.Sprintf("receipt %d is filed", n), func() (bool, string) {
		_, body := c.do(http.MethodGet, "/api/activity", nil, http.StatusOK)
		var page struct{ Items []activityItem }
		_ = json.Unmarshal(body, &page)
		for _, it := range page.Items {
			if it.Subject == fmt.Sprintf("Receipt %d", n) {
				id = it.ID
			}
		}
		if id == 0 {
			return false, string(body)
		}
		_, msg := c.do(http.MethodGet, "/api/messages/"+strconv.FormatInt(id, 10), nil, http.StatusOK)
		return strings.Contains(string(msg), `"status":"done"`), string(msg)
	})
	return id
}

type questionView struct {
	ID     int64  `json:"id"`
	From   string `json:"from"`
	Inbox  bool   `json:"inbox"`
	Folder string `json:"folder"`
	Emails int    `json:"emails"`
}

func questionsOf(t *testing.T, c *client) []questionView {
	t.Helper()
	_, body := c.do(http.MethodGet, "/api/activity/questions", nil, http.StatusOK)
	var out struct{ Questions []questionView }
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out.Questions
}

// End to end, with the in-memory IMAP server: the person moving an email MailRules filed,
// in another mail app, reaches the module with what it needs; MailRules' own undo and fix
// do not; the module's question shows in Activity; No closes it for good and Yes stores
// the sender rule.
func TestMovesEndToEnd(t *testing.T) {
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	for _, f := range []string{"Receipts", "Later"} {
		if err := srv.User.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}
	oauth := &fakeOAuth{srv: srv, rotate: map[string]string{}, revoked: map[string]bool{}}
	f := &fakeMover{}
	d := serveForTest(t, srv.TLS, oauth.module(), f.module(50*time.Millisecond))
	c := d.c
	if _, body := c.do(http.MethodGet, "/api/settings", nil, http.StatusOK); !strings.Contains(string(body), `"move_questions":true`) {
		t.Fatalf("features.move_questions is not on: %s", body)
	}
	hdr, _ := c.do(http.MethodGet, "/api/oauthtest/connect?provider=gmail", nil, http.StatusSeeOther)
	var account int64
	if _, err := fmt.Sscanf(hdr.Get("Location"), "/#/accounts?added=%d", &account); err != nil {
		t.Fatalf("added: %q", hdr.Get("Location"))
	}
	eventually(t, "the mailbox goes live", func() (bool, string) { a := c.account(account); return a.Status == "live", a.Status })
	_, body := c.do(http.MethodPost, "/api/rules/batch", map[string]any{"rules": []map[string]any{{"name": "Receipts",
		"conditions": map[string]any{"field": "subject", "op": "contains_any", "value": []string{"Receipt"}},
		"actions":    []map[string]any{{"type": "move", "folder": "Receipts"}}}}}, http.StatusCreated)
	var created struct{ Items []struct{ ID int64 } }
	if err := json.Unmarshal(body, &created); err != nil || len(created.Items) != 1 {
		t.Fatalf("rules = %s, %v", body, err)
	}
	c.do(http.MethodPatch, "/api/settings", map[string]any{"dry_run": false}, http.StatusOK)
	other := person(t, srv)

	// The person moves a filed receipt back to the Inbox.
	filed(t, c, srv, 1)
	moveMail(t, other, 1, "Receipts", "INBOX")
	eventually(t, "the module is told of the move", func() (bool, string) { m, _ := f.told(); return len(m) == 1, fmt.Sprint(m) })
	moves, asked := f.told()
	m := moves[0]
	if m.Mailbox != imaptest.Username || m.From != "orders@shop.example" || m.Filed != "Receipts" || m.Chosen != "INBOX" || !m.Inbox ||
		m.RuleID != created.Items[0].ID || m.RuleName != "Receipts" || m.Count != 1 || m.AccountID != account || m.FiledAt.IsZero() ||
		m.At.Before(m.FiledAt) || asked[0] != nil {
		t.Errorf("move = %+v, ask: %v", m, asked[0])
	}
	if qs := questionsOf(t, c); len(qs) != 1 || !qs[0].Inbox || qs[0].Emails != 1 || qs[0].From != "orders@shop.example" {
		t.Fatalf("questions = %+v", qs)
	}

	// MailRules' own undo and fix put mail back too: neither is the person's move.
	undone := filed(t, c, srv, 2)
	c.do(http.MethodPost, "/api/messages/"+strconv.FormatInt(undone, 10)+"/undo", nil, http.StatusOK)
	fixed := filed(t, c, srv, 3)
	c.do(http.MethodPost, "/api/messages/"+strconv.FormatInt(fixed, 10)+"/correct", map[string]any{"rule_id": nil}, http.StatusOK)
	eventually(t, "the undo and the fix are done", func() (bool, string) { n := count(t, srv, "Receipts"); return n == 0, fmt.Sprint(n) })
	time.Sleep(20 * 50 * time.Millisecond) // many sweeps
	if moves, _ := f.told(); len(moves) != 1 {
		t.Fatalf("MailRules' own moves were told: %+v", moves)
	}

	// A move to another folder asks another question; No closes it, and the next move there
	// is told but asks nothing.
	filed(t, c, srv, 4)
	moveMail(t, other, 4, "Receipts", "Later")
	eventually(t, "the move to Later is told", func() (bool, string) { m, _ := f.told(); return len(m) == 2, fmt.Sprint(m) })
	if moves, _ := f.told(); moves[1].Chosen != "Later" || moves[1].Inbox || moves[1].Count != 1 {
		t.Errorf("move to Later = %+v", moves[1])
	}
	qs := questionsOf(t, c)
	if len(qs) != 2 || qs[0].Folder != "Later" || qs[0].Inbox {
		t.Fatalf("questions = %+v", qs)
	}
	c.do(http.MethodPost, fmt.Sprintf("/api/activity/questions/%d/answer", qs[0].ID), map[string]string{"answer": "no"}, http.StatusOK)
	c.do(http.MethodPost, fmt.Sprintf("/api/activity/questions/%d/answer", qs[0].ID), map[string]string{"answer": "no"}, http.StatusNotFound)
	filed(t, c, srv, 5)
	moveMail(t, other, 5, "Receipts", "Later")
	eventually(t, "the second move to Later is told", func() (bool, string) { m, _ := f.told(); return len(m) == 3, fmt.Sprint(m) })
	if moves, asked := f.told(); moves[2].Count != 2 || !errors.Is(asked[2], ext.ErrAnswered) {
		t.Errorf("second move to Later = %+v, ask: %v", moves[2], asked[2])
	}
	if qs := questionsOf(t, c); len(qs) != 1 || !qs[0].Inbox {
		t.Fatalf("questions after No = %+v", qs)
	}
	if v, src := senderRule(t, c, "orders@shop.example"); v != "" {
		t.Errorf("No stored a sender rule: %s by %s", v, src)
	}

	// Yes: the sender's mail stays in the Inbox from now on.
	c.do(http.MethodPost, fmt.Sprintf("/api/activity/questions/%d/answer", qs[1].ID), map[string]string{"answer": "yes"}, http.StatusOK)
	if v, src := senderRule(t, c, "orders@shop.example"); v != rules.VerdictKeep || src != "user" {
		t.Errorf("sender rule after Yes: %q by %q", v, src)
	}
	if qs := questionsOf(t, c); len(qs) != 0 {
		t.Errorf("questions after Yes = %+v", qs)
	}
}

// senderRule is the verdict of the sender rule for address and who made it; empty when the
// rules decide.
func senderRule(t *testing.T, c *client, address string) (verdict, source string) {
	t.Helper()
	_, body := c.do(http.MethodGet, "/api/senders?limit=100", nil, http.StatusOK)
	var page struct {
		Items []struct {
			Value   string  `json:"value"`
			Verdict *string `json:"verdict"`
			Source  *string `json:"source"`
		}
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	for _, s := range page.Items {
		if s.Value == address && s.Verdict != nil {
			return *s.Verdict, *s.Source
		}
	}
	return "", ""
}

// A build without the module watches nothing and offers nothing: no flag, no questions.
func TestNoMovesWithoutTheModule(t *testing.T) {
	srv := imaptest.Start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}})
	d := serveForTest(t, srv.TLS)
	if _, body := d.c.do(http.MethodGet, "/api/settings", nil, http.StatusOK); !strings.Contains(string(body), `"move_questions":false`) {
		t.Errorf("features.move_questions is on: %s", body)
	}
	_, body := d.c.do(http.MethodGet, "/api/activity/questions", nil, http.StatusNotFound)
	if !strings.Contains(string(body), "not_available") {
		t.Errorf("questions without the module: %s", body)
	}
	d.c.do(http.MethodPost, "/api/activity/questions/1/answer", map[string]string{"answer": "yes"}, http.StatusNotFound)
	if movesModule(nil) != nil {
		t.Error("a build without modules has a moves module")
	}
}

// The worker hands every move over until the module takes it, and Ask of a move no longer
// kept says so.
func TestMoveWorkerHandsOverAtLeastOnce(t *testing.T) {
	ctx := t.Context()
	st, _ := storetest.New(t)
	u, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := st.CreateAccount(ctx, store.Account{UserID: u.ID, Label: "me", Preset: "generic", Host: "h", Port: 993,
		TLSMode: "implicit", Username: "me@example.test", WatchFolder: "INBOX"}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	msg, _, err := st.IngestMessage(ctx, mail.MsgRef{AccountID: acct.ID, Folder: "INBOX", UIDValidity: 1, UID: 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	a := store.Action{MessageID: msg.ID, AccountID: acct.ID, Kind: rules.ActMove, Folder: "Receipts",
		Before: store.Snapshot{Folder: "INBOX", UIDValidity: 1, UID: 1}, Status: store.ActionFailed, CreatedAt: 1}
	if a.ID, err = st.InsertAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Status, a.After = store.ActionDone, &store.Snapshot{Folder: "Receipts", UIDValidity: 2, UID: 1}
	if err := st.FinishAction(ctx, a, store.FollowUps{Notices: true}); err != nil {
		t.Fatal(err)
	}
	id, err := st.RecordMove(ctx, store.Move{TenantID: u.TenantID, AccountID: acct.ID, MessageID: msg.ID, ActionID: a.ID,
		From: "x@shop.test", Filed: "Receipts", Chosen: "INBOX", Inbox: true, Person: true, FiledAt: 1, CreatedAt: 100})
	if err != nil || id == 0 {
		t.Fatalf("RecordMove = %d, %v", id, err)
	}
	f := &fakeMover{refuse: 1}
	now := time.Unix(200, 0)
	w := newMoveWorker(st, f.module(0).Moves, nil)
	w.now = func() time.Time { return now }
	if err := w.once(ctx); err != nil {
		t.Fatal(err)
	}
	if moves, _ := f.told(); len(moves) != 0 {
		t.Fatalf("refused, yet kept: %+v", moves)
	}
	if err := w.once(ctx); err != nil {
		t.Fatal(err)
	}
	if moves, _ := f.told(); len(moves) != 0 {
		t.Fatal("handed over again before its time")
	}
	now = now.Add(retryAfter(0))
	if err := w.once(ctx); err != nil {
		t.Fatal(err)
	}
	moves, asked := f.told()
	if len(moves) != 1 || moves[0].ID != id || asked[0] != nil {
		t.Fatalf("told %+v, %v", moves, asked)
	}
	if err := w.once(ctx); err != nil {
		t.Fatal(err)
	}
	if moves, _ := f.told(); len(moves) != 1 {
		t.Errorf("handed over again once taken: %+v", moves)
	}
	if err := w.questions.Ask(ctx, id+100); !errors.Is(err, ext.ErrNoMove) {
		t.Errorf("Ask of a move not kept: %v", err)
	}
}
