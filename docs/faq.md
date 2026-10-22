---
title: FAQ
order: 7
---

# Questions

### Do I need an account?

No. Each machine makes its own identity, a pair of keys, the first time
it pairs. Nothing is registered anywhere.

### Can the rendezvous read my files?

No. It introduces the two machines and, for offline contacts, holds sealed
messages. File data goes directly between the machines and never through it. The [threat model](security.html) lists
exactly what the rendezvous can see: who is online, when two machines
meet, and how much sealed data passes.

### What if someone overhears the code?

If they use it before your friend does, they get the file instead, and
your friend's attempt fails. That is why codes are short-lived and work
once. Read the code aloud or send it privately; for repeated transfers,
pair, and codes go away.

### Why are the codes so short?

Because the key exchange (CPace) only allows one guess per code. Three
words give about two billion possibilities; a single guess almost never
succeeds, and after it the code is dead. Long passwords protect against
attackers who can try many times offline. Here nobody can.

### How fast is it?

As fast as the slower of the two internet connections, in most cases. On
a 40 ms path ferry moved 0.9 to 2.1 Gbit/s in testing (both ends on one
busy machine); with 0.5% packet loss, 0.64 to 0.69, where a single TCP
stream managed 0.4. See
[measurements](throughput.html).

### Does it work behind a company or university network?

Often. ferry only sends directly, so a network that blocks UDP between
the two machines stops it, and ferry says so. Then try another network,
put one machine on a VPN or a tailnet, or, where you control the router,
forward one UDP port: `ferry config port 40000`. `ferry check` shows what
your network allows.

### What happens if the transfer is interrupted?

Run the same send and get again, with a new code. The receiver keeps
every piece that arrived and checked out, and asks only for the rest. A
drop of a few seconds does not even interrupt the transfer.

### Can I send to someone who is offline?

Messages, yes: they wait in a sealed mailbox for up to two weeks. Files
need both machines online at the same time. `ferry up` keeps a machine
reachable in the background; `ferry up --unit` prints a systemd unit for
starting it at login.

### Will ferry run something a contact sends me?

Never. ferry stores messages and files; it does not open or execute
them. Fetched files lose their execute bit.

### Where does ferry keep its data?

In `~/.config/ferry` (or `$FERRY_HOME`): the identity, contacts, the
message log, and files received while `ferry up` runs. The folder is
readable only by you.

### How do I remove a contact?

`ferry forget sam`. To talk again, pair again.

### Why WebRTC?

It is the most widely deployed way to get two machines behind routers to
talk directly, with encryption built in, and it runs in browsers too,
which leaves the door open for a web version. Its usual weakness for bulk
data, slow recovery from loss, is what ferry's multiple lanes address.
