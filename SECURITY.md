# Security

## Reporting a problem

Please report vulnerabilities privately through GitHub's
"Report a vulnerability" button on this repository, not in a public issue.
You will get an answer within a week.

## Threat model

ferry connects two machines whose owners trust each other. Everything
else on the path is assumed hostile: the network, and the rendezvous that
introduces the two sides. File data goes directly between the two
machines and never through a server.

### What each party can and cannot do

**Someone on the network** (your café's Wi-Fi, your ISP) sees that you
talk to a rendezvous and, later, to the other machine's IP address, and
roughly how much you send. They cannot read or change anything: every byte is
encrypted and authenticated.

**The rendezvous** sees which ferry identities are online, when two of
them meet, the slot number of a code, and the size and timing of sealed
signaling and mailbox messages. It cannot:

- read anything: signaling and mailbox messages are sealed with keys it
  never sees, and file data never passes through it;
- learn or guess a code's words: they are never sent; CPace reveals
  nothing about them, even to an active attacker;
- put itself in the middle: the WebRTC offers that name each side's DTLS
  key fingerprint are sealed under the pairing or contact keys, and the
  key agreement is bound to both sides' network identities;
- impersonate a contact: contact sessions are authenticated by long-term
  Ed25519 keys exchanged at pairing.

It can refuse service, delay or drop messages. ferry then fails; it does
not fail open.

**Someone who learns a code** before it is used can take the transfer or
the pairing in your place. Codes are meant to be read aloud or sent
privately, and are good for one attempt. Pairing ends with two words shown
on both screens; if they differ, someone was in the middle: run
`ferry forget <name>` and pair again.

**Someone guessing a code** gets exactly one try: a wrong guess makes both
sides abandon the code. With 31 bits of secret words, one guess succeeds
about once in two billion tries.

**A paired contact** can send you messages and files, and can see that you
are online. They cannot make your machine run anything. The one exception
you can opt into is `ferry pipe`: if you run `ferry up --allow-pipe 2222`,
a paired contact can open a TCP connection to port 2222 on this machine's
localhost (127.0.0.1), and to no other port or address. It is off by
default, granted per port, never to anyone unpaired, never to the mesh
sshd of `ferry mine` (that is for your own devices only), and every pipe
opened or refused is logged by `ferry up`. Whatever listens on an allowed port
(sshd, say) does its own authentication, and what a contact can do through
it is what that service allows. ferry never
executes received content; fetched files lose their execute bit and are
named `ferry-<n>-<name>`, and a received folder named like `.git` or
`.envrc` is saved as `dot-git`, so nothing lands where a tool would act on
it by name; a contact can run one transfer at a time, and only if the
files fit in the free space; the MCP
server tells the model that received text is untrusted and is not to be
followed without the user's consent. A future job runner (running
commands on request) will be off by default and need a per-contact
allowlist you write yourself.

**Your own devices** (`ferry mine`) get full access to each other, on
purpose: each one can log in to every other as you, read and change your
files there and run anything. So **one compromised device reaches all of
them**; link only machines that are yours and that you would trust with
each other's keys. How it is built:

- Linking is pairing (CPace over a one-time code) plus a check both
  people must answer: each screen shows two words, and both must say they
  match before either side records anything. A no on either side links
  nothing.
- Each device runs a private sshd on 127.0.0.1 only, as you (no root),
  that accepts only public keys, only the mesh keys of your devices
  (no passwords, no agent or X11 forwarding, local port forwarding only),
  and has its own host key, checked strictly from a known_hosts file in
  ferry's folder. ferry's agent connects a pipe to it only for a contact
  recorded as one of your devices; for anyone else (`mine-*` requests, or
  `ferry pipe` to its port, even if `--allow-pipe` lists it) it refuses
  and logs. A friend cannot become one of your devices except by you
  running `ferry mine add`/`join` with them.
- Outside ferry's folder it writes one marked block per device at the top
  of `~/.ssh/config`, an `on` script in `~/.local/bin` (never over one it
  did not make), mount points under `~/m`, and, only if you turn the
  fallback on, one marked line in `~/.bashrc`/`~/.zshrc`. It does not
  touch `~/.ssh/authorized_keys` unless you choose `--system-sshd`.
- A device you link to one of yours is introduced to the others by it,
  over their authenticated contact session. That adds nothing an attacker
  holding one of your devices could not do anyway: it already has a shell
  on the others.
- `ferry mine rm <device>`, on any of your devices, removes it there and
  tells the removed device to forget all of yours and every other device
  to forget it. A device that is offline is told when both are next
  online; until then it still trusts the removed one. A removed device
  that ignores the message is still turned away by every device that got
  it: it is no longer their contact, and its key is gone from their sshd.
- Claude sessions are pulled from each device into the others: only
  transcript files (`.jsonl`) and their side files under
  `~/.claude/projects`, never `memory/`, settings, credentials or anything
  else. Nothing is deleted or overwritten; a transcript only grows when
  the copy here is exactly the start of the other.
- What the rendezvous sees is what it sees for any contact: that your
  devices are online and when they connect to each other. ferry up checks
  your devices every 15 s over one long-lived ssh connection each, so it
  goes back to the rendezvous only to reconnect (once a minute for a
  device that stays offline).

**A box you joined** (`ferry join`) has no way into your machine. Your
machine reaches into the box (ssh, the box's files mounted at `~/<box>`,
its ports forwarded to your localhost); nothing in the box connects to
yours. ferry's ssh config block for the box turns off agent and X11
forwarding and forwards no ports from the box. The one thing that flows
back is the box's web traffic, and only while `ferry shell` is open: the
box's `127.0.0.1:1080` is forwarded (a plain `ssh -R`, not a dynamic one)
to a SOCKS5 proxy ferry runs on your 127.0.0.1. That proxy allows only TCP
CONNECT, resolves names itself, dials the address it checked (never the
name, so DNS rebinding cannot change it afterwards), checks it again at
connect time, and refuses loopback, private (10/8, 172.16/12, 192.168/16),
link-local, CGNAT (100.64/10), multicast, reserved and unspecified
addresses, IPv6 ULA and link-local, IPv4 carried inside IPv6 (mapped,
compatible, NAT64) for all of those, and your machine's own interface
addresses. So the box reaches the public internet with your IP address,
and nothing on your machine or your network. Refusals are logged in
`box/<name>/proxy.log` in ferry's folder and counted when the shell closes.
`--no-internet` (or `ferry config box-internet off`) turns it off.

**Everything you bring is visible to the lender.** `ferry bring` copies
what you choose into the box (it leaves out secret-looking files unless
you say otherwise), and the lender can read anything in the box. **What
you take comes from the lender's machine.** `ferry take` writes only the
paths it asked for, inside that folder, never through a symlink, never
over a newer file of yours, and never `.git/config`, `.git/hooks` or a
`.git` file (from the box, those could make git run commands on your
machine). Other files can still be anything the lender put there: review
what you take like any change from someone else (`git diff`) before you
run it.

Older ferry versions let the box reach back into your machine (a private
sshd, the box's key, reverse tunnels). `ferry join` and `ferry shell`
remove that state when they find it, and say so.

While `ferry shell` is open, every port the box's user listens on (1024 to
32767, on a loopback or wildcard address) is forwarded to the same port,
or the next free one, on your machine's 127.0.0.1, and never to another
interface. That makes whatever the box serves reachable to anyone on your
machine: every local user and program, and a web page in your browser
that knows the port. The box, and so its lender, choose what is served
there. Ports never go the other way (yours into the box) unless you
forward them yourself. `ferry shell --no-ports` or
`ferry config box-ports off` turns it off; the forwards end with the last
`ferry shell` to the box.

**Someone with access to your machine** can read `~/.config/ferry`
(mode 0700): your identity key, contacts and message log. ferry does not
defend against that; use disk encryption.

### Forward secrecy

Live sessions use fresh ephemeral keys (X25519 for contact sessions,
CPace for codes, DTLS for data), so recorded traffic stays private even if
a long-term key later leaks. Messages left in a mailbox for an offline
contact are sealed with a key derived from both long-term keys and have no
forward secrecy: someone who records them and later steals a key could
read them. ferry delivers live whenever both sides are online.

### What ferry relies on

| purpose | construction | implementation |
|---|---|---|
| code to key | CPace over ristretto255 | filippo.io/cpace, github.com/gtank/ristretto255 |
| sealed signaling | ChaCha20-Poly1305, counter nonces, per-direction keys from HKDF-SHA256 | golang.org/x/crypto |
| contact sessions | ephemeral X25519, Ed25519 signatures over the transcript | Go standard library |
| mailbox envelopes | XChaCha20-Poly1305, key from static X25519 + HKDF | golang.org/x/crypto |
| data | WebRTC: DTLS 1.2, SCTP | pion |
| integrity | BLAKE3 per 256 KiB piece and per file | lukechampine.com/blake3 |
| rendezvous transport | libp2p Noise | go-libp2p |

No cryptography is implemented in ferry itself; it composes the libraries
above. The CPace package is small and readable but has not had a formal
audit.

### Integrity

Each piece of a file carries its BLAKE3 hash, checked before it is written.
Each file is hashed whole as it lands and compared with the sender's hash
before it gets its real name. Received paths are checked so that nothing
can be written outside the destination folder.
