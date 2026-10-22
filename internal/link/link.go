// Package link opens a fast WebRTC connection between two peers that
// already share a sealed signaling channel.
//
// One SCTP association over a long, lossy path is slow to recover from
// loss, so a link is several peer connections ("lanes") used side by side.
// Lane 0 also carries a control channel. Offers and answers travel over
// the sealed channel, so the DTLS fingerprints in them are authenticated
// by the same keys as everything else: nobody can swap in their own.
package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"github.com/pion/webrtc/v4"

	"github.com/ZahakJ/ferry/internal/secure"
)

// Tuning measured in docs/throughput.md.
const (
	sctpRecvBuf = 4 << 20
	sockBuf     = 4 << 20
	caStep      = 256 << 10
	// MaxMessage is the largest data-channel message we send or accept.
	MaxMessage = 1 << 20
)

// Options configures a link.
type Options struct {
	Lanes      int
	ICEServers []webrtc.ICEServer
	// Loopback allows 127.0.0.1 candidates (tests on one machine).
	Loopback bool
	// Port, when set, is the one UDP port every connection uses, so it can
	// be forwarded on a router.
	Port    int
	Timeout time.Duration
}

// Link is an open connection.
type Link struct {
	pcs []*webrtc.PeerConnection
	// Ctl is an ordered, reliable message channel for control.
	Ctl datachannel.ReadWriteCloser
	// Lanes are reliable, unordered message channels for bulk data.
	Lanes []*Lane
	// Path describes how traffic flows: always "direct", as ferry never relays.
	Path string

	up        atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
}

// ErrClosed means the link was closed, often because the peer went away.
var ErrClosed = errors.New("connection closed")

// ErrNoPath means no direct connection could be made between the two
// machines: the networks between them block it.
var ErrNoPath = errors.New("no direct path between the two machines")

// Lane is one bulk-data channel.
type Lane struct {
	DC   *webrtc.DataChannel
	RW   datachannel.ReadWriteCloser
	low  chan struct{}
	done chan struct{}
	high uint64 // queue this much, then wait
}

// queueBudget is how much all lanes together may have queued and unsent.
// One lane gets up to 16 MiB, enough to keep a long path full; with many
// lanes each queues less, so a transfer holds at most about 64 MiB.
const queueBudget = 64 << 20

func highWater(lanes int) uint64 {
	return uint64(min(max(queueBudget/max(lanes, 1), 2<<20), 16<<20))
}

// Write sends one message, waiting while too much is already queued.
func (l *Lane) Write(ctx context.Context, b []byte) error {
	for l.DC.BufferedAmount() > l.high {
		select {
		case <-l.low:
		case <-ctx.Done():
			return ctx.Err()
		case <-l.done:
			return ErrClosed
		case <-time.After(100 * time.Millisecond):
		}
	}
	_, err := l.RW.Write(b)
	return err
}

func (o *Options) api() (*webrtc.API, error) {
	var s webrtc.SettingEngine
	s.DetachDataChannels()
	s.SetSCTPMaxReceiveBufferSize(sctpRecvBuf)
	s.SetSCTPMaxMessageSize(MaxMessage)
	s.SetSCTPCwndCAStep(caStep)
	s.EnableSCTPZeroChecksum(true)
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	if o.Loopback {
		s.SetIncludeLoopbackCandidate(true)
		s.SetInterfaceFilter(func(n string) bool { return n == "lo" || n == "lo0" })
	} else if os.Getenv("FERRY_ALL_INTERFACES") == "" {
		s.SetInterfaceFilter(usefulInterface)
	}
	s.SetIPFilter(func(ip net.IP) bool { return !ip.IsLinkLocalUnicast() })
	s.SetICETimeouts(10*time.Second, 30*time.Second, 2*time.Second)
	// NATs often drop the first probes of a hole punch, so keep checking
	// longer, and let a link move to a better path once one works (both ends are pion, which supports renomination).
	s.SetICEMaxBindingRequests(25)
	if err := s.SetICERenomination(); err != nil {
		return nil, err
	}
	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = logging.LogLevelError
	switch os.Getenv("FERRY_DEBUG_ICE") {
	case "":
	case "trace":
		lf.DefaultLogLevel = logging.LogLevelTrace
	default:
		lf.DefaultLogLevel = logging.LogLevelDebug
	}
	s.LoggerFactory = lf
	n, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	s.SetNet(bufNet{n})
	if o.Port > 0 {
		if err := o.useFixedPort(&s); err != nil {
			return nil, err
		}
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(s)), nil
}

type signal struct {
	Lane int    `json:"lane"`
	Type string `json:"type"` // lanes | offer | answer | failed
	SDP  string `json:"sdp,omitempty"`
	Err  string `json:"err,omitempty"`
}

// Open builds the link. Both sides call it; the initiator makes offers.
// sig must not be used by anyone else until Open returns.
func Open(ctx context.Context, sig *secure.Conn, initiator bool, o Options) (*Link, error) {
	if o.Lanes < 1 {
		o.Lanes = 1
	}
	if o.Timeout == 0 {
		o.Timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	// Each side says how many lanes it wants (the sender knows the size,
	// the receiver may not); the link uses the larger number.
	sent := make(chan error, 1)
	go func() { sent <- sig.SendJSON(signal{Type: "lanes", Lane: o.Lanes}) }()
	var ls signal
	if err := sig.RecvJSON(&ls); err != nil {
		return nil, err
	}
	if err := <-sent; err != nil {
		return nil, err
	}
	if ls.Type != "lanes" || ls.Lane < 1 || ls.Lane > 32 {
		return nil, errors.New("unexpected signaling message")
	}
	// A peer may ask for more lanes than we would, but not without bound.
	o.Lanes = max(o.Lanes, min(ls.Lane, 16))

	l := &Link{done: make(chan struct{})}
	cfg := webrtc.Configuration{ICEServers: o.ICEServers}
	type ready struct {
		lane int
		err  error
	}
	readyc := make(chan ready, 4*o.Lanes)
	report := func(r ready) {
		select {
		case readyc <- r:
		default:
		}
	}
	var ctl datachannel.ReadWriteCloser
	lanes := make([]*Lane, o.Lanes)

	for i := 0; i < o.Lanes; i++ {
		api, err := o.api()
		if err != nil {
			l.Close()
			return nil, err
		}
		pc, err := api.NewPeerConnection(cfg)
		if err != nil {
			l.Close()
			return nil, err
		}
		l.pcs = append(l.pcs, pc)
		i := i
		need := 1
		if i == 0 {
			need = 2
		}
		var mu sync.Mutex
		opened := 0
		done := func() {
			mu.Lock()
			opened++
			if opened == need {
				report(ready{lane: i})
			}
			mu.Unlock()
		}
		if i == 0 {
			yes, id := true, uint16(0)
			dc, err := pc.CreateDataChannel("ctl", &webrtc.DataChannelInit{Negotiated: &yes, ID: &id})
			if err != nil {
				l.Close()
				return nil, err
			}
			dc.OnOpen(func() {
				rw, err := dc.Detach()
				if err != nil {
					report(ready{lane: i, err: err})
					return
				}
				ctl = rw
				done()
			})
		}
		yes, no, id := true, false, uint16(1)
		dc, err := pc.CreateDataChannel("data", &webrtc.DataChannelInit{Negotiated: &yes, ID: &id, Ordered: &no})
		if err != nil {
			l.Close()
			return nil, err
		}
		lane := &Lane{DC: dc, low: make(chan struct{}, 1), done: l.done, high: highWater(o.Lanes)}
		dc.SetBufferedAmountLowThreshold(lane.high / 2)
		dc.OnBufferedAmountLow(func() {
			select {
			case lane.low <- struct{}{}:
			default:
			}
		})
		dc.OnOpen(func() {
			rw, err := dc.Detach()
			if err != nil {
				report(ready{lane: i, err: err})
				return
			}
			lane.RW = rw
			lanes[i] = lane
			done()
		})
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			if s == webrtc.PeerConnectionStateFailed {
				report(ready{lane: i, err: errors.New("connection failed")})
				// Once in use, a lane failing means the peer is gone or the
				// path broke for good: close everything so that blocked reads
				// and writes return and the transfer can stop (and resume).
				if l.up.Load() {
					go l.Close()
				}
			}
		})
	}

	if err := l.signal(ctx, sig, initiator); err != nil {
		l.Close()
		return nil, err
	}

	var errs []string
	for n := 0; n < o.Lanes; n++ {
		select {
		case r := <-readyc:
			if r.err != nil {
				errs = append(errs, fmt.Sprintf("lane %d: %v", r.lane, r.err))
			}
		case <-ctx.Done():
			n = o.Lanes
			errs = append(errs, "timed out")
		}
	}
	if ctl == nil || lanes[0] == nil {
		l.Close()
		return nil, fmt.Errorf("%w (%s)", ErrNoPath, strings.Join(errs, "; "))
	}
	l.Ctl = ctl
	for _, ln := range lanes {
		if ln != nil {
			l.Lanes = append(l.Lanes, ln)
		}
	}
	l.Path = path(l.pcs[0])
	l.up.Store(true)
	return l, nil
}

func (l *Link) signal(ctx context.Context, sig *secure.Conn, initiator bool) error {
	// Without trickle ICE each side waits for its candidates, then sends
	// one complete description per lane. Lanes gather in parallel, and
	// sending runs apart from receiving so neither side can block the other.
	describe := func(pc *webrtc.PeerConnection, d webrtc.SessionDescription) (string, error) {
		gathered := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(d); err != nil {
			return "", err
		}
		select {
		case <-gathered:
		case <-ctx.Done():
			return "", errors.New("timed out gathering network addresses")
		}
		return pc.LocalDescription().SDP, nil
	}
	errc := make(chan error, 2*len(l.pcs))
	var wg sync.WaitGroup
	if initiator {
		for i, pc := range l.pcs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				off, err := pc.CreateOffer(nil)
				if err == nil {
					var sdp string
					if sdp, err = describe(pc, off); err == nil {
						err = sig.SendJSON(signal{Lane: i, Type: "offer", SDP: sdp})
					}
				}
				if err != nil {
					errc <- err
				}
			}()
		}
	}
	for range l.pcs {
		var s signal
		if err := sig.RecvJSON(&s); err != nil {
			return err
		}
		if s.Type == "failed" {
			return fmt.Errorf("the other side could not set up the connection: %s", s.Err)
		}
		want := "offer"
		if initiator {
			want = "answer"
		}
		if s.Type != want || s.Lane < 0 || s.Lane >= len(l.pcs) {
			return errors.New("unexpected signaling message (do both sides use the same number of lanes?)")
		}
		pc := l.pcs[s.Lane]
		if initiator {
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: directOnly(s.SDP)}); err != nil {
				return err
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: directOnly(s.SDP)})
			var ans webrtc.SessionDescription
			if err == nil {
				ans, err = pc.CreateAnswer(nil)
			}
			var sdp string
			if err == nil {
				sdp, err = describe(pc, ans)
			}
			if err != nil {
				sig.SendJSON(signal{Lane: s.Lane, Type: "failed", Err: err.Error()})
				errc <- err
				return
			}
			if err := sig.SendJSON(signal{Lane: s.Lane, Type: "answer", SDP: sdp}); err != nil {
				errc <- err
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errc:
		return err
	default:
		return nil
	}
}

func path(pc *webrtc.PeerConnection) string {
	sctp := pc.SCTP()
	if sctp == nil {
		return "unknown"
	}
	p, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || p == nil {
		return "unknown"
	}
	return "direct"
}

// Close tears everything down. It is safe to call more than once.
func (l *Link) Close() error {
	l.closeOnce.Do(l.close)
	return nil
}

func (l *Link) close() {
	close(l.done)
	if l.Ctl != nil {
		l.Ctl.Close()
	}
	for _, pc := range l.pcs {
		pc.Close()
	}
}

// SendCtl sends one JSON control message.
func (l *Link) SendCtl(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = l.Ctl.Write(b)
	return err
}

// virtualPrefixes name interfaces that only reach containers and VMs on
// this machine. Offering them makes ICE try hundreds of hopeless pairs
// before it finds the real one.
var virtualPrefixes = []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "cni", "flannel", "kube", "lxc", "lxd", "podman", "cali", "vnet"}

func usefulInterface(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// bufNet gives every UDP socket pion opens bigger buffers. They absorb
// the bursts that otherwise cost a retransmission timeout. The kernel caps
// them at net.core.rmem_max / wmem_max.
type bufNet struct{ transport.Net }

type buffered interface {
	SetReadBuffer(int) error
	SetWriteBuffer(int) error
}

func grow(c any) {
	if b, ok := c.(buffered); ok {
		b.SetReadBuffer(sockBuf)
		b.SetWriteBuffer(sockBuf)
	}
}

func (n bufNet) ListenUDP(network string, addr *net.UDPAddr) (transport.UDPConn, error) {
	c, err := n.Net.ListenUDP(network, addr)
	if err == nil {
		grow(c)
	}
	return c, err
}

func (n bufNet) ListenPacket(network, address string) (net.PacketConn, error) {
	c, err := n.Net.ListenPacket(network, address)
	if err == nil {
		grow(c)
	}
	return c, err
}

// Candidate is one way this machine could be reached.
type Candidate struct {
	Type    string `json:"type"` // host | srflx
	Address string `json:"address"`
}

// Gather lists the candidates ICE finds with these options: local
// addresses and the public address STUN reports.
func Gather(ctx context.Context, o Options) ([]Candidate, error) {
	api, err := o.api()
	if err != nil {
		return nil, err
	}
	cfg := webrtc.Configuration{ICEServers: o.ICEServers}
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	var mu sync.Mutex
	var out []Candidate
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		mu.Lock()
		out = append(out, Candidate{Type: c.Typ.String(), Address: net.JoinHostPort(c.Address, fmt.Sprint(c.Port))})
		mu.Unlock()
	})
	if _, err := pc.CreateDataChannel("probe", nil); err != nil {
		return nil, err
	}
	off, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(off); err != nil {
		return nil, err
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	return out, nil
}

// directOnly drops relay candidates from a peer's description, so data
// never goes through a TURN server even if the peer offers one.
func directOnly(sdp string) string {
	lines := strings.SplitAfter(sdp, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "a=candidate:") && strings.Contains(l, " typ relay") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "")
}
