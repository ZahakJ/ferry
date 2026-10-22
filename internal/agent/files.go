package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ZahakJ/ferry/internal/keys"
)

// fileLog lists files contacts have sent, one JSON line each.
type fileLog struct{ path string }

func (f *fileLog) with(write bool, fn func(*os.File) error) error {
	flags := os.O_RDONLY | os.O_CREATE
	if write {
		flags = os.O_RDWR | os.O_CREATE | os.O_APPEND
	}
	fh, err := os.OpenFile(f.path, flags, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	how := syscall.LOCK_SH
	if write {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(fh.Fd()), how); err != nil {
		return err
	}
	defer syscall.Flock(int(fh.Fd()), syscall.LOCK_UN)
	return fn(fh)
}

func readFiles(fh *os.File) []Received {
	fh.Seek(0, 0)
	var out []Received
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var r Received
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func (f *fileLog) add(r *Received) error {
	return f.with(true, func(fh *os.File) error {
		var last int64
		for _, x := range readFiles(fh) {
			last = max(last, x.Seq)
		}
		r.Seq = last + 1
		r.At = time.Now().UTC()
		b, _ := json.Marshal(r)
		_, err := fh.Write(append(b, '\n'))
		return err
	})
}

func (f *fileLog) since(seq int64) ([]Received, error) {
	out := []Received{}
	err := f.with(false, func(fh *os.File) error {
		for _, r := range readFiles(fh) {
			if r.Seq > seq {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

func peerOf(c keys.Contact) (peer.ID, error) { return peer.Decode(c.Key.Peer) }
