# Throughput measurements

Two processes on one machine (16 cores), inside a network namespace.
Shaped runs put netem on `lo`, so delay and loss apply in both directions:
`delay 20ms` gives 40 ms RTT, `loss 0.25%` each way is about 0.5% per round trip.
Each run is capped at 30 s. Reproduce with `go run ./cmd/spike -h`.

Tuned SCTP means: zero checksum, 4 MiB SCTP receive window, 4 MiB UDP socket
buffers, 16 MiB buffered-amount high-water mark, congestion-avoidance step
256 KiB, and the transfer striped over N peer connections (one SCTP
association each).

| link | TCP (CUBIC) | QUIC (quic-go, 1 stream) | SCTP, pion defaults | SCTP tuned, 1 conn | SCTP tuned, N conns |
|---|---|---|---|---|---|
| loopback | 53 Gbps | 9.5 Gbps | 0.6-1.2 Gbps | 1.2 Gbps | 2.5 Gbps (N=8) |
| 40 ms RTT, no loss | 0.80 Gbps | 0.23 Gbps | 0.07-0.7 Gbps (erratic) | 1.08 Gbps | 1.9-2.1 Gbps (N=8-16) |
| 40 ms RTT, 0.5% loss | 0.39-0.41 Gbps | 0.026 Gbps | 0.006 Gbps | 0.05 Gbps | 0.78 Gbps (N=16) |

Why the defaults fail on a long link: slow start overshoots, the receiving
UDP socket drops a burst, retransmission timeouts back off to seconds, and
NewReno regrows the window by one 1191-byte MTU per round trip. Raising the
growth step and spreading the load over several associations, each of which
loses and recovers independently, fixes most of it.

## ferry itself, through the harness

`test/harness.sh`: two hosts, each behind its own NAT (nft masquerade),
connected by a bridge; netem on both NATs' WAN side. Transfers of 1-2 GB by
code, on the default settings (lanes chosen by size: 16 for these). Both
ends and both NATs share one 16-core machine, so CPU contention limits the
unshaped case and numbers vary between runs.

| path | ferry, 8 lanes | ferry, 16 lanes |
|---|---|---|
| LAN-like (veth, no shaping) | 2.3-3.3 Gbps | 1.2-3.2 Gbps |
| 40 ms RTT, no loss | 0.68-1.46 Gbps | 0.9-2.1 Gbps |
| 40 ms RTT, 0.5% loss | 0.38-0.39 Gbps | 0.64-0.69 Gbps |

The low ends were measured with the machine's load average around 10 from
unrelated work; the same runs on the previous commit gave the same numbers,
so they reflect contention, not the code.

Sixteen lanes is the default for transfers of 512 MB and up; smaller ones
use fewer (at least 2). `ferry config lanes N` fixes the number.

On loopback (one machine, a local rendezvous, 16 lanes), ferry moves 2 GB
end to end in 7.9 seconds, 2.0 Gbps, including both BLAKE3 checks and
writing to disk.

## Through the rendezvous relay

For peers that cannot connect directly, ferry's data could also travel over
the rendezvous's libp2p circuit relay on TCP. That fallback is planned but
not built yet. Concord's relay puts no time or byte cap on circuits, and
raw streams through it were measured with the rendezvous on the same
machine (`FERRY_RELAY_BULK=<MB> go test -run TestRelayBulk ./internal/rv`):

| path | 1 stream | 4 streams |
|---|---|---|
| loopback | 1.7–1.8 Gbps | 2.0–2.1 Gbps |
| 20 ms RTT on each leg | 0.51 Gbps | 0.64 Gbps |
| same, 0.25% loss on each leg | 0.30 Gbps | 0.45 Gbps |

On a real deployment the relay machine's CPU and bandwidth set the
limit; see `deploy/fly/README.md`.
