package composer

import (
	"context"
	"slices"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// headersOnly is the text a fetch takes when no body is wanted: the connector always asks
// for some, and one byte of it is no body.
const headersOnly = 1

// FetchOptions says how much of each email's text Scanner.Fetch reads.
type FetchOptions struct {
	// NoBody reads the headers only: no text is fetched, and Summary.Body is empty.
	NoBody bool
	// BodyChars cuts the parsed text to this many characters; 0 keeps all the connector
	// read.
	BodyChars int
}

// Scanner reads the mail already in one account's folders for a feature that is not in
// this package (ext.Mail): the same listing and BODY.PEEK fetches the tester uses, so
// nothing is marked read, and it is given only Reader, so it cannot move or flag anything.
type Scanner struct {
	Store     *store.Store // the contacts index, for the is_contact and replied_before signals
	Mailbox   Reader
	AccountID int64
}

func (s Scanner) tester() Tester {
	return Tester{Store: s.Store, Mailbox: s.Mailbox, AccountID: s.AccountID}
}

// List lists the newest limit emails of folder received on or after since (zero = all of
// it), newest last, and how many the folder holds in that range before limit cut it.
func (s Scanner) List(ctx context.Context, folder string, since time.Time, limit int) ([]mail.MsgRef, int, error) {
	return s.tester().list(ctx, folder, since, limit)
}

// Fetch reads the listed emails with BODY.PEEK, in batches, and calls each for every one
// with its index in refs, whether the owner has read it and its summary, nil when it is
// gone or cannot be read as an email. each is called for several emails at once; its first
// error ends the fetch and is returned.
func (s Scanner) Fetch(ctx context.Context, refs []mail.MsgRef, opts FetchOptions, each func(ctx context.Context, i int, email *message.Summary, seen bool) error) error {
	t := s.tester()
	maxBody := 0
	if opts.NoBody {
		maxBody = headersOnly
	}
	return t.eachRaw(ctx, refs, maxBody, func(ctx context.Context, i int, raw *message.Raw) error {
		sum, err := t.parse(ctx, raw, opts.BodyChars)
		if err != nil {
			return err
		}
		if sum == nil {
			return each(ctx, i, nil, false)
		}
		if opts.NoBody {
			sum.Body = "" // the one byte read is not a body
		}
		return each(ctx, i, sum, slices.Contains(raw.Flags, `\Seen`))
	})
}
