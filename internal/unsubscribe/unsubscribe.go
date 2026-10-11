// Package unsubscribe unsubscribes a person from a mailing list with the one-click web link
// the list's mail carries (RFC 8058): a List-Unsubscribe header that names an https URL,
// and a List-Unsubscribe-Post header of "List-Unsubscribe=One-Click". It is the only
// package that calls an unsubscribe link. MailRules never sends email on the person's
// behalf, so a list that offers only a mailto: address cannot be unsubscribed from here.
//
// The link comes from the email, so whoever sent it chose it. Client calls it only on the
// public internet (publicnet): a link that leads to the daemon's own machine or network is
// refused when it is dialed, on the address actually connected to. The fake for tests is in
// unsubscribe/unsubscribetest.
package unsubscribe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail/publicnet"
)

// Timeout bounds one unsubscribe request, connection and answer included.
const Timeout = 10 * time.Second

// maxURL is the longest link kept: a header that names a longer one offers none.
const maxURL = 2048

// body is what RFC 8058 says the POST carries.
const body = "List-Unsubscribe=One-Click"

// ErrNotOneClick means the link is not an RFC 8058 one-click link MailRules calls: not
// https, not on port 443, or not a URL at all. Nothing was sent.
var ErrNotOneClick = errors.New("not a one-click unsubscribe link")

// RefusedError is the answer of a site that did not take the unsubscribe: any status but
// 2xx, a redirect included (RFC 8058 has the site answer the POST itself).
type RefusedError struct {
	Host   string
	Status int
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("unsubscribe at %s: answered %d", e.Host, e.Status)
}

// Unsubscriber calls a one-click unsubscribe link. The daemon's is Client; tests use
// unsubscribetest.Fake.
type Unsubscriber interface {
	// Unsubscribe POSTs "List-Unsubscribe=One-Click" to link and returns nil when the site
	// answered 2xx. An error never contains the link, which often carries a token that
	// names the person; at most its host.
	Unsubscribe(ctx context.Context, link string) error
}

// OneClick returns the https link of an RFC 8058 one-click unsubscribe that headers
// (canonical keys, as message.Summary.Headers has them) offer, or "" when they offer none:
// no List-Unsubscribe, one that names only mailto: or http: links, or no
// List-Unsubscribe-Post of "List-Unsubscribe=One-Click".
func OneClick(headers map[string][]string) string {
	post, list := headers["List-Unsubscribe-Post"], headers["List-Unsubscribe"]
	if len(post) == 0 || len(list) == 0 || !strings.EqualFold(strings.TrimSpace(post[0]), body) {
		return ""
	}
	// RFC 2369: a comma-separated list of <URI>, in the order the list prefers them.
	for _, part := range strings.Split(list[0], ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "<") || !strings.HasSuffix(part, ">") {
			continue
		}
		link := strings.TrimSpace(part[1 : len(part)-1])
		if _, err := parse(link); err == nil {
			return link
		}
	}
	return ""
}

// Host returns the host name of a link, for logs and messages: never the whole link.
func Host(link string) string {
	if u, err := url.Parse(link); err == nil {
		return u.Hostname()
	}
	return ""
}

// parse accepts an https link on port 443, without a user name or password in it.
func parse(link string) (*url.URL, error) {
	if len(link) > maxURL {
		return nil, ErrNotOneClick
	}
	u, err := url.Parse(link)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Port() != "" && u.Port() != "443") {
		return nil, ErrNotOneClick
	}
	return u, nil
}

// Client is the daemon's Unsubscriber: an HTTP client that connects only to public
// addresses, sends no cookies, follows no redirect and gives up after Timeout.
type Client struct {
	http *http.Client
}

var _ Unsubscriber = (*Client)(nil)

// New returns the daemon's client. r looks up a link's host; nil is the system's resolver.
func New(r publicnet.Resolver) *Client {
	return newClient(publicnet.Dialer{Resolver: r}.DialContext, nil)
}

// newClient is New with the dialer and TLS settings given, for tests.
func newClient(dial func(ctx context.Context, network, address string) (net.Conn, error), tlsConfig *tls.Config) *Client {
	return &Client{http: &http.Client{
		Timeout: Timeout,
		Transport: &http.Transport{
			Proxy:               nil, // a proxy would be what is dialed, and checked, instead of the site
			DialContext:         dial,
			TLSClientConfig:     tlsConfig,
			TLSHandshakeTimeout: Timeout,
			DisableKeepAlives:   true, // one request per click; nothing to keep open
			ForceAttemptHTTP2:   true,
		},
		// RFC 8058 has the site answer the POST; a redirect could lead anywhere, plain http
		// included, so it is an answer that did not unsubscribe.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Unsubscribe POSTs the one-click body to link. No cookie, authorization or other context
// goes with it (RFC 8058).
func (c *Client) Unsubscribe(ctx context.Context, link string) error {
	u, err := parse(link)
	if err != nil {
		return err
	}
	host := u.Hostname()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("unsubscribe at %s: %w", host, ErrNotOneClick)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "MailRules (one-click unsubscribe, RFC 8058)")
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error prints the whole link; keep only what went wrong.
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("unsubscribe at %s: %w", host, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &RefusedError{Host: host, Status: resp.StatusCode}
	}
	return nil
}
