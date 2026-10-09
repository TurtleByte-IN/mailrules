package mail

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Cert describes a mail server's certificate as a person checks it before accepting it.
type Cert struct {
	// Fingerprint is the SHA-256 of the certificate (its DER bytes) as upper-case hex pairs
	// separated by colons, the form browsers and `openssl x509 -fingerprint -sha256` show.
	Fingerprint string
	Subject     string
	Issuer      string
	NotBefore   time.Time
	NotAfter    time.Time
}

// CertOf describes c.
func CertOf(c *x509.Certificate) Cert {
	return Cert{Fingerprint: Fingerprint(c.Raw), Subject: c.Subject.String(), Issuer: c.Issuer.String(),
		NotBefore: c.NotBefore.UTC(), NotAfter: c.NotAfter.UTC()}
}

// Fingerprint returns the SHA-256 fingerprint of a DER certificate, as Cert.Fingerprint has it.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return pairs(sum[:])
}

// pairs writes b as upper-case hex pairs separated by colons.
func pairs(b []byte) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	var out strings.Builder
	out.Grow(len(h) + len(h)/2)
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			out.WriteByte(':')
		}
		out.WriteString(h[i : i+2])
	}
	return out.String()
}

// ErrFingerprint means a fingerprint is not 32 bytes of hex.
var ErrFingerprint = errors.New("a SHA-256 fingerprint is 64 hex digits, in pairs separated by colons or not")

// ParseFingerprint reads a SHA-256 fingerprint as people copy it: any case, with or without
// colons or spaces between the pairs. It returns it as Cert.Fingerprint has it.
func ParseFingerprint(s string) (string, error) {
	b, err := hex.DecodeString(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
	if err != nil || len(b) != sha256.Size {
		return "", ErrFingerprint
	}
	return pairs(b), nil
}

// CertError is a server certificate MailRules will not use as it is. With Pinned empty, the
// system does not trust it (Reason says why) and no certificate was accepted for the
// account; the person can accept this one by its fingerprint. With Pinned set, a
// certificate was accepted and the server now presents another. It wraps ErrTLS: retrying
// cannot fix it, only the person can.
type CertError struct {
	Host   string
	Cert   Cert   // what the server presented
	Pinned string // the fingerprint accepted for the account; empty when none was
	Reason string // why the system does not trust Cert; empty when Pinned is set
}

func (e *CertError) Error() string {
	if e.Pinned != "" {
		return fmt.Sprintf("the certificate of %s changed: it is %s, not the accepted %s", e.Host, e.Cert.Fingerprint, e.Pinned)
	}
	return fmt.Sprintf("the certificate of %s is not trusted (%s): SHA-256 %s", e.Host, e.Reason, e.Cert.Fingerprint)
}

func (e *CertError) Unwrap() error { return ErrTLS }
