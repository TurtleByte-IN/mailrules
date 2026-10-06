// Package contacts keeps the index of addresses the user has written to, built from the
// Sent folder. It answers the is_contact and replied_before signals.
package contacts

import (
	"context"
	"fmt"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// ScanLimit is how many of the newest sent messages one scan reads.
const ScanLimit = 2000

// Sync reads the recipients of recent sent mail and upserts them into the account's
// contacts. It returns how many addresses it saw. Run it on connect and every few hours.
func Sync(ctx context.Context, mb mail.Mailbox, st *store.Store, accountID int64, sentFolder string) (int, error) {
	recipients, err := mb.SentRecipients(ctx, sentFolder, ScanLimit)
	if err != nil {
		return 0, fmt.Errorf("scan sent mail: %w", err)
	}
	rows := make([]store.Contact, len(recipients))
	for i, r := range recipients {
		rows[i] = store.Contact{Address: r.Address, LastSentAt: r.SentAt.Unix()}
	}
	if err := st.UpsertContacts(ctx, accountID, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// Fill sets the two contact signals on a parsed message. The index only records
// addresses the user has sent mail to, so today both signals mean the same thing.
func Fill(ctx context.Context, st *store.Store, s *message.Summary) error {
	known, err := st.IsContact(ctx, s.AccountID, s.From)
	if err != nil {
		return err
	}
	s.IsContact, s.RepliedBefore = known, known
	return nil
}
