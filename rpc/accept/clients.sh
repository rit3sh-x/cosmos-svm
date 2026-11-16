#!/usr/bin/env bash
# Runs the nightly client suites against the local single-validator chain,
# with each client at a pinned version. Every case asserts inclusion, a
# rendered result or the specific refusal, and waits on its condition.
#
#   make accept-clients
#
# Needs the Solana CLI 4.3.0, spl-token-cli 5.6.1, Node with pnpm and
# Playwright's Chromium on PATH. The flake provides them. scripts/prebuild.sh
# builds the Explorer once.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
export SVM_RPC=http://127.0.0.1:8899
export SVM_WS=ws://127.0.0.1:8900
RESULTS=${SVM_CLIENTS_RESULTS:-$ROOT/rpc/accept/results/clients-$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "$RESULTS"
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
stop() {
    stop_pidfile "$WORK/node.pid"
    stop_group "$WORK/explorer.pid"
    rm -rf "$WORK"
}
trap stop EXIT

[ "$(solana --version | cut -d' ' -f2)" = 4.3.0 ] || die "the CLI suite pins solana 4.3.0"
[ "$(spl-token --version | cut -d' ' -f2)" = 5.6.1 ] || die "the CLI suite pins spl-token 5.6.1"

# shellcheck disable=SC2119 # no extra start flags
start_local_chain
URL=(-C "$WORK/cli.yml" -u "$SVM_RPC")
PAYER=$(solana-keygen pubkey "$WORK/payer.json")

# The token that the other suites read: a mint, the payer's account with
# 1,000, and a delegate approved on that account.
MINT=$(spl-token "${URL[@]}" create-token --decimals 6 --output json | json '["commandOutput"]["address"]')
ACCOUNT=$(spl-token "${URL[@]}" create-account "$MINT" --output json | json '["commandOutput"]["address"]' 2>/dev/null ||
    spl-token "${URL[@]}" address --token "$MINT" --verbose --output json | json '["associatedTokenAddress"]')
spl-token "${URL[@]}" mint "$MINT" 1000 >/dev/null
keygen "$WORK/delegate.json"
DELEGATE=$(solana-keygen pubkey "$WORK/delegate.json")
spl-token "${URL[@]}" approve "$ACCOUNT" 10 "$DELEGATE" >/dev/null
[ "$(spl-token "${URL[@]}" supply "$MINT")" = 1000 ]
echo "cli: spl-token supply 1000 for $MINT"
# The default deploy path writes buffers over QUIC.
keygen "$WORK/program.json"
timeout 600 solana "${URL[@]}" program deploy --program-id "$WORK/program.json" "$ROOT/programs/spl_token.so" >/dev/null
solana "${URL[@]}" program show "$(solana-keygen pubkey "$WORK/program.json")" | grep -q Authority
echo "cli: program deploy over QUIC"
solana "${URL[@]}" feature status >/dev/null
solana "${URL[@]}" validators >/dev/null
echo "cli: feature status and validators exit 0"
# Before the first epoch boundary there is no reward block. The CLI says the
# same against solana-test-validator.
if solana "${URL[@]}" inflation rewards "$PAYER" > "$WORK/rewards.txt" 2>&1 || ! grep -q "Rewards not available" "$WORK/rewards.txt"; then
    cat "$WORK/rewards.txt" >&2
    echo "inflation rewards did not refuse before the first boundary" >&2
    exit 1
fi
solana "${URL[@]}" supply | grep -q "Total:"
solana "${URL[@]}" largest-accounts | grep -q "$PAYER"
SLOT=$(solana "${URL[@]}" slot)
solana "${URL[@]}" block-time "$SLOT" | grep -q "^Date:"
solana "${URL[@]}" transaction-count | grep -qE '^[0-9]+$'
echo "cli: inflation rewards refuses before the boundary; supply, largest-accounts, block-time and transaction-count answer"

cargo run --quiet --release --locked --manifest-path "$ROOT/tools/rpc-client-suite/Cargo.toml" -- \
    "$SVM_RPC" "$WORK/payer.json" "$MINT" "$ACCOUNT" "$PAYER" "$DELEGATE" | tee "$RESULTS/rust.txt"

(cd clients && pnpm install --frozen-lockfile --silent)
export SVM_PAYER=$WORK/payer.json SVM_MINT=$MINT
for suite in js anchor wallets; do
    node "clients/$suite.mjs" | tee "$RESULTS/$suite.txt"
done

EXPLORER=$(third_party explorer)/explorer
# It runs in its own session, so stop_group also stops the server that pnpm starts.
spawn explorer env -C "$EXPLORER" pnpm start -p 3000
serves() { curl -sf -o /dev/null http://localhost:3000; }
if ! wait_for 120 "the Explorer to serve" serves; then
    tail -20 "$WORK/explorer.log" >&2
    exit 1
fi
SIG=$(solana "${URL[@]}" transfer --allow-unfunded-recipient "$(solana-keygen pubkey "$WORK/dest.json")" 0.5 --output json | json '["signature"]')
SLOT=$(solana "${URL[@]}" confirm -v "$SIG" --output json | json '["slot"]')
mkdir -p "$RESULTS/explorer"
SVM_SIGNATURE=$SIG SVM_SLOT=$SLOT SVM_PAYER_ADDRESS=$PAYER SVM_EXPLORER_OUT=$RESULTS/explorer \
    node clients/explorer.mjs pages | tee "$RESULTS/explorer.txt"
# Restart svmd with an origin list that omits the Explorer's origin. svmd
# refuses the Explorer's calls, and the Explorer shows that the cluster failed.
stop_pidfile "$WORK/node.pid"
spawn node "$WORK/svmd" start --home "$WORK/home" --expected-genesis-hash "$GENESIS_HASH" \
    --svm.rpc-cors-origins http://localhost:4000
wait_for_chain
SVM_SIGNATURE=$SIG SVM_EXPLORER_OUT=$RESULTS/explorer node clients/explorer.mjs refused | tee -a "$RESULTS/explorer.txt"

echo "results in $RESULTS"
echo PASS
