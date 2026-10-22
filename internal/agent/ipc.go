package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ZahakJ/ferry/internal/keys"
	"github.com/ZahakJ/ferry/internal/link"
	"github.com/ZahakJ/ferry/internal/msg"
	"github.com/ZahakJ/ferry/internal/session"
	"github.com/ZahakJ/ferry/internal/xfer"
)

// SocketPath is where a background agent listens. Unix socket paths are
// limited to about 100 bytes, so a deep FERRY_HOME uses a private
// directory in the runtime dir (or the temp dir) instead.
func SocketPath() string {
	p := filepath.Join(keys.Dir(), "agent.sock")
	if len(p) < 100 {
		return p
	}
	h := sha256.Sum256([]byte(keys.Dir()))
	return filepath.Join(privateDir(), fmt.Sprintf("%x.sock", h[:8]))
}

// privateDir is a directory only this user can enter, so nobody else can
// put a socket where ours is expected.
func privateDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	d := filepath.Join(base, fmt.Sprintf("ferry-%d", os.Getuid()))
	os.Mkdir(d, 0o700)
	if st, err := os.Lstat(d); err == nil && st.IsDir() && st.Mode().Perm() == 0o700 && owned(st) {
		return d
	}
	// Someone else made it, or it is a symlink: fall back to our own home.
	return keys.Dir()
}

type request struct {
	Op      string          `json:"op"`
	Send    *SendRequest    `json:"send,omitempty"`
	Since   int64           `json:"since,omitempty"`
	From    string          `json:"from,omitempty"`
	To      string          `json:"to,omitempty"`
	Path    string          `json:"path,omitempty"`
	Timeout time.Duration   `json:"timeout,omitempty"`
	Extra   json.RawMessage `json:"extra,omitempty"`
}

type response struct {
	Progress *[2]int64 `json:"progress,omitempty"`
	OK       bool      `json:"ok"`
	Result   any       `json:"result,omitempty"`
	Error    string    `json:"error,omitempty"`
	Kind     string    `json:"kind,omitempty"`
}

// Error kinds carried across the socket, so callers can still use errors.Is.
var kinds = map[string]error{
	"offline":         session.ErrOffline,
	"timeout":         ErrTimeout,
	"unknown_contact": ErrUnknownContact,
	"no_rendezvous":   session.ErrNoRendezvous,
	"canceled":        context.Canceled,
	"integrity":       xfer.ErrIntegrity,
	"declined":        session.ErrDeclined,
	"auth":            session.ErrKeyChanged,
	"no_room":         xfer.ErrNoRoom,
	"no_path":         link.ErrNoPath,
}

func kindOf(err error) string {
	for k, e := range kinds {
		if errors.Is(err, e) {
			return k
		}
	}
	return "error"
}

type remoteError struct {
	msg  string
	kind error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.kind }

// Serve answers requests on the socket until ctx ends.
func Serve(ctx context.Context, a API) error {
	path := SocketPath()
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return errors.New("another ferry agent is already running on this machine")
	}
	os.Remove(path)
	old := unix.Umask(0o177)
	l, err := net.Listen("unix", path)
	unix.Umask(old)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close(); os.Remove(path) }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handle(ctx, a, c.(*net.UnixConn))
	}
}

func sameUser(c *net.UnixConn) bool {
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	raw.Control(func(fd uintptr) {
		uid, err := peerUID(int(fd))
		ok = err == nil && uid == os.Getuid()
	})
	return ok
}

func handle(ctx context.Context, a API, c *net.UnixConn) {
	defer c.Close()
	if !sameUser(c) {
		return
	}
	var req request
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		return
	}
	enc := json.NewEncoder(c)
	// A request ends when the client hangs up, so a cancelled `ferry wait`
	// does not leave work running here.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		var b [1]byte
		c.Read(b[:])
		cancel()
	}()
	var res any
	var err error
	switch req.Op {
	case "status":
		res, err = a.Status(rctx)
	case "contacts":
		res, err = a.Contacts(rctx)
	case "send":
		if req.Send == nil {
			err = errors.New("missing message")
			break
		}
		res, err = a.Send(rctx, *req.Send)
	case "inbox":
		res, err = a.Inbox(rctx, req.Since, req.From)
	case "wait":
		res, err = a.Wait(rctx, req.From, req.Since, req.Timeout)
	case "send_file":
		res, err = a.SendFile(rctx, req.To, req.Path, func(done, total int64) {
			p := [2]int64{done, total}
			enc.Encode(response{Progress: &p})
		})
	case "files":
		res, err = a.Files(rctx, req.Since)
	default:
		err = fmt.Errorf("unknown request %q", req.Op)
	}
	if err != nil {
		enc.Encode(response{Error: err.Error(), Kind: kindOf(err)})
		return
	}
	enc.Encode(response{OK: true, Result: res})
}

// Remote is a client for a background agent.
type Remote struct{}

// Running reports whether a background agent answers on the socket.
func Running() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := dialAgent(ctx)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Open returns the background agent if one is running, else a local one.
func Open(cfg *session.Config) (API, error) {
	if Running() {
		return &Remote{}, nil
	}
	return NewLocal(cfg)
}

func (r *Remote) call(ctx context.Context, req request, out any, progress xfer.Progress) error {
	c, err := dialAgent(ctx)
	if err != nil {
		return fmt.Errorf("background agent: %w", err)
	}
	defer c.Close()
	go func() { <-ctx.Done(); c.Close() }()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var resp struct {
			Progress *[2]int64       `json:"progress"`
			OK       bool            `json:"ok"`
			Result   json.RawMessage `json:"result"`
			Error    string          `json:"error"`
			Kind     string          `json:"kind"`
		}
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			return err
		}
		if resp.Progress != nil {
			if progress != nil {
				progress(resp.Progress[0], resp.Progress[1])
			}
			continue
		}
		if !resp.OK {
			return &remoteError{msg: resp.Error, kind: kinds[resp.Kind]}
		}
		if out != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("background agent hung up")
}

func (r *Remote) Status(ctx context.Context) (*Status, error) {
	var s Status
	return &s, r.call(ctx, request{Op: "status"}, &s, nil)
}

func (r *Remote) Contacts(ctx context.Context) ([]Contact, error) {
	var cs []Contact
	return cs, r.call(ctx, request{Op: "contacts"}, &cs, nil)
}

func (r *Remote) Send(ctx context.Context, req SendRequest) (*SendResult, error) {
	var s SendResult
	return &s, r.call(ctx, request{Op: "send", Send: &req}, &s, nil)
}

func (r *Remote) Inbox(ctx context.Context, since int64, from string) (*InboxResult, error) {
	var res InboxResult
	return &res, r.call(ctx, request{Op: "inbox", Since: since, From: from}, &res, nil)
}

func (r *Remote) Wait(ctx context.Context, from string, since int64, timeout time.Duration) (*msg.Record, error) {
	var rec msg.Record
	if err := r.call(ctx, request{Op: "wait", From: from, Since: since, Timeout: timeout}, &rec, nil); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *Remote) SendFile(ctx context.Context, to, path string, progress xfer.Progress) (*FileResult, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	var res FileResult
	return &res, r.call(ctx, request{Op: "send_file", To: to, Path: abs}, &res, progress)
}

func (r *Remote) Files(ctx context.Context, since int64) ([]Received, error) {
	var fs []Received
	return fs, r.call(ctx, request{Op: "files", Since: since}, &fs, nil)
}

func (r *Remote) Close() error { return nil }

func owned(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Getuid()
}

// dialAgent connects to the agent socket and checks that whoever answers
// runs as this user; a socket from anyone else is treated as absent.
func dialAgent(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", SocketPath())
	if err != nil {
		return nil, err
	}
	if !sameUser(c.(*net.UnixConn)) {
		c.Close()
		return nil, fmt.Errorf("the agent socket %s belongs to another user; refusing it", SocketPath())
	}
	return c, nil
}
