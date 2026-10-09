package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

type presetJSON struct {
	Name           string `json:"name"`
	Label          string `json:"label"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	TLSMode        string `json:"tls_mode"`
	HelpURL        string `json:"help_url"`
	LocalPartLogin bool   `json:"local_part_login"`
	PasteLabel     string `json:"secret_label"` // what the provider calls the secret the user pastes
}

// accountJSON is an account as the API shows it. The password is not a field of
// store.Account either, so there is nothing here to leak.
type accountJSON struct {
	ID           int64    `json:"id"`
	Label        string   `json:"label"`
	Preset       string   `json:"preset"`
	Host         string   `json:"host"`
	Port         int      `json:"port"`
	TLSMode      string   `json:"tls_mode"`
	Username     string   `json:"username"`
	WatchFolder  string   `json:"watch_folder"`
	Status       string   `json:"status"`
	LastError    string   `json:"last_error"`
	LastEventAt  *int64   `json:"last_event_at"` // when the status last changed
	LastMailAt   *int64   `json:"last_mail_at"`  // when the newest email seen arrived
	Capabilities []string `json:"capabilities"`
	CanMove      bool     `json:"can_move"`
	FolderCount  int      `json:"folder_count"`
	CreatedAt    int64    `json:"created_at"`
	Shared       bool     `json:"shared"` // everyone in the tenant sees it
	Mine         bool     `json:"mine"`   // the viewer owns it, so may manage it
}

type folderJSON struct {
	Name       string `json:"name"`
	Delimiter  string `json:"delimiter"`
	SpecialUse string `json:"special_use"`
}

// accountJSON shapes an account as v sees it.
func (s *server) accountJSON(ctx context.Context, v store.Viewer, a store.Account) accountJSON {
	out := accountJSON{ID: a.ID, Label: a.Label, Preset: a.Preset, Host: a.Host, Port: a.Port, TLSMode: a.TLSMode,
		Username: a.Username, WatchFolder: a.WatchFolder, Status: a.Status, LastError: a.LastError,
		LastEventAt: ts(a.LastEventAt), Capabilities: append([]string{}, a.Capabilities...), CreatedAt: a.CreatedAt,
		Shared: a.Shared, Mine: a.UserID == v.UserID}
	for _, c := range a.Capabilities {
		out.CanMove = out.CanMove || c == "MOVE" || c == "UIDPLUS"
	}
	if folders, err := s.store.Folders(ctx, a.ID); err == nil {
		out.FolderCount = len(folders)
	}
	if at, err := s.store.LastMailAt(ctx, a.ID); err == nil {
		out.LastMailAt = ts(at)
	}
	return out
}

func foldersJSON(folders []store.Folder) []folderJSON {
	out := make([]folderJSON, len(folders))
	for i, f := range folders {
		out[i] = folderJSON{f.Name, f.Delimiter, f.SpecialUse}
	}
	return out
}

func (s *server) handlePresets(w http.ResponseWriter, _ *http.Request) {
	var out []presetJSON
	for _, p := range presets.All() {
		out = append(out, presetJSON{p.Name, p.Label, p.Host, p.Port, p.TLSMode, p.HelpURL, p.LocalPartLogin, p.PasteLabel})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

type accountInput struct {
	Preset      string `json:"preset"`
	Label       string `json:"label"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	TLSMode     string `json:"tls_mode"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	WatchFolder string `json:"watch_folder"`
}

// account turns the wizard's form into an account, with the preset's defaults filled in.
func (in accountInput) account(w http.ResponseWriter) (store.Account, bool) {
	preset, ok := presets.Get(in.Preset)
	if !ok {
		invalid(w, "preset", "Choose a provider: icloud, gmail, fastmail, yahoo, zoho or generic.")
		return store.Account{}, false
	}
	a := store.Account{Label: strings.TrimSpace(in.Label), Preset: preset.Name, Host: preset.Host, Port: preset.Port,
		TLSMode: preset.TLSMode, Username: strings.TrimSpace(in.Username), WatchFolder: strings.TrimSpace(in.WatchFolder)}
	if h := strings.TrimSpace(in.Host); h != "" {
		a.Host = h
	}
	if in.Port != 0 {
		a.Port = in.Port
	}
	if in.TLSMode != "" {
		a.TLSMode = in.TLSMode
	}
	if a.WatchFolder == "" {
		a.WatchFolder = "INBOX"
	}
	if a.Label == "" {
		a.Label = a.Username
	}
	switch {
	case a.Username == "":
		invalid(w, "username", "Enter the email address or username you sign in with.")
	case in.Password == "":
		invalid(w, "password", "Enter the app password.")
	case a.Host == "":
		invalid(w, "host", "Enter the IMAP server's host name.")
	case a.Port < 1 || a.Port > 65535:
		invalid(w, "port", "The port must be between 1 and 65535.")
	case a.TLSMode != presets.TLSImplicit && a.TLSMode != presets.TLSStartTLS:
		invalid(w, "tls_mode", "The TLS mode is implicit or starttls.")
	default:
		return a, true
	}
	return store.Account{}, false
}

// connected is what a successful login found.
type connected struct {
	account store.Account // with the username that worked and the server's capabilities
	folders []store.Folder
	status  mail.FolderStatus // of the watch folder
	caps    mail.Caps
}

// tryAccount logs in with credentials that are not stored yet and reads what first
// connect needs. On failure it has answered already. The password is used for the login
// and nothing else; the error text sent to the browser is ours, not the server's.
func (s *server) tryAccount(w http.ResponseWriter, r *http.Request, a store.Account, password string) (connected, bool) {
	refuse := func(code, message, path string) (connected, bool) {
		writeError(w, http.StatusUnprocessableEntity, code, message, path)
		return connected{}, false
	}
	mb, username, err := s.Connect(r.Context(), a, password)
	if err == nil {
		defer mb.Close()
		a.Username = username
		c := connected{account: a, caps: mb.Capabilities()}
		c.account.Capabilities = c.caps.All
		var folders []mail.Folder
		if folders, err = mb.Folders(r.Context()); err == nil {
			for _, f := range folders {
				c.folders = append(c.folders, store.Folder{Name: f.Name, Delimiter: f.Delimiter, SpecialUse: f.SpecialUse})
			}
			if c.status, err = mb.Status(r.Context(), a.WatchFolder); err == nil {
				return c, true
			}
		}
	}
	slog.InfoContext(r.Context(), "account connection test failed", "host", a.Host, "error", err.Error())
	switch {
	case errors.Is(err, mail.ErrAuth):
		msg := "The mail server refused the sign-in. Check the username and use an app password, not your account password."
		if p, _ := presets.Get(a.Preset); p.HelpURL != "" {
			msg += " Create one here: " + p.HelpURL
		}
		return refuse("auth_failed", msg, "password")
	case errors.Is(err, mail.ErrTLS):
		return refuse("tls_failed", "The secure connection to "+a.Host+" could not be set up. Check the host, port and TLS mode.", "host")
	case errors.Is(err, mail.ErrNoFolder):
		return refuse("no_folder", "The server has no folder named "+a.WatchFolder+".", "watch_folder")
	}
	return refuse("connection_failed", "Could not reach "+a.Host+":"+strconv.Itoa(a.Port)+". Check the host and port.", "host")
}

func (s *server) handleAccountTest(w http.ResponseWriter, r *http.Request) {
	var in accountInput
	if !readJSON(w, r, &in) {
		return
	}
	a, ok := in.account(w)
	if !ok {
		return
	}
	c, ok := s.tryAccount(w, r, a, in.Password)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": c.account.Username, "folders": foldersJSON(c.folders), "can_move": c.caps.CanMove(), "idle": c.caps.Idle,
	})
}

// handleStoredAccountTest checks a connected account's stored login on a connection of its
// own, opened for this and closed again. The account's watcher is not touched: its
// connection, its status and what it is doing stay as they are. It uses the owner's stored
// password, so only the owner may.
func (s *server) handleStoredAccountTest(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ownAccount(w, r)
	if !ok {
		return
	}
	secret, err := s.store.AccountSecret(r.Context(), s.Master, a.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	c, ok := s.tryAccount(w, r, a, secret)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": c.account.Username, "folders": foldersJSON(c.folders), "can_move": c.caps.CanMove(), "idle": c.caps.Idle,
	})
}

func (s *server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	accounts, err := s.store.VisibleAccounts(r.Context(), v)
	if err != nil {
		internalError(w, r, err)
		return
	}
	out := make([]accountJSON, len(accounts))
	for i, a := range accounts {
		out[i] = s.accountJSON(r.Context(), v, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleAccountCreate checks the account against its server before anything is stored,
// records what first connect found (folders, and the watch position: only mail that
// arrives from now on is sorted), and starts watching.
func (s *server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in accountInput
	if !readJSON(w, r, &in) {
		return
	}
	a, ok := in.account(w)
	if !ok {
		return
	}
	c, ok := s.tryAccount(w, r, a, in.Password)
	if !ok {
		return
	}
	// A mailbox connected twice in one tenant would be sorted twice by the same rules, so
	// the check spans the tenant, teammates' private mailboxes included. What it finds is
	// only ever this refusal, never the other mailbox.
	existing, err := s.store.TenantAccounts(ctx, viewer(r).TenantID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	for _, e := range existing {
		if strings.EqualFold(e.Host, c.account.Host) && strings.EqualFold(e.Username, c.account.Username) {
			writeError(w, http.StatusConflict, "account_exists", "This mailbox is already connected.", "username")
			return
		}
	}
	c.account.UserID, c.account.CreatedAt = user(r).ID, s.now().Unix()
	acct, err := s.store.CreateAccount(ctx, s.Master, c.account, in.Password)
	if err == nil {
		err = s.store.SaveFolders(ctx, acct.ID, c.folders)
	}
	if err == nil {
		err = s.store.SetFolderPosition(ctx, acct.ID, acct.WatchFolder, c.status.UIDValidity, c.status.UIDNext-1)
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.StartAccount(acct)
	writeJSON(w, http.StatusCreated, map[string]any{"account": s.accountJSON(ctx, viewer(r), acct)})
}

func (s *server) handleAccount(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.visibleAccount(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"account": s.accountJSON(r.Context(), viewer(r), a)})
	}
}

// handleAccountPatch edits the label, the watched folder, the app password, whether the
// account is shared with the tenant, or pauses and resumes the account. Only the owner may:
// to anyone else the account answers as missing. A change of folder, password or pause
// restarts the account's supervisor, so the change is in force when the response arrives.
func (s *server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, ok := s.ownAccount(w, r)
	if !ok {
		return
	}
	var password string
	paused, shared := a.Status == worker.StatusPaused, a.Shared
	wasPaused, oldFolder := paused, a.WatchFolder
	sent, ok := readPatch(w, r, map[string]any{"label": &a.Label, "watch_folder": &a.WatchFolder, "password": &password,
		"paused": &paused, "shared": &shared})
	if !ok {
		return
	}
	a.Label, a.WatchFolder = strings.TrimSpace(a.Label), strings.TrimSpace(a.WatchFolder)
	switch {
	case a.Label == "":
		invalid(w, "label", "The label cannot be empty.")
		return
	case a.WatchFolder == "":
		invalid(w, "watch_folder", "Name the folder to watch.")
		return
	case sent["password"] && password == "":
		invalid(w, "password", "Enter the app password.")
		return
	}
	restart := a.WatchFolder != oldFolder || sent["password"] || paused != wasPaused
	if restart {
		s.StopAccount(a.ID) // before the row changes, so the old supervisor cannot write over it
	}
	switch {
	case paused:
		a.Status = worker.StatusPaused
	case wasPaused:
		a.Status = "new" // until the supervisor reports
	}
	err := s.store.UpdateAccount(ctx, a)
	if err == nil && sent["password"] {
		err = s.store.SetAccountSecret(ctx, s.Master, a.ID, password)
	}
	if err == nil && shared != a.Shared {
		if err = s.store.SetAccountShared(ctx, a.ID, shared); err == nil {
			a.Shared = shared
		}
	}
	if err != nil {
		fail(w, r, err, "account")
		return
	}
	if restart && !paused {
		s.StartAccount(a)
	}
	s.publish(r, a.ID, events.AccountStatus, a)
	writeJSON(w, http.StatusOK, map[string]any{"account": s.accountJSON(ctx, viewer(r), a)})
}

// handleAccountDelete removes a mailbox and everything MailRules kept of it. Only its
// owner may.
func (s *server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ownAccount(w, r)
	if !ok {
		return
	}
	s.StopAccount(a.ID)
	changed, err := s.store.DeleteAccount(r.Context(), a.ID, s.now().Unix())
	if err != nil {
		fail(w, r, err, "account")
		return
	}
	for _, rule := range changed {
		slog.InfoContext(r.Context(), "a rule was changed as its mailbox was removed",
			"rule_id", rule.ID, "rule", rule.Name, "switched_off", rule.MailboxRemoved)
	}
	if len(changed) > 0 {
		s.publish(r, 0, events.RulesChanged, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAccountReconnect drops the account's connection and connects again, which is also
// how an account that stopped on wrong credentials or a TLS failure is started again. Only
// its owner may.
func (s *server) handleAccountReconnect(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ownAccount(w, r)
	if !ok {
		return
	}
	if a.Status == worker.StatusPaused {
		writeError(w, http.StatusConflict, "account_paused", "This account is paused. Resume it first.", "")
		return
	}
	s.StartAccount(a)
	writeJSON(w, http.StatusOK, map[string]any{"account": s.accountJSON(r.Context(), viewer(r), a)})
}

func (s *server) handleAccountFolders(w http.ResponseWriter, r *http.Request) {
	a, ok := s.visibleAccount(w, r)
	if !ok {
		return
	}
	folders, err := s.store.Folders(r.Context(), a.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": foldersJSON(folders)})
}
