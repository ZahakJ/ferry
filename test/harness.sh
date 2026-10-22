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
#                          mailbox, TURN on tcp/3478) and a STUN server
#
# Both NATs masquerade like home routers; netem on their WAN side shapes
# the path. Then it proves, in order: pairing, a wrong code rejected,
# no plaintext on the wire (rendezvous and TURN included), relay fallback
# when direct UDP is blocked, throughput, a 5 GB transfer killed part way
# and resumed with a hash match, and two MCP clients talking.
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
block_udp() { # between the two homes, so only TURN is left
	ns natA nft add rule ip filter between ip daddr 203.0.113.20 meta l4proto udp drop
	ns natA nft add rule ip filter between ip saddr 203.0.113.20 meta l4proto udp drop
}
unblock_udp() { ns natA nft flush chain ip filter between; }

ns hostA ping -c1 -W2 203.0.113.1 >/dev/null && ns hostB ping -c1 -W2 203.0.113.1 >/dev/null && ok "both homes reach the internet through NAT" || bad "no route through NAT"

step "rendezvous"
ip netns exec rv env PORT=4001 CONCORD_GUEST_PORT=8080 CONCORD_RELAY_SEED=$(printf 'cd%.0s' $(seq 32)) \
	CONCORD_PUBLIC_HOST=203.0.113.1 CONCORD_TURN_SECRET=harness-secret CONCORD_TURN_RELAY_IP=203.0.113.1 \
	CONCORD_TURN_ALLOW_PRIVATE=1 "$RVBIN" > "$LOG/rv.log" 2>&1 &
PIDS="$PIDS $!"
ip netns exec rv "$BIN/stund" -l 203.0.113.1:3479 2> "$LOG/stund.log" &
PIDS="$PIDS $!"
for i in $(seq 100); do grep -q '^PeerID' "$LOG/rv.log" 2>/dev/null && break; sleep 0.1; done
RVID=$(sed -n 's/^PeerID: //p' "$LOG/rv.log")
[ -n "$RVID" ] && ok "rendezvous up ($RVID)" || { bad "rendezvous did not start"; cat "$LOG/rv.log"; exit 1; }
grep -q TURN "$LOG/rv.log" && ok "TURN relay enabled" || bad "TURN not enabled: $(grep -i turn "$LOG/rv.log" | head -2)"

export FERRY_RENDEZVOUS=/ip4/203.0.113.1/tcp/4001/p2p/$RVID
export FERRY_RENDEZVOUS_HTTP=http://203.0.113.1:8080
export FERRY_STUN=stun:203.0.113.1:3479
HA=$WORK/home-a; HB=$WORK/home-b
rm -rf "$HA" "$HB"; mkdir -p "$HA" "$HB" "$WORK/in-a" "$WORK/in-b"
FA="ip netns exec hostA env FERRY_HOME=$HA $BIN/ferry"
FB="ip netns exec hostB env FERRY_HOME=$HB $BIN/ferry"
$FA config name avi >/dev/null 2>&1; $FB config name zyad >/dev/null 2>&1

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
$FA contacts | grep -q zyad && $FB contacts | grep -q avi && ok "each side lists the other" || bad "contacts missing"
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
# A file by code (direct), a message into an offline mailbox, then the
# same file again through TURN.
$FA send "$WORK/marked.txt" --json > "$LOG/pt-a.json" 2> "$LOG/pt-a.err" &
P=$!
C=$(codeof "$LOG/pt-a.json")
rm -f "$WORK/in-b/marked.txt"
$FB get "$C" --to "$WORK/in-b" > /dev/null 2> "$LOG/pt-b.err" && wait $P && cmp -s "$WORK/marked.txt" "$WORK/in-b/marked.txt" \
	&& ok "marked file sent directly" || bad "direct send of marked file failed: $(tail -1 "$LOG/pt-b.err")"
$FA msg zyad "$MMSG please" > /dev/null 2>&1 && $FB inbox 2>/dev/null | grep -q "$MMSG" && ok "marked message through the mailbox" || bad "mailbox message failed"
block_udp
rm -f "$WORK/in-b/marked.txt"
$FB get --from avi --to "$WORK/in-b" > /dev/null 2> "$LOG/pt-b2.err" &
P=$!
sleep 2
$FA send zyad "$WORK/marked.txt" --json > "$LOG/pt-a2.json" 2> "$LOG/pt-a2.err" && wait $P && cmp -s "$WORK/marked.txt" "$WORK/in-b/marked.txt" \
	&& ok "marked file through TURN ($(field "$LOG/pt-a2.json" path))" || bad "TURN send of marked file failed: $(tail -1 "$LOG/pt-a2.err")"
unblock_udp
sleep 0.5
kill -TERM $SN; wait $SN || true
frames=$(sed -n 's/.*"frames":\([0-9]*\).*/\1/p' "$LOG/sniff.json")
if grep -q "\"$MARK\":0" "$LOG/sniff.json" && grep -q "\"$MMSG\":0" "$LOG/sniff.json" && [ "${frames:-0}" -gt 1000 ]; then
	ok "no marker in $frames frames crossing the internet (rendezvous, relay and TURN included)"
else bad "plaintext seen or nothing captured: $(cat "$LOG/sniff.json")"; fi
fi

if want relay; then
step "relay fallback"
block_udp
mkfile "$WORK/r.bin" 200
rm -f "$WORK/in-b/r.bin"
$FB get --from avi --to "$WORK/in-b" > /dev/null 2> "$LOG/relay-b.err" &
P=$!
sleep 2
if $FA send zyad "$WORK/r.bin" --json > "$LOG/relay-a.json" 2> "$LOG/relay-a.err" && wait $P; then
	path=$(field "$LOG/relay-a.json" path)
	[ "$path" = "relay (TURN)" ] && cmp -s "$WORK/r.bin" "$WORK/in-b/r.bin" && ok "direct UDP blocked; 200 MB went through TURN and matches" || bad "path was '$path'"
else bad "relay send failed: $(tail -1 "$LOG/relay-a.err")"; fi
unblock_udp
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
kill -KILL $PB 2>/dev/null; wait $PB 2>/dev/null || true
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

if want mcp; then
step "MCP: two model clients"
mkfile "$WORK/m.bin" 20
rm -rf "$WORK/fetched"; mkdir -p "$WORK/fetched"
envs="FERRY_RENDEZVOUS=$FERRY_RENDEZVOUS FERRY_RENDEZVOUS_HTTP=$FERRY_RENDEZVOUS_HTTP FERRY_STUN=$FERRY_STUN"
if ns hostA "$BIN/mcpdrive" \
	-a "ip netns exec hostA env $envs FERRY_HOME=$HA $BIN/ferry mcp" \
	-b "ip netns exec hostB env $envs FERRY_HOME=$HB $BIN/ferry mcp" \
	-file "$WORK/m.bin" -dest "$WORK/fetched" > "$LOG/mcp.out" 2> "$LOG/mcp.err"; then
	sed 's/^PASS /  mcp: /' "$LOG/mcp.out" | grep -v 'round trip'
	ok "MCP tools round-trip between two clients"
else bad "MCP: $(grep FAIL "$LOG/mcp.err" | head -1)"; fi
fi

step "result"
[ -f "$LOG/speeds" ] && cp "$LOG/speeds" "$WORK/speeds.txt"
echo "  $pass passed, $failed failed. Logs: $LOG"
[ $failed -eq 0 ]
