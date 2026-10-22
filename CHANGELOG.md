# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/). The `--json` output and
exit codes are part of the public interface.

## [Unreleased]

### Added

- `ferry send` / `ferry get` with one-time codes: files and folders over
  WebRTC data channels, striped across up to 16 connections, BLAKE3 checks
  per piece and per file, resume after interruption.
- Pairing (`ferry pair`) with CPace and two check words; contact sends
  without codes.
- Signed messages (`ferry msg`, `inbox`, `wait`), delivered live or left
  sealed in the rendezvous mailbox.
- `ferry up`, a background agent with a unix-socket API, and a systemd
  user unit printed by `ferry up --unit`.
- `ferry mcp`, an MCP server for Claude Code and other agents.
- `ferry check`, `ferry help --llm`, `--json` everywhere, stable exit codes.
- A rootless two-machine test harness with NAT, netem and TURN.
