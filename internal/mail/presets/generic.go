// Package presets holds per-provider connection defaults: host, port, TLS mode,
// folder-name fallbacks for servers without SPECIAL-USE, and known quirks.
package presets

import (
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// TLS modes, as stored in accounts.tls_mode.
const (
	TLSImplicit = "implicit"
	TLSStartTLS = "starttls"
)

// Preset is one provider's defaults.
type Preset struct {
	Name    string // stored in accounts.preset
	Label   string
	Host    string // empty for generic: the user supplies it
	Port    int
	TLSMode string
	// Folders maps a special-use role to folder names to look for, most likely first,
	// when the server does not report the role itself. Matching ignores case.
	Folders map[string][]string
	HelpURL string // where the user creates an app password
	// PasteLabel is what the provider calls the secret the user pastes into the wizard.
	PasteLabel string
	// LocalPartLogin: the server may want the part before "@" as the username.
	LocalPartLogin bool
	// Domains are the domains one account's address is known by, lowercase: the same name
	// before "@" receives mail at each of them. Empty = not known, and only a username that
	// is a full address says what the account's address is.
	Domains []string
}

// commonFolders are names seen across providers; every preset falls back to them.
var commonFolders = map[string][]string{
	mail.RoleJunk:    {"Junk", "Spam", "Junk E-mail", "Junk Mail", "Bulk Mail", "Bulk"},
	mail.RoleTrash:   {"Trash", "Deleted Messages", "Deleted Items", "Bin"},
	mail.RoleArchive: {"Archive", "Archives"},
	mail.RoleSent:    {"Sent", "Sent Messages", "Sent Items", "Sent Mail"},
	mail.RoleDrafts:  {"Drafts", "Draft"},
}

// withCommon puts a provider's own names ahead of the common ones.
func withCommon(own map[string][]string) map[string][]string {
	out := make(map[string][]string, len(commonFolders))
	for role, names := range commonFolders {
		out[role] = append(append([]string{}, own[role]...), names...)
	}
	return out
}

var generic = Preset{
	Name:       "generic",
	Label:      "Other IMAP server",
	Port:       993,
	TLSMode:    TLSImplicit,
	Folders:    withCommon(nil),
	PasteLabel: "Password",
}

// All returns every preset, generic last.
func All() []Preset { return []Preset{icloud, fastmail, yahoo, zoho, generic} }

// Get looks a preset up by name.
func Get(name string) (Preset, bool) {
	for _, p := range All() {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// Usernames returns the usernames to try at login, in order.
func (p Preset) Usernames(username string) []string {
	if local, _, ok := strings.Cut(username, "@"); ok && p.LocalPartLogin && local != "" {
		return []string{username, local}
	}
	return []string{username}
}

// rank orders candidates for one role: a lower index is a better match, -1 is none.
func (p Preset) rank(role, folder string) int {
	for i, n := range p.Folders[role] {
		if strings.EqualFold(n, folder) {
			return i
		}
	}
	return -1
}

// FillRoles assigns preset roles to folders for every role the server did not report,
// picking the best-ranked name when several match.
func (p Preset) FillRoles(folders []mail.Folder) {
	for role := range p.Folders {
		best, bestRank := -1, -1
		for i, f := range folders {
			if f.SpecialUse == role {
				best = -1
				break
			}
			if f.SpecialUse != "" {
				continue
			}
			if r := p.rank(role, f.Name); r >= 0 && (best < 0 || r < bestRank) {
				best, bestRank = i, r
			}
		}
		if best >= 0 {
			folders[best].SpecialUse = role
		}
	}
}
