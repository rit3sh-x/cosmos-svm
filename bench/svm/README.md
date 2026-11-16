# SVM execution benchmark

This benchmark measures the execution cost of Solana transactions. Compare it with the commit cost that [`bench/storage`](../storage/README.md) measures. It also measures program-cache memory, compilation costs and the effects of program size.

## Commands

```bash
make bench           # Criterion execution timings and Go FFI benchmarks
make fetch-programs  # the programs the report below runs
cd bench/svm && cargo run --release --locked   # Compute units, allocations, cache memory, program size and thread scaling
```

Criterion stores its results in `bench/svm/target/criterion`. It compares each run with the previous baseline.

## What it measures

The benchmark times `TransactionBatchProcessor::load_and_execute_sanitized_transactions`. The times do not include signature verification, IAVL account loading or commitment.

Criterion measures three workloads:

| Workload | What it runs |
| --- | --- |
| Execution | Native system and SPL Token transfers, in batches of 1, 1,000 and 10,000 transactions |
| Instructions | Transactions with 1, 4 or 16 SPL transfers |
| Cold cache | The benchmark removes the program before each batch, which measures ELF loading, verification and JIT compilation |

The report adds four more measurements:

| Measurement | What it records |
| --- | --- |
| Program cache | Memory footprint, JIT code size, and RSS before loading, after compilation and after the entry is dropped |
| Compute and allocations | Compute units and Rust allocations for each transaction and each batch |
| Program-size sweep | Load, verify, compile, cache-miss and deploy costs for the programs in `programs/` |
| Thread scaling | Execution with 1, 2, 4, 8 and 16 threads |

The cache-memory measurements run first. The process-global page pool of `solana-sbpf` stays warm after use, so RSS does not always fall when the benchmark drops a cache entry. The benchmark measures JIT memory separately through `JitProgram::mem_size()`, because JIT code uses `mmap`, not Rust's global allocator.

To run only the program-size sweep:

```bash
cargo run --release --locked -- sweep
```

## Program under test

`programs/spl_token.so`, at the repository root, is the real SPL Token program. It is the `p-token@v1.0.0` release of `solana-program/token`, which is the build that mainnet runs. `make fetch-programs` downloads it and checks it against `programs/manifest.json`.

## Method and validation

The benchmark prepares the transactions and the check results outside the timed routine. Criterion does the warmup and the statistical sampling. A counting allocator measures allocations separately.

Before it measures, the benchmark checks that the transfers change account state, and it checks the compute meter at the reported cost and at one unit below it.

The mock bank does not have the test-only account-access tracking of upstream. It uses Agave's runtime environment with the debugging features off, which is closer to what a validator runs.

## Workspace isolation

The root workspace excludes `bench/svm`, because its dependencies need `dev-context-only-utils`. Without the exclusion, Cargo feature unification can turn on test hooks in the production static library. The benchmark has its own lockfile, which takes its versions from the lockfile of Agave v4.3.0.
