//! Program loading: the block's text budget, the parallel pre-warm and the
//! cache's byte budget.
//!
//! Only the text budget is consensus. It reads account data the block already
//! loaded and counts in block order, so every machine computes the same set
//! and the same refusals. The cache budget and the pre-warm pool are
//! machine-local, because consensus cannot see which programs are compiled or
//! when.

use solana_account::{AccountSharedData, ReadableAccount};
use solana_loader_v3_interface::state::UpgradeableLoaderState;
use solana_loader_v4_interface::state::LoaderV4State;
use solana_program_runtime::loaded_programs::ProgramCache;
use solana_program_runtime::program_cache_entry::{ProgramCacheEntry, ProgramCacheEntryType};
use solana_svm::transaction_processing_callback::TransactionProcessingCallback;
use solana_svm_timings::ExecuteTimings;
use solana_pubkey::Pubkey;
use solana_sdk_ids::{bpf_loader, bpf_loader_deprecated, bpf_loader_upgradeable, loader_v4};
use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

/// A block may reference at most this much distinct executable program text.
/// Consensus, unlike `CACHE_BYTES` below.
///
/// It is a static, not a constant, for one reason. A constant would make a test
/// need 400 MB of account data to reach the boundary, because the count is
/// clamped to what a program stores. `svm_block_program_text_for_tests` is the
/// only writer, and `svmd` never calls it. `svm_block_limits` always reports
/// the value a block is judged against.
static BLOCK_PROGRAM_TEXT: AtomicU64 = AtomicU64::new(400_000_000);

pub fn max_block_program_text() -> u64 {
    BLOCK_PROGRAM_TEXT.load(Ordering::Relaxed)
}

/// See `BLOCK_PROGRAM_TEXT`. The value is consensus. Every machine in a
/// network must hold the same value, so nothing outside a test may move it.
pub fn set_max_block_program_text_for_tests(bytes: u64) {
    BLOCK_PROGRAM_TEXT.store(bytes, Ordering::Relaxed);
}

/// The program cache's byte budget, in resident bytes, machine-local. Zero
/// means unbounded.
pub static CACHE_BYTES: AtomicU64 = AtomicU64::new(1536 * 1024 * 1024);

/// ELF constants. `p_type` PT_LOAD and `p_flags` PF_X are the two this reads.
const PT_LOAD: u32 = 1;
const PF_X: u32 = 1;

/// The executable text of an ELF: the sum of `p_filesz` over the `PT_LOAD`
/// segments flagged `PF_X`, and never more than the file.
///
/// This is a header parse, not a verify. It walks the program header table and
/// nothing else, so a file that is not a loadable program returns whatever its
/// header claims and fails later, at the load the budget was protecting.
/// Anything that is not a 64-bit little-endian ELF is zero.
pub fn text_bytes(elf: &[u8]) -> u64 {
    // e_ident: magic, then EI_CLASS 2 (64-bit) and EI_DATA 1 (little-endian).
    if elf.len() < 64 || elf[..4] != [0x7f, b'E', b'L', b'F'] || elf[4] != 2 || elf[5] != 1 {
        return 0;
    }
    let u16_at = |at: usize| u16::from_le_bytes([elf[at], elf[at + 1]]) as usize;
    let u32_at = |at: usize| u32::from_le_bytes(elf[at..at + 4].try_into().unwrap());
    let u64_at = |at: usize| u64::from_le_bytes(elf[at..at + 8].try_into().unwrap());

    let phoff = u64_at(0x20) as usize;
    let phentsize = u16_at(0x36);
    let phnum = u16_at(0x38);
    if phentsize < 56 {
        return 0;
    }
    let mut text = 0u64;
    for i in 0..phnum {
        let at = match phoff.checked_add(i.saturating_mul(phentsize)) {
            Some(at) if at.saturating_add(phentsize) <= elf.len() => at,
            _ => break,
        };
        if u32_at(at) == PT_LOAD && u32_at(at + 4) & PF_X != 0 {
            // Clamp to what the segment can hold. A header may claim more than
            // the account stores. An unclamped count would let one small
            // account fill a block's budget and fail every other transaction.
            // Spending a budget byte costs storing a byte.
            let stored = (elf.len() as u64).saturating_sub(u64_at(at + 8)); // p_offset
            text = text.saturating_add(u64_at(at + 32).min(stored)); // p_filesz
        }
    }
    // Segments can overlap, and the loader accepts that. Without this limit,
    // the same stored bytes listed in a thousand headers would count a
    // thousand times.
    text.min(elf.len() as u64)
}

/// The fixed part of what one distinct program charges against a block's
/// budget. `charge` adds the text, and the other bytes divided by
/// `OTHER_BYTE_DIVISOR`.
pub const PROGRAM_FIXED_CHARGE: u64 = 8 * 1024;
pub const OTHER_BYTE_DIVISOR: u64 = 8;

/// The charge of a program with `text` bytes of text in `len` bytes of ELF.
pub const fn program_charge(text: u64, len: u64) -> u64 {
    let other = len.saturating_sub(text);
    text.saturating_add(other / OTHER_BYTE_DIVISOR)
        .saturating_add(PROGRAM_FIXED_CHARGE)
}

pub fn charge(elf: &[u8]) -> u64 {
    program_charge(text_bytes(elf), elf.len() as u64)
}

/// The ELF of a program account, wherever its loader keeps it: in the account
/// for loaders v1 and v2, after the 48-byte state for loader v4, and in a
/// separate programdata account after its 45-byte metadata for the
/// upgradeable loader.
///
/// `None` means the SVM would not try to compile the account, because it does
/// not exist or no loader owns it. Every other answer is `Some`. A
/// loader-owned account whose ELF cannot be located gives an empty slice. Such
/// an account is a program that points at missing programdata, a buffer, a
/// programdata account named directly, or data too short to hold what the
/// loader's state claims. Each of those still costs a load and a failed parse,
/// so it charges the fixed part, not nothing.
fn elf<'a>(
    accounts: &'a HashMap<Pubkey, AccountSharedData>,
    key: &Pubkey,
) -> Option<&'a [u8]> {
    let account = accounts.get(key)?;
    let owner = account.owner();
    if bpf_loader::check_id(owner) || bpf_loader_deprecated::check_id(owner) {
        return Some(account.data());
    }
    if loader_v4::check_id(owner) {
        return Some(
            account
                .data()
                .get(LoaderV4State::program_data_offset()..)
                .unwrap_or_default(),
        );
    }
    if bpf_loader_upgradeable::check_id(owner) {
        let Ok(UpgradeableLoaderState::Program { programdata_address }) =
            bincode::deserialize(account.data())
        else {
            return Some(&[]);
        };
        let Some(programdata) = accounts.get(&programdata_address) else {
            return Some(&[]);
        };
        return Some(
            programdata
                .data()
                .get(UpgradeableLoaderState::size_of_programdata_metadata()..)
                .unwrap_or_default(),
        );
    }
    None
}

/// What a block's transactions charge against the program budget, counted in
/// block order.
///
/// `keys` is each transaction's account keys, in block order, with `None` for
/// the transactions that will not run anyway. The result is the programs the
/// block may compile and the indexes of the transactions that crossed the
/// budget.
///
/// Every loader-owned account key counts, invoked or not, because that is what
/// the SVM compiles. A transaction is counted whole. Either all of its new
/// programs fit, or none of them are added and the transaction fails.
pub fn budget<I: IntoIterator<Item = Pubkey>>(
    accounts: &HashMap<Pubkey, AccountSharedData>,
    keys: impl Iterator<Item = Option<I>>,
    limit: u64,
) -> (Vec<Pubkey>, Vec<usize>) {
    let mut seen: HashSet<Pubkey> = HashSet::new();
    let mut order: Vec<Pubkey> = Vec::new();
    let mut over: Vec<usize> = Vec::new();
    let mut used = 0u64;
    let mut fresh: Vec<(Pubkey, u64)> = Vec::new();
    for (i, tx) in keys.enumerate() {
        let Some(tx) = tx else { continue };
        fresh.clear();
        let mut added = 0u64;
        for key in tx {
            if seen.contains(&key) || fresh.iter().any(|(k, _)| *k == key) {
                continue;
            }
            if let Some(elf) = elf(accounts, &key) {
                let cost = charge(elf);
                fresh.push((key, cost));
                added = added.saturating_add(cost);
            }
        }
        if used.saturating_add(added) > limit {
            over.push(i);
            continue;
        }
        used = used.saturating_add(added);
        for (key, _) in fresh.drain(..) {
            seen.insert(key);
            order.push(key);
        }
    }
    crate::stats::set_program_text(used);
    (order, over)
}

/// What one cache entry keeps resident. The budget counts this.
///
/// For the JIT, that is the pages that sbpf writes: the pc table, four bytes
/// for each instruction slot, and the emitted code, each rounded up to a page.
/// sbpf never touches the rest of the power-of-two bucket that it reserves.
/// sbpf counts a text that ends in a two-slot `lddw` as one slot longer, which
/// is at most one page more. Beside the JIT, it adds what
/// `Executable::mem_size` counts: the copy of the ELF, the owned copy of text
/// and rodata, and the function registry. An entry that holds no executable
/// holds nothing.
///
/// `mem_size` walks the function registry, and no public call gives the size
/// of the ELF copy without it. So a count of a full 1.5 GiB cache takes about
/// 0.5 ms, and of a full 4 GiB cache 1.3 ms, against 5 us for the buckets. If
/// that ever shows in block time, count each entry once, when it lands.
fn resident(entry: &ProgramCacheEntry) -> u64 {
    let ProgramCacheEntryType::Loaded(executable) = &entry.program else {
        return 0;
    };
    // x86_64 Linux, the only target, has 4 KiB pages.
    let page = |bytes: usize| (bytes as u64).div_ceil(4096) * 4096;
    let mut bytes = executable.mem_size() as u64;
    if let Some(jit) = executable.get_compiled_program() {
        let slots = executable.get_text_bytes().1.len() / solana_sbpf::ebpf::INSN_SIZE;
        bytes = bytes.saturating_sub(jit.mem_size() as u64) + page(slots * 4) + page(jit.machine_code_length());
    }
    bytes
}

/// Compiles the programs a block is about to reference, across a thread pool,
/// before any transaction runs.
///
/// This changes no result. Which programs are compiled, and when, is invisible
/// to consensus. Execution that finds an entry missing loads it itself. The
/// gain is that the block's cold loads run on `workers()` threads, not serially
/// inside the transactions that need them.
///
/// The pieces are Anza's: `load_program_with_pubkey` and `assign_program`,
/// the same two cooperative loading uses.
///
/// The cache keeps its byte budget, `limit`, while the block compiles. Each
/// program makes room for itself as it lands, in the order of the block-end
/// hook. It never gives up a program that the block references. So the cache
/// holds more than `limit` only when the programs of the block do, and the
/// compiles in flight add at most one program for each worker. Before, the
/// block compiled everything on top of a full cache, and what the block-end
/// hook then freed stayed resident in the pool of sbpf.
pub fn prewarm(
    root: &crate::runtime::Root,
    accounts: &(impl TransactionProcessingCallback + Sync),
    height: u64,
    programs: &[Pubkey],
    limit: u64,
) {
    if programs.is_empty() {
        return;
    }
    let environment = root.processor.program_runtime_environment.clone();
    let cache = &root.processor.global_program_cache;
    let cached: HashSet<Pubkey> = cache
        .read()
        .unwrap()
        .get_flattened_entries()
        .into_iter()
        .map(|(id, _, _)| id)
        .collect();
    let missing: Vec<Pubkey> = programs.iter().copied().filter(|k| !cached.contains(k)).collect();
    crate::stats::add_prewarm_misses(missing.len());
    if missing.is_empty() {
        return;
    }
    let victims = std::sync::Mutex::new(Victims::of(
        &cache.read().unwrap(),
        &programs.iter().copied().collect(),
        &HashSet::new(),
    ));
    crate::runtime::fan_out(
        crate::runtime::workers().min(missing.len()),
        missing.len(),
        |i| {
            let loaded = solana_svm::program_loader::load_program_with_pubkey(
                accounts,
                &environment,
                &missing[i],
                height,
                &mut ExecuteTimings::default(),
            );
            if let Some((entry, slot)) = loaded {
                let mut cache = cache.write().unwrap();
                let mut victims = victims.lock().unwrap();
                victims.held = victims.held.saturating_add(resident(&entry));
                cache.assign_program(&environment, missing[i], slot, entry);
                victims.fit(&mut cache, limit);
            }
        },
    );
}

/// The entries of the cache in the order that the byte budget gives them up,
/// and what the whole cache holds. The order is ascending retention score, the
/// order that the random-selection pass of Agave prefers, taken exactly and not
/// sampled. The key is (score, pubkey), so the pass does not depend on the
/// order the index returns entries in. Consensus does not depend on it, but a
/// reproducible eviction is easier to debug.
struct Victims {
    held: u64,
    /// The last goes first.
    order: Vec<(Pubkey, u64)>,
}

impl Victims {
    /// Every entry counts toward what the cache holds. An entry at a key in
    /// `keep` is never given up. An entry in `shared` is also in the cache of
    /// the block path, whose budget bounds it. So it does not count, and it
    /// does not go.
    fn of(
        cache: &ProgramCache<crate::runtime::Linear>,
        keep: &HashSet<Pubkey>,
        shared: &HashSet<*const ProgramCacheEntry>,
    ) -> Self {
        let mut live = cache.get_flattened_entries();
        live.retain(|(_, _, e)| !shared.contains(&Arc::as_ptr(e)));
        let total = live.iter().map(|(_, _, e)| resident(e)).sum();
        live.retain(|(id, _, _)| !keep.contains(id));
        live.sort_by_key(|(id, _, e)| std::cmp::Reverse((e.retention_score(), *id)));
        Victims { held: total, order: live.iter().map(|(id, _, e)| (*id, resident(e))).collect() }
    }

    /// Gives entries up until the cache holds `limit` or less, or until no
    /// entry that may go is left. A `limit` of zero is unbounded.
    fn fit(&mut self, cache: &mut ProgramCache<crate::runtime::Linear>, limit: u64) {
        let mut gone = Vec::new();
        while limit != 0 && self.held > limit {
            let Some((id, bytes)) = self.order.pop() else { break };
            self.held = self.held.saturating_sub(bytes);
            gone.push(id);
        }
        cache.remove_programs(gone.into_iter());
    }
}

/// The block-end cache hook. It prunes superfluous versions, then applies the
/// byte budget. The entries the block's transactions deployed are already in
/// the cache, merged batch by batch as Agave merges them.
///
/// The budget is the piece the crate does not have. Its own eviction counts
/// entries (`MAX_LOADED_ENTRY_COUNT`), and a block of maximum-size deploys goes
/// straight past that count. This hook bounds the bytes.
pub fn end_block(
    cache: &std::sync::RwLock<ProgramCache<crate::runtime::Linear>>,
    fork_graph: &crate::runtime::Linear,
    height: u64,
) {
    let mut cache = cache.write().unwrap();
    cache.prune(height, None, fork_graph);
    Victims::of(&cache, &HashSet::new(), &HashSet::new()).fit(&mut cache, CACHE_BYTES.load(Ordering::Relaxed));
}

/// Fills the cache of `next`, the root of the block at `height`, before
/// anything runs on it. It puts in the same entries, not copies:
///
/// - Each program in `block`, the root of the block path, that was deployed
///   before `height`. The block in flight deploys at `height`, so none of its
///   deployments cross over. A simulation does not see what the block in
///   flight deploys. An older entry holds the code that committed state
///   holds, or older code, which the deployment slot check reloads.
/// - Each program in `before`, the root of the height before, unless the
///   first step already put that program and deployment there. These are the
///   programs that simulations compiled, and the programs that the block path
///   gave up after it lent them.
///
/// An entry goes only into a root of its own runtime environment. Then the
/// prune and the byte budget of the block-end hook run. One budget bounds
/// both roots. The entries of `next` that the block path does not hold get
/// the part of the budget that the root of the block path does not use.
pub fn lend(
    block: Option<&crate::runtime::Root>,
    before: Option<&crate::runtime::Root>,
    next: &crate::runtime::Root,
    height: u64,
) {
    let env = &next.processor.program_runtime_environment;
    let entries = |root: &crate::runtime::Root| root.processor.global_program_cache.read().unwrap().get_flattened_entries();
    let block = block.map(entries).unwrap_or_default();
    let shared: HashSet<*const ProgramCacheEntry> = block.iter().map(|(_, _, e)| Arc::as_ptr(e)).collect();
    let block_bytes: u64 = block.iter().map(|(_, _, e)| resident(e)).sum();
    let ours = |e: &Arc<ProgramCacheEntry>| e.program.get_environment() == Some(env);
    let mut cache = next.processor.global_program_cache.write().unwrap();
    let mut placed = HashSet::new();
    for (key, _, entry) in block.into_iter().filter(|(_, _, e)| e.deployment_slot < height && ours(e)) {
        placed.insert((key, entry.deployment_slot));
        cache.assign_program(env, key, entry.deployment_slot, entry);
    }
    for (key, _, entry) in before.map(entries).unwrap_or_default() {
        if ours(&entry) && placed.insert((key, entry.deployment_slot)) {
            cache.assign_program(env, key, entry.deployment_slot, entry);
        }
    }
    cache.prune(height, None, &next.fork_graph.read().unwrap());
    let limit = match CACHE_BYTES.load(Ordering::Relaxed) {
        0 => 0,
        all => all.saturating_sub(block_bytes).max(1),
    };
    Victims::of(&cache, &HashSet::new(), &shared).fit(&mut cache, limit);
}

/// The cache's accounted bytes, which is what the budget bounds.
pub fn cache_bytes(cache: &std::sync::RwLock<ProgramCache<crate::runtime::Linear>>) -> u64 {
    cache.read().unwrap().get_flattened_entries().iter().map(|(_, _, e)| resident(e)).sum()
}

/// What the root of the block path and the roots of the next blocks hold
/// together. Each entry counts once, however many roots hold it. The byte
/// budget bounds this value at each height.
pub fn held_bytes() -> u64 {
    let roots = crate::runtime::root().into_iter().chain(crate::runtime::next_roots());
    let mut seen = HashSet::new();
    roots
        .flat_map(|r| r.processor.global_program_cache.read().unwrap().get_flattened_entries())
        .filter(|(_, _, e)| seen.insert(Arc::as_ptr(e)))
        .map(|(_, _, e)| resident(&e))
        .sum()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::Path;
    use std::sync::Arc;

    /// The seven programs in programs/manifest.json and the `text_bytes` the
    /// harness measured with `Executable::get_text_bytes`. A header can claim
    /// other than what the executable holds, as the old serum-v3 dump did.
    /// Then this counts the header, because a validator can read it without
    /// building the executable.
    const CORPUS: [(&str, u64); 7] = [
        ("spl_token.so", 102_608),
        ("serum-v3.so", 262_352),
        ("token-2022.so", 552_928),
        ("openbook-v2.so", 699_088),
        ("kamino-lend.so", 2_113_656),
        ("mango-v4.so", 3_242_336),
        ("drift-v2.so", 4_057_576),
    ];

    #[test]
    fn text_is_the_executable_load_segments() {
        for (name, want) in CORPUS {
            let elf = std::fs::read(programs().join(name)).unwrap();
            assert_eq!(text_bytes(&elf), want, "{name}");
        }
    }

    /// A header may claim more than the account stores. The count is what is
    /// stored, or one small account could fill a block's budget and fail every
    /// other transaction for the price of one.
    #[test]
    fn a_claim_larger_than_the_file_counts_the_file() {
        let mut elf = vec![0u8; 64 + 56 + 10];
        elf[..6].copy_from_slice(&[0x7f, b'E', b'L', b'F', 2, 1]);
        elf[0x20..0x28].copy_from_slice(&64u64.to_le_bytes());
        elf[0x36..0x38].copy_from_slice(&56u16.to_le_bytes());
        elf[0x38..0x3a].copy_from_slice(&1u16.to_le_bytes());
        elf[64..68].copy_from_slice(&PT_LOAD.to_le_bytes());
        elf[68..72].copy_from_slice(&PF_X.to_le_bytes());
        elf[64 + 8..64 + 16].copy_from_slice(&(64u64 + 56).to_le_bytes()); // p_offset
        elf[64 + 32..64 + 40].copy_from_slice(&400_000_000u64.to_le_bytes()); // p_filesz
        assert_eq!(text_bytes(&elf), 10);
    }

    /// Headers can list the same bytes many times. The count is still at most
    /// what is stored, or one 150 KB program could spend the 400 MB budget of a
    /// block.
    #[test]
    fn overlapping_segments_count_the_file_once() {
        let mut elf = vec![0u8; 64 + 3 * 56];
        elf[..6].copy_from_slice(&[0x7f, b'E', b'L', b'F', 2, 1]);
        elf[0x20..0x28].copy_from_slice(&64u64.to_le_bytes());
        elf[0x36..0x38].copy_from_slice(&56u16.to_le_bytes());
        elf[0x38..0x3a].copy_from_slice(&3u16.to_le_bytes());
        let len = elf.len() as u64;
        for at in [64, 64 + 56, 64 + 2 * 56] {
            elf[at..at + 4].copy_from_slice(&PT_LOAD.to_le_bytes());
            elf[at + 4..at + 8].copy_from_slice(&PF_X.to_le_bytes());
            elf[at + 32..at + 40].copy_from_slice(&len.to_le_bytes()); // p_filesz, p_offset 0
        }
        assert_eq!(text_bytes(&elf), len);
    }

    #[test]
    fn a_file_that_is_not_an_elf_has_no_text() {
        assert_eq!(text_bytes(&[]), 0);
        assert_eq!(text_bytes(&[0; 64]), 0);
        let mut elf = vec![0x7f, b'E', b'L', b'F', 2, 1];
        elf.resize(64, 0);
        assert_eq!(text_bytes(&elf), 0, "an ELF with no program headers");
    }

    /// programs/, where `make fetch-programs` downloads every program a test
    /// reads and checks it against programs/manifest.json.
    fn programs() -> std::path::PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../programs")
    }

    /// Every program this repository ships or benchmarks, smallest first, by
    /// its path from the repository root, for the compile-rate sweep. `make
    /// fetch-programs` downloads them. programs/ holds the genesis Token-2022
    /// and SPL Token again, byte for byte, so only one copy of each is here.
    const SWEEP: [&str; 9] = [
        "genesis/programs/memo-v1.so",
        "genesis/programs/ata.so",
        "programs/spl_token.so",
        "programs/serum-v3.so",
        "genesis/programs/token-2022.so",
        "programs/openbook-v2.so",
        "programs/kamino-lend.so",
        "programs/mango-v4.so",
        "programs/drift-v2.so",
    ];

    /// Builds `copies` loader-v2 accounts holding `elf`, and their keys.
    fn spread(
        accounts: &mut HashMap<Pubkey, solana_account::AccountSharedData>,
        elf: &[u8],
        copies: usize,
    ) -> Vec<Pubkey> {
        let data = Arc::new(elf.to_vec());
        (0..copies)
            .map(|_| {
                let key = Pubkey::new_unique();
                accounts.insert(
                    key,
                    solana_account::AccountSharedData::create_from_existing_shared_data(
                        1_000_000_000,
                        data.clone(),
                        bpf_loader::id(),
                        true,
                        0,
                    ),
                );
                key
            })
            .collect()
    }

    /// Times one cold pre-warm of `keys` at `threads`, best of three.
    fn time_prewarm(
        arena: &crate::runtime::Arena,
        keys: &[Pubkey],
        threads: usize,
    ) -> f64 {
        let mut best = f64::MAX;
        for _ in 0..3 {
            crate::runtime::configure(&[]);
            crate::runtime::set_threads(threads);
            let root = crate::runtime::root().unwrap();
            let start = std::time::Instant::now();
            prewarm(&root, arena, 1, keys, CACHE_BYTES.load(Ordering::Relaxed));
            best = best.min(start.elapsed().as_secs_f64());
        }
        best
    }

    #[test]
    #[ignore]
    fn compile_rate_by_size() {
        let root_dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("..");
        let mut rows = Vec::new();
        for name in SWEEP {
            let elf = std::fs::read(root_dir.join(name)).unwrap();
            let text = text_bytes(&elf);
            // Aim at about 8 MB of text per program so that even the smallest
            // is timed over tens of milliseconds, capped so that the largest
            // does not hold gigabytes of JIT.
            let copies = ((8_000_000 / text.max(1)) as usize).clamp(2, 600);
            let mut accounts = HashMap::new();
            let keys = spread(&mut accounts, &elf, copies);
            let arena = crate::runtime::Arena(accounts);
            let elapsed = time_prewarm(&arena, &keys, 1);
            let per = elapsed * 1e6 / copies as f64;
            rows.push((name, text, elf.len(), copies, per));
            println!(
                "compile program={name} text_bytes={text} file_bytes={} copies={copies} \
                 per_program_us={per:.1} us_per_mb={:.1}",
                elf.len(),
                per / (text as f64 / 1e6),
            );
        }

        // The worst case is a block that spends the whole budget on the
        // smallest program this repository has. 400 MB of 13 KB programs is
        // 30,229 of them, which is more JIT than a test should hold. So this
        // runs a tenth of it and reports the projection.
        let (name, text, _, _, _) = rows[0];
        let copies = (40_000_000 / text) as usize;
        let mut accounts = HashMap::new();
        let elf = std::fs::read(root_dir.join(name)).unwrap();
        let keys = spread(&mut accounts, &elf, copies);
        let arena = crate::runtime::Arena(accounts);
        for threads in [1usize, 4] {
            let elapsed = time_prewarm(&arena, &keys, threads);
            println!(
                "smallest program={name} text_bytes={text} programs={copies} threads={threads} \
                 elapsed_ms={:.1} projected_400mb_s={:.2}",
                elapsed * 1e3,
                elapsed * 10.0,
            );
        }
        crate::runtime::set_threads(0);
    }

    /// A program charges more than its text. A block of small programs compiles far
    /// more of them per budgeted byte than the corpus does. A program whose account
    /// is mostly not executable still costs to read and verify. A loader-owned
    /// account that is not a program still costs a load.
    #[test]
    fn a_program_charges_its_fixed_part_and_its_other_bytes() {
        assert_eq!(charge(&[]), PROGRAM_FIXED_CHARGE, "not a program, still a load");

        let memo = std::fs::read(Path::new(env!("CARGO_MANIFEST_DIR")).join("../genesis/programs/memo-v1.so"))
            .expect("genesis/programs/memo-v1.so: run make fetch-programs");
        let text = text_bytes(&memo);
        assert_eq!(text, 13_232);
        // 13,232 of text, 3,840 other bytes, and the fixed part.
        assert_eq!(charge(&memo), text + (17_072 - text) / 8 + PROGRAM_FIXED_CHARGE);

        // At 400 MB a text-only budget would admit 30,229 of these, and the
        // charge admits 18,261.
        let budget = 400_000_000u64;
        assert_eq!(budget / text, 30_229);
        assert_eq!(budget / charge(&memo), 18_261);
    }

    #[test]
    fn a_program_charges_its_text_an_eighth_of_its_other_bytes_and_8_kib() {
        assert_eq!(program_charge(0, 0), 8_192);
        assert_eq!(program_charge(100, 100), 8_292);
        assert_eq!(program_charge(100, 117), 8_294, "17 other bytes charge 2");
        assert_eq!(program_charge(200, 100), 8_392, "text past the end");
        assert_eq!(program_charge(u64::MAX, u64::MAX), u64::MAX);
    }

    /// Measures how compilation scales with cores, instead of assuming that
    /// four cores divide serial compile time by four.
    ///
    /// Ignored by default, because it compiles tens of megabytes of text and
    /// belongs on the four-core reference machine, not in `make test`. Run it
    /// with `cargo test --release --locked programs::tests::prewarm_speedup --
    /// --ignored --nocapture`.
    #[test]
    #[ignore]
    fn prewarm_speedup() {
        let mut elves = Vec::new();
        for (name, _) in CORPUS {
            elves.push(std::fs::read(programs().join(name)).unwrap());
        }
        // Four distinct keys per ELF, because a block's budget is distinct
        // programs and one key compiles once. 44 MB of text, about a ninth of
        // the 400 MB budget.
        const COPIES: usize = 4;
        let mut accounts = HashMap::new();
        let mut keys = Vec::new();
        let mut text = 0u64;
        for _ in 0..COPIES {
            for elf in &elves {
                let key = Pubkey::new_unique();
                text += text_bytes(elf);
                accounts.insert(
                    key,
                    solana_account::AccountSharedData::create_from_existing_shared_data(
                        1_000_000_000,
                        Arc::new(elf.clone()),
                        bpf_loader::id(),
                        true,
                        0,
                    ),
                );
                keys.push(key);
            }
        }
        let arena = crate::runtime::Arena(accounts);

        // One untimed pass first. The JIT allocator keeps a process-global bucketed free
        // list, so the first run in a process pays every page fault and the ones after
        // it reuse the buckets. Timing without this makes four threads look superlinear.
        crate::runtime::configure(&[]);
        crate::runtime::set_threads(1);
        prewarm(&crate::runtime::root().unwrap(), &arena, 1, &keys, CACHE_BYTES.load(Ordering::Relaxed));

        let mut serial = 0f64;
        for threads in [1usize, 2, 4, 8] {
            let mut elapsed = f64::MAX;
            for _ in 0..3 {
                // A fresh root is a cold cache, which is what a pre-warm faces.
                crate::runtime::configure(&[]);
                crate::runtime::set_threads(threads);
                let root = crate::runtime::root().unwrap();
                let start = std::time::Instant::now();
                prewarm(&root, &arena, 1, &keys, CACHE_BYTES.load(Ordering::Relaxed));
                elapsed = elapsed.min(start.elapsed().as_secs_f64());
            }
            if threads == 1 {
                serial = elapsed;
            }
            println!(
                "prewarm threads={threads} programs={} text_mb={:.2} elapsed_ms={:.1} \
                 speedup={:.2}x ms_per_mb={:.2} projected_400mb_s={:.2}",
                keys.len(),
                text as f64 / 1e6,
                elapsed * 1e3,
                serial / elapsed,
                elapsed * 1e3 / (text as f64 / 1e6),
                elapsed * 400e6 / text as f64,
            );
        }
        crate::runtime::set_threads(0);
    }

    /// The JIT bucket that sbpf reserves for an entry: a power of two.
    fn bucket(entry: &ProgramCacheEntry) -> u64 {
        match &entry.program {
            ProgramCacheEntryType::Loaded(executable) => {
                executable.get_compiled_program().map_or(0, |jit| jit.mem_size() as u64)
            }
            _ => 0,
        }
    }

    /// Reads /proc/self/smaps: the resident bytes of the process, its
    /// anonymous bytes, and the resident bytes of its anonymous r-x and r--
    /// mappings. A sealed JIT program is exactly those last two: its code and
    /// its pc table. A freed one goes back to read-write in the pool of sbpf
    /// and leaves both.
    fn smaps() -> [u64; 4] {
        let smaps = std::fs::read_to_string("/proc/self/smaps").unwrap();
        let (mut out, mut perms, mut anonymous) = ([0u64; 4], "", false);
        for line in smaps.lines() {
            let mut fields = line.split_whitespace();
            let (Some(first), Some(second)) = (fields.next(), fields.next()) else { continue };
            if !first.ends_with(':') {
                // A mapping: range, perms, offset, device, inode, then a path
                // unless it is anonymous.
                perms = second;
                anonymous = fields.nth(3).is_none();
                continue;
            }
            let bytes = second.parse::<u64>().unwrap_or(0) * 1024;
            match first {
                "Rss:" => {
                    out[0] += bytes;
                    if anonymous && perms == "r-xp" {
                        out[2] += bytes;
                    }
                    if anonymous && perms == "r--p" {
                        out[3] += bytes;
                    }
                }
                "Anonymous:" => out[1] += bytes,
                _ => {}
            }
        }
        out
    }

    /// The peak resident bytes of the process since the last `clear_refs`
    /// reset.
    fn peak_rss() -> u64 {
        let status = std::fs::read_to_string("/proc/self/status").unwrap();
        let line = status.lines().find(|l| l.starts_with("VmHWM:")).unwrap();
        line.split_whitespace().nth(1).unwrap().parse::<u64>().unwrap() * 1024
    }

    /// Measures one cache entry in three ways, for every program in the
    /// repository, 13 KB to 4.1 MB of text. `held` is the JIT bucket. `gauge`
    /// is what the cache budget counts. `jit` is what smaps finds resident in
    /// the r-x and r-- mappings of the JIT. `anon` is the anonymous growth of
    /// the process, which adds the ELF and rodata copies that the JIT does not
    /// hold. Each program compiles into a root of its own that the test keeps,
    /// so no allocation goes back to the pool of sbpf and none is used again.
    /// Bytes are for each copy.
    ///
    /// Ignored by default. Run it alone, with `--ignored --exact --nocapture`.
    #[test]
    #[ignore]
    fn cache_bytes_by_program() {
        let root_dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("..");
        let mut kept = Vec::new();
        for name in SWEEP {
            let elf = std::fs::read(root_dir.join(name)).unwrap();
            let text = text_bytes(&elf);
            let copies = ((8_000_000 / text.max(1)) as usize).clamp(2, 600);
            let mut accounts = HashMap::new();
            let keys = spread(&mut accounts, &elf, copies);
            let arena = crate::runtime::Arena(accounts);
            let root = crate::runtime::build_root(&[]);
            let before = smaps();
            prewarm(&root, &arena, 1, &keys, 0);
            let after = smaps();
            let cache = &root.processor.global_program_cache;
            let held: u64 =
                cache.read().unwrap().get_flattened_entries().iter().map(|(_, _, e)| bucket(e)).sum();
            let per = |bytes: u64| bytes / copies as u64;
            println!(
                "entry program={name} text_bytes={text} file_bytes={} copies={copies} held={} gauge={} \
                 jit={} jit_rx={} jit_r={} anon={}",
                elf.len(),
                per(held),
                per(cache_bytes(cache)),
                per(after[2] + after[3] - before[2] - before[3]),
                per(after[2] - before[2]),
                per(after[3] - before[3]),
                per(after[1].saturating_sub(before[1])),
            );
            kept.push((root, arena));
        }
    }

    /// Measures what a block that compiles a lot leaves behind when the cache
    /// is already full. Blocks of about 100 MB of new program text fill the
    /// cache until its budget evicts. Then two blocks of about 400 MB each,
    /// the whole program text budget of a block, compile new programs. smaps
    /// gives the resident bytes of the JIT and the anonymous and resident
    /// bytes of the process before and after each block. VmHWM gives the peak
    /// in between. Freed JIT allocations stay resident in the pool of sbpf, so
    /// the anonymous growth is what the process keeps. The test also times one
    /// count of the full cache, the pass that the gauge and each eviction make.
    ///
    /// Ignored by default. Run it alone, with `--ignored --exact --nocapture`.
    #[test]
    #[ignore]
    fn cache_peak_while_a_block_compiles() {
        let elves: Vec<Vec<u8>> =
            CORPUS.iter().map(|(name, _)| std::fs::read(programs().join(name)).unwrap()).collect();
        let set: u64 = elves.iter().map(|elf| text_bytes(elf)).sum();
        let mut accounts = HashMap::new();
        let mut block = |sets: usize| {
            let mut keys = Vec::new();
            for elf in &elves {
                keys.extend(spread(&mut accounts, elf, sets));
            }
            keys
        };
        let warm: Vec<Vec<Pubkey>> = (0..16).map(|_| block(9)).collect();
        let sets = (400_000_000 / set) as usize;
        let big: Vec<Vec<Pubkey>> = (0..2).map(|_| block(sets)).collect();
        let arena = crate::runtime::Arena(accounts);

        let root = crate::runtime::build_root(&[]);
        let cache = &root.processor.global_program_cache;
        let report = |what: String| {
            let live = cache.read().unwrap().get_flattened_entries();
            let m = smaps();
            println!(
                "{what} entries={} held={} gauge={} jit={} anon={} rss={}",
                live.len(),
                live.iter().map(|(_, _, e)| bucket(e)).sum::<u64>(),
                cache_bytes(cache),
                m[2] + m[3],
                m[1],
                m[0],
            );
            m
        };

        let (mut height, mut compiled) = (1, 0);
        for keys in &warm {
            prewarm(&root, &arena, height, keys, CACHE_BYTES.load(Ordering::Relaxed));
            end_block(cache, &root.fork_graph.read().unwrap(), height);
            height += 1;
            compiled += keys.len();
            if cache.read().unwrap().get_flattened_entries().len() < compiled {
                break;
            }
        }
        let start = std::time::Instant::now();
        for _ in 0..10 {
            std::hint::black_box(cache_bytes(cache));
        }
        println!("count cache_bytes_us={:.1}", start.elapsed().as_secs_f64() * 1e5);

        let mut before = report(format!("full blocks={}", height - 1));
        for (i, keys) in big.iter().enumerate() {
            std::fs::write("/proc/self/clear_refs", "5").unwrap();
            let start = std::time::Instant::now();
            prewarm(&root, &arena, height, keys, CACHE_BYTES.load(Ordering::Relaxed));
            let elapsed = start.elapsed().as_secs_f64();
            let peak = peak_rss();
            report(format!(
                "compiled block={i} programs={} text_bytes={} prewarm_ms={:.1}",
                keys.len(),
                set * sets as u64,
                elapsed * 1e3
            ));
            end_block(cache, &root.fork_graph.read().unwrap(), height);
            height += 1;
            let after = report(format!("end block={i}"));
            println!(
                "growth block={i} peak_rss={} rss={} anon={} jit={}",
                peak.saturating_sub(before[0]),
                after[0] as i64 - before[0] as i64,
                after[1] as i64 - before[1] as i64,
                (after[2] + after[3]) as i64 - (before[2] + before[3]) as i64,
            );
            before = after;
        }
    }

    /// Measures an RPC node that simulates the programs that its blocks run.
    /// Blocks compile the corpus under new keys until the cache of the block
    /// path evicts, as in `cache_peak_while_a_block_compiles`. Then Go names
    /// the next height. One simulation runs each program that the block path
    /// holds, in two passes. Then Go names the height after. The test prints
    /// the time to name a height, the time for each simulation, and the
    /// anonymous growth of the process. It also prints what the root of the
    /// block path and the next root hold together, with each entry counted
    /// once.
    ///
    /// Ignored by default. Run it alone, with `--ignored --exact --nocapture`.
    #[test]
    #[ignore]
    fn simulations_of_the_block_paths_programs() {
        use solana_message::Message;
        use solana_transaction::{versioned::VersionedTransaction, Transaction};
        let _guard = crate::runtime::config_lock();
        let elves: Vec<Vec<u8>> =
            CORPUS.iter().map(|(name, _)| std::fs::read(programs().join(name)).unwrap()).collect();
        let mut accounts = HashMap::new();
        let mut blocks = Vec::new();
        for _ in 0..16 {
            blocks.push(elves.iter().flat_map(|elf| spread(&mut accounts, elf, 9)).collect::<Vec<_>>());
        }
        let arena = crate::runtime::Arena(accounts);
        let payer = Pubkey::new_unique();
        let mut base: HashMap<Pubkey, AccountSharedData> =
            crate::conformance::sysvar_accounts(crate::conformance::RENT_PER_BYTE_YEAR)
                .into_iter()
                .map(|(k, a)| (k, a.into()))
                .collect();
        base.insert(payer, AccountSharedData::new(10_000_000_000, 0, &solana_sdk_ids::system_program::ID));
        base.insert(
            bpf_loader::ID,
            solana_account::Account {
                lamports: 1,
                data: b"solana_bpf_loader_program".to_vec(),
                owner: solana_sdk_ids::native_loader::ID,
                executable: true,
                rent_epoch: u64::MAX,
            }
            .into(),
        );

        crate::runtime::configure(&[]);
        let block = crate::runtime::root().unwrap();
        let cache = &block.processor.global_program_cache;
        let (mut height, mut compiled) = (1, 0);
        for keys in &blocks {
            prewarm(&block, &arena, height, keys, CACHE_BYTES.load(Ordering::Relaxed));
            end_block(cache, &block.fork_graph.read().unwrap(), height);
            height += 1;
            compiled += keys.len();
            if cache.read().unwrap().get_flattened_entries().len() < compiled {
                break;
            }
        }
        let working: Vec<Pubkey> = cache.read().unwrap().get_flattened_entries().into_iter().map(|(k, _, _)| k).collect();
        let together = |next: &Arc<crate::runtime::Root>| {
            let mut seen = HashSet::new();
            [&block.processor.global_program_cache, &next.processor.global_program_cache]
                .iter()
                .flat_map(|c| c.read().unwrap().get_flattened_entries())
                .filter(|(_, _, e)| seen.insert(Arc::as_ptr(e)))
                .map(|(_, _, e)| resident(&e))
                .sum::<u64>()
        };
        let name = |height: u64| {
            let start = std::time::Instant::now();
            crate::runtime::configure_next(height, &[]);
            let us = start.elapsed().as_secs_f64() * 1e6;
            let next = crate::runtime::next_root(height).unwrap();
            println!(
                "named height={height} us={us:.0} block_entries={} block_bytes={} next_entries={} together={}",
                working.len(),
                cache_bytes(cache),
                next.processor.global_program_cache.read().unwrap().get_flattened_entries().len(),
                together(&next),
            );
            next
        };

        let next = name(height);
        let before = smaps();
        for pass in 0..2 {
            let start = std::time::Instant::now();
            let mut ran = 0;
            for key in &working {
                let mut message =
                    Message::new(&[solana_instruction::Instruction::new_with_bytes(*key, &[0], vec![])], Some(&payer));
                message.recent_blockhash = solana_hash::Hash::new_from_array([5; 32]);
                let bytes = wincode::serialize(&VersionedTransaction::from(Transaction::new_unsigned(message))).unwrap();
                let mut a = base.clone();
                a.insert(*key, arena.0[key].clone());
                let sim = crate::runtime::simulate(height, [5; 32], [6; 32], &bytes, false, true, false, a, &HashSet::new());
                ran += sim.is_ok_and(|s| s.processed) as usize;
            }
            let us = start.elapsed().as_secs_f64() * 1e6 / working.len() as f64;
            let m = smaps();
            println!(
                "pass={pass} sims={} processed={ran} us_per_sim={us:.0} anon_growth={} rss_growth={} together={}",
                working.len(),
                m[1] as i64 - before[1] as i64,
                m[0] as i64 - before[0] as i64,
                together(&next),
            );
        }
        name(height + 1);
    }

    /// The cache keeps its budget while a block compiles, not only at block
    /// end, and never gives up a program that the block references to make
    /// room. Before, the block compiled on top of a full cache, and the cache
    /// held the old programs and the new ones together until block end.
    #[test]
    fn a_block_compiles_inside_the_cache_budget() {
        let elf = std::fs::read(Path::new(env!("CARGO_MANIFEST_DIR")).join("../genesis/programs/memo-v1.so"))
            .expect("genesis/programs/memo-v1.so: run make fetch-programs");
        let mut accounts = HashMap::new();
        let old = spread(&mut accounts, &elf, 8);
        let new = spread(&mut accounts, &elf, 8);
        let arena = crate::runtime::Arena(accounts);
        let root = crate::runtime::build_root(&[]);
        let cache = &root.processor.global_program_cache;

        prewarm(&root, &arena, 1, &old, 0);
        // Room for about ten of them.
        let limit = cache_bytes(cache) * 10 / 8;
        prewarm(&root, &arena, 2, &new, limit);

        assert!(cache_bytes(cache) <= limit, "{} bytes against a {limit} budget", cache_bytes(cache));
        let kept: HashSet<Pubkey> =
            cache.read().unwrap().get_flattened_entries().into_iter().map(|(id, _, _)| id).collect();
        assert!(new.iter().all(|k| kept.contains(k)), "a program the block references was given up");
        let old_kept = old.iter().filter(|k| kept.contains(k)).count();
        assert!(old_kept > 0 && old_kept < old.len(), "{old_kept} of the old programs kept");
    }

    /// A loader-owned account the SVM cannot read still costs a load and a failed parse,
    /// so it charges the fixed part, not nothing. Otherwise a block could name as many
    /// of them as it had room for.
    #[test]
    fn an_unreadable_program_account_still_charges() {
        let mut accounts = HashMap::new();
        let orphan = Pubkey::new_unique();
        // `UpgradeableLoaderState::Program` pointing at programdata that is
        // not in this block's arena: a tag of 2, then the address.
        let mut data = 2u32.to_le_bytes().to_vec();
        data.extend_from_slice(Pubkey::new_unique().as_ref());
        accounts.insert(
            orphan,
            solana_account::AccountSharedData::create_from_existing_shared_data(
                1,
                Arc::new(data),
                bpf_loader_upgradeable::id(),
                true,
                0,
            ),
        );
        let plain = Pubkey::new_unique();
        accounts.insert(plain, AccountSharedData::default());

        let block = [Some(vec![orphan, plain])];
        let (order, over) = budget(&accounts, block.iter().cloned(), PROGRAM_FIXED_CHARGE);
        assert_eq!(order, vec![orphan], "no loader owns the plain account");
        assert!(over.is_empty());

        // A byte under the fixed charge and the transaction no longer fits.
        let (order, over) = budget(&accounts, block.iter().cloned(), PROGRAM_FIXED_CHARGE - 1);
        assert!(order.is_empty());
        assert_eq!(over, vec![0]);
    }

    /// Transactions are counted in block order, a transaction is counted whole, and one
    /// that crosses the budget takes none of its programs with it.
    #[test]
    fn the_budget_counts_distinct_text_in_block_order() {
        let mut accounts = HashMap::new();
        let mut program = |text: usize| {
            let mut elf = vec![0u8; 64 + 56 + text];
            elf[..6].copy_from_slice(&[0x7f, b'E', b'L', b'F', 2, 1]);
            elf[0x20..0x28].copy_from_slice(&64u64.to_le_bytes()); // e_phoff
            elf[0x36..0x38].copy_from_slice(&56u16.to_le_bytes()); // e_phentsize
            elf[0x38..0x3a].copy_from_slice(&1u16.to_le_bytes()); // e_phnum
            elf[64..68].copy_from_slice(&PT_LOAD.to_le_bytes());
            elf[68..72].copy_from_slice(&PF_X.to_le_bytes());
            elf[64 + 32..64 + 40].copy_from_slice(&(text as u64).to_le_bytes());
            let key = Pubkey::new_unique();
            accounts.insert(
                key,
                AccountSharedData::create_from_existing_shared_data(1, std::sync::Arc::new(elf), bpf_loader::id(), true, 0),
            );
            key
        };
        let (a, b, c) = (program(100), program(100), program(100));
        let plain = Pubkey::new_unique();
        accounts.insert(plain, AccountSharedData::default());

        // Each of these charges its 100 bytes of text plus the fixed part, so
        // the limit is written in charges rather than in text bytes.
        let one = PROGRAM_FIXED_CHARGE + 100;
        let block = [
            Some(vec![a, plain]),
            Some(vec![a, b]), // a is already charged, so this adds one more
            None,
            Some(vec![c]), // crosses two and a half charges
            Some(vec![a]), // and this one still fits, because c was not added
        ];
        let (order, over) =
            budget(&accounts, block.iter().cloned(), one * 5 / 2);
        assert_eq!(order, vec![a, b]);
        assert_eq!(over, vec![3]);
    }
}
