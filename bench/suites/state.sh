# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# The shipped store against the number of accounts.
about="The store against account count, 1M to 10M, and a 10M export"
disk=1
needs=(storage)

cells() {
    local n
    for n in 1000000 2000000 5000000 10000000; do
        store_cell "iavl-scale-$n" -exp cache -preload "$n" -caches 1000000,4000000
    done
    store_cell iavl-export-10000000 -exp export -preload 10000000 -caches 4000000
}
