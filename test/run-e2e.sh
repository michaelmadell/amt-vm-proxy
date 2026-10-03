#!/usr/bin/env bash
# End-to-end test: real ui-toolkit KVM client (Node) -> Console relay logic ->
# fake-amt -> test VNC server. Needs Go, Node 22+, and a ui-toolkit checkout
# with dependencies installed (npm ci && npm i --no-save tsx).
#
#   UI_TOOLKIT=/path/to/ui-toolkit ./test/run-e2e.sh            # quick
#   UI_TOOLKIT=/path/to/ui-toolkit IDLE=1 ./test/run-e2e.sh     # + 70s idle/keepalive checks
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD
UI_TOOLKIT=${UI_TOOLKIT:?set UI_TOOLKIT to a ui-toolkit checkout}
OUT=${OUT:-$(mktemp -d)}
echo "logs in $OUT"

go build -o "$OUT/fake-amt" .
go build -o "$OUT/fakevnc" ./test/fakevnc
(cd test/relay && GOFLAGS=-mod=mod go build -o "$OUT/relay" .)

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; }
trap cleanup EXIT

start_stack() { # name bind relayport vncport keepalive [vnc flags...]
  local name=$1 bind=$2 rport=$3 vport=$4 ka=$5; shift 5
  "$OUT/fakevnc" -listen "127.0.0.1:$vport" "$@" >>"$OUT/$name-vnc.jsonl" 2>"$OUT/$name-vnc.err" & pids+=($!)
  "$OUT/fake-amt" -bind "$bind" -vnc "127.0.0.1:$vport" -keepalive "$ka" -trace-pointer all >"$OUT/$name-amt.log" 2>&1 & pids+=($!)
  "$OUT/relay" -listen "127.0.0.1:$rport" -device "$bind" >"$OUT/$name-relay.log" 2>&1 & pids+=($!)
}

start_stack main 127.0.0.1 8181 5901 10s
sleep 1
rc=0
for bpp in 2 1; do
  : >"$OUT/main-vnc.jsonl"
  (cd "$UI_TOOLKIT" && UI_TOOLKIT="$UI_TOOLKIT" BPP=$bpp RELAY=ws://127.0.0.1:8181/relay FAKEVNC_LOG="$OUT/main-vnc.jsonl" \
    npx tsx "$ROOT/test/kvm-client.ts") || rc=1
  sleep 0.5
done

if [[ "${IDLE:-}" == 1 ]]; then
  # Static desktop: VNC answers incremental requests with nothing, like an idle VM.
  start_stack ka 127.0.0.2 8182 5902 10s -static
  start_stack noka 127.0.0.3 8183 5903 0 -static
  sleep 1
  (cd "$UI_TOOLKIT" && BPP=2 RELAY=ws://127.0.0.1:8182/relay FAKEVNC_LOG="$OUT/ka-vnc.jsonl" IDLE_SECONDS=65 \
    npx tsx "$ROOT/test/kvm-client.ts") || rc=1 &
  p1=$!
  (cd "$UI_TOOLKIT" && BPP=2 RELAY=ws://127.0.0.1:8183/relay FAKEVNC_LOG="$OUT/noka-vnc.jsonl" IDLE_SECONDS=65 EXPECT_IDLE=dropped \
    npx tsx "$ROOT/test/kvm-client.ts") || rc=1 &
  p2=$!
  wait $p1 || rc=1
  wait $p2 || rc=1
  echo "--- relay log, keepalive off:"; grep -i "closing" "$OUT/noka-relay.log" || true
fi

echo "--- fake-amt log (main):"; grep -v "pointer x=" "$OUT/main-amt.log" | tail -25
exit $rc
