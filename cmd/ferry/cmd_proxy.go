package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The box's internet, through this machine: while `ferry shell` is open, the
// box's 127.0.0.1:1080 is forwarded (a plain ssh -R, never a dynamic one) to
// a SOCKS5 server ferry runs here on 127.0.0.1. It is the only thing that
// flows from the box to this machine, so it decides everything itself:
//
//   - TCP CONNECT only (no BIND, no UDP);
//   - it resolves names here and dials the IP it checked, never the name, so
//     a name that resolves to a public address once and a local one the next
//     time (DNS rebinding) cannot slip through;
//   - it refuses every destination that is not the public internet: loopback,
//     private, link-local, CGNAT, multicast, unspecified, IPv6 ULA and
//     link-local, IPv4 inside IPv6 (mapped, compatible, NAT64) for all of
//     those, and this machine's own interface addresses;
//   - the dialer checks the address again at connect time.

// blockedPrefixes are the destinations the box may never reach through us.
var blockedPrefixes = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",      // "this network", unspecified
		"10.0.0.0/8",     // private
		"100.64.0.0/10",  // CGNAT (Tailscale lives here)
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local, cloud metadata
		"172.16.0.0/12",  // private (Docker too)
		"192.0.0.0/24",   // IETF protocol assignments
		"192.168.0.0/16", // private
		"198.18.0.0/15",  // benchmarking, used inside some networks
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved, broadcast
		"::/128",         // unspecified
		"::1/128",        // loopback
		"64:ff9b:1::/48", // local-use NAT64
		"fc00::/7",       // ULA
		"fe80::/10",      // link-local
		"fec0::/10",      // old site-local
		"ff00::/8",       // multicast
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// embeddedV4 returns the IPv4 address inside an IPv6 one that carries it
// (mapped ::ffff:a.b.c.d, compatible ::a.b.c.d, NAT64 64:ff9b::a.b.c.d).
func embeddedV4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	if a.Is4In6() {
		return a.Unmap(), true
	}
	b := a.As16()
	compat := netip.PrefixFrom(netip.IPv6Unspecified(), 96)
	nat64 := netip.MustParsePrefix("64:ff9b::/96")
	if compat.Contains(a) || nat64.Contains(a) {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	return netip.Addr{}, false
}

// blockedAddr says whether the box must not reach a, given this machine's
// own addresses.
func blockedAddr(a netip.Addr, own []netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	a = a.WithZone("")
	if v4, ok := embeddedV4(a); ok {
		if blockedAddr(v4, own) {
			return true
		}
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	for _, o := range own {
		if o.Unmap() == a.Unmap() {
			return true
		}
	}
	return false
}

// ownAddrs lists this machine's interface addresses.
func ownAddrs() []netip.Addr {
	as, _ := net.InterfaceAddrs()
	out := make([]netip.Addr, 0, len(as))
	for _, a := range as {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out
}

// boxProxy is the SOCKS5 server the box's internet goes through.
type boxProxy struct {
	ln      net.Listener
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dial    func(ctx context.Context, ap netip.AddrPort) (net.Conn, error)
	own     func() []netip.Addr
	logf    func(format string, a ...any) // one line per refusal

	conns, refused atomic.Int64
	mu             sync.Mutex
	open           map[net.Conn]struct{}
	closed         bool
	wg             sync.WaitGroup
}

func newBoxProxy() (*boxProxy, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0") // loopback only: the ssh -R is its one client
	if err != nil {
		return nil, err
	}
	p := &boxProxy{ln: ln, own: ownAddrs, logf: func(string, ...any) {}, open: map[net.Conn]struct{}{}}
	p.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	p.dial = func(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
		d := net.Dialer{Timeout: 15 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			// the socket's real destination, checked once more right before connect
			got, err := netip.ParseAddrPort(address)
			if err != nil || blockedAddr(got.Addr(), p.own()) {
				return fmt.Errorf("refused %s", address)
			}
			return nil
		}}
		return d.DialContext(ctx, "tcp", ap.String())
	}
	return p, nil
}

func (p *boxProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

// serve accepts until close.
func (p *boxProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			c.Close()
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(c)
			p.handle(c)
		}()
	}
}

func (p *boxProxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.open[c] = struct{}{}
	return true
}

func (p *boxProxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.open, c)
	p.mu.Unlock()
	c.Close()
}

// close stops the proxy and every connection through it.
func (p *boxProxy) close() {
	p.mu.Lock()
	p.closed = true
	for c := range p.open {
		c.Close()
	}
	p.mu.Unlock()
	p.ln.Close()
	p.wg.Wait()
}

// SOCKS5 (RFC 1928) reply codes.
const (
	socksOK          = 0x00
	socksFail        = 0x01
	socksNotAllowed  = 0x02
	socksNetUnreach  = 0x03
	socksHostUnreach = 0x04
	socksRefused     = 0x05
	socksCmdUnsupp   = 0x07
	socksAddrUnsupp  = 0x08
)

func socksReply(w io.Writer, code byte) error {
	_, err := w.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

// handle serves one SOCKS5 client: greeting, one CONNECT, then bytes both ways.
func (p *boxProxy) handle(c net.Conn) {
	c.SetDeadline(time.Now().Add(20 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil || h[0] != 5 {
		return
	}
	methods := make([]byte, h[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	noAuth := false
	for _, m := range methods {
		noAuth = noAuth || m == 0
	}
	if !noAuth {
		c.Write([]byte{5, 0xff})
		return
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != 5 {
		return
	}
	var host string
	var lit netip.Addr
	switch req[3] {
	case 1:
		var b [4]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return
		}
		lit = netip.AddrFrom4(b)
	case 4:
		var b [16]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return
		}
		lit = netip.AddrFrom16(b)
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	default:
		socksReply(c, socksAddrUnsupp)
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(c, pb[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(pb[:])
	if req[1] != 1 { // only CONNECT: no BIND, no UDP ASSOCIATE
		p.refuse("%s command %d", dest(host, lit, port), req[1])
		socksReply(c, socksCmdUnsupp)
		return
	}
	up, code := p.connect(host, lit, port)
	if up == nil {
		socksReply(c, code)
		return
	}
	defer up.Close()
	p.conns.Add(1)
	if socksReply(c, socksOK) != nil {
		return
	}
	c.SetDeadline(time.Time{})
	p.mu.Lock()
	tracked := !p.closed
	if tracked {
		p.open[up] = struct{}{}
	}
	p.mu.Unlock()
	if !tracked {
		return
	}
	defer func() { p.mu.Lock(); delete(p.open, up); p.mu.Unlock() }()
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if t, ok := c.(interface{ CloseWrite() error }); ok {
		t.CloseWrite()
	} else {
		c.Close()
	}
}

func dest(host string, lit netip.Addr, port uint16) string {
	if host != "" {
		return net.JoinHostPort(host, strconv.Itoa(int(port)))
	}
	return netip.AddrPortFrom(lit, port).String()
}

func (p *boxProxy) refuse(format string, a ...any) {
	p.refused.Add(1)
	p.logf("refused "+format, a...)
}

// connect resolves (if needed), filters, and dials an allowed IP.
func (p *boxProxy) connect(host string, lit netip.Addr, port uint16) (net.Conn, byte) {
	if port == 0 {
		p.refuse("%s (port 0)", dest(host, lit, port))
		return nil, socksNotAllowed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var cands []netip.Addr
	if host == "" {
		cands = []netip.Addr{lit}
	} else if a, err := netip.ParseAddr(host); err == nil {
		cands = []netip.Addr{a} // an address sent as a name
	} else {
		as, err := p.resolve(ctx, host)
		if err != nil || len(as) == 0 {
			return nil, socksHostUnreach
		}
		cands = as
	}
	own := p.own()
	var ok []netip.Addr
	for _, a := range cands {
		if !blockedAddr(a, own) {
			ok = append(ok, a.Unmap())
		}
	}
	if len(ok) == 0 {
		p.refuse("%s (%s: not a public internet address)", dest(host, lit, port), cands[0])
		return nil, socksNotAllowed
	}
	var last error
	for _, a := range ok {
		up, err := p.dial(ctx, netip.AddrPortFrom(a, port))
		if err == nil {
			return up, socksOK
		}
		last = err
	}
	var se syscall.Errno
	switch {
	case errors.As(last, &se) && se == syscall.ECONNREFUSED:
		return nil, socksRefused
	case errors.As(last, &se) && se == syscall.ENETUNREACH:
		return nil, socksNetUnreach
	default:
		return nil, socksHostUnreach
	}
}
