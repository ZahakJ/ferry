package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestBlockedAddr(t *testing.T) {
	own := []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8:1::5")}
	blocked := []string{
		"127.0.0.1", "127.8.9.10", "0.0.0.0", "0.1.2.3", "10.1.2.3", "172.16.0.1", "172.17.0.2", "172.31.255.255",
		"192.168.1.1", "169.254.169.254", "100.64.0.1", "100.127.255.254", "224.0.0.251", "239.1.2.3",
		"255.255.255.255", "240.0.0.1", "192.0.0.8", "198.18.0.1",
		"::", "::1", "fe80::1", "fe80::1%eth0", "fc00::1", "fd12:3456::1", "fec0::1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:192.168.1.1", "::ffff:169.254.169.254", "::ffff:100.64.0.1",
		"::127.0.0.1", "64:ff9b::7f00:1", "64:ff9b::c0a8:101", "64:ff9b:1::1",
		"203.0.113.7", "::ffff:203.0.113.7", "2001:db8:1::5", // this machine's own addresses
	}
	for _, s := range blocked {
		if !blockedAddr(netip.MustParseAddr(s), own) {
			t.Errorf("%s: allowed, want refused", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "172.32.0.1", "100.128.0.1", "2606:4700::1111",
		"::ffff:8.8.8.8", "64:ff9b::808:808", "203.0.113.8"}
	for _, s := range allowed {
		if blockedAddr(netip.MustParseAddr(s), own) {
			t.Errorf("%s: refused, want allowed", s)
		}
	}
	if !blockedAddr(netip.Addr{}, nil) {
		t.Error("the zero address is allowed")
	}
}

// testProxy is a boxProxy with a fake resolver and a dialer that records
// what it was asked to dial and connects "public" addresses to a local echo
// server instead.
type testProxy struct {
	*boxProxy
	mu     sync.Mutex
	dialed []string
}

func newTestProxy(t *testing.T, names map[string][]string) *testProxy {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	p, err := newBoxProxy()
	if err != nil {
		t.Fatal(err)
	}
	tp := &testProxy{boxProxy: p}
	p.own = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("203.0.113.7")} }
	calls := map[string]int{}
	p.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		calls[host]++
		var out []netip.Addr
		for _, s := range names[host] {
			out = append(out, netip.MustParseAddr(s))
		}
		// a rebinding name answers public first, then loopback; ferry must
		// ask once and dial the very address it checked
		if host == "rebind.test" && calls[host] > 1 {
			out = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
		}
		return out, nil
	}
	p.dial = func(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
		tp.mu.Lock()
		tp.dialed = append(tp.dialed, ap.String())
		tp.mu.Unlock()
		return net.Dial("tcp", echo.Addr().String())
	}
	go p.serve()
	t.Cleanup(p.close)
	return tp
}

// socksDial runs a SOCKS5 CONNECT (or another command) and returns the reply code.
func socksDial(t *testing.T, proxy string, cmd byte, host string, ip netip.Addr, port uint16) (net.Conn, byte) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{5, 1, 0})
	var g [2]byte
	if _, err := io.ReadFull(c, g[:]); err != nil || g != [2]byte{5, 0} {
		t.Fatalf("greeting: %v %v", g, err)
	}
	req := []byte{5, cmd, 0}
	switch {
	case host != "":
		req = append(append(req, 3, byte(len(host))), host...)
	case ip.Is4():
		b := ip.As4()
		req = append(append(req, 1), b[:]...)
	default:
		b := ip.As16()
		req = append(append(req, 4), b[:]...)
	}
	req = binary.BigEndian.AppendUint16(req, port)
	c.Write(req)
	var r [10]byte
	if _, err := io.ReadFull(c, r[:]); err != nil {
		t.Fatalf("reply: %v", err)
	}
	return c, r[1]
}

func TestProxyRefusesLocal(t *testing.T) {
	p := newTestProxy(t, map[string][]string{
		"localtest.me": {"127.0.0.1"},
		"lan.test":     {"192.168.1.10", "10.0.0.2"},
		"v6local.test": {"::1", "fd00::5"},
		"mapped.test":  {"::ffff:127.0.0.1"},
		"me.test":      {"203.0.113.7"},
		"example.test": {"93.184.216.34"},
		"mixed.test":   {"127.0.0.1", "10.1.1.1", "93.184.216.34"},
		"rebind.test":  {"93.184.216.34"},
		"nothing.test": {},
		"localhost":    {"::1", "127.0.0.1"},
	})
	addr := p.ln.Addr().String()
	for _, tc := range []struct {
		host string
		ip   string
		want byte
	}{
		{ip: "127.0.0.1", want: socksNotAllowed},
		{ip: "172.17.0.3", want: socksNotAllowed},
		{ip: "192.168.1.244", want: socksNotAllowed},
		{ip: "::1", want: socksNotAllowed},
		{ip: "::ffff:192.168.1.1", want: socksNotAllowed},
		{ip: "fe80::1", want: socksNotAllowed},
		{host: "localtest.me", want: socksNotAllowed},
		{host: "lan.test", want: socksNotAllowed},
		{host: "v6local.test", want: socksNotAllowed},
		{host: "mapped.test", want: socksNotAllowed},
		{host: "me.test", want: socksNotAllowed},
		{host: "127.0.0.1", want: socksNotAllowed}, // an address sent as a name
		{host: "localhost", want: socksNotAllowed},
		{host: "nothing.test", want: socksHostUnreach},
	} {
		var ip netip.Addr
		if tc.ip != "" {
			ip = netip.MustParseAddr(tc.ip)
		}
		c, code := socksDial(t, addr, 1, tc.host, ip, 443)
		c.Close()
		if code != tc.want {
			t.Errorf("%s%s: reply %d, want %d", tc.host, tc.ip, code, tc.want)
		}
	}
	if len(p.dialed) != 0 {
		t.Fatalf("dialed %v for refused destinations", p.dialed)
	}
	if n := p.refused.Load(); n != 13 {
		t.Errorf("refused count %d, want 13", n)
	}
}

func TestProxyConnectsPublic(t *testing.T) {
	p := newTestProxy(t, map[string][]string{
		"example.test": {"93.184.216.34"},
		"mixed.test":   {"127.0.0.1", "10.1.1.1", "2606:4700::1111"},
		"rebind.test":  {"93.184.216.34"},
	})
	addr := p.ln.Addr().String()
	for _, h := range []string{"example.test", "mixed.test", "rebind.test", ""} {
		var ip netip.Addr
		if h == "" {
			ip = netip.MustParseAddr("1.1.1.1")
		}
		c, code := socksDial(t, addr, 1, h, ip, 443)
		if code != socksOK {
			t.Fatalf("%s: reply %d", h, code)
		}
		c.Write([]byte("ping"))
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
			t.Fatalf("%s: echo %q %v", h, b, err)
		}
		c.Close()
	}
	// Each dial went to the IP that was checked, never a name or a local one.
	want := []string{"93.184.216.34:443", "[2606:4700::1111]:443", "93.184.216.34:443", "1.1.1.1:443"}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.dialed) != len(want) {
		t.Fatalf("dialed %v, want %v", p.dialed, want)
	}
	for i := range want {
		if p.dialed[i] != want[i] {
			t.Fatalf("dialed %v, want %v", p.dialed, want)
		}
	}
	if p.refused.Load() != 0 || p.conns.Load() != 4 {
		t.Errorf("refused %d conns %d", p.refused.Load(), p.conns.Load())
	}
}

func TestProxyOnlyConnect(t *testing.T) {
	p := newTestProxy(t, nil)
	for _, cmd := range []byte{2, 3} { // BIND, UDP ASSOCIATE
		c, code := socksDial(t, p.ln.Addr().String(), cmd, "", netip.MustParseAddr("1.1.1.1"), 53)
		c.Close()
		if code != socksCmdUnsupp {
			t.Errorf("command %d: reply %d", cmd, code)
		}
	}
	if len(p.dialed) != 0 {
		t.Fatalf("dialed %v", p.dialed)
	}
	// No "no auth" method offered: refused at the greeting.
	c, _ := net.Dial("tcp", p.ln.Addr().String())
	defer c.Close()
	c.Write([]byte{5, 1, 2})
	var g [2]byte
	io.ReadFull(c, g[:])
	if g != [2]byte{5, 0xff} {
		t.Errorf("greeting without no-auth: %v", g)
	}
}

// The real dialer re-checks the address it connects to.
func TestProxyDialerRechecks(t *testing.T) {
	p, err := newBoxProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if c, err := p.dial(context.Background(), netip.MustParseAddrPort(l.Addr().String())); err == nil {
		c.Close()
		t.Fatal("the dialer connected to loopback")
	}
}

func TestProxyCloseEndsConnections(t *testing.T) {
	p := newTestProxy(t, nil)
	c, code := socksDial(t, p.ln.Addr().String(), 1, "", netip.MustParseAddr("1.1.1.1"), 443)
	if code != socksOK {
		t.Fatal(code)
	}
	defer c.Close()
	done := make(chan struct{})
	go func() { p.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("close did not end an open connection")
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still open after close")
	}
}
