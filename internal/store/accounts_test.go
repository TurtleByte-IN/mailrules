package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store/migrations"
	"github.com/TurtleByte-IN/mailrules/internal/store/pgtest"
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
	a, err := s.CreateAccount(t.Context(), Account{
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
	if got.WatchFolder != "INBOX" || got.Status != "new" || got.LastError != "" || got.LastEventAt != 0 || got.CertFingerprint != "" ||
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
	if err := s.SetAccountCert(ctx, a.ID, "AA:BB"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Account(ctx, a.ID); got.CertFingerprint != "AA:BB" {
		t.Errorf("certificate not stored: %+v", got)
	}

	if _, err := s.DeleteAccount(ctx, a.ID, 400); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"get":    func() error { _, err := s.Account(ctx, a.ID); return err }(),
		"secret": func() error { _, err := s.AccountSecret(ctx, a.ID); return err }(),
		"status": s.SetAccountStatus(ctx, a.ID, "live", "", 1),
		"caps":   s.SetAccountCapabilities(ctx, a.ID, nil),
		"cert":   s.SetAccountCert(ctx, a.ID, ""),
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

// Removing a mailbox deletes its messages and their decisions row by row, and for each row
// deleted the database finds the rows that reference it. Every column that references a
// message or a decision is indexed, so that is a lookup rather than a read of the whole
// table: before, removing 10,000 messages held the write lock for 5.7 s, past the 5 s other
// writers wait, and the time grew with the square of the mail (MAI-214).
func TestReferencesToMessagesAreIndexed(t *testing.T) {
	_, db := open(t)
	q := `SELECT m.name || '.' || f."from", EXISTS (SELECT 1 FROM pragma_index_list(m.name) l, pragma_index_info(l.name) i
		       WHERE i.seqno = 0 AND i.name = f."from")
		 FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		 WHERE m.type = 'table' AND f."table" IN ('messages', 'decisions')`
	if pgtest.DSN() != "" { // the keys there are (tenant_id, id): the other column is the one to look up by
		q = `SELECT c.conrelid::regclass::text || '.' || a.attname::text, EXISTS (SELECT 1 FROM pg_index i
		       WHERE i.indrelid = c.conrelid AND i.indkey[0] = a.attnum)
		 FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey) AND a.attname <> 'tenant_id'
		 WHERE c.contype = 'f' AND c.confrelid IN ('messages'::regclass, 'decisions'::regclass)`
	}
	rows, err := db.QueryContext(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		var indexed bool
		if err := rows.Scan(&ref, &indexed); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
		if !indexed {
			t.Errorf("%s references a message or a decision and has no index", ref)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(refs, "actions.decision_id") {
		t.Fatalf("references found: %v; want actions.decision_id among them", refs)
	}
}

// A mailbox a module added keeps its mark through edits that leave its server alone, and
// loses it when it moves: the new server is the person's.
func TestAccountModuleMark(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	person := newAccount(t, s, "me@icloud.com", "pw")
	a, err := s.CreateAccount(ctx, Account{UserID: person.UserID, Label: "Bridge", Preset: "proton", Host: "127.0.0.1", Port: 1143,
		TLSMode: "starttls", Username: "me@proton.me", Module: true}, "bridge-pw")

	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Account(ctx, person.ID); got.Module {
		t.Errorf("a mailbox the person added is marked: %+v", got)
	}
	for _, tc := range []struct {
		name   string
		edit   func(*Account)
		module bool
	}{
		{"a new label keeps it", func(a *Account) { a.Label = "Proton" }, true},
		{"a new folder and status keep it", func(a *Account) { a.WatchFolder, a.Status = "Work", "live" }, true},
		{"a new port clears it", func(a *Account) { a.Port = 1144 }, false},
		{"moving back does not restore it", func(a *Account) { a.Port = 1143 }, false},
	} {
		got, _ := s.Account(ctx, a.ID)
		tc.edit(&got)
		if err := s.UpdateAccount(ctx, got); err != nil {
			t.Fatal(err)
		}
		if got, _ = s.Account(ctx, a.ID); got.Module != tc.module {
			t.Errorf("%s: module = %v", tc.name, got.Module)
		}
	}
	for _, edit := range []func(*Account){func(a *Account) { a.Host = "10.0.0.2" }, func(a *Account) { a.TLSMode = "implicit" }} {
		b, err := s.CreateAccount(ctx, Account{UserID: person.UserID, Label: "B", Preset: "proton", Host: "127.0.0.1", Port: 1143,
			TLSMode: "starttls", Username: "b@proton.me", Module: true}, "pw")

		if err != nil {
			t.Fatal(err)
		}
		edit(&b)
		if err := s.UpdateAccount(ctx, b); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Account(ctx, b.ID); got.Module {
			t.Errorf("a move to %s:%d %s kept the mark", got.Host, got.Port, got.TLSMode)
		}
		if _, err := s.DeleteAccount(ctx, b.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
}

// Every one-click mailbox there was before the module mark existed was added by a module.
func TestMigrationModuleMark(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 5)
	user := legacyUser(t, db)
	password := legacyAccount(t, db, user)
	res, err := db.ExecContext(ctx, `INSERT INTO accounts (user_id, label, preset, host, port, tls_mode, username, secret_enc, dek_enc, watch_folder, status, created_at)
		 VALUES (?, 'Work', 'gmail', 'imap.gmail.com', 993, 'implicit', 'me@gmail.com', x'', x'', 'INBOX', 'new', 1)`, user)
	if err != nil {
		t.Fatal(err)
	}
	oneClick, _ := res.LastInsertId()
	p, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.SQLite())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 15); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE accounts SET oauth = 1 WHERE id = ?`, oneClick); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := keyed(db)
	for id, want := range map[int64]bool{password: false, oneClick: true} {
		if a, err := s.Account(ctx, id); err != nil || a.Module != want {
			t.Errorf("account %d: module = %v, %v; want %v", id, a.Module, err, want)
		}
	}
}

// A mailbox a module's form added that had already stopped on a refused password waits
// for the person to reconnect it instead; every other mailbox keeps its status.
func TestMigrationModuleMailboxesReconnect(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 16)
	s := keyed(db)
	u := User{ID: legacyUser(t, db)}
	want := map[int64]string{}
	for i, c := range []struct {
		module bool
		status string
		want   string
	}{
		{true, "auth_failed", "reconnect_needed"},
		{false, "auth_failed", "auth_failed"},
		{true, "error", "error"},
	} {
		// Written by hand: the store seals a secret under its tenant's key, which 0025 adds.
		var id int64
		err := db.QueryRowContext(ctx, `INSERT INTO accounts (user_id, label, preset, host, port, tls_mode, username, secret_enc, dek_enc, watch_folder, status, created_at, module)
			VALUES (?, 'm', 'proton', '127.0.0.1', 1143, 'starttls', ?, x'00', x'00', 'INBOX', ?, 1, ?) RETURNING id`,
			u.ID, fmt.Sprintf("me%d@proton.test", i), c.status, c.module).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		want[id] = c.want
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for id, status := range want {
		if a, err := s.Account(ctx, id); err != nil || a.Status != status {
			t.Errorf("account %d: status %q, %v; want %q", id, a.Status, err, status)
		}
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
	sqliteOnly(t, "reads the SQLite database and WAL files on disk")
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
	s := keyed(db)
	const secretA, secretB = "abcd-efgh-ijkl-mnop", "qrst-uvwx-yzab-cdef"
	a := newAccount(t, s, "me@icloud.com", secretA)
	b := newAccount(t, s, "other@icloud.com", secretB)

	for id, want := range map[int64]string{a.ID: secretA, b.ID: secretB} {
		if got, err := s.AccountSecret(ctx, id); err != nil || got != want {
			t.Errorf("secret %d = %q, %v", id, got, err)
		}
	}
	if _, err := New(db).WithKeys(crypto.MasterKey(bytes.Repeat([]byte{8}, 32))).AccountSecret(ctx, a.ID); !errors.Is(err, crypto.ErrDecrypt) {
		t.Errorf("wrong master key: %v, want ErrDecrypt", err)
	}
	if _, err := New(db).AccountSecret(ctx, a.ID); !errors.Is(err, ErrNoKeys) {
		t.Errorf("no keys: %v, want ErrNoKeys", err)
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
	if _, err := s.AccountSecret(ctx, b.ID); !errors.Is(err, crypto.ErrDecrypt) {
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
