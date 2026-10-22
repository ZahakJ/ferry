// Package xfer moves files over a link.
//
// The sender describes what it has (a manifest); the receiver answers with
// what it already holds from an earlier, interrupted attempt; the sender
// streams the missing pieces over every lane at once; and the receiver
// checks each piece and then each whole file against a BLAKE3 hash before
// giving it its real name.
package xfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PieceSize is the unit of transfer and of resume.
const PieceSize = 256 << 10

// File is one entry in a manifest. Paths use forward slashes and are
// relative to the transfer's root name.
type File struct {
	Path  string      `json:"path"`
	Size  int64       `json:"size"`
	Mode  fs.FileMode `json:"mode"`
	Dir   bool        `json:"dir,omitempty"`
	MTime int64       `json:"mtime"`
}

// Pieces is how many pieces the file has.
func (f File) Pieces() int { return int((f.Size + PieceSize - 1) / PieceSize) }

// Manifest describes a transfer.
type Manifest struct {
	// Name is the top-level file or directory name the receiver creates.
	Name  string `json:"name"`
	Files []File `json:"files"`
	Total int64  `json:"total"`
	// Skipped lists things the sender left out, such as symlinks.
	Skipped []string `json:"skipped,omitempty"`
}

// ID identifies the transfer for resuming: the same files, sizes and
// modification times give the same ID, whoever sends them.
func (m *Manifest) ID() string {
	h := sha256.New()
	b, _ := json.Marshal(struct {
		N string
		F []File
	}{m.Name, m.Files})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// Scan builds a manifest for a file or directory.
func Scan(path string) (*Manifest, error) {
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if name == "/" || name == "." {
		abs, _ := filepath.Abs(path)
		name = filepath.Base(abs)
	}
	m := &Manifest{Name: name}
	if !st.IsDir() {
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file or directory", path)
		}
		m.Files = []File{{Path: "", Size: st.Size(), Mode: st.Mode().Perm(), MTime: st.ModTime().Unix()}}
		m.Total = st.Size()
		return m, nil
	}
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(path, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			m.Files = append(m.Files, File{Path: rel, Dir: true, Mode: info.Mode().Perm(), MTime: info.ModTime().Unix()})
		case info.Mode().IsRegular():
			m.Files = append(m.Files, File{Path: rel, Size: info.Size(), Mode: info.Mode().Perm(), MTime: info.ModTime().Unix()})
			m.Total += info.Size()
		default:
			m.Skipped = append(m.Skipped, rel)
		}
		return nil
	})
	return m, err
}

// Check rejects a manifest from a peer that could write outside the
// destination or is otherwise unreasonable. Received manifests are
// untrusted input.
func (m *Manifest) Check() error {
	if m.Name == "" || m.Name == "." || m.Name == ".." || strings.ContainsAny(m.Name, `/\`) || strings.HasPrefix(m.Name, ".ferry") {
		return fmt.Errorf("refusing unsafe name %q", m.Name)
	}
	if len(m.Files) == 0 {
		return errors.New("empty transfer")
	}
	if len(m.Files) > 1_000_000 {
		return errors.New("too many files")
	}
	var total int64
	seen := map[string]bool{}
	single := len(m.Files) == 1 && m.Files[0].Path == "" && !m.Files[0].Dir
	for _, f := range m.Files {
		if !single && (f.Path == "" || !filepath.IsLocal(filepath.FromSlash(f.Path)) || strings.Contains(f.Path, `\`)) {
			return fmt.Errorf("refusing unsafe path %q", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("duplicate path %q", f.Path)
		}
		seen[f.Path] = true
		if f.Size < 0 || (f.Dir && f.Size != 0) {
			return fmt.Errorf("bad size for %q", f.Path)
		}
		total += f.Size
	}
	if total != m.Total {
		return errors.New("manifest total does not add up")
	}
	return nil
}
