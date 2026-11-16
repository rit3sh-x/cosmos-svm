#!/usr/bin/env bash
# Tests the stock Solana CLI, spl-token CLI and @solana/web3.js against a
# local chain: transfers, transaction history, subscriptions, program
# deployment, SPL Token and Token-2022.
#
#   make accept-rpc
#
# Needs solana and spl-token on PATH, and Node with pnpm. The harness installs
# the pinned @solana/web3.js and ws of rpc/accept/clients from the lockfile.
# The flake provides these tools.
#
# To test an existing chain, set SVM_ACCEPT_URL. Give its funded payer in
# SVM_ACCEPT_PAYER and its genesis file in SVM_ACCEPT_GENESIS. SVM_ACCEPT_WS
# and SVM_ACCEPT_METRICS set its websocket and metrics URLs.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
export SVM_RPC=${SVM_ACCEPT_URL:-http://127.0.0.1:8899}
export SVM_WS=${SVM_ACCEPT_WS:-ws://127.0.0.1:8900}
METRICS=${SVM_ACCEPT_METRICS:-http://127.0.0.1:26660/metrics}
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
stop() {
    stop_pidfile "$WORK/node.pid"
    rm -rf "$WORK"
}
trap stop EXIT

if [ -n "${SVM_ACCEPT_URL:-}" ]; then
    cp "$SVM_ACCEPT_PAYER" "$WORK/payer.json"
    keygen "$WORK/dest.json"
    cli_config "$WORK/cli.yml" "$WORK/payer.json"
    GENESIS_HASH=$(python3 genesis_hash.py "$SVM_ACCEPT_GENESIS")
    echo "against a chain this harness did not start: $SVM_RPC, genesis $GENESIS_HASH"
    wait_for_chain
else
    # shellcheck disable=SC2119 # no extra start flags
    start_local_chain
fi

(cd clients && pnpm install --frozen-lockfile --silent)
export NODE_PATH=$ROOT/rpc/accept/clients/node_modules
URL=(-C "$WORK/cli.yml" -u "$SVM_RPC")
DEST=$(solana-keygen pubkey "$WORK/dest.json")
SIG=$(solana "${URL[@]}" -k "$WORK/payer.json" transfer "$DEST" 1.5 \
    --allow-unfunded-recipient --output json | json '["signature"]')
[ "$(solana "${URL[@]}" confirm "$SIG")" = "Finalized" ]
[ "$(solana "${URL[@]}" balance "$DEST")" = "1.5 SOL" ]
echo "solana transfer: $SIG Finalized"
solana "${URL[@]}" confirm -v "$SIG" | grep -q "Program 11111111111111111111111111111111 success"
echo "solana confirm -v: logs present"

# The CLI creates a lookup table at the finalized Clock's slot and sends it
# with preflight. Preflight runs as the next block, whose SlotHashes holds
# that slot.
ALT=$(solana "${URL[@]}" -k "$WORK/payer.json" address-lookup-table create --output json | json '["lookupTableAddress"]')
solana "${URL[@]}" address-lookup-table get "$ALT" | grep -q "Lookup Table Address: $ALT"
echo "solana address-lookup-table create: $ALT"

# The address index serves getSignaturesForAddress for the recipient, which
# the transfer names and which signs nothing.
solana "${URL[@]}" transaction-history "$DEST" | grep -q "$SIG"
echo "solana transaction-history: $DEST names $SIG"

# getBlock and getFirstAvailableBlock. The CLI asks for base64 and
# maxSupportedTransactionVersion 0, and prints the block's transactions, so
# the assertion is that the CLI decodes the response.
SLOT=$(solana "${URL[@]}" confirm -v "$SIG" --output json | json '["slot"]')
solana "${URL[@]}" block "$SLOT" | grep -q "$SIG"
FIRST=$(solana "${URL[@]}" first-available-block)
[ "$FIRST" -ge 1 ]
echo "solana block $SLOT: carries $SIG, first available block $FIRST"

for commitment in processed confirmed finalized; do
    timeout 120 node send.js "$commitment" "$WORK/payer.json"
done

# Validate WebSocket subscriptions, limits, ping handling and idle connections.
timeout 180 node subscribe.js "$WORK/payer.json"
timeout 180 node limits.js

# The HTTP body limit is Agave's 50 KiB, so a 60 kB body gets 413. A batch of
# notifications alone gets no answer.
printf '{"jsonrpc":"2.0","id":1,"method":"getSlot","params":["%s"]}\n' "$(head -c 60000 /dev/zero | tr '\0' x)" >"$WORK/big.json"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'content-type: application/json' \
    --data-binary "@$WORK/big.json" "$SVM_RPC")
[ "$CODE" = "413" ]
EMPTY=$(curl -sf -X POST -H 'content-type: application/json' \
    -d '[{"jsonrpc":"2.0","method":"getSlot"},{"jsonrpc":"2.0","method":"ping"}]' "$SVM_RPC")
[ -z "$EMPTY" ]
echo "http: a 60 kB body is 413, a batch of notifications is empty"

python3 checks.py fees "$SVM_RPC"

# Check that application and consensus metrics are exposed.
curl -sf "$METRICS" > "$WORK/metrics.txt"
for metric in svm_load_list_entries_total svm_push_accounts_seconds_count \
    svm_admission_rejected_total svm_history_index_bytes svm_dedup_entries \
    svm_block_units svm_program_cache_hits_total svm_prewarm_misses_total \
    svm_conflict_groups svm_block_program_text_bytes cometbft_consensus_height; do
    grep -q "^$metric" "$WORK/metrics.txt"
done
echo "metrics: $(grep -c '^svm_' "$WORK/metrics.txt") svm_ series beside cometbft's own"

[ "$(solana "${URL[@]}" cluster-version)" = "4.3.0" ]
GENESIS=$(solana "${URL[@]}" genesis-hash)
[ "$GENESIS" = "$GENESIS_HASH" ]
HEALTH=$(curl -sf -X POST -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' "$SVM_RPC" | json '["result"]')
[ "$HEALTH" = "ok" ]
echo "getHealth: ok, cluster-version 4.3.0, genesis-hash $GENESIS"

# Deploy unmodified programs through RPC, including a 10 MB ELF.
cp "$ROOT/programs/token-2022.so" "$WORK/ten-mb.so"
truncate -s 10000000 "$WORK/ten-mb.so"
for program in "$ROOT/programs/spl_token.so" "$ROOT/programs/serum-v3.so" \
    "$ROOT/programs/token-2022.so" "$WORK/ten-mb.so"; do
    keygen "$WORK/program.json"
    timeout 600 solana "${URL[@]}" -k "$WORK/payer.json" program deploy --use-rpc \
        --program-id "$WORK/program.json" "$program" >/dev/null
    solana "${URL[@]}" program show "$(solana-keygen pubkey "$WORK/program.json")" | grep -q "Authority"
    echo "deployed $(basename "$program")"
done

# Also test the CLI's default QUIC deploy path if the chain advertises a TPU endpoint.
TPU=$(curl -sf -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"getClusterNodes"}' "$SVM_RPC" | json '["result"][0]["tpuQuic"] or ""')
if [ -n "$TPU" ]; then
    keygen "$WORK/program.json"
    timeout 600 solana "${URL[@]}" -k "$WORK/payer.json" program deploy \
        --program-id "$WORK/program.json" "$ROOT/programs/token-2022.so" >/dev/null
    solana "${URL[@]}" program show "$(solana-keygen pubkey "$WORK/program.json")" | grep -q "Authority"
    echo "deployed token-2022.so without --use-rpc, over QUIC to $TPU"
elif [ -z "${SVM_ACCEPT_URL:-}" ]; then
    echo "this harness's own machine advertises no tpuQuic" >&2
    exit 1
fi

# Exercise the seeded SPL Token and Associated Token Account programs.
SPL=(spl-token -C "$WORK/cli.yml")
MINT=$("${SPL[@]}" create-token --output json | json '["commandOutput"]["address"]')
"${SPL[@]}" create-account "$MINT" >/dev/null
"${SPL[@]}" mint "$MINT" 1000 >/dev/null
"${SPL[@]}" transfer "$MINT" 250 "$DEST" --fund-recipient --allow-unfunded-recipient >/dev/null
ATA=$("${SPL[@]}" address --token "$MINT" --verbose --output json | json '["associatedTokenAddress"]')
AMOUNT=$(solana "${URL[@]}" account "$ATA" --output json | python3 checks.py token-amount)
[ "$AMOUNT" = "750000000000" ]
echo "spl-token: create-token, create-account, mint, transfer on $MINT"

[ "$("${SPL[@]}" balance "$MINT")" = "750" ]
"${SPL[@]}" accounts --output json | python3 checks.py held "$MINT"
PAYER=$(solana-keygen pubkey "$WORK/payer.json")
python3 checks.py tokens "$SVM_RPC" "$PAYER" "$MINT" "$ATA"

# parsed.js uses the client that throws on a base64 body inside a parsed
# array. The client validates the envelope, not the fields. A response
# with the wrong envelope fails here.
timeout 60 node parsed.js "$PAYER" "$MINT" "$ATA"

# Exercise getProgramAccounts with owner, dataSize, memcmp and dataSlice filters.
python3 checks.py program-accounts "$SVM_RPC" "$MINT"

TOKEN22=TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb
SPL_VERSION=$(spl-token --version | awk '{print $2}')
if [ "$(printf '%s\n5.6.1\n' "$SPL_VERSION" | sort -V | head -1)" != "5.6.1" ]; then
    echo "spl-token $SPL_VERSION is too old for confidential transfers; need 5.6.1 or later" >&2
    exit 1
fi

cli_config "$WORK/dest.yml" "$WORK/dest.json"
TO=(spl-token -C "$WORK/dest.yml")
CMINT=$("${SPL[@]}" create-token --program-id "$TOKEN22" --enable-confidential-transfers auto \
    --output json | json '["commandOutput"]["address"]')
"${SPL[@]}" create-account "$CMINT" >/dev/null
"${SPL[@]}" configure-confidential-transfer-account "$CMINT" >/dev/null
"${SPL[@]}" mint "$CMINT" 100 >/dev/null
"${SPL[@]}" deposit-confidential-tokens "$CMINT" 100 >/dev/null
"${SPL[@]}" apply-pending-balance "$CMINT" >/dev/null
"${TO[@]}" create-account "$CMINT" >/dev/null
"${TO[@]}" configure-confidential-transfer-account "$CMINT" >/dev/null
"${SPL[@]}" transfer "$CMINT" 40 "$DEST" --confidential >/dev/null
"${TO[@]}" apply-pending-balance "$CMINT" >/dev/null
"${TO[@]}" withdraw-confidential-tokens "$CMINT" 40 >/dev/null
# The withdrawn amount is back in the non-confidential balance, bytes 64 to 72
# of the token account.
DEST_ATA=$("${TO[@]}" address --token "$CMINT" --verbose --output json | json '["associatedTokenAddress"]')
WITHDRAWN=$(solana "${URL[@]}" account "$DEST_ATA" --output json | python3 checks.py token-amount)
[ "$WITHDRAWN" = "40000000000" ]
echo "spl-token confidential: configure, deposit, apply, transfer, withdraw on $CMINT"

echo PASS
