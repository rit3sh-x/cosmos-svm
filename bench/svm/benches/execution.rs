//! The cost to execute a batch of transactions, under Criterion (`make bench`).
//! Each benchmark counts throughput in transactions. Criterion reports a time
//! per batch and a rate per transaction, with a confidence interval, and
//! compares each run with the previous one.
//!
//! The batches are the same as in the report (src/main.rs). Each batch must
//! succeed before it is timed. The setup clones the check results outside the
//! timed routine, because execution does not produce them in svmd.

use {
    criterion::{criterion_group, criterion_main, BatchSize, BenchmarkId, Criterion, Throughput},
    solana_transaction::sanitized::SanitizedTransaction,
    svm_bench::{
        build_spl_16, build_spl_4, build_spl_transfers, build_transfers, check_results, verify, World,
        BATCHES,
    },
};

type Build = fn(&World, usize) -> Vec<SanitizedTransaction>;

/// One benchmark: `batch` transactions from `build`, executed warm or cold.
/// Cold drops SPL Token from the program cache before each run, so each run
/// pays ELF load, verify and JIT compile.
fn execute(c: &mut Criterion, group: &str, scenario: &str, batch: usize, cold: bool, build: Build) {
    let world = World::new();
    let txs = build(&world, batch);
    let checks = check_results(&txs);
    let (ok, _) = verify(&world, &txs, &checks);
    assert_eq!(ok, batch, "{scenario}: not every transaction succeeded");

    let mut g = c.benchmark_group(group);
    g.throughput(Throughput::Elements(batch as u64));
    if batch >= 10_000 {
        g.sample_size(10);
    }
    g.bench_with_input(BenchmarkId::new(scenario, batch), &batch, |b, _| {
        b.iter_batched(
            || {
                if cold {
                    world.evict_spl_token();
                }
                checks.clone()
            },
            |checks| {
                world.processor.load_and_execute_sanitized_transactions(
                    &world.bank,
                    &txs,
                    checks,
                    &world.env,
                    &world.config,
                )
            },
            BatchSize::PerIteration,
        )
    });
    g.finish();
}

fn per_transaction(c: &mut Criterion) {
    for &batch in BATCHES {
        execute(c, "execute", "transfer", batch, false, build_transfers);
        execute(c, "execute", "spl-transfer", batch, false, build_spl_transfers);
    }
}

/// The fixed cost of a transaction against the marginal cost of one more
/// program invocation, at a thousand transactions.
fn per_instruction(c: &mut Criterion) {
    execute(c, "instructions", "spl-transfer-x1", 1000, false, build_spl_transfers);
    execute(c, "instructions", "spl-transfer-x4", 1000, false, build_spl_4);
    execute(c, "instructions", "spl-transfer-x16", 1000, false, build_spl_16);
}

/// ELF load, verify and JIT compile on every batch: a program cache miss.
fn cold_cache(c: &mut Criterion) {
    for &batch in BATCHES {
        execute(c, "cold-cache", "spl-transfer", batch, true, build_spl_transfers);
    }
}

criterion_group!(benches, per_transaction, per_instruction, cold_cache);
criterion_main!(benches);
