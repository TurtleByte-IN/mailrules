package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

func TestCreateAccountWithin(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	signIn := func(sub, org string) User {
		u, err := s.SignInIdentity(ctx, Identity{Provider: "workos", Subject: sub, Email: sub + "@example.test", Tenant: org}, 10)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	mate, leaver, other := signIn("mate", "org_team"), signIn("leaver", "org_team"), signIn("other", "org_other")
	add := func(u User, name string, max int) error {
		_, err := s.CreateAccountWithin(ctx, Account{UserID: u.ID, Label: name, Preset: "generic", Host: "imap.test",
			Port: 993, TLSMode: "implicit", Username: name + "@imap.test"}, "pw", max)

		return err
	}
	// The team has two mailboxes, one of them private to the leaver: both count.
	if err := add(mate, "a", 3); err != nil {
		t.Fatal(err)
	}
	if err := add(leaver, "b", 3); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		who  User
		max  int
		want string // the refusal's sentence; empty = added
	}{
		{"the last place", mate, 3, ""},
		{"over the limit", mate, 3, "Your plan allows 3 mailboxes, and 3 are connected already. Remove one to connect another."},
		{"a limit lowered below the count", leaver, 2, "Your plan allows 2 mailboxes, and 3 are connected already. Remove one to connect another."},
		{"one mailbox", leaver, 1, "Your plan allows 1 mailbox, and it is connected already. Remove it to connect another."},
		{"none at all", leaver, 0, "Your plan does not include a mailbox, so this one was not connected."},
		{"no limit", leaver, NoLimit, ""},
		{"another tenant counts its own", other, 1, ""},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := s.TenantAccounts(ctx, tt.who.TenantID)
			err := add(tt.who, fmt.Sprint("m", i), tt.max)
			after, _ := s.TenantAccounts(ctx, tt.who.TenantID)
			if tt.want == "" {
				if err != nil || len(after) != len(before)+1 {
					t.Fatalf("add = %v, %d → %d mailboxes", err, len(before), len(after))
				}
				return
			}
			if !errors.Is(err, ErrMailboxLimit) || err.Error() != tt.want {
				t.Fatalf("add = %v, want %q", err, tt.want)
			}
			if len(after) != len(before) {
				t.Errorf("a refused mailbox was stored: %d → %d", len(before), len(after))
			}
		})
	}
}

// Mailboxes added at the same moment cannot together pass the limit: the count and the
// insert are one write transaction.
func TestCreateAccountWithinConcurrently(t *testing.T) {
	s, _ := open(t) // file-backed, in WAL mode: every goroutine has its own connection
	ctx := t.Context()
	u, err := s.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	const max, tries = 3, 12
	var added, refused atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range tries {
		wg.Go(func() {
			<-start
			_, err := s.CreateAccountWithin(ctx, Account{UserID: u.ID, Label: "m", Preset: "generic", Host: "imap.test",
				Port: 993, TLSMode: "implicit", Username: fmt.Sprintf("m%d@imap.test", i)}, "pw", max)

			switch {
			case err == nil:
				added.Add(1)
			case errors.Is(err, ErrMailboxLimit):
				refused.Add(1)
			default:
				t.Errorf("add %d: %v", i, err)
			}
		})
	}
	close(start)
	wg.Wait()
	all, err := s.TenantAccounts(ctx, u.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if added.Load() != max || refused.Load() != tries-max || len(all) != max {
		t.Errorf("added %d, refused %d, stored %d; want %d added and stored", added.Load(), refused.Load(), len(all), max)
	}
}

func TestAIEmailsCountOncePerEmailAndMonth(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	a := newAccount(t, s, "me@icloud.com", "pw")
	octLast := time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC)
	novFirst := octLast.Add(time.Second)
	uid := uint32(0)
	decide := func(at time.Time, metered bool, again int) {
		t.Helper()
		uid++
		m, _, err := s.IngestMessage(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: uid}, at.Unix())
		if err != nil {
			t.Fatal(err)
		}
		// A message decided again (tried again after its actions failed) is counted once.
		for range 1 + again {
			if _, err := s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "decider", CreatedAt: at.Unix(), Metered: metered}, StateDecided); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := s.Message(ctx, m.ID); got.Metered != metered {
			t.Errorf("message %d Metered = %v, want %v", m.ID, got.Metered, metered)
		}
	}
	decide(octLast, true, 2)
	decide(octLast, false, 0) // the tenant's own key, or no model
	decide(octLast, true, 0)
	decide(novFirst, true, 0)                                             // a new month starts from nothing
	if err := s.SpendAIEmails(ctx, a.TenantID, novFirst, 5); err != nil { // a cleanup check
		t.Fatal(err)
	}
	for month, want := range map[string]int{"2026-10": 2, "2026-11": 6, "2026-12": 0} {
		if got, err := s.AIEmails(ctx, a.TenantID, month); err != nil || got != want {
			t.Errorf("AIEmails(%s) = %d, %v; want %d", month, got, err, want)
		}
	}
	if got, _ := s.AIEmails(ctx, a.TenantID+1, "2026-10"); got != 0 {
		t.Errorf("another tenant has %d", got)
	}
	if Month(time.Date(2026, 11, 1, 0, 30, 0, 0, time.FixedZone("IST", 19800))) != "2026-10" {
		t.Error("Month is not in UTC")
	}
}

// A cleanup check's AI-sorted emails draw on the tenant's one-time first-cleanup grant first,
// then on the month's allowance. The grant is kept across restarts, never resets, and checks
// running at once never take more of it than there is.
func TestFirstCleanupGrant(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	openAt := func() *Store {
		t.Helper()
		db, err := Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		return keyed(db)
	}
	s := openAt()
	oct := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	spend := func(s *Store, at time.Time, grant int) bool {
		t.Helper()
		fromGrant, err := s.SpendCleanupEmail(ctx, 7, at, grant)
		if err != nil {
			t.Fatal(err)
		}
		return fromGrant
	}
	state := func(s *Store, month string) (grant, monthly int) {
		t.Helper()
		g, err1 := s.FirstCleanupUsed(ctx, 7)
		m, err2 := s.AIEmails(ctx, 7, month)
		if err := errors.Join(err1, err2); err != nil {
			t.Fatal(err)
		}
		return g, m
	}

	// No grant: straight to the month's allowance.
	if spend(s, oct, 0) {
		t.Error("an email drew on a grant of 0")
	}
	// A grant of 3: the next three draw on it, the fourth on the month.
	for i := range 3 {
		if !spend(s, oct, 3) {
			t.Fatalf("email %d did not draw on the grant", i+1)
		}
	}
	if spend(s, oct, 3) {
		t.Error("an email drew on a used-up grant")
	}
	if g, m := state(s, "2026-10"); g != 3 || m != 2 {
		t.Errorf("grant used %d, October %d; want 3 and 2", g, m)
	}

	// After a restart, and in another month, the grant is still used up.
	s = openAt()
	nov := oct.AddDate(0, 1, 0)
	if spend(s, nov, 3) {
		t.Error("the grant came back after a restart or a new month")
	}
	if g, m := state(s, "2026-11"); g != 3 || m != 1 {
		t.Errorf("grant used %d, November %d; want 3 and 1", g, m)
	}

	// A bigger grant (another plan) has room again, and concurrent checks share it exactly.
	var fromGrant atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if ok, err := s.SpendCleanupEmail(ctx, 7, nov, 8); err != nil {
				t.Error(err)
			} else if ok {
				fromGrant.Add(1)
			}
		})
	}
	wg.Wait()
	if g, m := state(s, "2026-11"); fromGrant.Load() != 5 || g != 8 || m != 1+7 {
		t.Errorf("concurrently: %d from the grant, grant used %d, November %d; want 5, 8, 8", fromGrant.Load(), g, m)
	}
}
