package presets

// fastmail: server settings from
// https://www.fastmail.help/hc/en-us/articles/1500000278342-Server-names-and-ports.
// Implicit TLS only (not STARTTLS); an app password is required.
var fastmail = Preset{
	Name:    "fastmail",
	Label:   "Fastmail",
	Host:    "imap.fastmail.com",
	Port:    993,
	TLSMode: TLSImplicit,
	Folders: withCommon(nil),
	HelpURL: "https://www.fastmail.help/hc/en-us/articles/360058752854",
}
