package presets

import "github.com/TurtleByte-IN/mailrules/internal/mail"

// gmail: server settings from https://developers.google.com/workspace/gmail/imap/imap-smtp
// (also https://support.google.com/mail/answer/7126229). Implicit TLS; the username is the
// full address. Plain passwords are refused: an app password is required, which needs
// 2-Step Verification, and a Google Workspace admin can switch app passwords off. Gmail
// has no Archive folder; the IMAP client archives to All Mail, which it reports as \All.
// The names below are used only when the server reports no SPECIAL-USE.
var gmail = Preset{
	Name:    "gmail",
	Label:   "Gmail",
	Host:    "imap.gmail.com",
	Port:    993,
	TLSMode: TLSImplicit,
	Folders: withCommon(map[string][]string{
		mail.RoleJunk:    {"[Gmail]/Spam", "[Google Mail]/Spam"},
		mail.RoleTrash:   {"[Gmail]/Trash", "[Google Mail]/Trash", "[Gmail]/Bin", "[Google Mail]/Bin"},
		mail.RoleArchive: {"[Gmail]/All Mail", "[Google Mail]/All Mail"},
		mail.RoleSent:    {"[Gmail]/Sent Mail", "[Google Mail]/Sent Mail"},
		mail.RoleDrafts:  {"[Gmail]/Drafts", "[Google Mail]/Drafts"},
	}),
	PasteLabel: "App password",
	HelpURL:    "https://support.google.com/accounts/answer/185833",
	// One Gmail name receives mail at both domains. Workspace accounts use their own domain.
	Domains: []string{"gmail.com", "googlemail.com"},
}
