---
title: Protocol
order: 8
---

# The ferry protocol

This page describes what goes over the wire, layer by layer, for anyone
who wants to audit ferry, write a compatible client, or extend it. Every
version-sensitive value carries a version label (`ferry/1`, `v1`), and
anything a side does not understand is refused politely rather than
guessed at.

## Layers

| layer | carries | protected by |
|---|---|---|
| rendezvous | discovery, relayed streams, mailbox | libp2p Noise between each peer and the node; ferry's own sealing inside |
| sealed channel | signaling, intents, messages | ChaCha20-Poly1305 under keys from CPace or the contact handshake |
| link | file data and transfer control | DTLS (WebRTC), keys pinned by fingerprints sent over the sealed channel |

## Framing

Everything on a stream is a frame: a 4-byte big-endian length, then that
many bytes. A sealed frame is `ChaCha20-Poly1305(key, nonce, plaintext)`
with an empty additional data, where the nonce is a per-direction message
counter (64-bit, big-endian, in the last 8 of 12 bytes). Each direction
has its own key, derived with HKDF-SHA256 from the session secret and a
transcript hash, labels `ferry secure v1 initiator` and `… responder`.
A replayed, reordered, reflected or dropped frame fails to open, and the
session ends.

## Codes

A code is `<slot>-<w1>-<w2>-<w3>`. The slot (1-99) is announced on the
rendezvous DHT as a provider record for the CID of
`sha256("ferry slot v1\0" + slot)`. The words, joined by spaces, are the
CPace password.

On the libp2p protocol `/ferry/code/1`:

1. The joiner sends `{"v":1,"purpose":"send"|"pair"}` in the clear. A
   purpose mismatch is answered with a hint and costs no attempt.
2. CPace (ristretto255): the joiner sends `msgA` (48 bytes), the creator
   `msgB` (32). Context: identities `joiner` and `creator`; additional data
   `"ferry pair v1\0" + purpose + "\0" + joinerPeerID + "\0" + creatorPeerID`,
   binding the key to this libp2p connection.
3. The sealed channel starts, keyed by the CPace output and
   `sha256(msgA ‖ msgB ‖ ad)`. Each side sends `ferry confirm`; the
   creator gives up the code on the first failure.
4. Two words for comparison come from `sha256("ferry sas v1\0" + binding)`.

For pairing, each side then sends `{"key": Public}` where Public is
`{name, sign (Ed25519), box (X25519), peer (libp2p ID)}`.

## Contact sessions

On `/ferry/contact/1`, through the rendezvous relay to the contact's peer
ID:

1. Both send `{"proto":"ferry/1","key":Public,"eph":X25519 ephemeral}`.
   The responder checks the key is a contact, and that it arrives from the
   peer ID recorded at pairing, before anything else.
2. Both send an Ed25519 signature over
   `"ferry handshake v1\0" + transcript`, where the transcript hashes a
   context (both peer IDs) and both sides' long-term and ephemeral keys.
3. The sealed channel starts, keyed by the ephemeral X25519 secret.
4. The dialer sends an **intent**, `{"want": …}`, and the other side
   answers `{"ok":true}` or `{"ok":false,"why":"…"}`.

| want | then |
|---|---|
| `msg` | a stream of signed messages, each acknowledged `{"id","ok"}` |
| `files` | a link, then a transfer |

New kinds of work are new `want` values. A side that does not know one
declines with a reason; nothing else changes.

## Messages

```json
{"v":1, "id":"<24 hex>", "from":"<key id>", "to":"<key id>", "ts":<unix ms>,
 "type":"note", "text":"…", "data":{…}, "reply_to":"<id>", "sig":"<base64>"}
```

`sig` is Ed25519 over `"ferry msg v1\0"` plus the JSON of the message
without `sig`. A key id is the first 9 bytes of
`sha256("ferry id\0" ‖ sign ‖ box)`, base64url. The receiver logs a
message once per id.

When the contact is offline, the message goes into their rendezvous
mailbox (`/concord/mailbox/1.0.0`) as an envelope:
`{"v":1,"k":<sender key id>,"n":<24-byte nonce>,"c":<ciphertext>}`, sealed
with XChaCha20-Poly1305 under
`HKDF(X25519(sender, recipient), salt = sorted key ids, "ferry mailbox v1")`
with additional data `"ferry mbox v1\0" + from + "\0" + to`.

## Link

Over the sealed channel, each side sends `{"type":"lanes","lane":n}`; the
link uses the larger n (at most 32). The dialer then sends one offer per
lane, `{"lane":i,"type":"offer","sdp":…}`, and the other side answers each
one. There is no trickle ICE: each description is complete.

Each lane is one peer connection with a pre-negotiated data channel `data`
(id 1, unordered, reliable). Lane 0 also has `ctl` (id 0, ordered).

## Transfers

Control messages on `ctl` are JSON objects with a `t` field:

| t | from | meaning |
|---|---|---|
| `offer` | sender | id, name, total, nfiles, skipped |
| `files` | sender | up to 2000 manifest entries `{path,size,mode,dir,mtime}` |
| `accept` / `decline` | receiver | `done`: indexes it already holds; `partial`: index → base64 bitmap of pieces held |
| `end-file` | sender | index and whole-file BLAKE3-256 (hex) |
| `progress` | receiver | bytes held |
| `complete` | receiver | ok, saved name, or reason |
| `bye` | sender | the receiver may hang up |

A piece on a data lane is `0x01 ‖ file index (u32) ‖ offset (u64) ‖
BLAKE3-128(data) ‖ data`, all big-endian, with 256 KiB of data except at
the end of a file. The transfer id is `sha256` over the name and the file
list (paths, sizes, modes, modification times), so sending the same files
again resumes.

## Jobs (designed, not yet built)

Running work on another machine fits the layers above without changing
them. This is the plan, so that what ships now stays compatible.

**Opt-in, per contact.** A machine runs jobs only if its owner writes a
rule for that contact in `~/.config/ferry/jobs.json`. There is no default
rule and no wildcard contact:

```json
{"sam": {
  "commands": [["make", "test"], ["go", "test", "./..."]],
  "workdirs": ["~/src/ferry"],
  "limits": {"cpu": 4, "memory": "8G", "wall": "30m"}
}}
```

A command matches only if its argv equals an entry, element by element;
there is no shell.

**Wire.** A new intent, `{"want":"job"}`, then sealed JSON:
`submit {id, argv, workdir, bundle?, nonce, expires, sig}` is signed by the
requester over its contents, the receiver's key id, a fresh nonce and an
expiry a few minutes ahead. The runner refuses an unknown nonce window,
an expired request, or any reuse, so a request cannot be replayed. Then
`log {stream, data}`, `status {state, exit}`, `cancel {id}`, and artifacts
over a link with the ordinary transfer protocol.

**Audit.** The runner appends every request to `jobs.jsonl`: who asked,
the exact argv and directory, start, end and exit code, whether it was
allowed.

**Interfaces.** `ferry run sam -- make test` on the CLI;
`submit_job`, `job_status`, `job_logs`, `cancel_job`, `fetch_artifacts` as
MCP tools.
