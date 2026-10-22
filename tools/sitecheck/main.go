// Command sitecheck opens every page of the built site in headless Chromium
// at a range of widths, in light and dark, and fails if any page is wider
// than the screen: document.documentElement.scrollWidth must not exceed
// innerWidth. With -shots it also saves a full-page capture of each.
//
//	go run ./tools/sitegen -out site
//	go run ./tools/sitecheck -site site [-shots /tmp/shots]
//
// It needs a Chromium (or Chrome) on the PATH, or -chrome. One browser runs
// at a time and is closed at the end.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	site := flag.String("site", "site", "built site directory")
	shots := flag.String("shots", "", "directory for captures (none if empty)")
	widths := flag.String("widths", "320,360,390,412,768,1024,1440", "viewport widths in CSS pixels")
	scale := flag.Float64("scale", 3, "device scale factor")
	chrome := flag.String("chrome", "", "browser binary (default: chromium, then google-chrome)")
	flag.Parse()

	pages, err := findPages(*site)
	if err != nil {
		log.Fatal(err)
	}
	var ws []int
	for _, s := range strings.Split(*widths, ",") {
		w, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			log.Fatalf("bad width %q", s)
		}
		ws = append(ws, w)
	}
	if *shots != "" {
		if err := os.MkdirAll(*shots, 0o755); err != nil {
			log.Fatal(err)
		}
	}

	base, stopServer, err := serve(*site)
	if err != nil {
		log.Fatal(err)
	}
	defer stopServer()
	b, err := launch(*chrome)
	if err != nil {
		log.Fatal(err)
	}
	defer b.close()

	failed := 0
	for _, page := range pages {
		for _, theme := range []string{"light", "dark"} {
			for _, w := range ws {
				r, err := b.check(base+"/"+page, w, *scale, theme, *shots, shotName(page, theme, w))
				if err != nil {
					b.close()
					stopServer()
					log.Fatalf("%s at %d (%s): %v", page, w, theme, err) // after cleanup: Fatalf skips defers
				}
				if r.ScrollWidth > w || r.InnerWidth > w {
					failed++
					fmt.Printf("FAIL %-22s %5d %-5s scrollWidth %d, innerWidth %d on a %d-wide screen: %s\n", page, w, theme, r.ScrollWidth, r.InnerWidth, w, strings.Join(r.Wide, "; "))
				}
			}
		}
		fmt.Printf("ok   %s\n", page)
	}
	if failed > 0 {
		fmt.Printf("%d page views wider than the screen\n", failed)
		b.close() // os.Exit skips defers, and the browser must not outlive us
		stopServer()
		os.Exit(1)
	}
	fmt.Printf("no horizontal overflow: %d pages × %d widths × 2 themes\n", len(pages), len(ws))
}

func findPages(site string) ([]string, error) {
	var out []string
	err := filepath.Walk(site, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".html") {
			rel, _ := filepath.Rel(site, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	if len(out) == 0 && err == nil {
		err = fmt.Errorf("no pages in %s", site)
	}
	return out, err
}

func shotName(page, theme string, w int) string {
	n := strings.TrimSuffix(strings.ReplaceAll(page, "/", "-"), ".html")
	return fmt.Sprintf("%s-%s-%d.png", n, theme, w)
}

func serve(dir string) (string, func(), error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(dir))}
	go srv.Serve(l)
	return "http://" + l.Addr().String(), func() { srv.Close() }, nil
}

// ---- a minimal DevTools protocol client ----

type browser struct {
	cmd     *exec.Cmd
	profile string
	conn    *websocket.Conn
	session string

	mu      sync.Mutex
	nextID  int
	waiting map[int]chan reply
	events  chan event
}

type reply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type event struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func launch(bin string) (*browser, error) {
	if bin == "" {
		for _, c := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
			if p, err := exec.LookPath(c); err == nil {
				bin = p
				break
			}
		}
	}
	if bin == "" {
		return nil, errors.New("no Chromium found; pass -chrome")
	}
	profile, err := os.MkdirTemp("", "sitecheck-")
	if err != nil {
		return nil, err
	}
	args := []string{"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--hide-scrollbars", "--remote-debugging-port=0", "--user-data-dir=" + profile}
	if os.Getenv("CI") != "" {
		// CI runners restrict the user namespaces Chrome's sandbox needs; the
		// only thing loaded is our own built site.
		args = append(args, "--no-sandbox")
	}
	cmd := exec.Command(bin, append(args, "about:blank")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so close can end every child
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(profile)
		return nil, err
	}
	b := &browser{cmd: cmd, profile: profile, waiting: map[int]chan reply{}, events: make(chan event, 64)}
	found := make(chan string, 1)
	var said []string
	var saidMu sync.Mutex
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if u, ok := strings.CutPrefix(sc.Text(), "DevTools listening on "); ok {
				found <- u
			}
			saidMu.Lock()
			said = append(said, sc.Text())
			saidMu.Unlock()
		}
		close(found)
	}()
	var wsURL string
	select {
	case wsURL = <-found:
	case <-time.After(30 * time.Second):
	}
	if wsURL == "" {
		b.close()
		saidMu.Lock()
		defer saidMu.Unlock()
		return nil, fmt.Errorf("browser did not start:\n%s", strings.Join(said, "\n"))
	}
	if b.conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil); err != nil {
		b.close()
		return nil, err
	}
	go b.read()
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := b.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &t); err != nil {
		b.close()
		return nil, err
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.call("", "Target.attachToTarget", map[string]any{"targetId": t.TargetID, "flatten": true}, &s); err != nil {
		b.close()
		return nil, err
	}
	b.session = s.SessionID
	if err := b.call(b.session, "Page.enable", nil, nil); err != nil {
		b.close()
		return nil, err
	}
	return b, nil
}

func (b *browser) close() {
	if b.conn != nil {
		b.conn.Close()
	}
	if b.cmd.Process != nil {
		syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		b.cmd.Wait()
	}
	os.RemoveAll(b.profile)
}

func (b *browser) read() {
	for {
		var m struct {
			ID int `json:"id"`
			reply
			event
		}
		if err := b.conn.ReadJSON(&m); err != nil {
			close(b.events)
			return
		}
		if m.ID != 0 {
			b.mu.Lock()
			ch := b.waiting[m.ID]
			delete(b.waiting, m.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- m.reply
			}
			continue
		}
		select {
		case b.events <- m.event:
		default:
		}
	}
}

func (b *browser) call(session, method string, params any, out any) error {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan reply, 1)
	b.waiting[id] = ch
	b.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	b.mu.Lock()
	err := b.conn.WriteJSON(msg)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s: no answer", method)
	}
}

type result struct {
	ScrollWidth int      `json:"sw"`
	InnerWidth  int      `json:"iw"`
	Wide        []string `json:"wide"`
}

// measure reports the page width and the outermost elements that stick out
// past the right edge, skipping anything inside a box that scrolls on its
// own (code blocks, wide tables).
const measure = `((iw) => {
	const wide = [];
	const scrolls = el => { for (let p = el.parentElement; p; p = p.parentElement) {
		const o = getComputedStyle(p).overflowX; if (o === "auto" || o === "scroll" || o === "hidden" || o === "clip") return true; } return false; };
	for (const el of document.body.querySelectorAll("*")) {
		const r = el.getBoundingClientRect();
		if (r.width && r.right > iw + 0.5 && !scrolls(el) && !(el.parentElement && el.parentElement.getBoundingClientRect().right > iw + 0.5)) {
			wide.push(el.tagName.toLowerCase() + (el.className && typeof el.className === "string" ? "." + el.className.trim().split(/\s+/).join(".") : "") + " " + Math.round(r.right) + "px");
		}
		if (wide.length >= 6) break;
	}
	return JSON.stringify({sw: document.documentElement.scrollWidth, iw, wide});
})(%d)`

func (b *browser) check(url string, width int, scale float64, theme, shots, name string) (result, error) {
	var r result
	if err := b.call(b.session, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": width, "height": 900, "deviceScaleFactor": scale, "mobile": width < 800,
	}, nil); err != nil {
		return r, err
	}
	if err := b.call(b.session, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{
		{"name": "prefers-color-scheme", "value": theme},
		{"name": "prefers-reduced-motion", "value": "reduce"},
	}}, nil); err != nil {
		return r, err
	}
	if err := b.call(b.session, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		return r, err
	}
	deadline := time.After(20 * time.Second)
wait:
	for {
		select {
		case e, ok := <-b.events:
			if !ok {
				return r, errors.New("browser went away")
			}
			if e.Method == "Page.loadEventFired" {
				break wait
			}
		case <-deadline:
			return r, errors.New("page did not load")
		}
	}
	time.Sleep(300 * time.Millisecond) // let the replay draw its final frame
	var ev struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := b.call(b.session, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf(measure, width), "returnByValue": true}, &ev); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(ev.Result.Value), &r); err != nil {
		return r, err
	}
	if shots != "" {
		var shot struct {
			Data string `json:"data"`
		}
		if err := b.call(b.session, "Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": true}, &shot); err != nil {
			return r, err
		}
		png, err := base64.StdEncoding.DecodeString(shot.Data)
		if err != nil {
			return r, err
		}
		if err := os.WriteFile(filepath.Join(shots, name), png, 0o644); err != nil {
			return r, err
		}
	}
	return r, nil
}
