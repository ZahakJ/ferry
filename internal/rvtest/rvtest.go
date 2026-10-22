// Package rvtest starts a throwaway Concord rendezvous for tests.
//
// Set FERRY_RV_BIN to a built cmd/rendezvous binary from the Concord repo;
// tests that need one are skipped otherwise.
package rvtest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Start runs a rendezvous on loopback and returns its multiaddr and the
// base URL of its HTTP gateway. env adds variables such as TURN settings.
func Start(t testing.TB, env ...string) (addr, httpURL string) {
	t.Helper()
	bin := os.Getenv("FERRY_RV_BIN")
	if bin == "" {
		t.Skip("FERRY_RV_BIN not set")
	}
	port, gport := freePort(t), freePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		fmt.Sprint("PORT=", port),
		fmt.Sprint("CONCORD_GUEST_PORT=", gport),
		"CONCORD_RELAY_SEED="+strings.Repeat("ab", 32),
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		want := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/", port)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); strings.HasPrefix(l, want) {
				select {
				case found <- l:
				default:
				}
			}
		}
	}()
	select {
	case addr = <-found:
	case <-time.After(20 * time.Second):
		t.Fatal("rendezvous did not start")
	}
	return addr, fmt.Sprintf("http://127.0.0.1:%d", gport)
}

func freePort(t testing.TB) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
