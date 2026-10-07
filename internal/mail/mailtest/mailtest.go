// Package mailtest is an in-memory mail.Mailbox for tests of the packages that sit on top
// of the connector. It keeps the same promises as the IMAP one: UIDs only grow, a moved
// message gets a new UID in its destination, fetching never sets \Seen, and a reference
// with the wrong UIDVALIDITY finds nothing.
package mailtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// Mailbox is the fake. Set Caps before use to simulate a limited server.
type Mailbox struct {
	AccountID int64
	Caps      mail.Caps

	mu      sync.Mutex
	folders map[string]*folder
	nextVal uint32
	changed chan struct{} // closed and replaced on every delivery
}

type folder struct {
	role     string
	validity uint32
	next     uint32
	msgs     []*msg
}

type msg struct {
	uid uint32
	raw message.Raw
}

var _ mail.Mailbox = (*Mailbox)(nil)

// New returns a mailbox with an empty INBOX on a server that supports everything.
func New(accountID int64) *Mailbox {
	m := &Mailbox{
		AccountID: accountID,
		Caps:      mail.Caps{Move: true, UIDPlus: true, SpecialUse: true, Idle: true},
		folders:   map[string]*folder{},
		changed:   make(chan struct{}),
	}
	m.AddFolder("INBOX", "")
	return m
}

// AddFolder creates a folder with a special-use role ("" for none). Re-adding an existing
// name rebuilds it empty with a new UIDVALIDITY.
func (m *Mailbox) AddFolder(name, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextVal++
	m.folders[name] = &folder{role: role, validity: m.nextVal, next: 1}
}

// Deliver puts an RFC 5322 message into a folder, as new mail arriving, and wakes watchers.
func (m *Mailbox) Deliver(folderName, eml string, flags ...string) mail.MsgRef {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.folders[folderName]
	if f == nil {
		panic("mailtest: no folder " + folderName)
	}
	header, text := eml, ""
	for _, sep := range []string{"\r\n\r\n", "\n\n"} {
		if i := strings.Index(eml, sep); i >= 0 {
			header, text = eml[:i+len(sep)], eml[i+len(sep):]
			break
		}
	}
	raw := message.Raw{Header: []byte(header), Text: []byte(text), Flags: slices.Clone(flags), InternalDate: time.Now(), Size: int64(len(eml))}
	ref := m.add(folderName, f, raw)
	close(m.changed)
	m.changed = make(chan struct{})
	return ref
}

func (m *Mailbox) add(name string, f *folder, raw message.Raw) mail.MsgRef {
	f.msgs = append(f.msgs, &msg{uid: f.next, raw: raw})
	f.next++
	return mail.MsgRef{AccountID: m.AccountID, Folder: name, UIDValidity: f.validity, UID: f.next - 1}
}

func (m *Mailbox) folder(name string) (*folder, error) {
	f := m.folders[name]
	if f == nil {
		return nil, fmt.Errorf("%w: %q", mail.ErrNoFolder, name)
	}
	return f, nil
}

// find returns the folder and the index of the referenced message in it.
func (m *Mailbox) find(ref mail.MsgRef) (*folder, int, error) {
	f, err := m.folder(ref.Folder)
	if err != nil {
		return nil, 0, err
	}
	i := slices.IndexFunc(f.msgs, func(x *msg) bool { return x.uid == ref.UID })
	if ref.UIDValidity != f.validity || i < 0 {
		return nil, 0, fmt.Errorf("%w: %+v", mail.ErrNotFound, ref)
	}
	return f, i, nil
}

// Capabilities returns Caps.
func (m *Mailbox) Capabilities() mail.Caps { return m.Caps }

// Close does nothing.
func (m *Mailbox) Close() error { return nil }

// Folders lists folders by name.
func (m *Mailbox) Folders(ctx context.Context) ([]mail.Folder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mail.Folder
	for name, f := range m.folders {
		out = append(out, mail.Folder{Name: name, Delimiter: "/", SpecialUse: f.role})
	}
	slices.SortFunc(out, func(a, b mail.Folder) int { return strings.Compare(a.Name, b.Name) })
	return out, ctx.Err()
}

// EnsureFolder creates the folder if it is missing.
func (m *Mailbox) EnsureFolder(ctx context.Context, name string) error {
	m.mu.Lock()
	exists := m.folders[name] != nil
	m.mu.Unlock()
	if !exists {
		m.AddFolder(name, "")
	}
	return ctx.Err()
}

// Status returns the folder's UIDVALIDITY and next UID.
func (m *Mailbox) Status(ctx context.Context, name string) (mail.FolderStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.folder(name)
	if err != nil {
		return mail.FolderStatus{}, err
	}
	return mail.FolderStatus{UIDValidity: f.validity, UIDNext: f.next}, ctx.Err()
}

// Watch sends every message above lastUID, then each delivery, until ctx is done.
func (m *Mailbox) Watch(ctx context.Context, name string, lastUID uint32, out chan<- mail.NewMail) error {
	for {
		m.mu.Lock()
		f, err := m.folder(name)
		if err != nil {
			m.mu.Unlock()
			return err
		}
		var refs []mail.MsgRef
		for _, x := range f.msgs {
			if x.uid > lastUID {
				refs = append(refs, mail.MsgRef{AccountID: m.AccountID, Folder: name, UIDValidity: f.validity, UID: x.uid})
			}
		}
		changed := m.changed
		m.mu.Unlock()
		for _, ref := range refs {
			select {
			case out <- mail.NewMail{Ref: ref}:
				lastUID = ref.UID
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Fetch returns a copy of the message with its text cut to maxBody bytes. Flags are untouched.
func (m *Mailbox) Fetch(ctx context.Context, ref mail.MsgRef, maxBody int) (*message.Raw, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := m.fetch(ref, maxBody)
	if err != nil {
		return nil, err
	}
	return raw, ctx.Err()
}

// FetchMany returns a copy of each message, like Fetch, and nil for one that is gone.
func (m *Mailbox) FetchMany(ctx context.Context, refs []mail.MsgRef, maxBody int) ([]*message.Raw, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*message.Raw, len(refs))
	for i, ref := range refs {
		if ref.Folder != refs[0].Folder || ref.UIDValidity != refs[0].UIDValidity {
			return nil, fmt.Errorf("fetch %q: the messages are not all in one folder", refs[0].Folder)
		}
		raw, err := m.fetch(ref, maxBody)
		if err != nil && !errors.Is(err, mail.ErrNotFound) {
			return nil, err
		}
		out[i] = raw
	}
	return out, ctx.Err()
}

// fetch copies one message. Callers hold m.mu.
func (m *Mailbox) fetch(ref mail.MsgRef, maxBody int) (*message.Raw, error) {
	f, i, err := m.find(ref)
	if err != nil {
		return nil, err
	}
	raw := f.msgs[i].raw
	raw.Flags = slices.Clone(raw.Flags)
	if maxBody > 0 && len(raw.Text) > maxBody {
		raw.Text = raw.Text[:maxBody]
	}
	return &raw, nil
}

// FetchSince lists messages delivered at or after since, oldest first, newest limit only.
func (m *Mailbox) FetchSince(ctx context.Context, name string, since time.Time, limit int) ([]mail.MsgRef, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.folder(name)
	if err != nil {
		return nil, 0, err
	}
	var out []mail.MsgRef
	for _, x := range f.msgs {
		if !x.raw.InternalDate.Before(since) {
			out = append(out, mail.MsgRef{AccountID: m.AccountID, Folder: name, UIDValidity: f.validity, UID: x.uid})
		}
	}
	matched := len(out)
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, matched, ctx.Err()
}

// Move takes the message out of its folder and gives it a new UID in dest.
func (m *Mailbox) Move(ctx context.Context, ref mail.MsgRef, dest string) (mail.MsgRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.Caps.CanMove() {
		return mail.MsgRef{}, fmt.Errorf("move: %w", mail.ErrUnsupported)
	}
	f, i, err := m.find(ref)
	if err != nil {
		return mail.MsgRef{}, err
	}
	d, err := m.folder(dest)
	if err != nil {
		return mail.MsgRef{}, err
	}
	raw := f.msgs[i].raw
	f.msgs = slices.Delete(f.msgs, i, i+1)
	return m.add(dest, d, raw), ctx.Err()
}

// SetFlags adds and removes flags.
func (m *Mailbox) SetFlags(ctx context.Context, ref mail.MsgRef, add, remove []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, i, err := m.find(ref)
	if err != nil {
		return err
	}
	raw := &f.msgs[i].raw
	for _, flag := range add {
		if !slices.Contains(raw.Flags, flag) {
			raw.Flags = append(raw.Flags, flag)
		}
	}
	raw.Flags = slices.DeleteFunc(raw.Flags, func(flag string) bool { return slices.Contains(remove, flag) })
	return ctx.Err()
}

// Flags returns the message's flags, sorted.
func (m *Mailbox) Flags(ctx context.Context, ref mail.MsgRef) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, i, err := m.find(ref)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(f.msgs[i].raw.Flags)
	slices.Sort(out)
	return out, ctx.Err()
}

func header(raw message.Raw) netmail.Header {
	parsed, err := netmail.ReadMessage(bytes.NewReader(raw.Header))
	if err != nil {
		return netmail.Header{}
	}
	return parsed.Header
}

// FindByMessageID returns the newest message in the folder with that Message-ID.
func (m *Mailbox) FindByMessageID(ctx context.Context, name, messageID string) (mail.MsgRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.folder(name)
	if err != nil {
		return mail.MsgRef{}, err
	}
	want := strings.Trim(strings.TrimSpace(messageID), "<>")
	for i := len(f.msgs) - 1; i >= 0 && want != ""; i-- {
		if strings.Trim(strings.TrimSpace(header(f.msgs[i].raw).Get("Message-Id")), "<>") == want {
			return mail.MsgRef{AccountID: m.AccountID, Folder: name, UIDValidity: f.validity, UID: f.msgs[i].uid}, ctx.Err()
		}
	}
	return mail.MsgRef{}, fmt.Errorf("%w: message-id %q in %q", mail.ErrNotFound, messageID, name)
}

// SentRecipients returns the To and Cc addresses of the newest limit messages.
func (m *Mailbox) SentRecipients(ctx context.Context, name string, limit int) ([]mail.Recipient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.folder(name)
	if err != nil {
		return nil, err
	}
	msgs := f.msgs
	if limit < len(msgs) {
		msgs = msgs[len(msgs)-max(limit, 0):]
	}
	latest := map[string]time.Time{}
	for _, x := range msgs {
		h := header(x.raw)
		for _, key := range []string{"To", "Cc"} {
			addrs, _ := h.AddressList(key)
			for _, a := range addrs {
				latest[strings.ToLower(a.Address)] = x.raw.InternalDate
			}
		}
	}
	out := make([]mail.Recipient, 0, len(latest))
	for addr, at := range latest {
		out = append(out, mail.Recipient{Address: addr, SentAt: at})
	}
	slices.SortFunc(out, func(a, b mail.Recipient) int { return strings.Compare(a.Address, b.Address) })
	return out, ctx.Err()
}
