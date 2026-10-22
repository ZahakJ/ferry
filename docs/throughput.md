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
