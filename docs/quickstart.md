---
title: Quick start
order: 1
---

# Quick start

This page takes you from nothing to your first file sent. It takes about
five minutes, and most of that is downloading.

## 1. Install ferry on both machines

ferry is one program with no dependencies. Download it for your system
from the [releases page](https://github.com/ZahakJ/ferry/releases), or, if
you have Go:

```
go install github.com/ZahakJ/ferry/cmd/ferry@latest
```

Check that it runs:

```
$ ferry version
ferry v0.1.0
```

## 2. Tell ferry where to meet

Two machines on the internet usually cannot just call each other: each
sits behind a router that blocks unexpected visitors. So both first
connect *out* to a small server everyone can reach, a **rendezvous**, which
introduces them. After that they talk directly, and the rendezvous drops
out of the conversation. It never sees what you send (see
[how it works](pairing.html)).

ferry uses the rendezvous from [Concord](https://github.com/ZahakJ/concord).
Run your own on any machine with a public address (the repository's
`deploy/fly` folder has the steps for fly.io), or use one a friend runs.
Set its address once on each machine:

```
ferry config rendezvous /dns/rv.example.org/tcp/4001/p2p/12D3KooW...
```

The rendezvous only introduces the machines; the data then goes directly
between them. Most home routers let that happen. If one does not, forward
one UDP port on it to the machine behind it, and tell ferry to use that
port:

```
ferry config port 40000
```

Then ask ferry whether everything it needs is in place:

```
$ ferry check
✓ identity     none yet; one is made the first time you pair
✓ rendezvous   reached in 38ms
✓ relay slot   granted: contacts can reach this machine for signaling
✓ local        192.168.1.20:51833
✓ public       81.2.69.160:51833 (from STUN; direct connections are likely)
✓ agent        not running (start with `ferry up` to stay reachable)
```

Each line is one thing ferry depends on, in the order it needs them. A ✗
comes with a sentence saying what it means.

## 3. Send something

On the machine that has the file:

```
$ ferry send ./holiday.mp4
Sending holiday.mp4 (2.1 GB).
On the other machine, run:

    ferry get 7-acorn-tulip-otter

Waiting for them… (Ctrl-C to cancel)
```

Read the code to the other person, or send it to them privately. On their
machine:

```
$ ferry get 7-acorn-tulip-otter
Connected. (both sides see: motto sway)
Receiving holiday.mp4 (2.1 GB).
━━━━━━━━━━━━━━━━━━━━━━━━ ↓  64.0%  1.3 GB of 2.1 GB  112.4 MB/s · 7s left
✓ Received 2.1 GB in 19s (110.5 MB/s). Checked: every byte matches.
holiday.mp4
```

That is all. A few things happened that you did not have to think about:

- The code worked **once**. If someone else had typed it first, or typed
  it wrong, it would now be dead, and you would make a new one.
- The file went **directly** between the two machines; ferry never sends
  data through a server. The line `(both sides see: motto sway)` is
  printed on both screens; if you want to be sure no one sat in the middle,
  compare the two words.
- Every piece was **checked** as it arrived, and the whole file once more
  at the end. "Every byte matches" is a statement, not a hope.

## 4. If it stops half way

Close the laptop, lose the Wi-Fi, press Ctrl-C: run the same commands
again, with a new code. ferry keeps what already arrived and sends only
the rest.

```
$ ferry get 3-lemon-cider-robin
Receiving holiday.mp4 (2.1 GB).
━━━━━━━━━━━━━━━━━━━━━━━━ ↓  71.3%  1.5 GB of 2.1 GB …
```

Short drops do not even need that: a connection that goes quiet for a few
seconds picks up where it was.

## Next

- If you send to the same person often, [pair](pairing.html) once and
  stop using codes.
- To let a Claude Code session use ferry, see [ferry for agents](mcp.html).
