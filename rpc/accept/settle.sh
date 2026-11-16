#!/usr/bin/env bash
# The settlement acceptance run. Each part runs on Agave and on this chain.
# gate.py judges this chain against Agave, with settle/known.json as the list
# of known differences.
#
#   make accept-settle             every part
#   rpc/accept/settle.sh PART...   hyperlane: Hyperlane's own sealevel e2e. Agave's side is
#                                             upstream's run on its solana-test-validator 3.0.14
#                                  proofs:    SP1's fixture proof and RISC Zero's fixture
#                                             receipt, directly and through the router
#                                  squads:    a 2-of-3 Squads v4 vault upgrades a program
#
# On Agave, proofs and squads run on solana-test-validator 4.3.0. On the svmd
# side, the shim (shim/solana-test-validator) is first on PATH, and
# SOLANA_TEST_VALIDATOR_BIN names it for Hyperlane's e2e.
#
# Needs the tools of flake.nix on PATH, Docker for the e2e's Postgres, and
# network access to GitHub and mainnet. scripts/prebuild.sh prepares the
# verifiers, Hyperlane's monorepo and its registry once, into
# target/third-party unless SVM_THIRD_PARTY says otherwise. The e2e's first
# build of Hyperlane's agents takes about an hour on four cores.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
HERE=$ROOT/rpc/accept
WORK=$(mktemp -d)
RESULTS=${SVM_SETTLE_RESULTS:-$ROOT/rpc/accept/results/settle-$(date -u +%Y%m%dT%H%M%SZ)}
export SVM_RPC=http://127.0.0.1:8899
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
# shellcheck source=scripts/pins.sh
. "$ROOT/scripts/pins.sh"

# The pins. Squads is pinned by its executable hash, the sha256 of its bytes
# without trailing zeros. Its AGPL source is Squads-Protocol/v4 at 64af733,
# built on solana-verifiable-build 1.18.16. scripts/pins.sh pins the sources of
# Hyperlane and the verifiers.
SQUADS=SQDS4ep65T869zMMBKyuUq6aD6EgTu8psMjkvj52pCf
SQUADS_SHA=d48660833989ecea3145ff726164fe640bd90696f03ce00dfd0cda258cbf2fac

PARTS=" ${*:-hyperlane proofs squads} "
has() { [[ $PARTS == *" $1 "* ]]; }
for part in $PARTS; do
    case $part in hyperlane | proofs | squads) ;; *) die "no part $part: hyperlane, proofs or squads" ;; esac
done
trap 'rm -rf "$WORK"' EXIT

# fetch gets what the parts need, the preload arguments of the base chains and
# the clients. The Squads bytes and config come from mainnet at each run.
fetch() {
    PRELOAD=()
    need solana solana-test-validator node pnpm cargo mold
    build svmd-devnet nodetool
    TP=$(third_party settle)
    (cd settle && pnpm install --frozen-lockfile --silent) >"$WORK/pnpm.log" 2>&1 || { tail -20 "$WORK/pnpm.log" >&2; die "pnpm install failed in rpc/accept/settle"; }
    if has squads; then
        dump_program "$SQUADS" "$SQUADS_SHA" "$WORK/squads.so"
        # The global config at ["multisig", "program_config"]. Only a key that
        # is hard-coded to the Squads team can create it. The copy keeps
        # mainnet's layout, with a fee of 0, and an authority and a treasury
        # that nobody can sign for.
        local nobody config
        nobody=$(solana find-program-derived-address 11111111111111111111111111111111 string:squads-program-config | head -1)
        config=$(solana find-program-derived-address "$SQUADS" string:multisig string:program_config | head -1)
        mainnet_account "$config" "$WORK/mainnet-config.json"
        python3 settle/settle.py squads-config "$WORK/mainnet-config.json" "$WORK/config.json" "$nobody"
        PRELOAD+=(--bpf-program "$SQUADS" "$WORK/squads.so" --account "$config" "$WORK/config.json")
    fi
}

# deploy KEYPAIR SO deploys a program with the payer as its upgrade authority.
deploy() {
    solana -C "$WORK/cli.yml" program deploy --use-rpc --program-id "$1" "$2" >"$WORK/deploy.log" 2>&1 ||
        { tail -5 "$WORK/deploy.log" >&2; die "deploy of $2 failed"; }
}

# chain starts the base chain with PRELOAD, and funds the payer.
chain() {
    keygen "$WORK/payer.json"
    cli_config "$WORK/cli.yml" "$WORK/payer.json"
    spawn base solana-test-validator --reset --quiet --ledger "$WORK/ledger" --rpc-port 8899 "${PRELOAD[@]}"
    wait_for_chain
    solana -C "$WORK/cli.yml" airdrop 1000 >/dev/null
}

proofs() {
    local b=$TP/r0 verifier router
    deploy "$TP/sp1/fibonacci_verifier_contract-keypair.json" "$TP/sp1/fibonacci_verifier_contract.so"
    node settle/run.js sp1 "$SVM_RPC" "$WORK/payer.json" "$(solana-keygen pubkey "$TP/sp1/fibonacci_verifier_contract-keypair.json")" \
        "$TP/sp1-solana/proofs/fibonacci_proof.bin" 3>"$1/sp1.jsonl" >"$1/sp1.log" 2>&1
    # The router accepts a verifier only when the router's PDA is the
    # verifier's upgrade authority.
    solana -C "$WORK/cli.yml" airdrop 100 "$(solana-keygen pubkey "$b/owner.json")" >/dev/null
    cli_config "$WORK/owner.yml" "$b/owner.json"
    verifier=$(solana-keygen pubkey "$b/verifier-keypair.json")
    router=$(solana-keygen pubkey "$b/router-keypair.json")
    for p in groth_16_verifier=verifier verifier_router=router; do
        solana -C "$WORK/owner.yml" program deploy --use-rpc --program-id "$b/${p#*=}-keypair.json" "$b/${p%%=*}.so" >"$WORK/deploy.log" 2>&1 ||
            { tail -5 "$WORK/deploy.log" >&2; die "deploy of ${p%%=*} failed"; }
    done
    solana -C "$WORK/owner.yml" program set-upgrade-authority "$verifier" --skip-new-upgrade-authority-signer-check \
        --new-upgrade-authority "$(solana find-program-derived-address "$router" string:router | head -1)" >/dev/null
    node settle/run.js r0 "$SVM_RPC" "$b/owner.json" "$verifier" "$router" \
        "$TP/risc0-solana/solana-verifier/programs/groth_16_verifier/src/v3_test_receipt.rs" 3>"$1/r0.jsonl" >"$1/r0.log" 2>&1
}

# squads makes a vault the upgrade authority of a program and of a buffer,
# as an operator would with the CLI, and upgrades the program through it.
squads() {
    local program vault buffer
    keygen "$WORK/program.json"
    deploy "$WORK/program.json" "$TP/sp1/fibonacci_verifier_contract.so"
    program=$(solana-keygen pubkey "$WORK/program.json")
    node settle/run.js squads-create "$SVM_RPC" "$WORK/payer.json" "$WORK/squads.json" 3>"$1/squads.jsonl" >"$1/squads.log" 2>&1
    vault=$(json '["vault"]' <"$WORK/squads.json")
    solana -C "$WORK/cli.yml" program set-upgrade-authority "$program" --new-upgrade-authority "$vault" \
        --skip-new-upgrade-authority-signer-check >/dev/null
    buffer=$(solana -C "$WORK/cli.yml" program write-buffer --use-rpc "$TP/sp1/fibonacci_verifier_contract.so" --output json | json '["buffer"]')
    solana -C "$WORK/cli.yml" program set-buffer-authority "$buffer" --new-buffer-authority "$vault" >/dev/null
    node settle/run.js squads-upgrade "$SVM_RPC" "$WORK/squads.json" "$program" "$buffer" 3>>"$1/squads.jsonl" >>"$1/squads.log" 2>&1
}

# hyperlane runs the e2e as Hyperlane's CI does (test-rust-e2e.yml, "agent
# tests (Sealevel)"), with E2E_CI_MODE and its 600 s budget. Its one result is
# whether it passed. The error is what the e2e said about the failure.
hyperlane() {
    local m=$TP/hyperlane-monorepo status=0
    git -C "$m" checkout -q -- rust/main/utils/run-locally
    if [ "$2" = svmd ]; then
        git -C "$m" apply "$HERE/settle/hl-e2e.patch"
        export SOLANA_TEST_VALIDATOR_BIN=$HERE/shim/solana-test-validator
    fi
    # By default the e2e builds its agents in the dev profile with full debug
    # info and links them with GNU ld. Then relayer, validator and scraper are
    # about 1.4 GB each, and two links at once use 11.6 GB. That is more than
    # the 8 GB of a hosted runner. Without debug info the agents run the same,
    # and a panic still names its file and line.
    #
    # mold, which Hyperlane's own CI uses, links the relayer in 5 s and 0.7 GB.
    # GNU ld takes 26 s and 3.9 GB. With both changes, a cold run on two cores
    # peaks at 3.1 GB.
    (cd "$m/rust/main" && CARGO_PROFILE_DEV_DEBUG=0 E2E_CI_MODE=true E2E_CI_TIMEOUT_SEC=600 timeout 7200 \
        mold -run cargo test --release --package run-locally --bin run-locally --features sealevel -- sealevel::test --nocapture) \
        >"$1/hyperlane.log" 2>&1 || status=$?
    unset SOLANA_TEST_VALIDATOR_BIN
    git -C "$m" checkout -q -- rust/main/utils/run-locally
    python3 settle/settle.py e2e-result "$1/hyperlane.log" "$status" >"$1/hyperlane.jsonl"
}

stop_all() { stop_group "$WORK/base.pid" 30; }

# side NAME runs every part on one base chain, agave or svmd.
side() {
    local out=$RESULTS/$1
    mkdir -p "$out"
    trap stop_all EXIT
    if [ "$1" = svmd ]; then
        export PATH=$HERE/shim:$PATH SHIM_BIN=$WORK SHIM_WORK=$WORK/shim
        mkdir -p "$SHIM_WORK"
    fi
    if has proofs || has squads; then
        chain
        if has proofs; then proofs "$out"; fi
        if has squads; then squads "$out"; fi
        stop_all
        [ ! -f "$WORK/base.log" ] || mv "$WORK/base.log" "$out/base.log"
    fi
    if has hyperlane; then hyperlane "$out" "$1"; fi
}

fetch
echo "fetched; results in $RESULTS"
(side agave) || die "the Agave side did not run, so the run is broken"
(side svmd) || echo "the svmd side stopped early; the gate judges what ran" >&2
required=()
has proofs && required+=(--require sp1 --require r0)
has squads && required+=(--require squads)
has hyperlane && required+=(--require hyperlane)
python3 gate.py settle/known.json "$RESULTS/agave" "$RESULTS/svmd" "${required[@]}" | tee "$RESULTS/gate.txt"
[ "$(tail -1 "$RESULTS/gate.txt")" = PASS ]
