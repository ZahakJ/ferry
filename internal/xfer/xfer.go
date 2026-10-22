package xfer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"lukechampine.com/blake3"

	"github.com/ZahakJ/ferry/internal/link"
)

// Control messages, JSON on the link's control channel.
type ctl struct {
	T       string            `json:"t"`
	ID      string            `json:"id,omitempty"`
	Name    string            `json:"name,omitempty"`
	Total   int64             `json:"total,omitempty"`
	NFiles  int               `json:"nfiles,omitempty"`
	Skipped []string          `json:"skipped,omitempty"`
	Files   []File            `json:"files,omitempty"`
	Done    []int             `json:"done,omitempty"`
	Partial map[string]string `json:"partial,omitempty"` // file index -> base64 bitmap
	Idx     int               `json:"idx,omitempty"`
	Hash    string            `json:"hash,omitempty"`
	Bytes   int64             `json:"bytes,omitempty"`
	OK      bool              `json:"ok,omitempty"`
	Reason  string            `json:"reason,omitempty"`
	Saved   string            `json:"saved,omitempty"`
}

const (
	pieceHeader = 1 + 4 + 8 + 16
	filesBatch  = 2000
)

// Progress reports bytes the receiver holds out of the total.
type Progress func(done, total int64)

func readCtl(l *link.Link) (ctl, error) {
	buf := make([]byte, link.MaxMessage)
	n, err := l.Ctl.Read(buf)
	if err != nil {
		return ctl{}, err
	}
	var m ctl
	if err := json.Unmarshal(buf[:n], &m); err != nil {
		return ctl{}, fmt.Errorf("bad control message: %w", err)
	}
	return m, nil
}

func pieceHash(b []byte) []byte {
	h := blake3.New(16, nil)
	h.Write(b)
	return h.Sum(nil)
}

// ---- bitmap ----

type bitmap []uint64

func newBitmap(n int) bitmap { return make(bitmap, (n+63)/64) }

func (b bitmap) set(i int) { atomicOr(&b[i/64], 1<<(uint(i)%64)) }
func (b bitmap) has(i int) bool {
	return atomic.LoadUint64(&b[i/64])&(1<<(uint(i)%64)) != 0
}

func atomicOr(p *uint64, v uint64) {
	for {
		old := atomic.LoadUint64(p)
		if atomic.CompareAndSwapUint64(p, old, old|v) {
			return
		}
	}
}

func (b bitmap) snapshot() bitmap {
	out := make(bitmap, len(b))
	for i := range b {
		out[i] = atomic.LoadUint64(&b[i])
	}
	return out
}

func (b bitmap) encode() string {
	buf := make([]byte, 8*len(b))
	for i, w := range b {
		binary.LittleEndian.PutUint64(buf[8*i:], w)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func decodeBitmap(s string, n int) (bitmap, error) {
	buf, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(buf) != 8*((n+63)/64) {
		return nil, errors.New("bad bitmap")
	}
	b := newBitmap(n)
	for i := range b {
		b[i] = binary.LittleEndian.Uint64(buf[8*i:])
	}
	return b, nil
}

func (b bitmap) count(n int) int {
	c := 0
	for i := 0; i < n; i++ {
		if b.has(i) {
			c++
		}
	}
	return c
}

// ---- sender ----

// Send offers root (a file or directory) over l and streams it.
// It returns once the receiver confirms every file arrived intact.
func Send(ctx context.Context, l *link.Link, root string, m *Manifest, progress Progress) (saved string, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A transfer that fails leaves the link unusable; closing it unblocks
	// any write still waiting on a peer that has gone away.
	var ok atomic.Bool
	go func() {
		<-ctx.Done()
		if !ok.Load() {
			l.Close()
		}
	}()
	if err := l.SendCtl(ctl{T: "offer", ID: m.ID(), Name: m.Name, Total: m.Total, NFiles: len(m.Files), Skipped: m.Skipped}); err != nil {
		return "", err
	}
	for i := 0; i < len(m.Files); i += filesBatch {
		if err := l.SendCtl(ctl{T: "files", Files: m.Files[i:min(i+filesBatch, len(m.Files))]}); err != nil {
			return "", err
		}
	}
	reply, err := readCtl(l)
	if err != nil {
		return "", err
	}
	switch reply.T {
	case "accept":
	case "decline":
		return "", fmt.Errorf("the receiver declined: %s", reply.Reason)
	default:
		return "", fmt.Errorf("unexpected reply %q", reply.T)
	}
	done := map[int]bool{}
	for _, i := range reply.Done {
		done[i] = true
	}
	have := map[int]bitmap{}
	for k, v := range reply.Partial {
		var i int
		if _, err := fmt.Sscan(k, &i); err != nil || i < 0 || i >= len(m.Files) {
			return "", errors.New("receiver sent a bad resume map")
		}
		if have[i], err = decodeBitmap(v, m.Files[i].Pieces()); err != nil {
			return "", err
		}
	}

	// Control replies arrive while we stream.
	final := make(chan ctl, 1)
	ctlErr := make(chan error, 1)
	go func() {
		for {
			r, err := readCtl(l)
			if err != nil {
				ctlErr <- err
				cancel()
				return
			}
			switch r.T {
			case "progress":
				if progress != nil {
					progress(r.Bytes, m.Total)
				}
			case "complete", "cancel":
				final <- r
				return
			}
		}
	}()

	pieces := make(chan []byte, 4*len(l.Lanes))
	var pool = sync.Pool{New: func() any { b := make([]byte, pieceHeader+PieceSize); return &b }}
	errc := make(chan error, len(l.Lanes)+1)
	var lanes sync.WaitGroup
	for _, ln := range l.Lanes {
		lanes.Add(1)
		go func() {
			defer lanes.Done()
			for p := range pieces {
				if err := ln.Write(ctx, p); err != nil {
					errc <- err
					cancel()
					for range pieces {
					}
					return
				}
				b := p[:cap(p)]
				pool.Put(&b)
			}
		}()
	}

	readErr := func() error {
		defer close(pieces)
		for i, f := range m.Files {
			if f.Dir || done[i] {
				continue
			}
			p := root
			if f.Path != "" {
				p = filepath.Join(root, filepath.FromSlash(f.Path))
			}
			fh, err := os.Open(p)
			if err != nil {
				return err
			}
			whole := blake3.New(32, nil)
			hv := have[i]
			for n := 0; n < f.Pieces(); n++ {
				bp := pool.Get().(*[]byte)
				buf := *bp
				want := min(PieceSize, f.Size-int64(n)*PieceSize)
				if _, err := io.ReadFull(fh, buf[pieceHeader:pieceHeader+want]); err != nil {
					fh.Close()
					return fmt.Errorf("%s changed while sending: %w", p, err)
				}
				data := buf[pieceHeader : pieceHeader+want]
				whole.Write(data)
				if hv != nil && hv.has(n) {
					pool.Put(bp)
					continue
				}
				buf[0] = 1
				binary.BigEndian.PutUint32(buf[1:], uint32(i))
				binary.BigEndian.PutUint64(buf[5:], uint64(n)*PieceSize)
				copy(buf[13:29], pieceHash(data))
				select {
				case pieces <- buf[:pieceHeader+want]:
				case <-ctx.Done():
					fh.Close()
					return ctx.Err()
				}
			}
			fh.Close()
			if err := l.SendCtl(ctl{T: "end-file", Idx: i, Hash: hex.EncodeToString(whole.Sum(nil))}); err != nil {
				return err
			}
		}
		return l.SendCtl(ctl{T: "end"})
	}()
	lanes.Wait()
	if readErr != nil {
		return "", readErr
	}
	select {
	case err := <-errc:
		return "", fmt.Errorf("connection lost: %w", err)
	default:
	}
	select {
	case r := <-final:
		if r.T == "cancel" || !r.OK {
			return "", fmt.Errorf("the receiver reported a problem: %s", r.Reason)
		}
		ok.Store(true)
		return r.Saved, nil
	case err := <-ctlErr:
		return "", fmt.Errorf("connection lost before the receiver confirmed: %w", err)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ---- receiver ----

// Offer is what the sender proposes.
type Offer struct {
	Manifest *Manifest
	ID       string
}

// ReadOffer reads and checks the sender's manifest.
func ReadOffer(l *link.Link) (*Offer, error) {
	o, err := readCtl(l)
	if err != nil {
		return nil, err
	}
	if o.T != "offer" {
		return nil, fmt.Errorf("expected an offer, got %q", o.T)
	}
	if o.NFiles <= 0 || o.NFiles > 1_000_000 {
		return nil, errors.New("offer has an unreasonable file count")
	}
	m := &Manifest{Name: o.Name, Total: o.Total, Skipped: o.Skipped}
	for len(m.Files) < o.NFiles {
		f, err := readCtl(l)
		if err != nil {
			return nil, err
		}
		if f.T != "files" || len(f.Files) == 0 || len(m.Files)+len(f.Files) > o.NFiles {
			return nil, errors.New("malformed file list")
		}
		m.Files = append(m.Files, f.Files...)
	}
	if err := m.Check(); err != nil {
		return nil, err
	}
	if m.ID() != o.ID {
		return nil, errors.New("offer ID does not match its contents")
	}
	return &Offer{Manifest: m, ID: o.ID}, nil
}

// Decline refuses an offer.
func Decline(l *link.Link, reason string) error {
	return l.SendCtl(ctl{T: "decline", Reason: reason})
}

// state is what the receiver keeps on disk to resume.
type state struct {
	ID      string            `json:"id"`
	Root    string            `json:"root"` // chosen top-level name in dest
	Done    map[int]string    `json:"done"` // file index -> verified hash
	Partial map[string]string `json:"partial"`
}

func statePath(dest, id string) string { return filepath.Join(dest, ".ferry-"+id+".json") }

func loadState(dest, id string) *state {
	b, err := os.ReadFile(statePath(dest, id))
	if err != nil {
		return nil
	}
	var s state
	if json.Unmarshal(b, &s) != nil || s.ID != id {
		return nil
	}
	return &s
}

func (s *state) save(dest string) error {
	b, _ := json.Marshal(s)
	tmp := statePath(dest, s.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dest, s.ID))
}

// freeName picks name, or name-2, name-3..., that does not exist in dest.
func freeName(dest, name string) string {
	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]
	n := name
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dest, n)); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Lstat(filepath.Join(dest, n+".ferry-part")); errors.Is(err, os.ErrNotExist) {
				return n
			}
		}
		n = fmt.Sprintf("%s-%d%s", base, i, ext)
	}
}

type rfile struct {
	f       File
	part    string // path while incomplete
	final   string
	fh      *os.File
	have    bitmap
	arrived chan struct{} // poked when a piece lands
	hash    chan string   // sender's whole-file hash
	result  chan error
}

// Receive accepts an offer and writes it into dest. It resumes from any
// earlier attempt at the same transfer. It returns the saved path.
func Receive(ctx context.Context, l *link.Link, o *Offer, dest string, progress Progress) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := o.Manifest
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	st := loadState(dest, o.ID)
	if st == nil {
		st = &state{ID: o.ID, Root: freeName(dest, m.Name), Done: map[int]string{}, Partial: map[string]string{}}
	}
	if err := st.save(dest); err != nil {
		return "", fmt.Errorf("cannot write to %s: %w", dest, err)
	}
	single := len(m.Files) == 1 && m.Files[0].Path == ""
	rootPath := filepath.Join(dest, st.Root)

	files := make([]*rfile, len(m.Files))
	var held atomic.Int64
	reply := ctl{T: "accept", Partial: map[string]string{}}
	for i, f := range m.Files {
		final := rootPath
		if !single {
			final = filepath.Join(rootPath, filepath.FromSlash(f.Path))
		}
		rf := &rfile{f: f, final: final, part: final + ".ferry-part", hash: make(chan string, 1), result: make(chan error, 1), arrived: make(chan struct{}, 1)}
		files[i] = rf
		if f.Dir {
			if err := os.MkdirAll(final, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if _, ok := st.Done[i]; ok {
			if _, err := os.Stat(final); err == nil {
				reply.Done = append(reply.Done, i)
				held.Add(f.Size)
				continue
			}
			delete(st.Done, i)
		}
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			return "", err
		}
		rf.have = newBitmap(f.Pieces())
		if s, ok := st.Partial[fmt.Sprint(i)]; ok {
			if _, err := os.Stat(rf.part); err == nil {
				if b, err := decodeBitmap(s, f.Pieces()); err == nil {
					rf.have = b
					reply.Partial[fmt.Sprint(i)] = s
					for n := 0; n < f.Pieces(); n++ {
						if b.has(n) {
							held.Add(min(PieceSize, f.Size-int64(n)*PieceSize))
						}
					}
				}
			}
		}
		fh, err := os.OpenFile(rf.part, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return "", err
		}
		defer fh.Close()
		if err := fh.Truncate(f.Size); err != nil {
			return "", fmt.Errorf("not enough room for %s: %w", f.Path, err)
		}
		rf.fh = fh
	}
	if err := l.SendCtl(reply); err != nil {
		return "", err
	}

	errc := make(chan error, len(l.Lanes)+2)
	fail := func(err error) {
		select {
		case errc <- err:
		default:
		}
		cancel()
	}

	// Lanes: check and write pieces.
	for _, ln := range l.Lanes {
		go func() {
			buf := make([]byte, link.MaxMessage)
			for {
				n, err := ln.RW.Read(buf)
				if err != nil {
					if ctx.Err() == nil {
						fail(fmt.Errorf("connection lost: %w", err))
					}
					return
				}
				p := buf[:n]
				if n < pieceHeader || p[0] != 1 {
					fail(errors.New("malformed piece"))
					return
				}
				idx := int(binary.BigEndian.Uint32(p[1:]))
				off := int64(binary.BigEndian.Uint64(p[5:]))
				data := p[pieceHeader:]
				if idx >= len(files) || files[idx].fh == nil || off%PieceSize != 0 || off+int64(len(data)) > files[idx].f.Size ||
					int64(len(data)) != min(PieceSize, files[idx].f.Size-off) {
					fail(errors.New("piece does not fit the manifest"))
					return
				}
				if !bytes.Equal(pieceHash(data), p[13:29]) {
					fail(fmt.Errorf("piece of %s failed its integrity check", files[idx].f.Path))
					return
				}
				rf := files[idx]
				if _, err := rf.fh.WriteAt(data, off); err != nil {
					fail(fmt.Errorf("writing %s: %w", rf.part, err))
					return
				}
				if !rf.have.has(int(off / PieceSize)) {
					rf.have.set(int(off / PieceSize))
					held.Add(int64(len(data)))
				}
				select {
				case rf.arrived <- struct{}{}:
				default:
				}
			}
		}()
	}

	// Hash each file in order as its pieces land, reading back from disk
	// (usually the page cache), so memory stays flat however big it is.
	for _, rf := range files {
		if rf.fh == nil {
			continue
		}
		go func() {
			h := blake3.New(32, nil)
			buf := make([]byte, PieceSize)
			for n := 0; n < rf.f.Pieces(); n++ {
				for !rf.have.has(n) {
					select {
					case <-rf.arrived:
					case <-time.After(200 * time.Millisecond):
					case <-ctx.Done():
						return
					}
				}
				size := min(PieceSize, rf.f.Size-int64(n)*PieceSize)
				if _, err := rf.fh.ReadAt(buf[:size], int64(n)*PieceSize); err != nil {
					rf.result <- err
					return
				}
				h.Write(buf[:size])
			}
			got := hex.EncodeToString(h.Sum(nil))
			select {
			case want := <-rf.hash:
				if got != want {
					rf.result <- fmt.Errorf("%s does not match the sender's copy", rf.f.Path)
					return
				}
				rf.result <- nil
			case <-ctx.Done():
			}
		}()
	}

	// Control: whole-file hashes from the sender.
	go func() {
		for {
			c, err := readCtl(l)
			if err != nil {
				if ctx.Err() == nil {
					fail(fmt.Errorf("connection lost: %w", err))
				}
				return
			}
			switch c.T {
			case "end-file":
				if c.Idx < 0 || c.Idx >= len(files) || files[c.Idx].fh == nil {
					fail(errors.New("bad end-file"))
					return
				}
				select {
				case files[c.Idx].hash <- c.Hash:
				default:
				}
			case "cancel":
				fail(fmt.Errorf("the sender cancelled: %s", c.Reason))
				return
			}
		}
	}()

	// Checkpoint: sync data, then record what is safely on disk.
	checkpoint := func() {
		snaps := map[int]bitmap{}
		for i, rf := range files {
			if rf.fh != nil {
				snaps[i] = rf.have.snapshot()
			}
		}
		for i, rf := range files {
			if rf.fh != nil {
				if rf.fh.Sync() == nil {
					st.Partial[fmt.Sprint(i)] = snaps[i].encode()
				}
			}
		}
		st.save(dest)
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	lastCP := time.Now()

	pending := 0
	for _, rf := range files {
		if rf.fh != nil {
			pending++
		}
	}
	results := make(chan struct {
		i   int
		err error
	}, len(files))
	for i, rf := range files {
		if rf.fh == nil {
			continue
		}
		go func() {
			select {
			case err := <-rf.result:
				results <- struct {
					i   int
					err error
				}{i, err}
			case <-ctx.Done():
			}
		}()
	}

	for pending > 0 {
		select {
		case r := <-results:
			pending--
			rf := files[r.i]
			if r.err != nil {
				// Start this file over next time.
				delete(st.Partial, fmt.Sprint(r.i))
				st.save(dest)
				l.SendCtl(ctl{T: "complete", OK: false, Reason: r.err.Error()})
				return "", r.err
			}
			rf.fh.Sync()
			rf.fh.Close()
			rf.fh = nil
			if err := os.Rename(rf.part, rf.final); err != nil {
				return "", err
			}
			os.Chmod(rf.final, safeMode(rf.f.Mode))
			mt := time.Unix(rf.f.MTime, 0)
			os.Chtimes(rf.final, mt, mt)
			st.Done[r.i] = "ok"
			delete(st.Partial, fmt.Sprint(r.i))
			st.save(dest)
		case <-tick.C:
			l.SendCtl(ctl{T: "progress", Bytes: held.Load()})
			if progress != nil {
				progress(held.Load(), m.Total)
			}
			if time.Since(lastCP) > 2*time.Second {
				checkpoint()
				lastCP = time.Now()
			}
		case err := <-errc:
			checkpoint()
			return "", err
		case <-ctx.Done():
			checkpoint()
			return "", ctx.Err()
		}
	}
	for i, f := range m.Files {
		if f.Dir {
			os.Chmod(files[i].final, safeMode(f.Mode)|0o700)
		}
	}
	if progress != nil {
		progress(m.Total, m.Total)
	}
	os.Remove(statePath(dest, o.ID))
	l.SendCtl(ctl{T: "complete", OK: true, Saved: st.Root})
	return rootPath, nil
}

// safeMode keeps permission bits but never setuid, setgid or sticky, and
// never write access for others.
func safeMode(m os.FileMode) os.FileMode { return m.Perm() &^ 0o002 }
