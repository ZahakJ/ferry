// Command mediagen draws the README's images: a banner with the strait from
// the landing page, and an animated replay of a transfer on two machines.
// Each comes in a light and a dark version.
//
//	go run ./tools/mediagen -out docs/media
//
// The images are plain SVG with CSS animation, so GitHub renders them as
// they are and they stay small.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

type palette struct {
	name                                 string
	bg, ink, soft, faint                 string
	sea, deep, wave, land, edge          string
	accent, navy, yellow, red, blue      string
	port, stbd                           string
	term, termInk, termDim, green, amber string
	cabin                                string
}

var light = palette{
	name: "", bg: "#e8f0ee", ink: "#0c2830", soft: "#3d5a62", faint: "#7a9399",
	sea: "#bcd8d6", deep: "#8fbfbe", wave: "#f8fbfa", land: "#e9dcc0", edge: "#c9b48a",
	accent: "#d9411e", navy: "#14365c", yellow: "#f2b90f", red: "#d63a2f", blue: "#1d4f8c",
	port: "#d63a2f", stbd: "#128a5c",
	term: "#0b1f28", termInk: "#d7e6e8", termDim: "#7a9aa2", green: "#6fdcae", amber: "#ffc95c",
	cabin: "#f8fbfa",
}

var dark = palette{
	name: "-dark", bg: "#06141c", ink: "#e2eef0", soft: "#9db7bd", faint: "#5f7d85",
	sea: "#0b2a38", deep: "#0e3446", wave: "#2d6a7c", land: "#13262c", edge: "#2a4650",
	accent: "#ff6a3d", navy: "#9cc4f0", yellow: "#ffcf4a", red: "#ff5a4c", blue: "#4f8fe0",
	port: "#ff5a4c", stbd: "#3ddc97",
	term: "#030c11", termInk: "#d7e6e8", termDim: "#7a9aa2", green: "#6fdcae", amber: "#ffc95c",
	cabin: "#0c222d",
}

const (
	mono = `ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace`
	sans = `system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", sans-serif`
)

func main() {
	out := flag.String("out", "docs/media", "output directory")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, p := range []palette{light, dark} {
		write(filepath.Join(*out, "banner"+p.name+".svg"), banner(p))
		write(filepath.Join(*out, "demo"+p.name+".svg"), demo(p))
	}
}

func write(path, s string) {
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s  %d bytes\n", path, len(s))
}

// ---- the strait, in the landing page's 1000×230 coordinates ----

const boatPath = `<path fill="%[1]s" d="M0 126 L78 126 L70 140 L8 140 Z"/>` +
	`<rect fill="%[2]s" stroke="%[1]s" stroke-width="2" x="12" y="110" width="46" height="16" rx="2"/>` +
	`<rect fill="%[3]s" x="42" y="98" width="9" height="12"/>` +
	`<rect fill="%[4]s" stroke="%[1]s" stroke-width="1.5" x="18" y="114" width="12" height="8"/>` +
	`<rect fill="%[4]s" stroke="%[1]s" stroke-width="1.5" x="32" y="114" width="8" height="8"/>` +
	`<circle fill="%[5]s" cx="4" cy="129" r="2.6"/><circle fill="%[6]s" cx="74" cy="129" r="2.6"/>`

func boat(p palette) string {
	return fmt.Sprintf(boatPath, p.navy, p.cabin, p.accent, p.yellow, p.port, p.stbd)
}

// shores draws everything but the boat, the hoists and the cargo.
func shores(p palette, id string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<defs><clipPath id="w%s"><rect x="150" y="128" width="700" height="102"/></clipPath></defs>`, id)
	fmt.Fprintf(&b, `<rect fill="%s" x="0" y="128" width="1000" height="102"/>`, p.sea)
	fmt.Fprintf(&b, `<rect fill="%s" x="0" y="186" width="1000" height="44"/>`, p.deep)
	fmt.Fprintf(&b, `<g clip-path="url(#w%s)" fill="none" stroke="%s" stroke-width="2" stroke-linecap="round" opacity="0.8">`, id, p.wave)
	b.WriteString(`<path class="drift" d="`)
	for x := 100; x < 1150; x += 120 {
		fmt.Fprintf(&b, "M%d %d q15 -6 30 0 t30 0 ", x, 196+(x/120%2)*16)
	}
	b.WriteString(`"/><path class="drift fast" d="`)
	for x := 130; x < 1180; x += 120 {
		fmt.Fprintf(&b, "M%d %d q12 -5 24 0 t24 0 ", x, 158+(x/120%2)*14)
	}
	b.WriteString(`"/></g>`)
	fmt.Fprintf(&b, `<g fill="%s" stroke="%s" stroke-width="1.5">`, p.land, p.edge)
	b.WriteString(`<path d="M0 70 L95 66 Q130 70 150 104 L168 130 L168 230 L0 230 Z"/>`)
	b.WriteString(`<path d="M1000 70 L905 66 Q870 70 850 104 L832 130 L832 230 L1000 230 Z"/></g>`)
	fmt.Fprintf(&b, `<g fill="%s"><rect x="150" y="126" width="62" height="7" rx="1"/><rect x="788" y="126" width="62" height="7" rx="1"/></g>`, p.edge)
	fmt.Fprintf(&b, `<g stroke="%s" stroke-width="2"><line x1="40" y1="68" x2="40" y2="14"/><line x1="960" y1="68" x2="960" y2="14"/></g>`, p.soft)
	fmt.Fprintf(&b, `<g stroke="%s" stroke-width="1" fill="none"><path d="M40 16 L140 52"/><path d="M960 16 L860 52"/></g>`, p.faint)
	return b.String()
}

// hoist strings words as pennants from a mast, like the landing page does.
func hoist(p palette, side string, words []string) string {
	var b strings.Builder
	x0, dir := 40.0, 1.0
	if side == "r" {
		x0, dir = 960, -1
	}
	colors := []string{p.yellow, p.red, p.blue}
	for i, w := range words {
		x := x0 + dir*(22+float64(i)*34)
		y := 16 + (x-x0)*dir*0.36
		fmt.Fprintf(&b, `<path fill="%s" d="M%.1f %.1f L%.1f %.1f L%.1f %.1f Z"/>`, colors[i%3], x-9, y, x+9, y+6.5, x, y+22)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="%s" font-family='%s' style="font-size:11px" font-weight="600">%s</text>`, x, y+36, p.ink, mono, w)
	}
	return b.String()
}

func crate(p palette) string {
	return fmt.Sprintf(`<rect fill="%s" stroke="%s" stroke-width="1.5" x="796" y="104" width="46" height="22" rx="2"/>`+
		`<text x="819" y="119" text-anchor="middle" fill="%s" font-family='%s' style="font-size:10px" font-weight="700">✓ 2.1 GB</text>`,
		p.yellow, p.navy, "#0b1f28", mono)
}

func driftStyle() string {
	return `@keyframes drift{to{transform:translateX(-120px)}}.drift{animation:drift 15s linear infinite;opacity:.6}.drift.fast{animation-duration:9s;opacity:1}` +
		`@media (prefers-reduced-motion:reduce){*{animation:none!important}}`
}

// ---- banner: 1600×520 ----

func banner(p palette) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1600 520" width="1600" height="520">`)
	b.WriteString(`<title>ferry: hand a file across</title>`)
	fmt.Fprintf(&b, `<style>%s</style>`, driftStyle())
	fmt.Fprintf(&b, `<rect width="1600" height="520" fill="%s"/>`, p.bg)
	// Wordmark and line, centred over the strait.
	fmt.Fprintf(&b, `<g transform="translate(800 0)" text-anchor="middle">`)
	fmt.Fprintf(&b, `<text y="128" fill="%s" font-family='%s' font-size="132" font-weight="800" letter-spacing="-5">ferry</text>`, p.ink, sans)
	fmt.Fprintf(&b, `<text y="178" fill="%s" font-family='%s' font-size="30">Hand a file <tspan fill="%s" font-weight="700">across</tspan>. Direct, encrypted, no accounts.</text>`, p.soft, sans, p.accent)
	b.WriteString(`</g>`)
	fmt.Fprintf(&b, `<g transform="translate(0 152) scale(1.6)">`)
	b.WriteString(shores(p, "b"))
	b.WriteString(hoist(p, "l", []string{"motto", "sway"}))
	b.WriteString(hoist(p, "r", []string{"motto", "sway"}))
	x := 214 + (708-214)*0.56
	fmt.Fprintf(&b, `<path fill="none" stroke="%s" stroke-width="2.5" stroke-linecap="round" stroke-dasharray="6 9" opacity="0.9" d="M214 140 L%.0f 140"/>`, p.wave, x)
	fmt.Fprintf(&b, `<g transform="translate(%.0f 0)">%s</g>`, x, boat(p))
	b.WriteString(`</g></svg>`)
	return b.String() + "\n"
}

// ---- demo: 1600×760, animated, one 18-second loop ----

const loop = 18.0

type anim struct {
	css strings.Builder
	n   int
}

// show returns a class that makes an element visible from t0 to t1 seconds
// into the loop (t1 = 0: until the loop ends).
func (a *anim) show(t0, t1 float64) string {
	a.n++
	c := fmt.Sprintf("s%d", a.n)
	if t1 == 0 {
		t1 = loop - 0.6
	}
	p0, p1 := 100*t0/loop, 100*t1/loop
	fmt.Fprintf(&a.css, "@keyframes %s{0%%,%.2f%%{opacity:0}%.2f%%,%.2f%%{opacity:1}%.2f%%,100%%{opacity:0}}.%s{opacity:0;animation:%s %.0fs step-end infinite}",
		c, p0, p0+0.01, p1, p1+0.01, c, c, loop)
	return c
}

// move slides from x0 to x1 between t0 and t1, holds, and jumps back.
func (a *anim) move(x0, x1, t0, t1 float64) string {
	a.n++
	c := fmt.Sprintf("m%d", a.n)
	fmt.Fprintf(&a.css, "@keyframes %s{0%%,%.2f%%{transform:translateX(%.1fpx)}%.2f%%,%.2f%%{transform:translateX(%.1fpx)}100%%{transform:translateX(%.1fpx)}}.%s{animation:%s %.0fs linear infinite}",
		c, 100*t0/loop, x0, 100*t1/loop, 100*(loop-0.6)/loop, x1, x0, c, c, loop)
	return c
}

// grow scales a bar from nothing to full between t0 and t1.
func (a *anim) grow(t0, t1 float64) string {
	a.n++
	c := fmt.Sprintf("g%d", a.n)
	fmt.Fprintf(&a.css, "@keyframes %s{0%%,%.2f%%{transform:scaleX(0)}%.2f%%,%.2f%%{transform:scaleX(1)}100%%{transform:scaleX(0)}}.%s{transform-box:fill-box;transform-origin:left;animation:%s %.0fs linear infinite}",
		c, 100*t0/loop, 100*t1/loop, 100*(loop-0.6)/loop, c, c, loop)
	return c
}

type term struct {
	x, y, w, h float64
	title      string
	line       int
}

const (
	termFont = 18.0
	lineH    = 28.0
)

func (t *term) next() (float64, float64) {
	y := t.y + 62 + float64(t.line)*lineH
	t.line++
	return t.x + 22, y
}

// span is one coloured run of a terminal line.
type span struct{ color, text string }

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func textLine(b *strings.Builder, x, y float64, class string, spans ...span) {
	fmt.Fprintf(b, `<text class="%s" x="%.0f" y="%.0f" xml:space="preserve">`, class, x, y)
	for _, s := range spans {
		fmt.Fprintf(b, `<tspan fill="%s">%s</tspan>`, s.color, esc(s.text))
	}
	b.WriteString(`</text>`)
}

func demo(p palette) string {
	var a anim
	var b strings.Builder
	code := "7-acorn-tulip-otter"
	words := []string{"acorn", "tulip", "otter"}
	const (
		tCode    = 1.4
		tGet     = 2.8
		tConnect = 3.6
		tStart   = 4.2
		tEnd     = 11.2
	)

	// The strait across the top, the two terminals below on their shores.
	var scene strings.Builder
	scene.WriteString(shores(p, "d"))
	fmt.Fprintf(&scene, `<g class="%s">%s</g>`, a.show(tCode, tConnect), hoist(p, "l", words))
	fmt.Fprintf(&scene, `<g class="%s">%s</g>`, a.show(tGet+0.4, tConnect), hoist(p, "r", words))
	fmt.Fprintf(&scene, `<g class="%s">%s%s</g>`, a.show(tConnect, 0), hoist(p, "l", []string{"motto", "sway"}), hoist(p, "r", []string{"motto", "sway"}))
	fmt.Fprintf(&scene, `<g class="%s">%s</g>`, a.show(tEnd+0.2, 0), crate(p))
	fmt.Fprintf(&scene, `<g class="%s"><g transform="translate(214 0)">%s</g></g>`, a.move(0, 708-214, tStart, tEnd), boat(p))

	left := term{x: 30, y: 384, w: 760, h: 346, title: "ana@laptop"}
	right := term{x: 810, y: 384, w: 760, h: 346, title: "sam@desktop"}

	var lines strings.Builder
	prompt := span{p.termDim, "$ "}
	ink := func(s string) span { return span{p.termInk, s} }
	dim := func(s string) span { return span{p.termDim, s} }
	ok := span{p.green, "✓ "}
	at := func(t *term, t0, t1 float64, spans ...span) {
		x, y := t.next()
		textLine(&lines, x, y, a.show(t0, t1), spans...)
	}

	at(&left, 0.3, 0, prompt, ink("ferry send ./holiday.mp4"))
	at(&left, 0.8, 0, ink("Sending holiday.mp4 (2.1 GB)."))
	at(&left, 1.0, 0, ink("On the other machine, run:"))
	at(&left, tCode, 0, ink("    ferry get "), span{p.amber, code})
	at(&left, 1.6, 0, dim("Waiting for them… (Ctrl-C to cancel)"))
	at(&left, tConnect, 0, ink("Connected. "), dim("(both sides see: motto sway)"))

	at(&right, tGet, 0, prompt, ink("ferry get "+code))
	at(&right, tConnect, 0, ink("Connected. "), dim("(both sides see: motto sway)"))
	at(&right, tConnect+0.3, 0, ink("Receiving holiday.mp4 (2.1 GB)."))

	// Progress: a bar that fills, and the numbers stepping beside it.
	bar := func(t *term, arrow string, full bool) {
		x, y := t.next()
		w := 14 * termFont * 0.6
		fmt.Fprintf(&lines, `<g class="%s"><rect x="%.0f" y="%.0f" width="%.0f" height="4" fill="%s"/>`, a.show(tStart, tEnd), x, y-7, w, p.termDim)
		fmt.Fprintf(&lines, `<rect class="%s" x="%.0f" y="%.0f" width="%.0f" height="4" fill="%s"/></g>`, a.grow(tStart, tEnd), x, y-7, w, p.green)
		steps := 10
		for i := 0; i <= steps; i++ {
			pct := float64(i) * 100 / float64(steps)
			t0 := tStart + (tEnd-tStart)*float64(i)/float64(steps)
			t1 := tStart + (tEnd-tStart)*float64(i+1)/float64(steps)
			if i == steps {
				t1 = tEnd
			}
			s := fmt.Sprintf(" %s %5.1f%%", arrow, pct)
			if full {
				left := int((100 - pct) / 100 * 19)
				s += fmt.Sprintf("  %.1f GB of 2.1 GB  %.1f MB/s · %ds left", 2.1*pct/100, 108+6*float64(i%3-1), left)
			}
			textLine(&lines, x+w, y, a.show(t0, t1), ink(s))
		}
	}
	bar(&right, "↓", true)
	bar(&left, "↑", false)
	left.line--
	right.line--

	at(&right, tEnd, 0, ok, ink("Received 2.1 GB in 19s (110.5 MB/s)."))
	at(&right, tEnd, 0, ink("  Checked: every byte matches."))
	at(&right, tEnd+0.2, 0, ink("holiday.mp4"))
	at(&left, tEnd, 0, ok, ink("Sent 2.1 GB in 19s (110.5 MB/s, direct)."))
	at(&left, tEnd+1.2, 0, prompt, ink(`ferry msg sam "the director's cut, enjoy"`))
	at(&left, tEnd+1.8, 0, ok, ink("Sent to sam, delivered."))
	at(&right, tEnd+2.6, 0, prompt, ink("ferry inbox"))
	at(&right, tEnd+3.2, 0, dim("#12 ← "), ink("ana        "), dim("21:04"), ink("  the director's cut, enjoy"))

	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1600 760" width="1600" height="760">`)
	b.WriteString(`<title>A transfer with ferry, as it looks on both machines</title>`)
	fmt.Fprintf(&b, `<style>%s%s text{font-family:%s;font-size:%.0fpx}</style>`, driftStyle(), a.css.String(), mono, termFont)
	fmt.Fprintf(&b, `<rect width="1600" height="760" fill="%s"/>`, p.bg)
	fmt.Fprintf(&b, `<g transform="translate(0 6) scale(1.6)">%s</g>`, scene.String())
	for i, t := range []term{left, right} {
		light := p.port
		if i == 1 {
			light = p.stbd
		}
		fmt.Fprintf(&b, `<rect x="%.0f" y="%.0f" width="%.0f" height="%.0f" rx="8" fill="%s"/>`, t.x, t.y, t.w, t.h, p.term)
		fmt.Fprintf(&b, `<rect x="%.0f" y="%.0f" width="%.0f" height="4" fill="%s"/>`, t.x, t.y, t.w, light)
		fmt.Fprintf(&b, `<circle cx="%.0f" cy="%.0f" r="5" fill="%s"/>`, t.x+24, t.y+26, light)
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" fill="%s" style="font-size:15px">%s</text>`, t.x+40, t.y+31, p.termDim, t.title)
		fmt.Fprintf(&b, `<rect x="%.0f" y="%.0f" width="%.0f" height="1" fill="%s" opacity="0.15"/>`, t.x, t.y+44, t.w, p.termInk)
	}
	b.WriteString(lines.String())
	b.WriteString(`</svg>`)
	return b.String() + "\n"
}
