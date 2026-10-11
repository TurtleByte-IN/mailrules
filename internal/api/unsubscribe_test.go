package api

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/unsubscribe"
	"github.com/TurtleByte-IN/mailrules/internal/unsubscribe/unsubscribetest"
)

// lockedBuffer is a log destination the daemon's goroutines may write to while a test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

const oneClickPost = "List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n"

// deliverList delivers a list email with these extra header lines to box.
func deliverList(box *mailtest.Mailbox, from, subject, headers string) {
	box.Deliver("INBOX", "From: "+from+"\r\nTo: me@example.test\r\nSubject: "+subject+"\r\n"+headers+
		"Message-ID: <"+strings.ReplaceAll(subject, " ", "-")+"@example.test>\r\n\r\nNews.\r\n")
}

// sendersByValue lists the Senders rows by address or domain, each checked against the contract.
func (e *env) sendersByValue() map[string]map[string]any {
	e.t.Helper()
	out := map[string]map[string]any{}
	for _, it := range e.call(http.MethodGet, "/api/senders", "", http.StatusOK)["items"].([]any) {
		s := it.(map[string]any)
		conform(e.t, e.doc, "Sender", s)
		out[s["value"].(string)] = s
	}
	return out
}

// One-click unsubscribe (MAI-105, RFC 8058): only an https link with the one-click Post
// header is offered and called; one click unsubscribes every mailbox, is remembered per
// mailbox, and sends what still comes to Trash. Dry-run, which is on here, does not stop it.
func TestUnsubscribe(t *testing.T) {
	var logs lockedBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	fake := &unsubscribetest.Fake{}
	e := newEnvWith(t, func(o *Options) { o.Unsubscriber = fake })
	e.signIn()
	if e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["unsubscribe"] != true {
		t.Fatal("features.unsubscribe is off")
	}
	e.call(http.MethodPost, "/api/accounts", accountBody, http.StatusCreated)
	e.live()
	if e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["dry_run"] != true {
		t.Fatal("dry-run is off")
	}

	both := "List-Unsubscribe: <mailto:u@one.example>, <https://one.example/u/%s?list=1>\r\n" + oneClickPost
	deliverList(e.mb, "One <news@one.example>", "one old", fmt.Sprintf(both, "token-old"))
	deliverList(e.mb, "One <news@one.example>", "one new", fmt.Sprintf(both, "token-new"))
	deliverList(e.mb, "Two <news@two.example>", "two", "List-Unsubscribe: <mailto:u@two.example>\r\n"+oneClickPost)
	deliverList(e.mb, "Three <news@three.example>", "three", "List-Unsubscribe: <http://three.example/u/token-3>\r\n"+oneClickPost)
	deliverList(e.mb, "Four <news@four.example>", "four", "List-Unsubscribe: <https://four.example/u/token-4>\r\n")
	deliverList(e.mb, "Five <news@five.example>", "five", "List-Unsubscribe: <https://five.example/u/token-5>\r\n"+oneClickPost)
	deliverList(e.mb, "Six <news@six.example>", "six", "List-Unsubscribe: <https://six.example/u/token-6>\r\n"+oneClickPost)
	deliverList(e.mb, "Seven <news@seven.example>", "seven", "List-Unsubscribe: <https://seven.example/u/token-7>\r\n"+oneClickPost)
	var rows map[string]map[string]any
	eventually(t, "the senders to be listed", func() bool { rows = e.sendersByValue(); return len(rows) == 7 })

	for addr, want := range map[string][2]bool{ // has_list_unsubscribe, one_click_unsubscribe
		"news@one.example": {true, true}, "news@two.example": {true, false}, "news@three.example": {true, false},
		"news@four.example": {true, false}, "news@five.example": {true, true},
	} {
		if r := rows[addr]; r["has_list_unsubscribe"] != want[0] || r["one_click_unsubscribe"] != want[1] || r["unsubscribed_at"] != nil {
			t.Errorf("%s = %v, want list %v, one-click %v", addr, r, want[0], want[1])
		}
	}

	// One click: the newest email's link is called once, and the row says it is done, with a
	// sender rule that trashes the stragglers.
	got := e.call(http.MethodPost, "/api/senders/address/news%40one.example/unsubscribe", "", http.StatusOK)["sender"].(map[string]any)
	conform(t, e.doc, "Sender", got)
	if calls := fake.Calls(); len(calls) != 1 || calls[0] != "https://one.example/u/token-new?list=1" {
		t.Fatalf("links called = %v", calls)
	}
	if got["unsubscribed_at"] == nil || got["one_click_unsubscribe"] != true || got["verdict"] != "block" || got["source"] != "user" {
		t.Errorf("after unsubscribing = %v", got)
	}
	// It survives a reload, and asking again calls nothing.
	if r := e.sendersByValue()["news@one.example"]; r["unsubscribed_at"] != got["unsubscribed_at"] || r["verdict"] != "block" {
		t.Errorf("after a reload = %v", r)
	}
	if again := e.call(http.MethodPost, "/api/senders/address/news%40one.example/unsubscribe", "", http.StatusOK)["sender"].(map[string]any); again["unsubscribed_at"] != got["unsubscribed_at"] || len(fake.Calls()) != 1 {
		t.Errorf("again = %v, calls %v", again, fake.Calls())
	}

	// No one-click link: mailto only, http, no Post header, no mail at all, a domain.
	for _, addr := range []string{"news%40two.example", "news%40three.example", "news%40four.example", "quiet%40nowhere.example"} {
		e.refuse(http.MethodPost, "/api/senders/address/"+addr+"/unsubscribe", "", http.StatusConflict, "no_unsubscribe_link", "")
	}
	e.refuse(http.MethodPost, "/api/senders/domain/one.example/unsubscribe", "", http.StatusBadRequest, "invalid_input", "type")
	e.refuse(http.MethodPost, "/api/senders/address/not-an-address/unsubscribe", "", http.StatusBadRequest, "invalid_input", "value")
	if n := len(fake.Calls()); n != 1 {
		t.Errorf("links called for senders without one: %d", n-1)
	}

	// A site that refuses, or a link to a private network: 502 naming the host, never the
	// link, and nothing is recorded.
	fake.SetErr(func(link string) error {
		switch unsubscribe.Host(link) {
		case "five.example":
			return &unsubscribe.RefusedError{Host: "five.example", Status: 500}
		case "six.example":
			return fmt.Errorf("unsubscribe at six.example: connect to 10.0.0.1:443: %w", mail.ErrNotPublic)
		}
		return nil
	})
	for addr, want := range map[string]string{
		"news@five.example": "five.example answered 500",
		"news@six.example":  "The unsubscribe link of six.example leads to a private network",
	} {
		r := e.do(http.MethodPost, "/api/senders/address/"+strings.ReplaceAll(addr, "@", "%40")+"/unsubscribe", "")
		if r.status != http.StatusBadGateway || r.body.Error.Code != "unsubscribe_failed" || !strings.Contains(r.body.Error.Message, want) ||
			strings.Contains(r.body.Error.Message, "token") {
			t.Errorf("%s = %d %+v", addr, r.status, r.body.Error)
		}
		if row := e.sendersByValue()[addr]; row["unsubscribed_at"] != nil || row["verdict"] != nil {
			t.Errorf("%s after a failure = %v", addr, row)
		}
	}

	// An address that already has a sender rule keeps it.
	fake.SetErr(nil)
	e.call(http.MethodPut, "/api/senders/address/news%40seven.example", `{"verdict":"keep"}`, http.StatusOK)
	if got := e.call(http.MethodPost, "/api/senders/address/news%40seven.example/unsubscribe", "", http.StatusOK)["sender"].(map[string]any); got["verdict"] != "keep" || got["unsubscribed_at"] == nil {
		t.Errorf("a kept sender after unsubscribing = %v", got)
	}

	// Two mailboxes get the same list: each is unsubscribed with its own link. One that fails
	// is tried again on the next click; the one done is not called twice.
	second := e.tenantAccount("me2@example.test")
	deliverList(e.mb, "Eight <news@eight.example>", "eight first", "List-Unsubscribe: <https://eight.example/u/first>\r\n"+oneClickPost)
	deliverList(e.boxes["me2@example.test"], "Eight <news@eight.example>", "eight second", "List-Unsubscribe: <https://eight.example/u/second>\r\n"+oneClickPost)
	eventually(t, "both emails to be seen", func() bool { return e.sendersByValue()["news@eight.example"]["messages"] == float64(2) })
	fake.SetErr(func(link string) error {
		if strings.HasSuffix(link, "/second") {
			return &unsubscribe.RefusedError{Host: "eight.example", Status: 503}
		}
		return nil
	})
	before := len(fake.Calls())
	e.refuse(http.MethodPost, "/api/senders/address/news%40eight.example/unsubscribe", "", http.StatusBadGateway, "unsubscribe_failed", "")
	if row := e.sendersByValue()["news@eight.example"]; row["unsubscribed_at"] != nil || row["verdict"] != nil {
		t.Errorf("half unsubscribed = %v", row)
	}
	fake.SetErr(nil)
	got = e.call(http.MethodPost, "/api/senders/address/news%40eight.example/unsubscribe", "", http.StatusOK)["sender"].(map[string]any)
	if calls := fake.Calls()[before:]; len(calls) != 3 || calls[0] != "https://eight.example/u/first" || calls[1] != "https://eight.example/u/second" || calls[2] != "https://eight.example/u/second" {
		t.Errorf("links called = %v", calls)
	}
	if got["unsubscribed_at"] == nil || got["verdict"] != "block" {
		t.Errorf("after both mailboxes = %v", got)
	}
	if n := e.count(fmt.Sprintf(`SELECT COUNT(*) FROM unsubscribes WHERE address = 'news@eight.example' AND account_id IN (1, %d)`, second)); n != 2 {
		t.Errorf("unsubscribes recorded = %d, want one per mailbox", n)
	}

	// Logs name the host and never the link, which carries a token naming the person.
	if l := logs.String(); !strings.Contains(l, "host=one.example") || strings.Contains(l, "token") || strings.Contains(l, "/u/") {
		t.Errorf("logs:\n%s", l)
	}
}
