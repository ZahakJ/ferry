package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

// ferry bring copies a folder of yours into the box at the same path (so
// paths in your notes, scripts and Claude sessions mean the same thing
// there), with your Claude sessions for it. ferry take copies it back. Both
// run over your own ssh connection to the box; nothing in the box ever
// connects here. Whatever you bring, the lender can see. Whatever you take
// comes from the lender's machine, so take writes only what it asked for,
// only inside the folder, never through a symlink, never over a newer file,
// and never git's config or hooks (they could run commands here).

// treeEntry is one file, symlink or folder under a root.
type treeEntry struct {
	rel   string // slash-separated, relative to the root
	typ   byte   // 'f' file, 'l' symlink, 'd' folder
	size  int64
	mtime int64 // seconds
	exec  bool
	link  string
}

// claudeProjectKey is the folder name Claude Code keeps a directory's
// sessions under in ~/.claude/projects: every UTF-16 unit that is not an
// ASCII letter or digit becomes '-', and names longer than 200 get cut with
// a hash of the path appended (Claude Code 2.1: kme/hL).
func claudeProjectKey(p string) string {
	u := utf16.Encode([]rune(p))
	b := make([]byte, len(u))
	for i, c := range u {
		if c < 128 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = byte(c)
		} else {
			b[i] = '-'
		}
	}
	if len(b) <= 200 {
		return string(b)
	}
	var h int32
	for _, c := range u {
		h = h*31 + int32(c)
	}
	a := int64(h)
	if a < 0 {
		a = -a
	}
	return string(b[:200]) + "-" + strconv.FormatInt(a, 36)
}

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ---- what bring leaves out ----

var excludedDirs = map[string]bool{"node_modules": true, ".venv": true, "__pycache__": true, "target": true}

var secretDirs = map[string]bool{".ssh": true, ".aws": true, ".gnupg": true}

var credURL = regexp.MustCompile(`(?i)https?://[^/\s@]+@`)

// secretFile says whether a file looks like a secret: brought only with --include-secrets.
func secretFile(rel, full string) bool {
	base := path.Base(rel)
	switch {
	case strings.HasPrefix(base, ".env"),
		strings.HasSuffix(base, ".pem"),
		base == ".netrc", base == ".git-credentials", base == ".npmrc", base == ".pypirc",
		strings.HasPrefix(base, "credentials") && strings.HasSuffix(base, ".json"):
		return true
	case strings.HasPrefix(base, "id_") && (!strings.Contains(base, ".") || strings.HasSuffix(base, ".pub")):
		return true // id_rsa, id_ed25519.pub; not id_utils.go
	case base == "config" && strings.HasSuffix(path.Dir(rel), ".git"):
		b, _ := os.ReadFile(full) // a remote URL with a token in it
		return credURL.Match(b)
	}
	return false
}

// gitProtected says whether take must leave a path alone: git's config and
// hooks (and a .git file pointing elsewhere) can make git run commands here.
func gitProtected(rel string, typ byte) bool {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if !strings.EqualFold(p, ".git") {
			continue
		}
		rest := parts[i+1:]
		if len(rest) == 0 {
			return typ != 'd'
		}
		for _, r := range rest {
			if strings.EqualFold(r, "hooks") {
				return true
			}
		}
		last := rest[len(rest)-1]
		if strings.EqualFold(last, "config") || strings.EqualFold(last, "config.worktree") {
			return true
		}
	}
	return false
}

// cleanRel checks a path the box sent: relative, inside the root, no tricks.
func cleanRel(s string) (string, bool) {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "./"), "/")
	if s == "" || strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\x00\\") {
		return "", false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	return s, true
}

// linkInside says whether a symlink at rel pointing to target stays inside the root.
func linkInside(rel, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.Contains(target, "\x00") {
		return false
	}
	j := path.Clean(path.Join(path.Dir(rel), target))
	return j != ".." && !strings.HasPrefix(j, "../")
}

// ---- local side ----

type walkOpts struct {
	project bool // apply the project filters below
	secrets bool // --include-secrets
	all     bool // --all
}

func walkLocal(root string, opt walkOpts) (es []treeEntry, secrets, excluded []string, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable: skip
		}
		if p == root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		base := d.Name()
		if d.IsDir() {
			if opt.project && !opt.all && excludedDirs[base] {
				excluded = append(excluded, rel+"/")
				return filepath.SkipDir
			}
			if opt.project && !opt.secrets && secretDirs[base] {
				secrets = append(secrets, rel+"/")
				return filepath.SkipDir
			}
		} else if opt.project && !opt.secrets && secretFile(rel, p) {
			secrets = append(secrets, rel)
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		e := treeEntry{rel: rel, mtime: fi.ModTime().Unix()}
		switch {
		case fi.Mode().IsRegular():
			e.typ, e.size, e.exec = 'f', fi.Size(), fi.Mode()&0o111 != 0
		case fi.Mode()&fs.ModeSymlink != 0:
			e.typ = 'l'
			if e.link, err = os.Readlink(p); err != nil {
				return nil
			}
		case fi.IsDir():
			e.typ = 'd'
		default:
			return nil // sockets, fifos, devices
		}
		es = append(es, e)
		return nil
	})
	return
}

// localFolder resolves a folder of yours to the real path both machines use.
// It must be inside your home (not the home itself), at a path the box can
// recreate (/home/<you>/… or /Users/<you>/…). With mustExist false it may not
// exist yet (take creates it).
func localFolder(arg string, mustExist bool) (string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	rhome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if mustExist || !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// the nearest folder that exists, then the rest as given
		anc, rest := abs, ""
		for {
			if r, err := filepath.EvalSymlinks(anc); err == nil {
				real = filepath.Join(r, rest)
				break
			}
			if anc == "/" {
				return "", fmt.Errorf("%s: no such folder", arg)
			}
			rest = filepath.Join(filepath.Base(anc), rest)
			anc = filepath.Dir(anc)
		}
	} else if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a folder", arg)
	}
	if real == rhome {
		return "", fmt.Errorf("pick a folder inside your home, not all of it (the lender can see what you bring)")
	}
	if !strings.HasPrefix(real, rhome+"/") {
		return "", fmt.Errorf("%s is not inside your home folder (%s)", arg, rhome)
	}
	if top := strings.SplitN(strings.TrimPrefix(real, "/"), "/", 3); len(top) < 3 || !validHisHome("/"+top[0]+"/"+top[1]) {
		return "", fmt.Errorf("%s: the box can only hold folders under /home/<you> or /Users/<you>", real)
	}
	return real, nil
}

// joinedBox picks the box: --box, or the only one joined.
func joinedBox(name string) (*boxState, error) {
	if name != "" {
		st, err := loadBox(name)
		if err != nil {
			return nil, fmt.Errorf("%q is not a box you joined (ferry join <code>)", name)
		}
		return st, nil
	}
	ms, _ := filepath.Glob(filepath.Join(boxesDir(), "*", "box.json"))
	switch len(ms) {
	case 0:
		return nil, errors.New("you haven't joined a box (ferry join <code>)")
	case 1:
		return loadBox(filepath.Base(filepath.Dir(ms[0])))
	}
	var ns []string
	for _, m := range ms {
		ns = append(ns, filepath.Base(filepath.Dir(m)))
	}
	return nil, usagef("you joined several boxes (%s); say which with --box <name>", strings.Join(ns, ", "))
}

func boxesDir() string { return filepath.Dir(boxDir("x")) }

func tildePath(p string) string {
	home, _ := os.UserHomeDir()
	if rh, err := filepath.EvalSymlinks(home); err == nil && strings.HasPrefix(p, rh+"/") {
		return "~" + strings.TrimPrefix(p, rh)
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func listSome(xs []string) string {
	if len(xs) <= 5 {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:5], ", ") + fmt.Sprintf(" and %d more", len(xs)-5)
}

// ---- box side ----

// boxRoot is a folder in the box: an absolute path, or one under the box
// account's home.
type boxRoot struct {
	abs     string
	homeRel string
}

func (r boxRoot) expr() string {
	if r.homeRel != "" {
		return `"$HOME"/` + shq(r.homeRel)
	}
	return shq(r.abs)
}

func (r boxRoot) String() string {
	if r.homeRel != "" {
		return "~/" + r.homeRel
	}
	return r.abs
}

// stream runs cmd in the box over the master with stdin and stdout, no
// time limit but ctx.
func (c sshConn) stream(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	x := exec.CommandContext(ctx, "ssh", append(c.base(c.name), cmd)...)
	var stderr bytes.Buffer
	x.Stdin, x.Stdout, x.Stderr = stdin, stdout, &stderr
	if err := x.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%v: %s", err, clean(msg))
		}
		return err
	}
	return nil
}

// listBox lists a folder in the box (untrusted output: parsed strictly).
// ok is false if the folder isn't there.
func listBox(ctx context.Context, c sshConn, r boxRoot) (map[string]treeEntry, bool, error) {
	var out bytes.Buffer
	cmd := "if cd -- " + r.expr() + " 2>/dev/null; then printf 'ok\\0'; find . -mindepth 1 \\( -type f -o -type l -o -type d \\) -printf '%y %s %T@ %P\\0%l\\0'; fi"
	if err := c.stream(ctx, cmd, nil, &out); err != nil {
		return nil, false, err
	}
	return parseBoxList(out.Bytes())
}

func parseBoxList(b []byte) (map[string]treeEntry, bool, error) {
	f := strings.Split(string(b), "\x00")
	if len(f) == 0 || f[0] != "ok" {
		return nil, false, nil
	}
	m := map[string]treeEntry{}
	for i := 1; i+1 < len(f); i += 2 {
		p := strings.SplitN(f[i], " ", 4)
		if len(p) != 4 || len(p[0]) != 1 || !strings.Contains("fld", p[0]) {
			continue
		}
		size, err1 := strconv.ParseInt(p[1], 10, 64)
		mt, err2 := strconv.ParseFloat(p[2], 64)
		rel, ok := cleanRel(p[3])
		if err1 != nil || err2 != nil || !ok || size < 0 || math.IsNaN(mt) || math.IsInf(mt, 0) {
			continue
		}
		m[rel] = treeEntry{rel: rel, typ: p[0][0], size: size, mtime: int64(math.Floor(mt)), link: f[i+1]}
	}
	return m, true, nil
}

// ---- ferry bring ----

type sendPlan struct {
	send       []treeEntry
	bytes      int64
	newerThere []string // newer in the box: left alone
	conflicts  []string // a file here, a folder there (or the other way)
	same       int
}

func planSend(local []treeEntry, box map[string]treeEntry) sendPlan {
	var p sendPlan
	var blocked []string // folders that are files in the box: skip what is under them
	under := func(rel string) bool {
		for _, b := range blocked {
			if strings.HasPrefix(rel, b+"/") {
				return true
			}
		}
		return false
	}
	for _, e := range local {
		if under(e.rel) {
			continue
		}
		b, there := box[e.rel]
		switch {
		case !there:
			p.send = append(p.send, e)
			p.bytes += e.size
		case (e.typ == 'd') != (b.typ == 'd'):
			p.conflicts = append(p.conflicts, e.rel)
			if e.typ == 'd' {
				blocked = append(blocked, e.rel)
			}
		case e.typ == 'd':
		case b.mtime > e.mtime:
			p.newerThere = append(p.newerThere, e.rel)
		case b.mtime == e.mtime && b.size == e.size && b.typ == e.typ:
			p.same++
		default:
			p.send = append(p.send, e)
			p.bytes += e.size
		}
	}
	return p
}

// writeTar streams the entries under root as a tar.
func writeTar(w io.Writer, root string, es []treeEntry) error {
	tw := tar.NewWriter(w)
	for _, e := range es {
		h := &tar.Header{Name: e.rel, ModTime: time.Unix(e.mtime, 0), Mode: 0o644}
		full := filepath.Join(root, filepath.FromSlash(e.rel))
		switch e.typ {
		case 'd':
			h.Typeflag, h.Name, h.Mode = tar.TypeDir, e.rel+"/", 0o755
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
		case 'l':
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, e.link, 0o777
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
		case 'f':
			f, err := os.Open(full)
			if err != nil {
				continue // gone since the walk
			}
			fi, err := f.Stat()
			if err != nil || !fi.Mode().IsRegular() {
				f.Close()
				continue
			}
			h.Typeflag, h.Size, h.ModTime = tar.TypeReg, fi.Size(), time.Unix(fi.ModTime().Unix(), 0)
			if fi.Mode()&0o111 != 0 {
				h.Mode = 0o755
			}
			if err := tw.WriteHeader(h); err != nil {
				f.Close()
				return err
			}
			_, err = io.CopyN(tw, f, fi.Size())
			f.Close()
			if err != nil {
				return fmt.Errorf("%s changed while bringing it; run ferry bring again: %v", e.rel, err)
			}
		}
	}
	return tw.Close()
}

// sendTree copies the planned entries into r in the box.
func sendTree(ctx context.Context, c sshConn, localRoot string, r boxRoot, es []treeEntry) error {
	if len(es) == 0 {
		return nil
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, localRoot, es)) }()
	cmd := "umask 022; mkdir -p -- " + r.expr() + " && cd -- " + r.expr() + " && exec tar -x --no-same-owner -f -"
	err := c.stream(ctx, cmd, pr, io.Discard)
	pr.CloseWithError(errors.New("done"))
	return err
}

func cmdBring(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("bring", flag.ContinueOnError)
	box := fs.String("box", "", "the box (default: the one you joined)")
	secrets := fs.Bool("include-secrets", false, "also bring .env files, keys and other secret-looking files")
	all := fs.Bool("all", false, "also bring node_modules, .venv, __pycache__ and target")
	claudeMD := fs.Bool("claude-md", true, "bring your ~/.claude/CLAUDE.md (as the box's ~/.claude/his-CLAUDE.md)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usagef("usage: ferry bring [--box <box>] [--include-secrets] [--all] [folder]")
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	st, err := joinedBox(*box)
	if err != nil {
		return err
	}
	real, err := localFolder(dir, true)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	key := claudeProjectKey(real)
	sessLocal := filepath.Join(home, ".claude", "projects", key)

	files, secretList, excluded, err := walkLocal(real, walkOpts{project: true, secrets: *secrets, all: *all})
	if err != nil {
		return err
	}
	sess, _, _, _ := walkLocal(sessLocal, walkOpts{})
	var total int64
	nf, ns := 0, 0
	for _, e := range files {
		total += e.size
		if e.typ == 'f' {
			nf++
		}
	}
	for _, e := range sess {
		total += e.size
		if e.typ == 'f' && strings.HasSuffix(e.rel, ".jsonl") && !strings.Contains(e.rel, "/") {
			ns++
		}
	}
	o.say("Bringing %s to %s:%s (%s, %s, %s).", tildePath(real), st.Contact, real, plural(nf, "file"), plural(ns, "Claude session"), size(total))
	o.say("%s the lender can see everything you bring into the box.", o.bold("Remember:"))
	if len(secretList) > 0 {
		o.say("Left out %d secret-looking: %s (--include-secrets brings them).", len(secretList), listSome(secretList))
	}
	if len(excluded) > 0 {
		o.say("Left out %s (--all brings them).", listSome(excluded))
	}

	c, err := ensureMaster(st)
	if err != nil {
		return err
	}
	type part struct {
		local string
		box   boxRoot
		es    []treeEntry
	}
	parts := []part{{real, boxRoot{abs: real}, files}}
	if len(sess) > 0 {
		parts = append(parts, part{sessLocal, boxRoot{homeRel: ".claude/projects/" + key}, sess})
	}
	var sent, sentSess, same int
	var sentBytes int64
	var newer, conflicts []string
	for _, pt := range parts {
		there, _, err := listBox(ctx, c, pt.box)
		if err != nil {
			return fmt.Errorf("could not look at %s in the box: %w", pt.box, err)
		}
		p := planSend(pt.es, there)
		if err := sendTree(ctx, c, pt.local, pt.box, p.send); err != nil {
			return fmt.Errorf("copying to %s in the box: %w", pt.box, err)
		}
		for _, e := range p.send {
			switch {
			case e.typ == 'd':
			case pt.local == real:
				sent++
			default:
				sentSess++
			}
		}
		sentBytes += p.bytes
		same += p.same
		newer = append(newer, p.newerThere...)
		conflicts = append(conflicts, p.conflicts...)
	}
	if *claudeMD {
		if f, err := os.Open(filepath.Join(home, ".claude", "CLAUDE.md")); err == nil {
			err = c.stream(ctx, `umask 077; mkdir -p "$HOME/.claude" && cat >"$HOME/.claude/his-CLAUDE.md"`, f, io.Discard)
			f.Close()
			if err != nil {
				o.say("note: could not bring your CLAUDE.md: %v", err)
			}
		}
	}
	// just who you are in git, so commits made in the box are yours (not the whole file)
	gitName, _ := exec.Command("git", "config", "--global", "--get", "user.name").Output()
	gitMail, _ := exec.Command("git", "config", "--global", "--get", "user.email").Output()
	if n, m := strings.TrimSpace(string(gitName)), strings.TrimSpace(string(gitMail)); n != "" || m != "" {
		var cmd []string
		if n != "" {
			cmd = append(cmd, "git config --global user.name "+shq(n))
		}
		if m != "" {
			cmd = append(cmd, "git config --global user.email "+shq(m))
		}
		c.Run(strings.Join(cmd, " && ") + " 2>/dev/null; true")
	}

	if len(newer) > 0 {
		o.say("Kept %s newer in the box (not overwritten): %s. ferry take brings them here.", plural(len(newer), "file"), listSome(newer))
	}
	if len(conflicts) > 0 {
		o.say("Skipped %d that are a file on one side and a folder on the other: %s.", len(conflicts), listSome(conflicts))
	}
	what := plural(sent, "file")
	if sentSess > 0 {
		what += " and " + plural(sentSess, "Claude session file")
	}
	o.result("Brought %s (%s) to %s:%s; %d already up to date. In the box: cd %s", what, size(sentBytes), st.Contact, real, same, real)
	o.event("brought", map[string]any{"box": st.Contact, "path": real, "files": sent, "bytes": sentBytes, "unchanged": same,
		"newer_in_box": newer, "secrets_left_out": secretList, "left_out": excluded, "sessions": ns})
	return nil
}

// ---- ferry take ----

type takePlan struct {
	want      map[string]treeEntry // what to ask the box for
	bytes     int64
	newerHere []string // newer on this machine: kept
	conflicts []string
	protected []string
	links     []string // symlinks pointing out of the folder: not taken
	same      int
	order     []string
}

func planTake(localRoot string, box map[string]treeEntry) takePlan {
	p := takePlan{want: map[string]treeEntry{}}
	rels := make([]string, 0, len(box))
	for r := range box {
		rels = append(rels, r)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		e := box[rel]
		if gitProtected(rel, e.typ) {
			// listed only if the box's differs from yours (the hooks git ships are the same)
			fi, err := os.Lstat(filepath.Join(localRoot, filepath.FromSlash(rel)))
			if e.typ != 'd' && (err != nil || fi.Size() != e.size || fi.ModTime().Unix() != e.mtime) {
				p.protected = append(p.protected, rel)
			}
			continue
		}
		if e.typ == 'l' && !linkInside(rel, e.link) {
			p.links = append(p.links, rel)
			continue
		}
		fi, err := os.Lstat(filepath.Join(localRoot, filepath.FromSlash(rel)))
		switch {
		case err != nil:
			p.want[rel] = e
		case e.typ == 'd':
			if !fi.IsDir() {
				p.conflicts = append(p.conflicts, rel)
			}
			continue
		case e.typ == 'l':
			continue // a symlink here already: leave it
		case !fi.Mode().IsRegular():
			p.conflicts = append(p.conflicts, rel)
			continue
		case fi.ModTime().Unix() > e.mtime:
			p.newerHere = append(p.newerHere, rel)
			continue
		case fi.ModTime().Unix() == e.mtime && fi.Size() == e.size:
			p.same++
			continue
		default:
			p.want[rel] = e
		}
		if e.typ == 'f' {
			p.bytes += e.size
		}
		p.order = append(p.order, rel)
	}
	return p
}

type takeResult struct {
	files     int
	bytes     int64
	newerHere []string
	refused   []string // what the box sent that take did not ask for, or could not place safely
}

// mkdirInside makes the folders of rel under root, refusing to pass through
// anything that is not a real folder (a symlink, say).
func mkdirInside(root, rel string) error {
	dir := root
	for _, p := range strings.Split(path.Dir(rel), "/") {
		if p == "." {
			break
		}
		dir = filepath.Join(dir, p)
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("%s is not a folder here", dir)
		}
	}
	return nil
}

// extractTake writes what the box sent: only entries take asked for, each
// at most once, inside root, never through a symlink, never over a file
// that is newer here.
func extractTake(r io.Reader, root string, want map[string]treeEntry, fileMode os.FileMode) (takeResult, error) {
	var res takeResult
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		rel, ok := cleanRel(h.Name)
		e, asked := want[rel]
		if !ok || !asked {
			res.refused = append(res.refused, h.Name)
			continue
		}
		delete(want, rel)
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := mkdirInside(root, rel); err != nil {
			res.refused = append(res.refused, rel)
			continue
		}
		switch {
		case h.Typeflag == tar.TypeDir && e.typ == 'd':
			if fi, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
				os.Mkdir(dst, 0o755)
			} else if err == nil && !fi.IsDir() {
				res.refused = append(res.refused, rel)
			}
		case h.Typeflag == tar.TypeSymlink && e.typ == 'l':
			if !linkInside(rel, h.Linkname) {
				res.refused = append(res.refused, rel)
			} else if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
				os.Symlink(h.Linkname, dst)
			}
		case (h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA) && e.typ == 'f':
			if h.Size != e.size {
				res.refused = append(res.refused, rel+" (changed in the box while taking it; run ferry take again)")
				continue
			}
			if fi, err := os.Lstat(dst); err == nil {
				if !fi.Mode().IsRegular() {
					res.refused = append(res.refused, rel)
					continue
				}
				if fi.ModTime().Unix() > h.ModTime.Unix() {
					res.newerHere = append(res.newerHere, rel)
					continue
				}
			}
			tmp, err := os.CreateTemp(filepath.Dir(dst), ".ferry-take-*")
			if err != nil {
				return res, err
			}
			n, err := io.Copy(tmp, tr)
			mode := fileMode
			if h.Mode&0o111 != 0 && fileMode == 0o644 {
				mode = 0o755
			}
			tmp.Chmod(mode)
			if cerr := tmp.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				os.Chtimes(tmp.Name(), h.ModTime, h.ModTime)
				err = os.Rename(tmp.Name(), dst) // replaces a file, never follows a link
			}
			if err != nil {
				os.Remove(tmp.Name())
				return res, err
			}
			res.files++
			res.bytes += n
		default:
			res.refused = append(res.refused, rel)
		}
	}
	return res, nil
}

func freeSpace(dir string) int64 {
	for {
		var s syscall.Statfs_t
		if syscall.Statfs(dir, &s) == nil {
			return int64(s.Bavail) * int64(s.Bsize)
		}
		if dir == "/" || dir == "." {
			return -1
		}
		dir = filepath.Dir(dir)
	}
}

// takeTree asks the box for the planned entries of r and writes them under localRoot.
func takeTree(ctx context.Context, c sshConn, r boxRoot, localRoot string, p takePlan, fileMode os.FileMode) (takeResult, error) {
	if len(p.order) == 0 {
		return takeResult{}, nil
	}
	dirMode := os.FileMode(0o755)
	if fileMode == 0o600 {
		dirMode = 0o700
	}
	if err := os.MkdirAll(localRoot, dirMode); err != nil {
		return takeResult{}, err
	}
	var list bytes.Buffer
	for _, rel := range p.order {
		list.WriteString("./" + rel + "\x00")
	}
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		cmd := "cd -- " + r.expr() + " && exec tar -c --no-recursion --verbatim-files-from --null -T - -f -"
		err := c.stream(ctx, cmd, &list, pw)
		pw.CloseWithError(err)
		errc <- err
	}()
	want := map[string]treeEntry{}
	for _, rel := range p.order {
		want[rel] = p.want[rel]
	}
	res, err := extractTake(pr, localRoot, want, fileMode)
	pr.CloseWithError(errors.New("done"))
	if serr := <-errc; err == nil && serr != nil && res.files == 0 {
		err = serr
	}
	return res, err
}

func cmdTake(ctx context.Context, o *out, args []string) error {
	fs := flag.NewFlagSet("take", flag.ContinueOnError)
	box := fs.String("box", "", "the box (default: the one you joined)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usagef("usage: ferry take [--box <box>] [folder]")
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	st, err := joinedBox(*box)
	if err != nil {
		return err
	}
	real, err := localFolder(dir, false)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	key := claudeProjectKey(real)
	c, err := ensureMaster(st)
	if err != nil {
		return err
	}
	type part struct {
		box   boxRoot
		local string
		mode  os.FileMode
	}
	parts := []part{
		{boxRoot{abs: real}, real, 0o644},
		{boxRoot{homeRel: ".claude/projects/" + key}, filepath.Join(home, ".claude", "projects", key), 0o600},
	}
	var plans []takePlan
	var total int64
	for i, pt := range parts {
		there, ok, err := listBox(ctx, c, pt.box)
		if err != nil {
			return fmt.Errorf("could not look at %s in the box: %w", pt.box, err)
		}
		if !ok && i == 0 {
			return fmt.Errorf("the box has no %s (ferry bring puts it there)", real)
		}
		p := planTake(pt.local, there)
		plans = append(plans, p)
		total += p.bytes
	}
	nf := 0
	for _, p := range plans {
		for _, rel := range p.order {
			if p.want[rel].typ == 'f' {
				nf++
			}
		}
	}
	o.say("Taking %s:%s to %s (%s to copy, %s).", st.Contact, real, tildePath(real), plural(nf, "file"), size(total))
	if free := freeSpace(real); free >= 0 && total > free {
		return fmt.Errorf("not enough free space here: %s to copy, %s free", size(total), size(free))
	}
	var got takeResult
	var newer, protected, links, conflicts []string
	same := 0
	for i, pt := range parts {
		r, err := takeTree(ctx, c, pt.box, pt.local, plans[i], pt.mode)
		got.files += r.files
		got.bytes += r.bytes
		got.refused = append(got.refused, r.refused...)
		newer = append(append(newer, plans[i].newerHere...), r.newerHere...)
		protected = append(protected, plans[i].protected...)
		links = append(links, plans[i].links...)
		conflicts = append(conflicts, plans[i].conflicts...)
		same += plans[i].same
		if err != nil {
			return fmt.Errorf("copying from %s in the box: %w", pt.box, err)
		}
	}
	if len(newer) > 0 {
		o.say("Kept %s that %s newer here than in the box: %s.", plural(len(newer), "file"), map[bool]string{true: "is", false: "are"}[len(newer) == 1], listSome(newer))
	}
	if len(protected) > 0 {
		o.say("Kept your own git config and hooks (from the box they could run commands here): %s.", listSome(protected))
	}
	if len(links) > 0 {
		o.say("Left out %s pointing outside the folder: %s.", plural(len(links), "symlink"), listSome(links))
	}
	if len(conflicts) > 0 {
		o.say("Skipped %d that are a file on one side and a folder on the other: %s.", len(conflicts), listSome(conflicts))
	}
	if len(got.refused) > 0 {
		o.say("Refused %d the box sent that were not asked for or could not be placed safely: %s.", len(got.refused), listSome(got.refused))
	}
	o.result("Took %s (%s) from %s into %s; %d already up to date.", plural(got.files, "file"), size(got.bytes), st.Contact, tildePath(real), same)
	o.event("took", map[string]any{"box": st.Contact, "path": real, "files": got.files, "bytes": got.bytes, "unchanged": same,
		"newer_here": newer, "protected": protected, "refused": got.refused})
	return nil
}
