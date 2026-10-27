# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/). The `--json` output and
exit codes are part of the public interface.

## [Unreleased]

### Added

- `ferry join <code>` uses a machine someone lends you (a "box"), as if the
  two were one: it pairs, adds `Host <box>` to `~/.ssh/config`, and sets up
  the way back: inside your own ssh session, the box reaches your machine
  (your home at `~/his`, commands it lacks run on yours, your internet as a
  SOCKS proxy) through a private sshd on 127.0.0.1 that only the box's key
  opens. `ferry shell <box>` mounts the box's home at `~/<box>` and logs in.
  `ferry leave <box>` closes the sessions, unmounts, and removes the key,
  the ssh config and the pairing.
- `ferry pipe <contact>` with no port uses the one port the other side serves.
- `ferry shell <box>` shares the box's ports: a server started in the box
  (`npm run dev` on :3000) is http://localhost:3000 on your machine while
  the shell is open, on 127.0.0.1 only, or on the next free port if that one
  is taken (the box's tmux says which). `ferry ports <box>` lists them;
  `ferry shell --no-ports` or `ferry config box-ports off` turns it off.
  The shells to a box share one ssh connection; the last one to close ends
  it, so closing the terminal, or `ferry leave`, leaves nothing running.

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
