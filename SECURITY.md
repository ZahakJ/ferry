# Security

## Reporting a problem

Please report vulnerabilities privately through GitHub's
"Report a vulnerability" button on this repository, not in a public issue.
You will get an answer within a week.

## Threat model

ferry connects two machines whose owners trust each other. Everything
else on the path is assumed hostile: the network, the rendezvous that
introduces the two sides, and the TURN relay that carries traffic when a
direct path fails.

### What each party can and cannot do

**Someone on the network** (your café's Wi-Fi, your ISP) sees that you
talk to a rendezvous and, later, to an IP address or a relay, and roughly
how much you send. They cannot read or change anything: every byte is
encrypted and authenticated.

**The rendezvous** sees which ferry identities are online, when two of
them meet, the slot number of a code, and the size and timing of sealed
signaling and mailbox messages. It cannot:

- read anything: signaling, mailbox messages and file data are sealed
  with keys it never sees;
- learn or guess a code's words: they are never sent; CPace reveals
  nothing about them, even to an active attacker;
- put itself in the middle: the WebRTC offers that name each side's DTLS
  key fingerprint are sealed under the pairing or contact keys, and the
  key agreement is bound to both sides' network identities;
- impersonate a contact: contact sessions are authenticated by long-term
  Ed25519 keys exchanged at pairing.

It can refuse service, delay or drop messages, and hand out bad TURN
credentials. ferry then fails; it does not fail open.

**The TURN relay** (when used) forwards DTLS-encrypted packets. It sees
their size and timing, nothing else.

**Someone who learns a code** before it is used can take the transfer or
the pairing in your place. Codes are meant to be read aloud or sent
privately, and are good for one attempt. Pairing ends with two words shown
on both screens; if they differ, someone was in the middle: run
`ferry forget <name>` and pair again.

**Someone guessing a code** gets exactly one try: a wrong guess makes both
sides abandon the code. With 31 bits of secret words, one guess succeeds
about once in two billion tries.

**A paired contact** can send you messages and files, and can see that you
are online. They cannot make your machine run anything. ferry never
executes received content; fetched files lose their execute bit and are
named `ferry-<n>-<name>`, and a received folder named like `.git` or
`.envrc` is saved as `dot-git`, so nothing lands where a tool would act on
it by name; a contact can run one transfer at a time, and only if the
files fit in the free space; the MCP
server tells the model that received text is untrusted and is not to be
followed without the user's consent. A future job runner (running
commands on request) will be off by default and need a per-contact
allowlist you write yourself.

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
