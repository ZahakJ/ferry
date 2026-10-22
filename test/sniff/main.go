//go:build linux

// Command sniff watches every frame on an interface and counts how often
// marker strings appear in them. The harness uses it to show that nothing
// crossing the network, rendezvous included, carries plaintext.
//
//	sniff -i br0 -m SECRET1 -m SECRET2 -o result.json
//
// It stops on SIGTERM or SIGINT and writes its counts. Payloads are never
// stored.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type markers []string

func (m *markers) String() string     { return strings.Join(*m, ",") }
func (m *markers) Set(s string) error { *m = append(*m, s); return nil }

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func main() {
	iface := flag.String("i", "any", "interface, or any")
	outPath := flag.String("o", "", "write JSON result here")
	var ms markers
	flag.Var(&ms, "m", "marker to look for (repeatable)")
	flag.Parse()
	index := 0 // every interface
	if *iface != "any" {
		ifi, err := net.InterfaceByName(*iface)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		index = ifi.Index
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "socket:", err)
		os.Exit(1)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: htons(syscall.ETH_P_ALL), Ifindex: index}); err != nil {
		fmt.Fprintln(os.Stderr, "bind:", err)
		os.Exit(1)
	}
	syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 8<<20)
	counts := make(map[string]int, len(ms))
	var frames, total int64
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		buf := make([]byte, 1<<16)
		for {
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				continue
			}
			frames++
			total += int64(n)
			for _, m := range ms {
				counts[m] += bytes.Count(buf[:n], []byte(m))
			}
		}
	}()
	fmt.Fprintln(os.Stderr, "sniff: ready")
	<-done
	res := map[string]any{"interface": *iface, "frames": frames, "bytes": total, "matches": counts}
	b, _ := json.Marshal(res)
	if *outPath != "" {
		os.WriteFile(*outPath, append(b, '\n'), 0o644)
	} else {
		fmt.Println(string(b))
	}
}
