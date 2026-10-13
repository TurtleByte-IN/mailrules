// Package imaptest runs go-imap's in-memory IMAP server over TLS for integration tests.
package imaptest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"maps"
	"math/big"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// The one account on the test server.
const (
	Username = "me@example.test"
	Password = "app-pass-wxyz-1234" // #nosec G101 -- exists only on the in-memory test server
)

// Server is a running in-memory IMAP server with one user and an INBOX.
type Server struct {
	Host string
	Port int
	TLS  *tls.Config         // client config that trusts the server's certificate
	User *imapmemserver.User // create folders and inspect state directly

	mu        sync.Mutex
	conns     []net.Conn
	imapConns []*imapserver.Conn
	// tokens are the OAuth access tokens AUTHENTICATE XOAUTH2 accepts (AllowToken), and
	// tokenLogins the ones it was given and accepted, in order.
	tokens      map[string]bool
	tokenLogins []string
	noCreate    atomic.Bool
	// special holds the folders made with CreateSpecial; once there is one, LIST is
	// answered here (see session.List).
	special map[string][]imap.MailboxAttr
	cert    atomic.Pointer[tls.Certificate] // what the server presents; see Rotate
	// password is the one LOGIN accepts once ChangePassword set it; nil = Password.
	password atomic.Pointer[string]
	delayNew atomic.Bool            // see DelayNewMail
	onIdle   atomic.Pointer[func()] // see OnIdle
}

// DelayNewMail makes the server behave as Dovecot does: a session that has a folder
// selected sees mail delivered after the SELECT only once a later command has completed
// (NOOP being the one meant for it). Until then the new UID does not exist for it: UID
// FETCH answers nothing, UID STORE, COPY and MOVE ignore it and still answer OK (RFC 9051
// §6.4.9 allows this of a UID that does not exist), and UID SEARCH leaves it out. The
// in-memory server otherwise shows every message at once.
func (s *Server) DelayNewMail(delay bool) { s.delayNew.Store(delay) }

// OnIdle has f run each time a session begins IDLE, before anything is announced to it,
// so mail a test delivers from f lands between the client's last command and its IDLE.
// nil removes it.
func (s *Server) OnIdle(f func()) { s.onIdle.Store(&f) }

// view is what one session has been told about its selected folder.
type view struct {
	mu       sync.Mutex
	selected string
	known    imap.UID // the highest UID announced to the session
}

// announce brings the session up to the folder's current highest UID.
func (x session) announce(folder string) {
	known := imap.UID(0)
	if folder != "" {
		if st, err := x.s.User.Status(folder, &imap.StatusOptions{UIDNext: true}); err == nil && st.UIDNext > 0 {
			known = st.UIDNext - 1
		}
	}
	x.v.mu.Lock()
	x.v.selected, x.v.known = folder, known
	x.v.mu.Unlock()
}

func (x session) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	data, err := x.SessionIMAP4rev2.Select(name, options)
	if err == nil {
		x.announce(name)
	}
	return data, err
}

func (x session) Unselect() error {
	x.announce("")
	return x.SessionIMAP4rev2.Unselect()
}

// Poll runs once each command completes: what arrived since is announced from here on.
func (x session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	err := x.SessionIMAP4rev2.Poll(w, allowExpunge)
	x.v.mu.Lock()
	folder := x.v.selected
	x.v.mu.Unlock()
	x.announce(folder)
	return err
}

// idleTick is how often an idling session is told what changed since it was last told.
const idleTick = 5 * time.Millisecond

// Idle announces what changed since the last command completed as soon as IDLE begins,
// then whatever changes while the session idles, as Dovecot does. go-imap's own Idle only
// announces what changes after it began listening: mail delivered between a client's
// last command and its IDLE went unannounced until the client next left IDLE, so a
// watcher that had just searched idled past it (MAI-241, MAI-245).
func (x session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if f := x.s.onIdle.Load(); f != nil && *f != nil {
		(*f)()
	}
	tick := time.NewTicker(idleTick)
	defer tick.Stop()
	for {
		if err := x.SessionIMAP4rev2.Poll(w, true); err != nil {
			return err
		}
		select {
		case <-stop:
			return nil
		case <-tick.C:
		}
	}
}

// announced narrows a UID set to the UIDs announced to the session when DelayNewMail is
// on; anything else comes back unchanged. An empty set means none is known.
func (x session) announced(numSet imap.NumSet) imap.NumSet {
	uids, ok := numSet.(imap.UIDSet)
	if !ok || !x.s.delayNew.Load() {
		return numSet
	}
	known := x.known()
	var seen imap.UIDSet
	for _, r := range uids {
		start, stop := r.Start, r.Stop
		if stop == 0 || stop > known {
			stop = known
		}
		if start == 0 {
			start = known
		}
		if start != 0 && start <= stop {
			seen.AddRange(start, stop)
		}
	}
	return seen
}

func (x session) known() imap.UID {
	x.v.mu.Lock()
	defer x.v.mu.Unlock()
	return x.v.known
}

func empty(numSet imap.NumSet) bool {
	uids, ok := numSet.(imap.UIDSet)
	return ok && len(uids) == 0
}

// Fetch leaves out UIDs not yet announced to the session when DelayNewMail is on.
func (x session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if numSet = x.announced(numSet); empty(numSet) {
		return nil
	}
	return x.SessionIMAP4rev2.Fetch(w, numSet, options)
}

// Store ignores UIDs not yet announced to the session when DelayNewMail is on, as it
// would UIDs that do not exist: no error, nothing changed.
func (x session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	if numSet = x.announced(numSet); empty(numSet) {
		return nil
	}
	return x.SessionIMAP4rev2.Store(w, numSet, flags, options)
}

// Copy copies nothing not yet announced to the session when DelayNewMail is on.
func (x session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	if numSet = x.announced(numSet); empty(numSet) {
		return nil, nil // OK without COPYUID: nothing was copied
	}
	return x.SessionIMAP4rev2.Copy(numSet, dest)
}

// Move moves nothing not yet announced to the session when DelayNewMail is on.
func (x session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	if numSet = x.announced(numSet); empty(numSet) {
		return nil
	}
	return x.SessionIMAP4rev2.Move(w, numSet, dest)
}

// Search finds nothing not yet announced to the session when DelayNewMail is on.
func (x session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if x.s.delayNew.Load() {
		known := x.known()
		if known == 0 {
			if kind == imapserver.NumKindUID {
				return &imap.SearchData{All: imap.UIDSet{}}, nil
			}
			return &imap.SearchData{All: imap.SeqSet{}}, nil
		}
		narrowed := *criteria
		narrowed.UID = append(slices.Clone(criteria.UID), imap.UIDSet{{Start: 1, Stop: known}})
		criteria = &narrowed
	}
	return x.SessionIMAP4rev2.Search(kind, criteria, options)
}

// Fingerprint is the SHA-256 fingerprint of the certificate the server presents now, as
// mail.Cert.Fingerprint has it.
func (s *Server) Fingerprint() string { return mail.Fingerprint(s.cert.Load().Certificate[0]) }

// Rotate makes the server present a new self-signed certificate from the next connection
// on, as a server whose certificate was replaced would. TLS keeps trusting only the old one.
func (s *Server) Rotate(t testing.TB) {
	t.Helper()
	cert, _ := selfSigned(t)
	s.cert.Store(&cert)
}

// CreateSpecial makes a folder that LIST reports with the given SPECIAL-USE attributes
// (RFC 6154), always, as Gmail does. The in-memory server cannot store them, so once any
// folder is made this way LIST shows only INBOX and folders made with CreateSpecial.
func (s *Server) CreateSpecial(t testing.TB, name string, attrs ...imap.MailboxAttr) {
	t.Helper()
	if err := s.User.Create(name, nil); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.special == nil {
		s.special = map[string][]imap.MailboxAttr{"INBOX": nil}
	}
	s.special[name] = attrs
}

// List answers from CreateSpecial's folders when there are any, adding their attributes.
func (x session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	x.s.mu.Lock()
	special := maps.Clone(x.s.special)
	x.s.mu.Unlock()
	if special == nil || len(patterns) == 0 {
		return x.SessionIMAP4rev2.List(w, ref, patterns, options)
	}
	for _, name := range slices.Sorted(maps.Keys(special)) {
		if !slices.ContainsFunc(patterns, func(p string) bool { return imapserver.MatchList(name, '/', ref, p) }) {
			continue
		}
		data := &imap.ListData{Mailbox: name, Delim: '/', Attrs: special[name]}
		if options.ReturnStatus != nil {
			st, err := x.s.User.Status(name, options.ReturnStatus)
			if err != nil {
				return err
			}
			data.Status = st
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

// RefuseCreate makes the server answer CREATE with NO, as a server that does not let a
// client make folders would, until it is called with false.
func (s *Server) RefuseCreate(refuse bool) { s.noCreate.Store(refuse) }

// ChangePassword makes LOGIN accept only p as the account's password from now on, as
// Proton Mail Bridge does when the account is signed in to it again; sessions already
// open stay. XOAUTH2 is not affected.
func (s *Server) ChangePassword(p string) { s.password.Store(&p) }

// Login checks the password ChangePassword set, when it set one.
func (x session) Login(username, password string) error {
	if p := x.s.password.Load(); p != nil {
		if password != *p {
			return imapserver.ErrAuthFailed
		}
		password = Password // what the in-memory user was made with
	}
	return x.SessionIMAP4rev2.Login(username, password)
}

// session is the in-memory server's session with CREATE behind RefuseCreate.
type session struct {
	imapserver.SessionIMAP4rev2
	s *Server
	v *view
}

// AllowToken makes AUTHENTICATE XOAUTH2 accept token for the one account, as a provider
// accepts an access token it issued, until ExpireSessions.
func (s *Server) AllowToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[string]bool{}
	}
	s.tokens[token] = true
}

// TokenLogins are the access tokens of the XOAUTH2 logins the server accepted, in order.
func (s *Server) TokenLogins() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tokenLogins)
}

// ExpireSessions does what Gmail and Outlook do when an access token expires, about an
// hour after it was issued: every token accepted so far stops working, and every open
// session is closed with "* BYE".
func (s *Server) ExpireSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = nil
	for _, c := range s.imapConns {
		_ = c.Bye("Session expired, please login again.")
	}
	s.imapConns = nil
}

// AuthenticateMechanisms offers XOAUTH2; LOGIN with the password works as before.
func (x session) AuthenticateMechanisms() []string { return []string{"XOAUTH2"} }

// Authenticate runs XOAUTH2 as Gmail does
// (https://developers.google.com/workspace/gmail/imap/xoauth2-protocol): a token it does
// not accept gets a challenge describing the error, then NO once the client answers it.
func (x session) Authenticate(mech string) (sasl.Server, error) {
	if mech != "XOAUTH2" {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "SASL mechanism not supported"}
	}
	return &xoauth2Server{x: x}, nil
}

type xoauth2Server struct {
	x       session
	refused bool
}

func (a *xoauth2Server) Next(resp []byte) ([]byte, bool, error) {
	if a.refused {
		return nil, false, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed, Text: "Invalid credentials (Failure)"}
	}
	if resp == nil {
		return []byte{}, false, nil // ask for the initial response
	}
	var user, token string
	for _, field := range bytes.Split(resp, []byte{1}) {
		if v, ok := bytes.CutPrefix(field, []byte("user=")); ok {
			user = string(v)
		} else if v, ok := bytes.CutPrefix(field, []byte("auth=Bearer ")); ok {
			token = string(v)
		}
	}
	s := a.x.s
	s.mu.Lock()
	ok := user == Username && s.tokens[token]
	if ok {
		s.tokenLogins = append(s.tokenLogins, token)
	}
	s.mu.Unlock()
	if !ok {
		a.refused = true
		return []byte(`{"status":"401","schemes":"bearer","scope":"https://mail.google.com/"}`), false, nil
	}
	return nil, true, a.x.SessionIMAP4rev2.Login(Username, Password)
}

func (x session) Create(name string, options *imap.CreateOptions) error {
	if x.s.noCreate.Load() {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Folders cannot be created here"}
	}
	return x.SessionIMAP4rev2.Create(name, options)
}

// tracking remembers accepted connections so a test can cut them.
type tracking struct {
	net.Listener
	s *Server
}

func (l tracking) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.s.mu.Lock()
		l.s.conns = append(l.s.conns, c)
		l.s.mu.Unlock()
	}
	return c, err
}

// Start serves until the test ends. caps is what the server advertises; nil means plain
// IMAP4rev1 (which still includes IDLE, but neither MOVE nor UIDPLUS).
func Start(t testing.TB, caps imap.CapSet) *Server {
	t.Helper()
	return start(t, caps, false)
}

// StartSTARTTLS is Start for a server that greets in the clear and offers STARTTLS, as
// Proton Mail Bridge does on its IMAP port; it refuses a login before TLS.
func StartSTARTTLS(t testing.TB, caps imap.CapSet) *Server {
	t.Helper()
	return start(t, caps, true)
}

func start(t testing.TB, caps imap.CapSet, starttls bool) *Server {
	t.Helper()
	cert, pool := selfSigned(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(Username, Password)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	var s *Server
	serve := &tls.Config{MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.cert.Load(), nil }}
	opts := &imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s.mu.Lock()
			s.imapConns = append(s.imapConns, c)
			s.mu.Unlock()
			return session{mem.NewSession().(imapserver.SessionIMAP4rev2), s, &view{}}, nil, nil
		},
		Caps:   caps,
		Logger: log.New(io.Discard, "", 0),
	}
	if starttls {
		opts.TLSConfig = serve
	}
	srv := imapserver.New(opts)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s = &Server{
		Host: "127.0.0.1",
		Port: ln.Addr().(*net.TCPAddr).Port,
		TLS:  &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		User: user,
	}
	s.cert.Store(&cert)
	if !starttls {
		ln = tls.NewListener(ln, serve)
	}
	go func() {
		_ = srv.Serve(tracking{ln, s})
	}()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

// KillConnections cuts every open connection, as a network drop would.
func (s *Server) KillConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

// Append delivers a message straight into a folder and returns its UID.
func (s *Server) Append(t testing.TB, folder, eml string, flags ...imap.Flag) uint32 {
	t.Helper()
	data, err := s.User.Append(folder, literal{bytes.NewReader([]byte(eml))}, &imap.AppendOptions{Flags: flags})
	if err != nil {
		t.Fatal(err)
	}
	return uint32(data.UID)
}

func selfSigned(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "imaptest"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}
