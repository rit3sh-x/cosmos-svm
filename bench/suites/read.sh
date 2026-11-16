# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# Measure one account lookup at the shipped store. It is the only cost in the
# harness that is not commit-shaped. The cell reads hits and misses, through
# the fast index that the chain ships and through the tree descent that
# app.toml's iavl-disable-fastnode falls back to. 40 warmup blocks fill the
# caches. 200,000 names is the load list of a full attack block.
about="One account lookup at the shipped store"
disk=1
needs=(storage)

cells() {
    store_cell gl-10M-4M-read -exp read -preload 10000000 -caches 4000000 -keeps 2 -prune-interval 1 \
        -patterns uniform -read-names 200000 -warmup 40 -rounds 60
}
