#!/usr/bin/env bash
# Checks that a devnet hands out money and production cannot. The unmodified
# solana CLI gets an airdrop from a devnet binary. A production binary has no
# faucet setting and refuses to start on the devnet chain.
#
#   make accept-devnet
#
# Needs solana on PATH.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
SVM_RPC=http://127.0.0.1:8899
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
stop() {
    stop_pidfile "$WORK/node.pid"
    rm -rf "$WORK"
}
trap stop EXIT

build svmd-devnet svmd nodetool

# The key is the account, so the genesis row comes from the keypair, not from
# a second setting.
keygen "$WORK/faucet_key.json" "$WORK/reserve.json" "$WORK/dest.json"
HOT=$(solana-keygen pubkey "$WORK/faucet_key.json")
COLD=$(solana-keygen pubkey "$WORK/reserve.json")
DEST=$(solana-keygen pubkey "$WORK/dest.json")

"$WORK/svmd-devnet" init local --chain-id svm-devnet --home "$WORK/home" > "$WORK/init.log" 2>&1
# A hot faucet holds the funds for one refill period (1,000 SOL), and a cold
# reserve holds the rest. Both are ordinary accounts[] rows.
"$WORK/nodetool" fund -genesis "$WORK/home/config/genesis.json" -lamports 1000000000000 "$HOT"
"$WORK/nodetool" fund -genesis "$WORK/home/config/genesis.json" -lamports 9999000000000000 "$COLD"
"$WORK/nodetool" set "$WORK/home/config/app.toml" index-max-bytes=1073741824 \
    faucet-key="\"$WORK/faucet_key.json\""
if grep -q "faucet-key" <("$WORK/svmd" init other --chain-id svm-x --home "$WORK/prod" >/dev/null 2>&1; cat "$WORK/prod/config/app.toml"); then
    die "a production binary's app.toml offers a faucet-key setting"
fi
echo "app.toml: the faucet settings exist on the devnet build only"

# svmd refuses to start with a configured key that it cannot use. The error
# names the path and the reason.
chmod 644 "$WORK/faucet_key.json"
# A start that does not refuse runs until the timeout and fails here.
if timeout 60 "$WORK/svmd-devnet" start --home "$WORK/home" > "$WORK/perm.log" 2>&1 ||
    ! grep -q "chmod 600" "$WORK/perm.log" || ! grep -q "$WORK/faucet_key.json" "$WORK/perm.log"; then
    die "start accepted a group-readable faucet key"
fi
echo "faucet-key: a group-readable key refuses to start by name"
chmod 600 "$WORK/faucet_key.json"

spawn node "$WORK/svmd-devnet" start --home "$WORK/home"
wait_for_chain
URL=(-u "$SVM_RPC")

BEFORE=$(solana "${URL[@]}" balance "$DEST" | cut -d' ' -f1)
SIG=$(solana "${URL[@]}" airdrop 10 "$DEST" --output json | python3 checks.py signature)
[ "$(solana "${URL[@]}" confirm "$SIG")" = "Finalized" ]
AFTER=$(solana "${URL[@]}" balance "$DEST" | cut -d' ' -f1)
awk -v before="$BEFORE" -v after="$AFTER" 'BEGIN { exit !(after - before == 10) }'
echo "solana airdrop 10: $SIG Finalized, $DEST rose from $BEFORE to $AFTER SOL"

# The transfer came from the account of the key, and the chain minted nothing.
# The hot faucet paid for it.
solana "${URL[@]}" confirm -v "$SIG" | grep -q "$HOT"
echo "the airdrop is a transfer from $HOT, which is the key's own pubkey"

# Check the caps directly, and the faucet gauges on the registry that the
# CometBFT metrics server serves.
python3 checks.py airdrop-caps "$SVM_RPC" "$DEST" "$WORK/node.log"
python3 checks.py faucet-metrics http://127.0.0.1:26660/metrics

# When the devnet binary releases the data directory, start the production
# binary on the same chain. It refuses, because the chain is a devnet chain.
stop_pidfile "$WORK/node.pid"
rm -f "$WORK/node.pid"
if timeout 60 "$WORK/svmd" start --home "$WORK/home" > "$WORK/prodstart.log" 2>&1 ||
    ! grep -q "this is a production binary, the chain is a devnet chain" "$WORK/prodstart.log"; then
    die "a production binary started against a devnet chain"
fi
echo "a production binary refuses this chain by name"
echo PASS
