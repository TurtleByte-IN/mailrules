package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
)

var testMaster = bytes.Repeat([]byte{7}, 32)

func newAccount(t *testing.T, s *Store, username, secret string) Account {
	t.Helper()
	u, err := s.FirstUser(t.Context())
	if errors.Is(err, ErrNotFound) {
		u, err = s.CreateFirstUser(t.Context(), "me@icloud.com", "hash", 100)
	}
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAccount(t.Context(), testMaster, Account{
		UserID: u.ID, Label: "Personal", Preset: "icloud", Host: "imap.mail.me.com", Port: 993, TLSMode: "implicit",
		Username: username, Capabilities: []string{"IDLE", "MOVE"}, CreatedAt: 200,
	}, secret)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAccountsCRUD(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	if _, err := s.FirstUser(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FirstUser before setup: %v", err)
	}
	if list, err := s.Accounts(ctx); err != nil || len(list) != 0 {
		t.Fatalf("fresh db accounts = %v, %v", list, err)
	}
	a := newAccount(t, s, "me@icloud.com", "abcd-efgh-ijkl-mnop")
	b := newAccount(t, s, "other@icloud.com", "qrst-uvwx-yzab-cdef")

	got, err := s.Account(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Defaults come from the schema; everything else is what was stored.
	if got.WatchFolder != "INBOX" || got.Status != "new" || got.LastError != "" || got.LastEventAt != 0 ||
		got.Username != "me@icloud.com" || got.Port != 993 || !slices.Equal(got.Capabilities, []string{"IDLE", "MOVE"}) || got.ID != a.ID {
		t.Errorf("account = %+v", got)
	}
	if list, err := s.Accounts(ctx); err != nil || len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Errorf("accounts = %+v, %v", list, err)
	}

	if err := s.SetAccountStatus(ctx, a.ID, "auth_failed", "login refused", 300); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountCapabilities(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Account(ctx, a.ID); got.Status != "auth_failed" || got.LastError != "login refused" || got.LastEventAt != 300 || len(got.Capabilities) != 0 {
		t.Errorf("after status change = %+v", got)
	}
	if err := s.SetAccountStatus(ctx, a.ID, "live", "", 301); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Account(ctx, a.ID); got.Status != "live" || got.LastError != "" {
		t.Errorf("error not cleared: %+v", got)
	}

	if _, err := s.DeleteAccount(ctx, a.ID, 400); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"get":    func() error { _, err := s.Account(ctx, a.ID); return err }(),
		"secret": func() error { _, err := s.AccountSecret(ctx, testMaster, a.ID); return err }(),
		"status": s.SetAccountStatus(ctx, a.ID, "live", "", 1),
		"caps":   s.SetAccountCapabilities(ctx, a.ID, nil),
		"delete": func() error { _, err := s.DeleteAccount(ctx, a.ID, 401); return err }(),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on a deleted account: %v, want ErrNotFound", name, err)
		}
	}
	if _, err := s.Account(ctx, b.ID); err != nil {
		t.Errorf("deleting one account removed another: %v", err)
	}
}

func TestOwnAddresses(t *testing.T) {
	icloud := []string{"jane@icloud.com", "jane@me.com", "jane@mac.com"}
	for _, tt := range []struct {
		name, preset, username string
		want                   []string
	}{
		{"a full address", "generic", "jane@example.test", []string{"jane@example.test"}},
		{"a full address, any case", "fastmail", " Jane@Example.TEST ", []string{"jane@example.test"}},
		{"no domain, none known", "generic", "jane", nil},
		{"an unknown preset", "", "jane@example.test", []string{"jane@example.test"}},
		{"iCloud, the name only", "icloud", "Jane", icloud},
		{"iCloud, the full address", "icloud", "jane@icloud.com", icloud},
		{"iCloud, a me.com address", "icloud", "jane@me.com", icloud},
		{"iCloud with a custom domain", "icloud", "jane@family.example", []string{"jane@family.example"}},
		{"nothing before @", "icloud", "@icloud.com", nil},
		{"nothing after @", "generic", "jane@", nil},
		{"empty", "icloud", "", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Account{Preset: tt.preset, Username: tt.username}).OwnAddresses(); !slices.Equal(got, tt.want) {
				t.Errorf("OwnAddresses() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAccountSecretIsEncrypted(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	const secretA, secretB = "abcd-efgh-ijkl-mnop", "qrst-uvwx-yzab-cdef"
	a := newAccount(t, s, "me@icloud.com", secretA)
	b := newAccount(t, s, "other@icloud.com", secretB)

	for id, want := range map[int64]string{a.ID: secretA, b.ID: secretB} {
		if got, err := s.AccountSecret(ctx, testMaster, id); err != nil || got != want {
			t.Errorf("secret %d = %q, %v", id, got, err)
		}
	}
	if _, err := s.AccountSecret(ctx, bytes.Repeat([]byte{8}, 32), a.ID); !errors.Is(err, crypto.ErrDecrypt) {
		t.Errorf("wrong master key: %v, want ErrDecrypt", err)
	}

	// Nothing on disk holds the plaintext: not the database, not its write-ahead log.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(FULL)`); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) == 0 {
		t.Fatal("no database files found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{secretA, secretB} {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s contains a plaintext secret", filepath.Base(f))
			}
		}
	}
	var enc, dek []byte
	if err := db.QueryRowContext(ctx, `SELECT secret_enc, dek_enc FROM accounts WHERE id = ?`, a.ID).Scan(&enc, &dek); err != nil {
		t.Fatal(err)
	}
	if len(enc) <= len(secretA) || len(dek) == 0 {
		t.Errorf("ciphertext lengths %d and %d", len(enc), len(dek))
	}

	// A ciphertext copied onto another row does not decrypt there: it is bound to its row id.
	if _, err := db.ExecContext(ctx, `UPDATE accounts SET secret_enc = ?, dek_enc = ? WHERE id = ?`, enc, dek, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccountSecret(ctx, testMaster, b.ID); !errors.Is(err, crypto.ErrDecrypt) {
		t.Errorf("swapped ciphertext: %v, want ErrDecrypt", err)
	}
}

func TestFolders(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	a := newAccount(t, s, "me@icloud.com", "pw")
	if _, err := s.Folder(ctx, a.ID, "INBOX"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing folder: %v", err)
	}
	first := []Folder{{Name: "INBOX", Delimiter: "/"}, {Name: "Junk", Delimiter: "/", SpecialUse: `\Junk`}, {Name: "Old"}}
	if err := s.SaveFolders(ctx, a.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFolderPosition(ctx, a.ID, "INBOX", 11, 500); err != nil {
		t.Fatal(err)
	}
	// Discovery runs again: "Old" is gone, "Sent" is new, INBOX keeps its position.
	second := []Folder{{Name: "INBOX", Delimiter: "."}, {Name: "Junk", Delimiter: "."}, {Name: "Sent", Delimiter: ".", SpecialUse: `\Sent`}}
	if err := s.SaveFolders(ctx, a.ID, second); err != nil {
		t.Fatal(err)
	}
	got, err := s.Folders(ctx, a.ID)
	want := []Folder{
		{AccountID: a.ID, Name: "INBOX", Delimiter: ".", UIDValidity: 11, LastUID: 500},
		{AccountID: a.ID, Name: "Junk", Delimiter: "."},
		{AccountID: a.ID, Name: "Sent", Delimiter: ".", SpecialUse: `\Sent`},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("folders = %+v, %v\nwant %+v", got, err, want)
	}
	if err := s.SetFolderPosition(ctx, a.ID, "INBOX", 12, 3); err != nil {
		t.Fatal(err)
	}
	if f, err := s.Folder(ctx, a.ID, "INBOX"); err != nil || f.UIDValidity != 12 || f.LastUID != 3 || f.Delimiter != "." {
		t.Errorf("folder = %+v, %v", f, err)
	}
	// A watch position can be recorded before discovery has listed the folder.
	if err := s.SetFolderPosition(ctx, a.ID, "Later", 1, 9); err != nil {
		t.Fatal(err)
	}
	if f, err := s.Folder(ctx, a.ID, "Later"); err != nil || f.LastUID != 9 {
		t.Errorf("folder = %+v, %v", f, err)
	}
	if err := s.SaveFolders(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Folders(ctx, a.ID); len(got) != 0 {
		t.Errorf("folders after an empty discovery = %+v", got)
	}
}

func TestContacts(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	a := newAccount(t, s, "me@icloud.com", "pw")
	b := newAccount(t, s, "other@icloud.com", "pw")
	if err := s.UpsertContacts(ctx, a.ID, []Contact{{"ann@example.org", 100}, {"bob@example.org", 200}}); err != nil {
		t.Fatal(err)
	}
	// A later scan must not move a sent time backwards.
	if err := s.UpsertContacts(ctx, a.ID, []Contact{{"ann@example.org", 50}, {"bob@example.org", 300}}); err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]int64{"ann@example.org": 100, "bob@example.org": 300} {
		var got int64
		if err := db.QueryRowContext(ctx, `SELECT last_sent_at FROM contacts WHERE account_id = ? AND address = ?`, a.ID, addr).Scan(&got); err != nil || got != want {
			t.Errorf("%s last_sent_at = %d, %v, want %d", addr, got, err, want)
		}
	}
	for _, tc := range []struct {
		account int64
		addr    string
		want    bool
	}{
		{a.ID, "ann@example.org", true},
		{a.ID, "ANN@Example.org", true},
		{a.ID, "stranger@example.org", false},
		{b.ID, "ann@example.org", false}, // contacts are per account
	} {
		if got, err := s.IsContact(ctx, tc.account, tc.addr); err != nil || got != tc.want {
			t.Errorf("IsContact(%d, %s) = %v, %v", tc.account, tc.addr, got, err)
		}
	}
	// Deleting the account takes its folders and contacts with it.
	if err := s.SaveFolders(ctx, a.ID, []Folder{{Name: "INBOX"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAccount(ctx, a.ID, 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM contacts) + (SELECT COUNT(*) FROM folders)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows left behind = %d, %v", n, err)
	}
}
