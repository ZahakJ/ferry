// Command sitegen builds the ferry website: the hand-written landing page
// from web/, and the docs pages from Markdown.
//
//	go run ./tools/sitegen -ferry ./bin/ferry -out site
//
// The CLI reference page is generated from `ferry help --reference`, so it
// always matches the binary.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

type page struct {
	Slug, Title string
	Source      string // repository path, for "improve this page"
	Order       int
	Body        template.HTML
	Prev, Next  *page
}

func main() {
	ferry := flag.String("ferry", "./bin/ferry", "ferry binary, for the CLI reference")
	out := flag.String("out", "site", "output directory")
	flag.Parse()
	if err := build(*ferry, *out); err != nil {
		fmt.Fprintln(os.Stderr, "sitegen:", err)
		os.Exit(1)
	}
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

var front = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n`)

func build(ferry, out string) error {
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(out, "docs"), 0o755); err != nil {
		return err
	}
	// The landing page and assets are copied as they are.
	for _, f := range []string{"index.html", "style.css", "demo.js", "install.sh"} {
		if err := copyFile(filepath.Join("web", f), filepath.Join(out, f)); err != nil {
			return err
		}
	}
	if err := copyFile("web/style.css", filepath.Join(out, "docs", "style.css")); err != nil {
		return err
	}

	var pages []*page
	files, _ := filepath.Glob("docs/*.md")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		slug := strings.TrimSuffix(filepath.Base(f), ".md")
		p := &page{Slug: slug, Title: slug, Order: 50, Source: filepath.ToSlash(f)}
		if m := front.FindSubmatch(b); m != nil {
			for _, line := range strings.Split(string(m[1]), "\n") {
				k, v, _ := strings.Cut(line, ":")
				v = strings.TrimSpace(v)
				switch strings.TrimSpace(k) {
				case "title":
					p.Title = v
				case "order":
					p.Order, _ = strconv.Atoi(v)
				}
			}
			b = b[len(m[0]):]
		}
		if slug == "throughput" {
			p.Title, p.Order = "Measurements", 6
		}
		if err := render(p, b); err != nil {
			return err
		}
		pages = append(pages, p)
	}
	// The threat model lives at the repository root, where GitHub shows it.
	sec, err := os.ReadFile("SECURITY.md")
	if err != nil {
		return err
	}
	sp := &page{Slug: "security", Title: "Threat model", Order: 5, Source: "SECURITY.md"}
	if err := render(sp, sec); err != nil {
		return err
	}
	pages = append(pages, sp)
	// The CLI reference comes from the binary.
	ref, err := exec.Command(ferry, "help", "--reference").Output()
	if err != nil {
		return fmt.Errorf("ferry help --reference: %w", err)
	}
	cp := &page{Slug: "cli", Title: "CLI reference", Order: 4, Source: "cmd/ferry/help.go"}
	if err := render(cp, append([]byte("# CLI reference\n\n"), ref...)); err != nil {
		return err
	}
	pages = append(pages, cp)

	sort.SliceStable(pages, func(i, j int) bool { return pages[i].Order < pages[j].Order })
	for i, p := range pages {
		if i > 0 {
			p.Prev = pages[i-1]
		}
		if i+1 < len(pages) {
			p.Next = pages[i+1]
		}
	}
	for _, p := range pages {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, map[string]any{"Page": p, "Pages": pages}); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "docs", p.Slug+".html"), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	// GitHub Pages: serve files as they are.
	return os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o644)
}

// links rewrites links to Markdown sources into links to their pages.
var mdLink = regexp.MustCompile(`href="(?:docs/)?([a-z0-9-]+)\.md(#[^"]*)?"`)

func render(p *page, src []byte) error {
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return err
	}
	s := mdLink.ReplaceAllStringFunc(buf.String(), func(m string) string {
		sub := mdLink.FindStringSubmatch(m)
		slug := sub[1]
		if slug == "SECURITY" {
			slug = "security"
		}
		return `href="` + slug + `.html` + sub[2] + `"`
	})
	s = strings.ReplaceAll(s, `href="SECURITY.md"`, `href="security.html"`)
	p.Body = template.HTML(labelCells(s))
	return nil
}

var (
	tableRE = regexp.MustCompile(`(?s)<table>.*?</table>`)
	thRE    = regexp.MustCompile(`(?s)<th[^>]*>(.*?)</th>`)
	rowRE   = regexp.MustCompile(`(?s)<tr>(.*?)</tr>`)
	tdRE    = regexp.MustCompile(`<td([^>]*)>`)
	tagRE   = regexp.MustCompile(`<[^>]+>`)
)

// labelCells gives each table cell its column's heading as data-label, so
// that on a phone the stylesheet can show every row as a small card of
// "heading: value" lines instead of a table wider than the screen.
func labelCells(s string) string {
	return tableRE.ReplaceAllStringFunc(s, func(t string) string {
		var heads []string
		for _, m := range thRE.FindAllStringSubmatch(t, -1) {
			heads = append(heads, strings.TrimSpace(tagRE.ReplaceAllString(m[1], "")))
		}
		return rowRE.ReplaceAllStringFunc(t, func(row string) string {
			i := 0
			return tdRE.ReplaceAllStringFunc(row, func(td string) string {
				attrs := tdRE.FindStringSubmatch(td)[1]
				label := ""
				if i < len(heads) {
					label = heads[i]
				}
				i++
				return `<td` + attrs + ` data-label="` + template.HTMLEscapeString(label) + `">`
			})
		})
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var tmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Page.Title}} · ferry</title>
<link rel="stylesheet" href="style.css">
</head>
<body>
<header class="wrap top">
	<a class="mark" href="../">
		<svg viewBox="0 0 32 32" aria-hidden="true"><path d="M3 19h26l-3.5 6.5h-19z" fill="currentColor"/><rect x="8" y="13" width="15" height="6" rx="1" fill="none" stroke="currentColor" stroke-width="2"/><rect class="funnel" x="17" y="8" width="4" height="5"/><path d="M2 28.5q3.5-2 7 0t7 0 7 0 7 0" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" opacity=".5"/></svg>
		ferry
	</a>
	<nav><a href="quickstart.html">Docs</a><a href="https://github.com/ZahakJ/ferry">GitHub</a></nav>
</header>
<main class="wrap docs">
	<aside><nav>{{range .Pages}}<a href="{{.Slug}}.html"{{if eq .Slug $.Page.Slug}} class="here"{{end}}>{{.Title}}</a>{{end}}</nav></aside>
	<article>
{{.Page.Body}}
		<div class="pager">
			<span>{{with .Page.Prev}}← <a href="{{.Slug}}.html">{{.Title}}</a>{{end}}</span>
			<span>{{with .Page.Next}}<a href="{{.Slug}}.html">{{.Title}}</a> →{{end}}</span>
		</div>
	</article>
</main>
<footer><div class="wrap"><span>ferry · MIT licence</span><span>{{if .Page.Source}}<a href="https://github.com/ZahakJ/ferry/edit/main/{{.Page.Source}}">Improve this page</a>{{end}}</span></div></footer>
</body>
</html>
`))
