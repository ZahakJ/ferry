# ferry

Send files and messages between two machines, yours and a friend's.
No accounts, no port forwarding, end-to-end encrypted.

```
ferry send ./build.tar.zst
```

It prints a code like `7-acorn-tulip-otter`. On the other machine:

```
ferry get 7-acorn-tulip-otter
```

That is the whole thing. The file goes straight from one machine to the
other as fast as the slower connection allows, survives a dropped Wi-Fi,
resumes if interrupted, and is checked byte for byte on arrival.

## Pair once, then use names

```
ferry pair                    # prints a code; the friend runs: ferry pair <code>
ferry send sam ./photos       # from now on, no codes
ferry msg sam "pushed branch x, have a look"
ferry inbox
ferry wait --from sam --timeout 10m
```

Messages reach a friend who is offline too: they wait, sealed, in a
mailbox until the friend is next online. Files need both machines online;
`ferry up` keeps a machine reachable in the background.

## For Claude Code and other agents

ferry is an MCP server:

```
claude mcp add ferry -s user -- ferry mcp
```

Two Claude Code sessions on two machines can then message each other,
wait for replies, and hand over files and patches. The tools are
`send_message`, `read_messages`, `wait_for_message`, `send_file`,
`list_files_received`, `fetch_file`, `list_contacts` and `channel_status`.

Shell-only agents get the same through the CLI: every command takes
`--json`, writes one JSON event per line to stdout and progress to stderr,
never prompts, and exits with a meaningful code (`ferry help exit-codes`).
`ferry help --llm` prints a one-page manual written for a model.

What arrives through ferry is data from someone else. ferry never runs
it, fetched files lose their execute bit, and the MCP server tells the
model to treat received text as untrusted.

## Install

One static binary for Linux and macOS:

```
go install github.com/ZahakJ/ferry/cmd/ferry@latest
```

To reach each other across the internet, two machines need a rendezvous
server to introduce them, and a way through their routers. Run your own
rendezvous ([deploy/fly](deploy/fly) has the steps for fly.io; any machine
with a public address works) and point both machines at it:

```
ferry config rendezvous /dns/rv.example.org/tcp/4001/p2p/12D3KooW...
```

If a router blocks the direct connection, forward one UDP port to the
machine behind it and tell ferry to use it:

```
ferry config port 40000
ferry check
```

`ferry check` says what works and what does not: the rendezvous, your
public address, the port.

STUN (how each machine learns its public address) defaults to a public
server. To use your own, set `FERRY_STUN=stun:stun.example.org:3478`, or
`"stun": ["stun:…"]` in `config.json` in `ferry config`'s folder.

## Borrow a machine

Someone lends you a box on their machine (`lend`). You get a code and run:

```sh
ferry join 44-stray-stick-judge     # once
ferry shell zman-box                # or just: ssh zman-box
```

It is a one-way mirror: your machine reaches into the box, and the box has
no way back into yours. To work on a project there, copy it in:

```sh
cd ~/proj && ferry bring            # lands at the same path in the box, with your Claude sessions
cd ~/proj && ferry take             # copies it (and sessions started there) back here
```

`ferry bring` puts the folder at **the same path** in the box
(`/home/you/proj`), so paths in your notes, scripts and Claude sessions mean
the same thing, and `claude --resume` there lists the sessions you had here.
It also brings your `~/.claude/CLAUDE.md` and your git name and email. It
leaves out secret-looking files (`.env*`, `*.pem`, `id_*`, `.ssh/`, `.aws/`,
`.gnupg/`, `.netrc`, `credentials*.json`, `.git-credentials`, a `.git/config`
with a token in a URL; `--include-secrets` brings them) and `node_modules`,
`.venv`, `__pycache__`, `target` (`--all`). Running it again sends only what
changed and keeps anything that is newer in the box. `ferry take` never
overwrites a file that is newer here, writes nothing outside the folder or
through a symlink, and keeps your own `.git/config` and hooks.

On your machine, `~/zman-box` is the box's copy of your home (with sshfs),
so your own editor works on what you brought.

While `ferry shell` is open, a server you start in the box is on your
machine too: run `npm run dev` there and open http://localhost:3000 here.
ferry checks the box every 2 s and forwards each port you listen on
(1024-32767) to 127.0.0.1 on your machine; if that port is taken here it
uses the next free one, and the box's tmux status line says which.
`ferry ports zman-box` lists them. `ferry shell --no-ports`, or
`ferry config box-ports off`, turns it off.

Also while it is open, the box's web traffic can go out through your
connection (useful when the lender limits the box's own): the box's
`127.0.0.1:1080` is a SOCKS proxy that ferry runs on your machine, which
only connects to public internet addresses. `ferry join --no-internet`,
`ferry shell --no-internet` or `ferry config box-internet off` turns it off.

`ferry leave zman-box` undoes all of it. See SECURITY.md.

## SSH to a paired machine

`ferry pipe` connects stdin/stdout to a TCP port on a contact's localhost,
over the same direct, end-to-end encrypted link, with no open ports on
either side. On the machine to reach, allow exactly the ports you mean
(nothing is allowed by default; every pipe is logged):

```sh
ferry up --allow-pipe 22
```

On the other, after pairing:

```sh
ssh -o ProxyCommand='ferry pipe box 22' me@box
# or in ~/.ssh/config:  Host box
#                         ProxyCommand ferry pipe box 22
```

## How it works

1. **Meeting.** Both sides connect out to the rendezvous. The code's
   number is a meeting slot there; its words never leave your machine.
2. **Agreeing on a key.** The two sides run CPace, a password-authenticated
   key exchange, over the words. Someone watching, or the rendezvous
   itself, learns nothing about them, and a wrong guess burns the code:
   one try, then both sides stop.
3. **Connecting.** Sealed under that key, they swap WebRTC offers. Those
   carry the fingerprints of the encryption keys the data connection will
   use, so nobody in the middle can substitute their own. Then ICE punches
   through both NATs. Data only ever goes directly between the two
   machines; if no direct path exists, ferry says so and stops.
4. **Moving data.** The transfer is split across several connections at
   once (one alone recovers slowly from loss on long paths). Each piece
   carries a BLAKE3 hash and each file is hashed whole; nothing gets its
   real name until it matches.

Pairing does the same once and then swaps long-term keys. Later sessions
between contacts authenticate with those keys instead of a code.

The rendezvous is not trusted with anything but availability. The
[threat model](SECURITY.md) spells out what it and others can and cannot
do.

## Speed

Measured by the test harness (`test/harness.sh`): two machines, each behind
its own NAT, on one 16-core Linux box; netem shapes the path.

| path | ferry | one TCP stream |
|---|---|---|
| loopback, one machine | 2.0 Gbps | |
| LAN-like, through both NATs | 1.2–3.3 Gbps | |
| 40 ms round trip | 0.9–2.1 Gbps | 0.8 Gbps |
| 40 ms round trip, 0.5% loss | 0.64–0.69 Gbps | 0.4 Gbps |

Ranges are across runs; both ends share one machine's CPUs, so real
machines on a real LAN do better.
How the numbers were found and what was tuned: [docs/throughput.md](docs/throughput.md).

## Tested

`test/harness.sh` builds two NATed homes, a rendezvous and a STUN server
inside a user namespace (no root needed) and checks: pairing; a wrong code
rejected on both sides; a direct path through both NATs; a plain "no
direct path" when UDP between them is blocked; a forwarded port through
strict NATs; no plaintext anywhere on the wire, rendezvous included; a
20 GiB file (`FERRY_HARNESS_BIG=20480`) killed part way over a 40 ms, lossy path and resumed with a
matching hash; and two MCP clients exchanging messages and a file.

Tested with 20 GiB files. Offsets are 64-bit and nothing holds a whole
file in memory: during a 20 GiB transfer the sender used under 200 MB of
RAM and the receiver about 130 MB.

## License

MIT
