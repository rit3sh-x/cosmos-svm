#!/usr/bin/env bash
# Enforce dependency boundaries for the production engine.
#
# The root Cargo.lock is part of the engine's consensus configuration.
# Dependencies that pull in validator infrastructure or tooling must remain
# isolated in their respective workspaces.

set -euo pipefail

cd "$(dirname "$0")/.."

readonly LOCKFILE=Cargo.lock

fail() {
    echo "check-engine-deps: $*" >&2
    exit 1
}

# Fail if a package matching the supplied regex appears in the root lockfile.
refuse() {
    local description=$1
    local pattern=$2
    local found

    found=$(grep -nE "^name = \"(${pattern})\"$" "$LOCKFILE" || true)

    if [[ -n "$found" ]]; then
        fail "$description reached the engine's $LOCKFILE:"$'\n'"$found"
    fi
}

# Ensure the lockfile exists before checking dependency boundaries.
[[ -f "$LOCKFILE" ]] || fail "missing $LOCKFILE"

# The engine links the standalone SVM, not validator infrastructure.
refuse "validator infrastructure" \
    'solana-(runtime|accounts-db|ledger|core|gossip|poh|streamer|rpc)'

# Keep tooling dependencies in their dedicated workspaces.
refuse "account decoder (tools/parsed-oracle only)" \
    'solana-account-decoder(-client-types)?'

refuse "QUIC (tpu/ only)" \
    'quinn(-proto)?'

refuse "transaction renderer (tools/tx-oracle only)" \
    'solana-transaction-status(-client-types)?'

# Capture both feature graphs before checking them.
# -e features includes dev-dependencies.
# -e features,no-dev excludes dev-dependencies.
test_graph=$(cargo tree --locked -e features -i solana-svm) ||
    fail "could not resolve the solana-svm test dependency graph"

prod_graph=$(cargo tree --locked -e features,no-dev -i solana-svm) ||
    fail "could not resolve the solana-svm production dependency graph"

# The conformance tests require this development-only SVM feature.
if ! grep -qF 'dev-context-only-utils' <<<"$test_graph"; then
    fail "solana-svm/dev-context-only-utils is missing from the test graph"
fi

# The development-only feature must never reach the production graph.
if grep -qF 'dev-context-only-utils' <<<"$prod_graph"; then
    fail "solana-svm/dev-context-only-utils reached the production graph"
fi

printf '%s\n' 'check-engine-deps: ok'
