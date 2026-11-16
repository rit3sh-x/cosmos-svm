#!/usr/bin/env bash
# Build, once, everything that the acceptance runs need from third-party
# sources:
#
# - the example apps and test suites of MagicBlock and tuktuk
# - the proof verifiers
# - the Solana Explorer
# - a stock ibc-go chain
# - the JS clients of this repository's own harnesses
#
# Every source is at its pin in scripts/pins.sh. The tools come from flake.nix.
#
#   scripts/prebuild.sh [PART...]    js, er, tuktuk, settle, explorer, ibc;
#                                    every part when none is given

set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=scripts/lib.sh
. "$ROOT/scripts/lib.sh"
# shellcheck source=scripts/pins.sh
. "$ROOT/scripts/pins.sh"
THIRD=${SVM_THIRD_PARTY:-$ROOT/target/third-party}
LOGS=$THIRD/logs
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0

ALL_PARTS="js er tuktuk settle explorer ibc"
PARTS=${*:-$ALL_PARTS}
for part in $PARTS; do
    [[ " $ALL_PARTS " == *" $part "* ]] || die "no part $part; the parts are $ALL_PARTS"
done
mkdir -p "$LOGS"

# need COMMAND... dies unless each command is on PATH.
need() {
    local c
    for c in "$@"; do
        type -P "$c" >/dev/null || die "$c is not on PATH. The tools come from the flake: nix develop"
    done
}

# step NAME COMMAND... runs a build step with its output in LOGS/NAME.log, and
# on failure shows the end of the log and stops.
step() {
    local name=$1
    shift
    echo "  $name"
    "$@" >"$LOGS/$name.log" 2>&1 || { tail -40 "$LOGS/$name.log" >&2; die "$name failed; the log is $LOGS/$name.log"; }
}

# checkout DIR REPO COMMIT [TAG] fetches REPO at COMMIT into DIR, once, with
# no history. With TAG, it fetches the tag, and the tag must be COMMIT.
checkout() {
    local dir=$1 repo=$2 commit=$3 tag=${4:-} ref
    [ -n "$commit" ] || die "no commit pinned for $repo${tag:+ $tag}"
    [ -d "$dir" ] && [ "$(git -C "$dir" rev-parse HEAD 2>/dev/null)" = "$commit" ] && return 0
    ref=${tag:+refs/tags/$tag}
    rm -rf "$dir"
    git init -q "$dir"
    git -C "$dir" fetch -q --depth 1 "$repo" "${ref:-$commit}"
    git -C "$dir" -c advice.detachedHead=false checkout -q FETCH_HEAD
    [ "$(git -C "$dir" rev-parse HEAD)" = "$commit" ] || die "$repo tag $tag is not $commit"
}

# built PART PIN succeeds when PART was built at PIN. Otherwise it removes the
# part's directory, so the build starts from nothing.
built() {
    local dir=$THIRD/$1
    [ "$(cat "$dir/.pin" 2>/dev/null)" = "$2" ] && return 0
    rm -rf "$dir"
    mkdir -p "$dir"
    return 1
}

# finish PART PIN records that PART is built at PIN.
finish() {
    echo "$2" >"$THIRD/$1/.pin"
    echo "$1: built in $THIRD/$1"
}

# set_id FILE KEYPAIR points FILE's declare_id! at KEYPAIR's public key.
set_id() {
    sed -i "s/^declare_id!(\"[^\"]*\");/declare_id!(\"$(solana-keygen pubkey "$2")\");/" "$1"
}

# js installs the pinned clients of this repository's harnesses, from their
# lockfiles.
js() {
    local d
    need pnpm
    for d in clients er settle tuktuk; do
        [ -f "$ROOT/rpc/accept/$d/package.json" ] || continue
        step "js-$d" env -C "$ROOT/rpc/accept/$d" pnpm install --frozen-lockfile
    done
}

# er builds MagicBlock's three example apps with the Anchor they pin, and the
# programs of the validator's own test suites.
er() {
    local pin="$MB_EXAMPLES_COMMIT $MB_VALIDATOR_COMMIT $MB_ANCHOR" dir=$THIRD/er app
    built er "$pin" && return 0
    need "$MB_ANCHOR" corepack cargo-build-sbf
    checkout "$dir/examples" "$MB_EXAMPLES_REPO" "$MB_EXAMPLES_COMMIT"
    for app in counter session-keys roll-dice; do
        # `anchor keys sync` points declare_id! at the build's own keypair, as
        # the examples' CI does, so each program id is this machine's.
        step "er-$app" env -C "$dir/examples/$app/anchor" bash -c "corepack yarn install --frozen-lockfile && $MB_ANCHOR keys sync && $MB_ANCHOR build"
    done
    checkout "$dir/magicblock-validator" "$MB_VALIDATOR_REPO" "$MB_VALIDATOR_COMMIT" "$MB_VALIDATOR_TAG"
    # The build takes minutes. A hung build fails here with its log, and does
    # not hold the run until the job's own timeout.
    step er-suites timeout --kill-after=30 1800 make -C "$dir/magicblock-validator/test-integration" programs
    finish er "$pin"
}

# tuktuk installs the examples that the flows run. Then it builds tuktuk's
# programs as its CI does, with TESTING set so that a test's own key may
# initialize the config. Last, it builds the workspace packages that its tests
# import.
tuktuk() {
    local pin="$TUKTUK_COMMIT $TUKTUK_ANCHOR" dir=$THIRD/tuktuk
    built tuktuk "$pin" && return 0
    need "$TUKTUK_ANCHOR" corepack cargo-build-sbf
    checkout "$dir/tuktuk" "$TUKTUK_REPO" "$TUKTUK_COMMIT"
    step tuktuk-examples env -C "$dir/tuktuk/typescript-examples" corepack yarn install --frozen-lockfile
    step tuktuk-programs env -C "$dir/tuktuk/solana-programs" bash -c "corepack yarn install --immutable && TESTING=true $TUKTUK_ANCHOR build && corepack yarn build"
    finish tuktuk "$pin"
}

# settle checks out Hyperlane's monorepo and registry for the e2e, and builds
# the SP1 and RISC Zero verifiers as SBPF v3, which both chains deploy.
settle() {
    local pin="$HL_TAG $HL_REGISTRY_COMMIT $SP1_COMMIT $R0_COMMIT" dir=$THIRD/settle p programs owner
    built settle "$pin" && return 0
    need cargo-build-sbf solana-keygen
    checkout "$dir/sp1-solana" "$SP1_REPO" "$SP1_COMMIT"
    step settle-sp1 cargo build-sbf --arch v3 --manifest-path "$dir/sp1-solana/example/program/Cargo.toml" --sbf-out-dir "$dir/sp1"
    # risc0-solana has no Solana deployment, so an operator builds its
    # verifier and router with its own ids, and the router with its own
    # INITIAL_OWNER. The run keeps one set of keys here.
    checkout "$dir/risc0-solana" "$R0_REPO" "$R0_COMMIT"
    mkdir -p "$dir/r0"
    for p in owner verifier-keypair router-keypair; do
        solana-keygen new --no-bip39-passphrase -s -o "$dir/r0/$p.json" --force >/dev/null
    done
    programs=$dir/risc0-solana/solana-verifier/programs
    owner=$(solana-keygen pubkey "$dir/r0/owner.json")
    set_id "$programs/groth_16_verifier/src/lib.rs" "$dir/r0/verifier-keypair.json"
    set_id "$programs/verifier_router/src/lib.rs" "$dir/r0/router-keypair.json"
    for p in groth_16_verifier verifier_router; do
        step "settle-r0-$p" env INITIAL_OWNER="$owner" cargo build-sbf --arch v3 \
            --manifest-path "$programs/$p/Cargo.toml" --sbf-out-dir "$dir/r0"
    done
    git -C "$dir/risc0-solana" checkout -q -- .
    checkout "$dir/hyperlane-monorepo" "$HL_REPO" "$HL_COMMIT" "$HL_TAG"
    checkout "$dir/hyperlane-registry" "$HL_REGISTRY_REPO" "$HL_REGISTRY_COMMIT"
    finish settle "$pin"
}

# explorer builds the Solana Explorer at its pin. Its package.json names the
# pnpm it wants, and pnpm 10 switches to it.
explorer() {
    local pin=$EXPLORER_COMMIT dir=$THIRD/explorer
    built explorer "$pin" && return 0
    need pnpm
    checkout "$dir/explorer" "$EXPLORER_REPO" "$EXPLORER_COMMIT"
    step explorer-build env -C "$dir/explorer" bash -c "pnpm install --frozen-lockfile --silent && pnpm build"
    finish explorer "$pin"
}

# ibc builds simd, the stock ibc-go chain the IBC harness relays to.
ibc() {
    local pin=$IBC_GO_TAG dir=$THIRD/ibc
    built ibc "$pin" && return 0
    need go
    checkout "$dir/ibc-go" "$IBC_GO_REPO" "$IBC_GO_COMMIT" "$IBC_GO_TAG"
    step ibc-simd env -C "$dir/ibc-go/simapp" go build -o "$dir/simd" ./simd
    finish ibc "$pin"
}

for part in $PARTS; do
    "$part"
done
