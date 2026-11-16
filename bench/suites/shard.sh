# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# 50 warmup blocks fill the cache, at about 110,000 nodes a block.
about="Sharding at the shipped configuration"
disk=1
needs=(storage)

cells() {
    local n
    for n in 1 2 4 8; do
        store_cell "gl-10M-4M-int1-k$n" -exp shard -preload 10000000 -caches 4000000 -keeps 2 -prune-interval 1 \
            -patterns uniform -warmup 50 -rounds 150 -shard-counts "$n"
    done
}
