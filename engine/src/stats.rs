//! Machine-local counters.
//!
//! Everything here is off the path that produces a consensus result.
//! Wall-clock and cache numbers stay out of the FFI schema by construction,
//! not by convention. They travel through their own entry point, in their own
//! arena kind, read after the block is committed. A number in this module
//! cannot reach an app hash, because it never travels on the arena that
//! carries one.
//!
//! The counters accumulate for the life of the process. Agave resets its
//! program cache stats on each new bank, so its numbers are per-slot gauges.
//! These are counters in the Prometheus sense, so a dashboard ported from
//! Solana reads them wrongly unless it is told. The exceptions are gauges:
//! the cache's water level and held bytes, the last block's conflict groups,
//! widest group and program text, and the results each signature tier holds.

use std::sync::atomic::{AtomicU64, Ordering};

/// Programs a block asked for that the cache did not hold, summed over every
/// block.
pub static PREWARM_MISSES: AtomicU64 = AtomicU64::new(0);

/// Conflict groups in the last block, and the largest of them. The parallel
/// model rests on this partition. A block that degenerates to one group is
/// serial execution, and these numbers let an operator see it.
pub static CONFLICT_GROUPS: AtomicU64 = AtomicU64::new(0);
pub static WIDEST_GROUP: AtomicU64 = AtomicU64::new(0);

/// The program text the last block was charged against the budget. It is the
/// engine's number, not Go's, because only the engine sees the account bytes
/// the charge comes from.
pub static BLOCK_PROGRAM_TEXT: AtomicU64 = AtomicU64::new(0);

pub fn add_prewarm_misses(n: usize) {
    PREWARM_MISSES.fetch_add(n as u64, Ordering::Relaxed);
}

pub fn set_program_text(bytes: u64) {
    BLOCK_PROGRAM_TEXT.store(bytes, Ordering::Relaxed);
}

pub fn set_groups(groups: &[Vec<usize>]) {
    CONFLICT_GROUPS.store(groups.len() as u64, Ordering::Relaxed);
    WIDEST_GROUP.store(groups.iter().map(Vec::len).max().unwrap_or(0) as u64, Ordering::Relaxed);
}

/// The counters, in the order the response packs them. Go reads them by
/// position, so a field may be appended and never reordered. Fields 10 to 24
/// are the signature-result counters, by event and then by handler. Fields 25
/// and 26 are the results that the pool and block tiers hold.
pub const FIELDS: usize = 27;
/// Where the signature-result counters start, after the ten fixed counters.
/// Go reads from the same boundary, `statsFixedFields` in ffi/engine.go.
const SIGNATURE_RESULTS_AT: usize = 10;

pub fn snapshot() -> [u64; FIELDS] {
    use crate::signatures::{HANDLERS, TIERS};
    let mut out = [0u64; FIELDS];
    out[6] = PREWARM_MISSES.load(Ordering::Relaxed);
    out[7] = CONFLICT_GROUPS.load(Ordering::Relaxed);
    out[8] = WIDEST_GROUP.load(Ordering::Relaxed);
    out[9] = BLOCK_PROGRAM_TEXT.load(Ordering::Relaxed);
    for (e, row) in crate::signatures::counts().iter().enumerate() {
        out[SIGNATURE_RESULTS_AT + e * HANDLERS..][..HANDLERS].copy_from_slice(row);
    }
    out[FIELDS - TIERS..].copy_from_slice(&crate::signatures::tiers());
    let Some(root) = crate::runtime::root() else {
        return out;
    };
    let cache = &root.processor.global_program_cache;
    {
        let stats = &cache.read().unwrap().stats;
        out[0] = stats.hits.load(Ordering::Relaxed);
        out[1] = stats.misses.load(Ordering::Relaxed);
        out[2] = stats.evictions.values().sum();
        out[3] = stats.one_hit_wonders.load(Ordering::Relaxed);
        out[4] = stats.water_level.load(Ordering::Relaxed);
    }
    out[5] = crate::programs::held_bytes();
    out
}
