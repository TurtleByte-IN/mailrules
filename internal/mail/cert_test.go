package mail

import (
	"errors"
	"strings"
	"testing"
)

func TestFingerprint(t *testing.T) {
	// SHA-256 of the empty string.
	const empty = "E3:B0:C4:42:98:FC:1C:14:9A:FB:F4:C8:99:6F:B9:24:27:AE:41:E4:64:9B:93:4C:A4:95:99:1B:78:52:B8:55"
	if got := Fingerprint(nil); got != empty {
		t.Fatalf("Fingerprint = %s", got)
	}
	for _, tc := range []struct {
		name, in, want string
		err            error
	}{
		{"as shown", empty, empty, nil},
		{"lower case without colons", strings.ToLower(strings.ReplaceAll(empty, ":", "")), empty, nil},
		{"spaces and padding", "  " + strings.ReplaceAll(empty, ":", " ") + "\n", empty, nil},
		{"too short", empty[:20], "", ErrFingerprint},
		{"not hex", strings.Replace(empty, "E3", "ZZ", 1), "", ErrFingerprint},
		{"empty", "", "", ErrFingerprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFingerprint(tc.in)
			if got != tc.want || !errors.Is(err, tc.err) {
				t.Errorf("ParseFingerprint(%q) = %q, %v; want %q, %v", tc.in, got, err, tc.want, tc.err)
			}
		})
	}
}

func TestCertError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *CertError
		want string
	}{
		{"untrusted", &CertError{Host: "127.0.0.1", Cert: Cert{Fingerprint: "AA:BB"}, Reason: "self-signed"},
			"the certificate of 127.0.0.1 is not trusted (self-signed): SHA-256 AA:BB"},
		{"changed", &CertError{Host: "127.0.0.1", Cert: Cert{Fingerprint: "AA:BB"}, Pinned: "CC:DD"},
			"the certificate of 127.0.0.1 changed: it is AA:BB, not the accepted CC:DD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Error() != tc.want {
				t.Errorf("Error() = %q", tc.err.Error())
			}
			if !errors.Is(tc.err, ErrTLS) {
				t.Error("a certificate error is not a TLS error")
			}
		})
	}
}
