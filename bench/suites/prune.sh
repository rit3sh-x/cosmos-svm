# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# Measure pruning against cache, scale, backend, traffic and the three store
# knobs. Each cell measures 500 blocks of uniform traffic, unless its name says
# otherwise. The most important cells come first. The series CSVs show if a
# cell reached steady state.
about="Pruning against cache, scale, backend, traffic and store knobs"
disk=1
needs=(storage)

cells() {
    local p=(-exp prune -rounds 500 -patterns uniform) big=(-preload 10000000 -caches 4000000)
    store_cell gl-10M-4M "${p[@]}" "${big[@]}" -keeps 0,2
    store_cell gl-10M-4M-sync "${p[@]}" "${big[@]}" -keeps 2 -sync-prune
    store_cell gl-1M-1M "${p[@]}" -preload 1000000 -caches 1000000 -keeps 0,2
    store_cell gl-1M-1M-sync "${p[@]}" -preload 1000000 -caches 1000000 -keeps 2 -sync-prune
    store_cell gl-1M-4M "${p[@]}" -preload 1000000 -caches 4000000 -keeps 0,2
    store_cell gl-10M-1M "${p[@]}" -preload 10000000 -caches 1000000 -keeps 0,2
    store_cell pb-10M-4M "${p[@]}" -backend pebble "${big[@]}" -keeps 0,2
    store_cell pb-1M-1M "${p[@]}" -backend pebble -preload 1000000 -caches 1000000 -keeps 0,2
    store_cell gl-10M-4M-hot -exp prune -rounds 500 -patterns hot-1pct "${big[@]}" -keeps 0,2
    store_cell gl-10M-4M-nofast "${p[@]}" "${big[@]}" -keeps 0,2 -no-fastnode
    store_cell gl-10M-4M-int1 "${p[@]}" "${big[@]}" -keeps 2 -prune-interval 1
    store_cell gl-10M-4M-int100 "${p[@]}" "${big[@]}" -keeps 2 -prune-interval 100
}
