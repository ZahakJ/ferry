// Command spike measures pion data-channel throughput between two processes.
//
//	spike [flags]          sender; spawns "spike -recv" as the receiver
//
// SDP is exchanged over the child's stdin/stdout. Run inside a network
// namespace with netem on lo to shape the link.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

var (
	recv     = flag.Bool("recv", false, "act as receiver (internal)")
	total    = flag.Int64("bytes", 1<<30, "bytes to send")
	msgSize  = flag.Int("msg", 64<<10, "message size")
	pcs      = flag.Int("pcs", 1, "number of peer connections (SCTP associations)")
	chans    = flag.Int("chans", 1, "data channels per peer connection")
	hwm      = flag.Uint64("hwm", 4<<20, "per-channel buffered amount high-water mark")
	rbuf     = flag.Uint("rbuf", 0, "SCTP max receive buffer (0 = pion default)")
	zcsum    = flag.Bool("zerocsum", false, "enable SCTP zero checksum")
	unorder  = flag.Bool("unordered", false, "unordered (still reliable) channels")
	caStep   = flag.Uint("castep", 0, "SCTP congestion-avoidance cwnd step (0 = MTU)")
	minCwnd  = flag.Uint("mincwnd", 0, "SCTP minimum cwnd (0 = default)")
	fastRtx  = flag.Uint("fastrtx", 0, "SCTP fast retransmit window (0 = default)")
	sockBuf  = flag.Int("sockbuf", 0, "UDP socket read/write buffer (0 = kernel default)")
	timeout  = flag.Duration("timeout", 5*time.Minute, "give up after")
	jsonOut  = flag.Bool("json", false, "print result as JSON")
	trace    = flag.Bool("trace", false, "sample SCTP cwnd/rwnd/srtt to stderr")
	maxMsgSz = uint32(1 << 20)
)

func api() *webrtc.API {
	var s webrtc.SettingEngine
	s.SetIncludeLoopbackCandidate(true)
	s.SetInterfaceFilter(func(n string) bool { return n == "lo" })
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	s.SetICEMulticastDNSMode(0)
	s.SetSCTPMaxMessageSize(maxMsgSz)
	if *rbuf > 0 {
		s.SetSCTPMaxReceiveBufferSize(uint32(*rbuf))
	}
	if *caStep > 0 {
		s.SetSCTPCwndCAStep(uint32(*caStep))
	}
	if *minCwnd > 0 {
		s.SetSCTPMinCwnd(uint32(*minCwnd))
	}
	if *fastRtx > 0 {
		s.SetSCTPFastRtxWnd(uint32(*fastRtx))
	}
	s.EnableSCTPZeroChecksum(*zcsum)
	if *sockBuf > 0 {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		must(err)
		must(c.SetReadBuffer(*sockBuf))
		must(c.SetWriteBuffer(*sockBuf))
		lf := logging.NewDefaultLoggerFactory()
		s.SetICEUDPMux(webrtc.NewICEUDPMux(lf.NewLogger("mux"), c))
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(s))
}

func newPC() *webrtc.PeerConnection {
	pc, err := api().NewPeerConnection(webrtc.Configuration{})
	must(err)
	return pc
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "spike:", err)
		os.Exit(1)
	}
}

// localSDP waits for ICE gathering so the description carries all candidates.
func localSDP(pc *webrtc.PeerConnection, d webrtc.SessionDescription) webrtc.SessionDescription {
	done := webrtc.GatheringCompletePromise(pc)
	must(pc.SetLocalDescription(d))
	<-done
	return *pc.LocalDescription()
}

func main() {
	flag.Parse()
	*total -= *total % int64(*msgSize)
	if *proto != "webrtc" {
		streamMain()
		return
	}
	if *recv {
		receiver()
		return
	}
	sender()
}

func receiver() {
	var got, first atomic.Int64
	done := make(chan struct{})
	var once sync.Once
	in := json.NewDecoder(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	var conns []*webrtc.PeerConnection
	for i := 0; i < *pcs; i++ {
		pc := newPC()
		pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				first.CompareAndSwap(0, time.Now().UnixNano())
				if got.Add(int64(len(m.Data))) >= *total {
					once.Do(func() { close(done) })
				}
			})
		})
		var offer webrtc.SessionDescription
		must(in.Decode(&offer))
		must(pc.SetRemoteDescription(offer))
		ans, err := pc.CreateAnswer(nil)
		must(err)
		must(out.Encode(localSDP(pc, ans)))
		conns = append(conns, pc)
	}
	select {
	case <-done:
	case <-time.After(*timeout):
	}
	el := time.Since(time.Unix(0, first.Load())).Seconds()
	fmt.Printf("%d %f\n", got.Load(), el)
	for _, pc := range conns {
		pc.Close()
	}
}

func sender() {
	child := exec.Command(os.Args[0], append([]string{"-recv"}, os.Args[1:]...)...)
	child.Stderr = os.Stderr
	cin, err := child.StdinPipe()
	must(err)
	cout, err := child.StdoutPipe()
	must(err)
	must(child.Start())
	br := bufio.NewReader(cout)
	dec := json.NewDecoder(br)
	enc := json.NewEncoder(cin)

	ordered := !*unorder
	var dcs []*webrtc.DataChannel
	var conns []*webrtc.PeerConnection
	open := make(chan struct{}, *pcs**chans)
	for p := 0; p < *pcs; p++ {
		pc := newPC()
		for i := 0; i < *chans; i++ {
			dc, err := pc.CreateDataChannel(fmt.Sprint("d", i), &webrtc.DataChannelInit{Ordered: &ordered})
			must(err)
			dc.OnOpen(func() { open <- struct{}{} })
			dcs = append(dcs, dc)
		}
		off, err := pc.CreateOffer(nil)
		must(err)
		must(enc.Encode(localSDP(pc, off)))
		var ans webrtc.SessionDescription
		must(dec.Decode(&ans))
		must(pc.SetRemoteDescription(ans))
		conns = append(conns, pc)
	}
	for range dcs {
		<-open
	}

	buf := make([]byte, *msgSize)
	var sent atomic.Int64
	var wg sync.WaitGroup
	for _, dc := range dcs {
		wg.Add(1)
		low := make(chan struct{}, 1)
		dc.SetBufferedAmountLowThreshold(*hwm / 2)
		dc.OnBufferedAmountLow(func() {
			select {
			case low <- struct{}{}:
			default:
			}
		})
		go func(dc *webrtc.DataChannel) {
			defer wg.Done()
			for sent.Add(int64(len(buf))) <= *total {
				for dc.BufferedAmount() > *hwm {
					select {
					case <-low:
					case <-time.After(50 * time.Millisecond):
					}
				}
				if dc.Send(buf) != nil {
					return // receiver gave up; collect its result
				}
			}
		}(dc)
	}
	start := time.Now()
	if *trace {
		go traceStats(conns[0], start)
	}
	// Senders may still be blocked when the receiver times out, so wait for
	// its result rather than for them. The decoder may have buffered past the
	// last answer.
	var n int64
	var secs float64
	_, err = fmt.Fscan(io.MultiReader(dec.Buffered(), br), &n, &secs)
	must(err)
	child.Wait()
	gbps := float64(n) * 8 / secs / 1e9
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"bytes": n, "seconds": secs, "gbps": gbps, "msg": *msgSize, "pcs": *pcs, "chans": *chans,
			"hwm": *hwm, "rbuf": *rbuf, "zerocsum": *zcsum, "castep": *caStep, "sockbuf": *sockBuf,
			"wall": time.Since(start).Seconds(),
		})
		return
	}
	fmt.Printf("%7.1f Mbps  %.0f MB in %.2fs  pcs=%d chans=%d msg=%d hwm=%d rbuf=%d castep=%d sockbuf=%d\n",
		float64(n)*8/secs/1e6, float64(n)/1e6, secs, *pcs, *chans, *msgSize, *hwm, *rbuf, *caStep, *sockBuf)
	_ = gbps
}

func traceStats(pc *webrtc.PeerConnection, start time.Time) {
	for range time.Tick(500 * time.Millisecond) {
		for _, s := range pc.GetStats() {
			if t, ok := s.(webrtc.SCTPTransportStats); ok {
				fmt.Fprintf(os.Stderr, "t=%5.1fs cwnd=%8d rwnd=%8d srtt=%5.1fms unack=%5d sent=%dMB\n",
					time.Since(start).Seconds(), t.CongestionWindow, t.ReceiverWindow,
					t.SmoothedRoundTripTime*1000, t.UNACKData, t.BytesSent>>20)
			}
		}
	}
}
