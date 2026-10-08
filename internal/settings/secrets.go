package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// SecretsCheck counts the secrets the database holds sealed under a master key: mailbox
// passwords and provider keys.
type SecretsCheck struct {
	Total      int // every stored secret
	Unreadable int // the ones the master key cannot open
}

// Readable is how many secrets the key opens.
func (c SecretsCheck) Readable() int { return c.Total - c.Unreadable }

// CheckSecrets tries every stored secret against master. A nil master opens none, which
// is how the caller counts the secrets a key would have to protect before it has one.
// A secret that fails to open is counted, not an error; only a database failure is.
func CheckSecrets(ctx context.Context, st *store.Store, master []byte) (SecretsCheck, error) {
	var c SecretsCheck
	accounts, err := st.Accounts(ctx)
	if err != nil {
		return c, fmt.Errorf("check stored secrets: %w", err)
	}
	for _, a := range accounts {
		c.Total++
		if master == nil {
			c.Unreadable++
			continue
		}
		if _, err := st.AccountSecret(ctx, master, a.ID); errors.Is(err, crypto.ErrDecrypt) {
			c.Unreadable++
		} else if err != nil {
			return c, fmt.Errorf("check stored secrets: %w", err)
		}
	}
	tenants, err := st.SettingsLike(ctx, keyPrefix) // every tenant's stored provider keys
	if err != nil {
		return c, fmt.Errorf("check stored secrets: %w", err)
	}
	s := &Settings{Master: master}
	for _, rows := range tenants {
		for key, stored := range rows {
			name, ok := strings.CutPrefix(key, keyPrefix)
			if !ok {
				continue
			}
			c.Total++
			if master == nil {
				c.Unreadable++
			} else if _, err := s.open(name, stored); err != nil {
				// A row that cannot be opened for any reason, a wrong key or a damaged value,
				// is one the key does not protect.
				c.Unreadable++
			}
		}
	}
	return c, nil
}
