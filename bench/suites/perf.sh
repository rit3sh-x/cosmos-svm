# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# Measure the node cache curve, pruning, export and allocation on the shipped
# store. Then run the SVM report and the FFI benchmarks on the same machine.
about="The store's cache curve, pruning, export and allocation, with the SVM and FFI"
disk=1
needs=(storage svm ffi)

cells() {
    cell make-test - make test
    store_cell iavl-cache-goleveldb -exp cache -caches 250000,500000,1000000,2000000,4000000,8000000,16000000
    store_cell iavl-cache-pebble -backend pebble -exp cache -caches 1000000,4000000
    store_cell iavl-prune -exp prune -caches 1000000 -keeps 0,2,1000 -rounds 1200 -patterns uniform
    store_cell iavl-export -exp export -caches 1000000
    store_cell iavl-alloc -exp alloc -caches 1000000 -rounds 10 -warmup 2
    cell svm - bench/svm/target/release/svm-bench
    cell ffi-bench - go test ./ffi -run '^$' -bench . -benchtime 20x
}
