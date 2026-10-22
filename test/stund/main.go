// Command stund answers STUN binding requests on UDP, so peers behind the
// harness NATs can learn their public address. It relays nothing.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/pion/turn/v4"
)

func main() {
	addr := flag.String("l", "0.0.0.0:3479", "listen address")
	flag.Parse()
	c, err := net.ListenPacket("udp4", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s, err := turn.NewServer(turn.ServerConfig{
		Realm:       "harness",
		AuthHandler: func(string, string, net.Addr) ([]byte, bool) { return nil, false },
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn:            c,
			RelayAddressGenerator: &turn.RelayAddressGeneratorNone{Address: "0.0.0.0"},
		}},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "stund: ready on", *addr)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	s.Close()
}
