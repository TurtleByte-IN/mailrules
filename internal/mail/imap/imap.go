// Package imap implements mail.Mailbox over IMAP with go-imap v2.
//
// Each Mailbox uses two connections: Watch dials its own long-lived one that only
// selects, searches and IDLEs, and every other method shares one worker connection,
// dialed on first use and redialed when the server has dropped it.
package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	netmail "net/mail"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// maxText caps BODY.PEEK[TEXT]: models only ever see the start of a message.
const maxText = 64 * 1024

// Config is everything needed to reach one account.
type Config struct {
	AccountID int64
	Host      string
	Port      int
	TLSMode   string // presets.TLSImplicit or presets.TLSStartTLS; there is no plaintext mode
	Username  string
	Password  string
	Preset    presets.Preset // folder-name fallbacks; zero means generic
	TLSConfig *tls.Config    // nil uses the system roots; tests pass their own CA
	Logger    *slog.Logger   // nil uses slog.Default()

	// CertFingerprint is the server certificate the person accepted (mail.Cert.Fingerprint).
	// When set, the server must present exactly that certificate, and the system's roots
	// and the host name are not checked: the pin replaces both. Empty trusts the system.
	CertFingerprint string

	// Token, when set, is how the account signs in: with OAuth (SASL XOAUTH2) as Username,
	// with the access token it returns, asked for again just before every login, so a
	// connection the server closed when its token expired comes back with a fresh one.
	// Password is not used. An error that wraps mail.ErrReconnect stops the account; any
	// other is a failed connection, retried. The server refusing the token is
	// mail.ErrReconnect too: the only fix is signing in to the provider again.
	Token func(ctx context.Context) (string, error)

	// Timers. Zero means the default; tests shrink them to milliseconds.
	IdleRestart     time.Duration // leave IDLE and check the connection (25 min)
	PollInterval    time.Duration // search for new mail when the server lacks IDLE (60 s)
	BackoffMin      time.Duration // first reconnect delay (1 s), doubling up to BackoffMax
	BackoffMax      time.Duration // longest reconnect delay (5 min)
	ThrottleBackoff time.Duration // delay after [UNAVAILABLE] or throttling (5 min)

	forcePoll bool // tests: behave as if the server lacked IDLE
}

// Mailbox is a mail.Mailbox over IMAP.
type Mailbox struct {
	cfg  Config
	log  *slog.Logger
	caps mail.Caps
	set  imap.CapSet

	mu          sync.Mutex // guards the worker connection and its selected folder
	worker      *imapclient.Client
	selName     string
	selValidity uint32
}

var _ mail.Mailbox = (*Mailbox)(nil)

// Open logs in once, which checks the credentials and reads the server's capabilities.
func Open(ctx context.Context, cfg Config) (*Mailbox, error) {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.IdleRestart, 25*time.Minute)
	def(&cfg.PollInterval, 60*time.Second)
	def(&cfg.BackoffMin, time.Second)
	def(&cfg.BackoffMax, 5*time.Minute)
	def(&cfg.ThrottleBackoff, 5*time.Minute)
	if cfg.Preset.Name == "" {
		cfg.Preset, _ = presets.Get("generic")
	}
	m := &Mailbox{cfg: cfg, log: cfg.Logger}
	if m.log == nil {
		m.log = slog.Default()
	}
	c, err := m.dial(ctx, nil)
	if err != nil {
		return nil, err
	}
	m.worker = c
	m.set = c.Caps()
	for cp := range m.set {
		m.caps.All = append(m.caps.All, string(cp))
	}
	slices.Sort(m.caps.All)
	m.caps.Move = m.set.Has(imap.CapMove)
	m.caps.UIDPlus = m.set.Has(imap.CapUIDPlus)
	m.caps.SpecialUse = m.set.Has(imap.CapSpecialUse)
	m.caps.Idle = m.set.Has(imap.CapIdle) && !cfg.forcePoll
	return m, nil
}

// dial connects, negotiates TLS and logs in. Errors never contain the password.
func (m *Mailbox) dial(ctx context.Context, handler *imapclient.UnilateralDataHandler) (*imapclient.Client, error) {
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if m.cfg.TLSConfig != nil {
		tlsCfg = m.cfg.TLSConfig.Clone()
	}
	if tlsCfg.ServerName == "" {
		tlsCfg.ServerName = m.cfg.Host
	}
	// The certificate is checked in verifyCert instead of by crypto/tls, so a failure can
	// say which certificate it was, and an accepted one can be pinned.
	roots, name, pin := tlsCfg.RootCAs, tlsCfg.ServerName, m.cfg.CertFingerprint
	tlsCfg.InsecureSkipVerify = true // #nosec G402 -- verifyCert checks the chain and the name, or the pin
	tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error { return verifyCert(cs, roots, name, pin) }
	opts := &imapclient.Options{TLSConfig: tlsCfg, UnilateralDataHandler: handler}
	nd := &net.Dialer{Timeout: 30 * time.Second}

	var c *imapclient.Client
	switch m.cfg.TLSMode {
	case presets.TLSImplicit:
		conn, err := (&tls.Dialer{NetDialer: nd, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, classify("connect to "+addr, err)
		}
		c = imapclient.New(conn, opts)
	case presets.TLSStartTLS:
		conn, err := nd.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, classify("connect to "+addr, err)
		}
		if c, err = imapclient.NewStartTLS(conn, opts); err != nil {
			return nil, classify("starttls with "+addr, err)
		}
	default:
		return nil, fmt.Errorf("tls mode %q must be %s or %s", m.cfg.TLSMode, presets.TLSImplicit, presets.TLSStartTLS)
	}
	// go-imap commands take no context; closing the connection is how a cancel lands.
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	if err := m.login(ctx, c, addr); err != nil {
		_ = c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return c, nil
}

// login signs in on a connection just made: with the password, or with an access token
// (Config.Token). Errors never contain the password or the token.
func (m *Mailbox) login(ctx context.Context, c *imapclient.Client, addr string) error {
	if m.cfg.Token == nil {
		err := c.Login(m.cfg.Username, m.cfg.Password).Wait()
		var ie *imap.Error
		if err != nil && errors.As(err, &ie) && kindOf(err) == nil {
			// Many servers answer a bad password with a bare NO and no response code.
			return fmt.Errorf("log in to %s: %w: %w", addr, mail.ErrAuth, err)
		}
		return classify("log in to "+addr, err)
	}
	token, err := m.cfg.Token(ctx)
	switch {
	case err == nil:
	case errors.Is(err, mail.ErrReconnect), ctx.Err() != nil:
		return fmt.Errorf("sign in to %s: %w", addr, err)
	default:
		return fmt.Errorf("sign in to %s: get an access token: %w: %w", addr, mail.ErrConnection, err)
	}
	err = c.Authenticate(xoauth2{user: m.cfg.Username, token: token})
	var ie *imap.Error
	if errors.Is(err, errTokenRefused) ||
		(errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo && (kindOf(err) == nil || errors.Is(kindOf(err), mail.ErrAuth))) {
		return fmt.Errorf("log in to %s with an access token: %w: %w", addr, mail.ErrReconnect, err)
	}
	return classify("log in to "+addr+" with an access token", err)
}

// verifyCert accepts the server's certificate when it is the pinned one or, with no pin,
// when it chains to a trusted root and is made out to host. crypto/tls runs it in place of
// its own check (see dial). Any other certificate is a *mail.CertError naming it.
func verifyCert(cs tls.ConnectionState, roots *x509.CertPool, host, pin string) error {
	if len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("%w: the server sent no certificate", mail.ErrTLS)
	}
	leaf := cs.PeerCertificates[0]
	cert := mail.CertOf(leaf)
	if pin != "" {
		if cert.Fingerprint == pin {
			return nil
		}
		return &mail.CertError{Host: host, Cert: cert, Pinned: pin}
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: host})
	if err == nil {
		return nil
	}
	var (
		unknownCA x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	reason := "the system cannot verify it"
	switch {
	case errors.As(err, &unknownCA):
		reason = "it is self-made, or signed by an authority this system does not know"
	case errors.As(err, &hostname):
		reason = "it is made out to another name than " + host
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		reason = "it has expired, or is not valid yet"
	}
	return &mail.CertError{Host: host, Cert: cert, Reason: reason}
}

// kindOf maps an error to the sentinel a caller should act on, or nil for a plain
// server refusal (a NO that is nobody's fault but the request's).
func kindOf(err error) error {
	var ie *imap.Error
	if errors.As(err, &ie) {
		switch ie.Code {
		case imap.ResponseCodeAuthenticationFailed, imap.ResponseCodeAuthorizationFailed, imap.ResponseCodeExpired:
			return mail.ErrAuth
		case imap.ResponseCodeUnavailable, imap.ResponseCodeLimit, imap.ResponseCodeInUse:
			return mail.ErrThrottled
		case imap.ResponseCodeNonExistent, imap.ResponseCodeTryCreate:
			return mail.ErrNoFolder
		}
		if ie.Type == imap.StatusResponseTypeBye {
			return mail.ErrConnection
		}
		return nil
	}
	var (
		unknownCA x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
		verify    *tls.CertificateVerificationError
		record    tls.RecordHeaderError
		alert     tls.AlertError
	)
	if errors.As(err, &unknownCA) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &verify) || errors.As(err, &record) || errors.As(err, &alert) {
		return mail.ErrTLS
	}
	for _, s := range []error{mail.ErrAuth, mail.ErrReconnect, mail.ErrTLS, mail.ErrConnection, mail.ErrThrottled,
		mail.ErrUnsupported, mail.ErrNoFolder, mail.ErrNotFound, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, s) {
			return nil // already classified
		}
	}
	return mail.ErrConnection // anything else came from the network
}

// classify wraps err with what was being done and the sentinel for its kind.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if kind := kindOf(err); kind != nil {
		return fmt.Errorf("%s: %w: %w", op, kind, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// do runs fn on the worker connection, redialing first if the server dropped it. A command
// is never retried: a move that half-happened must not be repeated blindly.
func (m *Mailbox) do(ctx context.Context, op string, fn func(c *imapclient.Client) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.worker != nil {
		select {
		case <-m.worker.Closed():
			m.worker = nil
		default:
		}
	}
	if m.worker == nil {
		c, err := m.dial(ctx, nil)
		if err != nil {
			return err
		}
		m.worker, m.selName = c, ""
	}
	c := m.worker
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	err := fn(c)
	if ctx.Err() != nil || (err != nil && errors.Is(kindOf(err), mail.ErrConnection)) {
		// The connection is gone (a cancel closes it); the next call dials a fresh one.
		_ = c.Close()
		m.worker = nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return classify(op, err)
}

// selectFolder selects folder on the worker unless it already is. Callers hold m.mu.
func (m *Mailbox) selectFolder(c *imapclient.Client, folder string) error {
	if m.selName == folder {
		return nil
	}
	m.selName = ""
	data, err := c.Select(folder, nil).Wait()
	if err != nil {
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo && kindOf(err) == nil {
			return fmt.Errorf("select %q: %w: %w", folder, mail.ErrNoFolder, err)
		}
		return fmt.Errorf("select %q: %w", folder, err)
	}
	m.selName, m.selValidity = folder, data.UIDValidity
	return nil
}

// onMessage runs fn with ref's folder selected, after checking the UID still means the
// same message: acting on a UID from an older UIDVALIDITY would hit someone else's mail.
func (m *Mailbox) onMessage(ctx context.Context, op string, ref mail.MsgRef, fn func(c *imapclient.Client, uid imap.UIDSet) error) error {
	return m.do(ctx, op, func(c *imapclient.Client) error {
		if err := m.selectFolder(c, ref.Folder); err != nil {
			return err
		}
		if ref.UIDValidity != m.selValidity {
			return fmt.Errorf("%w: uidvalidity of %q is %d, the reference has %d", mail.ErrNotFound, ref.Folder, m.selValidity, ref.UIDValidity)
		}
		return fn(c, imap.UIDSetNum(imap.UID(ref.UID)))
	})
}

// Capabilities reports what the server advertised after login.
func (m *Mailbox) Capabilities() mail.Caps { return m.caps }

// Close drops the worker connection. A running Watch stops through its context.
func (m *Mailbox) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.worker == nil {
		return nil
	}
	err := m.worker.Close()
	m.worker, m.selName = nil, ""
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// roles are the special-use attributes worth keeping.
var roles = map[imap.MailboxAttr]string{
	imap.MailboxAttrJunk:    mail.RoleJunk,
	imap.MailboxAttrTrash:   mail.RoleTrash,
	imap.MailboxAttrArchive: mail.RoleArchive,
	imap.MailboxAttrSent:    mail.RoleSent,
	imap.MailboxAttrDrafts:  mail.RoleDrafts,
}

// toFolders keeps selectable folders, takes roles from the server's attributes and
// fills the rest from the preset's folder names. A server with no \Archive folder but a
// folder holding all mail (\All, Gmail's All Mail) archives there: on Gmail, archiving is
// taking the INBOX label off, which moving a message from INBOX to All Mail does.
func toFolders(list []*imap.ListData, preset presets.Preset) []mail.Folder {
	var out []mail.Folder
	all, hasArchive := -1, false
	for _, l := range list {
		if slices.Contains(l.Attrs, imap.MailboxAttrNoSelect) || slices.Contains(l.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		f := mail.Folder{Name: l.Mailbox}
		if l.Delim != 0 {
			f.Delimiter = string(l.Delim)
		}
		for _, a := range l.Attrs {
			if r, ok := roles[a]; ok {
				f.SpecialUse = r
				break
			}
		}
		hasArchive = hasArchive || f.SpecialUse == mail.RoleArchive
		if all < 0 && f.SpecialUse == "" && slices.Contains(l.Attrs, imap.MailboxAttrAll) {
			all = len(out)
		}
		out = append(out, f)
	}
	if !hasArchive && all >= 0 {
		out[all].SpecialUse = mail.RoleArchive
	}
	preset.FillRoles(out)
	return out
}

// Folders runs LIST "" "*", asking for special-use attributes where the server can return them.
func (m *Mailbox) Folders(ctx context.Context) ([]mail.Folder, error) {
	var opts *imap.ListOptions
	if m.caps.SpecialUse && m.set.Has(imap.CapListExtended) {
		opts = &imap.ListOptions{ReturnSpecialUse: true}
	}
	var out []mail.Folder
	err := m.do(ctx, "list folders", func(c *imapclient.Client) error {
		list, err := c.List("", "*", opts).Collect()
		if err != nil {
			return err
		}
		out = toFolders(list, m.cfg.Preset)
		return nil
	})
	return out, err
}

// EnsureFolder creates name and any missing parents, split on the server's hierarchy
// delimiter, and subscribes what it creates. name is a server-side name, as Folders returns.
func (m *Mailbox) EnsureFolder(ctx context.Context, name string) error {
	return m.do(ctx, fmt.Sprintf("ensure folder %q", name), func(c *imapclient.Client) error {
		root, err := c.List("", "", nil).Collect()
		if err != nil {
			return err
		}
		levels := []string{name}
		if len(root) > 0 && root[0].Delim != 0 {
			delim := string(root[0].Delim)
			levels = levels[:0]
			parts := strings.Split(strings.Trim(name, delim), delim)
			for i := range parts {
				levels = append(levels, strings.Join(parts[:i+1], delim))
			}
		}
		for _, level := range levels {
			found, err := c.List("", level, nil).Collect()
			if err != nil {
				return err
			}
			if len(found) > 0 && !slices.Contains(found[0].Attrs, imap.MailboxAttrNonExistent) {
				continue
			}
			if err := c.Create(level, nil).Wait(); err != nil {
				var ie *imap.Error
				if !errors.As(err, &ie) || ie.Code != imap.ResponseCodeAlreadyExists {
					return err
				}
			}
			if err := c.Subscribe(level).Wait(); err != nil {
				return err
			}
		}
		return nil
	})
}

// Status asks for UIDVALIDITY and UIDNEXT without selecting the folder.
func (m *Mailbox) Status(ctx context.Context, folder string) (mail.FolderStatus, error) {
	var st mail.FolderStatus
	err := m.do(ctx, fmt.Sprintf("status of %q", folder), func(c *imapclient.Client) error {
		data, err := c.Status(folder, &imap.StatusOptions{UIDNext: true, UIDValidity: true}).Wait()
		if err != nil {
			var ie *imap.Error
			if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo && kindOf(err) == nil {
				return fmt.Errorf("%w: %w", mail.ErrNoFolder, err)
			}
			return err
		}
		st = mail.FolderStatus{UIDValidity: data.UIDValidity, UIDNext: uint32(data.UIDNext)}
		return nil
	})
	return st, err
}

// fetchItems is what Fetch and FetchMany ask for: BODY.PEEK only, so nothing is marked read.
type fetchItems struct {
	opts         *imap.FetchOptions
	header, text *imap.FetchItemBodySection
}

func newFetchItems(maxBody int) fetchItems {
	if maxBody <= 0 || maxBody > maxText {
		maxBody = maxText
	}
	header := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	text := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true,
		Partial: &imap.SectionPartial{Offset: 0, Size: int64(maxBody)}}
	return fetchItems{header: header, text: text, opts: &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{header, text},
	}}
}

func (f fetchItems) raw(b *imapclient.FetchMessageBuffer) *message.Raw {
	raw := &message.Raw{
		Header:       b.FindBodySection(f.header),
		Text:         b.FindBodySection(f.text),
		Flags:        flagStrings(b.Flags),
		InternalDate: b.InternalDate,
		Size:         b.RFC822Size,
	}
	raw.HasAttachment, raw.AttachmentExts = attachments(b.BodyStructure)
	return raw
}

// Fetch reads one message with BODY.PEEK only, so it stays unread.
func (m *Mailbox) Fetch(ctx context.Context, ref mail.MsgRef, maxBody int) (*message.Raw, error) {
	items := newFetchItems(maxBody)
	var raw *message.Raw
	err := m.onMessage(ctx, "fetch", ref, func(c *imapclient.Client, uid imap.UIDSet) error {
		b, err := fetchOne(c, uid, items.opts)
		if err != nil {
			return err
		}
		raw = items.raw(b)
		return nil
	})
	return raw, err
}

// FetchMany reads the messages with one UID FETCH, BODY.PEEK only, so they stay unread. A
// UID the server does not return, or a folder whose UIDVALIDITY has changed, means gone.
func (m *Mailbox) FetchMany(ctx context.Context, refs []mail.MsgRef, maxBody int) ([]*message.Raw, error) {
	out := make([]*message.Raw, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	folder, validity := refs[0].Folder, refs[0].UIDValidity
	var uids imap.UIDSet
	at := make(map[imap.UID][]int, len(refs))
	for i, ref := range refs {
		if ref.Folder != folder || ref.UIDValidity != validity {
			return nil, fmt.Errorf("fetch %q: the messages are not all in one folder", folder)
		}
		uid := imap.UID(ref.UID)
		if at[uid] == nil {
			uids.AddNum(uid)
		}
		at[uid] = append(at[uid], i)
	}
	items := newFetchItems(maxBody)
	err := m.do(ctx, fmt.Sprintf("fetch %q", folder), func(c *imapclient.Client) error {
		if err := m.selectFolder(c, folder); err != nil {
			return err
		}
		if validity != m.selValidity {
			return nil // every UID now means another message, or none
		}
		msgs, err := c.Fetch(uids, items.opts).Collect()
		if err != nil {
			return err
		}
		for _, b := range msgs {
			for _, i := range at[b.UID] {
				out[i] = items.raw(b)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fetchOne returns the FETCH data for exactly the UID asked for, or ErrNotFound.
func fetchOne(c *imapclient.Client, uid imap.UIDSet, opts *imap.FetchOptions) (*imapclient.FetchMessageBuffer, error) {
	msgs, err := c.Fetch(uid, opts).Collect()
	if err != nil {
		return nil, err
	}
	for _, b := range msgs {
		if uid.Contains(b.UID) {
			return b, nil
		}
	}
	return nil, fmt.Errorf("%w: uid %s", mail.ErrNotFound, uid.String())
}

func flagStrings(flags []imap.Flag) []string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	slices.Sort(out)
	return out
}

func toFlags(flags []string) []imap.Flag {
	out := make([]imap.Flag, len(flags))
	for i, f := range flags {
		out[i] = imap.Flag(f)
	}
	return out
}

// attachments reads BODYSTRUCTURE: a part counts when it is marked as an attachment, or
// has a filename and is not inline (inline images in signatures are not attachments).
func attachments(bs imap.BodyStructure) (bool, []string) {
	if bs == nil {
		return false, nil
	}
	has := false
	var exts []string
	bs.Walk(func(_ []int, part imap.BodyStructure) bool {
		single, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		disp := ""
		if d := single.Disposition(); d != nil {
			disp = strings.ToLower(d.Value)
		}
		name := single.Filename()
		if disp != "attachment" && (name == "" || disp == "inline") {
			return true
		}
		has = true
		if ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), ".")); ext != "" && !slices.Contains(exts, ext) {
			exts = append(exts, ext)
		}
		return true
	})
	return has, exts
}

// FetchSince searches by internal date, which IMAP compares by day.
func (m *Mailbox) FetchSince(ctx context.Context, folder string, since time.Time, limit int) (refs []mail.MsgRef, matched int, err error) {
	err = m.do(ctx, fmt.Sprintf("search %q", folder), func(c *imapclient.Client) error {
		if err := m.selectFolder(c, folder); err != nil {
			return err
		}
		data, err := c.UIDSearch(&imap.SearchCriteria{Since: since}, nil).Wait()
		if err != nil {
			return err
		}
		uids := data.AllUIDs()
		slices.Sort(uids)
		matched = len(uids)
		if limit > 0 && len(uids) > limit {
			uids = uids[len(uids)-limit:]
		}
		for _, uid := range uids {
			refs = append(refs, mail.MsgRef{AccountID: m.cfg.AccountID, Folder: folder, UIDValidity: m.selValidity, UID: uint32(uid)})
		}
		return nil
	})
	return refs, matched, err
}

// Move uses UID MOVE, or UID COPY, UID STORE +FLAGS (\Deleted) and UID EXPUNGE of that one
// UID. It never sends a bare EXPUNGE, which would also remove mail other clients had marked
// deleted; a server with neither MOVE nor UIDPLUS gets ErrUnsupported instead.
func (m *Mailbox) Move(ctx context.Context, ref mail.MsgRef, dest string) (mail.MsgRef, error) {
	out := mail.MsgRef{AccountID: ref.AccountID, Folder: dest}
	messageID := ""
	err := m.onMessage(ctx, "move", ref, func(c *imapclient.Client, uid imap.UIDSet) error {
		var destUIDs imap.NumSet
		if !m.caps.CanMove() {
			return fmt.Errorf("%w: the server has neither MOVE nor UIDPLUS, so mail cannot be moved safely", mail.ErrUnsupported)
		}
		// Check the message is there first: servers disagree on how they answer a move of nothing.
		if _, err := fetchOne(c, uid, &imap.FetchOptions{UID: true}); err != nil {
			return err
		}
		switch {
		case m.caps.Move:
			if !m.caps.UIDPlus {
				// No COPYUID will come back, so remember the Message-ID to find the message again.
				var err error
				if messageID, err = fetchMessageID(c, uid); err != nil {
					return err
				}
			}
			data, err := c.Move(uid, dest).Wait()
			if err != nil {
				return err
			}
			out.UIDValidity, destUIDs = data.UIDValidity, data.DestUIDs
		default: // UIDPLUS
			data, err := c.Copy(uid, dest).Wait()
			if err != nil {
				return err
			}
			if len(data.DestUIDs) == 0 {
				return fmt.Errorf("%w: uid %d", mail.ErrNotFound, ref.UID) // nothing copied: delete nothing
			}
			out.UIDValidity, destUIDs = data.UIDValidity, data.DestUIDs
			store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
			if err := c.Store(uid, store, nil).Close(); err != nil {
				return fmt.Errorf("copied to %q but could not mark the original deleted: %w", dest, err)
			}
			if err := c.UIDExpunge(uid).Close(); err != nil {
				return fmt.Errorf("copied to %q but could not expunge the original: %w", dest, err)
			}
		}
		if set, ok := destUIDs.(imap.UIDSet); ok {
			if uids, ok := set.Nums(); ok && len(uids) > 0 {
				out.UID = uint32(uids[0])
			}
		}
		if out.UID == 0 && messageID == "" {
			return fmt.Errorf("%w: uid %d", mail.ErrNotFound, ref.UID)
		}
		return nil
	})
	if err != nil {
		return mail.MsgRef{}, err
	}
	if out.UID == 0 {
		return m.FindByMessageID(ctx, dest, messageID)
	}
	return out, nil
}

// fetchMessageID peeks at one message's Message-ID header.
func fetchMessageID(c *imapclient.Client, uid imap.UIDSet) (string, error) {
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Message-ID"}, Peek: true}
	b, err := fetchOne(c, uid, &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}})
	if err != nil {
		return "", err
	}
	msg, err := netmail.ReadMessage(bytes.NewReader(b.FindBodySection(section)))
	if err != nil {
		return "", fmt.Errorf("read message-id: %w", err)
	}
	id := strings.Trim(strings.TrimSpace(msg.Header.Get("Message-Id")), "<>")
	if id == "" {
		return "", fmt.Errorf("%w: the server reports no new UID for moves and the message has no Message-ID to find it by", mail.ErrUnsupported)
	}
	return id, nil
}

// SetFlags adds and removes flags without reading the message.
func (m *Mailbox) SetFlags(ctx context.Context, ref mail.MsgRef, add, remove []string) error {
	return m.onMessage(ctx, "set flags", ref, func(c *imapclient.Client, uid imap.UIDSet) error {
		for _, ch := range []struct {
			op    imap.StoreFlagsOp
			flags []string
		}{{imap.StoreFlagsAdd, add}, {imap.StoreFlagsDel, remove}} {
			if len(ch.flags) == 0 {
				continue
			}
			if err := c.Store(uid, &imap.StoreFlags{Op: ch.op, Silent: true, Flags: toFlags(ch.flags)}, nil).Close(); err != nil {
				return err
			}
		}
		return nil
	})
}

// Flags returns the message's current flags, sorted.
func (m *Mailbox) Flags(ctx context.Context, ref mail.MsgRef) ([]string, error) {
	var flags []string
	err := m.onMessage(ctx, "read flags", ref, func(c *imapclient.Client, uid imap.UIDSet) error {
		b, err := fetchOne(c, uid, &imap.FetchOptions{UID: true, Flags: true})
		if err != nil {
			return err
		}
		flags = flagStrings(b.Flags)
		return nil
	})
	return flags, err
}

// FindByMessageID searches the Message-ID header; angle brackets are optional.
func (m *Mailbox) FindByMessageID(ctx context.Context, folder, messageID string) (mail.MsgRef, error) {
	messageID = strings.Trim(strings.TrimSpace(messageID), "<>")
	ref := mail.MsgRef{AccountID: m.cfg.AccountID, Folder: folder}
	err := m.do(ctx, fmt.Sprintf("search %q", folder), func(c *imapclient.Client) error {
		if messageID == "" {
			return fmt.Errorf("%w: empty message-id", mail.ErrNotFound)
		}
		if err := m.selectFolder(c, folder); err != nil {
			return err
		}
		crit := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: messageID}}}
		data, err := c.UIDSearch(crit, nil).Wait()
		if err != nil {
			return err
		}
		uids := data.AllUIDs()
		if len(uids) == 0 {
			return fmt.Errorf("%w: message-id %q in %q", mail.ErrNotFound, messageID, folder)
		}
		ref.UIDValidity, ref.UID = m.selValidity, uint32(slices.Max(uids))
		return nil
	})
	if err != nil {
		return mail.MsgRef{}, err
	}
	return ref, nil
}

// SentRecipients peeks at the To and Cc headers of the newest limit messages. The folder is
// opened read-only (EXAMINE), so nothing in it can change.
func (m *Mailbox) SentRecipients(ctx context.Context, folder string, limit int) ([]mail.Recipient, error) {
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"To", "Cc"}, Peek: true}
	latest := map[string]time.Time{}
	err := m.do(ctx, fmt.Sprintf("scan %q", folder), func(c *imapclient.Client) error {
		m.selName = "" // the read-only selection must not be reused by a later write
		data, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			var ie *imap.Error
			if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo && kindOf(err) == nil {
				return fmt.Errorf("%w: %w", mail.ErrNoFolder, err)
			}
			return err
		}
		if data.NumMessages == 0 || limit <= 0 {
			return nil
		}
		first := uint32(1)
		if n := uint32(limit); data.NumMessages > n { // #nosec G115 -- limit is positive
			first = data.NumMessages - n + 1
		}
		var seqs imap.SeqSet
		seqs.AddRange(first, data.NumMessages)
		msgs, err := c.Fetch(seqs, &imap.FetchOptions{InternalDate: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
		if err != nil {
			return err
		}
		for _, b := range msgs {
			msg, err := netmail.ReadMessage(bytes.NewReader(b.FindBodySection(section)))
			if err != nil {
				continue // an unreadable header costs one message's contacts, not the scan
			}
			for _, key := range []string{"To", "Cc"} {
				addrs, _ := msg.Header.AddressList(key)
				for _, a := range addrs {
					addr := strings.ToLower(a.Address)
					if b.InternalDate.After(latest[addr]) || latest[addr].IsZero() {
						latest[addr] = b.InternalDate
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]mail.Recipient, 0, len(latest))
	for addr, at := range latest {
		out = append(out, mail.Recipient{Address: addr, SentAt: at})
	}
	slices.SortFunc(out, func(a, b mail.Recipient) int { return strings.Compare(a.Address, b.Address) })
	return out, nil
}
