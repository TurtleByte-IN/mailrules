package presets

import "github.com/TurtleByte-IN/mailrules/internal/mail"

// outlook: Outlook.com, Hotmail, Live and Microsoft 365. Server settings from
// https://support.microsoft.com/en-us/outlook/pop-imap-and-smtp-settings-for-outlook-com:
// outlook.office365.com, port 993, implicit TLS, the username is the full address, and
// Microsoft takes only its own sign-in (OAuth 2.0, which IMAP carries as SASL XOAUTH2,
// https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth):
// neither the account password nor an app password works over IMAP. So it is OAuthOnly: a
// mailbox here is connected only through a module's one-click sign-in. Outlook reports
// SPECIAL-USE; the names below are its English ones, used only when it does not.
var outlook = Preset{
	Name:    "outlook",
	Label:   "Outlook",
	Host:    "outlook.office365.com",
	Port:    993,
	TLSMode: TLSImplicit,
	Folders: withCommon(map[string][]string{
		mail.RoleJunk:    {"Junk Email"},
		mail.RoleTrash:   {"Deleted Items"},
		mail.RoleArchive: {"Archive"},
		mail.RoleSent:    {"Sent Items"},
		mail.RoleDrafts:  {"Drafts"},
	}),
	HelpURL:   "https://support.microsoft.com/en-us/outlook/pop-imap-and-smtp-settings-for-outlook-com",
	OAuthOnly: true,
}
