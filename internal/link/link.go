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
	// RelayOnly forces traffic through TURN (for testing the fallback).
	RelayOnly bool
	// Loopback allows 127.0.0.1 candidates (tests on one machine).
	Loopback bool
	Timeout  time.Duration
}

// Link is an open connection.
type Link struct {
	pcs []*webrtc.PeerConnection
	// Ctl is an ordered, reliable message channel for control.
	Ctl datachannel.ReadWriteCloser
	// Lanes are reliable, unordered message channels for bulk data.
	Lanes []*Lane
	// Path describes how traffic flows, e.g. "direct" or "relay (TURN)".
	Path string
}

// Lane is one bulk-data channel.
type Lane struct {
	DC  *webrtc.DataChannel
	RW  datachannel.ReadWriteCloser
	low chan struct{}
}

const highWater = 16 << 20

// Write sends one message, waiting while too much is already queued.
func (l *Lane) Write(ctx context.Context, b []byte) error {
	for l.DC.BufferedAmount() > highWater {
		select {
		case <-l.low:
		case <-ctx.Done():
			return ctx.Err()
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
		s.SetInterfaceFilter(func(n string) bool { return n == "lo" })
	} else if os.Getenv("FERRY_ALL_INTERFACES") == "" {
		s.SetInterfaceFilter(usefulInterface)
	}
	s.SetIPFilter(func(ip net.IP) bool { return !ip.IsLinkLocalUnicast() })
	s.SetICETimeouts(10*time.Second, 30*time.Second, 2*time.Second)
	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = logging.LogLevelError
	if os.Getenv("FERRY_DEBUG_ICE") != "" {
		lf.DefaultLogLevel = logging.LogLevelDebug
	}
	s.LoggerFactory = lf
	n, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	s.SetNet(bufNet{n})
	return webrtc.NewAPI(webrtc.WithSettingEngine(s)), nil
}

type signal struct {
	Lane int    `json:"lane"`
	Type string `json:"type"` // offer | answer | failed
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

	l := &Link{}
	cfg := webrtc.Configuration{ICEServers: o.ICEServers}
	if o.RelayOnly {
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
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
		lane := &Lane{DC: dc, low: make(chan struct{}, 1)}
		dc.SetBufferedAmountLowThreshold(highWater / 2)
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
		return nil, fmt.Errorf("could not connect (%s); run `ferry check` on both machines", strings.Join(errs, "; "))
	}
	l.Ctl = ctl
	for _, ln := range lanes {
		if ln != nil {
			l.Lanes = append(l.Lanes, ln)
		}
	}
	l.Path = path(l.pcs[0])
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
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: s.SDP}); err != nil {
				return err
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: s.SDP})
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
	if p.Local.Typ == webrtc.ICECandidateTypeRelay || p.Remote.Typ == webrtc.ICECandidateTypeRelay {
		return "relay (TURN)"
	}
	return "direct"
}

// Close tears everything down.
func (l *Link) Close() error {
	if l.Ctl != nil {
		l.Ctl.Close()
	}
	for _, pc := range l.pcs {
		pc.Close()
	}
	return nil
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
