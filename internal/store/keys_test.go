package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store/pgtest"
)

// Each tenant's secrets are under a key of its own: a ciphertext moved to another tenant's
// row, or to another row of the same tenant, does not open.
func TestTenantKeysKeepSecretsApart(t *testing.T) {
	w := forgetWorld(t)
	s, db, ctx := w.s, w.db, t.Context()
	var keys [][]byte
	for _, tenant := range []int64{w.solo.TenantID, w.other.TenantID} {
		var wrapped []byte
		var kek string
		if err := db.QueryRowContext(ctx, `SELECT dek_enc, kek_id FROM tenants WHERE id = ?`, tenant).Scan(&wrapped, &kek); err != nil {
			t.Fatal(err)
		}
		if len(wrapped) == 0 || kek != crypto.MasterKeyID {
			t.Fatalf("tenant %d: key %x under %q", tenant, wrapped, kek)
		}
		keys = append(keys, wrapped)
	}
	if bytes.Equal(keys[0], keys[1]) {
		t.Fatal("two tenants share a wrapped key")
	}
	var enc, dek []byte
	if err := db.QueryRowContext(ctx, `SELECT secret_enc, dek_enc FROM accounts WHERE id = ?`, w.soloBox).Scan(&enc, &dek); err != nil {
		t.Fatal(err)
	}
	if crypto.Envelope(dek) != 2 {
		t.Fatalf("a new secret is in envelope %d, want 2", crypto.Envelope(dek))
	}
	for name, id := range map[string]int64{"another tenant's mailbox": w.otherBox, "another mailbox of the tenant": w.adminBox} {
		if _, err := db.ExecContext(ctx, `UPDATE accounts SET secret_enc = ?, dek_enc = ? WHERE id = ?`, enc, dek, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AccountSecret(ctx, id); !errors.Is(err, crypto.ErrDecrypt) {
			t.Errorf("moved to %s: %v, want ErrDecrypt", name, err)
		}
	}
	if pw, err := s.AccountSecret(ctx, w.soloBox); err != nil || pw != "pw-solo" {
		t.Errorf("the original: %q, %v", pw, err)
	}
}

// Removing a tenant removes its key: what was sealed under it no longer opens anywhere,
// while the copy a removed mailbox keeps for its module still does.
func TestRemovedTenantsKeyIsShredded(t *testing.T) {
	w := forgetWorld(t)
	s, db, ctx := w.s, w.db, t.Context()
	if _, err := db.ExecContext(ctx, `UPDATE accounts SET module = TRUE, oauth = TRUE WHERE id = ?`, w.soloBox); err != nil {
		t.Fatal(err)
	}
	var enc, dek, wrapped []byte
	if err := db.QueryRowContext(ctx, `SELECT secret_enc, dek_enc FROM accounts WHERE id = ?`, w.soloBox).Scan(&enc, &dek); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT dek_enc FROM tenants WHERE id = ?`, w.solo.TenantID).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
	// Removal of the solo tenant, the module's mailbox queued in the same transaction.
	if _, err := s.DeleteOwnAccount(ctx, "workos", "user_solo", fNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveForgotten(ctx, w.solo.ID, fDue); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants WHERE dek_enc = ?`, wrapped).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the removed tenant's key is still stored: %d, %v", n, err)
	}
	// Even put back under its old binding, in a tenant of the same number, it opens with no key.
	key, err := s.TenantKey(ctx, w.other.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := key.Open(secretAccount, "1", 0, enc, dek); !errors.Is(err, crypto.ErrDecrypt) {
		t.Errorf("another tenant's key opened the removed tenant's secret: %v", err)
	}
	removed, err := s.WaitingRemovedMailboxes(ctx, fDue, 10)
	if err != nil || len(removed) != 1 || removed[0].AccountID != w.soloBox {
		t.Fatalf("removed mailboxes = %+v, %v", removed, err)
	}
	if pw, err := s.RemovedMailboxSecret(ctx, removed[0].ID); err != nil || pw != "pw-solo" {
		t.Errorf("the module's copy after the tenant went: %q, %v", pw, err)
	}
}

// The key check unwraps each tenant's key instead of opening each secret: the right key reads
// everything, another reads nothing, and a store with no key counts what a key protects.
func TestCheckSecrets(t *testing.T) {
	w := forgetWorld(t)
	ctx := t.Context()
	got, err := w.s.CheckSecrets(ctx)
	// Per seeded mailbox: its password and its destination; for three tenants a model key,
	// which forgetWorld stores as a placeholder that no key opens.
	const total = 6*2 + 3
	if err != nil || got.Total != total || got.Unreadable != 3 {
		t.Fatalf("right key: %+v, %v; want %d, the 3 placeholders unreadable", got, err, total)
	}
	if got, err := w.s.WithKeys(crypto.MasterKey(bytes.Repeat([]byte{8}, 32))).CheckSecrets(ctx); err != nil || got.Total != total || got.Readable() != 0 {
		t.Errorf("another key: %+v, %v", got, err)
	}
	if got, err := New(w.db).CheckSecrets(ctx); err != nil || got.Total != total || got.Readable() != 0 {
		t.Errorf("no key: %+v, %v", got, err)
	}
}

// A database an older version wrote, every secret under the master key itself, opens as it
// is, and UpgradeSecrets reseals each under its tenant's key (the removed mailbox's copy
// under the KEK), once. The fixture was made at fe70b0f (testdata/v1/mkfixture.go).
func TestUpgradeFromMasterKeySecrets(t *testing.T) {
	sqliteOnly(t, "the fixture is a self-host SQLite database")
	ctx := t.Context()
	dir := t.TempDir()
	copyFile(t, filepath.Join("testdata", "v1", "mailrules.db"), filepath.Join(dir, "mailrules.db"))
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := New(db).WithKeys(crypto.MasterKey(bytes.Repeat([]byte{7}, 32)))
	accounts, err := s.Accounts(ctx)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts = %+v, %v", accounts, err)
	}
	want := map[string]string{"admin@icloud.com": "self-host app password", "a@fastmail.test": "hosted app password"}
	opens := func(when string) {
		t.Helper()
		for _, a := range accounts {
			if pw, err := s.AccountSecret(ctx, a.ID); err != nil || pw != want[a.Username] {
				t.Errorf("%s: %s = %q, %v", when, a.Username, pw, err)
			}
		}
		k, err := s.TenantKey(ctx, SelfHostTenant)
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.Setting(ctx, SelfHostTenant, "key.anthropic_api_key")
		if err != nil {
			t.Fatal(err)
		}
		if key, err := OpenSetting(k, "anthropic_api_key", v); err != nil || key != "sk-ant-self" {
			t.Errorf("%s: model key %q, %v", when, key, err)
		}
		ds, err := s.Destinations(ctx, SelfHostTenant)
		if err != nil || len(ds) != 1 {
			t.Fatalf("destinations = %+v, %v", ds, err)
		}
		if sec, err := s.DestinationSecret(ctx, ds[0].ID); err != nil || sec.URL != "https://hooks.test/self" || sec.Secret != "whsec_self" {
			t.Errorf("%s: destination %+v, %v", when, sec, err)
		}
		rs, err := s.WaitingRemovedMailboxes(ctx, 1<<40, 10)
		if err != nil || len(rs) != 1 {
			t.Fatalf("removed mailboxes = %+v, %v", rs, err)
		}
		if tok, err := s.RemovedMailboxSecret(ctx, rs[0].ID); err != nil || tok != "refresh-token-a" {
			t.Errorf("%s: removed mailbox %q, %v", when, tok, err)
		}
	}
	check, err := s.CheckSecrets(ctx)
	if err != nil || check.Total != 4 || check.Unreadable != 0 {
		t.Fatalf("check before = %+v, %v; want 4 readable", check, err)
	}
	if bad, err := s.WithKeys(crypto.MasterKey(bytes.Repeat([]byte{8}, 32))).CheckSecrets(ctx); err != nil || bad.Readable() != 0 {
		t.Errorf("another master key before the upgrade: %+v, %v", bad, err)
	}
	opens("before the upgrade")
	n, err := s.UpgradeSecrets(ctx)
	if err != nil || n != 5 {
		t.Fatalf("upgraded %d, %v; want 5 (2 mailboxes, a model key, a destination, a removed mailbox)", n, err)
	}
	envelopes := map[string]int{}
	readEnvelopes := func(what, query string) {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var dek []byte
			if err := rows.Scan(&dek); err != nil {
				t.Fatal(err)
			}
			if e := crypto.Envelope(dek); envelopes[what] == 0 || envelopes[what] == e {
				envelopes[what] = e
			} else {
				envelopes[what] = -1
			}
		}
	}
	readEnvelopes("accounts", `SELECT dek_enc FROM accounts`)
	readEnvelopes("notify_destinations", `SELECT dek_enc FROM notify_destinations`)
	readEnvelopes("removed_mailboxes", `SELECT dek_enc FROM removed_mailboxes`)
	if envelopes["accounts"] != 2 || envelopes["notify_destinations"] != 2 || envelopes["removed_mailboxes"] != 3 {
		t.Errorf("envelopes after the upgrade: %v", envelopes)
	}
	opens("after the upgrade")
	if n, err := s.UpgradeSecrets(ctx); err != nil || n != 0 {
		t.Errorf("a second upgrade resealed %d, %v", n, err)
	}
	if check, err := s.CheckSecrets(ctx); err != nil || check.Total != 4 || check.Unreadable != 0 {
		t.Errorf("check after = %+v, %v", check, err)
	}
}

// The same upgrade on whichever database the tests run on: the fixture's envelope-1
// ciphertexts, bound only to their row ids, are put on rows of the same ids here. The
// fixture's master key is testMaster.
func TestUpgradeSecretsOnThisDatabase(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	copyFile(t, filepath.Join("testdata", "v1", "mailrules.db"), filepath.Join(dir, "mailrules.db"))
	old, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	blob := func(query string) (secretEnc, dekEnc []byte) {
		t.Helper()
		if err := old.QueryRowContext(ctx, query).Scan(&secretEnc, &dekEnc); err != nil {
			t.Fatal(err)
		}
		return secretEnc, dekEnc
	}
	accSec, accDek := blob(`SELECT secret_enc, dek_enc FROM accounts WHERE id = 1`)
	destSec, destDek := blob(`SELECT secret_enc, dek_enc FROM notify_destinations WHERE id = 1`)
	remSec, remDek := blob(`SELECT secret_enc, dek_enc FROM removed_mailboxes WHERE account_id = 2`)
	var setting string
	if err := old.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'key.anthropic_api_key'`).Scan(&setting); err != nil {
		t.Fatal(err)
	}

	s, db := open(t)
	a := newAccount(t, s, "admin@icloud.com", "placeholder")
	d, err := s.CreateDestination(ctx, Destination{TenantID: SelfHostTenant, Name: "Hook", Kind: DestinationWebhook}, DestinationSecret{URL: "x"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 1 || d.ID != 1 {
		t.Fatalf("ids %d and %d, want 1", a.ID, d.ID)
	}
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`UPDATE accounts SET secret_enc = ?, dek_enc = ? WHERE id = 1`, []any{accSec, accDek}},
		{`UPDATE notify_destinations SET secret_enc = ?, dek_enc = ? WHERE id = 1`, []any{destSec, destDek}},
	} {
		if _, err := db.ExecContext(ctx, q.query, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSetting(ctx, SelfHostTenant, "key.anthropic_api_key", setting); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO removed_mailboxes (account_id, preset, oauth, username, secret_enc, dek_enc, created_at, next_at)
		VALUES (2, 'gmail', TRUE, 'a@gmail.com', ?, ?, 1, 1)`
	if pgtest.DSN() != "" { // a superuser's row has no tenant of its own to take
		insert = `INSERT INTO removed_mailboxes (tenant_id, account_id, preset, oauth, username, secret_enc, dek_enc, created_at, next_at)
			VALUES (1, 2, 'gmail', TRUE, 'a@gmail.com', ?, ?, 1, 1)`
	}
	if _, err := db.ExecContext(ctx, insert, remSec, remDek); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UpgradeSecrets(ctx); err != nil || n != 4 {
		t.Fatalf("upgraded %d, %v; want 4", n, err)
	}
	if pw, err := s.AccountSecret(ctx, 1); err != nil || pw != "self-host app password" {
		t.Errorf("mailbox: %q, %v", pw, err)
	}
	if sec, err := s.DestinationSecret(ctx, 1); err != nil || sec.Secret != "whsec_self" {
		t.Errorf("destination: %+v, %v", sec, err)
	}
	k, err := s.TenantKey(ctx, SelfHostTenant)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Setting(ctx, SelfHostTenant, "key.anthropic_api_key")
	if err != nil {
		t.Fatal(err)
	}
	if key, err := OpenSetting(k, "anthropic_api_key", v); err != nil || key != "sk-ant-self" {
		t.Errorf("model key: %q, %v", key, err)
	}
	rs, err := s.WaitingRemovedMailboxes(ctx, 10, 10)
	if err != nil || len(rs) != 1 {
		t.Fatalf("removed = %+v, %v", rs, err)
	}
	if tok, err := s.RemovedMailboxSecret(ctx, rs[0].ID); err != nil || tok != "refresh-token-a" {
		t.Errorf("removed mailbox: %q, %v", tok, err)
	}
	if n, err := s.UpgradeSecrets(ctx); err != nil || n != 0 {
		t.Errorf("again: %d, %v", n, err)
	}
}

// With the old master key gone for good, the tenants' keys it wrapped are dropped, and a
// secret entered again is sealed under a new key the new master key opens.
func TestReplaceUnopenableTenantKeys(t *testing.T) {
	w := forgetWorld(t)
	ctx := t.Context()
	if n, err := w.s.ReplaceUnopenableTenantKeys(ctx); err != nil || n != 0 {
		t.Fatalf("with the right key: dropped %d, %v", n, err)
	}
	s := w.s.WithKeys(crypto.MasterKey(bytes.Repeat([]byte{8}, 32)))
	if err := s.SetAccountSecret(ctx, w.soloBox, "again"); !errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("before replacing: %v, want ErrDecrypt", err)
	}
	n, err := s.ReplaceUnopenableTenantKeys(ctx)
	if err != nil || n != 4 { // admin, solo, team, other
		t.Fatalf("dropped %d, %v; want 4", n, err)
	}
	if err := s.SetAccountSecret(ctx, w.soloBox, "again"); err != nil {
		t.Fatal(err)
	}
	if pw, err := s.AccountSecret(ctx, w.soloBox); err != nil || pw != "again" {
		t.Errorf("re-entered: %q, %v", pw, err)
	}
	// One not entered again went with the old key: deleted, to be entered again.
	if _, err := s.AccountSecret(ctx, w.otherBox); !errors.Is(err, ErrNoSecret) {
		t.Errorf("one not re-entered: %v, want ErrNoSecret", err)
	}
	if a, err := s.Account(ctx, w.otherBox); err != nil || !a.SecretGone {
		t.Errorf("one not re-entered: %+v, %v; want its secret gone", a, err)
	}
	if check, err := s.CheckSecrets(ctx); err != nil || check.Total != 4 || check.Readable() != 1 {
		t.Errorf("check = %+v, %v; want the re-entered one and the 3 placeholders", check, err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
