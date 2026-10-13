package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// Account is one mailbox the daemon watches. Its password is never part of this struct:
// read it with AccountSecret at the moment a connection is made.
type Account struct {
	ID           int64
	UserID       int64 // the owner: who added it
	TenantID     int64 // the owner's tenant
	Shared       bool  // everyone in the tenant sees it; otherwise only the owner
	Label        string
	Preset       string
	Host         string
	Port         int
	TLSMode      string
	Username     string
	WatchFolder  string
	Status       string // new | live | reconnecting | auth_failed | reconnect_needed | cert_changed | error | paused
	LastError    string
	LastEventAt  int64
	Capabilities []string
	CreatedAt    int64
	// CertFingerprint is the server certificate the person accepted, as mail.Cert has it:
	// the server must present exactly that one. Empty: the system's trust store decides.
	CertFingerprint string
	// OAuth marks a mailbox connected with one-click sign-in: the module that signs in to its
	// preset turns the stored secret (a refresh token) into an access token before every login.
	OAuth bool
	// Module marks a mailbox a module added (ext.Host.AddMailbox): its server is the
	// module's, so the hosted build connects to it even when it is not on the public
	// internet (publicnet). Moving the mailbox to another server clears it.
	Module bool
	// SecretGone: the mailbox's password or token was deleted (ForgetIdentity), so nothing
	// can log in to it and no supervisor is started for it until its owner enters a new one.
	SecretGone bool
}

const accountCols = `a.id, a.user_id, a.tenant_id, a.shared, a.label, a.preset, a.host, a.port, a.tls_mode, a.username, a.watch_folder, a.status,
	COALESCE(a.last_error, ''), COALESCE(a.last_event_at, 0), COALESCE(a.capabilities, '[]'), a.created_at, a.cert_fingerprint, a.oauth,
	a.module, length(a.secret_enc) = 0`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var caps string
	if err := row.Scan(&a.ID, &a.UserID, &a.TenantID, &a.Shared, &a.Label, &a.Preset, &a.Host, &a.Port, &a.TLSMode, &a.Username,
		&a.WatchFolder, &a.Status, &a.LastError, &a.LastEventAt, &caps, &a.CreatedAt, &a.CertFingerprint, &a.OAuth, &a.Module, &a.SecretGone); err != nil {
		return Account{}, err
	}
	if err := json.Unmarshal([]byte(caps), &a.Capabilities); err != nil {
		return Account{}, fmt.Errorf("decode capabilities: %w", err)
	}
	return a, nil
}

// OwnAddresses returns the account's own email addresses, lowercase: the username when it
// is a full address and, for a provider whose domains are known (presets.Preset.Domains),
// the name before "@" at each of them, so an iCloud username of only "jane" is
// jane@icloud.com, jane@me.com and jane@mac.com. Aliases are not known. It is nil when no
// address can be told: a username without "@" on a provider whose domains are not known.
func (a Account) OwnAddresses() []string {
	user := strings.ToLower(strings.TrimSpace(a.Username))
	local, domain, full := strings.Cut(user, "@")
	p, _ := presets.Get(a.Preset)
	switch {
	case local == "" || (full && domain == ""):
		return nil
	case full && !slices.Contains(p.Domains, domain):
		return []string{user}
	case len(p.Domains) == 0: // a name without "@", and no domain to put after it
		return nil
	}
	out := make([]string, 0, len(p.Domains))
	for _, d := range p.Domains {
		out = append(out, local+"@"+d)
	}
	return out
}

// FirstUser returns the admin account, or ErrNotFound before first-run setup. The command
// line acts as this user, in its tenant.
func (s *Store) FirstUser(ctx context.Context) (User, error) {
	ctx = s.tenantFor(ctx, ofFirstUser, nil)
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// CreateAccount stores an account with its secret sealed under its tenant's data key. The
// account goes into its owner's (a.UserID's) tenant and starts shared or not as
// NewAccountShared says; a.TenantID and a.Shared are ignored. The ciphertext is bound to
// the row id, which only exists after the insert, so the row is inserted, sealed and
// updated in one transaction: no reader ever sees it without a secret. Its tenant may have
// any number of mailboxes; CreateAccountWithin caps them.
func (s *Store) CreateAccount(ctx context.Context, a Account, secret string) (Account, error) {
	return s.CreateAccountWithin(ctx, a, secret, NoLimit)
}

// createAccount is CreateAccount's insert in tx.
func (s *Store) createAccount(ctx context.Context, tx *txn, a Account, secret string) (Account, error) {
	caps, err := json.Marshal(append([]string{}, a.Capabilities...))
	if err != nil {
		return Account{}, fmt.Errorf("encode capabilities: %w", err)
	}
	if a.WatchFolder == "" {
		a.WatchFolder = "INBOX"
	}
	if a.Status == "" {
		a.Status = "new"
	}
	a.Shared = NewAccountShared
	// The account is its owner's tenant's. The secret columns start empty, bound rather than
	// written as x'' (Postgres reads that as a bit string), and are filled once the id they
	// are bound to exists.
	err = tx.QueryRowContext(ctx, `SELECT tenant_id FROM users WHERE id = ?`, a.UserID).Scan(&a.TenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("create account: user %d: %w", a.UserID, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO accounts (user_id, tenant_id, shared, label, preset, host, port, tls_mode, username, secret_enc, dek_enc,
		                       watch_folder, status, capabilities, created_at, cert_fingerprint, oauth, module)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		a.UserID, a.TenantID, a.Shared, a.Label, a.Preset, a.Host, a.Port, a.TLSMode, a.Username, []byte{}, []byte{}, a.WatchFolder, a.Status,
		string(caps), a.CreatedAt, a.CertFingerprint, a.OAuth, a.Module).
		Scan(&a.ID); err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	key, err := s.tenantKey(ctx, tx, a.TenantID)
	if err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	secretEnc, dekEnc, err := key.Seal(secretAccount, strconv.FormatInt(a.ID, 10), []byte(secret))
	if err != nil {
		return Account{}, fmt.Errorf("encrypt account secret: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET secret_enc = ?, dek_enc = ? WHERE id = ?`, secretEnc, dekEnc, a.ID); err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	if err := record(ctx, tx, a.TenantID, a.CreatedAt, AuditMailboxAdded, a.Username, a.Label+", "+a.Host); err != nil {
		return Account{}, err
	}
	return a, nil
}

// Account returns one account, or ErrNotFound. It does not look at who asks: it is for
// internal work on a known account (the worker, the executor). A request reads
// VisibleAccount.
func (s *Store) Account(ctx context.Context, id int64) (Account, error) {
	ctx = s.tenantFor(ctx, ofAccount, id)
	a, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts a WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return a, nil
}

// VisibleAccount returns one account the viewer sees, or ErrNotFound: one they do not see
// is answered as missing.
func (s *Store) VisibleAccount(ctx context.Context, v Viewer, id int64) (Account, error) {
	ctx = WithTenant(ctx, v.TenantID)
	a, err := scanAccount(s.db.QueryRowContext(ctx,
		`SELECT `+accountCols+` FROM accounts a WHERE a.id = ? AND `+visibleAccounts("a"), id, v.tenantArg(), v.userArg()))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return a, nil
}

// Accounts lists every account of every tenant, oldest first: for the daemon's own work
// (starting the watchers, the master key check), never for a request.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	var out []Account
	err := s.eachTenant(ctx, tenantsAll, func(ctx context.Context, _ int64) error {
		as, err := s.listAccounts(ctx, `SELECT `+accountCols+` FROM accounts a ORDER BY a.id`)
		out = append(out, as...)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b Account) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// VisibleAccounts lists the accounts the viewer sees, oldest first.
func (s *Store) VisibleAccounts(ctx context.Context, v Viewer) ([]Account, error) {
	ctx = WithTenant(ctx, v.TenantID)
	return s.listAccounts(ctx, `SELECT `+accountCols+` FROM accounts a WHERE `+visibleAccounts("a")+` ORDER BY a.id`, v.tenantArg(), v.userArg())
}

// TenantAccounts lists every account of a tenant, private ones included, oldest first.
// It is for checks that span the tenant without showing what they find (a mailbox already
// connected), never for listing to a person.
func (s *Store) TenantAccounts(ctx context.Context, tenantID int64) ([]Account, error) {
	ctx = WithTenant(ctx, tenantID)
	return s.listAccounts(ctx, `SELECT `+accountCols+` FROM accounts a WHERE a.tenant_id = ? ORDER BY a.id`, tenantID)
}

func (s *Store) listAccounts(ctx context.Context, query string, args ...any) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return out, nil
}

// SetAccountShared shares an account with everyone in its tenant, or makes it private to
// its owner again. The caller checks that the owner asks.
func (s *Store) SetAccountShared(ctx context.Context, id int64, shared bool) error {
	return s.accountChange(ctx, id, "share account", func(ctx context.Context, tx *txn, a Account) error {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET shared = ? WHERE id = ?`, shared, id); err != nil {
			return fmt.Errorf("share account: %w", err)
		}
		if a.Shared == shared {
			return nil
		}
		how := "made private"
		if shared {
			how = "shared with the team"
		}
		return record(ctx, tx, a.TenantID, 0, AuditMailboxChanged, a.Username, how)
	})
}

// accountChange runs fn in one write transaction of the account's tenant, handing it the
// account as it was. ErrNotFound for an account that does not exist.
func (s *Store) accountChange(ctx context.Context, id int64, what string, fn func(ctx context.Context, tx *txn, a Account) error) error {
	ctx = s.tenantFor(ctx, ofAccount, id)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	a, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts a WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if err := fn(ctx, tx, a); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// ErrNoSecret is the answer for a mailbox whose secret was deleted (Account.SecretGone).
var ErrNoSecret = errors.New("the mailbox's password was deleted; enter it again")

// AccountSecret decrypts an account's app password or token. It returns crypto.ErrDecrypt
// when the key it was sealed under does not open, and ErrNoSecret when the secret was
// deleted.
func (s *Store) AccountSecret(ctx context.Context, id int64) (string, error) {
	ctx = s.tenantFor(ctx, ofAccount, id)
	var tenantID int64
	var secretEnc, dekEnc []byte
	err := s.db.QueryRowContext(ctx, `SELECT tenant_id, secret_enc, dek_enc FROM accounts WHERE id = ?`, id).Scan(&tenantID, &secretEnc, &dekEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get account secret: %w", err)
	}
	if len(secretEnc) == 0 {
		return "", ErrNoSecret
	}
	key, err := s.tenantKey(WithTenant(ctx, tenantID), s.db, tenantID)
	if err != nil {
		return "", fmt.Errorf("decrypt account secret: %w", err)
	}
	secret, err := key.Open(secretAccount, strconv.FormatInt(id, 10), id, secretEnc, dekEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt account secret: %w", err)
	}
	return string(secret), nil
}

// SetAccountStatus records the connection state; lastError is cleared by passing "".
func (s *Store) SetAccountStatus(ctx context.Context, id int64, status, lastError string, now int64) error {
	ctx = s.tenantFor(ctx, ofAccount, id)
	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET status = ?, last_error = NULLIF(?, ''), last_event_at = ? WHERE id = ?`, status, lastError, now, id)
	if err != nil {
		return fmt.Errorf("set account status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAccountCert stores the server certificate the person accepted for an account; ""
// leaves the decision to the system's trust store again.
func (s *Store) SetAccountCert(ctx context.Context, id int64, fingerprint string) error {
	ctx = s.tenantFor(ctx, ofAccount, id)
	res, err := s.db.ExecContext(ctx, `UPDATE accounts SET cert_fingerprint = ? WHERE id = ?`, fingerprint, id)
	if err != nil {
		return fmt.Errorf("set account certificate: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAccountCapabilities stores what the server advertised at the last login.
func (s *Store) SetAccountCapabilities(ctx context.Context, id int64, capabilities []string) error {
	ctx = s.tenantFor(ctx, ofAccount, id)
	caps, err := json.Marshal(append([]string{}, capabilities...))
	if err != nil {
		return fmt.Errorf("encode capabilities: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE accounts SET capabilities = ? WHERE id = ?`, string(caps), id)
	if err != nil {
		return fmt.Errorf("set account capabilities: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAccount wipes an account with its secret, folders, contacts, messages, decisions,
// actions and corrections: each references the account with ON DELETE CASCADE. The rules
// are kept: first, in the same transaction, every user's rules that name the mailbox are
// rewritten as rules.Rule.DropAccount has it, so a rule limited to it is switched off and
// marked, with no mailbox, before the cascade could reach it. It returns those rules. A
// mailbox a module connected is queued for that module (queueRemovedMailboxes).
func (s *Store) DeleteAccount(ctx context.Context, id, now int64) ([]rules.Rule, error) {
	ctx = s.tenantFor(ctx, ofAccount, id)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	var tenantID int64
	var username string
	err = tx.QueryRowContext(ctx, `SELECT tenant_id, username FROM accounts WHERE id = ?`, id).Scan(&tenantID, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}
	changed, err := dropMailboxes(ctx, tx, func(named int64) bool { return named == id }, now)
	if err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}
	if err := s.queueRemovedMailboxes(ctx, tx, []int64{id}, now); err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}
	if err := record(ctx, tx, tenantID, now, AuditMailboxRemoved, username, ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("delete account: %w", err)
	}
	return changed, nil
}

// Folder is one folder of an account, with the watch position when it is watched.
type Folder struct {
	AccountID   int64
	Name        string
	Delimiter   string
	SpecialUse  string // \Junk \Trash \Archive \Sent \Drafts or empty
	UIDValidity uint32
	LastUID     uint32 // highest UID processed
}

// SaveFolders replaces an account's folder list with what discovery found. Watch
// positions of folders that still exist are kept; folders that are gone are removed.
func (s *Store) SaveFolders(ctx context.Context, accountID int64, folders []Folder) error {
	ctx = s.tenantFor(ctx, ofAccount, accountID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save folders: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	names := make([]any, 0, len(folders))
	for _, f := range folders {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO folders (account_id, name, delimiter, special_use) VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''))
			 ON CONFLICT (account_id, name) DO UPDATE SET delimiter = excluded.delimiter, special_use = excluded.special_use`,
			accountID, f.Name, f.Delimiter, f.SpecialUse); err != nil {
			return fmt.Errorf("save folders: %w", err)
		}
		names = append(names, f.Name)
	}
	enc, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("save folders: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM folders WHERE account_id = ? AND name NOT IN (`+s.db.d.jsonValues("?", "TEXT")+`)`, accountID, string(enc)); err != nil {
		return fmt.Errorf("save folders: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save folders: %w", err)
	}
	return nil
}

// AddFolder records a folder MailRules itself just created on the server, so it shows up
// before the next folder discovery. A folder already on record is left as it is.
func (s *Store) AddFolder(ctx context.Context, accountID int64, name string) error {
	ctx = s.tenantFor(ctx, ofAccount, accountID)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO folders (account_id, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, accountID, name); err != nil {
		return fmt.Errorf("add folder: %w", err)
	}
	return nil
}

const folderCols = `account_id, name, COALESCE(delimiter, ''), COALESCE(special_use, ''), COALESCE(uidvalidity, 0), last_uid`

func scanFolder(row interface{ Scan(...any) error }) (Folder, error) {
	var f Folder
	err := row.Scan(&f.AccountID, &f.Name, &f.Delimiter, &f.SpecialUse, &f.UIDValidity, &f.LastUID)
	return f, err
}

// Folders lists an account's folders by name.
func (s *Store) Folders(ctx context.Context, accountID int64) ([]Folder, error) {
	ctx = s.tenantFor(ctx, ofAccount, accountID)
	rows, err := s.db.QueryContext(ctx, `SELECT `+folderCols+` FROM folders WHERE account_id = ? ORDER BY name`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, fmt.Errorf("list folders: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	return out, nil
}

// Folder returns one folder, or ErrNotFound.
func (s *Store) Folder(ctx context.Context, accountID int64, name string) (Folder, error) {
	ctx = s.tenantFor(ctx, ofAccount, accountID)
	f, err := scanFolder(s.db.QueryRowContext(ctx, `SELECT `+folderCols+` FROM folders WHERE account_id = ? AND name = ?`, accountID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Folder{}, ErrNotFound
	}
	if err != nil {
		return Folder{}, fmt.Errorf("get folder: %w", err)
	}
	return f, nil
}

// SetFolderPosition records how far a watched folder has been processed.
func (s *Store) SetFolderPosition(ctx context.Context, accountID int64, name string, uidValidity, lastUID uint32) error {
	ctx = s.tenantFor(ctx, ofAccount, accountID)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO folders (account_id, name, uidvalidity, last_uid) VALUES (?, ?, ?, ?)
		 ON CONFLICT (account_id, name) DO UPDATE SET uidvalidity = excluded.uidvalidity, last_uid = excluded.last_uid`,
		accountID, name, uidValidity, lastUID); err != nil {
		return fmt.Errorf("set folder position: %w", err)
	}
	return nil
}

// Contact is an address the account's owner has sent mail to.
type Contact struct {
	Address    string // lower-case
	LastSentAt int64
}

// UpsertContacts adds addresses to an account's contacts, keeping the latest sent time.
func (s *Store) UpsertContacts(ctx context.Context, accountID int64, contacts []Contact) error {
	ctx = s.tenantFor(ctx, ofAccount, accountID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upsert contacts: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	for _, c := range contacts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO contacts (account_id, address, last_sent_at) VALUES (?, ?, ?)
			 ON CONFLICT (account_id, address) DO UPDATE
			 SET last_sent_at = `+s.db.d.greatest("COALESCE(contacts.last_sent_at, 0)", "excluded.last_sent_at"),
			accountID, c.Address, c.LastSentAt); err != nil {
			return fmt.Errorf("upsert contacts: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upsert contacts: %w", err)
	}
	return nil
}

// IsContact reports whether the account's owner has sent mail to address.
func (s *Store) IsContact(ctx context.Context, accountID int64, address string) (bool, error) {
	ctx = s.tenantFor(ctx, ofAccount, accountID)
	var ok bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM contacts WHERE account_id = ? AND lower(address) = lower(?))`, accountID, address).Scan(&ok); err != nil {
		return false, fmt.Errorf("look up contact: %w", err)
	}
	return ok, nil
}

// UpdateAccount stores an account's label, watch folder, server (host, port and TLS mode)
// and status (the fields the user edits). A new server clears Module: the server is then
// the person's, not the module's.
func (s *Store) UpdateAccount(ctx context.Context, a Account) error {
	return s.accountChange(ctx, a.ID, "update account", func(ctx context.Context, tx *txn, old Account) error {
		// The right-hand sides read the row as it was, so module stays only on the same server.
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET label = ?, watch_folder = ?,
			module = (module AND host = ? AND port = ? AND tls_mode = ?), host = ?, port = ?, tls_mode = ?, status = ? WHERE id = ?`,
			a.Label, a.WatchFolder, a.Host, a.Port, a.TLSMode, a.Host, a.Port, a.TLSMode, a.Status, a.ID); err != nil {
			return fmt.Errorf("update account: %w", err)
		}
		var how []string
		if old.Label != a.Label {
			how = append(how, "renamed from "+old.Label)
		}
		if old.WatchFolder != a.WatchFolder {
			how = append(how, "watches "+a.WatchFolder)
		}
		if old.Host != a.Host || old.Port != a.Port || old.TLSMode != a.TLSMode {
			how = append(how, "server "+a.Host)
		}
		switch {
		case old.Status != "paused" && a.Status == "paused":
			how = append(how, "paused")
		case old.Status == "paused" && a.Status != "paused":
			how = append(how, "resumed")
		}
		if len(how) == 0 {
			return nil
		}
		return record(ctx, tx, old.TenantID, 0, AuditMailboxChanged, old.Username, strings.Join(how, ", "))
	})
}

// SetAccountSecret replaces an account's app password, sealed under its tenant's key. The
// change is recorded under ctx's actor: one rotated by a provider is made WithoutActor.
func (s *Store) SetAccountSecret(ctx context.Context, id int64, secret string) error {
	return s.accountChange(ctx, id, "set account secret", func(ctx context.Context, tx *txn, a Account) error {
		key, err := s.tenantKey(ctx, tx, a.TenantID)
		if err != nil {
			return fmt.Errorf("set account secret: %w", err)
		}
		secretEnc, dekEnc, err := key.Seal(secretAccount, strconv.FormatInt(id, 10), []byte(secret))
		if err != nil {
			return fmt.Errorf("encrypt account secret: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET secret_enc = ?, dek_enc = ? WHERE id = ?`, secretEnc, dekEnc, id); err != nil {
			return fmt.Errorf("set account secret: %w", err)
		}
		return record(ctx, tx, a.TenantID, 0, AuditMailboxChanged, a.Username, "new password or sign-in")
	})
}

// LastMailAt is when the newest email MailRules has seen in an account arrived (its
// received date, or when it was first seen if it carried none); 0 when it has seen none.
func (s *Store) LastMailAt(ctx context.Context, accountID int64) (int64, error) {
	ctx = s.tenantFor(ctx, ofAccount, accountID)
	var at sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(COALESCE(received_at, created_at)) FROM messages WHERE account_id = ?`, accountID).Scan(&at); err != nil {
		return 0, fmt.Errorf("last mail of account %d: %w", accountID, err)
	}
	return at.Int64, nil
}
