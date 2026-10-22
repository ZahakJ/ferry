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
	"strings"
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
	// Batches are bounded by count and by size, so long paths still fit in
	// one control message.
	for start := 0; start < len(m.Files); {
		end, bytes := start, 0
		for end < len(m.Files) && end-start < filesBatch && bytes < link.MaxMessage/4 {
			bytes += len(m.Files[end].Path) + 96
			end++
		}
		if err := l.SendCtl(ctl{T: "files", Files: m.Files[start:end]}); err != nil {
			return "", err
		}
		start = end
	}
	reply, err := readCtl(l)
	if err != nil {
		return "", err
	}
	switch reply.T {
	case "accept":
	case "decline":
		return "", fmt.Errorf("%w: the receiver said: %s", ErrDeclined, reply.Reason)
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
				// Let the receiver know it may hang up.
				l.SendCtl(ctl{T: "bye"})
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
	pieces  int
	claimed bitmap // a lane has taken this piece; no one else may write it
	have    bitmap // the piece is written; the hasher may read it
	hash    chan string
}

// claim marks piece i as taken and reports whether this caller got it.
func (b bitmap) claim(i int) bool {
	p := &b[i/64]
	bit := uint64(1) << (uint(i) % 64)
	for {
		old := atomic.LoadUint64(p)
		if old&bit != 0 {
			return false
		}
		if atomic.CompareAndSwapUint64(p, old, old|bit) {
			return true
		}
	}
}

// handles keeps a small number of part files open, so a transfer of a
// million files does not need a million file descriptors. The lock only
// guards the table; reads and writes run in parallel, and a file in use
// is never closed under them.
type handles struct {
	mu    sync.Mutex
	files []*rfile
	open  map[int]*handle
	order []int // least recently used first
}

type handle struct {
	fh   *os.File
	refs int
	gone bool // evicted or finished; close when the last user is done
}

const maxOpen = 32

func (h *handles) acquire(i int) (*handle, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if x, ok := h.open[i]; ok {
		x.refs++
		h.touch(i)
		return x, nil
	}
	rf := h.files[i]
	fh, err := os.OpenFile(rf.part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := fh.Truncate(rf.f.Size); err != nil {
		fh.Close()
		return nil, fmt.Errorf("not enough room for %s: %w", rf.f.Path, err)
	}
	if len(h.order) >= maxOpen {
		h.evict(h.order[0])
	}
	x := &handle{fh: fh, refs: 1}
	h.open[i] = x
	h.order = append(h.order, i)
	return x, nil
}

func (h *handles) touch(i int) {
	for k, v := range h.order {
		if v == i {
			h.order = append(append(h.order[:k:k], h.order[k+1:]...), i)
			return
		}
	}
}

// evict drops file i from the table; it closes now if idle, else when
// its last user releases it. The caller holds the lock.
func (h *handles) evict(i int) *handle {
	x := h.open[i]
	delete(h.open, i)
	for k, v := range h.order {
		if v == i {
			h.order = append(h.order[:k], h.order[k+1:]...)
			break
		}
	}
	if x != nil {
		x.gone = true
		if x.refs == 0 {
			x.fh.Sync()
			x.fh.Close()
		}
	}
	return x
}

func (h *handles) release(x *handle) {
	h.mu.Lock()
	defer h.mu.Unlock()
	x.refs--
	if x.gone && x.refs == 0 {
		x.fh.Sync()
		x.fh.Close()
	}
}

func (h *handles) writeAt(i int, b []byte, off int64) error {
	x, err := h.acquire(i)
	if err != nil {
		return err
	}
	defer h.release(x)
	_, err = x.fh.WriteAt(b, off)
	return err
}

func (h *handles) readAt(i int, b []byte, off int64) error {
	x, err := h.acquire(i)
	if err != nil {
		return err
	}
	defer h.release(x)
	_, err = x.fh.ReadAt(b, off)
	return err
}

// finish makes sure file i exists on disk, flushed, and closes it. Every
// piece is written by then, so no lane still uses it.
func (h *handles) finish(i int) error {
	x, err := h.acquire(i)
	if err != nil {
		return err
	}
	err = x.fh.Sync()
	h.mu.Lock()
	h.evict(i)
	h.mu.Unlock()
	h.release(x)
	return err
}

func (h *handles) syncAll() {
	h.mu.Lock()
	var xs []*handle
	for _, x := range h.open {
		x.refs++
		xs = append(xs, x)
	}
	h.mu.Unlock()
	for _, x := range xs {
		x.fh.Sync()
		h.release(x)
	}
}

func (h *handles) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.open {
		h.evict(i)
	}
}

// rootName is the name the transfer gets in dest. A name starting with a
// dot (.git, .envrc, .vscode) could change how tools behave in that
// folder, so it is saved under a plain name instead.
func rootName(name string) string {
	if strings.HasPrefix(name, ".") {
		return "dot-" + strings.TrimLeft(name, ".")
	}
	return name
}

// ErrNoRoom means the destination has too little free space.
var ErrNoRoom = errors.New("not enough free space")

// Receive accepts an offer and writes it into dest. It resumes from any
// earlier attempt at the same transfer. It returns the saved path; onSaved,
// if set, hears it as soon as the files are in place, a moment before
// Receive returns.
func Receive(ctx context.Context, l *link.Link, o *Offer, dest string, progress Progress, onSaved func(path string)) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := o.Manifest
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	st := loadState(dest, o.ID)
	if st == nil {
		st = &state{ID: o.ID, Root: freeName(dest, rootName(m.Name)), Done: map[int]string{}, Partial: map[string]string{}}
	}
	single := len(m.Files) == 1 && m.Files[0].Path == ""
	rootPath := filepath.Join(dest, st.Root)

	files := make([]*rfile, len(m.Files))
	h := &handles{files: files, open: map[int]*handle{}}
	defer h.closeAll()
	var held atomic.Int64
	reply := ctl{T: "accept", Partial: map[string]string{}}
	for i, f := range m.Files {
		final := rootPath
		if !single {
			final = filepath.Join(rootPath, filepath.FromSlash(f.Path))
		}
		rf := &rfile{f: f, final: final, part: final + ".ferry-part", pieces: f.Pieces(), hash: make(chan string, 1)}
		files[i] = rf
		if f.Dir {
			continue
		}
		if _, ok := st.Done[i]; ok {
			if _, err := os.Stat(final); err == nil {
				reply.Done = append(reply.Done, i)
				held.Add(f.Size)
				rf.pieces = -1 // nothing to do
				continue
			}
			delete(st.Done, i)
		}
		rf.claimed, rf.have = newBitmap(rf.pieces), newBitmap(rf.pieces)
		if s, ok := st.Partial[fmt.Sprint(i)]; ok {
			if _, err := os.Stat(rf.part); err == nil {
				if b, err := decodeBitmap(s, rf.pieces); err == nil {
					copy(rf.have, b)
					copy(rf.claimed, b)
					reply.Partial[fmt.Sprint(i)] = s
					for n := 0; n < rf.pieces; n++ {
						if b.has(n) {
							held.Add(min(PieceSize, f.Size-int64(n)*PieceSize))
						}
					}
				}
			}
		}
	}
	if need := m.Total - held.Load(); need > 0 {
		if free, err := freeSpace(dest); err == nil && free < need+64<<20 {
			Decline(l, fmt.Sprintf("not enough room: needs %d more bytes, %d free", need, free))
			return "", fmt.Errorf("%w in %s: the transfer needs %d more bytes and %d are free", ErrNoRoom, dest, need, free)
		}
	}
	if err := st.save(dest); err != nil {
		return "", fmt.Errorf("cannot write to %s: %w", dest, err)
	}
	for i, f := range m.Files {
		if f.Dir {
			if err := os.MkdirAll(files[i].final, 0o755); err != nil {
				return "", err
			}
		} else if files[i].pieces >= 0 {
			if err := os.MkdirAll(filepath.Dir(files[i].final), 0o755); err != nil {
				return "", err
			}
		}
	}
	if err := l.SendCtl(reply); err != nil {
		return "", err
	}
	// The first progress report is what was already here, so both sides
	// can tell a resume from bytes moved now.
	if err := l.SendCtl(ctl{T: "progress", Bytes: held.Load()}); err != nil {
		return "", err
	}
	if progress != nil {
		progress(held.Load(), m.Total)
	}

	errc := make(chan error, len(l.Lanes)+2)
	fail := func(err error) {
		select {
		case errc <- err:
		default:
		}
		cancel()
	}
	arrived := make(chan struct{}, 1)

	// Lanes: check and write pieces. A piece is claimed before it is
	// written and never written twice, so what the hasher reads cannot
	// change underneath it.
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
				off := binary.BigEndian.Uint64(p[5:])
				data := p[pieceHeader:]
				if idx >= len(files) || files[idx].pieces <= 0 || off%PieceSize != 0 || off >= uint64(files[idx].f.Size) ||
					int64(len(data)) != min(PieceSize, files[idx].f.Size-int64(off)) {
					fail(errors.New("piece does not fit the manifest"))
					return
				}
				rf := files[idx]
				piece := int(off / PieceSize)
				if !bytes.Equal(pieceHash(data), p[13:29]) {
					fail(fmt.Errorf("%w: a piece of %s failed its check", ErrIntegrity, rf.f.Path))
					return
				}
				if !rf.claimed.claim(piece) {
					continue // already have it
				}
				if err := h.writeAt(idx, data, int64(off)); err != nil {
					fail(fmt.Errorf("writing %s: %w", rf.part, err))
					return
				}
				rf.have.set(piece)
				held.Add(int64(len(data)))
				select {
				case arrived <- struct{}{}:
				default:
				}
			}
		}()
	}

	// Control: whole-file hashes from the sender.
	bye := make(chan struct{})
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
				if c.Idx < 0 || c.Idx >= len(files) || files[c.Idx].pieces < 0 || files[c.Idx].f.Dir {
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
			case "bye":
				close(bye)
				return
			}
		}
	}()

	// One hasher walks the files in order as their pieces land, reading
	// back from disk (usually the page cache), so memory stays flat
	// however many files there are.
	type result struct {
		i   int
		err error
	}
	results := make(chan result, 1)
	go func() {
		buf := make([]byte, PieceSize)
		for i, rf := range files {
			if rf.f.Dir || rf.pieces < 0 {
				continue
			}
			hs := blake3.New(32, nil)
			for n := 0; n < rf.pieces; n++ {
				for !rf.have.has(n) {
					select {
					case <-arrived:
					case <-time.After(200 * time.Millisecond):
					case <-ctx.Done():
						return
					}
				}
				size := min(PieceSize, rf.f.Size-int64(n)*PieceSize)
				if err := h.readAt(i, buf[:size], int64(n)*PieceSize); err != nil {
					results <- result{i, err}
					return
				}
				hs.Write(buf[:size])
			}
			got := hex.EncodeToString(hs.Sum(nil))
			var err error
			select {
			case want := <-rf.hash:
				if got != want {
					err = fmt.Errorf("%w: %s does not match the sender's copy", ErrIntegrity, rf.f.Path)
				}
			case <-ctx.Done():
				return
			}
			select {
			case results <- result{i, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Checkpoint: sync data, then record what is safely on disk.
	checkpoint := func() {
		snaps := map[int]bitmap{}
		for i, rf := range files {
			if rf.pieces > 0 {
				snaps[i] = rf.have.snapshot()
			}
		}
		h.syncAll()
		for i, b := range snaps {
			if _, done := st.Done[i]; !done {
				st.Partial[fmt.Sprint(i)] = b.encode()
			}
		}
		st.save(dest)
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	lastCP := time.Now()

	pending := 0
	for _, rf := range files {
		if !rf.f.Dir && rf.pieces >= 0 {
			pending++
		}
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
			if err := h.finish(r.i); err != nil {
				return "", err
			}
			if err := os.Rename(rf.part, rf.final); err != nil {
				return "", err
			}
			os.Chmod(rf.final, safeMode(rf.f.Mode))
			mt := time.Unix(rf.f.MTime, 0)
			os.Chtimes(rf.final, mt, mt)
			st.Done[r.i] = "ok"
			delete(st.Partial, fmt.Sprint(r.i))
			if time.Since(lastCP) > 2*time.Second {
				st.save(dest)
			}
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
	if onSaved != nil {
		onSaved(rootPath)
	}
	// Hanging up at once could discard the message above; wait for the
	// sender to acknowledge it.
	select {
	case <-bye:
	case <-time.After(5 * time.Second):
	}
	return rootPath, nil
}

// ErrIntegrity means received data did not match its hash.
var ErrIntegrity = errors.New("integrity check failed")

// safeMode keeps permission bits but never setuid, setgid or sticky, and
// never write access for others.
func safeMode(m os.FileMode) os.FileMode { return m.Perm() &^ 0o002 }

// ErrDeclined means the receiver refused the offer.
var ErrDeclined = errors.New("declined")
