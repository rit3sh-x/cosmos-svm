# Benchmarks

The `bench/` directory holds the performance benchmarks for the Go, Rust, storage and validator parts.

The [glossary](GLOSSARY.md) explains the terms and cell names that the suites use.

## Benchmark suites

| Suite | Measures | Duration |
|---|---|---|
| `micro` | Go FFI overhead and SVM execution | Minutes |
| `perf` | The store's cache curve, pruning, export and allocation, then the SVM report and the FFI benchmarks | Hours |
| `state` | The store at 1M to 10M accounts, and a 10M export | Hours |
| `prune` | Pruning across cache sizes, backends, workloads, and store settings | Hours |
| `shard` | Sharding at the shipped configuration | Hours |
| `read` | One account lookup at the shipped store | Hours |
| `blockcost` | Account loading through the application store stack | Hours |
| `blockcost-memory` | The `blockcost` 16 GB cell again, and a 20 GB cell | Hours |
| `block-limits` | Each block limit, at the published minimum hardware | Hours |
| `validator-memory` | What a validator's memory goes to, under the published cap | About an hour |
| `proof-window` | How long a committed height stays provable | Minutes |
| `smoke` | Every harness and scenario at toy sizes, to check the setup. Its numbers mean nothing. | Minutes |

## Layout

| Path | Language | What it is |
|---|---|---|
| `bench.sh` | Shell | The entry point. It builds the binaries, runs the cells and collects the results. |
| `lib.sh` | Shell | Shared helpers for memory limits and validator setup |
| `suites/` | Shell | Suite definitions, with their configuration and benchmark cells |
| `scenarios/` | Shell | Scenarios that run a validator: validator memory and proof retention |
| `storage/` | Go | Account-store benchmark harness. See [README](storage/README.md). |
| `blockcost/` | Go | Measurements of block processing in four phases. See [README](blockcost/README.md). |
| `svm/` | Rust | SVM execution benchmarks and performance reports. See [README](svm/README.md). |
| `../tools/nodetool/` | Go | Memory sampling, queries to a running `svmd`, and genesis and configuration utilities |
| `report.py` | Python | Offline analysis and comparison of benchmark results |
| `results/` | - | Benchmark runs. Git ignores this directory. |

## Results

`bench.sh` stores each run in `bench/results/SUITE-UTC/`.

| File or directory | Contents |
|---|---|
| `summary.txt` | Cell status, duration, and final result line |
| `hardware.txt` | Environment and toolchain metadata |
| `build.txt` | Build output |
| `CELL.txt` | Executed command and cell output |
| `CELL.iostat`, `CELL.vmstat` | System statistics, when available |
| `series/` | Per-block CSV measurements |
| Other files | Scenario-specific output, such as memory samples and heap profiles |

`bench.sh` records a failed cell and continues with the remaining cells. If any cell fails, the command exits with status 1.

## Adding a benchmark

1. Write the workload and the measurements in Go or Rust, with structured `exp=` result lines.
2. If the benchmark needs a build step, add it to `build` in `bench.sh`.
3. Add a suite definition under `suites/`, with its dependencies, parameters and cells.
4. Add a small test case to `suites/smoke.sh`.
5. If the suite needs its own analysis, add it to `report.py`.
6. Add the suite to the table above.

## Tools

| Tool | Purpose |
|---|---|
| Go `testing.B` | Go microbenchmarks |
| `benchstat` | Go benchmark statistics and comparisons |
| Criterion | Rust benchmark statistics |
| `iostat`, `vmstat` | System monitoring |
| `systemd-run` | Memory-limited benchmark execution |
| ShellCheck | Shell-script linting |

`systemd-run` caps a cell through sudo. On a shared machine with no sudo without a password, set
`BENCH_USER_SCOPE=1`. The cell then runs in a scope of your own systemd, which must delegate the
memory controller, and the page cache is not dropped. `BENCH_CPUS`, a `taskset` list such as `0-3`,
pins a capped cell to those CPUs, so a large machine runs it on the core count of the minimum
machine. Use runs made this way only to compare two builds on the same machine, never to set a
price.

A capped `blockcost` cell also holds 3 GiB of ballast, for a program cache that is full. A 1.5 GiB
budget keeps this much resident after two maximum blocks. Runs before 2026-11-07 held 4 GiB.
`BALLAST=mmap`, the default, holds it outside the Go heap, where the program cache of svmd is.
`BALLAST=heap` makes it a Go allocation, as in the runs before 2026-11-07. A `block-limits` cell
also starts glibc as svmd starts it (`MALLOC=svmd`, the default). `MALLOC=glibc` keeps the defaults
of glibc, as in the runs before 2026-11-07.

To profile a running `svmd`, see [Profile a node](../docs/runbook.md#profile-a-node).
