//! SVM execution report. Complements Criterion timings in
//! `benches/execution.rs` (`make bench`) with compute units, allocations
//! per transaction, program-cache footprint, deploy and cold-load costs
//! by program size, and thread scaling (`cargo run --release --locked` here).
//!
//! Methodology:
//! - Build transactions and expected results outside measured runs.
//! - Warm up before measurement. Report medians across rounds.
//! - Count allocations separately with a counting global allocator.
//! - Assert correctness first so failing transactions cannot skew results.

use {
    solana_program_runtime::{
        deploy::deploy_program,
        invoke_context::InvokeContext,
        loaded_programs::ProgramCacheForTxBatch,
        program_cache_entry::{ProgramCacheEntry, ProgramCacheEntryType},
        program_metrics::LoadProgramMetrics,
        solana_sbpf::{elf::Executable, verifier::RequisiteVerifier},
    },
    solana_svm_feature_set::SVMFeatureSet,
    solana_svm_type_overrides::sync::Arc,
    solana_transaction::sanitized::SanitizedTransaction,
    std::{
        alloc::{GlobalAlloc, Layout, System},
        sync::atomic::{AtomicU64, Ordering},
        sync::Barrier,
        thread,
        time::{Duration, Instant},
    },
    svm_bench::{
        build_spl_16, build_spl_4, build_spl_transfers, build_transfers, check_results,
        mock_bank::create_runtime_environment, self_check, verify, World, BATCHES, SPL_TOKEN_ELF,
        SPL_TOKEN_ID,
    },
};

const THREAD_COUNTS: &[usize] = &[1, 2, 4, 8, 16];

/// The pinned release programs, SPL Token among them. `make fetch-programs`
/// downloads them.
const PROGRAMS_DIR: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../../programs");

struct Counting;

static ALLOC_BYTES: AtomicU64 = AtomicU64::new(0);
static ALLOC_COUNT: AtomicU64 = AtomicU64::new(0);

unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        ALLOC_BYTES.fetch_add(layout.size() as u64, Ordering::Relaxed);
        ALLOC_COUNT.fetch_add(1, Ordering::Relaxed);
        System.alloc(layout)
    }
    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        System.dealloc(ptr, layout)
    }
    unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
        ALLOC_BYTES.fetch_add(new_size.saturating_sub(layout.size()) as u64, Ordering::Relaxed);
        System.realloc(ptr, layout, new_size)
    }
}

#[global_allocator]
static GLOBAL: Counting = Counting;

fn alloc_snapshot() -> (u64, u64) {
    (
        ALLOC_BYTES.load(Ordering::Relaxed),
        ALLOC_COUNT.load(Ordering::Relaxed),
    )
}

/// What one warm execution of a batch spends that is not time: compute units
/// and allocated bytes per transaction, and allocations per transaction.
fn profile(
    scenario: &'static str,
    batch: usize,
    cold: bool,
    build: fn(&World, usize) -> Vec<SanitizedTransaction>,
) {
    let world = World::new();
    let txs = build(&world, batch);
    let checks = check_results(&txs);

    let (ok, units) = verify(&world, &txs, &checks);
    assert_eq!(ok, batch, "{scenario}: not every transaction succeeded");

    if cold {
        world.evict_spl_token();
    }
    let c = checks.clone();
    let (bytes0, count0) = alloc_snapshot();
    let out = world.processor.load_and_execute_sanitized_transactions(
        &world.bank,
        &txs,
        c,
        &world.env,
        &world.config,
    );
    let (bytes1, count1) = alloc_snapshot();
    std::hint::black_box(&out);
    println!(
        "exp=svm-profile scenario={scenario} batch={batch} cache={} cu_per_tx={} alloc_b_per_tx={} allocs_per_tx={}",
        if cold { "cold" } else { "warm" },
        units / batch as u64,
        (bytes1 - bytes0) / batch as u64,
        (count1 - count0) / batch as u64,
    );
}

/// Each thread gets its own world, so this measures whether SVM execution is
/// embarrassingly parallel.
fn measure_threads(
    batch: usize,
    threads: usize,
    build: fn(&World, usize) -> Vec<SanitizedTransaction>,
) -> f64 {
    let barrier = Arc::new(Barrier::new(threads + 1));
    let mut handles = Vec::with_capacity(threads);

    for _ in 0..threads {
        let barrier = barrier.clone();
        handles.push(thread::spawn(move || {
            let world = World::new();
            let txs = build(&world, batch);
            let checks = check_results(&txs);

            // Warm up. The program cache fills here, not under timing.
            let _ = world.processor.load_and_execute_sanitized_transactions(
                &world.bank,
                &txs,
                checks.clone(),
                &world.env,
                &world.config,
            );

            barrier.wait();
            let start = Instant::now();
            let out = world.processor.load_and_execute_sanitized_transactions(
                &world.bank,
                &txs,
                checks,
                &world.env,
                &world.config,
            );
            std::hint::black_box(&out);
            start.elapsed()
        }));
    }

    barrier.wait();
    let start = Instant::now();
    for h in handles {
        h.join().unwrap();
    }
    let wall = start.elapsed();
    (threads * batch) as f64 / wall.as_secs_f64()
}

/// Resident set size in bytes, read from the kernel.
///
/// The counting allocator above cannot see the JIT. solana-sbpf mmaps its
/// output through `memory_management::allocate_pages_pooled`, which bypasses
/// Rust's global allocator. Only RSS gives a total that includes it.
fn rss_bytes() -> u64 {
    let statm = std::fs::read_to_string("/proc/self/statm").unwrap();
    let resident: u64 = statm.split_whitespace().nth(1).unwrap().parse().unwrap();
    resident.saturating_mul(4096)
}

/// What one cached program costs, and whether dropping it returns the memory.
///
/// `machine_code_length` is the size the JIT emitted. `mem_size` is the size it
/// holds: the up-front over-allocation, rounded to a power-of-two bucket. A
/// memory ceiling must use `mem_size`.
fn cache_footprint() {
    let world = World::new();
    let txs = build_spl_transfers(&world, 1);
    let checks = check_results(&txs);

    let rss_cold = rss_bytes();
    let _ = world.processor.load_and_execute_sanitized_transactions(
        &world.bank,
        &txs,
        checks.clone(),
        &world.env,
        &world.config,
    );
    let rss_warm = rss_bytes();

    let mut held_total: u64 = 0;
    for (id, entry) in world
        .processor
        .global_program_cache
        .read()
        .unwrap()
        .get_flattened_entries_for_tests()
    {
        let (kind, text, code, held) = match &entry.program {
            ProgramCacheEntryType::Loaded(exe) => {
                let jit = exe.get_compiled_program();
                (
                    "Loaded",
                    exe.get_text_bytes().1.len(),
                    jit.as_ref().map(|j| j.machine_code_length()).unwrap_or(0),
                    jit.as_ref().map(|j| j.mem_size()).unwrap_or(0),
                )
            }
            ProgramCacheEntryType::Builtin(_) => ("Builtin", 0, 0, 0),
            ProgramCacheEntryType::Unloaded(_) => ("Unloaded", 0, 0, 0),
            ProgramCacheEntryType::Closed => ("Closed", 0, 0, 0),
            ProgramCacheEntryType::DelayVisibility => ("DelayVis", 0, 0, 0),
            ProgramCacheEntryType::FailedVerification(_) => ("FailedVer", 0, 0, 0),
        };
        held_total = held_total.saturating_add(held as u64);
        println!("exp=svm-cache-entry program={id} kind={kind} text_b={text} code_b={code} held_b={held}");
    }

    // Dropping the entry runs JitProgram::drop, which returns the pages to
    // solana-sbpf's process-global bucketed free list rather than to the OS.
    world.evict_spl_token();
    let rss_evicted = rss_bytes();

    println!(
        "exp=svm-footprint held_b={held_total} rss_cold_b={rss_cold} rss_warm_b={rss_warm} rss_evicted_b={rss_evicted}"
    );
}

fn time<T>(f: impl FnOnce() -> T) -> (Duration, T) {
    let start = Instant::now();
    let out = f();
    (start.elapsed(), out)
}

fn micros(mut v: Vec<Duration>) -> (u128, u128) {
    v.sort();
    (v[v.len() / 2].as_micros(), v[v.len() - 1].as_micros())
}

/// Deploy and cold-load cost against program size, on real mainnet programs.
///
/// The loader charges a flat 2,370 CU for a deploy, then loads, verifies and
/// JIT-compiles with no metering. A program cache miss pays load, verify and
/// JIT again, also with no metering. The two calls timed whole are the ones the
/// runtime makes: `deploy_program`, and `ProgramCacheEntry::load` on a miss.
/// The three phases are timed separately. A size-proportional cost term can
/// then use whichever of file bytes or text bytes predicts them.
fn program_sweep() {
    let mut programs: Vec<(String, Vec<u8>)> = Vec::new();
    for entry in std::fs::read_dir(PROGRAMS_DIR).unwrap().flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if name.ends_with(".so") {
            programs.push((name, std::fs::read(entry.path()).unwrap()));
        }
    }
    programs.sort_by_key(|(_, elf)| elf.len());

    let features = SVMFeatureSet::all_enabled();
    let env = create_runtime_environment(&features);
    let loader = solana_sdk_ids::bpf_loader_upgradeable::id();
    const ROUNDS: usize = 7;
    // The deploy environment's version gate is the only place where the chain's
    // feature set changes what deploys. The cost is timed with the gate open,
    // and the verdict under all_enabled is reported beside it.
    let deploy_once = |elf: &[u8], disable_v0_v1_v2: bool| {
        deploy_program(
            None,
            &mut LoadProgramMetrics::default(),
            &mut ProgramCacheForTxBatch::new(0),
            env.clone(),
            disable_v0_v1_v2,
            &SPL_TOKEN_ID,
            &loader,
            elf,
            0,
        )
    };

    for (name, elf) in &programs {
        let exe = match Executable::<InvokeContext>::load(elf, Arc::clone(&*env)) {
            Ok(exe) => exe,
            Err(e) => {
                println!("exp=svm-sweep program={name} file_bytes={} load_error={e:?}", elf.len());
                continue;
            }
        };
        let text_bytes = exe.get_text_bytes().1.len();
        let version = exe.get_sbpf_version();
        exe.jit_compile().unwrap();
        let jit = exe.get_compiled_program().unwrap();
        let (code_bytes, held_bytes) = (jit.machine_code_length(), jit.mem_size());
        drop(exe);

        let (mut load, mut verify, mut compile, mut miss, mut deploy) =
            (vec![], vec![], vec![], vec![], vec![]);
        let mut deploy_error = None;
        let rss_before = rss_bytes();
        for _ in 0..ROUNDS {
            let (d, exe) = time(|| Executable::<InvokeContext>::load(elf, Arc::clone(&*env)).unwrap());
            load.push(d);
            verify.push(time(|| exe.verify::<RequisiteVerifier>().unwrap()).0);
            compile.push(time(|| exe.jit_compile().unwrap()).0);
            drop(exe);

            let (d, entry) = time(|| {
                ProgramCacheEntry::load(&loader, env.clone(), 0, elf, &mut LoadProgramMetrics::default())
            });
            entry.unwrap();
            miss.push(d);

            let (d, r) = time(|| deploy_once(elf, false));
            match r {
                Ok(()) => deploy.push(d),
                Err(e) => deploy_error = Some(e),
            }
        }
        let chain_features = match deploy_once(elf, features.disable_sbpf_v0_v1_v2_deployment) {
            Ok(()) => "ok".to_string(),
            Err(e) => format!("rejected({e:?})"),
        };
        let rss_after = rss_bytes();

        let (load_p50, load_max) = micros(load);
        let (verify_p50, verify_max) = micros(verify);
        let (jit_p50, jit_max) = micros(compile);
        let (miss_p50, miss_max) = micros(miss);
        let deploy_col = match (deploy_error, deploy.is_empty()) {
            (Some(e), true) => format!("deploy=rejected({e:?})"),
            _ => {
                let (p50, max) = micros(deploy);
                format!("deploy_p50_us={p50} deploy_max_us={max}")
            }
        };
        println!(
            "exp=svm-sweep program={name} sbpf={version:?} file_bytes={} text_bytes={text_bytes} jit_code_bytes={code_bytes} jit_held_bytes={held_bytes} \
             load_p50_us={load_p50} load_max_us={load_max} verify_p50_us={verify_p50} verify_max_us={verify_max} \
             jit_p50_us={jit_p50} jit_max_us={jit_max} miss_p50_us={miss_p50} miss_max_us={miss_max} {deploy_col} deploy_under_all_enabled={chain_features} \
             rss_before_mb={} rss_after_mb={}",
            elf.len(),
            rss_before >> 20,
            rss_after >> 20,
        );
    }
}

fn main() {
    // `sweep` runs only the program size sweep. Any other argument runs everything.
    if std::env::args().nth(1).as_deref() == Some("sweep") {
        program_sweep();
        return;
    }
    println!("# SVM execution report; timings are `make bench`");
    println!(
        "# spl_token.so: {} bytes, dumped from mainnet at {}",
        SPL_TOKEN_ELF.len(),
        SPL_TOKEN_ID
    );

    println!("# program cache footprint");
    cache_footprint();

    self_check();

    println!("# compute units and allocations per transaction");
    for &batch in BATCHES {
        profile("transfer", batch, false, build_transfers);
    }
    for &batch in BATCHES {
        profile("spl-transfer", batch, false, build_spl_transfers);
    }
    for (name, build) in [
        ("spl-transfer-x4", build_spl_4 as fn(&World, usize) -> Vec<SanitizedTransaction>),
        ("spl-transfer-x16", build_spl_16),
    ] {
        profile(name, 1000, false, build);
    }
    for &batch in BATCHES {
        profile("spl-transfer", batch, true, build_spl_transfers);
    }

    println!("# program size sweep");
    program_sweep();

    // Rows past the core count measure oversubscription, not SVM scaling.
    let cores = thread::available_parallelism().map_or(1, |n| n.get());
    let thread_counts: Vec<usize> = THREAD_COUNTS.iter().copied().filter(|&t| t <= cores).collect();
    println!("# scaling with threads (independent batches, batch=1000, {cores} cores)");
    for (name, build) in [
        ("transfer", build_transfers as fn(&World, usize) -> Vec<SanitizedTransaction>),
        ("spl-transfer", build_spl_transfers),
    ] {
        let base = measure_threads(1000, 1, build);
        for &t in &thread_counts {
            let tps = measure_threads(1000, t, build);
            println!(
                "exp=svm-threads scenario={name} batch=1000 threads={t} cores={cores} tx_per_s={tps:.0} speedup={:.2}",
                tps / base
            );
        }
    }
}

