# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# Run every harness and scenario at toy sizes, in minutes. The suite checks
# that the setup builds and runs, not what anything costs. Its numbers mean
# nothing.
about="Every harness and scenario at toy sizes, to check the setup; the numbers mean nothing"
needs=(storage blockcost svm ffi node)

cells() {
    local toy=(-preload 20000 -batch 1000 -warmup 2)
    store_cell iavl-cache "${toy[@]}" -exp cache -rounds 10 -caches 5000,50000
    store_cell iavl-prune "${toy[@]}" -exp prune -rounds 40 -caches 20000 -keeps 0,2,15 -patterns uniform
    store_cell iavl-export "${toy[@]}" -exp export -rounds 10 -caches 20000
    store_cell iavl-alloc -exp alloc -preload 20000 -warmup 1 -rounds 3 -caches 20000 -allocs 4x262144,2000x165
    store_cell iavl-shard "${toy[@]}" -exp shard -rounds 10 -caches 40000 -keeps 2 -prune-interval 1 \
        -patterns uniform -shard-counts 1,2,4
    store_cell iavl-read "${toy[@]}" -exp read -rounds 5 -caches 40000 -keeps 2 -prune-interval 1 -read-names 2000
    cell iavl-single - "$BIN/storagebench" -mode single -preload 20000
    cell iavl-sharded - "$BIN/storagebench" -mode shard -preload 20000 -rounds 2

    local bc=(-accounts 20000 -names 2000 -writes 1000 -warmup 1 -rounds 3)
    blockcost_cell blockcost - "${bc[@]}"
    blockcost_cell blockcost-index - "${bc[@]}" -txs 100 -index -new 10 -new-bytes 1000
    blockcost_cell blockcost-8g 8G "${bc[@]}"

    cell svm - bench/svm/target/release/svm-bench
    cell svm-criterion - cargo test --release --locked --manifest-path bench/svm/Cargo.toml --benches
    cell ffi-bench - go test ./ffi -run '^$' -bench 'PlanPhase|BlockRoundTrip' -benchtime 2x

    cell validator-memory - env CAP=20G ROUNDS=1 IDLE=5 COMMIT= bench/scenarios/validator-memory.sh
    cell proof-window - env COMMIT=100ms bench/scenarios/proof-window.sh
}
