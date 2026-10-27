// Package pipe carries one byte stream, such as an SSH connection, over a
// link's control channel, which is ordered and reliable like TCP.
//
// Each message starts with one byte: 'd' and data, or 'e' meaning the
// sender has no more to send (a half-close, so a command that reads all
// its input still gets an answer back).
package pipe

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/ZahakJ/ferry/internal/link"
)

const chunk = 64 << 10

// Pump copies r to the link and the link to w until both directions end
// or the link breaks. closeWrite, if set, half-closes w when the peer has
// nothing more to send. It returns the bytes sent and received.
func Pump(ctx context.Context, l *link.Link, r io.Reader, w io.Writer, closeWrite func()) (sent, got int64, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	up := make(chan error, 1)
	go func() {
		b := make([]byte, chunk+1)
		b[0] = 'd'
		for {
			n, rerr := r.Read(b[1:])
			if n > 0 {
				if werr := l.WriteCtl(ctx, b[:n+1]); werr != nil {
					up <- werr
					return
				}
				sent += int64(n)
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					rerr = l.WriteCtl(ctx, []byte{'e'})
				}
				up <- rerr
				return
			}
		}
	}()
	down := make(chan error, 1)
	go func() {
		b := make([]byte, link.MaxMessage)
		for {
			n, rerr := l.Ctl.Read(b)
			if rerr != nil {
				down <- rerr
				return
			}
			if n == 0 {
				continue
			}
			switch b[0] {
			case 'd':
				if _, werr := w.Write(b[1:n]); werr != nil {
					down <- werr
					return
				}
				got += int64(n - 1)
			case 'e':
				if closeWrite != nil {
					closeWrite()
				}
				down <- nil
				return
			default:
				down <- errors.New("pipe: unexpected message")
				return
			}
		}
	}()
	// Done when the peer has finished sending and we have too, or on the
	// first failure either way.
	var upDone, downDone bool
	for !(upDone && downDone) {
		select {
		case e := <-up:
			upDone = true
			if e != nil {
				return sent, got, e
			}
		case e := <-down:
			downDone = true
			if e != nil {
				return sent, got, e
			}
		case <-ctx.Done():
			return sent, got, ctx.Err()
		}
	}
	// Let the last bytes (and our 'e') reach the peer before the caller
	// closes the link; the peer may still be reading them.
	fctx, fcancel := context.WithTimeout(ctx, 10*time.Second)
	defer fcancel()
	l.FlushCtl(fctx)
	return sent, got, nil
}
