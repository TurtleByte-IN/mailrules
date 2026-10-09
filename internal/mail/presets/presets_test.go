package presets

import (
	"slices"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

func TestPresets(t *testing.T) {
	var names []string
	for _, p := range All() {
		names = append(names, p.Name)
		if got, ok := Get(p.Name); !ok || got.Name != p.Name {
			t.Errorf("Get(%q) = %v", p.Name, ok)
		}
		if p.Port != 993 || p.TLSMode != TLSImplicit || p.Label == "" {
			t.Errorf("%s: port %d, tls %q, label %q", p.Name, p.Port, p.TLSMode, p.Label)
		}
		if (p.Host == "") != (p.Name == "generic") || (p.HelpURL == "") != (p.Name == "generic") {
			t.Errorf("%s: host %q, help %q", p.Name, p.Host, p.HelpURL)
		}
		for _, role := range []string{mail.RoleJunk, mail.RoleTrash, mail.RoleArchive, mail.RoleSent, mail.RoleDrafts} {
			if len(p.Folders[role]) == 0 {
				t.Errorf("%s has no fallback names for %s", p.Name, role)
			}
		}
	}
	// These are the values accounts.preset may hold.
	if want := []string{"icloud", "gmail", "fastmail", "yahoo", "zoho", "generic"}; !slices.Equal(names, want) {
		t.Errorf("presets = %v, want %v", names, want)
	}
	if _, ok := Get("outlook"); ok {
		t.Error("unknown preset found")
	}
}

func TestUsernames(t *testing.T) {
	icloud, _ := Get("icloud")
	fastmail, _ := Get("fastmail")
	for _, tc := range []struct {
		preset Preset
		in     string
		want   []string
	}{
		{icloud, "me@icloud.com", []string{"me@icloud.com", "me"}},
		{icloud, "me", []string{"me"}},
		{fastmail, "me@fastmail.com", []string{"me@fastmail.com"}},
	} {
		if got := tc.preset.Usernames(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("%s.Usernames(%q) = %v, want %v", tc.preset.Name, tc.in, got, tc.want)
		}
	}
}

func TestFillRoles(t *testing.T) {
	icloud, _ := Get("icloud")
	gmail, _ := Get("gmail")
	for _, tc := range []struct {
		name   string
		preset Preset
		in     []mail.Folder
		want   map[string]string // folder -> role, only folders that get one
	}{
		{"icloud names", icloud, []mail.Folder{{Name: "INBOX"}, {Name: "Deleted Messages"}, {Name: "sent messages"}, {Name: "Drafts"}},
			map[string]string{"Deleted Messages": mail.RoleTrash, "sent messages": mail.RoleSent, "Drafts": mail.RoleDrafts}},
		{"best-ranked name wins", icloud, []mail.Folder{{Name: "Trash"}, {Name: "Deleted Messages"}},
			map[string]string{"Deleted Messages": mail.RoleTrash}},
		{"the server's own role is kept", icloud, []mail.Folder{{Name: "Corbeille", SpecialUse: mail.RoleTrash}, {Name: "Trash"}},
			map[string]string{"Corbeille": mail.RoleTrash}},
		{"a folder with a role is not given another", icloud, []mail.Folder{{Name: "Junk", SpecialUse: mail.RoleArchive}},
			map[string]string{"Junk": mail.RoleArchive}},
		{"gmail names without SPECIAL-USE", gmail, []mail.Folder{{Name: "INBOX"}, {Name: "[Gmail]/All Mail"}, {Name: "[Gmail]/Spam"},
			{Name: "[Gmail]/Trash"}, {Name: "[Gmail]/Sent Mail"}, {Name: "[Gmail]/Drafts"}, {Name: "Receipts"}},
			map[string]string{"[Gmail]/All Mail": mail.RoleArchive, "[Gmail]/Spam": mail.RoleJunk, "[Gmail]/Trash": mail.RoleTrash,
				"[Gmail]/Sent Mail": mail.RoleSent, "[Gmail]/Drafts": mail.RoleDrafts}},
		{"google mail names", gmail, []mail.Folder{{Name: "[Google Mail]/All Mail"}, {Name: "[Google Mail]/Bin"}},
			map[string]string{"[Google Mail]/All Mail": mail.RoleArchive, "[Google Mail]/Bin": mail.RoleTrash}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.preset.FillRoles(tc.in)
			got := map[string]string{}
			for _, f := range tc.in {
				if f.SpecialUse != "" {
					got[f.Name] = f.SpecialUse
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("roles = %v, want %v", got, tc.want)
			}
			for name, role := range tc.want {
				if got[name] != role {
					t.Errorf("roles = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
