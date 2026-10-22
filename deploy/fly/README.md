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
inside the machine and are not reachable from outside.

The mailbox is kept in memory, so a restart or redeploy drops messages
waiting for offline contacts. Senders are not told. Envelopes are kept for
up to 14 days, 200 per recipient.

## Cost

The node carries only small things: DHT records, signaling and mailbox
messages. File data goes directly between the two machines and never passes
through it. The relay service is open to any libp2p client and has no
byte cap, so set a spending limit on the fly organization.
