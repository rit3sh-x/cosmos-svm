#!/usr/bin/env bash
# Records agave.json from solana-test-validator v4.3.0.
#
# Needs: go, solana-test-validator 4.3.0, python3 and curl on PATH, and the
# engine staticlib at target/release/libsvm_engine.a.
#
# The validator runs this chain's gate set and program bytes:
#   - It deactivates every v4.3.0 feature that is not active at this chain's
#     pin. deactivate.py lists them.
#   - --account-dir loads this chain's genesis programs with their ProgramData
#     accounts. The test validator prefers them to its own SPL programs.
set -euo pipefail
cd "$(dirname "$0")/../../.."
# shellcheck source=scripts/lib.sh
. scripts/lib.sh
work=$(mktemp -d)
pid=
trap 'stop_pid "$pid"; rm -rf "$work"' EXIT

solana-test-validator --version | grep -q ' 4\.3\.0 ' || die "need solana-test-validator 4.3.0"
curl -fsSL https://raw.githubusercontent.com/anza-xyz/agave/v4.3.0/feature-set/src/lib.rs -o "$work/features.rs"
python3 rpc/testdata/transcript/deactivate.py "$work/features.rs" genesis/features.json > "$work/deactivate"
TRANSCRIPT_ACCOUNTS="$work/accounts" go test ./rpc -count=1 -run TestTheChainAnswersTheAgaveTranscript

args=(--reset --quiet --ledger "$work/ledger" --account-dir "$work/accounts")
while read -r id; do args+=(--deactivate-feature "$id"); done < "$work/deactivate"
solana-test-validator "${args[@]}" > "$work/validator.log" 2>&1 &
pid=$!

# Wait for the RPC to answer, not for a fixed time. The deadline only catches
# a validator that never starts.
healthy() {
    curl -s -X POST -H 'content-type: application/json' \
        -d '{"jsonrpc":"2.0","id":1,"method":"getHealth"}' http://127.0.0.1:8899 | grep -q '"ok"'
}
if ! wait_for 180 "solana-test-validator to answer getHealth" healthy; then
    tail -40 "$work/validator.log" >&2
    exit 1
fi

command="solana-test-validator ${args[*]}"
command="${command//$work/<work>}"
go test ./rpc -count=1 -timeout 45m -run TestRecordAgaveTranscript \
    -record=http://127.0.0.1:8899 -record-command="$command"
echo "recorded rpc/testdata/transcript/agave.json"
