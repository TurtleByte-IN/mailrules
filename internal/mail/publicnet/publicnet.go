// Package publicnet keeps the hosted build's connections to mail servers on the public
// internet. A person names their own IMAP server; in MAILRULES_MODE=cloud that name must
// not lead the daemon into the operator's own network: its loopback, a private network, a
// cloud metadata service on a link-local address. Check refuses such a server before
// anything is dialed, and Dialer refuses at dial time, on the address actually connected
// to, so a name that resolves to a public address when it is checked and to a private one
// when it is dialed (DNS rebinding) is refused too. Both answer with an error that wraps
// mail.ErrNotPublic.
package publicnet

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// Resolver looks up a host name's addresses. *net.Resolver is one; tests fake it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// notPublic are the ranges, beyond what netip.Addr's own predicates name, of addresses
// that are not reachable on the public internet (the IANA special-purpose registries).
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (carrier-grade NAT)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (TEST-NET-1)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (TEST-NET-2)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (TEST-NET-3)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and the limited broadcast 255.255.255.255
	netip.MustParsePrefix("::/96"),           // IPv4-compatible (deprecated), with :: and ::1
	netip.MustParsePrefix("::ffff:0:0:0/96"), // IPv4-translated
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments: Teredo, benchmarking, ORCHID
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("3fff::/20"),       // documentation
	netip.MustParsePrefix("5f00::/16"),       // segment routing (SRv6) SIDs
	netip.MustParsePrefix("fec0::/10"),       // site-local (deprecated)
}

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96") // well-known NAT64: the last 4 bytes are the IPv4 address
	sixToFour = netip.MustParsePrefix("2002::/16")    // 6to4: bytes 2 to 5 are the IPv4 address
)

// Public reports whether ip is an address on the public internet: not loopback, private
// (RFC 1918, IPv6 unique local), link-local, unspecified, multicast or broadcast, not
// carrier-grade NAT, documentation or another special-purpose range, in IPv4, in IPv6 and
// in the IPv6 forms that carry an IPv4 address (IPv4-mapped, NAT64, 6to4), which are
// judged by the IPv4 address they carry.
func Public(ip netip.Addr) bool {
	ip = ip.WithZone("").Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	switch b := ip.As16(); {
	case nat64.Contains(ip):
		return Public(netip.AddrFrom4([4]byte(b[12:16])))
	case sixToFour.Contains(ip):
		return Public(netip.AddrFrom4([4]byte(b[2:6])))
	}
	for _, p := range notPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// Check refuses host, an IP address or a name, when it is or resolves to an address that is
// not public (an error that wraps mail.ErrNotPublic). A name that does not resolve is an
// error that does not wrap it. Nothing is dialed. r nil is the system's resolver.
func Check(ctx context.Context, r Resolver, host string) error {
	_, err := resolve(ctx, r, host)
	return err
}

// resolve returns host's addresses, every one of them public, or an error: one that wraps
// mail.ErrNotPublic when any of them is not.
func resolve(ctx context.Context, r Resolver, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !Public(ip) {
			return nil, fmt.Errorf("%s: %w", host, mail.ErrNotPublic)
		}
		return []netip.Addr{ip}, nil
	}
	if r == nil {
		r = net.DefaultResolver
	}
	ips, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("look up %s: no address", host)
	}
	for _, ip := range ips {
		if !Public(ip) {
			return nil, fmt.Errorf("%s is at %s: %w", host, ip, mail.ErrNotPublic)
		}
	}
	return ips, nil
}

// Dialer connects only to public addresses. It resolves the host itself, refuses it when
// any of its addresses is not public, as Check does, and then connects to those addresses
// one by one through a net.Dialer whose Control refuses, on the socket about to connect,
// an address that is not public. A name is resolved once per dial, and what is connected to
// is what was checked.
type Dialer struct {
	Resolver Resolver // nil: the system's resolver

	// connect dials one "ip:port"; nil is a net.Dialer with Control. Tests replace it.
	connect func(ctx context.Context, network, address string) (net.Conn, error)
}

// DialContext connects to address, "host:port", as net.Dialer.DialContext does, or fails
// before connecting with an error that wraps mail.ErrNotPublic.
func (d Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	ips, err := resolve(ctx, d.Resolver, host)
	if err != nil {
		return nil, err
	}
	connect := d.connect
	if connect == nil {
		connect = (&net.Dialer{Timeout: 30 * time.Second, Control: Control}).DialContext
	}
	var first error
	for i, ip := range ips {
		// Each address gets its share of the time left, as net.Dialer gives it, so one that
		// does not answer leaves time for the next.
		actx, cancel := ctx, context.CancelFunc(func() {})
		if deadline, ok := ctx.Deadline(); ok && len(ips)-i > 1 {
			actx, cancel = context.WithTimeout(ctx, max(time.Until(deadline)/time.Duration(len(ips)-i), 2*time.Second))
		}
		conn, err := connect(actx, network, net.JoinHostPort(ip.String(), port))
		cancel() // a connection made outlives its dial's context
		if err == nil {
			return conn, nil
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, first
}

// Control is a net.Dialer Control function that refuses to connect to an address that is
// not public. It runs on the address the socket is about to connect to, after any name
// was resolved, so no answer of a resolver gets past it. An address it cannot read is
// refused too.
func Control(_, address string, _ syscall.RawConn) error {
	if ap, err := netip.ParseAddrPort(address); err == nil && Public(ap.Addr()) {
		return nil
	}
	return fmt.Errorf("connect to %s: %w", address, mail.ErrNotPublic)
}
