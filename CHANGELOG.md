# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/). The `--json` output and
exit codes are part of the public interface.

## [Unreleased]

### Changed

- The rendezvous in `deploy/fly` caps every relayed circuit at 1 MiB and
  2 minutes (Concord v0.59.1): it carries signaling only, never data.
- The site has a phone layout for the crossing and the diagram; the README
  has a banner and an animated demo, drawn by `tools/mediagen`.
- The progress line fits narrow terminals: it drops the total and the time
  left, then the speed, rather than wrapping.
- The site fits every width from 320 px up with no sideways scrolling; docs
  tables become stacked cards on phones. `tools/sitecheck` checks every
  page at seven widths in both themes on each build.

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
