package presets

// yahoo: server settings from https://help.yahoo.com/kb/SLN3697.html. The username is the
// full address; an app password is required.
var yahoo = Preset{
	Name:       "yahoo",
	Label:      "Yahoo Mail",
	Host:       "imap.mail.yahoo.com",
	Port:       993,
	TLSMode:    TLSImplicit,
	Folders:    withCommon(nil),
	PasteLabel: "App password",
	HelpURL:    "https://help.yahoo.com/kb/SLN15241.html",
}
