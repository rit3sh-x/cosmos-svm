# Account store benchmark

This benchmark measures the cost to write Solana accounts to the Cosmos commitment tree. It measures IAVL updates, version commits, caching, pruning and account reads.

Run one benchmark cell:

```bash
cd bench/storage && go run . -mode store -backend goleveldb -dir /mnt/ssd -exp read -caches 4000000
```

These suites use this benchmark: `perf`, `state`, `prune`, `shard`, `read` and `smoke`. `bench/bench.sh list` lists them. Run every cell of one suite:

```bash
bench/bench.sh run --data /mnt/ssd perf
```

## Modes

### Single tree (`-mode single`)

This mode measures `Set`, `SaveVersion`, write cost, block time, throughput and allocations. It varies the access pattern, the batch size and the IAVL cache size.

```bash
go run . -backend=mem -preload=1000000
go run . -backend=pebble -preload=1000000
```

### Sharded (`-mode shard`)

This mode compares 1, 2, 4, 8 and 16 IAVL stores, with serial and parallel commits. It tests cache configurations. It uses random account keys to measure shard balance.

```bash
go run . -mode=shard -backend=pebble -preload=1000000
```

To run one configuration, use `-shards`, `-cache` and `-keys sequential`.

All shards share one database. Sharding makes smaller trees, not separate disks. The SDK commits the stores serially. The parallel results estimate the possible gain of parallel commits. Each store gets the configured cache size, so the total cache use increases with the shard count.

### Shipped store (`-mode store`)

This mode models the store stack of the chain: `rootmulti`, disk storage and pruning. Use `-exp` to select `cache`, `prune`, `export`, `alloc`, `shard` or `read`.

## Cache and measurement

The IAVL node cache has a fixed entry limit, and writes make new node keys. A short run may not fill a large cache. Single mode always measures 25 rounds. Shard mode measures 15 rounds by default, and store mode 150. `-rounds N` sets the count for both. In shard mode, `-gc` keeps garbage collection on during measurement.

The benchmark calculates the keys and values before it starts. It runs warmup rounds, then reports median timings, allocations and heap use. Single mode, and shard mode at 15 rounds or fewer, turn GC off during measurement, so that the results show only IAVL costs. Store mode keeps GC on, as on a validator.

NOTE: Check the heap use. If it is more than the available memory, the results can show paging, not storage performance.

Single-tree mode uses sequential keys. Sharded mode uses random 32-byte keys, like Solana public keys, to measure shard balance. The `shards=1` result is the control that compares the two modes.
