# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# The scenario is bench/scenarios/validator-memory.sh. bench/bench.sh report
# turns its samples and heap profiles into the memory budget.
about="What a validator's memory is spent on, under the published cap"
disk=1
needs=(node)
knob CAP 20G "the validator's MemoryMax, the published minimum"
knob ROUNDS 12 "fill rounds of 2.5 blocks at block.max_bytes"
knob IDLE 60 "seconds idle before the fill and after it"
knob COMMIT '' "timeout_commit if set, so the pool reaches its byte cap before each proposal"
knob SVMD_ENV '' "VAR=VALUE words for svmd alone: GOGC=50 GOMEMLIMIT=6GiB, MALLOC_ARENA_MAX=8, or LD_PRELOAD=libjemalloc.so.2 MALLOC_CONF=prof:true,... for an allocator profile"

cells() {
    cell validator-memory - env CAP="$CAP" ROUNDS="$ROUNDS" IDLE="$IDLE" COMMIT="$COMMIT" SVMD_ENV="$SVMD_ENV" \
        bench/scenarios/validator-memory.sh
}
