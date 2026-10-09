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

	mu       sync.Mutex
	conns    []net.Conn
	noCreate atomic.Bool
	// special holds the folders made with CreateSpecial; once there is one, LIST is
	// answered here (see session.List).
	special map[string][]imap.MailboxAttr
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

// session is the in-memory server's session with CREATE behind RefuseCreate.
type session struct {
	imapserver.SessionIMAP4rev2
	s *Server
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
	cert, pool := selfSigned(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(Username, Password)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	var s *Server
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return session{mem.NewSession().(imapserver.SessionIMAP4rev2), s}, nil, nil
		},
		Caps:   caps,
		Logger: log.New(io.Discard, "", 0),
	})
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
	go func() {
		_ = srv.Serve(tracking{tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}), s})
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
