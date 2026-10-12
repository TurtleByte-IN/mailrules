// Command mkfixture wrote mailrules.db beside it: a database as MailRules left it before
// tenant keys (MAI-150), every secret sealed under the master key itself (envelope 1). It
// ran at commit fe70b0f as cmd/mkfixture (`go run ./cmd/mkfixture <dir>`), against that
// version's API; it does not build against today's, and need never run again.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"log"
	"os"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

func main() {
	ctx := context.Background()
	dir := os.Args[1]
	master := bytes.Repeat([]byte{7}, 32)
	db, err := store.Open(ctx, dir)
	check(err)
	check(store.Migrate(ctx, db))
	st := store.New(db)
	admin, err := st.CreateFirstUser(ctx, "admin@self.test", "hash", 1)
	check(err)
	_, err = st.CreateAccount(ctx, master, store.Account{UserID: admin.ID, Label: "Self", Preset: "icloud", Host: "imap.mail.me.com", Port: 993, TLSMode: "implicit", Username: "admin@icloud.com", CreatedAt: 1}, "self-host app password")
	check(err)
	// A provider key as settings sealed it: crypto.Seal under an FNV hash of its name.
	h := fnv.New64a()
	h.Write([]byte("anthropic_api_key"))
	sec, dek, err := crypto.Seal(master, int64(h.Sum64()), []byte("sk-ant-self"))
	check(err)
	b, _ := json.Marshal(map[string]string{"sealed": base64.StdEncoding.EncodeToString(sec), "dek": base64.StdEncoding.EncodeToString(dek)})
	check(st.SetSetting(ctx, store.SelfHostTenant, "key.anthropic_api_key", string(b)))
	_, err = st.CreateDestination(ctx, master, store.Destination{TenantID: store.SelfHostTenant, Name: "Hook", Kind: store.DestinationWebhook},
		store.DestinationSecret{URL: "https://hooks.test/self", Secret: "whsec_self"}, 1)
	check(err)
	// A hosted tenant with a module's mailbox, removed so its copy waits for the module.
	u, err := st.SignInIdentity(ctx, store.Identity{Provider: "workos", Subject: "user_a", Email: "a@example.test", Tenant: "org_a"}, 2)
	check(err)
	gone, err := st.CreateAccount(ctx, master, store.Account{UserID: u.ID, Label: "Gmail", Preset: "gmail", Host: "imap.gmail.com", Port: 993, TLSMode: "implicit", Username: "a@gmail.com", OAuth: true, Module: true, CreatedAt: 2}, "refresh-token-a")
	check(err)
	_, err = st.CreateAccount(ctx, master, store.Account{UserID: u.ID, Label: "Work", Preset: "fastmail", Host: "imap.fastmail.com", Port: 993, TLSMode: "implicit", Username: "a@fastmail.test", CreatedAt: 2}, "hosted app password")
	check(err)
	_, err = st.DeleteAccount(ctx, gone.ID, 3)
	check(err)
	_, err = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	check(err)
	check(db.Close())
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
