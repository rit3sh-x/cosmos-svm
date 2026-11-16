# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# Measures block costs at the published minimum.
# Each cell times the block phases on one host for a workload that fills a
# block limit: account loads, account writes, sidecar writes or allocations.
# A capped cell holds 3 GiB in place of the program cache. Runs before
# 2026-11-07 held 4 GiB.
# `bench/bench.sh report` converts the results to unit prices.
about="Every block limit at the published minimum"
disk=1
needs=(blockcost engine-tests)

# The workload sizes fill MAX_BLOCK_UNITS. The suite reads the prices from
# engine/src/cost.rs, so the sizes follow a price change. LOCK and NAME
# replace WRITE_LOCK_UNITS and NAME_UNITS. LOCK=4000 repeats the sizes of the
# runs made before the write lock went to 4,300.
#
#   write   A written account is also a named one, so it costs LOCK plus
#           NAME. At 1,755 and 245 a block holds 30,000, in 235 transactions
#           of 128 accounts.
#   name    A read-only name costs NAME: 244,897 a block at 245.
#   block   A transfer costs 720 for its signature, two locks, three names and
#           3,000 for the builtin: 7,965 units at 1,755, so 7,532 a block.
units() {
    local v
    v=$(sed -nE "s/^pub const $1: u64 = ([0-9_]+);.*/\1/p" "$ROOT/engine/src/cost.rs")
    [ -n "$v" ] || die "engine/src/cost.rs has no $1"
    echo "${v//_/}"
}
BLOCK_UNITS=$(units MAX_BLOCK_UNITS)
SHIPPED_LOCK=$(units WRITE_LOCK_UNITS)
SHIPPED_NAME=$(units NAME_UNITS)
knob LOCK "$SHIPPED_LOCK" "write lock units the shapes are priced at, WRITE_LOCK_UNITS unless set"
knob NAME "$SHIPPED_NAME" "name units the shapes are priced at, NAME_UNITS unless set"
knob COMMIT serial "serial: one store after another, as every run before 2026-11-02 | app: the shards eight at a time, as the app commits"
knob ROUNDS 40 "measured blocks in each write-shaped cell; a long run shows whether the database settles"
knob EVERY 0 "print a window line every EVERY blocks of a cell, 0 for none"
knob PROFILE "" "a directory each blockcost cell writes its profiles and trace into, under its name; none when empty"
knob HEAP off "on: each blockcost cell writes Go heap profiles into the run under its name, at its first measured block, its middle one and after its last"
knob BALLAST mmap "mmap: a capped cell's 3 GiB ballast is anonymous memory outside the Go heap, where the program cache of the engine is | heap: a Go allocation that raises the GC goal of Go by 4 GiB, as in every run before 2026-11-07"
knob MALLOC svmd "svmd: glibc starts as svmd starts it, MALLOC_ARENA_MAX=4 and MALLOC_TRIM_THRESHOLD_=131072 unless the environment sets them | glibc: the defaults of glibc, as in every run before 2026-11-07"
knob BUFPOOL on "with BACKEND=goleveldb, on: goleveldb's buffer pool as the goleveldb options leave it | off: DisableBufferPool on the application database"
knob BACKEND pebble "pebble: every database of a cell opens as svmd opens it | goleveldb: on the old goleveldb options; each engine builds seeds of its own"
knob OVERLAP off "off: the sidecar writes run in the block | on: beside the shard commit, joined before rootmulti.Commit; needs COMMIT=app"
knob REST 8 "10 MiB accounts that each aged cell plants before its first block and that no block reads, the share of large accounts on mainnet; 0 for none"
knob INTERBLOCK off "off: no inter-block cache, as svmd ships | on: every store wrapped in the inter-block cache of the SDK, as inter-block-cache = true in app.toml wraps the stores of svmd"
W_WRITES=$((BLOCK_UNITS / (LOCK + NAME)))
W_TXS=$(((W_WRITES + 127) / 128))
N_NAMES=$((BLOCK_UNITS / NAME))
N_TXS=$(((N_NAMES + 127) / 128))
B_TXS=$((BLOCK_UNITS / (720 + 2 * LOCK + 3 * NAME + 3000)))
B_WRITES=$((2 * B_TXS)) B_NAMES=$((3 * B_TXS))
A_BIG=9 A_BIG_BYTES=10485760
A_MID=953 A_MID_BYTES=104857

# Lookup tables. A v0 transaction names accounts through tables, up to the
# 1,232-byte message limit. Phase 2 reads each table, one at a time, before the
# load of the block. Each shape below declares a 64 KiB loaded limit (16 units).
# Each also pays 6,000 units for two builtins and 4 units of instruction data.
#   tw  the write shape through one table. A payer and the System and Compute
#       Budget programs are static keys. 125 writable addresses come from one
#       table. A transaction has 126 write locks, 129 names and one table read.
#   tr  the table-read shape. 28 tables of one readonly address each, the most
#       that a 1,232-byte message holds, and one payer write. A transaction has
#       59 names and 28 table reads. Its few payers are not counted as writes.
TW_TXS=$((BLOCK_UNITS / (720 + 126 * LOCK + 129 * NAME + 6000 + 16 + 4)))
TW_WRITES=$((126 * TW_TXS))
TR_TXS=$((BLOCK_UNITS / (720 + LOCK + 59 * NAME + 6000 + 16 + 4)))
TR_TABLES=$((28 * TR_TXS))
# Loaded bytes. The engine's arena holds 512 MiB for a block, and a
# transaction loads at most 64 MiB. So nine transactions of six 10 MiB accounts
# fill it. Each pays the lock of a payer, eight names, two builtins and 16,384
# units for the 64 MiB loaded limit.
#   ld   51 accounts of 10 MiB and nothing else
#   ldw  50 of them, and the rest of the block's units spent on the write shape
LD_TX=$((720 + LOCK + 8 * NAME + 6000 + 16384 + 5))
LW_WRITES=$(((BLOCK_UNITS - 9 * LD_TX) / (LOCK + NAME)))
LW_TXS=$(((LW_WRITES + 127) / 128))

machine() {
    grep -nE '^pub const (MAX_BLOCK_UNITS|WRITE_LOCK_UNITS|NAME_UNITS|MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA|ALLOCATED_BYTES_PER_UNIT)' \
        engine/src/cost.rs
}

cells() {
    # The warmup blocks fill the node cache before the measured rounds. An
    # allocation workload grows the tree by about 100 MB a block, so it runs
    # fewer blocks.
    local each=(-commit "$COMMIT" -report-every "$EVERY")
    [ -z "$PROFILE" ] || each+=(-profile "$PROFILE")
    [ "$HEAP" = off ] || each+=(-heap-profile "$OUT")
    [ "$BALLAST" = heap ] || each+=(-ballast-mmap)
    # blockcost does not link cmd/svmd, whose constructor sets these, so the
    # suite sets them for its cells.
    [ "$MALLOC" = glibc ] || export MALLOC_ARENA_MAX="${MALLOC_ARENA_MAX-4}" MALLOC_TRIM_THRESHOLD_="${MALLOC_TRIM_THRESHOLD_-131072}"
    [ "$BUFPOOL" = on ] || each+=(-no-buffer-pool)
    each+=(-backend "$BACKEND")
    [ "$OVERLAP" = off ] || each+=(-overlap)
    [ "$INTERBLOCK" = off ] || each+=(-inter-block-cache)
    local w=(-warmup 30 -rounds "$ROUNDS" "${each[@]}") n=(-warmup 40 -rounds 60 "${each[@]}") a=(-warmup 5 -rounds 20 "${each[@]}")
    local write=(-writes "$W_WRITES" -names "$W_WRITES") indexed=(-txs "$W_TXS" -tx-names 128 -index)
    local blk=(-writes "$B_WRITES" -names "$B_NAMES")

    # An uncapped baseline, and the p2-hit-16g cell of the blockcost suite again.
    blockcost_cell w-28g - "${write[@]}" "${w[@]}"
    blockcost_cell p2-repro-16g 16G -names 143198 -writes 10000 -warmup 40 -rounds 60 "${each[@]}"

    # Phase costs at 16 GiB.
    blockcost_cell w-16g 16G "${write[@]}" "${w[@]}"
    blockcost_cell blk-16g 16G "${blk[@]}" -txs "$B_TXS" -index "${w[@]}"
    blockcost_cell blk-16g-noindex 16G "${blk[@]}" "${w[@]}"
    blockcost_cell n-16g 16G -writes 0 -names "$N_NAMES" "${n[@]}"
    blockcost_cell a-big-16g 16G -writes 0 -names 0 -new "$A_BIG" -new-bytes "$A_BIG_BYTES" "${a[@]}"
    blockcost_cell a-mid-16g 16G -writes 0 -names 0 -new "$A_MID" -new-bytes "$A_MID_BYTES" "${a[@]}"
    blockcost_cell wi-16g 16G "${write[@]}" "${indexed[@]}" "${w[@]}"

    # Repeat the main workloads at 20 GiB.
    blockcost_cell w-20g 20G "${write[@]}" "${w[@]}"
    blockcost_cell wi-20g 20G "${write[@]}" "${indexed[@]}" "${w[@]}"
    # The same block, with the shipped timeout_commit between the commit and
    # the next load. The load is then not timed against the pruner of the
    # previous block.
    blockcost_cell wi-20g-gap400 20G "${write[@]}" "${indexed[@]}" "${w[@]}" -gap 400ms
    blockcost_cell blk-20g 20G "${blk[@]}" -txs "$B_TXS" -index "${w[@]}"
    blockcost_cell n-20g 20G -writes 0 -names "$N_NAMES" "${n[@]}"
    blockcost_cell a-big-20g 20G -writes 0 -names 0 -new "$A_BIG" -new-bytes "$A_BIG_BYTES" "${a[@]}"
    blockcost_cell a-mid-20g 20G -writes 0 -names 0 -new "$A_MID" -new-bytes "$A_MID_BYTES" "${a[@]}"
    blockcost_cell tw-20g 20G -writes "$TW_WRITES" -names "$TW_WRITES" -tables "$TW_TXS" \
        -txs "$TW_TXS" -tx-names 128 -index "${w[@]}"
    blockcost_cell tr-20g 20G -writes 0 -names "$TR_TABLES" -tables "$TR_TABLES" \
        -txs "$TR_TXS" -tx-names 31 -index "${n[@]}"
    # The cell writes the big accounts in one commit before the first block.
    # goleveldb still compacts the 510 MiB when the first blocks run. So ld
    # warms up for as many blocks as ldw. With five blocks, it timed the
    # compaction.
    blockcost_cell ld-20g 20G -writes 0 -names 0 -big 51 "${w[@]}"
    blockcost_cell ldw-20g 20G -writes "$LW_WRITES" -names "$LW_WRITES" -big 50 \
        -txs "$LW_TXS" -tx-names 128 -index "${w[@]}"
    # Why ldw runs at twice its time. pl plants the big accounts, and no block
    # loads them. ldf loads them. It plants them at the top of the key range
    # of each shard, where no account that a block writes is near them. lwb is
    # the write shape of ldw with no big account and 1 GiB more ballast. That
    # is the memory that the big accounts could hold, so the cap leaves less
    # page cache.
    blockcost_cell pl-20g 20G -writes "$LW_WRITES" -names "$LW_WRITES" -big 50 -big-load=false \
        -txs "$LW_TXS" -tx-names 128 -index "${w[@]}"
    blockcost_cell ldf-20g 20G -writes "$LW_WRITES" -names "$LW_WRITES" -big 50 -big-far \
        -txs "$LW_TXS" -tx-names 128 -index "${w[@]}"
    blockcost_cell lwb-20g 20G -writes "$LW_WRITES" -names "$LW_WRITES" \
        -txs "$LW_TXS" -tx-names 128 -index "${w[@]}" -ballast-gib 4

    # The aged state. The first cell of each budget builds its seed. It fills
    # history.db to the budget with blocks of the transfers of blk, until the
    # purge runs. Then 300 blocks of the write shape run, and tokens.db is
    # rebuilt from every account. The evidence-window budget is 2,000 such
    # blocks, the blocks that every machine keeps (min-retain-blocks). The
    # counter of the history counts each block. 50 GiB is the second point of
    # the slope. The seeds take about 16 and 40 GB on disk, and each cell
    # copies its own. An aged cell warms up 70 blocks. By about block 70, the
    # held heap of Go and the page cache that it leaves stop changing. Each
    # aged cell also holds REST large accounts at rest.
    local aged=(-aged-blocks 300 -age-writes "$W_WRITES" -age-txs "$W_TXS" -fill-txs "$B_TXS")
    [ "$REST" = 0 ] || aged+=(-big "$REST" -big-load=false)
    local aw=(-warmup 70 -rounds "$ROUNDS" "${each[@]}")
    blockcost_cell wi-20g-aged 20G "${write[@]}" "${indexed[@]}" "${aw[@]}" "${aged[@]}" -history-blocks 2000
    blockcost_cell blk-20g-aged 20G "${blk[@]}" -txs "$B_TXS" -index "${aw[@]}" "${aged[@]}" -history-blocks 2000
    blockcost_cell wi-20g-aged50 20G "${write[@]}" "${indexed[@]}" "${aw[@]}" "${aged[@]}" -history-bytes $((50 << 30))
    blockcost_cell blk-20g-aged50 20G "${blk[@]}" -txs "$B_TXS" -index "${aw[@]}" "${aged[@]}" -history-bytes $((50 << 30))
    # The other shapes that a price comes from, on the evidence-window seed.
    # An aged seed keeps its sidecars, so every aged cell writes them. On it,
    # the shape of w is wi. The name shape puts its names in transactions of
    # 128, as wi does. The allocation cells grow the tree by about 100 MB a
    # block, so they warm up 30 blocks, not 70.
    local aa=(-warmup 30 -rounds "$ROUNDS" "${each[@]}") hist=(-history-blocks 2000)
    blockcost_cell n-20g-aged 20G -writes 0 -names "$N_NAMES" -txs "$N_TXS" -tx-names 128 -index \
        "${aw[@]}" "${aged[@]}" "${hist[@]}"
    blockcost_cell a-big-20g-aged 20G -writes 0 -names 0 -new "$A_BIG" -new-bytes "$A_BIG_BYTES" -txs "$A_BIG" -index \
        "${aa[@]}" "${aged[@]}" "${hist[@]}"
    blockcost_cell a-mid-20g-aged 20G -writes 0 -names 0 -new "$A_MID" -new-bytes "$A_MID_BYTES" -txs "$A_MID" -index \
        "${aa[@]}" "${aged[@]}" "${hist[@]}"
    blockcost_cell tw-20g-aged 20G -writes "$TW_WRITES" -names "$TW_WRITES" -tables "$TW_TXS" \
        -txs "$TW_TXS" -tx-names 128 -index "${aw[@]}" "${aged[@]}" "${hist[@]}"
    blockcost_cell tr-20g-aged 20G -writes 0 -names "$TR_TABLES" -tables "$TR_TABLES" \
        -txs "$TR_TXS" -tx-names 31 -index "${aw[@]}" "${aged[@]}" "${hist[@]}"
    # ldw loads 50 accounts of its own, the 512 MiB of the arena, and spends
    # the rest of the units of the block on the write shape.
    blockcost_cell ldw-20g-aged 20G -writes "$LW_WRITES" -names "$LW_WRITES" -txs "$LW_TXS" -tx-names 128 -index \
        "${aw[@]}" "${aged[@]}" "${hist[@]}" -big 50 -big-load=true

    # Measure compilation and prewarming under each memory limit.
    local rust=(cargo test --release --locked)
    cell compile-rate-16g 16G "${rust[@]}" programs::tests::compile_rate_by_size -- --ignored --nocapture
    cell prewarm-16g 16G "${rust[@]}" programs::tests::prewarm_speedup -- --ignored --nocapture
    cell compile-rate-20g 20G "${rust[@]}" programs::tests::compile_rate_by_size -- --ignored --nocapture
    cell prewarm-20g 20G "${rust[@]}" programs::tests::prewarm_speedup -- --ignored --nocapture
}
