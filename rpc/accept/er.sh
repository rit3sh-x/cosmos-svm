#!/usr/bin/env bash
# The ephemeral rollup acceptance run. MagicBlock's apps and suites run on
# solana-test-validator 4.3.0 and on this chain with the same pins. gate.py
# judges this chain against Agave. A test that passes on Agave must pass on
# this chain, unless er/known.json lists it.
#
#   make accept-er              every part
#   rpc/accept/er.sh PART...    apps:   counter, session-keys and roll-dice, with VRF
#                               pyth:   the Pyth replay, both id sets
#                               suites: MagicBlock's committor and table-mania suites
#
# On the svmd side, the shim (shim/solana-test-validator) is first on PATH, so
# each harness starts its base chain as it does on Agave. Needs the tools of
# flake.nix on PATH, and network access. Each run fetches the program bytes and
# the binaries under the Business Source License and stores no copy. The Pyth
# replay reads mainnet. scripts/prebuild.sh builds MagicBlock's apps and the
# programs of its suites once, into target/third-party unless SVM_THIRD_PARTY
# says otherwise.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
HERE=$ROOT/rpc/accept
WORK=$(mktemp -d)
RESULTS=${SVM_ER_RESULTS:-$ROOT/rpc/accept/results/er-$(date -u +%Y%m%dT%H%M%SZ)}
export SVM_RPC=http://127.0.0.1:8899
ER_RPC=http://127.0.0.1:7799
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
# shellcheck source=scripts/pins.sh
. "$ROOT/scripts/pins.sh"

# The pins. Each mainnet program is pinned by its executable hash, the sha256
# of its bytes without trailing zeros, as solana-verify reports it.
# scripts/pins.sh pins the sources, the validator and the oracle.
PROGRAMS=(
    "DELeGGvXpWV2fqJUhqcF5ZSYMS4JTLjteaAMARRSaeSh 9f33fea9576e6e13d08f1e68025a350a853f3b969b1ce8ca88b9d6430f728b2e" # delegation, deployed at slot 464,850,612
    "ComtrB2KEaWgXsW1dhr1xYL4Ht4Bjj3gXnnL6KMdABq e0a0a7d46e6830bd58d70faa568192a2f45899a4eea2924fcc051bb19379a2e2" # committor
    "noopb9bkMVfRPU8AsbpTUg8AQkHtKwMYZiFUjNRtMmV 1bec855d4c66b38f2852d73944bd9b459759745f3864d040927c471444f34818" # SPL Noop
    "KeyspM2ssCJbqUhQ4k7sveSiY4WjnYsrXkC8oDbwde5 c4e355256531f500a6e2a9ffa268d074bf9a45684fc431d5dc908ae52f620779" # session keys
    "Vrf1RNUjXmQGjmQrQLvJHs9SNkvDJEsRVFPkfSQUwGz 3a88342176ae6a5a33df77a3ed1cee0f83a519440892e92865706ba1e25370d3" # VRF, deployed at slot 457,592,029
)
PYTH_PROGRAMS=(
    "rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ c4f7e05b1d9264719be62bedbb53aa4cdb4563e94134c6199df4a732462dc1c7" # receiver
    "pythWSnswVUd12oZpeFP8e9CVaEqJg25g1Vtc2biRsT 1930dbc2dbea6471c1adb10df33b5f5dc3ffaca5c16f611574b74976941a7869" # push oracle
    "HDwcJBJXjL9FpJ7UBsYBtaDjsBUhuLCUYoz3zr8SWWaQ 35dd1e5c4f06ef77baa6685ac5fc5c048e286a4b917c7b67be78ed62a994fde2" # Wormhole receiver
    "rec2HHDDnjLfj4kE7VyEtFA1HPGQLK33259532cRyHp f602e35f66a4ef122218c37537e3255c5f8331c8a00cf4a5b724b6ae2a52bdc2" # pro-compatible receiver
    "pyt2F414BA6dPttK6RddPZUdHfapoBN24GL5wbrPCou 44bc0785f4fb1e143e01a7b3d86af5696b894de37031291b1b5bb930bc76d77f" # pro-compatible push oracle
    "HDw2E7P8X1SkCyjvoGsfBGAVUutKcj874bXjHrpVYrVL 14517936b6def82d7cd726520e4b9ca2c0f3a51bbb24ef45de9becebd31b292f" # pro-compatible Wormhole receiver
)
# The local queues of the examples: the oracle paywJ..., index 0 on the base
# chain and index 1 delegated to the rollup (scripts/local-env.sh).
VRF_BASE_QUEUE=GKE6d7iv8kCBrsxr78W3xVdjGLLLJnxsGiuzrsZCGEvb
VRF_EPHEMERAL_QUEUE=Sc9MJUngNbQXSXGP3F67KvKwVnhaYn6kcioxXNVowYT
VRF_ORACLE=paywJiVATrVDLYLmowJqzG6MsaCt77L8WyTnBb2754t
declare -A APPS=([counter]=counter/anchor [session-keys]=session-keys/anchor [roll-dice]=roll-dice/anchor)
# Each app's own test:local files.
declare -A TESTS=([counter]=tests/public-counter.ts [session-keys]=tests/anchor-counter-session.ts [roll-dice]="tests/roll-dice.ts tests/roll-dice-delegated.ts")

PARTS=" ${*:-apps pyth suites} "
has() { [[ $PARTS == *" $1 "* ]]; }
for part in $PARTS; do
    case $part in apps | pyth | suites) ;; *) die "no part $part: apps, pyth or suites" ;; esac
done
trap 'rm -rf "$WORK"' EXIT

# fetch gets what the parts need, and sets PRELOAD, the solana-test-validator
# flags that both base chains start with.
fetch() {
    local row id sha address name so file
    PRELOAD=()
    mkdir -p "$WORK/so" "$WORK/bin"
    need solana solana-test-validator node pnpm corepack
    build svmd-devnet nodetool
    TP=$(third_party er)
    (cd er && pnpm install --frozen-lockfile --silent) >"$WORK/pnpm.log" 2>&1 || { tail -20 "$WORK/pnpm.log" >&2; die "pnpm install failed in rpc/accept/er"; }
    if has apps || has pyth; then
        fetch_release "$MB_VALIDATOR_BIN_URL" "$MB_VALIDATOR_BIN_SHA" "$WORK/bin/ephemeral-validator"
        for row in "${PROGRAMS[@]}"; do
            read -r id sha <<<"$row"
            dump_program "$id" "$sha" "$WORK/so/$id.so"
            PRELOAD+=(--bpf-program "$id" "$WORK/so/$id.so")
        done
        keygen "$WORK/identity.json"
        IDENTITY=$(solana-keygen pubkey "$WORK/identity.json")
        while read -r address file; do PRELOAD+=(--account "$address" "$file"); done < <(node er/run.js accounts "$IDENTITY" "$WORK/accounts")
    fi
    if has apps; then
        fetch_release "$VRF_ORACLE_URL" "$VRF_ORACLE_SHA" "$WORK/bin/vrf-oracle"
        for name in "${!APPS[@]}"; do
            for so in "$TP/examples/${APPS[$name]}"/target/deploy/*.so; do
                PRELOAD+=(--bpf-program "$(solana-keygen pubkey "${so%.so}-keypair.json")" "$so")
            done
        done
    fi
    if has pyth; then
        for row in "${PYTH_PROGRAMS[@]}"; do
            read -r id sha <<<"$row"
            dump_program "$id" "$sha" "$WORK/so/$id.so"
            PRELOAD+=(--bpf-program "$id" "$WORK/so/$id.so")
        done
        for name in default pro; do
            node er/pyth.js rebuild "$name" er/pyth-updates.json "$WORK/pyth-$name.json"
            for address in $(node er/pyth.js accounts "$name" "$WORK/pyth-$name.json"); do
                mainnet_account "$address" "$WORK/$address.json"
                PRELOAD+=(--account "$address" "$WORK/$address.json")
            done
        done
    fi
    if has suites; then
        # Each run patches the suites' tests: the chain's rent in place of
        # Rent::default(), and write and close in one transaction after a
        # fresh slot.
        git -C "$TP/magicblock-validator" checkout -q -- test-integration/test-committor-service/tests
        git -C "$TP/magicblock-validator" apply "$HERE/er/mb-tests.patch"
    fi
}

stop_all() {
    local name
    for name in vrf-er vrf er base; do stop_group "$WORK/$name.pid" 30; done
}

er_answers() { solana -u "$ER_RPC" slot >/dev/null 2>&1; }

# chains starts the base chain with PRELOAD, and the ephemeral validator as the
# validator IDENTITY whose fees vault genesis holds. It funds the payer, the
# validator and the VRF oracle.
chains() {
    local key
    spawn base solana-test-validator --reset --quiet --ledger "$WORK/ledger" --rpc-port 8899 "${PRELOAD[@]}"
    wait_for_chain
    for key in "$(solana-keygen pubkey "$WORK/payer.json")" "$IDENTITY" "$VRF_ORACLE"; do
        solana -u "$SVM_RPC" airdrop 1000 "$key" >/dev/null
    done
    printf 'lifecycle = "ephemeral"\nremotes = ["%s", "ws://127.0.0.1:8900"]\nstorage = "%s"\n[aperture]\nlisten = "127.0.0.1:7799"\n' \
        "$SVM_RPC" "$WORK/er-ledger" >"$WORK/er.toml"
    spawn er env RUST_LOG=info MBV_VALIDATOR__KEYPAIR="$(node er/run.js secret "$WORK/identity.json")" \
        "$WORK/bin/ephemeral-validator" --no-tui "$WORK/er.toml"
    wait_for 120 "the ephemeral validator to answer" er_answers || { tail -40 "$WORK/er.log" >&2; die "the ephemeral validator did not start"; }
}

# app NAME OUT runs the app's own tests. Mocha's json reporter writes
# OUT/NAME.mocha.json. The gate judges a failing test.
app() {
    # shellcheck disable=SC2086 # TESTS holds a list
    (cd "$TP/examples/${APPS[$1]}" &&
        ANCHOR_WALLET="$WORK/payer.json" ANCHOR_PROVIDER_URL="$SVM_RPC" VALIDATOR="$IDENTITY" \
            PROVIDER_ENDPOINT="$SVM_RPC" WS_ENDPOINT=ws://127.0.0.1:8900 \
            EPHEMERAL_PROVIDER_ENDPOINT="$ER_RPC" EPHEMERAL_WS_ENDPOINT=ws://127.0.0.1:7800 \
            ROUTER_ENDPOINT="$ER_RPC" ROUTER_WS_ENDPOINT=ws://127.0.0.1:7800 \
            VRF_BASE_QUEUE=$VRF_BASE_QUEUE VRF_EPHEMERAL_QUEUE=$VRF_EPHEMERAL_QUEUE \
            timeout 1800 npx ts-mocha -p ./tsconfig.json -t 1000000 --exit --reporter json \
            --reporter-option "output=$2/$1.mocha.json" ${TESTS[$1]}) >"$2/$1.log" 2>&1 || true
    [ -s "$2/$1.mocha.json" ] || die "the $1 app's tests wrote no results: $2/$1.log"
}

# suite NAME TARGET OUT runs one of MagicBlock's suites with their runner,
# which starts its own base chain and validators. On both chains the suites
# load their own builds of the delegation program and the committor. Their
# delegations name no validator, and the compiled-in default of their build is
# their own validator tEsT3eV6..., not mainnet's MAS1Dt9q.... This chain's
# genesis for them admits no validator and seeds neither program.
suite() {
    spawn "suite-$1" env SHIM_EPHEMERAL_VALIDATORS= SHIM_VRF_ORACLES= make -C "$TP/magicblock-validator/test-integration" "$2"
    wait_for 14400 "MagicBlock's $1 suite" not group_alive "$(cat "$WORK/suite-$1.pid")" || true
    stop_group "$WORK/suite-$1.pid" 30
    mv "$WORK/suite-$1.log" "$3/$1.cargo.log"
}

# side NAME runs every part on one base chain, agave or svmd.
side() {
    local out=$RESULTS/$1 name app_id
    mkdir -p "$out"
    trap stop_all EXIT
    if [ "$1" = svmd ]; then
        # This chain's genesis admits the run's validator and VRF's oracle, so
        # it holds the delegation program, its companions, VRF, the
        # fees vaults and the oracle list. Agave preloads the same programs and
        # accounts.
        export PATH=$HERE/shim:$PATH SHIM_BIN=$WORK SHIM_WORK=$WORK/shim SHIM_EPHEMERAL_VALIDATORS=${IDENTITY:-}
        SHIM_VRF_ORACLES=$(node er/run.js oracle)
        export SHIM_VRF_ORACLES
        mkdir -p "$SHIM_WORK"
    fi
    echo "$1: $(solana-test-validator --version 2>/dev/null || echo "the shim, $(command -v solana-test-validator)")"
    keygen "$WORK/payer.json"
    if has apps || has pyth; then
        chains
        if has apps; then
            for name in counter session-keys; do app "$name" "$out"; done
            app_id=$(solana-keygen pubkey "$TP/examples/counter/anchor/target/deploy/public_counter-keypair.json")
            node er/run.js counter "$SVM_RPC" "$ER_RPC" "$app_id" 3>"$out/counter.jsonl" >>"$out/counter.log" 2>&1
            node er/run.js unlisted "$SVM_RPC" "$WORK/payer.json" 3>"$out/unlisted.jsonl" >"$out/unlisted.log" 2>&1
            node er/run.js queues "$SVM_RPC" 3>"$out/vrf.jsonl" >"$out/vrf-queues.log" 2>&1
            spawn vrf env RUST_LOG=info VRF_ORACLE_SKIP_PREFLIGHT=true RPC_URL="$SVM_RPC" WEBSOCKET_URL=ws://127.0.0.1:8900 "$WORK/bin/vrf-oracle"
            spawn vrf-er env RUST_LOG=info VRF_ORACLE_SKIP_PREFLIGHT=true RPC_URL="$ER_RPC" WEBSOCKET_URL=ws://127.0.0.1:7800 "$WORK/bin/vrf-oracle"
            app roll-dice "$out"
        fi
        if has pyth; then
            for name in default pro; do
                node er/pyth.js run "$name" "$WORK/pyth-$name.json" "$SVM_RPC" "$ER_RPC" "$WORK/payer.json" \
                    3>"$out/pyth-$name.jsonl" >"$out/pyth-$name.log" 2>&1
            done
        fi
        stop_all
        for name in base er vrf vrf-er; do [ ! -f "$WORK/$name.log" ] || mv "$WORK/$name.log" "$out/$name.log"; done
    fi
    if has suites; then
        suite table-mania test-table-mania "$out"
        suite committor test-committor "$out"
    fi
}

fetch
echo "fetched: ${#PRELOAD[@]} preload arguments; results in $RESULTS"
(side agave) || die "the Agave side did not run, so the run is broken"
(side svmd) || echo "the svmd side stopped early; the gate judges what ran" >&2
# A required suite must run and pass on Agave, or the run is broken.
# Otherwise a suite that never started on Agave passes the gate as "ran only
# on this chain".
required=()
has apps && required+=(--require counter)
has pyth && required+=(--require pyth-default --require pyth-pro)
has suites && required+=(--require table-mania --require committor)
python3 gate.py er/known.json "$RESULTS/agave" "$RESULTS/svmd" "${required[@]}" | tee "$RESULTS/gate.txt"
[ "$(tail -1 "$RESULTS/gate.txt")" = PASS ]
