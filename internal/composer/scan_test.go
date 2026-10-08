package composer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// recordingReader is the fake mailbox, remembering how much text each fetch asked for.
type recordingReader struct {
	Reader
	mu      sync.Mutex
	maxBody []int
}

func (r *recordingReader) FetchMany(ctx context.Context, refs []mail.MsgRef, maxBody int) ([]*message.Raw, error) {
	r.mu.Lock()
	r.maxBody = append(r.maxBody, maxBody)
	r.mu.Unlock()
	return r.Reader.FetchMany(ctx, refs, maxBody)
}

// The scanner a module reads mail through lists the newest of a folder, fetches it in
// batches with BODY.PEEK and only as much text as asked for, and marks nothing read.
func TestScannerReadsWithoutChangingTheMailbox(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("Long text. ", 100)
	for i := range 250 {
		var flags []string
		if i%2 == 0 {
			flags = []string{`\Seen`}
		}
		e.mb.Deliver("INBOX", fmt.Sprintf("From: Shop <news@shop.example>\r\nTo: me@example.test\r\nSubject: Offer %d\r\nList-Unsubscribe: <mailto:u@shop.example>\r\n\r\n%s\r\n", i, long), flags...)
	}
	readNow := func() int {
		refs, _, err := e.mb.FetchSince(t.Context(), "INBOX", time.Time{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ref := range refs {
			flags, err := e.mb.Flags(t.Context(), ref)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(flags, `\Seen`) {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		name    string
		opts    FetchOptions
		maxBody int
		body    func(string) bool
	}{
		{"headers only", FetchOptions{NoBody: true}, headersOnly, func(b string) bool { return b == "" }},
		{"the start of the text", FetchOptions{BodyChars: 500}, 0, func(b string) bool { return len([]rune(b)) == 500 }},
		{"the whole text", FetchOptions{}, 0, func(b string) bool { return len(b) > 1000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := &recordingReader{Reader: e.mb}
			sc := Scanner{Store: e.st, Mailbox: rr, AccountID: e.acct.ID}
			refs, matched, err := sc.List(t.Context(), "INBOX", time.Time{}, 200)
			if err != nil || len(refs) != 200 || matched != 250 {
				t.Fatalf("listed %d of %d, err %v", len(refs), matched, err)
			}
			var mu sync.Mutex
			got := make([]*message.Summary, len(refs))
			seen := 0
			err = sc.Fetch(t.Context(), refs, tc.opts, func(_ context.Context, i int, sum *message.Summary, read bool) error {
				mu.Lock()
				defer mu.Unlock()
				got[i] = sum
				if read {
					seen++
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for i, sum := range got {
				if sum == nil || !tc.body(sum.Body) || sum.AccountID != e.acct.ID || len(sum.Headers["List-Unsubscribe"]) != 1 {
					t.Fatalf("email %d = %+v", i, sum)
				}
			}
			if last := got[len(got)-1].Subject; last != "Offer 249" {
				t.Errorf("the newest email is %q, want it last", last)
			}
			if seen != 100 {
				t.Errorf("%d of the newest 200 are read, want 100", seen)
			}
			// 200 emails are read in batches, not with a request each (MAI-59), and no more
			// text is asked for than wanted.
			if len(rr.maxBody) != (200+fetchBatch-1)/fetchBatch || slices.ContainsFunc(rr.maxBody, func(n int) bool { return n != tc.maxBody }) {
				t.Errorf("fetches asked for %v bytes of text, want %d each, %d fetches", rr.maxBody, tc.maxBody, (200+fetchBatch-1)/fetchBatch)
			}
			if n := readNow(); n != 125 {
				t.Errorf("%d emails are read after the scan, want 125", n)
			}
		})
	}
}
