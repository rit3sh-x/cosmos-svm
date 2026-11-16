# shellcheck shell=bash
# Shared helpers for acceptance harnesses. Source after `set -euo pipefail`
# from rpc/accept, with ROOT and WORK set. Sets no shell options and runs
# nothing on its own.
#
# Uses scripts/lib.sh for common process and wait helpers, nodetool for chain
# setup, and Python helpers for JSON and RPC assertions.

# shellcheck source=scripts/lib.sh
. "$ROOT/scripts/lib.sh"

# Build named binaries into WORK.
build() {
    local name dir
    for name in "$@"; do
        case $name in
        svmd) go build -o "$WORK/svmd" "$ROOT/cmd/svmd" ;;
        svmd-devnet) go build -tags devnet -o "$WORK/svmd-devnet" "$ROOT/cmd/svmd" ;;
        fill) go build -o "$WORK/fill" "$ROOT/rpc/accept/fill" ;;
        nodetool) go build -o "$WORK/nodetool" "$ROOT/tools/nodetool" ;;
        svm-tpu | quic-client)
            dir=tpu
            [ "$name" = svm-tpu ] || dir=tools/quic-client
            if ! cargo build --release --locked --manifest-path "$ROOT/$dir/Cargo.toml" >"$WORK/cargo.log" 2>&1; then
                tail -20 "$WORK/cargo.log" >&2
                die "cargo build of $dir failed"
            fi
            cp "$ROOT/$dir/target/release/$name" "$WORK/"
            ;;
        *) die "no binary named $name" ;;
        esac
    done
}

# Generate a Solana keypair at each path.
keygen() {
    local file
    for file in "$@"; do
        solana-keygen new --no-bip39-passphrase -s -o "$file" --force >/dev/null
    done
}

# Write a Solana CLI config that uses SVM_RPC and the given signer.
cli_config() {
    printf 'json_rpc_url: %s\nwebsocket_url: ""\nkeypair_path: %s\naddress_labels: {}\ncommitment: confirmed\n' \
        "$SVM_RPC" "$2" >"$1"
}

# Run a detached process from WORK, and record its log and PID. A separate
# session lets stop_group stop the process and its children. The group exists
# only after setsid runs in the child, so spawn waits until the group exists or
# the child is gone. A caller can then check the group at once.
spawn() {
    local name=$1 pid
    shift
    (cd "$WORK" && exec setsid nohup "$@" >>"$name.log" 2>&1 </dev/null) &
    pid=$!
    echo "$pid" >"$WORK/$name.pid"
    until group_alive "$pid" || ! running "$pid"; do sleep 0.01; done
}

# Print the value selected by a Python expression from JSON on stdin.
json() {
    python3 -c "import json, sys; print(json.load(sys.stdin)$1)"
}

# Print a program file's executable hash: the sha256 of its bytes without
# trailing zeros, as solana-verify reports it.
exec_hash() {
    python3 -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read().rstrip(b"\0")).hexdigest())' "$1"
}

# Print the latest CometBFT height, or nothing if the machine does not answer.
height() {
    "$WORK/nodetool" height -comet "$1" 2>/dev/null
}

# Succeed when the machine reaches the requested height.
at_height() {
    local got
    got=$(height "$1") && [ "$got" -ge "$2" ]
}

# Wait for a machine to reach a height. On failure, report its last known height.
await_height() {
    local name=$1 url=$2 want=$3 what=$4
    if ! wait_for "${5:-240}" "$name at height $want ($what)" at_height "$url" "$want"; then
        echo "$name stalled at $(height "$url" || echo no answer), want $want ($what)" >&2
        return 1
    fi
    echo "  $name reached height $(height "$url") ($what)"
}

# Set up and start a local validator with a funded payer and QUIC enabled.
# Writes payer.json, dest.json and cli.yml to WORK, sets GENESIS_HASH and
# QUIC_PORT, and waits for SVM_RPC to respond.
start_local_chain() {
    build svmd svm-tpu nodetool
    QUIC_PORT=$("$WORK/nodetool" free-port -udp)
    keygen "$WORK/payer.json" "$WORK/dest.json"
    # `program deploy` takes its upgrade authority from the default signer, even
    # when -k names a payer. The config names one, so a deploy works on a
    # machine with no ~/.config/solana/id.json.
    cli_config "$WORK/cli.yml" "$WORK/payer.json"
    "$WORK/svmd" init local --chain-id svm-accept --home "$WORK/home" >"$WORK/init.log" 2>&1
    grep -q "^genesis hash: " "$WORK/init.log"
    "$WORK/svmd" add-genesis-programs "$ROOT/genesis" --home "$WORK/home"
    "$WORK/nodetool" fund -genesis "$WORK/home/config/genesis.json" -lamports 10000000000000000 \
        "$(solana-keygen pubkey "$WORK/payer.json")"
    "$WORK/nodetool" set "$WORK/home/config/app.toml" index-max-bytes=1073741824 \
        tpu-quic-address="\"127.0.0.1:$QUIC_PORT\""
    # The genesis is final only after the programs and the payer, so the hash
    # comes from the file here, not from init.
    GENESIS_HASH=$(python3 genesis_hash.py "$WORK/home/config/genesis.json")

    if timeout 60 "$WORK/svmd" start --home "$WORK/home" --expected-genesis-hash \
        11111111111111111111111111111111 >"$WORK/mismatch.log" 2>&1 ||
        ! grep -q "genesis hash mismatch: actual=$GENESIS_HASH, expected=1111" "$WORK/mismatch.log"; then
        die "start accepted a genesis it did not expect"
    fi
    echo "expected-genesis-hash: a mismatch refuses by name"
    spawn node "$WORK/svmd" start --home "$WORK/home" --expected-genesis-hash "$GENESIS_HASH" "$@"
    wait_for_chain
}

# Succeed when SVM_RPC responds to a block-height request.
answers() {
    solana -u "$SVM_RPC" block-height >/dev/null 2>&1
}

# Wait for the local chain to answer. On failure, print its log.
wait_for_chain() {
    if ! wait_for 240 "$SVM_RPC to answer block-height" answers; then
        tail -40 "$WORK/node.log" >&2 2>/dev/null || true
        exit 1
    fi
}

# Stop unless each named command is on PATH, and say where the tools come from.
need() {
    local c
    for c in "$@"; do
        type -P "$c" >/dev/null || die "$c is not on PATH. The tools come from the flake: nix develop"
    done
}

# Print the directory where scripts/prebuild.sh builds a part. prebuild.sh
# first builds the part if it is missing or its pins in scripts/pins.sh changed.
third_party() {
    "$ROOT/scripts/prebuild.sh" "$1" >&2
    echo "${SVM_THIRD_PARTY:-$ROOT/target/third-party}/$1"
}

# Download a released binary from URL to FILE, check it against SHA256 and make
# it executable. Each run fetches the binaries under the Business Source
# License this way and keeps no copy.
fetch_release() {
    curl -sSfL -o "$3" "$1"
    echo "$2  $3" | sha256sum -c --quiet || die "$1 is not the pinned $2"
    chmod +x "$3"
}

# Write mainnet's program at ID to FILE and check its executable hash against
# EXEC_SHA256. If the mainnet bytes do not match the pin, the run stops until a
# separate change updates the pin.
dump_program() {
    solana program dump -u https://api.mainnet-beta.solana.com "$1" "$3" >/dev/null
    [ "$(exec_hash "$3")" = "$2" ] || die "mainnet's program $1 is not the pinned $2"
}

# Write mainnet's account at ADDRESS to FILE, as `solana account --output json`
# writes it.
mainnet_account() {
    solana account "$1" -u https://api.mainnet-beta.solana.com --output json --output-file "$2" >/dev/null
}
