package presets

// zoho: server settings from https://www.zoho.com/mail/help/imap-access.html. IMAP access must
// be switched on in webmail first. Paid organisations use imappro.zoho.com and other data
// centres use their own domain (imap.zoho.eu, imap.zoho.in, ...): pass --host for those.
var zoho = Preset{
	Name:       "zoho",
	Label:      "Zoho Mail",
	Host:       "imap.zoho.com",
	Port:       993,
	TLSMode:    TLSImplicit,
	Folders:    withCommon(nil),
	PasteLabel: "App password",
	HelpURL:    "https://www.zoho.com/mail/help/imap-access.html",
}
