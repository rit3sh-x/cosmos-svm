#!/usr/bin/env bash
# The tuktuk acceptance run. Helium's tuktuk runs on solana-test-validator
# 4.3.0 and on this chain with the same pins. gate.py judges this chain against
# Agave. A test that passes on Agave must pass on this chain, unless
# tuktuk/known.json lists it.
#
#   make accept-tuktuk                every part
#   rpc/accept/tuktuk.sh PART...      suite: tuktuk's own Anchor tests, on its TESTING build
#                                     flows: the memo and cron examples with the published
#                                            crank turner, against the genesis programs
#
# On the svmd side, the shim (shim/solana-test-validator) is first on PATH.
# The suite's own build replaces the genesis rows on this chain (SHIM_REPLACE),
# so both chains start it clean. Needs the tools of flake.nix on PATH, which
# include tuktuk's CLI and crank turner, and network access to GitHub and
# mainnet. scripts/prebuild.sh builds tuktuk's programs, packages and examples
# once, into target/third-party unless SVM_THIRD_PARTY says otherwise.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
HERE=$ROOT/rpc/accept
WORK=$(mktemp -d)
RESULTS=${SVM_TUKTUK_RESULTS:-$ROOT/rpc/accept/results/tuktuk-$(date -u +%Y%m%dT%H%M%SZ)}
export SVM_RPC=http://127.0.0.1:8899
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
# shellcheck source=scripts/pins.sh
. "$ROOT/scripts/pins.sh"

# The pins. The mainnet bytes of the programs are genesis rows, pinned in
# genesis/manifest.json, and the flows run against them. The suite runs
# tuktuk's own TESTING build of helium/tuktuk at the commit that
# scripts/pins.sh names, as its CI does. The CLI and the crank turner come from
# the flake.
PROGRAMS=(
    "tuktukUrfhXT6ZT77QTU8RQtvgL967uRuVagWF57zVA a30a79f674b3ee9938778df26e648c31273f56d9813549830cb1b7cb5194f080" # tuktuk, deployed at slot 460,133,297
    "cronAjRZnJn3MTP3B9kE62NWDrjSuAPVXf9c4hu4grM 3247399abbc95b8ee1f8323d16b63141bf769538e458ab28bae49fe67423dbdf" # cron, slot 460,133,337
)
# Mainnet's config and both IDL accounts, which genesis copies.
ACCOUNTS=(HGBovqKte26DbEMBma3T1TvvdmAFYSSgjncRoKECqfXq GkUxZMcw2RbwZ64VL3MvBtYNV8zim3y7UfzabFTybAUJ EiFi2BQZaNbG3HFdVaU82web2GqXaq9vepobNNgzMHPb)
# The suite's programs at the ids its Anchor.toml names.
declare -A SUITE=(
    [tuktuk]=tuktukUrfhXT6ZT77QTU8RQtvgL967uRuVagWF57zVA
    [cron]=cronAjRZnJn3MTP3B9kE62NWDrjSuAPVXf9c4hu4grM
    [cpi_example]=cpic9j9sjqvhn2ZX3mqcCgzHKCwiiBTyEszyCwN7MBC
    [return_example]=5mwgRJMr7Cnb9jbpphPSBL7QbNKMM8y1TzdhsoZVjztf
)

PARTS=" ${*:-suite flows} "
has() { [[ $PARTS == *" $1 "* ]]; }
for part in $PARTS; do
    case $part in suite | flows) ;; *) die "no part $part: suite or flows" ;; esac
done
trap 'rm -rf "$WORK"' EXIT

# fetch gets the preload arguments of the base chains. The mainnet program
# bytes, the config and the IDL accounts come from mainnet at each run.
fetch() {
    local row id sha address name
    PRELOAD=() SUITE_PRELOAD=()
    mkdir -p "$WORK/so" "$WORK/bin"
    need solana solana-test-validator node corepack tuktuk tuktuk-crank-turner "$TUKTUK_ANCHOR"
    build svmd-devnet nodetool
    # The shim's genesis reads every manifest row, so the cache must hold them.
    (cd "$ROOT" && go run ./genesis/fetch) >"$WORK/fetch.log" 2>&1 || { tail -20 "$WORK/fetch.log" >&2; die "the genesis rows did not fetch"; }
    TP=$(third_party tuktuk)
    if has flows; then
        for row in "${PROGRAMS[@]}"; do
            read -r id sha <<<"$row"
            dump_program "$id" "$sha" "$WORK/so/$id.so"
            PRELOAD+=(--bpf-program "$id" "$WORK/so/$id.so")
        done
        for address in "${ACCOUNTS[@]}"; do
            mainnet_account "$address" "$WORK/$address.json"
            PRELOAD+=(--account "$address" "$WORK/$address.json")
        done
    fi
    if has suite; then
        # The tests call `anchor idl init` by name, so `anchor` on their PATH
        # is the Anchor that tuktuk pins.
        ln -sf "$(command -v "$TUKTUK_ANCHOR")" "$WORK/bin/anchor"
        for name in "${!SUITE[@]}"; do
            SUITE_PRELOAD+=(--bpf-program "${SUITE[$name]}" "$TP/tuktuk/solana-programs/target/deploy/$name.so")
        done
    fi
}

stop_all() {
    local name
    for name in turner base; do stop_group "$WORK/$name.pid" 30; done
}

# flows OUT runs the memo and cron examples with the crank turner on the base
# chain. The base chain holds the genesis programs: this chain's own, or the
# same mainnet bytes and accounts preloaded on Agave.
flows() {
    local turner
    spawn base solana-test-validator --reset --quiet --ledger "$WORK/ledger" --rpc-port 8899 "${PRELOAD[@]}"
    wait_for_chain
    turner=$(solana-keygen pubkey "$WORK/turner.json")
    solana -u "$SVM_RPC" airdrop 1000 "$(solana-keygen pubkey "$WORK/payer.json")" >/dev/null
    solana -u "$SVM_RPC" airdrop 10 "$turner" >/dev/null
    spawn turner env TUKTUK__RPC_URL="$SVM_RPC" TUKTUK__KEY_PATH="$WORK/turner.json" TUKTUK__MIN_CRANK_FEE=10000 \
        tuktuk-crank-turner
    python3 tuktuk/flows.py "$SVM_RPC" "$TP/tuktuk/typescript-examples" "$WORK/payer.json" "$turner" "$(command -v tuktuk)" \
        3>"$1/flows.jsonl" >"$1/flows.log" 2>&1 || true
    [ -s "$1/flows.jsonl" ] || die "the flows wrote no results: $1/flows.log"
    stop_all
    mv "$WORK/base.log" "$1/flows-base.log"
    mv "$WORK/turner.log" "$1/turner.log"
}

# suite OUT runs tuktuk's own tests as anchor test does. The base chain starts
# with the suite's build and the wallet as --mint. Then ts-mocha runs with
# mocha's json reporter. The tests call `anchor idl init` through the
# Anchor.toml provider, whose wallet is ~/.config/solana/id.json, so HOME is
# a directory of this run that holds the wallet at that path.
suite() {
    local wallet=$WORK/wallet.json home=$WORK/home
    mkdir -p "$home/.config/solana"
    cp "$wallet" "$home/.config/solana/id.json"
    SHIM_REPLACE=1 spawn base solana-test-validator --reset --quiet --ledger "$WORK/ledger" --rpc-port 8899 \
        --mint "$(solana-keygen pubkey "$wallet")" "${SUITE_PRELOAD[@]}"
    wait_for_chain
    (cd "$TP/tuktuk/solana-programs" &&
        HOME=$home ANCHOR_WALLET=$wallet ANCHOR_PROVIDER_URL=$SVM_RPC PATH=$WORK/bin:$PATH COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
            timeout 3600 corepack yarn run ts-mocha -p ./tsconfig.test.json -t 1000000 --exit --reporter json \
            --reporter-option "output=$1/suite.mocha.json" 'tests/**/*.ts') >"$1/suite.log" 2>&1 || true
    [ -s "$1/suite.mocha.json" ] || die "tuktuk's suite wrote no results: $1/suite.log"
    stop_all
    mv "$WORK/base.log" "$1/suite-base.log"
}

# side NAME runs every part on one base chain, agave or svmd.
side() {
    local out=$RESULTS/$1
    mkdir -p "$out"
    trap stop_all EXIT
    if [ "$1" = svmd ]; then
        export PATH=$HERE/shim:$PATH SHIM_BIN=$WORK SHIM_WORK=$WORK/shim
        mkdir -p "$SHIM_WORK"
    fi
    echo "$1: $(solana-test-validator --version 2>/dev/null || echo "the shim, $(command -v solana-test-validator)")"
    keygen "$WORK/payer.json" "$WORK/turner.json" "$WORK/wallet.json"
    ! has flows || flows "$out"
    ! has suite || suite "$out"
}

fetch
echo "fetched: ${#PRELOAD[@]} flow and ${#SUITE_PRELOAD[@]} suite preload arguments; results in $RESULTS"
(side agave) || die "the Agave side did not run, so the run is broken"
(side svmd) || echo "the svmd side stopped early; the gate judges what ran" >&2
required=()
has flows && required+=(--require flows)
has suite && required+=(--require suite)
python3 gate.py tuktuk/known.json "$RESULTS/agave" "$RESULTS/svmd" "${required[@]}" | tee "$RESULTS/gate.txt"
