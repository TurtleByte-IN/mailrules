package presets

// proton: Proton Mail through Proton Mail Bridge, which needs a paid Proton plan and runs
// on the same machine as MailRules. Bridge's defaults, from
// https://proton.me/support/protonmail-bridge-clients-windows-thunderbird and Bridge's
// Mailbox details: IMAP on 127.0.0.1, port 1143, STARTTLS. Bridge lets the person change
// the port and switch to SSL, so the wizard shows these as editable. The username is the
// Proton address and the password is the one Bridge generates for mail apps ("Bridge
// password"), not the Proton account password. Bridge presents a certificate it made
// itself, which the system does not trust: the person accepts it by its fingerprint.
var proton = Preset{
	Name:       "proton",
	Label:      "Proton Mail",
	Host:       "127.0.0.1",
	Port:       1143,
	TLSMode:    TLSStartTLS,
	Folders:    withCommon(nil),
	PasteLabel: "Bridge password",
	HelpURL:    "https://proton.me/support/protonmail-bridge-install",
}
