#!/usr/bin/env bash
# Acceptance for `svmd rollback` on a Pebble home.
#
#   make accept-rollback
#
# rollback is the SDK's command. It opens application.db through cosmos-db's
# own Pebble constructor, not svmd's opener, rolls the IAVL stores back one
# height and exits without closing the database. cosmos-db commits IAVL's
# batches to Pebble without a sync, and Pebble writes its log on a goroutine of
# its own. The SDK's last write, the commit info, is synced, and a synced write
# carries every record before it in the log. That is what keeps the rollback.
# If it stopped, CometBFT would not notice: an app one block ahead of its state
# is replayed from the saved block results, and the rollback is silently undone.
#
#   1. one validator finalizes a transfer, then stops on TERM
#   2. rollback exits 0, without a panic, and names height N
#   3. on restart, the handshake finds the app at N and block N+1 in the
#      store, so block N+1 runs again against the app
#   4. the chain gets past N+2, and the transfer is still there
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
SVM_RPC=http://127.0.0.1:8899
CMT=http://127.0.0.1:26657
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
stop() {
    stop_pidfile "$WORK/node.pid"
    rm -rf "$WORK"
}
trap stop EXIT
need solana solana-keygen

# shellcheck disable=SC2119 # no extra start flags
start_local_chain
URL=(-C "$WORK/cli.yml" -u "$SVM_RPC")
DEST=$(solana-keygen pubkey "$WORK/dest.json")
SIG=$(solana "${URL[@]}" -k "$WORK/payer.json" transfer "$DEST" 1.5 \
    --allow-unfunded-recipient --output json | json '["signature"]')
[ "$(solana "${URL[@]}" confirm "$SIG")" = "Finalized" ] || die "the transfer did not finalize"
stop_pidfile "$WORK/node.pid"

if ! "$WORK/svmd" rollback --home "$WORK/home" >"$WORK/rollback.log" 2>&1; then
    cat "$WORK/rollback.log" >&2
    die "rollback failed"
fi
if grep -q 'panic' "$WORK/rollback.log"; then
    cat "$WORK/rollback.log" >&2
    die "rollback panicked"
fi
N=$(grep -o 'Rolled back state to height [0-9]*' "$WORK/rollback.log" | grep -o '[0-9]*$') ||
    die "rollback did not name a height: $(cat "$WORK/rollback.log")"
echo "rollback: state and app back to height $N"

spawn node "$WORK/svmd" start --home "$WORK/home" --expected-genesis-hash "$GENESIS_HASH" --log_no_color
await_height node "$CMT" $((N + 3)) "past the block rollback took back" || { tail -40 "$WORK/node.log" >&2; exit 1; }
# CometBFT's handshake: the app and the state at N with block N+1 stored runs
# that block on the real app. An app still at N+1 would take the mock app.
replay=$(grep 'ABCI Replay Blocks' "$WORK/node.log" | tail -1)
for want in "appHeight=$N " "stateHeight=$N " "storeHeight=$((N + 1)) "; do
    [[ "$replay " == *"$want"* ]] || die "the restart did not find the app at $N: $replay"
done
grep -q 'Replay last block using real app' "$WORK/node.log" || die "the restart did not run block $((N + 1)) again"
echo "restart: app at $N and block $((N + 1)) in the store, so the block ran again"
[ "$(solana "${URL[@]}" balance "$DEST")" = "1.5 SOL" ] || die "the transfer is gone after the rollback"
echo "the transfer is still there"
