package link

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// A fixed port lets someone forward one UDP port on their router to this
// machine, for networks where hole punching fails. Every connection in the
// process shares one socket per interface on that port, and the router's
// public address is advertised alongside the local one.

var fixed struct {
	sync.Mutex
	port   int
	mux    ice.UDPMux
	public []string
}

// fixedPort returns the shared mux for port and the public IPv4 addresses
// to advertise with it. A port that cannot be bound gives a nil mux, and
// connections then use ephemeral ports as usual.
func (o *Options) fixedPort() (ice.UDPMux, []string) {
	fixed.Lock()
	defer fixed.Unlock()
	if fixed.port == o.Port {
		return fixed.mux, fixed.public
	}
	opts := []ice.UDPMuxFromPortOption{
		ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4, ice.NetworkTypeUDP6),
		ice.UDPMuxFromPortWithIPFilter(func(ip net.IP) bool { return !ip.IsLinkLocalUnicast() }),
		ice.UDPMuxFromPortWithReadBufferSize(sockBuf),
		ice.UDPMuxFromPortWithWriteBufferSize(sockBuf),
	}
	if o.Loopback {
		opts = append(opts, ice.UDPMuxFromPortWithLoopback(),
			ice.UDPMuxFromPortWithInterfaceFilter(func(n string) bool { return n == "lo" || n == "lo0" }))
	} else {
		opts = append(opts, ice.UDPMuxFromPortWithInterfaceFilter(usefulInterface))
	}
	mux, err := ice.NewMultiUDPMuxFromPort(o.Port, opts...)
	if err != nil {
		return nil, nil
	}
	fixed.port, fixed.mux, fixed.public = o.Port, mux, publicIPv4(o)
	return fixed.mux, fixed.public
}

// publicIPv4 asks STUN for this machine's public IPv4 address. IPv6
// addresses are global already and need no rewriting.
func publicIPv4(o *Options) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probe := Options{ICEServers: o.ICEServers, Loopback: o.Loopback}
	cands, _ := Gather(ctx, probe)
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		host, _, _ := net.SplitHostPort(c.Address)
		if ip := net.ParseIP(host); c.Type == "srflx" && ip.To4() != nil && !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	return out
}

func (o *Options) useFixedPort(s *webrtc.SettingEngine) error {
	mux, public := o.fixedPort()
	if mux == nil {
		return nil
	}
	s.SetICEUDPMux(mux)
	if len(public) == 0 {
		return nil
	}
	return s.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
		External:        public,
		AsCandidateType: webrtc.ICECandidateTypeHost,
		Mode:            webrtc.ICEAddressRewriteAppend,
		Networks:        []webrtc.NetworkType{webrtc.NetworkTypeUDP4},
	})
}

// FixedPort reports whether the configured port is bound and which public
// addresses are advertised with it.
func FixedPort(o Options) (bound bool, public []string) {
	mux, pub := o.fixedPort()
	return mux != nil, pub
}
