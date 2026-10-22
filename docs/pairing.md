---
title: How it works
order: 2
---

# How it works

This page explains, from the ground up, what ferry does when you type a
code, and why you can trust a connection that went through servers you do
not control. No cryptography background is assumed.

## The problem

You want to hand a file to a friend in another city. Between you are your
routers, your internet providers, and whatever servers help the two
machines find each other. ferry assumes **every one of them may be
hostile**: they may read what passes, change it, or pretend to be the
other side. The only thing you and your friend share is a short code that
one of you reads to the other.

From that code alone, ferry has to build a connection that nobody in the
middle can read or tamper with.

## Step 1: meeting

A code looks like `7-acorn-tulip-otter`. It has two parts.

- **7** is a meeting slot. Both machines connect out to the rendezvous
  and look for each other under slot 7. This part is not secret; it only
  keeps different people's transfers apart.
- **acorn tulip otter** are the secret. Three words from a list of 1295
  give about two billion possibilities. The words **never leave your
  machine**: not to the rendezvous, not to your friend's machine.

How can two machines prove they know the same words without sending them?

## Step 2: agreeing a key without sending the password

ferry uses **CPace**, a *password-authenticated key exchange*. Here is the
idea. Each side mixes the words into a random number of its own, using
mathematics in which mixing is easy but unmixing is practically
impossible, and sends the result. Each side then combines what it
received with its own secret number.

- If both used the **same words**, both arrive at the same new secret: a
  long, random **key** that never travelled over the network.
- If the words **differ**, they arrive at different keys, and the first
  message one sends the other fails to open.

What can someone in the middle do? Watching tells them nothing: the
messages look random and contain neither the words nor the key. Even
*replacing* the messages, which the rendezvous could do, gets them one
guess at the words, because to take part they must commit to some words.
A wrong guess shows up as a failure, and ferry then **throws the code
away**:

```
$ ferry get 7-acorn-tulip-zebra
ferry: the codes did not match. Check the words with the other person and try again with a new code.
```

One guess in two billion is not a useful attack.

## Step 3: connecting directly

Now both machines share a key. They use it to seal the next messages
(with ChaCha20-Poly1305, which both hides and authenticates), and those
messages are WebRTC *offers*: descriptions of how each machine can be
reached, and the **fingerprint** of the encryption key it will use for
the data itself.

That last detail is what keeps the rendezvous out for good. When the data
connection opens, each side checks that the other presents the key whose
fingerprint arrived inside a sealed message. Someone in the middle cannot
present a matching key, and cannot change the fingerprint in transit
without breaking the seal.

To get through both routers, the machines use **ICE**. Each asks a public
STUN server "what address do you see me as?", they swap those addresses,
and both send packets towards each other at once. To most home routers
this looks like replies to traffic going out, so they let it in: a
*hole punch*. When that fails (some networks block it), the traffic goes
through the rendezvous's **TURN relay** instead. The relay passes along
encrypted packets it cannot read.

You can see which happened at the end of a send:

```
✓ Sent 2.1 GB in 19s (110.5 MB/s, direct).
```

## Step 4: moving the data

A single connection over a long, slightly lossy path is slow: every lost
packet makes it back off and take its time recovering. ferry opens
**several connections at once** (up to 16 for big transfers) and spreads
the file across them; when one is recovering, the others keep going. On
a 40 ms path with 0.5% loss this is about 1.6 times faster than one TCP
stream ([measurements](throughput.html)).

The file is cut into 256 KiB pieces. Each piece carries a **BLAKE3** hash
and is checked before it is written. Each file is also hashed whole as it
lands, and compared with the sender's hash before it is given its real
name. Until then it is `name.ferry-part`, plus a small record of which
pieces arrived, which is how an interrupted transfer resumes.

## Pairing: doing it once

Codes are good for one transfer. If you send to the same person often,
**pair**:

```
$ ferry pair
Tell the other person to run:

    ferry pair 4-blade-crane-smirk

$ ferry pair 4-blade-crane-smirk          # on their machine
✓ Paired with avi. Both screens should show the same two words: debit music
```

Pairing runs exactly the steps above, and then the two machines swap
their **long-term keys**, sealed under the code's key. Each machine has
one such identity, made the first time it is needed and kept in
`~/.config/ferry`.

From then on there is no code. When you `ferry send zyad ./file`, your
machine finds zyad's through the rendezvous by the identity it learned at
pairing, and both prove who they are by signing a fresh exchange with
their long-term keys. A machine that answers as zyad but cannot sign as
zyad is refused:

```
ferry: the machine answering as zyad has a different key than when you paired: refusing
```

The two words at the end of pairing are a final check you can do with
your own eyes: they are derived from the shared key, so if someone had
managed to sit in the middle, your screens would show different words.

## Messages

`ferry msg zyad "…"` sends a short message, signed with your key. If zyad
is online it is delivered at once. If not, it is sealed with a key only
the two of you can compute and left in zyad's **mailbox** on the
rendezvous, which keeps it for up to two weeks. The rendezvous stores a
blob it cannot read.

There is one trade-off to know about: live connections use fresh keys
for every session, so a recording of them stays private even if a
machine's key is stolen later. Mailbox messages do not have that property.
See the [threat model](security.html) for the full picture.
