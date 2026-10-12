package store

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
)

// fakeKMS is a module's key service (ext.KeyProvider): it wraps under a key of its own,
// counts its calls, can be taken down, and can hold every unwrap until let go.
type fakeKMS struct {
	key            crypto.MasterKey
	down           atomic.Bool
	wraps, unwraps atomic.Int32
	mu             sync.Mutex
	gate           chan struct{} // when set, each Unwrap waits for it to close, or for its ctx
	asked          []string      // the key ids Unwrap was asked for
}

const fakeKMSKey = "projects/p/locations/global/keyRings/mailrules/cryptoKeys/tenant-keys"

var errKMSDown = errors.New("rpc error: code = Unavailable desc = connection refused")

func newFakeKMS() *fakeKMS { return &fakeKMS{key: crypto.MasterKey(bytes.Repeat([]byte{42}, 32))} }

func (f *fakeKMS) KeyID() string { return fakeKMSKey }

func (f *fakeKMS) Wrap(ctx context.Context, tenantID int64, key []byte) ([]byte, error) {
	f.wraps.Add(1)
	if f.down.Load() {
		return nil, errKMSDown
	}
	return f.key.Wrap(ctx, tenantID, key)
}

func (f *fakeKMS) Unwrap(ctx context.Context, tenantID int64, keyID string, wrapped []byte) ([]byte, error) {
	f.unwraps.Add(1)
	f.mu.Lock()
	f.asked = append(f.asked, keyID)
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.down.Load() {
		return nil, errKMSDown
	}
	if keyID != fakeKMSKey {
		return nil, crypto.ErrDecrypt
	}
	return f.key.Unwrap(ctx, tenantID, crypto.MasterKeyID, wrapped)
}

func (f *fakeKMS) hold() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
	return f.gate
}

// clock is a Keyring's time, moved by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ringFor is a Keyring of the key service alone (no master key), keeping keys an hour on c.
func ringFor(f *fakeKMS, master crypto.MasterKey, c *clock) *Keyring {
	k := NewKeyring(crypto.WithMasterKey(f, master), time.Hour)
	k.now = c.now
	return k
}

func newClock() *clock { return &clock{t: time.Unix(1_000_000, 0)} }

// kmsWorld is the self-host admin with one mailbox, its password sealed under a tenant key
// the key service wrapped. It returns the store without keys, so each test brings a
// Keyring of its own, which has unwrapped nothing yet.
func kmsWorld(t *testing.T) (*Store, *fakeKMS, int64) {
	t.Helper()
	s, _ := open(t)
	plain := &Store{db: s.db}
	f := newFakeKMS()
	ks := plain.WithKeyring(ringFor(f, nil, newClock()))
	ctx := t.Context()
	u, err := ks.CreateFirstUser(ctx, "admin@self.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ks.CreateAccount(ctx, Account{UserID: u.ID, Label: "a", Preset: "generic", Host: "imap.test", Port: 993,
		TLSMode: "implicit", Username: "a@imap.test", CreatedAt: 1}, "pw")
	if err != nil {
		t.Fatal(err)
	}
	f.unwraps.Store(0)
	return plain, f, a.ID
}

func secretIs(t *testing.T, s *Store, ctx context.Context, id int64, want string) {
	t.Helper()
	if got, err := s.AccountSecret(ctx, id); err != nil || got != want {
		t.Fatalf("secret = %q, %v; want %q", got, err, want)
	}
}

// An unwrapped key is kept for the Keyring's time, then asked for again.
func TestKeyringKeepsAKeyForItsTime(t *testing.T) {
	s, f, acct := kmsWorld(t)
	ctx, c := t.Context(), newClock()
	ks := s.WithKeyring(ringFor(f, nil, c))
	for range 3 {
		secretIs(t, ks, ctx, acct, "pw")
	}
	c.add(59 * time.Minute)
	secretIs(t, ks, ctx, acct, "pw")
	if n := f.unwraps.Load(); n != 1 {
		t.Fatalf("within its time: %d unwraps, want 1", n)
	}
	c.add(2 * time.Minute)
	secretIs(t, ks, ctx, acct, "pw")
	if n := f.unwraps.Load(); n != 2 {
		t.Fatalf("after its time: %d unwraps, want 2", n)
	}
}

// The key is let go of once its time is up, not only replaced when next asked for.
func TestKeyringLetsGoOfAKey(t *testing.T) {
	s, f, acct := kmsWorld(t)
	ring := NewKeyring(crypto.WithMasterKey(f, nil), 20*time.Millisecond)
	secretIs(t, s.WithKeyring(ring), t.Context(), acct, "pw")
	held := func() int {
		ring.mu.Lock()
		defer ring.mu.Unlock()
		return len(ring.keys)
	}
	if held() != 1 {
		t.Fatal("the key was not kept")
	}
	deadline := time.Now().Add(5 * time.Second)
	for held() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the key is still in memory after its time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Requests that need a key at the same moment share one unwrap.
func TestKeyringOneUnwrapForRequestsAtOnce(t *testing.T) {
	s, f, acct := kmsWorld(t)
	ctx := t.Context()
	ks := s.WithKeyring(ringFor(f, nil, newClock()))
	gate := f.hold()
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			pw, err := ks.AccountSecret(ctx, acct)
			if err == nil && pw != "pw" {
				err = errors.New("wrong secret " + pw)
			}
			errs <- err
		})
	}
	waitUntil(t, func() bool { return f.unwraps.Load() > 0 })
	time.Sleep(50 * time.Millisecond) // the others reach the unwrap in flight
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.unwraps.Load(); got != 1 {
		t.Fatalf("%d unwraps for %d requests at once, want 1", got, n)
	}
}

// A request that ends while the key it asked for is still being unwrapped does not fail
// the others waiting for it: one of them asks again.
func TestKeyringOthersAskAgainWhenTheFirstGivesUp(t *testing.T) {
	s, f, acct := kmsWorld(t)
	ctx := t.Context()
	ks := s.WithKeyring(ringFor(f, nil, newClock()))
	gate := f.hold()
	first, cancel := context.WithCancel(ctx)
	firstErr := make(chan error, 1)
	go func() {
		_, err := ks.AccountSecret(first, acct)
		firstErr <- err
	}()
	waitUntil(t, func() bool { return f.unwraps.Load() > 0 })
	secondErr := make(chan error, 1)
	go func() {
		pw, err := ks.AccountSecret(ctx, acct)
		if err == nil && pw != "pw" {
			err = errors.New("wrong secret " + pw)
		}
		secondErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // the second waits for the first's unwrap
	cancel()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("the request that gave up: %v", err)
	}
	waitUntil(t, func() bool { return f.unwraps.Load() > 1 })
	close(gate)
	if err := <-secondErr; err != nil {
		t.Fatalf("the request still waiting: %v", err)
	}
}

// While the key service cannot be reached, keys already unwrapped work until their time is
// up; then the tenant's secrets neither open nor are sealed, with an error that says the
// service is unreachable, never that the key is wrong, so nothing is dropped as lost.
func TestKeyringKeyServiceUnreachable(t *testing.T) {
	s, f, acct := kmsWorld(t)
	ctx, c := t.Context(), newClock()
	ks := s.WithKeyring(ringFor(f, nil, c))
	secretIs(t, ks, ctx, acct, "pw")
	f.down.Store(true)
	c.add(30 * time.Minute)
	secretIs(t, ks, ctx, acct, "pw")

	c.add(31 * time.Minute)
	_, err := ks.AccountSecret(ctx, acct)
	if !errors.Is(err, crypto.ErrKeyUnavailable) || errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("after its time: %v, want ErrKeyUnavailable", err)
	}
	if err := ks.SetAccountSecret(ctx, acct, "new"); !errors.Is(err, crypto.ErrKeyUnavailable) {
		t.Fatalf("sealing: %v, want ErrKeyUnavailable", err)
	}
	if _, err := ks.ReplaceUnopenableTenantKeys(ctx); !errors.Is(err, crypto.ErrKeyUnavailable) {
		t.Fatalf("replacing keys: %v, want ErrKeyUnavailable", err)
	}
	if _, err := ks.CheckSecrets(ctx); !errors.Is(err, crypto.ErrKeyUnavailable) {
		t.Fatalf("checking secrets: %v, want ErrKeyUnavailable", err)
	}
	if _, err := ks.TenantKey(ctx, SelfHostTenant); !errors.Is(err, crypto.ErrKeyUnavailable) {
		t.Fatalf("tenant key: %v, want ErrKeyUnavailable", err)
	}
	// The key service answering again is all it takes: nothing was dropped or changed.
	f.down.Store(false)
	secretIs(t, ks, ctx, acct, "pw")
}

// A new tenant gets its key when it is made; while the key service cannot be reached the
// sign-in fails and leaves nothing behind.
func TestSignUpNeedsTheKeyService(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	f := newFakeKMS()
	ks := (&Store{db: s.db}).WithKeyring(ringFor(f, nil, newClock()))
	count := func(q string) int {
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tenants := count(`SELECT COUNT(*) FROM tenants`)
	f.down.Store(true)
	id := identity("user_new", "new@example.com", "org_new")
	if _, err := ks.SignInIdentity(ctx, id, 10); !errors.Is(err, crypto.ErrKeyUnavailable) {
		t.Fatalf("sign-up while the key service is down: %v, want ErrKeyUnavailable", err)
	}
	if n := count(`SELECT COUNT(*) FROM tenants`); n != tenants {
		t.Fatalf("%d tenants after a failed sign-up, want %d", n, tenants)
	}
	if n := count(`SELECT COUNT(*) FROM users`); n != 0 {
		t.Fatalf("%d users after a failed sign-up", n)
	}
	f.down.Store(false)
	u, err := ks.SignInIdentity(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped []byte
	var kek string
	if err := db.QueryRowContext(ctx, `SELECT dek_enc, kek_id FROM tenants WHERE id = ?`, u.TenantID).Scan(&wrapped, &kek); err != nil {
		t.Fatal(err)
	}
	if len(wrapped) == 0 || kek != fakeKMSKey {
		t.Fatalf("the new tenant's key: %d bytes under %q", len(wrapped), kek)
	}
}

// Moving an install from the master key to a key service rewraps each tenant key once,
// reseals the removed mailboxes' copies, and leaves nothing that needs the master key:
// everything opens with the key service alone, nothing with the master key alone.
func TestMoveFromTheMasterKeyToAKeyService(t *testing.T) {
	w := forgetWorld(t)
	ctx := t.Context()
	// A mailbox a module connected is removed: its copy waits under the master key itself.
	if _, err := w.db.ExecContext(ctx, `UPDATE accounts SET oauth = TRUE, module = TRUE, preset = 'gmail' WHERE id = ?`, w.soloBox); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.DeleteAccount(ctx, w.soloBox, fNow); err != nil {
		t.Fatal(err)
	}
	removed, err := w.s.WaitingRemovedMailboxes(ctx, 1<<40, 10)
	if err != nil || len(removed) != 1 {
		t.Fatalf("removed = %+v, %v", removed, err)
	}
	master := crypto.MasterKey(testMaster)
	f := newFakeKMS()
	ks := w.s.WithKeyring(ringFor(f, master, newClock()))
	uses, err := ks.MasterKeyUses(ctx)
	if want := (MasterKeyUses{TenantKeys: 4, RemovedMailboxes: 1}); err != nil || uses != want {
		t.Fatalf("before: %+v, %v; want %+v", uses, err, want)
	}
	opens := func(name string, s *Store) {
		t.Helper()
		secretIs(t, s, ctx, w.otherBox, "pw-other")
		if sec, err := s.RemovedMailboxSecret(ctx, removed[0].ID); err != nil || sec != "pw-solo" {
			t.Fatalf("%s: removed mailbox's secret = %q, %v", name, sec, err)
		}
	}
	opens("before the move", ks)

	n, err := ks.RewrapTenantKeys(ctx)
	if err != nil || n != 4 {
		t.Fatalf("rewrapped %d, %v; want 4 (admin, solo, team, other)", n, err)
	}
	if n, err := ks.RewrapTenantKeys(ctx); err != nil || n != 0 {
		t.Fatalf("a second run rewrapped %d, %v", n, err)
	}
	if n, err := ks.UpgradeSecrets(ctx); err != nil || n != 1 {
		t.Fatalf("resealed %d, %v; want 1 (the removed mailbox's copy)", n, err)
	}
	if n, err := ks.UpgradeSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("a second upgrade resealed %d, %v", n, err)
	}
	if uses, err := ks.MasterKeyUses(ctx); err != nil || uses.Total() != 0 {
		t.Fatalf("after: %+v, %v; want nothing", uses, err)
	}
	opens("after the move", ks)
	opens("without the master key", w.s.WithKeyring(ringFor(f, nil, newClock())))
	if _, err := w.s.WithKeys(master).AccountSecret(ctx, w.otherBox); !errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("the master key alone: %v, want ErrDecrypt", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Contains(f.asked, crypto.MasterKeyID) {
		t.Error("the key service was asked to unwrap what the master key wrapped")
	}
}

// A key service that cannot be reached during the move leaves every tenant key under the
// master key, which still opens it.
func TestMoveWaitsForTheKeyService(t *testing.T) {
	w := forgetWorld(t)
	ctx := t.Context()
	f := newFakeKMS()
	f.down.Store(true)
	ks := w.s.WithKeyring(ringFor(f, crypto.MasterKey(testMaster), newClock()))
	if n, err := ks.RewrapTenantKeys(ctx); !errors.Is(err, crypto.ErrKeyUnavailable) || n != 0 {
		t.Fatalf("rewrapped %d, %v; want 0 and ErrKeyUnavailable", n, err)
	}
	secretIs(t, ks, ctx, w.otherBox, "pw-other")
	if uses, err := ks.MasterKeyUses(ctx); err != nil || uses.TenantKeys != 4 {
		t.Fatalf("uses = %+v, %v; want the 4 tenant keys", uses, err)
	}
	// A tenant key the master key does not open is left as it is, not counted as moved.
	other := w.s.WithKeyring(ringFor(newFakeKMS(), crypto.MasterKey(bytes.Repeat([]byte{8}, 32)), newClock()))
	if n, err := other.RewrapTenantKeys(ctx); err != nil || n != 0 {
		t.Fatalf("with another master key: rewrapped %d, %v", n, err)
	}
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
