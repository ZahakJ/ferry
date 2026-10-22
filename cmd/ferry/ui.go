package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Exit codes. They are part of the interface; see `ferry help exit-codes`.
const (
	exitOK        = 0
	exitError     = 1
	exitUsage     = 2
	exitAuth      = 3 // wrong code, unknown or changed key
	exitOffline   = 4 // peer or rendezvous unreachable
	exitTimeout   = 5
	exitIntegrity = 6 // data failed a hash check
	exitDeclined  = 7 // the other side said no
	exitInterrupt = 130
)

// out is how a command talks to its caller.
type out struct {
	json  bool
	tty   bool // stderr is a terminal
	color bool
	mu    sync.Mutex
}

func newOut(jsonMode bool) *out {
	tty := term.IsTerminal(int(os.Stderr.Fd()))
	return &out{
		json:  jsonMode,
		tty:   tty,
		color: tty && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb",
	}
}

// event writes one JSON line to stdout in --json mode.
func (o *out) event(kind string, fields map[string]any) {
	if !o.json {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	m := map[string]any{"event": kind}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	os.Stdout.Write(append(b, '\n'))
}

// say prints a human line to stderr (always; it is never the result).
func (o *out) say(format string, a ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.clearLine()
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// result prints the human-readable result to stdout, unless --json.
func (o *out) result(format string, a ...any) {
	if o.json {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.clearLine()
	fmt.Fprintf(os.Stdout, format+"\n", a...)
}

func (o *out) clearLine() {
	if o.tty {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

func (o *out) style(code, s string) string {
	if !o.color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (o *out) bold(s string) string  { return o.style("1", s) }
func (o *out) dim(s string) string   { return o.style("2", s) }
func (o *out) green(s string) string { return o.style("32", s) }
func (o *out) red(s string) string   { return o.style("31", s) }

// ---- progress ----

type progress struct {
	o       *out
	label   string
	total   int64
	start   time.Time
	last    time.Time
	samples []sample
	done    int64
	base    int64 // already there when the transfer started (a resume)
	seen    bool
	ended   bool
}

type sample struct {
	t time.Time
	n int64
}

func (o *out) progress(label string, total int64) *progress {
	return &progress{o: o, label: label, total: total, start: time.Now()}
}

// rate is bytes/s over the last few seconds, which reacts to change
// without jumping around.
func (p *progress) rate() float64 {
	if len(p.samples) < 2 {
		return 0
	}
	a, b := p.samples[0], p.samples[len(p.samples)-1]
	dt := b.t.Sub(a.t).Seconds()
	if dt <= 0 {
		return 0
	}
	return float64(b.n-a.n) / dt
}

func (p *progress) update(done, total int64) {
	now := time.Now()
	p.o.mu.Lock()
	defer p.o.mu.Unlock()
	if p.ended {
		return
	}
	if !p.seen {
		p.seen, p.base = true, done
	}
	p.done, p.total = done, total
	p.samples = append(p.samples, sample{now, done})
	for len(p.samples) > 2 && now.Sub(p.samples[0].t) > 5*time.Second {
		p.samples = p.samples[1:]
	}
	interval := 100 * time.Millisecond
	if !p.o.tty {
		interval = 5 * time.Second
	}
	if now.Sub(p.last) < interval && done < total {
		return
	}
	p.last = now
	r := p.rate()
	if p.o.json {
		b, _ := json.Marshal(map[string]any{"event": "progress", "bytes": done, "total": total, "rate": int64(r)})
		os.Stderr.Write(append(b, '\n'))
		return
	}
	pct := 100.0
	if total > 0 {
		pct = 100 * float64(done) / float64(total)
	}
	eta := ""
	if r > 0 && done < total {
		eta = " · " + human(time.Duration(float64(total-done)/r*float64(time.Second))) + " left"
	}
	speed := ""
	if r > 0 {
		speed = "  " + size(int64(r)) + "/s"
	}
	line := fmt.Sprintf("%s %5.1f%%  %s of %s%s%s", p.label, pct, size(done), size(total), speed, eta)
	if !p.o.tty {
		fmt.Fprintln(os.Stderr, line)
		return
	}
	w := 14
	fill := int(pct / 100 * float64(w))
	bar := p.o.green(strings.Repeat("━", fill)) + p.o.dim(strings.Repeat("━", w-fill))
	fmt.Fprintf(os.Stderr, "\r\033[K%s %s", bar, line)
}

func (p *progress) end() {
	p.o.mu.Lock()
	p.ended = true
	p.o.mu.Unlock()
	if p.o.tty && !p.o.json {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

func (p *progress) average() float64 {
	d := time.Since(p.start).Seconds()
	if d <= 0 {
		return 0
	}
	return float64(p.done-p.base) / d
}

// ---- formatting ----

func size(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f, i := float64(n), 0
	for f >= unit && i < 4 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, []string{"B", "kB", "MB", "GB", "TB"}[i])
}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// clean makes text that came from someone else safe to print on a
// terminal: control characters (which could move the cursor, rewrite the
// screen or change the title) and bidirectional overrides (which could
// make text read differently from what it is) become "?". Newlines and
// tabs stay.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return '?'
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return '?'
		}
		return r
	}, s)
}

// resumed says where a resumed transfer picked up, for the summary line.
func (p *progress) resumed() string {
	if p.base <= 0 {
		return ""
	}
	return "; resumed at " + size(p.base)
}
