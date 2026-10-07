package presets

import "github.com/TurtleByte-IN/mailrules/internal/mail"

// icloud: server settings from https://support.apple.com/en-us/102525. An app-specific
// password is required. Folder names are the ones iCloud shows when SPECIAL-USE is missing.
var icloud = Preset{
	Name:    "icloud",
	Label:   "iCloud Mail",
	Host:    "imap.mail.me.com",
	Port:    993,
	TLSMode: TLSImplicit,
	Folders: withCommon(map[string][]string{
		mail.RoleJunk:    {"Junk"},
		mail.RoleTrash:   {"Deleted Messages"},
		mail.RoleArchive: {"Archive"},
		mail.RoleSent:    {"Sent Messages"},
	}),
	PasteLabel: "App-specific password",
	HelpURL:    "https://support.apple.com/en-us/102654",
	// Apple documents the username as the part before "@"; some accounts take the full address.
	LocalPartLogin: true,
	// An iCloud Mail name may also be in use at Apple's older me.com and mac.com domains.
	Domains: []string{"icloud.com", "me.com", "mac.com"},
}
