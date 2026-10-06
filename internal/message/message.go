// Package message holds the email types shared by the IMAP connector, the
// rules engine and the model adapters. Bodies live in memory only.
package message

import "time"

// Raw is one message as fetched from the server with BODY.PEEK, before parsing.
type Raw struct {
	Header         []byte    // BODY.PEEK[HEADER]
	Text           []byte    // start of BODY.PEEK[TEXT], at most 64 KB
	Flags          []string  // IMAP flags at fetch time, e.g. \Seen
	InternalDate   time.Time // when the server received it
	Size           int64     // RFC822.SIZE in bytes
	HasAttachment  bool      // from BODYSTRUCTURE
	AttachmentExts []string  // lower-case, no dot, e.g. "pdf"
}

// Summary is the parsed email that conditions are matched against and that
// models are shown. Addresses and domains are lower-cased.
type Summary struct {
	AccountID      int64
	From           string // address only
	FromName       string
	FromDomain     string
	To             []string
	Cc             []string
	DeliveredTo    []string
	Subject        string
	Body           string              // plain text, control characters stripped, truncated to BODY_CHARS
	Headers        map[string][]string // canonical MIME header keys
	ListID         string
	HasAttachment  bool
	AttachmentExts []string
	SizeKB         float64
	ReceivedAt     time.Time

	// Signals.
	IsContact     bool   // sender is in the contacts index
	RepliedBefore bool   // the user has sent mail to this sender
	IsBulk        bool   // List-Unsubscribe or Precedence: bulk/list
	IsNoreply     bool   // noreply@-style sender
	DMARC         string // pass | fail | none
}
