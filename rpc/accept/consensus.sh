#!/usr/bin/env bash
# Runs four validators and a fifth machine, the joiner, through restarts,
# state sync, kills, full blocks, the arena ceiling, slashing and a
# double-sign. Each machine is a real svmd process, set up with `init` and run
# with `start`, as an operator runs one.
#
#   make accept-consensus
#
# The baseline under every assertion: every machine reports the same app hash
# at every height it reports. Beyond that are seven assertions. Each fails for
# a different reason, so a failure names its own cause.
#
#   1. a validator restarted mid-run catches up and keeps agreeing
#   2. a fifth machine joins by state sync and agrees from there
#   3. one of four killed leaves the chain producing blocks, and it rejoins
#   4. a block at the configured block.max_bytes is committed
#   5. blocks naming more than the 512 MiB arena ceiling are cut, every
#      transaction in them pays, and the four agree
#   6. the fourth validator, killed after x/slashing's 500-block window, is
#      jailed out of the set, and its MsgUnjail brings it back
#   7. its key signing on two machines at once is tombstoned, and no bond or
#      supply is burned
#
# Before the seven, svmd start must refuse a genesis hash mismatch, and the
# harness checks the leader schedule of every machine against the proposer of
# each block decided in round 0. After the joiner, a QUIC transfer lands
# through a non-proposer, through the joiner and across a leader change. With
# SVM_CONSENSUS_TRAFFIC=1, the rpc.sh workload runs against machine 0 before
# assertion 4, and the four must agree on every block it produced.
#
# Flip mode (SVM_CONSENSUS_FLIP=1) runs on one box. After the baseline it runs
# a governance gate flip in place of the seven:
#   - A V1 transfer lands, and the RPC answers V1 as Agave does (V1 is on from
#     genesis).
#   - Maximum-size V1 transactions fill the pool to its byte cap, and the pool
#     holds there.
#   - A schedule inside the 432,000-block floor fails, then x/gov schedules the
#     2,575 rent cut.
#   - The chain runs to the flip height, and the rent-exempt minimum changes
#     at it.
#   - The four agree across the flip.
# Because of those 432,000 blocks, the four commit as soon as every precommit
# is in after the V1 checks. That pacing is not the default.
#
# On loopback, assertion 4 asserts only that such a block can be assembled,
# validated and committed. Four processes on one box share a kernel and a
# loopback interface, so nothing here tests propagation. Only machines on a
# real network test the propagation budget that block.max_bytes comes from.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
FLIP=${SVM_CONSENSUS_FLIP:-}
KIND=consensus
[ -z "$FLIP" ] || KIND=flip
RESULTS=${SVM_CONSENSUS_RESULTS:-$ROOT/rpc/accept/results/$KIND-$(date -u +%Y%m%dT%H%M%SZ)}
CHAIN=svm-consensus
# Four validators, and a fifth home that is the joiner.
N=4
JOINER=4
# Where the machines run, as topology in cluster.sh reads it. Unset means five
# processes on this box. Five addresses means five machines.
HOSTS=${SVM_CONSENSUS_HOSTS:-}
REMOTE=${SVM_CONSENSUS_DIR:-/var/tmp/svm-consensus}
SSH="ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
# The pool's byte cap at the harness pool size of 20,000. It is the reap window
# of one block at 100 validators (8,373,477), less 3 bytes of framing for each
# transaction the pool can hold. svmd refuses to start one byte over it.
POOL_BYTES=8313477
# timeout_commit in seconds. Separate machines keep the shipped 400 ms, the
# pacing that the propagation budget is tested against. Five processes on one
# box run at 4 s. The pool holds one block, so a block fills only if CheckTx
# admits about 6,520 transactions between two reaps. At 400 ms no attempt on
# one box fills a block, at 2 s about one attempt in six does, and at 4 s every
# attempt does. The slashing window is 500 blocks, so at 4 s the one-box run
# waits about 33 minutes for the window and about 17 minutes for the jail.
# SVM_CONSENSUS_COMMIT_S overrides either value. The networked run at 2 s is
# how a block at block.max_bytes crosses the WAN. At 400 ms, four 2-vCPU
# machines admit about 1,400 transactions between two blocks, and no block
# fills.
COMMIT_S=${SVM_CONSENSUS_COMMIT_S:-0.4}
[ -n "$HOSTS${SVM_CONSENSUS_COMMIT_S:-}" ] || COMMIT_S=4
# shellcheck source=rpc/accept/lib.sh
. ./lib.sh
# shellcheck source=rpc/accept/cluster.sh
. ./cluster.sh
# shellcheck source=rpc/accept/flip.sh
. ./flip.sh
if [ -n "$FLIP" ] && [ -n "$HOSTS" ]; then
    die "flip mode runs on one box: unset SVM_CONSENSUS_HOSTS"
fi
topology
trap stop EXIT

mkdir -p "$RESULTS"
# Build the binaries here, unless SVM_CONSENSUS_BIN holds prebuilt ones.
if [ -n "${SVM_CONSENSUS_BIN:-}" ]; then
    for bin in svmd fill nodetool svm-tpu quic-client; do cp "$SVM_CONSENSUS_BIN/$bin" "$WORK/"; done
else
    build svmd fill nodetool svm-tpu quic-client
fi
make_genesis
make_configs
distribute

if on 0 "timeout 60 ./svmd start --home h0 --expected-genesis-hash 11111111111111111111111111111111 > mismatch.log 2>&1" ||
    ! on 0 "grep -q 'genesis hash mismatch: actual=$GENESIS_HASH, expected=1111' mismatch.log"; then
    die "start accepted a genesis it did not expect"
fi
echo "expected-genesis-hash: a mismatch refuses by name"

for i in "${VALIDATORS[@]}"; do start "$i"; done
for i in "${VALIDATORS[@]}"; do await "$i" 5 "the chain is producing blocks"; done
agree 1 5 "baseline" "${VALIDATORS[@]}"

cmts=() svms=()
for i in "${VALIDATORS[@]}"; do cmts+=("$(cmt "$i")") svms+=("$(svm "$i")"); done
python3 consensus.py leaders 4 20 "${cmts[@]}" -- "${svms[@]}" | sed 's/^/  /' | tee "$RESULTS/leaders.txt"

if [ -n "$FLIP" ]; then
    flip_mode
    echo "results in $RESULTS"
    echo PASS
    exit 0
fi

# A restart mid-run uses SIGTERM, not SIGKILL. This is the recovery path that
# an operator takes. On the way back it exercises the CometBFT handshake, the
# write-ahead log and LoadStateFromDBOrGenesisDocProvider.
BEFORE=$(tip 1)
signal 1 TERM
echo "restart: machine 1 stopped at height $BEFORE"
await 0 $((BEFORE + 3)) "the other three keep committing"
start 1
await 1 $((BEFORE + 6)) "machine 1 caught up from its own home"
agree 1 $((BEFORE + 6)) "after a restart" "${VALIDATORS[@]}"

# A joiner state syncs. State sync is the only supported way for a machine to
# join, so the harness tests it over the wire.
await 0 25 "a snapshot exists to join from"
read -r TRUST_HEIGHT TRUST_HASH < <(python3 consensus.py trust "$(cmt 0)" 12)

"$WORK/nodetool" set "$(home $JOINER)/config/config.toml" statesync.enable=true \
    statesync.rpc_servers="\"$(cmt 0),$(cmt 1)\"" statesync.trust_height="$TRUST_HEIGHT" \
    statesync.trust_hash="\"$TRUST_HASH\""
# The harness writes the joiner's config now, when the chain has a trust
# point. This is after the distribution, so the file must be sent again.
send_file "h$JOINER/config/config.toml" $JOINER
echo "joiner: state syncing from $TRUST_HEIGHT ($TRUST_HASH)"
start $JOINER
await $JOINER $((TRUST_HEIGHT + 20)) "the joiner reached the chain" 480
agree "$((TRUST_HEIGHT + 20))" 100000 "after a joiner" $(seq 0 $JOINER)
# The joiner must join by state sync, not by replay. A replayed machine holds block 1.
read -r EARLIEST _ < <(python3 consensus.py window "$(cmt $JOINER)")
if [ "$EARLIEST" -le 1 ]; then
    echo "the joiner holds block $EARLIEST, so it replayed rather than state synced" >&2
    exit 1
fi
echo "  the joiner's earliest block is $EARLIEST, so it restored a snapshot"
# The joiner must also execute SVM transactions without a restart. CometBFT
# restores the snapshot into the running app and goes straight to block sync.
# Those blocks carry no SVM transaction, so the agreement above proves nothing
# about the engine.
#
# A joiner with no configured engine answers CheckTx with an error, which
# CometBFT turns into a dropped peer, and halts at the first block that carries
# an SVM transaction. A small fill goes to the RPC of the joiner and of
# machine 0. The joiner must execute and agree on those blocks before anything
# restarts it.
FROM=$(tip 0)
"$WORK/fill" -url "$(svm 0),$(svm $JOINER)" -bytes 200000 -over 1 | tee "$RESULTS/joiner-fill.txt"
await 0 $((FROM + 4)) "the fill through the joiner committed" 120
TO=$(tip 0)
read -r _ _ _ _ JOINER_TXS < <(python3 consensus.py biggest "$(cmt 0)" "$FROM" "$TO" "$COMMIT_S")
if [ "${JOINER_TXS:-0}" -eq 0 ]; then
    echo "no block from $FROM to $TO carried the fill sent through the joiner" >&2
    exit 1
fi
await $JOINER "$TO" "the joiner executed blocks carrying SVM transactions" 240
agree "$FROM" "$TO" "after SVM traffic through a joiner" $(seq 0 $JOINER)
echo "  the joiner executed and agreed on $FROM-$TO, the largest block carrying $JOINER_TXS transactions"

# QUIC through any machine, and across a leader change. Every machine
# advertises its own QUIC port as the ingress of every leader, so a transfer
# sent there must land whoever proposes: through a validator that is not the
# next proposer, through the joiner, which proposes nothing, and to an address
# read before the leader changed. Each check waits for the signature to
# finalize, never for receipt.
rpcq() { curl -sf -H 'content-type: application/json' -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":$3}" "$(svm "$1")"; }
NEXT=$(rpcq 0 getSlotLeaders "[$(($(tip 0) + 1)), 1]" | json '["result"][0]')
NONPROPOSER=""
for i in "${VALIDATORS[@]}"; do
    if [ "$(rpcq "$i" getIdentity '[]' | json '["result"]["identity"]')" != "$NEXT" ]; then
        NONPROPOSER=$i
        break
    fi
done
[ -n "$NONPROPOSER" ] || { echo "every validator is the next proposer" >&2; exit 1; }
"$WORK/fill" -mode quic -url "$(svm "$NONPROPOSER")" -client "$WORK/quic-client" -nonce 1 | sed "s/^/  machine $NONPROPOSER, not the next proposer: /" | tee "$RESULTS/quic.txt"
"$WORK/fill" -mode quic -url "$(svm $JOINER)" -client "$WORK/quic-client" -nonce 2 | sed 's/^/  the joiner, which proposes nothing: /' | tee -a "$RESULTS/quic.txt"
"$WORK/fill" -mode quic -url "$(svm 0)" -client "$WORK/quic-client" -nonce 3 -after 3 | sed 's/^/  across a leader change: /' | tee -a "$RESULTS/quic.txt"
agree 1 "$(tip 0)" "after QUIC sends" $(seq 0 $JOINER)

# One of four killed, and back. SIGKILL gives no clean shutdown and no chance
# to write anything. Three of four is above two thirds, so the chain must keep
# committing. No validator has been in the set for the 500-block window of
# x/slashing yet, so nothing is jailed, and this proves liveness.
BEFORE=$(tip 0)
signal 2 KILL
echo "kill: machine 2 killed at height $BEFORE"
await 0 $((BEFORE + 4)) "three of four keep committing"
start 2
await 2 $((BEFORE + 8)) "the killed machine rejoined"
agree 1 $((BEFORE + 8)) "after a kill" "${VALIDATORS[@]}"

# Contracts, end to end. The whole accept-rpc workload runs against machine 0
# of the cluster: the program deploys including a 10 MB one, SPL Token and
# Token-2022 traffic including confidential transfers, the four subscriptions,
# the websocket limits and getProgramAccounts. Against one machine that
# workload proves the RPC. Here it also proves that four machines that execute
# real programs agree about every block those programs produced. The baseline
# then checks exactly those heights.
if [ "${SVM_CONSENSUS_TRAFFIC:-0}" = 1 ]; then
    FROM=$(tip 0)
    "$WORK/fill" -mode keypair -out "$WORK/traffic.json"
    SVM_ACCEPT_URL=$(svm 0) \
    SVM_ACCEPT_WS="ws://$(addr 0):$(($(svmPort 0) + 1))" \
    SVM_ACCEPT_METRICS="http://$(addr 0):$((26660 + $(step 0)))/metrics" \
    SVM_ACCEPT_PAYER="$WORK/traffic.json" \
    SVM_ACCEPT_GENESIS="$(home 0)/config/genesis.json" \
        ./rpc.sh 2>&1 | tee "$RESULTS/traffic.txt"
    TO=$(tip 0)
    # Wait for the four validators, not the joiner. The joiner is a follower,
    # and on separate machines it shares machine 0's host with a validator,
    # the solana tooling and a 10 MB deploy.
    for i in $(seq 1 $((N - 1))); do await "$i" "$TO" "received the contract traffic" 600; done
    agree "$FROM" "$TO" "after real contract traffic" "${VALIDATORS[@]}"
fi

# A block at block.max_bytes. The harness reads the limit from the chain and
# does not restate it.
read -r MAX_BYTES MAX_GAS < <(python3 consensus.py params "$(cmt 0)")
echo "byte limit: block.max_bytes is $MAX_BYTES, block.max_gas is $MAX_GAS"
URLS=$(for i in "${VALIDATORS[@]}"; do printf '%s,' "$(svm "$i")"; done)
FIRST=$(tip 0)
BEST=0
rm -f "$RESULTS/fill.txt"
# short prints how far a block's transactions, less 3 bytes of framing each,
# fall short of the pool's byte cap. The pool fits one reap by construction.
# The biggest block a machine can build is its whole pool, about 55 kB
# under block.max_bytes, not one transaction under it.
short() { echo $((POOL_BYTES - ($1 - 3 * $2))); }
# Send back to back until a block reaches the pool's cap. The pool holds one
# block, so a block carries what CheckTx admitted since the last reap. The
# block is full only when one interval of sending admitted a whole pool. A
# pause between sends leaves this to the phase of the sends against the blocks.
# With no pause every interval is a whole one, and the run waits for the block
# it asserts. The deadline only bounds a failure.
best() {
    read -r TOP SIZE INTERVAL WIRE TXS < <(python3 consensus.py biggest "$(cmt 0)" "$FIRST" "$(tip 0)" "$COMMIT_S")
    if [ "$SIZE" -gt "$BEST" ]; then
        BEST=$SIZE BEST_TOP=$TOP BEST_TXS=$TXS BEST_INTERVAL=$INTERVAL BEST_WIRE=$WIRE
    fi
}
full() { [ "$BEST" -gt 0 ] && [ "$(short "$BEST" "$BEST_TXS")" -lt 4096 ]; }
SENDS=0
DEADLINE=$((SECONDS + 600))
until full || [ "$SECONDS" -ge "$DEADLINE" ]; do
    SENDS=$((SENDS + 1))
    "$WORK/fill" -url "${URLS%,}" -bytes "$MAX_BYTES" -over "${SVM_FILL_OVER:-2.5}" |
        tee -a "$RESULTS/fill.txt"
    best
    echo "  send $SENDS: the largest block so far is height $BEST_TOP, $BEST_TXS transactions in $BEST bytes"
done
# Wait for the blocks that the last send left in flight, and for the block
# after the largest, whose LastCommit names the round of the largest.
await 0 $(($(tip 0) + 2)) "the filled blocks committed" 300
TO=$(tip 0)
best
ROUND=$(python3 consensus.py round "$(cmt 0)" "$BEST_TOP")
BEST_LINE="height $BEST_TOP
block_bytes $BEST of $MAX_BYTES
transactions $BEST_TXS
interval_s $BEST_INTERVAL
less_timeout_commit_s $BEST_WIRE
commit_round $ROUND
gap_bytes $((MAX_BYTES - BEST))
sends $SENDS
machines $((JOINER + 1)) at ${ADDR[*]}"
# Wait for all four validators before the comparison. Then the app hash of the
# full block is compared on every machine that voted for it, not only on the
# machines that kept up.
for i in $(seq 1 $((N - 1))); do await "$i" "$TO" "received the filled blocks" 420; done
agree "$FIRST" "$TO" "after a full block" "${VALIDATORS[@]}"
echo "$BEST_LINE" > "$RESULTS/byte-limit.txt"
# Each validator's signature results over the run so far, which the fill
# dominates. For each handler, it gives the checks that it reused and the
# checks that it made. This is an observation, with no pass or fail.
for i in "${VALIDATORS[@]}"; do
    on "$i" "curl -sf http://127.0.0.1:$((26660 + $(step "$i")))/metrics" |
        awk -v m="$i" '$1 ~ /^svm_signature_results_total[{]/ { split($1, a, "\""); n[a[4], a[2]] = $2; h[a[4]] = 1 }
            END { for (k in h) if (n[k, "hit"] + n[k, "miss"] > 0)
                printf "machine %s %s hits %d misses %d hit_rate %.3f\n", m, k, n[k, "hit"], n[k, "miss"],
                    n[k, "hit"] / (n[k, "hit"] + n[k, "miss"]) }' | tee -a "$RESULTS/signature-results.txt" || true
done
echo "  the full block is height $BEST_TOP: $BEST_TXS transactions, $BEST bytes of $MAX_BYTES," \
     "committed in round $ROUND, $BEST_INTERVAL s after the block before it"
# A block is at the limit when nothing else would fit, and the pool bounds it.
# max_txs_bytes plus the framing fits inside the window that block.max_bytes
# leaves after the header, the commit and the evidence reserve. The gap to the
# pool's cap must be smaller than one more transaction.
if [ "$(short "$BEST" "$BEST_TXS")" -ge 4096 ]; then
    echo "the largest block carried $BEST bytes in $BEST_TXS transactions, $(short "$BEST" "$BEST_TXS") short of" >&2
    echo "the $POOL_BYTES byte pool, which is more than one more transaction, after $SENDS sends in 600 s." >&2
    # Below 2 s of pacing a block is bound by what CheckTx admits between two
    # reaps, not by block.max_bytes, so the shortfall is the measurement and
    # the propagation test is the run at 2 s or more.
    if [ "$(python3 -c "print(int(float('$COMMIT_S') >= 2))")" = 1 ]; then exit 1; fi
    echo "  recorded: at $COMMIT_S s of pacing the block is bound by admission, not by block.max_bytes"
fi
# Round zero is what "committed inside the configured timeout" means without
# a stopwatch. A block that does not reach the other machines and collect
# their votes in time is not committed late. The round ends, and another block
# is proposed. The harness asserts round zero only of a block at the cap. An
# admission-bound block at 400 ms is small. A round that it loses shows what
# the machines had left for consensus, not what block.max_bytes costs to
# propagate.
if [ -n "$HOSTS" ] && [ "$ROUND" != "0" ] && [ "$(short "$BEST" "$BEST_TXS")" -lt 4096 ]; then
    echo "the full block committed in round $ROUND, so a round timed out before it" >&2
    exit 1
fi
[ "$ROUND" = "0" ] || echo "  it took $((ROUND + 1)) rounds, which on one box is a fact about the box"

# Blocks over the arena ceiling: 52 accounts of 10 MiB, then a block of
# transactions that name all of them through one table. Every block that
# carries one loads at most 512 MiB and drops the rest. The four must then
# agree on which accounts dropped and on every fee charged for them.
# The ceiling is sized for the 20 GB validator. The loaded accounts and the
# engine's copies of them hold more than a gigabyte, so on a 4 GB machine that
# also hosts the joiner the kernel kills svmd in this step.
# SVM_CONSENSUS_ARENA=0 skips the step there. The one-box run keeps it.
if [ "${SVM_CONSENSUS_ARENA:-1}" = 1 ]; then
FROM=$(tip 0)
"$WORK/fill" -mode arena -url "${URLS%,}" | tee "$RESULTS/arena.txt"
TO=$(tip 0)
for i in $(seq 1 $((N - 1))); do await "$i" "$TO" "received the arena blocks" 420; done
agree "$FROM" "$TO" "after the arena ceiling" "${VALIDATORS[@]}"
for i in "${VALIDATORS[@]}"; do
    DROPPED=$(on "$i" "curl -sf http://127.0.0.1:$((26660 + $(step "$i")))/metrics" |
        awk '$1 == "svm_arena_dropped_total" { printf "%d", $2 }')
    if [ "${DROPPED:-0}" -le 0 ]; then
        echo "machine $i reports svm_arena_dropped_total ${DROPPED:-absent}, so no block reached the ceiling" >&2
        exit 1
    fi
    echo "  machine $i dropped $DROPPED keys at the ceiling" | tee -a "$RESULTS/arena.txt"
done
else
echo "  the arena step is skipped (SVM_CONSENSUS_ARENA=0): these machines have too little memory for it" | tee "$RESULTS/arena.txt"
fi

# A validator that stops is jailed. When every validator has been in the set
# for the 500-block window, the harness kills the fourth. x/slashing jails it
# when it has missed 251 of the last 500 blocks, at the 252nd block after the
# last one it signed. CometBFT applies that block's validator update two
# blocks later, so the validator leaves the set 254 blocks after it last
# signed, or sooner if it had already missed some.
#
# Its operator's MsgUnjail, signed with the consensus key that the derived
# account answers to, brings it back.
await 0 500 "every validator has been in the set for the slashing window" 3000
V3=$(python3 consensus.py address "$(cmt 3)")
BOND=$(json '["validators"][3]["self_bond"]' <"$WORK/validators.json")
BEFORE=$(tip 0)
signal 3 KILL
echo "jail: machine 3 killed at height $BEFORE"
read -r SIGNED GONE < <(python3 consensus.py jailed "$(cmt 0)" "$V3" "$BEFORE" 1800)
echo "  machine 3 last signed height $SIGNED and is out of the set from height $GONE" | tee "$RESULTS/slashing.txt"
if [ $((GONE - SIGNED)) -gt 254 ]; then
    echo "machine 3 left the set $((GONE - SIGNED)) blocks after it last signed, which is over 254" >&2
    exit 1
fi
read -r TOMBSTONED JAILED TOKENS SUPPLY < <("$WORK/fill" -mode validator -home "$(home 3)" -node "$(cmt 0)")
if [ "$JAILED $TOMBSTONED $TOKENS" != "true false $BOND" ]; then
    echo "after the jail machine 3 is jailed $JAILED, tombstoned $TOMBSTONED, bonded $TOKENS of $BOND" >&2
    exit 1
fi
start 3
await 3 "$(tip 0)" "the jailed machine caught up" 600
FROM=$(tip 0)
"$WORK/fill" -mode unjail -home "$(home 3)" -node "$(cmt 0)" -chain "$CHAIN"
read -r BACK SIGNS < <(python3 consensus.py returns "$(cmt 0)" "$V3" "$FROM" 600)
echo "  machine 3 is in the set again from height $BACK and signed height $SIGNS, bonded $TOKENS" |
    tee -a "$RESULTS/slashing.txt"
agree "$BEFORE" "$SIGNS" "after a jail" "${VALIDATORS[@]}"

# A double-signer is tombstoned. The joiner restarts holding machine 3's
# consensus key, so one validator signs from two processes. At its turn each
# proposes its own block and prevotes it, and the other validators report the
# pair as DuplicateVoteEvidence. x/evidence tombstones the key. Both slash
# fractions are zero, so the bond and the supply do not change.
read -r _ _ TOKENS SUPPLY < <("$WORK/fill" -mode validator -home "$(home 3)" -node "$(cmt 0)")
signal "$JOINER" TERM
cp "$(home 3)/config/priv_validator_key.json" "$(home $JOINER)/config/priv_validator_key.json"
send_file "h$JOINER/config/priv_validator_key.json" $JOINER
FROM=$(tip 0)
start "$JOINER"
GONE=$(python3 consensus.py gone "$(cmt 0)" "$V3" "$FROM" 900)
read -r TOMBSTONED JAILED AFTER AFTER_SUPPLY < <("$WORK/fill" -mode validator -home "$(home 3)" -node "$(cmt 0)")
echo "  machine 3's key signed twice and is out of the set from height $GONE: tombstoned $TOMBSTONED," \
     "bonded $AFTER of $TOKENS, supply $AFTER_SUPPLY of $SUPPLY" | tee -a "$RESULTS/slashing.txt"
if [ "$TOMBSTONED $JAILED $AFTER $AFTER_SUPPLY" != "true true $TOKENS $SUPPLY" ]; then
    echo "the double-signer is not tombstoned, or something was burned" >&2
    exit 1
fi
await "$JOINER" "$(tip 0)" "the second signer caught up" 600

# The whole run: every machine over every height it holds, the joiner included.
agree 1 100000 "the whole run" $(seq 0 $JOINER)
cp "$WORK/validators.json" "$RESULTS/validators.json"
cat >"$RESULTS/README.txt" <<EOF
rpc/accept/consensus.sh.

$((JOINER + 1)) machines at ${ADDR[*]}, chain id $CHAIN, genesis $GENESIS_HASH.
A restart, a joiner by state sync, a kill, a block at block.max_bytes,
blocks over the arena ceiling, a jail and an unjail, and a double-sign, with
every machine's app hash compared at every common height after each.
Real contract traffic: ${SVM_CONSENSUS_TRAFFIC:-0} (rpc/accept/rpc.sh against machine 0).

byte-limit.txt is the full block. fill.txt is what was sent to produce it.
arena.txt is the arena attack and what each validator dropped.
slashing.txt is the jail, the unjail and the tombstone.
traffic.txt is the contract workload, when it ran. The harness deviates from
the shipped config in four places, each named in the script: the snapshot
interval, the mempool's size and byte cap, the program cache, and on one box
timeout_commit, which is $COMMIT_S s in this run.
EOF
echo "results in $RESULTS"
echo PASS
