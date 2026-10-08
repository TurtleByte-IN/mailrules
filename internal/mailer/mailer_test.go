package mailer

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

// server is a small SMTP server for these tests: EHLO, STARTTLS, AUTH PLAIN and LOGIN,
// MAIL, RCPT, DATA, QUIT.
type server struct {
	t        *testing.T
	ln       net.Listener
	tls      *tls.Config
	implicit bool     // TLS from the first byte
	starttls bool     // offer STARTTLS
	auth     []string // mechanisms offered; nil = no AUTH
	user     string   // the one user and password it takes
	pass     string
	rejectTo string // RCPT of this address is refused

	mu       sync.Mutex
	messages []received
}

type received struct {
	from, to, data, user string
	tls                  bool
}

// selfSigned makes a certificate for 127.0.0.1 and localhost, and a pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func (s *server) start() string {
	s.t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(s.t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	s.ln = ln
	s.t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return ln.Addr().String()
}

func (s *server) serve(conn net.Conn) {
	defer conn.Close()
	secure := false
	if s.implicit {
		conn = tls.Server(conn, s.tls)
		secure = true
	}
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	say := func(line string) { _, _ = w.WriteString(line + "\r\n"); _ = w.Flush() }
	var msg received
	user := ""
	say("220 test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			lines := []string{"250-test"}
			if s.starttls && !secure {
				lines = append(lines, "250-STARTTLS")
			}
			if s.auth != nil {
				lines = append(lines, "250-AUTH "+strings.Join(s.auth, " "))
			}
			lines = append(lines, "250 8BITMIME")
			for _, l := range lines {
				_, _ = w.WriteString(l + "\r\n")
			}
			_ = w.Flush()
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, s.tls)
			if err := tc.HandshakeContext(context.Background()); err != nil {
				return
			}
			conn, secure = tc, true
			r, w = bufio.NewReader(conn), bufio.NewWriter(conn)
		case "AUTH":
			mech, initial, _ := strings.Cut(arg, " ")
			var u, p string
			switch strings.ToUpper(mech) {
			case "PLAIN":
				b, _ := base64.StdEncoding.DecodeString(initial)
				parts := strings.Split(string(b), "\x00")
				if len(parts) == 3 {
					u, p = parts[1], parts[2]
				}
			case "LOGIN":
				say("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
				l, _ := r.ReadString('\n')
				b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
				u = string(b)
				say("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
				l, _ = r.ReadString('\n')
				b, _ = base64.StdEncoding.DecodeString(strings.TrimSpace(l))
				p = string(b)
			}
			if u == s.user && p == s.pass {
				user = u
				say("235 ok")
			} else {
				say("535 5.7.8 bad credentials")
			}
		case "MAIL":
			msg = received{from: angle(arg), user: user, tls: secure}
			say("250 ok")
		case "RCPT":
			msg.to = angle(arg)
			if s.rejectTo != "" && msg.to == s.rejectTo {
				say("550 5.1.1 no such user")
				continue
			}
			say("250 ok")
		case "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			msg.data = b.String()
			s.mu.Lock()
			s.messages = append(s.messages, msg)
			s.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (s *server) got() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.messages...)
}

// angle is the address between < and > in a MAIL or RCPT argument.
func angle(arg string) string {
	_, rest, _ := strings.Cut(arg, "<")
	addr, _, _ := strings.Cut(rest, ">")
	return addr
}

var msg = Message{To: "Neha <neha@example.com>", Subject: "MailRules: 3 sorted, 1 to review · Tue", Text: "Sorted\n- Receipts: 3\n", HTML: "<p>Sorted</p>"}

func TestSend(t *testing.T) {
	cert, pool := selfSigned(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	trust := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	tests := []struct {
		name    string
		srv     *server
		client  SMTP
		wantErr error
		wantTLS bool
	}{
		{"starttls with PLAIN", &server{starttls: true, auth: []string{"PLAIN", "LOGIN"}, user: "me", pass: "pw"}, SMTP{TLS: StartTLS, Username: "me", Password: "pw"}, nil, true},
		{"implicit TLS with LOGIN only", &server{implicit: true, auth: []string{"LOGIN"}, user: "me", pass: "pw"}, SMTP{TLS: Implicit, Username: "me", Password: "pw"}, nil, true},
		{"no TLS to this machine, no sign-in", &server{}, SMTP{TLS: NoTLS}, nil, false},
		{"no TLS to this machine, PLAIN", &server{auth: []string{"PLAIN"}, user: "me", pass: "pw"}, SMTP{TLS: NoTLS, Username: "me", Password: "pw"}, nil, false},
		{"starttls not offered", &server{}, SMTP{TLS: StartTLS}, ErrUnsupported, false},
		{"wrong password", &server{starttls: true, auth: []string{"PLAIN"}, user: "me", pass: "pw"}, SMTP{TLS: StartTLS, Username: "me", Password: "nope"}, ErrAuth, false},
		{"sign-in not offered", &server{starttls: true}, SMTP{TLS: StartTLS, Username: "me", Password: "pw"}, ErrUnsupported, false},
		{"unknown sign-in method", &server{starttls: true, auth: []string{"CRAM-MD5"}}, SMTP{TLS: StartTLS, Username: "me", Password: "pw"}, ErrUnsupported, false},
		{"recipient refused", &server{starttls: true, rejectTo: "neha@example.com"}, SMTP{TLS: StartTLS}, ErrRefused, false},
		{"untrusted certificate", &server{implicit: true}, SMTP{TLS: Implicit, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, ErrTLS, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.srv.t, tt.srv.tls = t, tlsCfg
			c := tt.client
			c.Addr = tt.srv.start()
			c.From = "MailRules <mailrules@example.com>"
			if c.TLSConfig == nil {
				c.TLSConfig = trust
			}
			c.Now = func() time.Time { return time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC) }
			err := c.Send(context.Background(), msg)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Send() = %v, want %v", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "nope") {
					t.Errorf("the error %q holds the password", err)
				}
				if got := tt.srv.got(); len(got) != 0 {
					t.Errorf("a message got through: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := tt.srv.got()
			if len(got) != 1 {
				t.Fatalf("server got %d messages, want 1", len(got))
			}
			if got[0].from != "mailrules@example.com" || got[0].to != "neha@example.com" || got[0].tls != tt.wantTLS || got[0].user != c.Username {
				t.Errorf("envelope = %+v", got[0])
			}
		})
	}
}

func TestSendRefusesBadAddresses(t *testing.T) {
	for _, tt := range []struct{ from, to string }{
		{"mailrules@example.com", "neha@example.com\r\nBcc: all@example.com"},
		{"not an address", "neha@example.com"},
		{"mailrules@example.com", "a@example.com, b@example.com"},
	} {
		c := SMTP{Addr: "127.0.0.1:1", TLS: NoTLS, From: tt.from}
		if err := c.Send(context.Background(), Message{To: tt.to, Text: "x"}); !errors.Is(err, ErrAddress) {
			t.Errorf("From %q To %q: %v, want ErrAddress", tt.from, tt.to, err)
		}
	}
}

func TestSendUnreachable(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c := SMTP{Addr: addr, TLS: NoTLS, From: "m@example.com", Timeout: 2 * time.Second}
	if err := c.Send(context.Background(), msg); !errors.Is(err, ErrConnection) {
		t.Fatalf("Send() = %v, want ErrConnection", err)
	}
}

func TestSendStopsWithContext(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { // accepts and never greets
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c := SMTP{Addr: ln.Addr().String(), TLS: NoTLS, From: "m@example.com"}
	start := time.Now()
	if err := c.Send(ctx, msg); !errors.Is(err, ErrConnection) {
		t.Fatalf("Send() = %v, want ErrConnection", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Send did not stop with its context")
	}
}

func TestCompose(t *testing.T) {
	from, _ := mail.ParseAddress("MailRules <mailrules@example.com>")
	to, _ := mail.ParseAddress("neha@example.com")
	raw, err := Compose(from, to, Message{Subject: "MailRules: 3 sorted · Tue\r\nBcc: x@example.com", Text: "Hello ünïcode\nline two", HTML: "<p>Hi</p>"}, time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || subject != "MailRules: 3 sorted · Tue  Bcc: x@example.com" {
		t.Errorf("Subject = %q, %v: a line break must not start a header", subject, err)
	}
	if m.Header.Get("Bcc") != "" {
		t.Error("a Bcc header was smuggled in through the subject")
	}
	for k, want := range map[string]string{"From": `"MailRules" <mailrules@example.com>`, "To": "<neha@example.com>", "Auto-Submitted": "auto-generated", "Date": "Tue, 06 Oct 2026 08:00:00 +0000"} {
		if got := m.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.HasSuffix(m.Header.Get("Message-ID"), "@example.com>") {
		t.Errorf("Message-ID = %q", m.Header.Get("Message-ID"))
	}
	mediaType, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q", m.Header.Get("Content-Type"))
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var parts []string
	for {
		p, err := mr.NextPart() // decodes quoted-printable
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		parts = append(parts, p.Header.Get("Content-Type")+"|"+string(b))
	}
	want := []string{"text/plain; charset=utf-8|Hello ünïcode\r\nline two", "text/html; charset=utf-8|<p>Hi</p>"}
	if strings.Join(parts, "\n") != strings.Join(want, "\n") {
		t.Errorf("parts = %q, want %q", parts, want)
	}
}

func TestComposeTextOnly(t *testing.T) {
	from, _ := mail.ParseAddress("m@example.com")
	raw, err := Compose(from, from, Message{Subject: "s", Text: "just text"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if ct := m.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}
