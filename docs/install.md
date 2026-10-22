---
title: Install
order: 0
---

# Install

ferry is a single program. Pick whichever way suits you; all of them end
with the same `ferry` binary.

## The one-liner

```
curl -fsSL https://zahakj.github.io/ferry/install.sh | sh
```

It downloads the release for your system (Linux or macOS, Intel or ARM),
checks its SHA-256 against the published list, and puts it in
`~/.local/bin`. The [script](../install.sh) is short; reading it first is
a good habit.

## By hand

Download the binary for your system and `SHA256SUMS` from the
[releases page](https://github.com/ZahakJ/ferry/releases), then check it:

```
sha256sum --ignore-missing -c SHA256SUMS
chmod +x ferry-*-linux-amd64 && mv ferry-*-linux-amd64 ~/.local/bin/ferry
```

Releases also carry `SHA256SUMS.sig`, a signature over the checksum list
made with the maintainer's SSH key, which is listed in the repository's
`allowed_signers` file. To check it as well:

```
ssh-keygen -Y verify -f allowed_signers -I ferry -n file -s SHA256SUMS.sig < SHA256SUMS
```

## With Go

```
go install github.com/ZahakJ/ferry/cmd/ferry@latest
```

## Starting at login (optional)

`ferry up` keeps your machine reachable by your contacts. To start it at
login on Linux, ferry prints a systemd user unit and the commands to
install it; it never installs anything itself:

```
ferry up --unit
```

## Next

[Quick start](quickstart.html): your first transfer.
