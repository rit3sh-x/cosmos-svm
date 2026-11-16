# shellcheck shell=bash
# Flip mode of rpc/accept/consensus.sh (SVM_CONSENSUS_FLIP=1). It runs a
# governance gate flip in place of the seven assertions. Source it after
# rpc/accept/cluster.sh. It sets no options and runs nothing. consensus.sh
# calls flip_mode when the four machines agree.

# pace rewrites the commit pacing of every machine before a start. fast is
# CometBFT's test pacing: commit as soon as every precommit is in, and gossip
# every 5 ms, not 100 ms. This pacing lets flip mode reach a height 432,000
# blocks out. slow is the harness pacing.
pace() {
    local i commit="\"${COMMIT_S}s\"" skip=false gossip='"100ms"'
    [ "$1" != fast ] || commit='"10ms"' skip=true gossip='"5ms"'
    for i in "${VALIDATORS[@]}"; do
        "$WORK/nodetool" set "$(home "$i")/config/config.toml" timeout_commit="$commit" \
            skip_timeout_commit=$skip peer_gossip_sleep_duration="$gossip"
    done
}

# decide has the four operators vote yes and waits for the proposal to leave
# its voting period. It then prints the status and any failure reason.
decide() {
    local id=$1 i
    for i in "${VALIDATORS[@]}"; do
        "$WORK/fill" -mode vote -home "$(home "$i")" -node "$(cmt 0)" -chain "$CHAIN" -proposal "$id"
    done
    # decide runs inside $(...), where set -e does not apply, so the exit is
    # explicit.
    wait_for 300 "proposal $id to leave its voting period" decided "$id" || exit 1
    "$WORK/fill" -mode proposal -node "$(cmt 0)" -proposal "$id"
}

# decided ID succeeds when the proposal is out of its voting period.
decided() {
    local status
    status=$("$WORK/fill" -mode proposal -node "$(cmt 0)" -proposal "$1") &&
        [ "${status%% *}" != PROPOSAL_STATUS_VOTING_PERIOD ]
}

# restart_at PACE stops the four, rewrites their pacing and starts them again.
restart_at() {
    local i
    for i in "${VALIDATORS[@]}"; do signal "$i" TERM; done
    pace "$1"
    for i in "${VALIDATORS[@]}"; do start "$i"; done
}

# flip_mode runs the governance flip and writes its README. The gate is the
# 2,575 rent cut (SIMD-0437-3), which is off here.
flip_mode() {
    local id status h0 h1 t0 ms rate eta i from filled sent ok full bad peak bytes secs
    # V1 is on from genesis, so its checks come first, at the harness pacing.
    # The pool shows full only while blocks are slower than the sends.
    "$WORK/fill" -mode v1 -url "$(svm 0)" | sed 's/^/  /' | tee "$RESULTS/flip.txt"
    filled=$("$WORK/fill" -mode v1fill -url "$(svm 0)" -node "$(cmt 0)" -cap "$POOL_BYTES")
    read -r sent ok full bad peak bytes secs <<<"$filled"
    echo "  pool: $sent V1 transactions of 4,096 bytes sent in $secs s, $ok answered with a signature, $full samples with no room for another;" \
        "it held at most $peak, $bytes bytes, against a cap of $POOL_BYTES" | tee -a "$RESULTS/flip.txt"
    if [ "$bytes" -gt "$POOL_BYTES" ] || [ "$full" -eq 0 ] || [ "$bad" -ne 0 ]; then
        echo "the pool did not fill to its byte cap and hold there" >&2
        exit 1
    fi

    from=$(tip 0)
    restart_at fast
    for i in "${VALIDATORS[@]}"; do await "$i" $((from + 10)) "at CometBFT's test pacing" 300; done

    "$WORK/fill" -mode rent -phase before -url "$(svm 0)" | sed 's/^/  /' | tee -a "$RESULTS/flip.txt"

    # Governance passes this proposal, and x/svm refuses it when it executes.
    # The floor counts from execution, so a schedule barely inside it fails there.
    id=$("$WORK/fill" -mode propose-gate -home "$(home 0)" -node "$(cmt 0)" -chain "$CHAIN" -height $(($(tip 0) + 100)))
    status=$(decide "$id")
    echo "  proposal $id, the 2,575 rent cut 100 blocks out: $status" | tee -a "$RESULTS/flip.txt"
    case "$status" in
    PROPOSAL_STATUS_FAILED*"the minimum is 432000"*) ;;
    *) echo "a schedule inside the floor did not fail on it" >&2; exit 1 ;;
    esac

    # Schedule the flip 432,000 blocks after the proposal executes, which is one
    # voting period from now. The margin is one minute of blocks at the
    # measured rate. The rate, in blocks a second, comes from 200 blocks and
    # the milliseconds they took.
    h0=$(tip 0)
    t0=$(date +%s%3N)
    await 0 $((h0 + 200)) "200 blocks to measure the pace" 300
    h1=$(tip 0)
    ms=$(($(date +%s%3N) - t0))
    rate=$((((h1 - h0) * 1000 + ms - 1) / ms))
    FLIP_AT=$((h1 + 432000 + 60 * rate + 50))
    id=$("$WORK/fill" -mode propose-gate -home "$(home 0)" -node "$(cmt 0)" -chain "$CHAIN" -height "$FLIP_AT")
    status=$(decide "$id")
    eta=$(((FLIP_AT - $(tip 0)) / (rate > 0 ? rate : 1) / 60))
    echo "  proposal $id, the 2,575 rent cut at $FLIP_AT: $status; about $rate blocks a second, about $eta minutes to the flip" |
        tee -a "$RESULTS/flip.txt"
    [ "${status%% *}" = PROPOSAL_STATUS_PASSED ] || { echo "the flip was not scheduled" >&2; exit 1; }

    # Wait until ten seconds of blocks before the flip, so the check lands
    # before the flip at this pace.
    await 0 $((FLIP_AT - 10 * rate - 20)) "ten seconds before the flip" $((eta * 60 * 3 + 3600))
    "$WORK/fill" -mode rent -phase before -url "$(svm 0)" | sed "s/^/  at $(tip 0): /" | tee -a "$RESULTS/flip.txt"
    for i in "${VALIDATORS[@]}"; do await "$i" $((FLIP_AT + 5)) "past the flip" 600; done
    "$WORK/fill" -mode rent -phase after -url "$(svm 0)" | sed 's/^/  /' | tee -a "$RESULTS/flip.txt"
    agree $((FLIP_AT - 50)) 100000 "across the flip" "${VALIDATORS[@]}" | tee -a "$RESULTS/flip.txt"

    cp "$WORK/validators.json" "$RESULTS/validators.json"
    cat >"$RESULTS/README.txt" <<EOF
rpc/accept/consensus.sh flip mode.

$N machines on one box, chain id $CHAIN, genesis $GENESIS_HASH. flip.txt is a
V1 transfer and the pool filled with V1 transactions, which are on from genesis,
then the flip of the 2,575 rent cut, scheduled through x/gov at $FLIP_AT once a
schedule inside the floor had failed, and what the chain answered on either
side of it. The V1 checks ran at the harness's pacing, and from there on the
four ran at CometBFT's test pacing: commit as soon as every precommit is in,
and gossip every 5 ms. The harness lowers the voting period to 20 s and
timeout_commit to $COMMIT_S s.
EOF
}
