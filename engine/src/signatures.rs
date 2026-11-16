//! The results of successful signature checks.
//!
//! A result means that ed25519 accepted every signature of these exact
//! transaction bytes. Phase 1 reuses a result in place of a second check, and
//! checks every transaction it holds no result for. So the store changes how
//! long a block takes, never what it answers. A node that loses a result to a
//! restart or to the cap gives the same answers. It only checks more. The
//! store is in memory only.
//!
//! `CheckTx` New writes it in two steps, so a result enters only after Go's
//! own checks pass too. The engine's check puts the key in a pending slot, and
//! `svm_record_verified` promotes it only when the slot holds that key. Go
//! never computes a key. One slot is enough, because the ABCI connection runs
//! one `CheckTx` at a time.
//!
//! The store forgets a result when its transaction commits, which is when
//! CometBFT takes the transaction out of the pool. Recheck does not make it
//! forget, so a block that carries a dropped transaction anyway reuses the
//! result.
//!
//! `ProcessProposal` writes the block tier. It keeps the misses that its check
//! passed, and pins the hits of a proposal it accepts, so that `CheckTx`
//! traffic before `FinalizeBlock` cannot push them out of the pool tier. The
//! tier holds at most one block for each round of the height. The height's
//! `FinalizeBlock` clears it when it ends.

use rayon::prelude::*;
use std::collections::{HashSet, VecDeque};
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{LazyLock, Mutex, MutexGuard, PoisonError};

/// BLAKE3 of the exact signed transaction bytes, the bytes inside
/// `MsgSVMTransaction`. A change to any byte is a different key, whatever the
/// decoder accepts, so a re-signed message never meets another's result.
pub type Key = [u8; 32];

pub fn key(tx: &[u8]) -> Key {
    *blake3::hash(tx).as_bytes()
}

/// Twice CometBFT's default `mempool.size`. svmd sets the node's own.
const DEFAULT_CAP: usize = 10_000;

pub struct Store {
    /// The pool tier holds what `CheckTx` admitted, as a queue, oldest first,
    /// and as a set.
    order: VecDeque<Key>,
    pool: HashSet<Key>,
    cap: usize,
    /// The key of the last `CheckTx` New check, when it passed.
    pending: Option<Key>,
    /// The block tier, which `ProcessProposal` writes.
    block: HashSet<Key>,
}

impl Store {
    fn new(cap: usize) -> Self {
        Store { order: VecDeque::new(), pool: HashSet::new(), cap, pending: None, block: HashSet::new() }
    }

    pub fn holds(&self, key: &Key) -> bool {
        self.pool.contains(key) || self.block.contains(key)
    }

    /// Puts `key` in the pool tier. Returns whether it was new, and how many
    /// older results the cap pushed out.
    fn insert(&mut self, key: Key) -> (bool, usize) {
        if !self.pool.insert(key) {
            return (false, 0);
        }
        self.order.push_back(key);
        let mut evicted = 0;
        while self.order.len() > self.cap {
            if let Some(old) = self.order.pop_front() {
                self.pool.remove(&old);
                evicted += 1;
            }
        }
        (true, evicted)
    }

    /// Takes `keys` out of the pool tier. Returns how many it held.
    fn forget(&mut self, keys: &[Key]) -> usize {
        let removed = keys.iter().filter(|k| self.pool.remove(*k)).count();
        if removed > 0 {
            self.order.retain(|k| self.pool.contains(k));
        }
        removed
    }

    /// Writes what a proposal's check leaves in the block tier. That is each
    /// miss whose check passed, and each hit when no check failed. Returns how
    /// many misses were new there. A pinned hit is a copy, not a write.
    fn settle(&mut self, keys: &[Key], hits: &[bool], checked: &[Option<bool>]) -> usize {
        self.block.reserve(keys.len());
        let accepted = !checked.contains(&Some(false));
        let mut written = 0;
        for ((key, hit), check) in keys.iter().zip(hits).zip(checked) {
            if *check == Some(true) {
                written += self.block.insert(*key) as usize;
            } else if *hit && accepted {
                self.block.insert(*key);
            }
        }
        written
    }

    /// Promotes `key` when the pending slot holds it, and empties the slot.
    /// Returns what `insert` returns, or `None` for a key the slot does not hold.
    fn record(&mut self, key: &Key) -> Option<(bool, usize)> {
        (self.pending.take() == Some(*key)).then(|| self.insert(*key))
    }
}

/// What `svm_signature_results_total` counts, by event and by the handler it
/// happens in. Go names both by position (`ffi.SignatureEvents`,
/// `ffi.SignatureHandlers`), so neither may be reordered.
#[derive(Clone, Copy)]
pub enum Event {
    Hit,
    Miss,
    Written,
    Evicted,
    Forgotten,
}

#[derive(Clone, Copy)]
pub enum Handler {
    CheckTx,
    ProcessProposal,
    FinalizeBlock,
}

pub const EVENTS: usize = 5;
pub const HANDLERS: usize = 3;
/// The tiers `svm_signature_results` reports, pool then block.
pub const TIERS: usize = 2;

static COUNTS: [[AtomicU64; HANDLERS]; EVENTS] = [const { [const { AtomicU64::new(0) }; HANDLERS] }; EVENTS];
/// The results each tier holds, kept beside the store so a scrape reads them
/// without its lock.
static HELD: [AtomicU64; TIERS] = [const { AtomicU64::new(0) }; TIERS];

static STORE: LazyLock<Mutex<Store>> = LazyLock::new(|| Mutex::new(Store::new(DEFAULT_CAP)));

/// Locks the store. A panic while a thread held it leaves nothing that a
/// result depends on, so this takes a poisoned lock as it is.
pub fn lock() -> MutexGuard<'static, Store> {
    STORE.lock().unwrap_or_else(PoisonError::into_inner)
}

pub fn count(event: Event, handler: Handler, n: usize) {
    if n > 0 {
        COUNTS[event as usize][handler as usize].fetch_add(n as u64, Ordering::Relaxed);
    }
}

fn held(store: &Store) {
    HELD[0].store(store.pool.len() as u64, Ordering::Relaxed);
    HELD[1].store(store.block.len() as u64, Ordering::Relaxed);
}

pub fn counts() -> [[u64; HANDLERS]; EVENTS] {
    COUNTS.each_ref().map(|row| row.each_ref().map(|c| c.load(Ordering::Relaxed)))
}

pub fn tiers() -> [u64; TIERS] {
    HELD.each_ref().map(|c| c.load(Ordering::Relaxed))
}

/// Sets the pending slot. For `CheckTx` New, `svm_check_tx` writes the key of
/// a check that passed, or nothing.
pub fn set_pending(key: Option<Key>) {
    lock().pending = key;
}

/// `svm_record_verified`. Returns whether the pending slot held `key`.
pub fn record_verified(key: &Key) -> bool {
    let mut store = lock();
    let Some((written, evicted)) = store.record(key) else {
        return false;
    };
    count(Event::Written, Handler::CheckTx, written as usize);
    count(Event::Evicted, Handler::CheckTx, evicted);
    held(&store);
    true
}

/// A block's transactions commit with it, so the store forgets their results.
pub fn forget(keys: &[Key]) {
    let mut store = lock();
    let removed = store.forget(keys);
    count(Event::Forgotten, Handler::FinalizeBlock, removed);
    held(&store);
}

/// `ProcessProposal`'s check of a proposal's signatures, after its cheap
/// checks. It hashes every transaction and looks each key up in
/// both tiers, then checks the misses until the first one that fails. Last,
/// under one lock, it writes the misses that passed to the block tier and,
/// when none failed, pins the hits there. A result is visible only after its
/// check passed. Returns the index of the first transaction whose signatures
/// fail.
///
/// The misses run on rayon. Every miss before the first failure is checked,
/// and a miss after a failure found so far is skipped, so the index returned
/// is the first failure in block order whatever order the threads ran in.
pub fn check_proposal(txs: &[&[u8]]) -> Option<usize> {
    let keys: Vec<Key> = txs.par_iter().map(|tx| key(tx)).collect();
    let hits: Vec<bool> = {
        let store = lock();
        keys.iter().map(|k| store.holds(k)).collect()
    };
    let first = AtomicUsize::new(usize::MAX);
    let checked: Vec<Option<bool>> = (0..txs.len())
        .into_par_iter()
        .map(|i| {
            if hits[i] || i > first.load(Ordering::Relaxed) {
                return None;
            }
            let passed = crate::check::verify_signatures(txs[i]);
            if !passed {
                first.fetch_min(i, Ordering::Relaxed);
            }
            Some(passed)
        })
        .collect();
    let failed = checked.iter().position(|c| *c == Some(false));
    let hit = hits.iter().filter(|h| **h).count();
    count(Event::Hit, Handler::ProcessProposal, hit);
    count(Event::Miss, Handler::ProcessProposal, txs.len() - hit);
    let mut store = lock();
    let written = store.settle(&keys, &hits, &checked);
    count(Event::Written, Handler::ProcessProposal, written);
    held(&store);
    failed
}

/// The height's `FinalizeBlock` is over, so the store forgets the block
/// tier.
pub fn end_height() {
    let mut store = lock();
    let removed = store.block.len();
    store.block.clear();
    count(Event::Forgotten, Handler::FinalizeBlock, removed);
    held(&store);
}

/// The pool tier's cap. A lower cap takes effect at the next insertion.
pub fn set_cap(cap: usize) {
    lock().cap = cap;
}

/// Empties the store, as a restart does. Tests only.
pub fn clear_for_tests() {
    let mut store = lock();
    let cap = store.cap;
    *store = Store::new(cap);
    held(&store);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn k(n: u8) -> Key {
        [n; 32]
    }

    /// The writes of one key from many threads leave one entry, and only one
    /// of them counts as an insertion.
    #[test]
    fn concurrent_writes_of_one_key_leave_one_entry() {
        let store = Mutex::new(Store::new(DEFAULT_CAP));
        let new = std::thread::scope(|s| {
            let writers: Vec<_> = (0..8).map(|_| s.spawn(|| store.lock().unwrap().insert(k(1)).0)).collect();
            writers.into_iter().map(|w| w.join().unwrap()).filter(|new| *new).count()
        });
        let store = store.into_inner().unwrap();
        assert_eq!(new, 1);
        assert_eq!((store.pool.len(), store.order.len()), (1, 1));
    }

    /// Go cannot record a key the engine did not just produce, and a record
    /// empties the slot whether it matched or not.
    #[test]
    fn a_record_promotes_only_the_pending_key() {
        let mut store = Store::new(DEFAULT_CAP);
        assert_eq!(store.record(&k(1)), None, "an empty slot promotes nothing");
        store.pending = Some(k(1));
        assert_eq!(store.record(&k(2)), None, "a key the slot does not hold");
        assert!(!store.holds(&k(1)) && !store.holds(&k(2)));
        assert_eq!(store.record(&k(1)), None, "the refused record emptied the slot");
        store.pending = Some(k(1));
        assert_eq!(store.record(&k(1)), Some((true, 0)));
        assert!(store.holds(&k(1)));
        assert_eq!(store.pending, None);
    }

    #[test]
    fn a_result_is_forgotten_when_its_transaction_commits() {
        let mut store = Store::new(DEFAULT_CAP);
        for n in 1..=3 {
            store.insert(k(n));
        }
        assert_eq!(store.forget(&[k(2), k(9)]), 1, "only a held result counts");
        assert!(store.holds(&k(1)) && !store.holds(&k(2)) && store.holds(&k(3)));
        assert_eq!(store.order, [k(1), k(3)]);
    }

    #[test]
    fn the_cap_evicts_the_oldest() {
        let mut store = Store::new(2);
        assert_eq!(store.insert(k(1)), (true, 0));
        assert_eq!(store.insert(k(2)), (true, 0));
        assert_eq!(store.insert(k(2)), (false, 0), "a held key is not inserted again");
        assert_eq!(store.insert(k(3)), (true, 1));
        assert!(!store.holds(&k(1)) && store.holds(&k(2)) && store.holds(&k(3)));
        store.cap = 1;
        assert_eq!(store.insert(k(4)), (true, 2), "a lower cap applies at the next insertion");
        assert_eq!(store.order, [k(4)]);
    }

    /// What `CheckTx` New adds to its ed25519 check, apart from the crossing,
    /// on the 4-vCPU reference machine. It times BLAKE3 of a transaction of
    /// each size that the gate's blocks carry, and an insertion into a full
    /// pool tier, which pushes one result out. Each figure is per transaction,
    /// the median of 100 runs of 10,000, after two warm-ups.
    ///
    /// `cargo test --release -p svm-engine signature_result_cost -- --ignored --nocapture`
    #[test]
    #[ignore]
    fn signature_result_cost() {
        const N: usize = 10_000;
        fn median_ns(mut run: impl FnMut(usize)) -> f64 {
            let mut d: Vec<f64> = (0..102)
                .map(|r| {
                    let start = std::time::Instant::now();
                    run(r);
                    start.elapsed().as_nanos() as f64 / N as f64
                })
                .skip(2)
                .collect();
            d.sort_by(f64::total_cmp);
            d[d.len() / 2]
        }
        let mut x: u64 = 0x9e37_79b9_7f4a_7c15;
        let mut next = move || {
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            x
        };
        for len in [263, 1_201] {
            let txs: Vec<Vec<u8>> = (0..N).map(|_| (0..len).map(|_| next() as u8).collect()).collect();
            let ns = median_ns(|_| txs.iter().for_each(|tx| {
                std::hint::black_box(key(tx));
            }));
            println!("exp=sigresults-engine what=blake3 tx_bytes={len} per_tx_ns={ns:.1}");
        }
        let fresh: Vec<Key> = (0..N * 103).map(|i| key(&(i as u64).to_le_bytes())).collect();
        let mut store = Store::new(DEFAULT_CAP);
        fresh[..N].iter().for_each(|k| {
            store.insert(*k);
        });
        let ns = median_ns(|r| fresh[N * (r + 1)..][..N].iter().for_each(|k| {
            std::hint::black_box(store.insert(*k));
        }));
        assert_eq!(store.pool.len(), DEFAULT_CAP);
        println!("exp=sigresults-engine what=insert-at-cap cap={DEFAULT_CAP} per_tx_ns={ns:.1}");
    }

    /// Pinned hits last through `CheckTx` traffic until the height ends. The
    /// cap pushes them out of the pool tier, and the block tier still holds
    /// them. A refused proposal pins nothing, and keeps what it checked.
    #[test]
    fn pinned_hits_last_until_the_height_ends() {
        let mut store = Store::new(2);
        store.insert(k(1));
        store.insert(k(2));
        assert_eq!(store.settle(&[k(1), k(2), k(3)], &[true, true, false], &[None, None, Some(true)]), 1);
        store.insert(k(4));
        store.insert(k(5));
        assert!(!store.pool.contains(&k(1)) && !store.pool.contains(&k(2)), "the cap pushed both out");
        assert!(store.holds(&k(1)) && store.holds(&k(2)) && store.holds(&k(3)));
        store.block.clear();
        assert!(!store.holds(&k(1)) && !store.holds(&k(3)));

        store.insert(k(6));
        let refused = store.settle(&[k(6), k(7), k(8)], &[true, false, false], &[None, Some(true), Some(false)]);
        assert_eq!(refused, 1, "the miss that passed is kept");
        assert!(store.block.contains(&k(7)) && !store.block.contains(&k(6)) && !store.block.contains(&k(8)));
    }

    /// The key covers the exact bytes. One byte more, less or different is
    /// another key.
    #[test]
    fn the_key_is_the_exact_bytes() {
        let tx = [7u8; 263];
        assert_eq!(key(&tx), key(&[7u8; 263]));
        assert_ne!(key(&tx), key(&tx[..262]));
        let mut flipped = tx;
        flipped[5] ^= 1;
        assert_ne!(key(&tx), key(&flipped));
    }
}
