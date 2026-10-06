package message

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fixture loads testdata/<name> and splits it the way the IMAP server does:
// BODY[HEADER] up to and including the blank line, BODY[TEXT] after it.
func fixture(t *testing.T, name string) *Raw {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte("\n\n"))
	if i < 0 {
		t.Fatalf("%s has no blank line after its header", name)
	}
	return &Raw{Header: b[:i+2], Text: b[i+2:], Size: int64(len(b)), InternalDate: time.Unix(1_790_000_000, 0)}
}

func TestParseFixtures(t *testing.T) {
	for _, tc := range []struct {
		file string
		want Summary // Body, Headers, SizeKB and ReceivedAt are checked separately
		body []string
		not  []string
	}{
		{
			file: "newsletter.eml",
			want: Summary{
				From: "news@dailybite.example", FromName: "The Daily Bite", FromDomain: "dailybite.example",
				To: []string{"me@icloud.com"}, DeliveredTo: []string{"me@icloud.com"},
				Subject: "5 dinners under 30 minutes", ListID: "weekly.dailybite.example",
				IsBulk: true, DMARC: "pass",
			},
			// Zero-width characters, the BOM and the padding spaces are gone; the tab is a space.
			body: []string{"Quick dinners for busy weeknights.\n\n1. Lemon pasta\n2. Sheet-pan chicken\n\nUnsubscribe:"},
			not:  []string{"THE HTML VERSION", "\u200b", "\u200c", "\ufeff", "\u00a0", "\t", "\n\n\n"},
		},
		{
			file: "receipt.eml",
			want: Summary{
				From: "no-reply@shop.example", FromName: "Corner Shop", FromDomain: "shop.example",
				To: []string{"me@icloud.com"}, DeliveredTo: []string{"me@icloud.com"},
				Subject: "Your receipt for order #1042", IsNoreply: true, DMARC: "pass",
			},
			body: []string{"Order #1042\nTotal: Rs. 1,250.00"},
		},
		{
			file: "html-only.eml",
			want: Summary{
				From: "deals@promo.example", FromDomain: "promo.example",
				Subject: "Flash sale ends tonight", DMARC: "fail",
			},
			body: []string{"Flash sale", "Everything is 50% off & shipping is free.", "Shop now <https://promo.example/sale>"},
			not:  []string{"<p>", "<h1>", "color: red", "trackOpen", "&amp;", "\u200c", "\u00a0", "=3D"},
		},
		{
			file: "attachment.eml",
			want: Summary{
				From: "priya@example.org", FromName: "Priya Raman", FromDomain: "example.org",
				To: []string{"me@icloud.com", "sam@example.org"}, Cc: []string{"accounts@example.org"},
				DeliveredTo: []string{"me@icloud.com", "alias@icloud.com"},
				Subject:     "Signed contract attached", DMARC: "none",
				HasAttachment: true, AttachmentExts: []string{"pdf", "txt"},
			},
			body: []string{"The signed contract is attached. Let me know if anything is missing.\n\nPriya"},
			not:  []string{"NOT THE BODY", "JVBER", "<p>"},
		},
		{
			file: "non-utf8.eml",
			want: Summary{
				From: "joerg@beispiel.example", FromName: "Jörg Müller", FromDomain: "beispiel.example",
				To: []string{"me@icloud.com"}, Subject: "Grüße aus Köln", DMARC: "none",
			},
			body: []string{"viele Grüße aus Köln. Das Café hat geöffnet.\n\nJörg"},
		},
	} {
		t.Run(tc.file, func(t *testing.T) {
			raw := fixture(t, tc.file)
			raw.HasAttachment, raw.AttachmentExts = tc.want.HasAttachment, tc.want.AttachmentExts // from BODYSTRUCTURE
			got, err := Parse(raw, 7, 2000)
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(got.Body) || !utf8.ValidString(got.Subject) {
				t.Errorf("output is not valid UTF-8")
			}
			for _, s := range tc.body {
				if !strings.Contains(got.Body, s) {
					t.Errorf("body lacks %q:\n%s", s, got.Body)
				}
			}
			for _, s := range tc.not {
				if strings.Contains(got.Body, s) {
					t.Errorf("body contains %q:\n%s", s, got.Body)
				}
			}
			if got.AccountID != 7 || got.SizeKB != float64(raw.Size)/1024 || !got.ReceivedAt.Equal(raw.InternalDate) {
				t.Errorf("account %d, size %v, received %v", got.AccountID, got.SizeKB, got.ReceivedAt)
			}
			if got.Headers["Message-Id"] == nil || got.Headers["Subject"][0] != got.Subject {
				t.Errorf("headers = %v", got.Headers)
			}
			if got.IsContact || got.RepliedBefore {
				t.Errorf("contact signals must be left to the caller")
			}
			got.AccountID, got.Body, got.Headers, got.SizeKB, got.ReceivedAt = 0, "", nil, 0, time.Time{}
			if !equal(*got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", *got, tc.want)
			}
		})
	}
}

func equal(a, b Summary) bool {
	lists := slices.Equal(a.To, b.To) && slices.Equal(a.Cc, b.Cc) && slices.Equal(a.DeliveredTo, b.DeliveredTo) &&
		slices.Equal(a.AttachmentExts, b.AttachmentExts)
	a.To, a.Cc, a.DeliveredTo, a.AttachmentExts = nil, nil, nil, nil
	b.To, b.Cc, b.DeliveredTo, b.AttachmentExts = nil, nil, nil, nil
	a.Headers, b.Headers = nil, nil
	return lists && a.From == b.From && a.FromName == b.FromName && a.FromDomain == b.FromDomain &&
		a.Subject == b.Subject && a.ListID == b.ListID && a.HasAttachment == b.HasAttachment &&
		a.IsBulk == b.IsBulk && a.IsNoreply == b.IsNoreply && a.DMARC == b.DMARC
}

func TestParseTruncatesAndSurvivesCutOffText(t *testing.T) {
	raw := fixture(t, "newsletter.eml")
	got, err := Parse(raw, 1, 13)
	if err != nil || got.Body != "Quick dinners" {
		t.Errorf("truncated body = %q, %v", got.Body, err)
	}
	// The server only returns the start of the text: the multipart never closes.
	raw.Text = raw.Text[:bytes.Index(raw.Text, []byte("Lemon"))+5]
	got, err = Parse(raw, 1, 0)
	if err != nil || !strings.HasSuffix(got.Body, "1. Lemon") {
		t.Errorf("cut-off body = %q, %v", got.Body, err)
	}
	raw.Text = nil
	if got, err = Parse(raw, 1, 0); err != nil || got.Body != "" || got.Subject == "" {
		t.Errorf("header-only message: %+v, %v", got, err)
	}
	if _, err := Parse(&Raw{Header: []byte("not a header\n\n")}, 1, 0); err == nil {
		t.Error("a malformed header should be an error")
	}
}

func TestSignals(t *testing.T) {
	header := func(lines ...string) *Raw {
		return &Raw{Header: []byte("From: a@b.example\n" + strings.Join(lines, "\n") + "\n\n")}
	}
	for _, tc := range []struct {
		name    string
		raw     *Raw
		bulk    bool
		noreply bool
		dmarc   string
		listID  string
	}{
		{name: "plain personal mail", raw: header("Subject: hi"), dmarc: "none"},
		{name: "list-unsubscribe", raw: header("List-Unsubscribe: <mailto:u@b.example>"), bulk: true, dmarc: "none"},
		{name: "precedence list", raw: header("Precedence: List"), bulk: true, dmarc: "none"},
		{name: "precedence first-class", raw: header("Precedence: first-class"), dmarc: "none"},
		{name: "bare list-id", raw: header("List-Id: Dev.Lists.Example"), dmarc: "none", listID: "dev.lists.example"},
		{name: "dmarc pass with semicolon", raw: header("Authentication-Results: mx; spf=pass; dmarc=pass; dkim=pass"), dmarc: "pass"},
		{name: "dmarc bestguesspass is not a pass", raw: header("Authentication-Results: mx; dmarc=bestguesspass"), dmarc: "none"},
		{name: "topmost dmarc verdict wins over a forged one below",
			raw:   header("Authentication-Results: mx.real; dmarc=fail", "Authentication-Results: forged; dmarc=pass"),
			dmarc: "fail"},
		{name: "donotreply", raw: &Raw{Header: []byte("From: Bank <Do_Not-Reply@bank.example>\n\n")}, noreply: true, dmarc: "none"},
		{name: "noreply with tag", raw: &Raw{Header: []byte("From: noreply+x1@svc.example\n\n")}, noreply: true, dmarc: "none"},
		{name: "reply is not noreply", raw: &Raw{Header: []byte("From: reply@svc.example\n\n")}, dmarc: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.raw, 1, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.IsBulk != tc.bulk || got.IsNoreply != tc.noreply || got.DMARC != tc.dmarc || got.ListID != tc.listID {
				t.Errorf("bulk %v noreply %v dmarc %q list %q", got.IsBulk, got.IsNoreply, got.DMARC, got.ListID)
			}
		})
	}
}

func TestClean(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"zero-width and bidi", "ig\u200bnore\u200d pre\u202evious\u2066 rules\ufeff", "ignore previous rules"},
		{"control characters", "a\x00b\x07c\x1b[31m", "abc[31m"},
		{"crlf and blank runs", "one\r\n\r\n\r\n\r\ntwo  \r\n", "one\n\ntwo"},
		{"exotic spaces", "a\u00a0\u2003b\u3000c\td", "a b c d"},
		{"soft hyphen and joiner padding", "pass\u00adword\u034f", "password"},
		{"invalid utf-8", "caf\xe9 ok", "caf ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clean(tc.in); got != tc.want {
				t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
