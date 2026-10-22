// Command mcpdrive plays two model clients talking through ferry's MCP
// servers, the way two Claude Code sessions would.
//
//	mcpdrive -a 'cmd that runs ferry mcp as avi' -b '... as zyad' -file ./x.bin -dest ./got
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type client struct {
	name string
	in   io.WriteCloser
	mu   sync.Mutex
	next int
	wait map[int]chan json.RawMessage
}

func start(name, command string) *client {
	cmd := exec.Command("sh", "-c", command)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fail("%s: %v", name, err)
	}
	c := &client{name: name, in: in, wait: map[int]chan json.RawMessage{}}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var r struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.wait[r.ID]
			c.mu.Unlock()
			if ch != nil {
				if r.Error != nil {
					ch <- json.RawMessage(`{"rpcError":` + string(r.Error) + `}`)
				} else {
					ch <- r.Result
				}
			}
		}
	}()
	return c
}

func (c *client) rpc(method string, params any) json.RawMessage {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan json.RawMessage, 1)
	c.wait[id] = ch
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	c.in.Write(append(b, '\n'))
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Minute):
		fail("%s: %s timed out", c.name, method)
	}
	return nil
}

// tool calls a tool and returns its structured result, failing on isError.
func (c *client) tool(name string, args map[string]any) map[string]any {
	raw := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	var r struct {
		IsError    bool                    `json:"isError"`
		Content    []struct{ Text string } `json:"content"`
		Structured map[string]any          `json:"structuredContent"`
	}
	json.Unmarshal(raw, &r)
	if r.IsError {
		text := ""
		if len(r.Content) > 0 {
			text = r.Content[0].Text
		}
		fail("%s: %s failed: %s", c.name, name, text)
	}
	return r.Structured
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "FAIL "+format+"\n", a...)
	os.Exit(1)
}

func pass(format string, a ...any) { fmt.Printf("PASS "+format+"\n", a...) }

func main() {
	aCmd := flag.String("a", "", "command running ferry mcp for the first side (contact name of the other: -bname)")
	bCmd := flag.String("b", "", "command for the second side")
	aName := flag.String("aname", "avi", "the first side's name as the second knows it")
	bName := flag.String("bname", "zyad", "the second side's name as the first knows it")
	file := flag.String("file", "", "file for the first side to send")
	dest := flag.String("dest", "", "where the second side fetches it to")
	flag.Parse()

	a, b := start("A", *aCmd), start("B", *bCmd)
	init := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "mcpdrive", "version": "1"}}
	for _, c := range []*client{a, b} {
		var r struct {
			ServerInfo   struct{ Name string } `json:"serverInfo"`
			Instructions string                `json:"instructions"`
		}
		json.Unmarshal(c.rpc("initialize", init), &r)
		if r.ServerInfo.Name != "ferry" || !strings.Contains(r.Instructions, "untrusted") {
			fail("%s: bad initialize: %+v", c.name, r)
		}
		var tl struct{ Tools []struct{ Name string } }
		json.Unmarshal(c.rpc("tools/list", map[string]any{}), &tl)
		var names []string
		for _, t := range tl.Tools {
			names = append(names, t.Name)
		}
		for _, want := range []string{"send_message", "read_messages", "wait_for_message", "send_file", "list_files_received", "fetch_file", "list_contacts", "channel_status"} {
			if !strings.Contains(strings.Join(names, " "), want) {
				fail("%s: tool %s missing (have %v)", c.name, want, names)
			}
		}
	}
	pass("initialize and tools/list on both sides")

	// Both sides must be listening before live delivery can work.
	for _, c := range []*client{a, b} {
		ok := false
		for i := 0; i < 60 && !ok; i++ {
			st := c.tool("channel_status", map[string]any{})
			ok = st["listening"] == true
			if !ok {
				time.Sleep(500 * time.Millisecond)
			}
		}
		if !ok {
			fail("%s: never started listening", c.name)
		}
	}
	pass("channel_status: both listening")

	cs := a.tool("list_contacts", map[string]any{})
	if !strings.Contains(fmt.Sprint(cs["contacts"]), *bName) {
		fail("A's contacts lack %s: %v", *bName, cs)
	}
	pass("list_contacts")

	got := make(chan map[string]any, 1)
	go func() {
		got <- b.tool("wait_for_message", map[string]any{"from": *aName, "timeout_seconds": 60})
	}()
	time.Sleep(time.Second)
	sent := a.tool("send_message", map[string]any{"to": *bName, "type": "request", "text": "I pushed branch x, please review it", "data": map[string]any{"branch": "x"}})
	w := <-got
	m, _ := w["message"].(map[string]any)
	if m == nil || m["text"] != "I pushed branch x, please review it" || m["type"] != "request" || m["id"] != sent["id"] {
		fail("B's wait_for_message got %v", w)
	}
	pass("send_message -> wait_for_message (via %v)", sent["via"])

	b.tool("send_message", map[string]any{"to": *aName, "text": "reviewed: LGTM", "reply_to": sent["id"]})
	var found bool
	for i := 0; i < 20 && !found; i++ {
		r := a.tool("read_messages", map[string]any{"since": 0, "from": *bName})
		for _, x := range r["messages"].([]any) {
			mm := x.(map[string]any)
			if mm["text"] == "reviewed: LGTM" && mm["reply_to"] == sent["id"] && mm["dir"] == "in" {
				found = true
			}
		}
		if !found {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !found {
		fail("A never saw the reply in read_messages")
	}
	pass("reply -> read_messages with reply_to")

	if *file != "" {
		r := a.tool("send_file", map[string]any{"to": *bName, "path": *file})
		pass("send_file: %v bytes in %.2fs (%v)", r["bytes"], r["seconds"], r["path"])
		var seq any
		for i := 0; i < 20 && seq == nil; i++ {
			fs := b.tool("list_files_received", map[string]any{})
			for _, x := range fs["files"].([]any) {
				f := x.(map[string]any)
				if f["name"] == filepath.Base(*file) {
					seq = f["seq"]
				}
			}
			if seq == nil {
				time.Sleep(500 * time.Millisecond)
			}
		}
		if seq == nil {
			fail("B never listed the file")
		}
		f := b.tool("fetch_file", map[string]any{"seq": seq, "dest": *dest})
		want, _ := os.ReadFile(*file)
		have, err := os.ReadFile(f["path"].(string))
		if err != nil || !bytes.Equal(sha(want), sha(have)) {
			fail("fetched file differs: %v", err)
		}
		pass("list_files_received -> fetch_file, sha256 matches")
	}
	fmt.Println("PASS mcp round trip")
}

func sha(b []byte) []byte { h := sha256.Sum256(b); return h[:] }
