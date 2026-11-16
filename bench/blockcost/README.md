# Block cost benchmark

This benchmark measures the storage cost of one block on the store stack that `FinalizeBlock` uses. It times four stages, one at a time and together:

| Stage | What it covers |
| --- | --- |
| Account loading | `svm.Stores.PushAccounts` and the arena finish |
| State writes | The writes of changed and new accounts |
| Sidecar indexes | The writes to the History, TokenIndex and Dedup indexes (only with `-index`) |
| Commit | The cache write and the root store commit, with hashing and pruning |

## Run a benchmark

```bash
make engine && go run ./bench/blockcost -dir /mnt/ssd -names 143198 -mode hit -label hit
```

The first run in a directory builds the seed database. By default, the seed holds 10,000,000 accounts, and building it takes about an hour. Later runs use the same seed again.

NOTE: Each run copies the seed to a cell directory beside it and measures the copy.

## Flags

- `-dir` sets the directory of the seed database and the cell.
- `-accounts` sets the number of accounts in state. The default is 10,000,000.
- `-names` sets the number of account keys in the load list of each block.
- `-writes` sets the number of accounts that each block writes. `-new` adds accounts that each block
  allocates, with `-new-bytes` of data each (default 165, a token account).
- `-warmup` and `-rounds` set the number of unmeasured and measured blocks (default 40 and 60).
- `-mode` sets the workload: `hit` (the keys exist) or `miss` (the keys do not exist).
- `-label` sets the cell label. The output line shows it.
- `-ballast-gib` holds this number of GiB of memory in place of the program cache. Use it with a `systemd-run -p MemoryMax` limit.
  A capped cell of a suite holds 3 GiB. Runs before 2026-11-07 held 4 GiB.
- `-ballast-mmap` holds the ballast outside the Go heap, where the program cache of the engine is. Without
  it the ballast is a Go allocation, and the GC goal of Go grows by its size.
- `-heap-profile DIR` writes Go heap profiles at the first measured block, the middle one and after the last.
- With `-report-every`, a `# memory` line follows each window line. It gives the Go memory classes and the
  live heap (`gc_live_mb`), what glibc holds and uses, what Pebble holds outside the Go heap, the page cache
  of each database, the highest anonymous memory in the window, refaults and the time spent stalled on memory.
  `heap_live_mb` on the window line is HeapAlloc. It also counts garbage that is not swept yet.
- `-gap` adds an idle interval between the commit and the next account load. It stands in for the CometBFT `timeout_commit` interval.
- `-index` runs the History, TokenIndex and Dedup indexes, as a block does. `-txs` sets the number of
  transactions that each block records in them, and `-tx-names` the account keys of each one
  (default 3).

Each benchmark cell measures one workload and one configuration. Compare cells to find the storage cost of a block at different workloads, memory limits and block intervals.
