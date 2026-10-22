# Contributing

Thanks for wanting to help. ferry tries to be small and to always work, so
changes are judged first on whether they keep it that way.

## Before you start

For anything bigger than a fix, open an issue first and say what you want
to change and why. It saves you writing code that does not fit.

## Building and testing

```
go build ./cmd/ferry
go test ./...
```

The rendezvous tests need a Concord rendezvous binary:

```
git clone https://github.com/ZahakJ/concord && (cd concord && go build -o ../rendezvous ./cmd/rendezvous)
FERRY_RV_BIN=$PWD/rendezvous go test ./internal/rv
```

The two-machine harness builds a small NATed internet in a user namespace
(Linux, no root) and runs every end-to-end check:

```
FERRY_RV_BIN=$PWD/rendezvous test/harness.sh
FERRY_HARNESS_ONLY="pair direct relay" test/harness.sh   # just some
```

It writes a few GB of test files; set `FERRY_HARNESS_DIR` to put them on a
disk with room.

## What a good change looks like

- It solves one problem, and the commit message says which and why.
- New behaviour has a test; network behaviour has a harness scenario.
- `gofmt`, `go vet` and `go test ./...` are clean.
- Errors tell the user what to do next. Output for machines (`--json`)
  stays stable: add fields, do not rename or remove them.
- No new cryptography. Compose audited libraries; explain the
  construction in SECURITY.md if you change one.
- No new dependencies without a reason in the pull request.

## Layout

| path | what |
|---|---|
| `cmd/ferry` | the CLI |
| `internal/pair` | codes and CPace |
| `internal/secure` | the sealed signaling channel and contact handshake |
| `internal/rv` | talking to a Concord rendezvous |
| `internal/link` | WebRTC lanes |
| `internal/xfer` | the file transfer protocol, resume, hashing |
| `internal/msg` | signed messages, mailbox envelopes, the message log |
| `internal/agent` | the per-machine agent and its unix-socket API |
| `internal/mcp` | the MCP server |
| `test/` | the harness and its tools |
| `docs/` | the documentation site sources |

## Conduct

Be kind and assume good faith. Harassment of any kind gets you removed.
