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
	"math/big"
	"net"
	"sync"
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

	mu    sync.Mutex
	conns []net.Conn
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
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:   caps,
		Logger: log.New(io.Discard, "", 0),
	})
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
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
