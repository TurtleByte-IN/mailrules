package unsubscribe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

func TestOneClick(t *testing.T) {
	const post = "List-Unsubscribe=One-Click"
	for _, tc := range []struct {
		name       string
		list, post []string
		want       string
	}{
		{name: "https and one-click post", list: []string{"<https://news.example/u/abc123>"}, post: []string{post},
			want: "https://news.example/u/abc123"},
		{name: "mailto first, then https", list: []string{"<mailto:u@news.example?subject=unsub>, <https://news.example/u?t=1>"}, post: []string{post},
			want: "https://news.example/u?t=1"},
		{name: "https first, then mailto", list: []string{" <https://news.example/u> , <mailto:u@news.example>"}, post: []string{" list-unsubscribe=one-click "},
			want: "https://news.example/u"},
		{name: "explicit port 443", list: []string{"<https://news.example:443/u>"}, post: []string{post}, want: "https://news.example:443/u"},
		{name: "mailto only", list: []string{"<mailto:u@news.example>"}, post: []string{post}},
		{name: "http only", list: []string{"<http://news.example/u>"}, post: []string{post}},
		{name: "https without the post header", list: []string{"<https://news.example/u>"}},
		{name: "https with another post value", list: []string{"<https://news.example/u>"}, post: []string{"List-Unsubscribe=Yes"}},
		{name: "post header without a link", post: []string{post}},
		{name: "another port", list: []string{"<https://news.example:8443/u>"}, post: []string{post}},
		{name: "user name in the link", list: []string{"<https://me:pw@news.example/u>"}, post: []string{post}},
		{name: "no angle brackets", list: []string{"https://news.example/u"}, post: []string{post}},
		{name: "empty brackets", list: []string{"<>"}, post: []string{post}},
		{name: "too long", list: []string{"<https://news.example/" + strings.Repeat("a", maxURL) + ">"}, post: []string{post}},
		{name: "nothing", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string][]string{}
			if tc.list != nil {
				h["List-Unsubscribe"] = tc.list
			}
			if tc.post != nil {
				h["List-Unsubscribe-Post"] = tc.post
			}
			if got := OneClick(h); got != tc.want {
				t.Errorf("OneClick = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHost(t *testing.T) {
	if got := Host("https://news.example:443/u/secret"); got != "news.example" {
		t.Errorf("Host = %q", got)
	}
	if got := Host("%zz"); got != "" {
		t.Errorf("Host of junk = %q", got)
	}
}

// site is an https server at example.com (the name in httptest's certificate) that the
// client reaches by dialing it directly: no guard, for the calls that should go through.
type site struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []*http.Request
	bodies []string
}

func newSite(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*site, *Client) {
	t.Helper()
	s := &site{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got, s.bodies = append(s.got, r), append(s.bodies, string(b))
		s.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(s.srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(s.srv.Certificate())
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, s.srv.Listener.Addr().String())
	}
	return s, newClient(dial, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
}

func TestUnsubscribePostsOneClick(t *testing.T) {
	s, c := newSite(t, func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "track", Value: "1"})
		w.WriteHeader(http.StatusAccepted)
	})
	for range 2 {
		if err := c.Unsubscribe(t.Context(), "https://example.com/u/secret-token?list=7"); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.got) != 2 {
		t.Fatalf("requests = %d", len(s.got))
	}
	for i, r := range s.got {
		if r.Method != http.MethodPost || r.URL.Path != "/u/secret-token" || r.URL.RawQuery != "list=7" {
			t.Errorf("request %d = %s %s", i, r.Method, r.URL)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", ct)
		}
		if s.bodies[i] != "List-Unsubscribe=One-Click" {
			t.Errorf("body = %q", s.bodies[i])
		}
		// RFC 8058: no cookies, authorization or other context, even the site's own cookie.
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("request %d sent context: %v", i, r.Header)
		}
	}
}

func TestUnsubscribeFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
		status int
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, 500},
		{"not found", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, 404},
		// A redirect is not followed, to plain http least of all: the site did not unsubscribe.
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.com/confirm", http.StatusFound)
		}, 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newSite(t, tc.answer)
			err := c.Unsubscribe(t.Context(), "https://example.com/u/secret-token")
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.Status != tc.status || refused.Host != "example.com" {
				t.Fatalf("err = %v, want refused with %d", err, tc.status)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Errorf("the error shows the link: %v", err)
			}
			if len(s.got) != 1 {
				t.Errorf("requests = %d, want 1 (no redirect followed)", len(s.got))
			}
		})
	}
}

func TestUnsubscribeRefusesLinksThatAreNotOneClick(t *testing.T) {
	dialed := false
	c := newClient(func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("dialed")
	}, nil)
	for _, link := range []string{"http://news.example/u", "https://news.example:8443/u", "mailto:u@news.example", "https://me:pw@news.example/u", "not a link"} {
		if err := c.Unsubscribe(t.Context(), link); !errors.Is(err, ErrNotOneClick) {
			t.Errorf("%s: err = %v, want ErrNotOneClick", link, err)
		}
	}
	if dialed {
		t.Error("a link that is not one-click was dialed")
	}
}

// resolver answers every name with the addresses set for it.
type resolver map[string][]netip.Addr

func (r resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return r[host], nil
}

// The daemon's client calls only the public internet. A link names a host the sender chose,
// so a name that resolves, when it is dialed, to the daemon's own machine or network is
// refused, whatever it said when the email arrived: the address checked is the one dialed.
func TestClientRefusesAddressesThatAreNotPublic(t *testing.T) {
	// Nothing is dialed: publicnet.Dialer resolves the name itself when it dials and refuses
	// before connecting, and its Control refuses again on the socket's own address.
	ip := func(s string) []netip.Addr { return []netip.Addr{netip.MustParseAddr(s)} }
	c := New(resolver{
		"localhost":           ip("127.0.0.1"),
		"intranet.example":    ip("10.1.2.3"),
		"home.example":        ip("192.168.1.10"),
		"metadata.example":    ip("169.254.169.254"),
		"ula.example":         ip("fd12:3456:789a::1"),
		"multicast.example":   ip("224.0.0.1"),
		"unspecified.example": ip("0.0.0.0"),
		"mapped.example":      ip("::ffff:127.0.0.1"),
		// One public address and one private: refused, never half-trusted.
		"mixed.example": {netip.MustParseAddr("9.9.9.9"), netip.MustParseAddr("127.0.0.1")},
	})
	for _, link := range []string{
		"https://127.0.0.1/u/secret-token",
		"https://[::1]/u/secret-token",
		"https://localhost/u/secret-token",
		"https://intranet.example/u/secret-token",
		"https://home.example/u/secret-token",
		"https://metadata.example/u/secret-token",
		"https://ula.example/u/secret-token",
		"https://multicast.example/u/secret-token",
		"https://unspecified.example/u/secret-token",
		"https://mapped.example/u/secret-token",
		"https://mixed.example/u/secret-token",
	} {
		err := c.Unsubscribe(t.Context(), link)
		if !errors.Is(err, mail.ErrNotPublic) {
			t.Errorf("%s: err = %v, want ErrNotPublic", link, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret-token") {
			t.Errorf("%s: the error shows the link: %v", link, err)
		}
	}
}
