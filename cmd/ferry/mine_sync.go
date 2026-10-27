package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Claude sessions, unified: every device pulls the others' Claude Code
// sessions (~/.claude/projects) into its own, so `claude --resume` on any
// of them lists the sessions of all. Only transcripts and their side files
// travel; never credentials, settings, memory or anything else in ~/.claude.
//
// Pull only, never deleting, never overwriting: a session file is append-
// only, so a longer copy there replaces nothing here; its new lines are
// appended, and only if what is here is exactly its beginning. If the two
// differ (the session went on on both devices), the other device's copy is
// kept beside this one as <session>.ferry-<device>.jsonl. Project folders
// are renamed between homes (/home/zman/x here is /Users/zman/x there) with
// Claude Code's own folder naming.

// syncPeer is the other device's ~/.claude/projects.
type syncPeer interface {
	// List returns every file's size, by slash-separated path.
	List(ctx context.Context) (map[string]int64, error)
	// Hashes returns the sha256 (hex) of the first n bytes of each file.
	Hashes(ctx context.Context, want map[string]int64) (map[string]string, error)
	// Fetch returns n bytes of a file from offset off (fewer if it is shorter).
	Fetch(ctx context.Context, rel string, off, n int64) ([]byte, error)
	// Cwd returns the working directory recorded in a project folder's sessions.
	Cwd(ctx context.Context, dir string) (string, error)
}

// syncCache remembers, per remote path, the sizes there and here when the
// file was last settled, so unchanged files cost nothing on the next round.
type syncCache map[string][2]int64

type syncResult struct {
	New       int `json:"new"`
	Extended  int `json:"extended"`
	Conflicts int `json:"kept_both"`
}

const maxSyncFile = 512 << 20

var (
	syncDirRE  = regexp.MustCompile(`^[A-Za-z0-9-][A-Za-z0-9._-]*$`)
	syncPartRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
)

// syncable says whether a path under ~/.claude/projects is part of a
// session: a transcript (.jsonl) or one of its side files (.json, .txt),
// not the per-device memory folder, nothing hidden, nothing odd.
func syncable(rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || len(parts) > 7 || !syncDirRE.MatchString(parts[0]) || parts[0] == "." || parts[0] == ".." {
		return false
	}
	if parts[1] == "memory" {
		return false
	}
	for _, p := range parts[1:] {
		if !syncPartRE.MatchString(p) {
			return false
		}
	}
	switch path.Ext(rel) {
	case ".jsonl", ".json", ".txt":
		return true
	}
	return false
}

// conflictName is where another device's diverged copy of rel is kept.
func conflictName(rel, dev string) string {
	ext := path.Ext(rel)
	return strings.TrimSuffix(rel, ext) + ".ferry-" + dev + ext
}

func isConflictCopy(rel string) bool { return strings.Contains(path.Base(rel), ".ferry-") }

func isConflictCopyOf(rel, dev string) bool {
	return strings.Contains(path.Base(rel), ".ferry-"+dev+".")
}

// mapProjectDir renames a project folder from the other device's home to
// this one's: Claude Code names it after the full path, so
// /home/zman/work (-home-zman-work) there is /Users/zman/work
// (-Users-zman-work) here. A name over 200 characters carries a hash of the
// whole path; then the real path is read from the sessions (cwd) and
// checked against the name. "" means it cannot be mapped (skip it).
func mapProjectDir(dir, peerHome, localHome string, cwd func() (string, error)) string {
	kp, kl := claudeProjectKey(peerHome), claudeProjectKey(localHome)
	if kp == kl && peerHome == localHome {
		return dir
	}
	if len(dir) <= 200 {
		var m string
		switch {
		case dir == kp:
			m = kl
		case strings.HasPrefix(dir, kp+"-"):
			m = kl + dir[len(kp):]
		default:
			return dir // not in the other device's home: the same path here
		}
		if len(m) <= 200 {
			return m
		}
	}
	c, err := cwd()
	if err != nil || c == "" || claudeProjectKey(c) != dir {
		return ""
	}
	if c == peerHome || strings.HasPrefix(c, peerHome+"/") {
		return claudeProjectKey(localHome + c[len(peerHome):])
	}
	return dir
}

func hashPrefix(p string, n int64) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	got, err := io.CopyN(h, f, n)
	if err != nil && !(errors.Is(err, io.EOF) && got == n) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSize(p string) int64 {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		if err == nil {
			return -2 // something that is not a plain file: leave it alone
		}
		return -1
	}
	return fi.Size()
}

// wholeLines cuts b after its last newline (a line being written there
// waits for the next round).
func wholeLines(b []byte) []byte {
	i := bytes.LastIndexByte(b, '\n')
	if i < 0 {
		return nil
	}
	return b[:i+1]
}

// createNew writes a file that must not exist yet.
func createNew(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// appendAt appends b to p if p is still exactly size bytes long.
func appendAt(p string, size int64, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() != size {
		return errors.New("changed meanwhile")
	}
	_, err = f.Write(b)
	return err
}

// syncSessions pulls the peer's sessions into localRoot (this device's
// ~/.claude/projects). me is this device's name, peer the other's.
func syncSessions(ctx context.Context, p syncPeer, peer, me, peerHome, localHome, localRoot string, cache syncCache) (syncResult, error) {
	var res syncResult
	if cache == nil {
		cache = syncCache{}
	}
	list, err := p.List(ctx)
	if err != nil {
		return res, err
	}
	// Project folders: map each once.
	dirs := map[string]string{}
	var rels []string
	for rel, size := range list {
		if !syncable(rel) || size > maxSyncFile || isConflictCopyOf(rel, me) {
			continue
		}
		d := rel[:strings.IndexByte(rel, '/')]
		if _, ok := dirs[d]; !ok {
			dirs[d] = mapProjectDir(d, peerHome, localHome, func() (string, error) { return p.Cwd(ctx, d) })
		}
		if dirs[d] == "" {
			continue
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	type check struct {
		rel, lrel  string
		rsize, lsz int64
	}
	var checks []check
	ask := map[string]int64{}
	for _, rel := range rels {
		d := rel[:strings.IndexByte(rel, '/')]
		lrel := dirs[d] + rel[len(d):]
		lp := filepath.Join(localRoot, filepath.FromSlash(lrel))
		rsize, lsize := list[rel], fileSize(lp)
		if c, ok := cache[rel]; ok && c == [2]int64{rsize, lsize} {
			continue
		}
		switch {
		case lsize == -2:
			continue
		case lsize == -1:
			if n, err := pullNew(ctx, p, rel, lp, rsize); err == nil && n {
				res.New++
			}
		case path.Ext(rel) != ".jsonl":
			// side files never change once written
		default: // a transcript on both: same, one goes on from the other, or diverged (even at equal size)
			n := min(rsize, lsize)
			checks = append(checks, check{rel, lrel, rsize, lsize})
			ask[rel] = n
			continue
		}
		cache[rel] = [2]int64{rsize, fileSize(lp)}
	}
	if len(ask) == 0 {
		return res, nil
	}
	theirs, err := p.Hashes(ctx, ask)
	if err != nil {
		return res, err
	}
	for _, c := range checks {
		lp := filepath.Join(localRoot, filepath.FromSlash(c.lrel))
		ours, err := hashPrefix(lp, min(c.rsize, c.lsz))
		if err != nil || theirs[c.rel] == "" {
			continue
		}
		same := ours == theirs[c.rel]
		switch {
		case same && c.rsize > c.lsz: // theirs goes on from ours: take the rest
			b, err := p.Fetch(ctx, c.rel, c.lsz, c.rsize-c.lsz)
			if b = wholeLines(b); err == nil && len(b) > 0 && appendAt(lp, c.lsz, b) == nil {
				res.Extended++
			}
		case same: // ours goes on from theirs: they will take it
		case isConflictCopy(c.rel):
			// a copy of a copy: leave it
		default: // the session went on on both: keep theirs beside ours
			if keepBoth(ctx, p, c.rel, filepath.Join(localRoot, filepath.FromSlash(conflictName(c.lrel, peer))), c.rsize) {
				res.Conflicts++
			}
		}
		cache[c.rel] = [2]int64{c.rsize, fileSize(lp)}
	}
	return res, nil
}

// pullNew copies a file that is not here yet.
func pullNew(ctx context.Context, p syncPeer, rel, lp string, size int64) (bool, error) {
	b, err := p.Fetch(ctx, rel, 0, size)
	if err != nil {
		return false, err
	}
	if path.Ext(rel) == ".jsonl" {
		b = wholeLines(b)
	} else if int64(len(b)) != size {
		return false, errors.New("short read")
	}
	if len(b) == 0 && size > 0 {
		return false, nil
	}
	if err := createNew(lp, b); err != nil {
		return false, err
	}
	return true, nil
}

// keepBoth keeps (or brings up to date) the other device's diverged copy
// at cp, by the same append-only rule.
func keepBoth(ctx context.Context, p syncPeer, rel, cp string, rsize int64) bool {
	csize := fileSize(cp)
	switch {
	case csize == -1:
		n, _ := pullNew(ctx, p, rel, cp, rsize)
		return n
	case csize < 0 || rsize <= csize:
		return false
	}
	theirs, err := p.Hashes(ctx, map[string]int64{rel: csize})
	if err != nil {
		return false
	}
	if ours, err := hashPrefix(cp, csize); err != nil || ours != theirs[rel] {
		return false
	}
	b, err := p.Fetch(ctx, rel, csize, rsize-csize)
	return err == nil && len(wholeLines(b)) > 0 && appendAt(cp, csize, wholeLines(b)) == nil
}

// syncLock keeps two syncs (ferry up's and a ferry mine sync) from
// appending the same lines twice.
func syncLock() (func(), error) {
	os.MkdirAll(mineDir(), 0o700)
	f, err := os.OpenFile(minePath("sync.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// ---- the other device over ssh ----

type sshPeer struct{ dev string }

const listScript = `cd "$HOME/.claude/projects" 2>/dev/null || exit 0
if stat -c %s . >/dev/null 2>&1; then find . -type f -exec stat -c '%s %n' {} +
else find . -type f -exec stat -f '%z %N' {} +; fi`

func (s sshPeer) List(ctx context.Context) (map[string]int64, error) {
	out, err := mineRun(ctx, s.dev, listScript, "", 2*time.Minute)
	if err != nil {
		return nil, err
	}
	return parseSizeList(string(out)), nil
}

// parseSizeList reads "size ./path" lines.
func parseSizeList(out string) map[string]int64 {
	m := map[string]int64{}
	for _, l := range strings.Split(out, "\n") {
		sz, p, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(sz, 10, 64)
		p, ok = strings.CutPrefix(p, "./")
		if err != nil || !ok || n < 0 {
			continue
		}
		m[p] = n
	}
	return m
}

const hashScript = `cd "$HOME/.claude/projects" || exit 1
if command -v sha256sum >/dev/null 2>&1; then H=sha256sum; else H='shasum -a 256'; fi
while read -r n f; do printf '%s %s\n' "$f" "$(head -c "$n" -- "./$f" | $H | cut -d' ' -f1)"; done`

func (s sshPeer) Hashes(ctx context.Context, want map[string]int64) (map[string]string, error) {
	var in strings.Builder
	for rel, n := range want {
		if !syncable(rel) {
			continue
		}
		fmt.Fprintf(&in, "%d %s\n", n, rel)
	}
	out, err := mineRun(ctx, s.dev, hashScript, in.String(), 5*time.Minute)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if p, h, ok := strings.Cut(l, " "); ok && len(h) == 64 {
			m[p] = h
		}
	}
	return m, nil
}

func (s sshPeer) Fetch(ctx context.Context, rel string, off, n int64) ([]byte, error) {
	if !syncable(rel) {
		return nil, errors.New("not a session file")
	}
	return mineRun(ctx, s.dev, fmt.Sprintf(`cd "$HOME/.claude/projects" && tail -c +%d -- ./%s | head -c %d`, off+1, shq(rel), n), "", 5*time.Minute)
}

func (s sshPeer) Cwd(ctx context.Context, dir string) (string, error) {
	if !syncDirRE.MatchString(dir) {
		return "", errors.New("bad folder")
	}
	out, err := mineRun(ctx, s.dev, fmt.Sprintf(`cd "$HOME/.claude/projects/"%s && for f in *.jsonl; do grep -m1 -o '"cwd": *"[^"]*"' "$f" && break; done`, shq(dir)), "", time.Minute)
	if err != nil && len(out) == 0 {
		return "", err
	}
	return parseCwd(string(out))
}

func parseCwd(out string) (string, error) {
	_, v, ok := strings.Cut(strings.TrimSpace(out), `"cwd":`)
	if !ok {
		return "", errors.New("no cwd recorded")
	}
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = v[:i]
	}
	var c string
	if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &c); err != nil {
		return "", err
	}
	return c, nil
}

// claudeProjects is this device's ~/.claude/projects.
func claudeProjects() string { return filepath.Join(mineHome(), ".claude", "projects") }
