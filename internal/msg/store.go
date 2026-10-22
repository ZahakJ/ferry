package msg

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Record is one entry in the local message log.
type Record struct {
	Seq     int64     `json:"seq"`
	Dir     string    `json:"dir"` // in | out
	Contact string    `json:"contact"`
	At      time.Time `json:"at"`  // when this machine logged it
	Via     string    `json:"via"` // direct | mailbox
	Msg     *Message  `json:"msg"`
}

// Store is an append-only log of messages, shared safely between
// processes on one machine with a file lock. Sequence numbers only grow,
// so "everything after N" is a stable cursor.
type Store struct{ path string }

// OpenStore opens (or creates) the log in dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "messages.jsonl")}, nil
}

func (s *Store) locked(write bool, f func(*os.File) error) error {
	flags := os.O_RDONLY | os.O_CREATE
	if write {
		flags = os.O_RDWR | os.O_CREATE | os.O_APPEND
	}
	fh, err := os.OpenFile(s.path, flags, 0o600)
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
	return f(fh)
}

func readAll(fh *os.File) ([]Record, error) {
	if _, err := fh.Seek(0, 0); err != nil {
		return nil, err
	}
	var out []Record
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Msg != nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// Add logs r unless a message with the same ID and direction is already
// there. It fills in Seq and At and reports whether it was new.
func (s *Store) Add(r *Record) (bool, error) {
	added := false
	err := s.locked(true, func(fh *os.File) error {
		all, err := readAll(fh)
		if err != nil {
			return err
		}
		var last int64
		for _, x := range all {
			if x.Msg.ID == r.Msg.ID && x.Dir == r.Dir {
				*r = x
				return nil
			}
			last = max(last, x.Seq)
		}
		r.Seq = last + 1
		if r.At.IsZero() {
			r.At = time.Now().UTC()
		}
		b, _ := json.Marshal(r)
		if _, err := fh.Write(append(b, '\n')); err != nil {
			return err
		}
		added = true
		return fh.Sync()
	})
	return added, err
}

// Since returns records after seq, optionally only for one contact and
// one direction ("" for any).
func (s *Store) Since(seq int64, contact, dir string) ([]Record, error) {
	var out []Record
	err := s.locked(false, func(fh *os.File) error {
		all, err := readAll(fh)
		for _, r := range all {
			if r.Seq > seq && (contact == "" || r.Contact == contact) && (dir == "" || r.Dir == dir) {
				out = append(out, r)
			}
		}
		return err
	})
	return out, err
}

// Last returns the highest sequence number, 0 if empty.
func (s *Store) Last() (int64, error) {
	var last int64
	err := s.locked(false, func(fh *os.File) error {
		all, err := readAll(fh)
		for _, r := range all {
			last = max(last, r.Seq)
		}
		return err
	})
	return last, err
}
