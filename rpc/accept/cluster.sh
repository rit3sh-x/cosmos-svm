# shellcheck shell=bash
# The machines that rpc/accept/consensus.sh runs, and what it asks of them:
# where each one lives and on which ports, the genesis and the configs they
# share, how to start and stop one, its height, and whether they agree. Source
# it after rpc/accept/lib.sh. It sets no options and runs nothing. consensus.sh
# sets N, JOINER, CHAIN, HOSTS, REMOTE, SSH, FLIP, POOL_BYTES and COMMIT_S, and
# calls topology first. selftest.sh sources it only for agree.
topology() {
    local i
    if [ -n "$HOSTS" ]; then
        IFS=',' read -r -a ADDR <<<"$HOSTS"
        [ "${#ADDR[@]}" -eq $((JOINER + 1)) ] || die "SVM_CONSENSUS_HOSTS needs $((JOINER + 1)) addresses"
        BIND=0.0.0.0
    else
        ADDR=()
        for i in $(seq 0 "$JOINER"); do ADDR+=(127.0.0.1); done
        BIND=127.0.0.1
    fi
    # Machines that share an address need a port set each, so the ports depend
    # on the address, not on the topology. An address list with no repeats is
    # five machines on the default ports. Five copies of 127.0.0.1 is the
    # loopback form. A list that repeats one address puts the joiner beside a
    # validator, so a run fits inside a small cloud vCPU quota.
    SHARED=0
    for i in $(seq 0 "$JOINER"); do [ "$(step "$i")" = 0 ] || SHARED=1; done
    mapfile -t VALIDATORS < <(seq 0 $((N - 1)))
}

addr() { echo "${ADDR[$1]}"; }

# step I is machine I's port offset: 100 for each machine before it on the
# same address.
step() {
    local i=$1 n=0 j
    for j in $(seq 0 "$i"); do
        [ "$j" -lt "$i" ] && [ "$(addr "$j")" = "$(addr "$i")" ] && n=$((n + 100))
    done
    echo "$n"
}
cmtPort() { echo $((26657 + $(step "$1"))); }
p2pPort() { echo $((26656 + $(step "$1"))); }
svmPort() { echo $((8899 + $(step "$1") / 10)); }

home() { echo "$WORK/h$1"; }
cmt() { echo "http://$(addr "$1"):$(cmtPort "$1")"; }
svm() { echo "http://$(addr "$1"):$(svmPort "$1")"; }

# on runs a command where machine i lives.
on() {
    local i=$1
    shift
    if [ -n "$HOSTS" ]; then
        $SSH "$(addr "$i")" "cd $REMOTE && $*"
    else
        (cd "$WORK" && eval "$*")
    fi
}

# svmd's own Go memory limit is a 14 GiB backstop, sized for the 20 GB
# validator. The harness's machines are 4 GiB, and the one-box form shares a
# developer's box with four other processes. Without a limit of its own, a
# process grows until the kernel kills it. The networked run's driver lost its
# validator that way in the arena step at 400 ms. There, five times the blocks
# a minute made five times the garbage.
GOMEMLIMIT=${SVM_CONSENSUS_GOMEMLIMIT:-1GiB}

# start runs one machine as an operator does, with the expected genesis hash.
# It uses setsid, because over ssh a background process that keeps the
# session's terminal keeps the ssh open. A second argument names another
# binary in the work directory, and start adds the rest to the start command.
# It runs through on, on a machine that may hold no copy of rpc/accept, so it
# does not use spawn.
start() {
    local i=$1 bin=${2:-svmd}
    shift
    [ $# -eq 0 ] || shift
    on "$i" "(GOMEMLIMIT=$GOMEMLIMIT setsid nohup ./$bin start --home h$i" \
        "--expected-genesis-hash $GENESIS_HASH" \
        "--svm.rpc-address $BIND:$(svmPort "$i")" \
        "--svm.ws-address $BIND:$(($(svmPort "$i") + 1))" "$@" \
        ">> p$i.log 2>&1 < /dev/null & echo \$! > p$i.pid)"
}

# signal I SIG sends SIG to machine I and waits for the process to exit, so a
# restart never races for the port.
signal() {
    local i=$1 sig=$2
    on "$i" "kill -$sig \$(cat p$i.pid) && while kill -0 \$(cat p$i.pid) 2>/dev/null; do sleep 0.2; done; rm -f p$i.pid"
}

# stop is the EXIT trap. It stops every machine, and on a failed run it keeps
# every machine's log, the only record of the cause. The kills and the waits
# run through on, so they run where machine i lives. The single quotes are for
# that shell, and scripts/lib.sh is not there.
stop() {
    local status=$? i
    for i in $(seq 0 "$JOINER"); do
        # shellcheck disable=SC2016
        on "$i" 'kill $(cat p'"$i"'.pid 2>/dev/null) 2>/dev/null' || true
    done
    for i in $(seq 0 "$JOINER"); do
        # shellcheck disable=SC2016
        on "$i" 'timeout 120 sh -c "while kill -0 \$(cat p'"$i"'.pid 2>/dev/null) 2>/dev/null; do sleep 0.2; done"' || true
    done
    if [ "$status" != 0 ] && [ -d "$RESULTS" ]; then
        for i in $(seq 0 "$JOINER"); do on "$i" "cat p$i.log" >"$RESULTS/p$i.log" 2>/dev/null || true; done
    fi
    rm -rf "$WORK"
}

# tip I is machine I's latest height, or nothing if it does not answer.
tip() { height "$(cmt "$1")"; }

# await I WANT WHAT [SECONDS] waits for machine I to reach a height, and on a
# stall prints the end of its log and stops the harness.
await() {
    await_height "machine $1" "$(cmt "$1")" "$2" "$3" "${4:-240}" || {
        on "$1" "tail -40 p$1.log" >&2 || true
        exit 1
    }
}

# agree is the baseline check. It runs after every assertion, not once at the
# end, so a divergence points to the event that caused it. The comparison goes
# into a variable, not into echo, because the echo status would hide its
# failure from set -e. selftest.sh runs this function against a divergence.
agree() {
    local lo=$1 hi=$2 what=$3 out i
    shift 3
    local urls=()
    for i in "$@"; do urls+=("$(cmt "$i")"); done
    out=$(python3 consensus.py agree "$lo" "$hi" "${urls[@]}") || {
        echo "$what: the machines do not agree" >&2
        exit 1
    }
    echo "  $what: $out"
}

# make_genesis writes the genesis that every machine runs to WORK/h0, copies it
# to every home, and sets GENESIS_HASH. Every machine runs `init` once for its
# own keys. The coordinator then runs `init` again over the four consensus
# pubkeys it collected.
#
# The coordinator builds all five homes, keys included, and hands them out. A
# real deployment does not do this. There, each operator keeps its consensus
# key and gives only its pubkey. The shortcut changes nothing that the
# assertions touch, and it gives one genesis path for both topologies.
make_genesis() {
    local i
    for i in $(seq 0 "$JOINER"); do
        "$WORK/svmd" init "v$i" --chain-id "$CHAIN" --home "$(home "$i")" >"$WORK/init$i.log" 2>&1
    done
    # The operators are equal, so "one of four" means a quarter of the voting
    # power. With no flags, `init` derives each operator's account from the
    # machine's own consensus key, so the first genesis of each home already
    # holds the account.
    for i in "${VALIDATORS[@]}"; do
        "$WORK/nodetool" validators -out "$WORK/validators.json" -genesis "$(home "$i")/config/genesis.json" \
            -pubkey "$("$WORK/svmd" comet show-validator --home "$(home "$i")")"
    done
    "$WORK/svmd" init coordinator --chain-id "$CHAIN" --home "$(home 0)" --overwrite \
        --validators-file "$WORK/validators.json" >"$WORK/genesis.log" 2>&1
    "$WORK/svmd" add-genesis-programs "$ROOT/genesis" --home "$(home 0)"

    # The fee payers of the byte-limit assertion. Genesis funds them, not
    # transfers, because a block at the byte limit is thousands of
    # transactions, and the payer of each is its only writable account.
    # Genesis also holds the lookup table of the arena assertion, already
    # extended.
    "$WORK/fill" -mode keys -payers 256 >"$WORK/payers.txt"
    "$WORK/fill" -mode arena-genesis >"$WORK/arena.json"
    "$WORK/nodetool" fund -genesis "$(home 0)/config/genesis.json" -payers "$WORK/payers.txt" \
        -account "$WORK/arena.json"
    if [ -n "$FLIP" ]; then
        # The default 24 h voting period is a day of blocks for the harness to
        # wait through. The harness lowers it, so the period under test is not
        # the default.
        "$WORK/nodetool" set "$(home 0)/config/genesis.json" app_state.gov.params.voting_period='"20s"' \
            app_state.gov.params.expedited_voting_period='"10s"'
    fi
    GENESIS_HASH=$(python3 genesis_hash.py "$(home 0)/config/genesis.json")
    for i in $(seq 1 "$JOINER"); do
        cp "$(home 0)/config/genesis.json" "$(home "$i")/config/genesis.json"
    done
    echo "genesis: $N operators, $(grep -c . "$WORK/payers.txt") funded payers, hash $GENESIS_HASH"
}

# make_configs writes the config.toml and app.toml of every machine: a port set
# for each, and the two settings that machines on one address need from
# CometBFT. These are an address book that does not require routable peers,
# and permission for peers to share an address.
make_configs() {
    local i id s peers='' strict=true duplicates=false comet app
    for i in "${VALIDATORS[@]}"; do
        id=$("$WORK/svmd" comet show-node-id --home "$(home "$i")")
        peers="$peers,$id@$(addr "$i"):$(p2pPort "$i")"
    done
    # On separate machines neither setting is needed. With both off, the run
    # takes CometBFT's ordinary path, not a relaxed one.
    [ "$SHARED" = 0 ] || strict=false duplicates=true
    for i in $(seq 0 "$JOINER"); do
        s=$(step "$i")
        # The harness polls the CometBFT RPC for heights, app hashes and the
        # trust pair, so the RPC binds where the harness can reach it.
        comet=(proxy_app="\"tcp://127.0.0.1:$((26658 + s))\"" rpc.laddr="\"tcp://$BIND:$((26657 + s))\""
            p2p.laddr="\"tcp://0.0.0.0:$((26656 + s))\"" pprof_laddr="\"localhost:$((6060 + i))\""
            prometheus_listen_addr="\":$((26660 + s))\"" persistent_peers="\"${peers#,}\""
            addr_book_strict="$strict" allow_duplicate_ip="$duplicates"
            external_address="\"$(addr "$i"):$(p2pPort "$i")\""
            size=20000 max_txs_bytes="$POOL_BYTES")
        [ "$COMMIT_S" = 0.4 ] || comet+=(timeout_commit="\"${COMMIT_S}s\"")
        "$WORK/nodetool" set "$(home "$i")/config/config.toml" "${comet[@]}"

        app=(index-max-bytes=1073741824 grpc.address="\"localhost:$((9090 + i))\""
            program-cache-bytes=268435456 threads=2
            tpu-quic-address="\"$BIND:$((9000 + s))\"" public-tpu-address="\"$(addr "$i"):$((9000 + s))\"")

        if [ -z "$FLIP" ]; then
            if [ "$COMMIT_S" = 0.4 ]; then app+=(snapshot-interval=50); else app+=(snapshot-interval=10); fi
        fi
        "$WORK/nodetool" set "$(home "$i")/config/app.toml" "${app[@]}"
    done
}

# distribute sends every machine the finished genesis, its own home and the
# binaries it runs. On one box there is nothing to send.
distribute() {
    local i
    [ -n "$HOSTS" ] || return 0
    for i in $(seq 0 "$JOINER"); do
        # Clear only the machine's own home. Two machines can share a host, so
        # this must not clear the directory around it. A stale data/ from an
        # earlier run is a different chain, not only a stale file. After the
        # first start, CometBFT reads its genesis doc from the state database,
        # never from genesis.json.
        $SSH "$(addr "$i")" "mkdir -p $REMOTE && rm -rf $REMOTE/h$i $REMOTE/p$i.log $REMOTE/p$i.pid"
        rsync -aq -e "$SSH" "$WORK/svmd" "$WORK/svm-tpu" "$WORK/fill" "$(home "$i")" "$(addr "$i"):$REMOTE/"
    done
    echo "distributed: $((JOINER + 1)) machines at ${ADDR[*]}"
}

# send_file FILE I sends one file under WORK to machine I, where it lives at
# the same path under REMOTE. On one box it is already there.
send_file() {
    [ -n "$HOSTS" ] || return 0
    rsync -aq -e "$SSH" "$WORK/$1" "$(addr "$2"):$REMOTE/$1"
}
