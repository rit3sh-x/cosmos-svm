#!/usr/bin/env bash
# The IBC acceptance run. A stock relayer moves a simd token here and back.
#
#   make accept-ibc
#
# One svmd validator, one svmd full node for the relayer, stock ibc-go v11.2.0
# simd and Hermes 1.13.1 run on one machine. The steps:
#   1. Hermes opens a transfer channel.
#   2. A transfer in. The voucher appears in an operator's balance here.
#   3. The transfer back pays the usvm fee floor, and simd's sender is whole.
#   4. With Hermes stopped, a transfer each way. Hermes clears both on restart.
#      Only the relayer node indexes the packet sent from here.
#   5. A transfer to an address that holds no usvm is refunded.
#   6. A second Hermes relays the same packets, and CheckTx refuses the copy.
#   7. A held relay lands with a proof at its send height.
# Beside the steps, usvm cannot leave over the channel.
#
# Queries of this chain's services go through stock simd's CLI. This chain
# serves the same bank, auth, tx and ibc services, and that is the claim under
# test.
#
# Separate operator accounts keep relayer transactions from racing the
# validator's account sequence. The relayer operators are bonded but not
# started, so the first validator commits alone.
#
# Set SVM_IBC_SIMD to use an existing simd binary. Otherwise simd comes from
# the ibc part of scripts/prebuild.sh. Hermes comes from the flake.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
RESULTS=${SVM_IBC_RESULTS:-$ROOT/rpc/accept/results/ibc-$(date -u +%Y%m%dT%H%M%SZ)}
SVM_CHAIN=svm-ibc
SIM_CHAIN=simd-ibc
# Separate RPC ports. Hermes connects to the gRPC endpoint of the relayer node.
VAL=http://127.0.0.1:28657
FULL=http://127.0.0.1:28757
SIM=http://127.0.0.1:27657
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh

stop() {
    local status=$? p
    # CONT first. A Hermes that step 7 holds with SIGSTOP does not act on TERM.
    for p in hermes hermes2 simd full val; do
        kill -CONT "$(cat "$WORK/$p.pid" 2>/dev/null)" 2>/dev/null || true
    done
    for p in hermes hermes2 simd full val; do stop_pidfile "$WORK/$p.pid"; done
    # A failed run keeps every process's log, the only record of the cause.
    if [ "$status" != 0 ] && [ -d "$RESULTS" ]; then cp "$WORK"/*.log "$RESULTS/" 2>/dev/null || true; fi
    rm -rf "$WORK"
}
trap stop EXIT

halt() {
    stop_pidfile "$WORK/$1.pid"
}

simd() { "$SIMD" --home "$WORK/sim" "$@"; }
hermes() { "$HERMES" --config "$WORK/hermes.toml" "$@"; }

# Print an account's balance in one denom.
balance() {
    simd query bank balances "$2" --node "$1" --output json | python3 ibc.py balance "$3"
}

# Wait until a balance reaches the expected value. A balance on one chain moves
# when the relayer acts on the other, so every step polls.
expect() {
    local node=$1 addr=$2 denom=$3 want=$4 what=$5
    if ! wait_for "${6:-180}" "$what" holds "$node" "$addr" "$denom" "$want"; then
        echo "$what: $denom is $(balance "$node" "$addr" "$denom" 2>/dev/null || echo unreadable), want $want" >&2
        tail -30 "$WORK/hermes.log" >&2 || true
        exit 1
    fi
    echo "  $what: $denom $want" | tee -a "$RESULTS/steps.txt"
}

# Succeed when the account holds exactly the expected balance.
holds() {
    [ "$(balance "$1" "$2" "$3" 2>/dev/null)" = "$4" ]
}

# Wait until a process logs a line. The next step waits on that event, not on
# a guess at how long it takes.
logged() {
    local name=$1 line=$2 what=$3
    if ! wait_for "${4:-60}" "$name to log '$line' ($what)" grep -qF -- "$line" "$WORK/$name.log"; then
        tail -30 "$WORK/$name.log" >&2 || true
        exit 1
    fi
}

# Submit an IBC transfer with simd's CLI and wait until a block includes it.
# On this chain gasPrices is the fee floor, which simd rounds up to whole usvm
# as the ante handler does.
send() {
    local node=$1 chain=$2 from=$3 to=$4 amount=$5 gasPrices=$6 out hash
    out=$(simd tx ibc-transfer transfer transfer channel-0 "$to" "$amount" --from "$from" \
        --chain-id "$chain" --node "$node" --keyring-backend test \
        --gas auto --gas-adjustment 1.5 --gas-prices "$gasPrices" -y --output json 2>&1) || { echo "$out" >&2; return 1; }
    hash=$(python3 ibc.py txhash <<<"$out")
    wait_for 60 "transfer $hash in a block" landed "$node" "$hash"
}

# Read the included transaction's height and packet sequence into SENT_HEIGHT and SENT_SEQ.
landed() {
    local out
    out=$(simd query tx "$2" --node "$1" --output json 2>/dev/null) || return 1
    read -r SENT_HEIGHT SENT_SEQ < <(python3 ibc.py landed <<<"$out")
}

mkdir -p "$RESULTS"
: > "$RESULTS/steps.txt"
build svmd nodetool
need hermes
SIMD=${SVM_IBC_SIMD:-$(third_party ibc)/simd}
HERMES=$(type -P hermes)
{ "$HERMES" version 2>/dev/null | head -1; sha256sum "$SIMD" "$HERMES" | sed "s#$ROOT/##"; } > "$RESULTS/versions.txt"

# All keys use simd's test keyring. The operator receives vouchers and pays
# fees on this chain. The relayer keys sign Hermes transactions on both chains.
simd init simd --chain-id "$SIM_CHAIN" --default-denom stake > "$WORK/simd-init.log" 2>&1
for k in val user operator relayer relayer2 stranger; do
    simd keys add "$k" --keyring-backend test --output json > "$WORK/key-$k.json"
done
addr() { simd keys show "$1" -a --keyring-backend test; }
USER_ADDR=$(addr user)
OPERATOR=$(addr operator)
RELAYER=$(addr relayer)
STRANGER=$(addr stranger)
for k in relayer relayer2; do
    json '["mnemonic"]' <"$WORK/key-$k.json" >"$WORK/$k.mnemonic"
done

for k in val user relayer relayer2; do
    simd genesis add-genesis-account "$k" 100000000000stake --keyring-backend test
done
simd genesis gentx val 10000000000stake --chain-id "$SIM_CHAIN" --keyring-backend test > "$WORK/gentx.log" 2>&1
simd genesis collect-gentxs > "$WORK/collect.log" 2>&1
"$WORK/nodetool" set "$WORK/sim/config/config.toml" proxy_app='"tcp://127.0.0.1:27658"' \
    rpc.laddr='"tcp://127.0.0.1:27657"' p2p.laddr='"tcp://127.0.0.1:27656"' pprof_laddr='""' timeout_commit='"1s"'
"$WORK/nodetool" set "$WORK/sim/config/app.toml" minimum-gas-prices='"0stake"' grpc.address='"localhost:27090"'

# On this chain h0 is the validator and h1 is the relayer's full node. h2 and
# h3 hold only the consensus keys of the relayer operators.
for i in 0 1 2 3; do
    "$WORK/svmd" init "n$i" --chain-id "$SVM_CHAIN" --home "$WORK/h$i" > "$WORK/svmd-init$i.log" 2>&1
done
# Ten units of power for the first operator and one for each other. The first
# alone has over two thirds.
operator() {
    "$WORK/nodetool" validators -out "$WORK/validators.json" -bond "$2" -account "$3" \
        -pubkey "$("$WORK/svmd" comet show-validator --home "$WORK/h$1")"
}
operator 0 10000000 "$OPERATOR"
operator 2 1000000 "$RELAYER"
operator 3 1000000 "$(addr relayer2)"
"$WORK/svmd" init coordinator --chain-id "$SVM_CHAIN" --home "$WORK/h0" --overwrite \
    --validators-file "$WORK/validators.json" > "$WORK/genesis.log" 2>&1
cp "$WORK/h0/config/genesis.json" "$WORK/h1/config/genesis.json"
VAL_ID=$("$WORK/svmd" comet show-node-id --home "$WORK/h0")
for i in 0 1; do
    "$WORK/nodetool" set "$WORK/h$i/config/config.toml" proxy_app="\"tcp://127.0.0.1:$((28658 + 100 * i))\"" \
        rpc.laddr="\"tcp://127.0.0.1:$((28657 + 100 * i))\"" p2p.laddr="\"tcp://127.0.0.1:$((28656 + 100 * i))\"" \
        pprof_laddr='""' prometheus_listen_addr="\":$((28660 + 100 * i))\"" addr_book_strict=false allow_duplicate_ip=true
    "$WORK/nodetool" set "$WORK/h$i/config/app.toml" index-max-bytes=1073741824 program-cache-bytes=268435456 threads=2
done
"$WORK/nodetool" set "$WORK/h0/config/app.toml" grpc.address='"localhost:29090"'
# Only the relayer node indexes transactions, so that Hermes can find outgoing packets.
"$WORK/nodetool" set "$WORK/h1/config/config.toml" persistent_peers="\"$VAL_ID@127.0.0.1:28656\"" indexer='"kv"'
# Retain enough state versions and blocks for Hermes client refreshes and proofs.
"$WORK/nodetool" set "$WORK/h1/config/app.toml" pruning-keep-recent='"100"' min-retain-blocks=100000
{
    grep -E '^indexer' "$WORK/h0/config/config.toml" "$WORK/h1/config/config.toml"
    grep -E '^(pruning-keep-recent|min-retain-blocks) =' "$WORK/h0/config/app.toml" "$WORK/h1/config/app.toml"
} | sed "s#$WORK/##" > "$RESULTS/roles.txt"

spawn val "$WORK/svmd" start --home "$WORK/h0" --svm.rpc-address 127.0.0.1:10899 --svm.ws-address 127.0.0.1:10900
spawn full "$WORK/svmd" start --home "$WORK/h1" --svm.rpc-address 127.0.0.1:10901 --svm.ws-address 127.0.0.1:10902
spawn simd "$SIMD" start --home "$WORK/sim"
await_height "the relayer node" "$FULL" 3 "following the validator"
await_height simd "$SIM" 3 "producing blocks"

# Hermes. compat_mode is 0.38, because Hermes knows no CometBFT version past
# 0.38. Hermes pulls this chain's events height by height. Step 7 holds Hermes
# with SIGSTOP, and CometBFT drops a websocket whose client misses pings for
# 30 s. A pushed event would then be lost, and a pulled event is only late.
sed "s#WORK#$WORK#g" hermes.toml >"$WORK/hermes.toml"
cp hermes.toml "$RESULTS/hermes.toml"
# The second Hermes differs from the first in its key and nothing else.
sed "s/^key_name = 'relayer'$/key_name = 'relayer2'/" "$WORK/hermes.toml" > "$WORK/hermes2.toml"
for chain in "$SVM_CHAIN" "$SIM_CHAIN"; do
    for k in relayer relayer2; do
        hermes keys add --chain "$chain" --key-name "$k" --mnemonic-file "$WORK/$k.mnemonic" > /dev/null
    done
done

# Step 1. The channel opens.
hermes create channel --a-chain "$SVM_CHAIN" --b-chain "$SIM_CHAIN" --a-port transfer --b-port transfer \
    --new-client-connection --yes > "$WORK/create.log" 2>&1 || { tail -40 "$WORK/create.log" >&2; exit 1; }
for node in "$FULL" "$SIM"; do
    state=$(simd query ibc channel end transfer channel-0 --node "$node" --output json | json '["channel"]["state"]')
    echo "  step 1: transfer/channel-0 on $node is $state" | tee -a "$RESULTS/steps.txt"
    [ "$state" = STATE_OPEN ] || { echo "the channel did not open" >&2; exit 1; }
done
spawn hermes "$HERMES" --config "$WORK/hermes.toml" start

# Step 2. A transfer in.
VOUCHER=ibc/$(printf 'transfer/channel-0/stake' | sha256sum | cut -c1-64 | tr a-f A-F)
START=$(balance "$SIM" "$USER_ADDR" stake)
send "$SIM" "$SIM_CHAIN" user "$OPERATOR" 1000stake 0stake
expect "$FULL" "$OPERATOR" "$VOUCHER" 1000 "step 2: the operator holds the voucher"

# Step 3. The transfer back.
FEES_FROM=$(balance "$FULL" "$OPERATOR" usvm)
send "$FULL" "$SVM_CHAIN" operator "$USER_ADDR" "1000$VOUCHER" 0.000001usvm
expect "$SIM" "$USER_ADDR" stake "$START" "step 3: simd's sender is whole again"
expect "$FULL" "$OPERATOR" "$VOUCHER" 0 "step 3: the voucher is burned here"
PAID=$((FEES_FROM - $(balance "$FULL" "$OPERATOR" usvm)))
echo "  step 3: the operator paid $PAID usvm in fees" | tee -a "$RESULTS/steps.txt"
[ "$PAID" -ge 1 ] || { echo "the transfer back paid no usvm" >&2; exit 1; }

# Step 4. Cleared after an outage.
halt hermes
send "$SIM" "$SIM_CHAIN" user "$OPERATOR" 500stake 0stake
await_height "the relayer node" "$FULL" $(($(height "$FULL") + 5)) "five blocks with no relayer"
[ "$(balance "$FULL" "$OPERATOR" "$VOUCHER")" = 0 ] || { echo "a packet moved with Hermes stopped" >&2; exit 1; }
spawn hermes "$HERMES" --config "$WORK/hermes.toml" start
expect "$FULL" "$OPERATOR" "$VOUCHER" 500 "step 4: a packet from simd cleared on restart"

halt hermes
send "$FULL" "$SVM_CHAIN" operator "$USER_ADDR" "500$VOUCHER" 0.000001usvm
query="send_packet.packet_src_channel='channel-0'"
found=$(curl -sf --get "$FULL/tx_search" --data-urlencode "query=\"$query\"" | json '["result"]["total_count"]')
refused=$(curl -s --get "$VAL/tx_search" --data-urlencode "query=\"$query\"" | json '.get("error", {}).get("data", "")')
echo "  step 4: the relayer node indexes $found packets sent from here; the validator: $refused" |
    tee -a "$RESULTS/steps.txt"
[ "$found" -ge 2 ] || { echo "the relayer node did not index the packets sent from here" >&2; exit 1; }
[ -n "$refused" ] || { echo "the validator answered tx_search" >&2; exit 1; }
spawn hermes "$HERMES" --config "$WORK/hermes.toml" start
expect "$SIM" "$USER_ADDR" stake "$START" "step 4: a packet from here cleared on restart"

# Step 5. A stranded receive is refunded. stranger is only an address on this
# chain. It has no usvm and cannot pay a fee, so a voucher there could never
# move.
send "$SIM" "$SIM_CHAIN" user "$STRANGER" 700stake 0stake
expect "$SIM" "$USER_ADDR" stake "$START" "step 5: refunded after the error ack" 240
[ "$(balance "$FULL" "$STRANGER" "$VOUCHER")" = 0 ] || { echo "the stranger holds a voucher" >&2; exit 1; }
if simd query auth account "$STRANGER" --node "$FULL" > /dev/null 2>&1; then
    echo "the refused receive created the stranger's account" >&2
    exit 1
fi
echo "  step 5: the stranger holds nothing and has no account here" | tee -a "$RESULTS/steps.txt"

# Step 6. Duplicate relays. Both Hermes instances relay the same events.
# CheckTx must refuse a duplicate while the original packet transaction is
# still in the mempool.
spawn hermes2 "$HERMES" --config "$WORK/hermes2.toml" start
# simd's events reach Hermes over its websocket, so the second Hermes sees a
# send only after it subscribes. Hermes subscribes to every chain before it
# logs that it has started.
logged hermes2 'Hermes has started' "step 6: the second relayer listens to simd"
for amount in 100 200 300; do
    send "$SIM" "$SIM_CHAIN" user "$OPERATOR" "${amount}stake" 0stake
done
expect "$FULL" "$OPERATOR" "$VOUCHER" 600 "step 6: three packets, each landed once"
# simd runs the same check on acknowledgements, so only refusals of
# transactions sent to this chain count. A relayer logs a refusal when CheckTx
# answers, which can be after the packet landed.
redundant() {
    grep -h 'packet messages are redundant' "$WORK/hermes.log" "$WORK/hermes2.log" |
        grep -F "send_messages_and_wait_check_tx{chain=$SVM_CHAIN "
}
refusals() { redundant > "$RESULTS/redundant.txt"; }
if ! wait_for 60 "a duplicate relay refused at CheckTx" refusals; then
    tail -40 "$WORK/hermes2.log" >&2
    exit 1
fi
refused=$(grep -c . "$RESULTS/redundant.txt")
echo "  step 6: $refused relays refused at this chain's CheckTx as redundant" | tee -a "$RESULTS/steps.txt"
halt hermes2

# Step 7. Prove a packet at its send height. Pause Hermes for ten blocks, so
# the validator prunes the send height while the relayer node keeps enough
# history for the proof.
kill -STOP "$(cat "$WORK/hermes.pid")"
send "$FULL" "$SVM_CHAIN" operator "$USER_ADDR" "400$VOUCHER" 0.000001usvm
SENT=$SENT_HEIGHT
await_height "the relayer node" "$FULL" $((SENT + 10)) "ten blocks with Hermes held"
key=$(printf 'commitments/ports/transfer/channels/channel-0/sequences/%s' "$SENT_SEQ" | od -An -tx1 | tr -d ' \n')
provable() {
    if "$WORK/nodetool" proven -comet "$1" -path /store/ibc/key -data "0x$key" -height "$SENT" 2>/dev/null; then
        echo yes
    else
        echo no
    fi
}
at_relayer=$(provable "$FULL")
at_validator=$(provable "$VAL")
kill -CONT "$(cat "$WORK/hermes.pid")"
expect "$SIM" "$USER_ADDR" stake $((START - 200)) "step 7: the held packet landed"
proved=$(simd query txs --query "recv_packet.packet_sequence='$SENT_SEQ' AND recv_packet.packet_dst_channel='channel-0'" \
    --node "$SIM" --output json | python3 ibc.py proof-height "$SENT_SEQ")
echo "  step 7: sent at $SENT, proved at $((proved - 1)); height $SENT provable at the relayer node: $at_relayer, at the validator: $at_validator" |
    tee -a "$RESULTS/steps.txt"
if [ "$proved" != $((SENT + 1)) ] || [ "$at_relayer" != yes ] || [ "$at_validator" != no ]; then
    echo "the held packet was not proved at its event height" >&2
    exit 1
fi

# usvm transfers are refused.
HELD=$(balance "$FULL" "$OPERATOR" usvm)
if out=$(send "$FULL" "$SVM_CHAIN" operator "$USER_ADDR" 1usvm 0.000001usvm 2>&1); then
    echo "usvm was sent over the channel" >&2
    exit 1
fi
echo "$out" | grep -q 'usvm transfers are currently disabled' || { echo "$out" >&2; exit 1; }
ESCROW=$(simd query ibc-transfer escrow-address transfer channel-0)
if [ "$(balance "$FULL" "$ESCROW" usvm)" != 0 ] || [ "$(balance "$FULL" "$OPERATOR" usvm)" != "$HELD" ]; then
    die "usvm moved"
fi
echo "  usvm: refused, 'usvm transfers are currently disabled'; escrow holds none" | tee -a "$RESULTS/steps.txt"

cp "$WORK/validators.json" "$RESULTS/validators.json"
{
    echo "at height $(height "$FULL"), KiB"
    for i in 0 1; do
        (cd "$WORK/h$i/data" && du -sk -- *.db | sed "s#^#h$i #")
    done
} | tee "$RESULTS/disk.txt"
{
    echo "rpc/accept/ibc.sh"
    echo
    echo "One svmd validator, one svmd full node with the kv index that Hermes relays"
    echo "through, stock ibc-go v11.2.0 simd and Hermes 1.13.1, on one machine."
    echo "versions.txt is what ran, hermes.toml how Hermes was configured, roles.txt"
    echo "each svmd node's index and retention, steps.txt each step, redundant.txt the"
    echo "duplicate relays this chain refused, and disk.txt each node's disk at the end."
} > "$RESULTS/README.txt"
echo "results in $RESULTS"
echo PASS
