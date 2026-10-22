package main

// Baselines over the same link: one TCP stream (kernel CUBIC) and one QUIC
// stream (quic-go), for comparison with SCTP over DTLS.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/quic-go/quic-go"
)

var proto = flag.String("proto", "webrtc", "webrtc, tcp or quic")

func streamMain() {
	if *recv {
		streamRecv()
		return
	}
	child := exec.Command(os.Args[0], append([]string{"-recv"}, os.Args[1:]...)...)
	child.Stderr = os.Stderr
	cout, err := child.StdoutPipe()
	must(err)
	must(child.Start())
	br := bufio.NewReader(cout)
	var addr string
	_, err = fmt.Fscanln(br, &addr)
	must(err)

	var w io.WriteCloser
	switch *proto {
	case "tcp":
		c, err := net.Dial("tcp", addr)
		must(err)
		w = c
	case "quic":
		conn, err := quic.DialAddr(context.Background(), addr,
			&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"spike"}}, quicConf())
		must(err)
		s, err := conn.OpenStreamSync(context.Background())
		must(err)
		w = s
	}
	buf := make([]byte, *msgSize)
	go func() {
		for sent := int64(0); sent < *total; sent += int64(len(buf)) {
			if _, err := w.Write(buf); err != nil {
				break
			}
		}
	}()
	var n int64
	var secs float64
	_, err = fmt.Fscan(br, &n, &secs)
	must(err)
	child.Wait()
	fmt.Printf("%7.1f Mbps  %.0f MB in %.2fs  proto=%s\n", float64(n)*8/secs/1e6, float64(n)/1e6, secs, *proto)
}

func quicConf() *quic.Config {
	return &quic.Config{
		MaxStreamReceiveWindow:     32 << 20,
		MaxConnectionReceiveWindow: 64 << 20,
	}
}

func streamRecv() {
	var r io.Reader
	switch *proto {
	case "tcp":
		l, err := net.Listen("tcp", "127.0.0.1:0")
		must(err)
		fmt.Println(l.Addr())
		c, err := l.Accept()
		must(err)
		r = c
	case "quic":
		l, err := quic.ListenAddr("127.0.0.1:0", selfSigned(), quicConf())
		must(err)
		fmt.Println(l.Addr())
		conn, err := l.Accept(context.Background())
		must(err)
		s, err := conn.AcceptStream(context.Background())
		must(err)
		r = s
	}
	buf := make([]byte, 1<<20)
	var got int64
	var start time.Time
	deadline := time.After(*timeout)
	done := make(chan struct{})
	go func() {
		for got < *total {
			n, err := r.Read(buf)
			if got == 0 && n > 0 {
				start = time.Now()
			}
			got += int64(n)
			if err != nil {
				break
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-deadline:
	}
	fmt.Printf("%d %f\n", got, time.Since(start).Seconds())
}

func selfSigned() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	must(err)
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	must(err)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{"spike"},
	}
}
