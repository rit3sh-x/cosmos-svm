# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# Measures account loads on the FinalizeBlock store path.
# Each cell times PushAccounts through rootmulti and CacheMultiStore with
# sorted account keys. The read suite times IAVL's Get in random order instead.
# The 143,198-name workload matches the block-cost model.
about="An account load on the app's own store stack"
disk=1
needs=(blockcost storage)

# The -label values match the recorded reference run, so
# `bench/bench.sh compare` pairs a new run with it.
cells() {
    blockcost_cell p2-hit - -names 143198 -mode hit -label hit
    blockcost_cell p2-miss - -names 143198 -mode miss -label miss
    blockcost_cell p2-hit-gap2s - -names 143198 -mode hit -gap 2s -label hit-gap2s

    # 16 GiB cap, including 4 GiB reserved for the program cache.
    blockcost_cell p2-hit-16g 16G -names 143198 -mode hit -label hit-16g

    # The IAVL read baseline, to separate host effects from store-path costs.
    store_cell iavl-read-fast-hit -exp read -preload 10000000 -caches 4000000 -keeps 2 -prune-interval 1 \
        -patterns uniform -read-names 200000 -warmup 40 -rounds 60 -read-cells fast-hit
}
