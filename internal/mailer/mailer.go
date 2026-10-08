// Package mailer sends email: the summary email. It is the only package that speaks SMTP.
// The in-memory fake for tests is in mailer/mailertest.
package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"slices"
	"strings"
	"time"
)

// Sentinel errors. SMTP wraps them, so callers test with errors.Is.
var (
	// ErrAuth means the mail server refused the user name or password.
	ErrAuth = errors.New("the mail server refused the user name or password")
	// ErrConnection means the mail server could not be reached, or the connection broke.
	ErrConnection = errors.New("could not reach the mail server")
	// ErrUnsupported means the server lacks what the settings require: STARTTLS, or a
	// sign-in method this package speaks.
	ErrUnsupported = errors.New("the mail server does not support what the settings require")
	// ErrTLS means the TLS handshake or the certificate check failed.
	ErrTLS = errors.New("the TLS handshake with the mail server failed")
	// ErrRefused means the server refused the sender, the recipient or the message.
	ErrRefused = errors.New("the mail server refused the email")
	// ErrAddress means an address in the message cannot be used.
	ErrAddress = errors.New("not a usable email address")
)

// Message is one email to one recipient. HTML is optional; Text is always sent.
type Message struct {
	To      string // one address, with or without a display name
	Subject string
	Text    string
	HTML    string
}

// Sender sends a message.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// TLS modes, as config.SMTPTLS spells them.
const (
	StartTLS = "starttls"
	Implicit = "implicit"
	NoTLS    = "none"
)

// SMTP sends through one mail server. Credentials are used only on the wire.
type SMTP struct {
	Addr     string // host:port
	TLS      string // StartTLS, Implicit or NoTLS
	Username string // empty = no sign-in
	Password string
	From     string // the From header: an address, or "Name <address>"

	// TLSConfig is copied for each connection, with ServerName set to the host when it is
	// empty; nil checks the server's certificate against the system's roots.
	TLSConfig *tls.Config
	Timeout   time.Duration    // the whole send; 0 = 30 seconds
	Now       func() time.Time // the Date header; nil = time.Now
}

var _ Sender = (*SMTP)(nil)

// Send delivers m: dial, TLS as configured, sign in when a user name is set, then one
// MAIL, RCPT and DATA.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	from, err := parseAddress(s.From)
	if err != nil {
		return fmt.Errorf("send email: From: %w", err)
	}
	to, err := parseAddress(m.To)
	if err != nil {
		return fmt.Errorf("send email: To: %w", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	data, err := Compose(from, to, m, now())
	if err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return fmt.Errorf("send email: server address %q: %w", s.Addr, err)
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := s.dial(ctx, host)
	if err != nil {
		return err
	}
	// The SMTP client has no context: a cancelled send closes the connection under it.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("send email: greeting: %w", wrap(ctx, err))
	}
	defer c.Close()
	if s.TLS == StartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("send email: the server does not offer STARTTLS; set MAILRULES_SMTP_TLS=implicit if it takes TLS from the start: %w", ErrUnsupported)
		}
		if err := c.StartTLS(s.tlsConfig(host)); err != nil {
			return fmt.Errorf("send email: STARTTLS: %w: %w", ErrTLS, err)
		}
	}
	if s.Username != "" {
		if err := s.auth(c, host); err != nil {
			return err
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("send email: sender: %w", wrap(ctx, err))
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("send email: recipient: %w", wrap(ctx, err))
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("send email: %w", wrap(ctx, err))
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("send email: %w", wrap(ctx, err))
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("send email: %w", wrap(ctx, err))
	}
	_ = c.Quit() // the server has the message; a failed goodbye changes nothing
	return nil
}

func (s *SMTP) tlsConfig(host string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.TLSConfig != nil {
		cfg = s.TLSConfig.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	return cfg
}

func (s *SMTP) dial(ctx context.Context, host string) (net.Conn, error) {
	d := &net.Dialer{}
	if s.TLS == Implicit {
		conn, err := (&tls.Dialer{NetDialer: d, Config: s.tlsConfig(host)}).DialContext(ctx, "tcp", s.Addr)
		if err != nil {
			var cert *tls.CertificateVerificationError
			var alert tls.AlertError
			if errors.As(err, &cert) || errors.As(err, &alert) || errors.As(err, new(tls.RecordHeaderError)) {
				return nil, fmt.Errorf("send email: %w: %w", ErrTLS, err)
			}
			return nil, fmt.Errorf("send email: %w: %w", ErrConnection, err)
		}
		return conn, nil
	}
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return nil, fmt.Errorf("send email: %w: %w", ErrConnection, err)
	}
	return conn, nil
}

// auth signs in with PLAIN, or LOGIN when the server offers only that.
func (s *SMTP) auth(c *smtp.Client, host string) error {
	ok, mechs := c.Extension("AUTH")
	if !ok {
		return fmt.Errorf("send email: the server does not take a sign-in; leave MAILRULES_SMTP_USER empty: %w", ErrUnsupported)
	}
	offered := strings.Fields(strings.ToUpper(mechs))
	var a smtp.Auth
	switch {
	case slices.Contains(offered, "PLAIN"):
		a = smtp.PlainAuth("", s.Username, s.Password, host)
	case slices.Contains(offered, "LOGIN"):
		a = loginAuth{s.Username, s.Password}
	default:
		return fmt.Errorf("send email: the server offers sign-in by %s only, and MailRules speaks PLAIN and LOGIN: %w", mechs, ErrUnsupported)
	}
	if err := c.Auth(a); err != nil {
		var tp *textproto.Error
		if errors.As(err, &tp) && (tp.Code == 535 || tp.Code == 534 || tp.Code == 530) {
			return fmt.Errorf("send email: %w (%d)", ErrAuth, tp.Code)
		}
		return fmt.Errorf("send email: sign-in: %w", err)
	}
	return nil
}

// wrap marks a server's refusal as ErrRefused and a broken connection as ErrConnection.
func wrap(ctx context.Context, err error) error {
	var tp *textproto.Error
	switch {
	case errors.As(err, &tp):
		return fmt.Errorf("%w: %d %s", ErrRefused, tp.Code, tp.Msg)
	case ctx.Err() != nil:
		return fmt.Errorf("%w: %w", ErrConnection, ctx.Err())
	default:
		return fmt.Errorf("%w: %w", ErrConnection, err)
	}
}

// loginAuth is the LOGIN mechanism: the user name, then the password, each asked for. Like
// smtp.PlainAuth it sends nothing over a connection that is not encrypted, except to this machine.
type loginAuth struct{ username, password string }

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !isLocal(server.Name) {
		return "", nil, errors.New("unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:", "user name:", "username":
		return []byte(a.username), nil
	case "password:", "password":
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN prompt")
}

func isLocal(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseAddress reads one address; no header can be smuggled in through it.
func parseAddress(s string) (*mail.Address, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, fmt.Errorf("%q: %w", s, ErrAddress)
	}
	a, err := mail.ParseAddress(s)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", s, ErrAddress)
	}
	return a, nil
}

// Compose writes m as an RFC 5322 message from one address to another: plain text, and
// with HTML a multipart/alternative of both, each quoted-printable in UTF-8.
func Compose(from, to *mail.Address, m Message, now time.Time) ([]byte, error) {
	var b bytes.Buffer
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := from.Address[strings.LastIndex(from.Address, "@")+1:]
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from.String())
	h("To", to.String())
	h("Subject", mime.QEncoding.Encode("utf-8", strings.NewReplacer("\r", " ", "\n", " ").Replace(m.Subject)))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated") // RFC 3834: no vacation replies to it
	h("X-Auto-Response-Suppress", "All")
	if m.HTML == "" {
		h("Content-Type", "text/plain; charset=utf-8")
		h("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		return b.Bytes(), writeQP(&b, m.Text)
	}
	mw := multipart.NewWriter(&b)
	h("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	b.WriteString("\r\n")
	for _, part := range []struct{ kind, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.kind + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQP(w, part.body); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeQP(w interface{ Write([]byte) (int, error) }, s string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(s)); err != nil {
		return err
	}
	return qp.Close()
}
