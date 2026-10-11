package publicnet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

func TestPublic(t *testing.T) {
	for _, tc := range []struct {
		ip     string
		public bool
	}{
		// Public, in IPv4, IPv6 and the IPv6 forms that carry a public IPv4 address.
		{"9.9.9.9", true},
		{"17.42.251.41", true}, // an iCloud IMAP server
		{"100.63.255.255", true},
		{"100.128.0.0", true},
		{"172.15.255.255", true},
		{"172.32.0.0", true},
		{"192.169.0.1", true},
		{"223.255.255.255", true},
		{"2620:fe::fe", true},
		{"2a00:1450:400c:c06::6c", true},
		{"::ffff:9.9.9.9", true},
		{"64:ff9b::909:909", true},
		{"2002:909:909::1", true},

		// Loopback.
		{"127.0.0.1", false},
		{"127.255.255.254", false},
		{"::1", false},
		{"::ffff:127.0.0.1", false},
		// Private: RFC 1918 and IPv6 unique local.
		{"10.0.0.1", false},
		{"10.255.255.255", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"192.168.1.10", false},
		{"fc00::1", false},
		{"fd12:3456:789a::1", false},
		{"::ffff:10.1.2.3", false},
		{"::ffff:192.168.0.1", false},
		// Link-local, the cloud metadata service among them.
		{"169.254.169.254", false},
		{"fe80::1", false},
		{"fe80::1%en0", false},
		{"::ffff:169.254.169.254", false},
		// Unspecified.
		{"0.0.0.0", false},
		{"::", false},
		{"::ffff:0.0.0.0", false},
		// Carrier-grade NAT.
		{"100.64.0.1", false},
		{"100.127.255.255", false},
		{"::ffff:100.64.0.1", false},
		// Multicast and broadcast.
		{"224.0.0.1", false},
		{"239.255.255.250", false},
		{"255.255.255.255", false},
		{"ff02::1", false},
		{"ff05::2", false},
		{"ff0e::1", false},
		// Other special-purpose ranges.
		{"0.1.2.3", false},
		{"192.0.0.8", false},
		{"192.0.2.1", false},
		{"198.18.0.1", false},
		{"198.51.100.1", false},
		{"203.0.113.1", false},
		{"240.0.0.1", false},
		{"::127.0.0.1", false},
		{"::ffff:0:a00:1", false},
		{"64:ff9b:1::1", false},
		{"100::1", false},
		{"2001::1", false},
		{"2001:db8::1", false},
		{"3fff::1", false},
		{"5f00::1", false},
		{"fec0::1", false},
		// IPv6 forms that carry a private IPv4 address.
		{"64:ff9b::7f00:1", false},
		{"64:ff9b::a9fe:a9fe", false},
		{"2002:7f00:1::1", false},
		{"2002:a00:1::1", false},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			if got := Public(netip.MustParseAddr(tc.ip)); got != tc.public {
				t.Errorf("Public(%s) = %v, want %v", tc.ip, got, tc.public)
			}
		})
	}
	if Public(netip.Addr{}) {
		t.Error("the zero address is public")
	}
}

// fakeResolver answers each name with its answers in turn, the last one again once they
// run out, so a name can resolve to one address when it is checked and another when it is
// dialed (DNS rebinding).
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]string
	asked   []string
}

func (f *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if network != "ip" {
		return nil, errors.New("unexpected network " + network)
	}
	f.asked = append(f.asked, host)
	answers := f.answers[host]
	if len(answers) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	answer := answers[0]
	if len(answers) > 1 {
		f.answers[host] = answers[1:]
	}
	var out []netip.Addr
	for _, a := range answer {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

func TestCheck(t *testing.T) {
	r := &fakeResolver{answers: map[string][][]string{
		"imap.example.test":     {{"9.9.9.9", "2620:fe::fe"}},
		"intranet.example.test": {{"10.0.0.5"}},
		"mixed.example.test":    {{"9.9.9.9", "192.168.1.1"}},
		"mapped.example.test":   {{"::ffff:127.0.0.1"}},
		"localhost":             {{"127.0.0.1", "::1"}},
	}}
	for _, tc := range []struct {
		host      string
		notPublic bool // refused as not public
		fails     bool // refused otherwise
	}{
		{"imap.example.test", false, false},
		{"9.9.9.9", false, false},
		{"2620:fe::fe", false, false},
		{"127.0.0.1", true, false},
		{"10.1.2.3", true, false},
		{"::1", true, false},
		{"fe80::1%en0", true, false},
		{"intranet.example.test", true, false},
		{"mixed.example.test", true, false}, // one private address is enough
		{"mapped.example.test", true, false},
		{"localhost", true, false},
		{"nowhere.example.test", false, true},
	} {
		t.Run(tc.host, func(t *testing.T) {
			err := Check(t.Context(), r, tc.host)
			if got := errors.Is(err, mail.ErrNotPublic); got != tc.notPublic {
				t.Errorf("Check = %v, not public %v, want %v", err, got, tc.notPublic)
			}
			if !tc.notPublic && (err != nil) != tc.fails {
				t.Errorf("Check = %v, want failure %v", err, tc.fails)
			}
		})
	}
	// An address is never looked up.
	if slices.ContainsFunc(r.asked, func(h string) bool { _, err := netip.ParseAddr(h); return err == nil }) {
		t.Errorf("addresses were looked up: %v", r.asked)
	}
}

// fakeConn is a connection that was made; nothing is read from or written to it.
type fakeConn struct{ net.Conn }

func (fakeConn) Close() error { return nil }

// The dial-time check holds when the name changes its answer between the check and the
// dial, and between one dial and the next: what is connected to is what was checked.
func TestDialerRebinding(t *testing.T) {
	r := &fakeResolver{answers: map[string][][]string{
		"rebind.example.test": {{"9.9.9.9"}, {"9.9.9.9"}, {"127.0.0.1"}, {"169.254.169.254"}, {"9.9.9.9", "10.0.0.1"}},
	}}
	var connected []string
	d := Dialer{Resolver: r, connect: func(_ context.Context, network, address string) (net.Conn, error) {
		connected = append(connected, network+" "+address)
		return fakeConn{}, nil
	}}
	ctx := t.Context()

	// The connection test checks the name: public.
	if err := Check(ctx, r, "rebind.example.test"); err != nil {
		t.Fatalf("Check = %v", err)
	}
	// The first dial is public too, and connects to the address it was told.
	if c, err := d.DialContext(ctx, "tcp", "rebind.example.test:993"); err != nil || c == nil {
		t.Fatalf("first dial = %v", err)
	}
	// Then the name points at the daemon's own machine, the metadata service, and a
	// mix: every later dial is refused without connecting anywhere.
	for i := range 3 {
		_, err := d.DialContext(ctx, "tcp", "rebind.example.test:993")
		if !errors.Is(err, mail.ErrNotPublic) {
			t.Errorf("dial %d after rebinding = %v, want ErrNotPublic", i+2, err)
		}
	}
	if want := []string{"tcp 9.9.9.9:993"}; !slices.Equal(connected, want) {
		t.Errorf("connected to %v, want %v", connected, want)
	}
	if len(r.asked) != 5 {
		t.Errorf("the name was looked up %d times, want once for the check and once per dial", len(r.asked))
	}

	// An address is refused without a lookup; a public one is connected to as it is.
	for _, addr := range []string{"127.0.0.1:993", "[::1]:993", "10.0.0.1:143", "[::ffff:192.168.0.1]:993", "[fe80::1%en0]:993"} {
		if _, err := d.DialContext(ctx, "tcp", addr); !errors.Is(err, mail.ErrNotPublic) {
			t.Errorf("dial %s = %v, want ErrNotPublic", addr, err)
		}
	}
	connected = nil
	if _, err := d.DialContext(ctx, "tcp", "[2620:fe::fe]:993"); err != nil || !slices.Equal(connected, []string{"tcp [2620:fe::fe]:993"}) {
		t.Errorf("dial a public IPv6 address = %v, connected %v", err, connected)
	}
	if _, err := d.DialContext(ctx, "tcp", "no-port"); err == nil {
		t.Error("an address without a port was dialed")
	}
}

// Each address of a name is tried in turn until one answers, with its share of the time.
func TestDialerTriesEachAddress(t *testing.T) {
	r := &fakeResolver{answers: map[string][][]string{"imap.example.test": {{"2620:fe::fe", "9.9.9.9"}}}}
	var tried []string
	refused := errors.New("refused")
	d := Dialer{Resolver: r, connect: func(ctx context.Context, _, address string) (net.Conn, error) {
		tried = append(tried, address)
		if address == "[2620:fe::fe]:993" {
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 6*time.Second {
				t.Errorf("the first of two addresses may take until %v", deadline)
			}
			return nil, refused
		}
		return fakeConn{}, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := d.DialContext(ctx, "tcp", "imap.example.test:993"); err != nil {
		t.Fatalf("dial = %v", err)
	}
	if want := []string{"[2620:fe::fe]:993", "9.9.9.9:993"}; !slices.Equal(tried, want) {
		t.Errorf("tried %v, want %v", tried, want)
	}

	// When none answers, the first failure is the answer.
	r.answers["down.example.test"] = [][]string{{"9.9.9.9", "1.1.1.1"}}
	d.connect = func(context.Context, string, string) (net.Conn, error) { return nil, refused }
	if _, err := d.DialContext(t.Context(), "tcp", "down.example.test:993"); !errors.Is(err, refused) {
		t.Errorf("dial = %v, want the connection's failure", err)
	}
}

// Control refuses on the socket itself, before it connects: nothing listening on a
// private address ever sees the connection, and the error is the same as for an address
// where nothing listens.
func TestControl(t *testing.T) {
	for _, tc := range []struct {
		address string
		ok      bool
	}{
		{"9.9.9.9:993", true},
		{"[2620:fe::fe]:993", true},
		{"127.0.0.1:993", false},
		{"[::1]:993", false},
		{"[::ffff:10.0.0.1]:993", false},
		{"169.254.169.254:80", false},
		{"[fe80::1%en0]:993", false},
		{"not an address", false},
	} {
		if err := Control("tcp", tc.address, nil); (err == nil) != tc.ok || (err != nil && !errors.Is(err, mail.ErrNotPublic)) {
			t.Errorf("Control(%s) = %v, want ok %v", tc.address, err, tc.ok)
		}
	}

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()
	listening := l.Addr().String()
	// A port where nothing listens: a second listener, closed.
	l2, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	silent := l2.Addr().String()
	_ = l2.Close()

	// The two refusals read the same but for the address: nothing says whether one answered.
	var msgs []string
	for _, addr := range []string{listening, silent} {
		_, err := (&net.Dialer{Control: Control}).DialContext(t.Context(), "tcp", addr)
		if !errors.Is(err, mail.ErrNotPublic) {
			t.Fatalf("dial %s through Control = %v, want ErrNotPublic", addr, err)
		}
		msgs = append(msgs, strings.ReplaceAll(err.Error(), addr, "ADDR"))
	}
	if msgs[0] != msgs[1] {
		t.Errorf("a listening address is refused with %q, a silent one with %q", msgs[0], msgs[1])
	}
	select {
	case <-accepted:
		t.Error("the private listener accepted a connection")
	case <-time.After(100 * time.Millisecond):
	}
}
