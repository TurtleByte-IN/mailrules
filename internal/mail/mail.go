// Package mail defines the Mailbox interface every mail provider implements, and the
// types and errors shared by its callers. The IMAP implementation is in mail/imap and
// the in-memory fake for tests is in mail/mailtest.
package mail

import (
	"context"
	"errors"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// Sentinel errors. Implementations wrap them, so callers test with errors.Is.
var (
	// ErrAuth means the server refused the credentials. Do not retry; ask the user.
	ErrAuth = errors.New("authentication failed")
	// ErrTLS means the TLS handshake or certificate check failed. Do not retry.
	ErrTLS = errors.New("tls failure")
	// ErrConnection means the connection was lost, reset or timed out. Reconnect.
	ErrConnection = errors.New("connection lost")
	// ErrThrottled means the server is unavailable or rate limiting. Back off for minutes.
	ErrThrottled = errors.New("server unavailable or throttling")
	// ErrUnsupported means the server lacks a capability the operation needs.
	ErrUnsupported = errors.New("unsupported capability")
	// ErrNoFolder means the folder does not exist. Re-run folder discovery.
	ErrNoFolder = errors.New("no such folder")
	// ErrNotFound means the message is not there: wrong UID, or the folder's UIDVALIDITY changed.
	ErrNotFound = errors.New("message not found")
)

// Special-use folder roles (RFC 6154), as stored in folders.special_use.
const (
	RoleJunk    = `\Junk`
	RoleTrash   = `\Trash`
	RoleArchive = `\Archive`
	RoleSent    = `\Sent`
	RoleDrafts  = `\Drafts`
)

// MsgRef addresses one message. A UID is only meaningful together with its UIDValidity.
type MsgRef struct {
	AccountID   int64
	Folder      string
	UIDValidity uint32
	UID         uint32
}

// NewMail is what Watch reports for each arrival.
type NewMail struct{ Ref MsgRef }

// Caps are the server capabilities the daemon cares about.
type Caps struct {
	Move       bool
	UIDPlus    bool
	SpecialUse bool
	Idle       bool
	All        []string // every advertised capability, sorted; stored in accounts.capabilities
}

// CanMove reports whether messages can be moved without risking other mail:
// UID MOVE, or UID COPY followed by a UID EXPUNGE of exactly the copied UID.
func (c Caps) CanMove() bool { return c.Move || c.UIDPlus }

// Folder is one selectable folder. Name is the server's own name, hierarchy delimiter included.
type Folder struct {
	Name       string
	Delimiter  string // empty when the server has a flat namespace
	SpecialUse string // one of the Role constants, or empty
}

// FolderStatus is what a caller needs to baseline a folder before watching it.
type FolderStatus struct {
	UIDValidity uint32
	UIDNext     uint32
}

// Recipient is an address the user sent mail to, read from a Sent folder.
type Recipient struct {
	Address string // lower-cased
	SentAt  time.Time
}

// Mailbox is one account on one mail server. Implementations are safe for concurrent use.
type Mailbox interface {
	Capabilities() Caps
	// Folders lists selectable folders with their special-use role.
	Folders(ctx context.Context) ([]Folder, error)
	// EnsureFolder creates the folder, and any missing parents, if it does not exist.
	EnsureFolder(ctx context.Context, name string) error
	// Status returns the folder's UIDVALIDITY and UIDNEXT. Callers compare UIDValidity with
	// the stored one and, when it differs (or on first connect), baseline lastUID to
	// UIDNext-1 so a whole mailbox is never re-sorted.
	Status(ctx context.Context, folder string) (FolderStatus, error)
	// Watch blocks until ctx is done. It sends every message newer than lastUID,
	// first as catch-up, then as arrivals are reported, in UID order. It reconnects on
	// its own and returns early only for errors that retrying cannot fix (ErrAuth, ErrTLS).
	Watch(ctx context.Context, folder string, lastUID uint32, out chan<- NewMail) error
	// Fetch reads headers and at most maxBody bytes of body without marking the message read.
	Fetch(ctx context.Context, ref MsgRef, maxBody int) (*message.Raw, error)
	// FetchSince lists messages received on or after since's date, oldest first.
	// A positive limit keeps only the newest limit messages. matched is how many messages
	// the folder holds in that range before the limit was applied, so a caller can say
	// "the newest 2,000 of 4,310"; the search that finds them already knows it.
	FetchSince(ctx context.Context, folder string, since time.Time, limit int) (refs []MsgRef, matched int, err error)
	// Move moves one message and returns where it now lives.
	Move(ctx context.Context, ref MsgRef, dest string) (MsgRef, error)
	SetFlags(ctx context.Context, ref MsgRef, add, remove []string) error
	Flags(ctx context.Context, ref MsgRef) ([]string, error)
	// FindByMessageID returns the newest message in folder with that Message-ID, or ErrNotFound.
	FindByMessageID(ctx context.Context, folder, messageID string) (MsgRef, error)
	// SentRecipients reads the To and Cc addresses of the newest limit messages in folder
	// (headers only), one entry per address with the latest time it was written to.
	SentRecipients(ctx context.Context, folder string, limit int) ([]Recipient, error)
	Close() error
}
