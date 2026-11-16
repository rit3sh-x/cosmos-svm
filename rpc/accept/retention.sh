#!/usr/bin/env bash
# Tests evidence acceptance and block retention over the configured evidence
# window, with a real svmd validator.
#
#   make accept-retention
#
# A second validator is bonded at a tenth of the first one's power and never
# starts. Its key signs conflicting votes. The test checks that the chain
# accepts old evidence and tombstones the signer. It also checks that block
# retention follows the evidence window, not the snapshot settings.
#
# A short commit timeout makes the test fast. The assertions use block heights.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
RESULTS=${SVM_RETENTION_RESULTS:-$ROOT/rpc/accept/results/retention-$(date -u +%Y%m%dT%H%M%SZ)}
CHAIN=svm-retention
# Ports that do not collide with a localnet on the default ports.
CMT=http://127.0.0.1:28657
EVIDENCE_AT=5

# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
stop() {
    stop_pidfile "$WORK/p0.pid"
    rm -rf "$WORK"
}
trap stop EXIT

# Wait for the validator to reach a height. On failure, print its log.
await() {
    await_height "the validator" "$CMT" "$1" "$2" "${3:-1800}" || {
        tail -40 "$WORK/p0.log" >&2
        exit 1
    }
}

mkdir -p "$RESULTS"
build svmd fill nodetool

# The genesis
for i in 0 1; do
    "$WORK/svmd" init "v$i" --chain-id "$CHAIN" --home "$WORK/h$i" > "$WORK/init$i.log" 2>&1
done
for i in 0 1; do
    "$WORK/nodetool" validators -out "$WORK/validators.json" -bond $((i == 0 ? 10000000 : 1000000)) \
        -genesis "$WORK/h$i/config/genesis.json" -pubkey "$("$WORK/svmd" comet show-validator --home "$WORK/h$i")"
done
"$WORK/svmd" init coordinator --chain-id "$CHAIN" --home "$WORK/h0" --overwrite \
    --validators-file "$WORK/validators.json" > "$WORK/genesis.log" 2>&1
WINDOW=$(json '["consensus"]["params"]["evidence"]["max_age_num_blocks"]' <"$WORK/h0/config/genesis.json")
AGE=$(json '["consensus"]["params"]["evidence"]["max_age_duration"]' <"$WORK/h0/config/genesis.json")
echo "genesis: evidence window $WINDOW blocks, $AGE ns"

# The configs
"$WORK/nodetool" set "$WORK/h0/config/config.toml" proxy_app='"tcp://127.0.0.1:28658"' \
    rpc.laddr='"tcp://127.0.0.1:28657"' p2p.laddr='"tcp://0.0.0.0:28656"' pprof_laddr='"localhost:8060"' \
    prometheus_listen_addr='":28660"' timeout_commit='"100ms"'
"$WORK/nodetool" set "$WORK/h0/config/app.toml" index-max-bytes=1073741824 grpc.address='"localhost:11090"' \
    snapshot-interval=100 program-cache-bytes=268435456 threads=2
grep -E '^(min-retain-blocks|snapshot-interval|snapshot-keep-recent) =' "$WORK/h0/config/app.toml" | tee "$RESULTS/settings.txt"

spawn p0 "$WORK/svmd" start --home "$WORK/h0" \
    --svm.rpc-address 127.0.0.1:10899 --svm.ws-address 127.0.0.1:10900
await $((EVIDENCE_AT + 2)) "the chain is producing blocks" 240

# Evidence 1,500 blocks back
read -r TOMBSTONED _ _ _ < <("$WORK/fill" -mode validator -home "$WORK/h1" -node "$CMT")
[ "$TOMBSTONED" = false ] || { echo "the second operator is tombstoned before it signed twice" >&2; exit 1; }
await $((EVIDENCE_AT + 1500)) "1,500 blocks past the equivocation"
FROM=$(height "$CMT")
"$WORK/fill" -mode evidence -home "$WORK/h1" -node "$CMT" -chain "$CHAIN" -height "$EVIDENCE_AT"
read -r CARRIED AGE_BLOCKS < <(python3 consensus.py evidence "$CMT" "$FROM" "$EVIDENCE_AT" 300)
read -r TOMBSTONED JAILED TOKENS _ < <("$WORK/fill" -mode validator -home "$WORK/h1" -node "$CMT")
echo "  height $CARRIED carried the evidence from height $EVIDENCE_AT, $AGE_BLOCKS blocks back:" \
     "tombstoned $TOMBSTONED, jailed $JAILED, bonded $TOKENS" | tee "$RESULTS/evidence.txt"
if [ "$TOMBSTONED" != true ] || [ "$AGE_BLOCKS" -lt 1500 ]; then
    echo "evidence from $AGE_BLOCKS blocks back did not tombstone the signer" >&2
    exit 1
fi

# The machine keeps 2,000 blocks. CometBFT saves a block before it executes it
# and prunes only after the commit, so a status read between the two sees one
# block more than the window. The check waits for the window and does not trust
# one read. A machine that never prunes to the window still fails.
await 2110 "past 2,100 blocks"
holds_window() {
    read -r EARLIEST LATEST < <(python3 consensus.py window "$CMT") && [ $((LATEST - EARLIEST)) -eq "$WINDOW" ]
}
if ! wait_for 60 "the machine to hold the $WINDOW-block window and its tip" holds_window; then
    echo "the machine holds $((LATEST - EARLIEST + 1)) blocks, want the $WINDOW-block window and its tip" >&2
    exit 1
fi
echo "  at height $LATEST the earliest block held is $EARLIEST, $((LATEST - EARLIEST)) below the tip" |
    tee "$RESULTS/retention.txt"
du -sh "$WORK/h0/data/blockstore.db" "$WORK/h0/data/state.db" | tee -a "$RESULTS/retention.txt"
cp "$WORK/validators.json" "$RESULTS/validators.json"
{
    echo "rpc/accept/retention.sh"
    echo
    echo "One validator with min-retain-blocks at the shipped 2000 and snapshot-interval 100,"
    echo "a second operator bonded at a tenth of its power and never started, chain id"
    echo "$CHAIN, timeout_commit 100 ms. settings.txt is the app.toml it ran with."
    echo "evidence.txt is evidence from $EVIDENCE_AT carried $AGE_BLOCKS blocks later."
    echo "retention.txt is the lowest block held past 2,100 blocks, and the disk it took."
} > "$RESULTS/README.txt"
echo "results in $RESULTS"
echo PASS
