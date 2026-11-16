#!/usr/bin/env bash
# Measure validator memory under load. The validator-memory suite sets CAP,
# ROUNDS, IDLE, COMMIT and SVMD_ENV.
#
# The script runs a validator under a CAP memory limit, fills blocks to
# block.max_bytes, and runs 16 RPC readers at the same time. `nodetool` samples
# cgroup, process and Go runtime memory every second. Heap profiles at
# intervals give the breakdown.
#
# The chain starts empty, so the results leave out the cost of the IAVL node
# cache and of the account database page cache.
set -euo pipefail
# shellcheck source=bench/lib.sh
. "$ROOT/bench/lib.sh"
WORK=$(mktemp -d "$DATA/validator-memory.XXXX")
H=$WORK/h0
UNIT=bench-validator-memory-svmd
RPC=http://127.0.0.1:8899
# capped puts the node in a scope of this user's systemd under BENCH_USER_SCOPE.
user=()
[ -z "${BENCH_USER_SCOPE:-}" ] || user=(--user)
cleanup() {
    jobs -p | xargs -r kill 2>/dev/null || true
    if [ ${#user[@]} -gt 0 ]; then
        systemctl --user stop "$UNIT.scope" 2>/dev/null || true
    else
        sudo -n systemctl stop "$UNIT.scope" 2>/dev/null || true
    fi
}
trap cleanup EXIT

one_validator "$WORK" svm-validator-memory
"$BIN/fill" -mode keys -payers 256 >"$WORK/payers.txt"
"$BIN/nodetool" fund -genesis "$H/config/genesis.json" -payers "$WORK/payers.txt"

consensus=(pprof_laddr='"localhost:6060"' size=20000 max_txs_bytes=8313477)
[ -z "$COMMIT" ] || consensus+=(timeout_commit="\"$COMMIT\"")
"$BIN/nodetool" set "$H/config/config.toml" "${consensus[@]}"
"$BIN/nodetool" set "$H/config/app.toml" min-retain-blocks=2000 index-max-bytes=1073741824
cp "$H/config/config.toml" "$H/config/app.toml" "$OUT/"

# SVMD_ENV is set for svmd and its svm-tpu child alone, not for curl, fill or
# the sampler, so a preloaded allocator profiles the node and nothing else.
# shellcheck disable=SC2086 # SVMD_ENV is VAR=VALUE words
capped "$UNIT" "$CAP" env $SVMD_ENV "$ROOT/bin/svmd" start --home "$H" >"$OUT/svmd.log" 2>&1 &
serving() {
    curl -s -m 2 "$RPC" -H 'content-type: application/json' \
        -d '{"jsonrpc":"2.0","id":1,"method":"getSlot"}' | grep -q result
}
wait_for 60 "svmd to answer getSlot" serving
CG=/sys/fs/cgroup$(systemctl "${user[@]}" show -P ControlGroup "$UNIT.scope")
PID=$(head -1 "$CG/cgroup.procs")
echo "svmd $PID in $CG"

echo idle >"$WORK/phase"
"$BIN/nodetool" sample -cgroup "$CG" -pid "$PID" -phase "$WORK/phase" >>"$OUT/samples.txt" &
profile() { curl -sf -m 30 http://localhost:6060/debug/pprof/heap -o "$OUT/heap-$1.pb.gz"; }
sleep "$IDLE"
profile idle

# A reader polls an account and the latest blockhash, as a wallet does. It
# sends no simulations. A reader starts a new curl process for each request,
# which adds load on the same four cores.
reader() {
    local k
    while true; do
        k=$(shuf -n 1 "$WORK/payers.txt")
        curl -s -m 5 "$RPC" -H 'content-type: application/json' -d \
            "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"getAccountInfo\",\"params\":[\"$k\",{\"encoding\":\"base64\"}]}" >/dev/null || true
        curl -s -m 5 "$RPC" -H 'content-type: application/json' \
            -d '{"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash"}' >/dev/null || true
    done
}
drained() { [ "$("$BIN/nodetool" pending)" = 0 ]; }

echo fill >"$WORK/phase"
readers=()
for _ in $(seq 16); do
    reader &
    readers+=($!)
done
for round in $(seq "$ROUNDS"); do
    "$BIN/fill" -url "$RPC" -bytes 8388608 -over 2.5 >>"$OUT/fill.txt" 2>&1
    # A round ends when the pool drains into blocks, not when fill returns,
    # because fill only hands the transactions over. A pool that does not
    # drain means a stalled chain, and every later row would measure the stall.
    wait_for 180 "the pool to drain into blocks" drained
    if [ $((round % 4)) -eq 0 ]; then profile "fill-$round"; fi
done
kill "${readers[@]}"

echo drain >"$WORK/phase"
sleep "$IDLE"
profile drain
cp "$CG/memory.peak" "$OUT/memory.peak"
# Record the sizes of the last 20 blocks, to show that blocks reached the byte limit.
curl -s http://127.0.0.1:26657/blockchain >"$OUT/blockchain.json"
"$BIN/nodetool" blocks <"$OUT/blockchain.json" >"$OUT/blocks.txt"
echo "done"
