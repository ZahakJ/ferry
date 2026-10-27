# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/). The `--json` output and
exit codes are part of the public interface.

## [Unreleased]

### Added

- `ferry join <code>` uses a machine someone lends you (a "box"): it pairs
  and adds `Host <box>` to `~/.ssh/config`. It is a one-way mirror: your
  machine reaches into the box and the box has no way back into yours.
  `ferry shell <box>` mounts the box's copy of your home at `~/<box>` and
  logs in. `ferry leave <box>` closes the sessions, unmounts, and removes
  the ssh config block, the box's host key and the pairing.
- `ferry bring [folder]` copies a folder inside your home into the box at
  the same path, with your Claude sessions for it (so `claude --resume`
  works there), your `~/.claude/CLAUDE.md` and your git name and email. It
  leaves out secret-looking files (`--include-secrets`) and
  `node_modules`/`.venv`/`__pycache__`/`target` (`--all`), sends only what
  changed, and keeps files that are newer in the box. `ferry take [folder]`
  copies it and its sessions back, never over a newer file here, never
  outside the folder or through a symlink, and never git's config or hooks.
- While `ferry shell` is open, the box's web traffic can go out through
  your connection: the box's `127.0.0.1:1080` is a SOCKS5 proxy on your
  machine that only connects to public internet addresses (it resolves
  names itself and dials the checked address). `--no-internet` or
  `ferry config box-internet off` turns it off.
- `ferry pipe <contact>` with no port uses the one port the other side serves.
- `ferry shell <box>` shares the box's ports: a server started in the box
  (`npm run dev` on :3000) is http://localhost:3000 on your machine while
  the shell is open, on 127.0.0.1 only, or on the next free port if that one
  is taken (the box's tmux says which). `ferry ports <box>` lists them;
  `ferry shell --no-ports` or `ferry config box-ports off` turns it off.
  The shells to a box share one ssh connection; the last one to close ends
  it, so closing the terminal, or `ferry leave`, leaves nothing running.

### Security

- Boxes can no longer reach back into the borrower's machine. Earlier
  builds of `ferry join` (unreleased) ran a private sshd that the box's key
  opened, through a reverse tunnel in your ssh session, plus a dynamic
  SOCKS tunnel that also reached your localhost and LAN; the box's lender
  could read your home and run commands as you while you were connected.
  `ferry join` and `ferry shell` now remove that state when they find it.

### Fixed

- A running `ferry up` that lost the rendezvous (an outage or a restart)
  stayed unreachable until restarted. It now reconnects within 15 s; the
  harness restarts the rendezvous to check.

- `ferry pipe <contact> <port>` connects stdin/stdout to a TCP port on a
  paired contact's localhost over the direct link, so ssh works with no
  open ports: `ssh -o ProxyCommand='ferry pipe box 22' me@box`.
- `ferry up --allow-pipe <port>[,<port>]` serves pipes only to the listed
  localhost ports and only to paired contacts (none by default); every
  pipe opened or refused, and every stranger turned away, is logged.

### Changed

- The rendezvous in `deploy/fly` caps every relayed circuit at 1 MiB and
  2 minutes (Concord v0.59.1): it carries signaling only, never data.

## [0.1.0] - 2026-10-22

First release. Static binaries for Linux and macOS, amd64 and arm64.

### Added

- `ferry send` / `ferry get` with one-time codes: files and folders over
  WebRTC data channels, striped across up to 16 connections, BLAKE3 checks
  per piece and per file, resume after interruption.
- Pairing (`ferry pair`) with CPace and two check words; contact sends
  without codes.
- Signed messages (`ferry msg`, `inbox`, `wait`), delivered live or left
  sealed in the rendezvous mailbox.
- `ferry config port N` (or `FERRY_PORT`): one fixed UDP port for every
  connection, advertised with the public address, so it can be forwarded
  on a router when hole punching fails.
- `ferry up`, a background agent with a unix-socket API, and a systemd
  user unit printed by `ferry up --unit`.
- `ferry mcp`, an MCP server for Claude Code and other agents.
- `ferry check`, `ferry help --llm`, `--json` everywhere, stable exit codes.
- A default rendezvous, so a first transfer needs no setup;
  `ferry config rendezvous` points at your own.
- Direct only: data goes machine to machine or not at all. When no direct
  path exists, ferry says so and lists what helps.
- A rootless two-machine test harness with NAT and netem.

### Security

- Received paths must be canonical; names that would collide (case,
  `.ferry-part`, `a/../b`) are refused. Dot-named roots are renamed.
- Pairing proves possession of the presented key.
- Bounded resources for untrusted manifests: lazy file handles, one
  hasher, size and count limits, a free-space check.
- Peer text is sanitized before it reaches the terminal.
- The agent socket lives in a private directory and both ends check
  each other's user.

[Unreleased]: https://github.com/ZahakJ/ferry/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ZahakJ/ferry/releases/tag/v0.1.0
