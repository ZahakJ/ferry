# ferry's rendezvous on fly.io

The rendezvous introduces two ferry machines and relays their signaling. It
is [Concord](https://github.com/ZahakJ/concord)'s rendezvous node, built
unmodified from a pinned release (`CONCORD_REF` and `CONCORD_SHA` in the
Dockerfile; the build fails if the tag moves). ferry uses three of its
services: DHT provider records for codes, circuit relay v2 for signaling,
and the mailbox for messages to offline contacts.

## Deploy

Needs `flyctl`, logged in. From this folder:

```
fly apps create ferry-rdv-zahak
fly ips allocate-v4 -a ferry-rdv-zahak     # dedicated: raw TCP and UDP need it
fly ips allocate-v6 -a ferry-rdv-zahak

openssl rand -hex 32 > ferry-rdv.seed      # keep this file; see below
fly secrets set -a ferry-rdv-zahak --stage \
  CONCORD_RELAY_SEED="$(cat ferry-rdv.seed)" \
  CONCORD_PUBLIC_HOST=ferry-rdv-zahak.fly.dev

fly deploy --ha=false
fly logs -a ferry-rdv-zahak --no-tail | grep -A2 SHARE
```

The last line prints the address, `/dns/ferry-rdv-zahak.fly.dev/tcp/4001/p2p/12D3KooW…`.
Check it from any machine:

```
FERRY_RENDEZVOUS=<that address> ferry check
```

Move `ferry-rdv.seed` into a password manager and delete it from disk. The seed
fixes the node's peer ID, which every ferry binary ships with; a new seed
means a new ID and every install has to be pointed at it again.

## Secrets

| name | required | what |
|---|---|---|
| `CONCORD_RELAY_SEED` | yes | 32-byte hex seed of the node's identity |
| `CONCORD_PUBLIC_HOST` | no | host name; the log then prints the full address |

Nothing else is set. With no `CONCORD_TURN_SECRET` the node runs no TURN.

## What the node exposes

- TCP 4001: libp2p over TCP, on IPv4 and IPv6.
- UDP 4001: libp2p over QUIC, on IPv4 only. Measured on Concord's node:
  fly does not carry UDP over IPv6.

No HTTP service is exposed. Concord's guest and booking pages listen on 8080
inside the machine and are not reachable from outside. ferry would need HTTP
only for TURN credentials.

The mailbox is kept in memory, so a restart or redeploy drops messages
waiting for offline contacts. Senders are not told. Envelopes are kept for
up to 14 days, 200 per recipient.

## Fallback for strict NATs

Fly cannot run TURN: its edge does not hairpin UDP between two relay
allocations (see Concord's `fly.rendezvous.toml`). The fallback for peers
that cannot connect directly is to carry ferry's data over the libp2p
circuit relay this node already provides, on TCP 4001. TCP gets out of
networks that block UDP.

**The relay already allows bulk data.** Concord sets `Limit = nil` in
`RendezvousRelayResources`. Circuits have no time or byte cap and are not
marked "limited", so ordinary streams run over them. A ferry transfer is
one circuit, with its lanes as streams over it. The other limits are
well above that:

| limit | Concord | ferry needs |
|---|---|---|
| circuit duration and data | unlimited | unlimited |
| circuits per peer | 64 | 1 per transfer |
| reservations | 512 total, 8 per peer, 16 per IP | 1 per running `ferry up` |
| copy buffer | 2 KiB per direction | (not a bottleneck, below) |

These are set in Concord's code, not by environment, so tuning them would
mean changing Concord. Nothing here needs it.

Measured with `internal/rv/relaybulk_test.go`, with the rendezvous built
from v0.59.0 and both peers on this machine:

| path | 1 stream | 4 streams |
|---|---|---|
| loopback | 1.7–1.8 Gbps | 2.0–2.1 Gbps |
| 20 ms RTT on each leg | 0.51 Gbps | 0.64 Gbps |
| same, 0.25% loss on each leg | 0.30 Gbps | 0.45 Gbps |

During a 2 GB relayed transfer the node used 37 MB of RAM and 1.35 cores.
The relay decrypts and re-encrypts each leg's Noise session. On a
`shared-cpu-1x` the CPU caps relayed throughput: fly gives a shared core a
small guaranteed slice with bursts above it. A `performance-1x` raises that
cap if the relay sees real use.

**Costs and abuse.** Every relayed byte enters and leaves the machine, and fly
bills egress. The relay is open to anyone who can reach it, with no byte
cap, so a stranger could use it as a free relay. Before turning the fallback
on in ferry, set a spending limit on the fly organization. Then decide whether
the node needs a per-peer byte budget, which would be a small Concord change.

**What ferry still needs** to use the fallback (not built yet): a lane
transport over a libp2p stream instead of an SCTP data channel. It would be
used when ICE fails, after both sides have proven they hold the session keys
over the existing signaling, and it would add a ChaCha20-Poly1305 layer from
the session keys. The relay never sees plaintext in any case: the relayed
connection runs its own end-to-end Noise session.

The later alternative is TURN on a small VPS that owns its public IP. ferry
already uses TURN when the rendezvous offers it (`/turn` on its HTTP gateway).
