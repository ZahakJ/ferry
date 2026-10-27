#!/bin/sh
# Two-peer test harness for ferry.
#
# Builds a small internet inside a user namespace (no root needed):
#
#   hostA ── natA ──┐                ┌── natB ── hostB
#   192.168.10.2    203.0.113.10   203.0.113.20    192.168.20.2
#                   └──── br0 ("the internet") ────┘
#                              │
#                          rv 203.0.113.1: Concord rendezvous (DHT, relay,
#                          mailbox, and TURN, which ferry must never use)
#                          and a STUN server
#
# Both NATs masquerade like home routers; netem on their WAN side shapes
# the path. Then it proves, in order: pairing, a wrong code rejected,
# no plaintext on the wire (rendezvous included), a plain "no direct path"
# when direct UDP is blocked, a forwarded port through strict NATs,
# throughput, a 5 GB transfer killed part way and resumed with a hash
# match, and two MCP clients talking.
#
# Usage: test/harness.sh            (from the repo root)
# Needs: bin/ferry, bin/rendezvous (built from the Concord repo:
#        go build -o bin/rendezvous ./cmd/rendezvous), and Linux with
#        unprivileged user namespaces, nft, tc.
# Env:   FERRY_HARNESS_DIR  where big test files go (default test/tmp)
#        FERRY_HARNESS_BIG  size of the resume test in MB (default 5000)
#        FERRY_HARNESS_ONLY space-separated scenario names to run

set -eu

if [ "${FERRY_HARNESS_INNER:-}" != 1 ]; then
	exec env FERRY_HARNESS_INNER=1 unshare --user --map-root-user --net --mount --propagation private sh "$0" "$@"
fi

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=${FERRY_HARNESS_DIR:-$ROOT/test/tmp}
BIG=${FERRY_HARNESS_BIG:-5000}
ONLY=${FERRY_HARNESS_ONLY:-}
BIN=$ROOT/test/tmp/bin
mkdir -p "$WORK" "$BIN"
LOG=$WORK/log
rm -rf "$LOG"; mkdir -p "$LOG"

pass=0; failed=0
ok()   { pass=$((pass+1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { failed=$((failed+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
want() { [ -z "$ONLY" ] || case " $ONLY " in *" $1 "*) return 0;; *) return 1;; esac; }

PIDS=""
cleanup() {
	for p in $PIDS; do kill "$p" 2>/dev/null || true; done
	sleep 1
	for p in $PIDS; do kill -KILL "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

step "build"
( cd "$ROOT" && go build -o "$BIN/ferry" ./cmd/ferry && go build -o "$BIN/sniff" ./test/sniff \
	&& go build -o "$BIN/stund" ./test/stund && go build -o "$BIN/mcpdrive" ./test/mcpdrive )
RVBIN=${FERRY_RV_BIN:-$ROOT/bin/rendezvous}
[ -x "$RVBIN" ] || { echo "need the Concord rendezvous at $RVBIN (or set FERRY_RV_BIN)"; exit 1; }
ok "built ferry and test tools"

step "network"
mount -t tmpfs none /run
mkdir -p /run/netns
ip link set lo up
ip link add br0 type bridge
ip addr add 203.0.113.254/24 dev br0
ip link set br0 up

ns() { ip netns exec "$@"; }
mkns() { ip netns add "$1"; ns "$1" ip link set lo up; }

# The rendezvous lives on the internet.
mkns rv
ip link add rv0 type veth peer name brrv
ip link set rv0 netns rv
ip link set brrv master br0 up
ns rv ip addr add 203.0.113.1/24 dev rv0
ns rv ip link set rv0 up

# A home: router with NAT, and one machine behind it.
home() { # name lan-octet wan-ip
	n=$1 o=$2 w=$3
	mkns "nat$n"; mkns "host$n"
	ip link add "wan$n" type veth peer name "br$n"
	ip link set "wan$n" netns "nat$n"
	ip link set "br$n" master br0 up
	ns "nat$n" ip link set "wan$n" name wan
	ns "nat$n" ip addr add "$w/24" dev wan
	ns "nat$n" ip link set wan up
	ns "nat$n" ip link add lan type veth peer name eth0 netns "host$n"
	ns "nat$n" ip addr add "192.168.$o.1/24" dev lan
	ns "nat$n" ip link set lan up
	ns "host$n" ip addr add "192.168.$o.2/24" dev eth0
	ns "host$n" ip link set eth0 up
	ns "host$n" ip route add default via "192.168.$o.1"
	ns "nat$n" sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
	ns "nat$n" nft add table ip nat
	ns "nat$n" nft "add chain ip nat post { type nat hook postrouting priority 100; }"
	ns "nat$n" nft add rule ip nat post oifname wan masquerade
	ns "nat$n" nft add table ip filter
	ns "nat$n" nft "add chain ip filter between { type filter hook forward priority 0; }"
	# Like a home router: drop unsolicited packets to the router itself.
	# (Without this, Linux conntrack remembers a stray hole-punch probe and
	# then gives the real outgoing flow a different port.)
	ns "nat$n" nft "add chain ip filter in { type filter hook input priority 0; }"
	ns "nat$n" nft add rule ip filter in iifname wan ct state new drop
}
home A 10 203.0.113.10
home B 20 203.0.113.20

shape() { # delay loss
	for n in A B; do
		ns "nat$n" tc qdisc replace dev wan root netem delay "$1" loss "$2" limit 100000
	done
}
unshape() { for n in A B; do ns "nat$n" tc qdisc del dev wan root 2>/dev/null || true; done; }
block_udp() { # between the two homes, so no direct path is left
	ns natA nft add rule ip filter between ip daddr 203.0.113.20 meta l4proto udp drop
	ns natA nft add rule ip filter between ip saddr 203.0.113.20 meta l4proto udp drop
}
unblock_udp() { ns natA nft flush chain ip filter between; }

ns hostA ping -c1 -W2 203.0.113.1 >/dev/null && ns hostB ping -c1 -W2 203.0.113.1 >/dev/null && ok "both homes reach the internet through NAT" || bad "no route through NAT"

step "rendezvous"
start_rv() {
	ip netns exec rv env PORT=4001 CONCORD_GUEST_PORT=8080 CONCORD_RELAY_SEED=$(printf 'cd%.0s' $(seq 32)) \
		CONCORD_PUBLIC_HOST=203.0.113.1 CONCORD_TURN_SECRET=harness-secret CONCORD_TURN_RELAY_IP=203.0.113.1 \
		CONCORD_TURN_ALLOW_PRIVATE=1 "$RVBIN" >> "$LOG/rv.log" 2>&1 &
	RVPID=$!; PIDS="$PIDS $RVPID"
}
start_rv
ip netns exec rv "$BIN/stund" -l 203.0.113.1:3479 2> "$LOG/stund.log" &
PIDS="$PIDS $!"
for i in $(seq 100); do grep -q '^PeerID' "$LOG/rv.log" 2>/dev/null && break; sleep 0.1; done
RVID=$(sed -n 's/^PeerID: //p' "$LOG/rv.log")
[ -n "$RVID" ] && ok "rendezvous up ($RVID)" || { bad "rendezvous did not start"; cat "$LOG/rv.log"; exit 1; }
grep -q TURN "$LOG/rv.log" && ok "rendezvous offers TURN (ferry must not use it)" || bad "TURN not enabled: $(grep -i turn "$LOG/rv.log" | head -2)"
if [ -n "${CONCORD_RELAY_CIRCUIT_BYTES:-}${CONCORD_RELAY_CIRCUIT_DURATION:-}" ]; then
	grep -q "Relay circuits capped" "$LOG/rv.log" && ok "relay circuits capped: $(sed -n 's/^Relay circuits capped at \(.*\) each.*/\1/p' "$LOG/rv.log")" || bad "CONCORD_RELAY_CIRCUIT_* set but the rendezvous did not cap circuits"
fi

export FERRY_RENDEZVOUS=/ip4/203.0.113.1/tcp/4001/p2p/$RVID
export FERRY_STUN=stun:203.0.113.1:3479
HA=$WORK/home-a; HB=$WORK/home-b
rm -rf "$HA" "$HB"; mkdir -p "$HA" "$HB" "$WORK/in-a" "$WORK/in-b"
FA="ip netns exec hostA env FERRY_HOME=$HA $BIN/ferry"
FB="ip netns exec hostB env FERRY_HOME=$HB $BIN/ferry"
$FA config name ana >/dev/null 2>&1; $FB config name sam >/dev/null 2>&1

# codeof FILE: wait for the code event in a --json stdout file.
codeof() { for i in $(seq 300); do grep -q '"code"' "$1" 2>/dev/null && break; sleep 0.1; done; sed -n 's/.*"code":"\([^"]*\)".*/\1/p' "$1" | head -1; }
field() { sed -n "s/.*\"$2\":\"\{0,1\}\([^\",}]*\).*/\1/p" "$1" | tail -1; }
mkfile() { # path MB
	[ -f "$1" ] && [ "$(stat -c %s "$1")" -eq $(($2*1000000)) ] && return
	head -c $(($2*1000000)) /dev/urandom > "$1"
}

if want pair; then
step "pairing"
$FA pair --json > "$LOG/pair-a.json" 2> "$LOG/pair-a.err" &
P=$!
C=$(codeof "$LOG/pair-a.json")
if $FB pair "$C" --json > "$LOG/pair-b.json" 2> "$LOG/pair-b.err" && wait $P; then
	sa=$(field "$LOG/pair-a.json" check); sb=$(field "$LOG/pair-b.json" check)
	[ -n "$sa" ] && [ "$sa" = "$sb" ] && ok "paired; both sides show the same check words ($sa)" || bad "check words differ: '$sa' vs '$sb'"
else bad "pairing failed: $(tail -1 "$LOG/pair-b.err")"; fi
$FA contacts | grep -q sam && $FB contacts | grep -q ana && ok "each side lists the other" || bad "contacts missing"
fi

if want check; then
step "ferry check"
if $FA check > "$LOG/check.out" 2>&1; then ok "check passes on hostA"; else bad "check failed"; fi
sed "s/^/    /" "$LOG/check.out"
fi

if want direct; then
step "direct through both NATs"
mkfile "$WORK/d.bin" 50
rm -f "$WORK/in-b/d.bin"
ip netns exec hostA env FERRY_HOME="$HA" FERRY_DEBUG_ICE=trace FERRY_LANES=1 "$BIN/ferry" send "$WORK/d.bin" --json > "$LOG/direct-a.json" 2> "$LOG/direct-a.err" &
P=$!
C=$(codeof "$LOG/direct-a.json")
if $FB get "$C" --to "$WORK/in-b" > /dev/null 2> "$LOG/direct-b.err" && wait $P; then
	path=$(field "$LOG/direct-a.json" path)
	[ "$path" = direct ] && cmp -s "$WORK/d.bin" "$WORK/in-b/d.bin" && ok "hole-punched: path direct, file matches" || bad "path was '$path' (ICE log: $LOG/direct-a.err)"
else bad "send failed: $(tail -1 "$LOG/direct-b.err")"; fi
fi

if want wrongcode; then
step "wrong code"
echo hello > "$WORK/small.txt"
$FA send "$WORK/small.txt" --json > "$LOG/wc-a.json" 2> "$LOG/wc-a.err" &
P=$!
C=$(codeof "$LOG/wc-a.json")
W=$(echo "$C" | awk -F- '{w=($4=="zebra")?"acorn":"zebra"; print $1"-"$2"-"$3"-"w}')
set +e
$FB get "$W" --to "$WORK/in-b" > /dev/null 2> "$LOG/wc-b.err"; eb=$?
wait $P; ea=$?
set -e
[ $eb -eq 3 ] && [ $ea -eq 3 ] && ok "wrong code rejected on both sides (exit 3)" || bad "exits were get=$eb send=$ea"
[ ! -e "$WORK/in-b/small.txt" ] && ok "nothing was written" || bad "a file arrived with a wrong code"
fi

if want plaintext; then
step "no plaintext on the wire"
MARK=FERRYMARKERa1b2c3d4
MMSG=FERRYMSGMARKERq9w8e7
yes "$MARK" | head -c 20000000 > "$WORK/marked.txt"
"$BIN/sniff" -i any -m "$MARK" -m "$MMSG" -o "$LOG/sniff.json" 2> "$LOG/sniff.err" &
SN=$!
sleep 0.5
# A file by code (direct), then a message into an offline mailbox.
$FA send "$WORK/marked.txt" --json > "$LOG/pt-a.json" 2> "$LOG/pt-a.err" &
P=$!
C=$(codeof "$LOG/pt-a.json")
rm -f "$WORK/in-b/marked.txt"
$FB get "$C" --to "$WORK/in-b" > /dev/null 2> "$LOG/pt-b.err" && wait $P && cmp -s "$WORK/marked.txt" "$WORK/in-b/marked.txt" \
	&& ok "marked file sent directly" || bad "direct send of marked file failed: $(tail -1 "$LOG/pt-b.err")"
$FA msg sam "$MMSG please" > /dev/null 2>&1 && $FB inbox 2>/dev/null | grep -q "$MMSG" && ok "marked message through the mailbox" || bad "mailbox message failed"
sleep 0.5
kill -TERM $SN; wait $SN || true
frames=$(sed -n 's/.*"frames":\([0-9]*\).*/\1/p' "$LOG/sniff.json")
if grep -q "\"$MARK\":0" "$LOG/sniff.json" && grep -q "\"$MMSG\":0" "$LOG/sniff.json" && [ "${frames:-0}" -gt 1000 ]; then
	ok "no marker in $frames frames crossing the internet (rendezvous included)"
else bad "plaintext seen or nothing captured: $(cat "$LOG/sniff.json")"; fi
fi

if want nopath; then
step "no direct path"
# UDP between the homes is blocked. The rendezvous offers TURN, but ferry
# sends directly or not at all, and says so plainly.
block_udp
mkfile "$WORK/r.bin" 20
rm -f "$WORK/in-b/r.bin"
$FB get --from ana --to "$WORK/in-b" > /dev/null 2> "$LOG/nopath-b.err" &
P=$!
sleep 2
set +e
$FA send sam "$WORK/r.bin" > /dev/null 2> "$LOG/nopath-a.err"; ea=$?
set -e
kill $P 2>/dev/null || true; wait $P 2>/dev/null || true
[ $ea -eq 4 ] && grep -q "no direct path" "$LOG/nopath-a.err" && ok "direct UDP blocked: send exits 4 and says there is no direct path" || bad "exit $ea: $(head -1 "$LOG/nopath-a.err")"
[ ! -e "$WORK/in-b/r.bin" ] && ok "no data went any other way" || bad "the file arrived without a direct path"
unblock_udp
fi

if want forward; then
step "port forward through strict NATs"
# Both routers map every destination to a new random port, so hole
# punching fails; sam's router then forwards udp 40000 to sam.
for n in A B; do
	ns "nat$n" nft flush chain ip nat post
	ns "nat$n" nft add rule ip nat post oifname wan masquerade random
done
mkfile "$WORK/f.bin" 50
fwd() { # label, extra env for sam
	rm -f "$WORK/in-b/f.bin"
	ns hostB env FERRY_HOME=$HB $2 "$BIN/ferry" get --from ana --to "$WORK/in-b" > /dev/null 2> "$LOG/fwd-$1-b.err" &
	P=$!
	sleep 2
	timeout 60 ip netns exec hostA env FERRY_HOME=$HA "$BIN/ferry" send sam "$WORK/f.bin" --json > "$LOG/fwd-$1-a.json" 2> "$LOG/fwd-$1-a.err"
	r=$?
	kill $P 2>/dev/null; wait $P 2>/dev/null
	return $r
}
if fwd none ""; then bad "direct worked without a forward; the NATs are not strict enough to test this"
elif grep -q "no direct path" "$LOG/fwd-none-a.err"; then ok "strict NATs: no direct path, as expected"
else bad "send failed for another reason: $(tail -1 "$LOG/fwd-none-a.err")"; fi
ns natB nft "add chain ip nat pre { type nat hook prerouting priority -100; }"
ns natB nft add rule ip nat pre iifname wan udp dport 40000 dnat to 192.168.20.2:40000
ns natB nft insert rule ip nat post ip saddr 192.168.20.2 udp sport 40000 snat to 203.0.113.20:40000
if fwd port FERRY_PORT=40000 && cmp -s "$WORK/f.bin" "$WORK/in-b/f.bin"; then
	ok "sam forwards udp 40000: 50 MB went $(field "$LOG/fwd-port-a.json" path) and matches"
else bad "send with a forwarded port failed: $(tail -1 "$LOG/fwd-port-a.err")"; fi
ns natB nft delete chain ip nat pre 2>/dev/null || ns natB nft flush chain ip nat pre
for n in A B; do
	ns "nat$n" nft flush chain ip nat post
	ns "nat$n" nft add rule ip nat post oifname wan masquerade
done
fi

# speed SIZE_MB LABEL: one code transfer, prints MB/s.
speed() {
	mkfile "$WORK/s.bin" "$1"
	rm -f "$WORK/in-b/s.bin"
	$FA send "$WORK/s.bin" --json > "$LOG/sp-a.json" 2> "$LOG/sp-a.err" &
	P=$!
	C=$(codeof "$LOG/sp-a.json")
	$FB get "$C" --to "$WORK/in-b" --json > "$LOG/sp-b.json" 2> "$LOG/sp-b.err" && wait $P || { bad "$2: transfer failed: $(tail -1 "$LOG/sp-b.err")"; return; }
	secs=$(field "$LOG/sp-b.json" seconds)
	path=$(field "$LOG/sp-a.json" path)
	mbps=$(awk -v b="$(($1*1000000))" -v s="$secs" 'BEGIN{printf "%.0f", b/s/1e6}')
	cmp -s "$WORK/s.bin" "$WORK/in-b/s.bin" && ok "$2: $1 MB at $mbps MB/s ($(awk -v m="$mbps" 'BEGIN{printf "%.2f", m*8/1000}') Gbps, $path)" || bad "$2: content differs"
	echo "$2 $mbps" >> "$LOG/speeds"
}

if want speed; then
step "throughput"
unshape
speed 2000 "LAN-ish (veth, no shaping)"
shape 20ms 0%
speed 1000 "WAN 40 ms RTT, no loss"
shape 20ms 0.25%
speed 1000 "WAN 40 ms RTT, 0.5% loss"
unshape
fi

if want resume; then
step "${BIG} MB, killed part way, resumed (40 ms RTT, 0.5% loss)"
mkfile "$WORK/big.bin" "$BIG"
rm -rf "$WORK/in-b/big.bin" "$WORK/in-b/big.bin.ferry-part" "$WORK"/in-b/.ferry-*
shape 20ms 0.25%
$FA send "$WORK/big.bin" --json > "$LOG/big-a.json" 2> "$LOG/big-a.err" &
PA=$!
C=$(codeof "$LOG/big-a.json")
$FB get "$C" --to "$WORK/in-b" --json > "$LOG/big-b.json" 2> "$LOG/big-b.err" &
PB=$!
# Wait until about a third has arrived, then kill the receiver outright.
third=$((BIG*1000000/3))
for i in $(seq 1200); do
	n=$(sed -n 's/.*"bytes":\([0-9]*\).*/\1/p' "$LOG/big-b.err" | tail -1)
	[ "${n:-0}" -gt "$third" ] && break
	sleep 0.25
done
kill -KILL $PB 2>/dev/null || true; wait $PB 2>/dev/null || true
# The sender must notice on its own that its peer vanished.
for i in $(seq 90); do kill -0 $PA 2>/dev/null || break; sleep 1; done
if kill -0 $PA 2>/dev/null; then bad "sender still running 90 s after the receiver died"; kill $PA; else ok "sender noticed the receiver vanished after ${i} s and stopped"; fi
wait $PA 2>/dev/null || true
part=$(stat -c %s "$WORK/in-b/big.bin.ferry-part" 2>/dev/null || echo 0)
[ "$part" -gt 0 ] && ok "receiver killed at ${n:-0} bytes; partial file kept" || bad "no partial file after kill"
$FA send "$WORK/big.bin" --json > "$LOG/big2-a.json" 2> "$LOG/big2-a.err" &
PA=$!
C=$(codeof "$LOG/big2-a.json")
if $FB get "$C" --to "$WORK/in-b" --json > "$LOG/big2-b.json" 2> "$LOG/big2-b.err" && wait $PA; then
	first=$(sed -n 's/.*"bytes":\([0-9]*\).*/\1/p' "$LOG/big2-b.err" | head -1)
	[ "${first:-0}" -gt 0 ] && ok "second attempt resumed from ${first} bytes" || bad "second attempt started from zero"
	ha=$(sha256sum "$WORK/big.bin" | cut -d' ' -f1); hb=$(sha256sum "$WORK/in-b/big.bin" | cut -d' ' -f1)
	[ "$ha" = "$hb" ] && ok "${BIG} MB arrived whole: sha256 $ha" || bad "sha256 differs"
	secs=$(field "$LOG/big2-b.json" seconds)
	echo "resume-leg ${secs}s" >> "$LOG/speeds"
else bad "resume failed: $(tail -2 "$LOG/big2-b.err")"; fi
unshape
rm -f "$WORK/in-b/big.bin"
fi

if want blip; then
step "Wi-Fi blip: the line goes dead for 8 s mid-transfer"
mkfile "$WORK/blip.bin" 1500
rm -f "$WORK/in-b/blip.bin"
shape 20ms 0%
$FA send "$WORK/blip.bin" --json > "$LOG/blip-a.json" 2> "$LOG/blip-a.err" &
PA=$!
C=$(codeof "$LOG/blip-a.json")
$FB get "$C" --to "$WORK/in-b" --json > "$LOG/blip-b.json" 2> "$LOG/blip-b.err" &
PB=$!
for i in $(seq 400); do n=$(sed -n 's/.*"bytes":\([0-9]*\).*/\1/p' "$LOG/blip-b.err" | tail -1); [ "${n:-0}" -gt 300000000 ] && break; sleep 0.25; done
ns natA nft add rule ip filter between drop
sleep 8
ns natA nft flush chain ip filter between
if wait $PB && wait $PA && cmp -s "$WORK/blip.bin" "$WORK/in-b/blip.bin"; then ok "transfer rode out the outage and completed intact"; else bad "transfer did not survive: $(tail -1 "$LOG/blip-b.err")"; fi
unshape
fi

# Only when asked for by name: it waits out the relay's 2-minute circuit limit.
case " $ONLY " in *" longlived "*)
step "contacts stay reachable past the relay's circuit limit"
$FB up > "$LOG/ll-b.out" 2>&1 &
UB=$!
$FA up > "$LOG/ll-a.out" 2>&1 &
UA=$!
sleep 4
r1=$($FA msg sam "longlived one" 2>&1 || true)
sleep 130
r2=$($FA msg sam "longlived two" 2>&1 || true)
kill $UA $UB 2>/dev/null || true; wait $UA $UB 2>/dev/null || true
case "$r1|$r2" in
*delivered*"|"*delivered*) ok "two live messages 130 s apart through the agents, both delivered" ;;
*) bad "first: $r1 / second: $r2" ;;
esac
$FB inbox 2>/dev/null | grep -q "longlived two" && ok "the later one is in sam's inbox" || bad "the later message never arrived"
;;
esac

if want mcp; then
step "MCP: two model clients"
mkfile "$WORK/m.bin" 20
rm -rf "$WORK/fetched"; mkdir -p "$WORK/fetched"
envs="FERRY_RENDEZVOUS=$FERRY_RENDEZVOUS FERRY_STUN=$FERRY_STUN"
if ns hostA "$BIN/mcpdrive" \
	-a "ip netns exec hostA env $envs FERRY_HOME=$HA $BIN/ferry mcp" \
	-b "ip netns exec hostB env $envs FERRY_HOME=$HB $BIN/ferry mcp" \
	-file "$WORK/m.bin" -dest "$WORK/fetched" > "$LOG/mcp.out" 2> "$LOG/mcp.err"; then
	sed 's/^PASS /  mcp: /' "$LOG/mcp.out" | grep -v 'round trip'
	ok "MCP tools round-trip between two clients"
else bad "MCP: $(grep FAIL "$LOG/mcp.err" | head -1)"; fi
fi

if want pipe; then
step "pipe: a TCP port through the direct link (what ssh's ProxyCommand uses)"
# sam serves a port that answers with the sha256 of everything it read: this
# checks every byte and the half-close (the answer only comes after EOF).
cat > "$WORK/shaecho.py" <<'PY'
import hashlib, socket, sys
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(sys.argv[1]))); s.listen(4)
while True:
    c, _ = s.accept(); h = hashlib.sha256()
    while (b := c.recv(1 << 16)): h.update(b)
    c.sendall(h.hexdigest().encode() + b"\n"); c.close()
PY
ns hostB python3 "$WORK/shaecho.py" 7000 & PIDS="$PIDS $!"
$FB up --allow-pipe 7000 > "$LOG/pipe-up.out" 2> "$LOG/pipe-up.err" & PIDS="$PIDS $!"
for i in $(seq 100); do grep -q "ferry is up" "$LOG/pipe-up.err" 2>/dev/null && break; sleep 0.1; done
mkfile "$WORK/p.bin" 100
want_sum=$(sha256sum < "$WORK/p.bin" | cut -d' ' -f1)
t0=$(date +%s.%N)
got_sum=$(timeout 120 $FA pipe sam 7000 < "$WORK/p.bin" 2> "$LOG/pipe-a.err")
t1=$(date +%s.%N)
if [ "$got_sum" = "$want_sum" ]; then
	mbs=$(echo "100 / ($t1 - $t0)" | bc -l | cut -c1-5)
	ok "100 MB through ferry pipe, sha256 matches, ${mbs} MB/s (setup included)"
	echo "pipe 100MB ${mbs} MB/s" >> "$LOG/speeds"
else bad "pipe: sha mismatch or failure: '$got_sum' $(tail -1 "$LOG/pipe-a.err")"; fi
if echo hi | timeout 60 $FA pipe sam 7001 > /dev/null 2> "$LOG/pipe-deny.err"; then bad "a port not in --allow-pipe was served"
else
	grep -q "not open for pipes" "$LOG/pipe-deny.err" && ok "a port not in --allow-pipe is refused: $(tail -1 "$LOG/pipe-deny.err" | sed 's/.*ferry: //')" || bad "refused, but unclearly: $(tail -1 "$LOG/pipe-deny.err")"
fi
grep -q "refused pipe from ana to port 7001" "$LOG/pipe-up.err" && ok "the refusal is logged on sam's side" || bad "no refusal in sam's log"
# A stranger who knows sam's key (a copied address book) but never paired.
HC=$WORK/home-c; rm -rf "$HC"; mkdir -p "$HC"
FC="ip netns exec hostA env FERRY_HOME=$HC $BIN/ferry"
$FC config name eve >/dev/null 2>&1; cp "$HA/contacts.json" "$HC/contacts.json"
if echo hi | timeout 60 $FC pipe sam 7000 > "$LOG/pipe-eve.out" 2> "$LOG/pipe-eve.err"; then bad "an unpaired peer got a pipe: $(cat "$LOG/pipe-eve.out")"
else ok "an unpaired peer is refused: $(tail -1 "$LOG/pipe-eve.err" | sed 's/.*ferry: //' | cut -c1-80)"; fi
sleep 1; grep -q "refused .*not a contact" "$LOG/pipe-up.err" && ok "the stranger is logged on sam's side" || bad "stranger not logged"
# The rendezvous restarts (an outage, a deploy): a running ferry up must come back on its own.
kill "$RVPID"; sleep 3; start_rv
for i in $(seq 100); do [ "$(grep -c '^PeerID' "$LOG/rv.log")" -ge 2 ] && break; sleep 0.1; done
sleep 35
got_sum=$(echo after-restart | timeout 60 $FA pipe sam 7000 2> "$LOG/pipe-rv.err")
[ "$got_sum" = "$(echo after-restart | sha256sum | cut -d' ' -f1)" ] && ok "after the rendezvous restarted, sam's running ferry up was reachable again" || bad "sam unreachable after a rendezvous restart: $(tail -1 "$LOG/pipe-rv.err")"
fi

step "result"
[ -f "$LOG/speeds" ] && cp "$LOG/speeds" "$WORK/speeds.txt"
echo "  $pass passed, $failed failed. Logs: $LOG"
[ $failed -eq 0 ]
