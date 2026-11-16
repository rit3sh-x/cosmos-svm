//! Execution: one root `TransactionBatchProcessor` for the process, one child
//! per block, and Agave's replay path for everything the SVM does not compute
//! itself.
//!
//! The engine reads no state. The callback serves only the accounts that Go put
//! in the execute arena, sysvars included.

use crate::abi;
use agave_feature_set::FeatureSet;
use agave_precompiles::{get_precompile, is_precompile};
use agave_reserved_account_keys::ReservedAccountKeys;
use solana_account::{AccountSharedData, ReadableAccount, WritableAccount};
use solana_nonce::state::DurableNonce;
use solana_compute_budget::compute_budget::ComputeBudget;
use solana_fee::{calculate_fee_details, FeeFeatures};
use solana_hash::Hash;
use solana_message::{v0::LoadedAddresses, SanitizedMessage, SanitizedVersionedMessage, SimpleAddressLoader, VersionedMessage};
use solana_program_runtime::{
    execution_budget::SVMTransactionExecutionAndFeeBudgetLimits,
    invoke_context::BuiltinFunctionRegisterer,
    loaded_programs::{BlockRelation, ForkGraph, ProgramRuntimeEnvironment, ProgramRuntimeEnvironments},
    program_cache_entry::{ProgramCacheEntry, DELAY_VISIBILITY_SLOT_OFFSET},
};
use solana_program_runtime::solana_sbpf::program::BuiltinFunctionDefinition;
use solana_precompile_error::PrecompileError;
use solana_pubkey::Pubkey;
use solana_rent::Rent;
use solana_slot_hashes::SlotHashes;
use solana_runtime_transaction::{
    runtime_transaction::RuntimeTransaction,
    transaction_meta::{TransactionConfiguration, TransactionMeta},
};
use solana_sdk_ids::{bpf_loader, bpf_loader_deprecated, bpf_loader_upgradeable, compute_budget};
use solana_svm::transaction_balances::SvmTokenInfo;
use solana_svm::{
    account_loader::{CheckedTransactionDetails, TransactionCheckResult},
    transaction_processing_result::{ProcessedTransaction, TransactionProcessingResult},
    transaction_processor::{
        TransactionBatchProcessor, TransactionProcessingConfig, TransactionProcessingEnvironment,
    },
};
use solana_svm_callback::{AccountState, InvokeContextCallback, TransactionProcessingCallback};
use solana_svm_transaction::svm_message::{SVMMessage, SVMStaticMessage};
use solana_transaction::{
    sanitized::{MessageHash, SanitizedTransaction},
    versioned::{TransactionVersion, VersionedTransaction},
};
use solana_transaction_error::TransactionError;
use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, LazyLock, Mutex, RwLock};

/// `fee_structure.lamports_per_signature`, a constant upstream.
pub const LAMPORTS_PER_SIGNATURE: u64 = 5_000;
/// Genesis `EpochSchedule.slots_per_epoch`.
pub const SLOTS_PER_EPOCH: u64 = 432_000;

/// A slot's epoch: `EpochSchedule::get_epoch` from solana-epoch-schedule, with
/// no warmup. So epoch 0 is a whole epoch from slot 0.
pub fn epoch_of(slot: u64) -> u64 {
    slot / SLOTS_PER_EPOCH
}

/// The builtin table. Names are Agave's `BUILTINS` names, which are also the
/// data of the mainnet builtin accounts. Vote would be one more row here.
///
/// The table has no gate column. For the ZK ElGamal proof row, Agave's
/// `BuiltinPrototype` carries an `enable_feature_id`, but that gate is class
/// `cleaned`. This chain never reads it, so the row is unconditional and the
/// classification stays true. The program's own counter-gates
/// (`disable_zk_elgamal_proof_program`, `reenable_zk_elgamal_proof_program`)
/// are class `svm`. They reach the program through the feature set the engine
/// already passes, so they work without a column here.
pub const BUILTINS: [(Pubkey, &str, BuiltinFunctionRegisterer); 6] = [
    (
        solana_sdk_ids::system_program::ID,
        "solana_system_program",
        solana_system_program::system_processor::Entrypoint::register,
    ),
    (
        bpf_loader_deprecated::ID,
        "solana_bpf_loader_program",
        solana_bpf_loader_program::Entrypoint::register,
    ),
    (
        bpf_loader::ID,
        "solana_bpf_loader_program",
        solana_bpf_loader_program::Entrypoint::register,
    ),
    (
        bpf_loader_upgradeable::ID,
        "solana_bpf_loader_upgradeable_program",
        solana_bpf_loader_program::Entrypoint::register,
    ),
    (
        compute_budget::ID,
        "compute_budget_program",
        solana_compute_budget_program::Entrypoint::register,
    ),
    (
        solana_sdk_ids::zk_elgamal_proof_program::ID,
        "zk_elgamal_proof_program",
        solana_zk_elgamal_proof_program::Entrypoint::register,
    ),
];

/// The ids `SysvarCache::fill_missing_entries` asks for, in its order.
/// Phase 1 always asks Go for all nine.
pub const SYSVAR_IDS: [Pubkey; 9] = [
    solana_sdk_ids::sysvar::clock::ID,
    solana_sdk_ids::sysvar::epoch_schedule::ID,
    solana_sdk_ids::sysvar::epoch_rewards::ID,
    solana_sdk_ids::sysvar::rent::ID,
    solana_sdk_ids::sysvar::slot_hashes::ID,
    solana_sdk_ids::sysvar::stake_history::ID,
    solana_sdk_ids::sysvar::last_restart_slot::ID,
    solana_sdk_ids::sysvar::fees::ID,
    solana_sdk_ids::sysvar::recent_blockhashes::ID,
];

/// Slots are block heights and heights never fork, so ancestry is ordering.
pub struct Linear;

impl ForkGraph for Linear {
    fn relationship(&self, a: u64, b: u64) -> BlockRelation {
        match a.cmp(&b) {
            std::cmp::Ordering::Less => BlockRelation::Ancestor,
            std::cmp::Ordering::Equal => BlockRelation::Equal,
            std::cmp::Ordering::Greater => BlockRelation::Descendant,
        }
    }
}

pub struct Root {
    pub fork_graph: Arc<RwLock<Linear>>,
    pub processor: TransactionBatchProcessor<Linear>,
    pub feature_set: FeatureSet,
    pub reserved: HashSet<Pubkey>,
    /// The gates it was built for, each with its activation height.
    pub gates: Vec<(Pubkey, u64)>,
}

static ROOT: RwLock<Option<Arc<Root>>> = RwLock::new(None);

/// The configured root, or `None` before Go has handed the engine its feature
/// set. Every block path treats `None` as a reason to halt.
pub fn root() -> Option<Arc<Root>> {
    ROOT.read().unwrap().clone()
}

/// Rebuilds the root processor for the gates active in `svmmeta`, each with
/// its activation height. Anza's code maps each feature pubkey to its
/// `SVMFeatureSet` field. It calls `FeatureSet::activate` for each key, then
/// `runtime_features()`, never `all_enabled()`, as
/// `Bank::apply_activated_features` does. A rebuild starts with a cold program
/// cache.
pub fn configure(active: &[(Pubkey, u64)]) {
    *ROOT.write().unwrap() = Some(Arc::new(build_root(active)));
}

/// Drops the configured root, so every path answers `Unconfigured` until the
/// next `configure`. Only the tests' hook calls it.
pub fn unconfigure() {
    *ROOT.write().unwrap() = None;
}

/// The roots the next blocks run on. For each of the last two heights Go named
/// as next, it holds the root of that block. A simulation names the height it
/// runs as, and never runs on the block path's root. `configure` swaps that
/// root in the middle of a block. The block in flight also merges its
/// deployments into that root's cache, at a height that a simulation of the
/// same height would see. Two heights are kept, so a simulation that started
/// before a commit still finds its root.
static NEXT: Mutex<Vec<(u64, Arc<Root>)>> = Mutex::new(Vec::new());

/// Names `active` as the gate set of the block at `height`, the block after
/// the committed one. The block gets a root of its own, built before anything
/// can run on it.
///
/// The program cache of the root starts with the same entries, not copies,
/// that two other roots hold. These are the programs that the block path
/// compiled from committed state, and the programs that the root of the
/// height before compiled. Only programs deployed before `height` cross over.
/// The block in flight deploys at `height`, so its deployments never cross
/// over, and a simulation of that block does not see them. An entry goes only
/// into a root built for the same gates, because it was compiled under the
/// runtime environment of those gates. So a root takes the runtime
/// environment of the block path when the gates match. Gates are scheduled at
/// least `MinActivationDelay` blocks ahead, so the gates differ only near an
/// activation height.
pub fn configure_next(height: u64, active: &[(Pubkey, u64)]) {
    let block = root();
    let mut next = NEXT.lock().unwrap();
    let before = next
        .iter()
        .rev()
        .find(|(h, root)| (*h == height || *h + 1 == height) && root.gates == active)
        .map(|(_, root)| root.clone());
    let env = block
        .as_ref()
        .filter(|b| b.gates == active)
        .or(before.as_ref())
        .map(|r| r.processor.program_runtime_environment.clone());
    let root = Arc::new(build_root_in(active, env));
    crate::programs::lend(block.as_deref(), before.as_deref(), &root, height);
    next.retain(|(h, _)| *h != height);
    next.push((height, root));
    if next.len() > 2 {
        next.remove(0);
    }
}

/// The root of the block at `height`, if Go named that height as next.
pub fn next_root(height: u64) -> Option<Arc<Root>> {
    NEXT.lock().unwrap().iter().find(|(h, _)| *h == height).map(|(_, root)| root.clone())
}

/// The roots of the next blocks, oldest first.
pub fn next_roots() -> Vec<Arc<Root>> {
    NEXT.lock().unwrap().iter().map(|(_, root)| root.clone()).collect()
}

/// build_root is `configure` without the global. The differential test uses
/// it to build a root for one run without replacing the engine's.
pub fn build_root(active: &[(Pubkey, u64)]) -> Root {
    build_root_in(active, None)
}

/// `build_root` in an existing runtime environment, which must be one built
/// for the same gates.
fn build_root_in(active: &[(Pubkey, u64)], env: Option<ProgramRuntimeEnvironment>) -> Root {
    let mut feature_set = FeatureSet::default();
    for (id, height) in active {
        feature_set.activate(id, *height);
    }
    let simd_0268 = feature_set.snapshot().raise_cpi_nesting_limit_to_8;
    let budget = ComputeBudget::new_with_defaults(simd_0268);

    let fork_graph = Arc::new(RwLock::new(Linear));
    let mut processor = TransactionBatchProcessor::<Linear>::new_uninitialized(0, 0);
    processor
        .global_program_cache
        .write()
        .unwrap()
        .set_fork_graph(Arc::downgrade(&fork_graph));
    processor.set_execution_cost(budget.to_cost());
    processor.program_runtime_environment = env.unwrap_or_else(|| {
        solana_syscalls::create_program_runtime_environment(
            &feature_set.runtime_features(),
            &budget.to_budget(),
            false,
            false,
        )
        .expect("program runtime environment")
    });
    for (id, _name, register) in BUILTINS {
        processor.add_builtin(id, ProgramCacheEntry::new_builtin(0, register));
    }

    let mut keys = ReservedAccountKeys::default();
    keys.update_active_set(&feature_set);
    Root {
        fork_graph,
        processor,
        feature_set,
        reserved: keys.active,
        gates: active.to_vec(),
    }
}

/// The block path's processing config. It uses Agave's replay values, with
/// all four flags false. The leader's values would make results depend on
/// each machine's cache. Recording is on, as in Agave's replay when a
/// transaction status sender exists. Recording changes what is reported, not
/// what executes.
pub fn replay_config() -> TransactionProcessingConfig<'static> {
    TransactionProcessingConfig {
        limit_to_load_programs: false,
        drop_on_failure: false,
        all_or_nothing: false,
        strict_nonce_size_check: false,
        recording_config: solana_svm::transaction_processor::ExecutionRecordingConfig::new_single_setting(true),
        ..TransactionProcessingConfig::default()
    }
}

/// The execute arena as the SVM's account source.
pub struct Arena(pub HashMap<Pubkey, AccountSharedData>);

/// The feature set passed to the precompile verifiers. Every entry of Anza's
/// `PRECOMPILES` is `feature: None`, and all three verify functions bind their
/// feature set as `_feature_set`. At this pin, the value cannot change a result.
/// `every_precompile_is_ungated` and `precompile_verification_ignores_the_gates`
/// test both halves of that. A crate bump that starts to read the value fails
/// those tests, and does not verify under the wrong rules. So `Arena` stays a
/// bare map, with no `FeatureSet` clone carried through eight construction
/// sites for an argument that nothing reads.
static VERIFIER_GATES: LazyLock<FeatureSet> = LazyLock::new(FeatureSet::default);

/// Precompile dispatch and the epoch stake syscalls, both written out, not
/// inherited from the trait default.
///
/// Verification runs here, inside the SVM, because SIMD-0159
/// (`move_precompile_verification_to_svm`) is active in the pinned gate set.
/// Mainnet takes this path too. The two precompile bodies are Anza's own
/// `ConformanceCallback`.
///
/// The epoch stake answers are zero because this chain has no vote accounts.
/// Cosmos staking sets the validator set. Bonded tokens would be a plausible but
/// wrong number, and a new consensus input with no consumer. Zero is a constant
/// here, never a setting. It feeds execution and so the app hash, and two
/// validators that disagree on it halt the chain.
impl InvokeContextCallback for Arena {
    fn get_epoch_stake(&self) -> u64 {
        0
    }

    fn get_epoch_stake_for_vote_account(&self, _vote_address: &Pubkey) -> u64 {
        0
    }

    fn is_precompile(&self, program_id: &Pubkey) -> bool {
        is_precompile(program_id, |_| true)
    }

    fn process_precompile(
        &self,
        program_id: &Pubkey,
        data: &[u8],
        instruction_datas: Vec<&[u8]>,
    ) -> Result<(), PrecompileError> {
        match get_precompile(program_id, |_| true) {
            Some(precompile) => precompile.verify(data, &instruction_datas, &VERIFIER_GATES),
            None => Err(PrecompileError::InvalidPublicKey),
        }
    }
}

impl TransactionProcessingCallback for Arena {
    fn get_account_shared_data(&self, key: &Pubkey) -> Option<(AccountSharedData, u64)> {
        self.0.get(key).map(|a| (a.clone(), 0))
    }
    fn inspect_account(&self, _: &Pubkey, _: AccountState, _: bool) {}
}

/// Copies the pinned account records in `records` into the map the SVM's
/// callback serves. This is the only copy of the payload on the Rust side.
///
/// # Safety
/// Every record in `records` must be a pinned record whose value Go keeps
/// pinned for this call.
pub unsafe fn arena_accounts(
    req: &abi::Reader,
    records: std::ops::Range<usize>,
) -> Result<HashMap<Pubkey, AccountSharedData>, ()> {
    let mut map = HashMap::with_capacity(records.len());
    for i in records {
        let a = abi::AccountRef::pinned(req.record(i)).ok_or(())?;
        map.insert(Pubkey::from(a.pubkey), shared(&a));
    }
    Ok(map)
}

/// An owned copy of an account read out of an arena record.
pub fn shared(a: &abi::AccountRef) -> AccountSharedData {
    let mut acct = AccountSharedData::default();
    acct.set_lamports(a.lamports);
    acct.set_owner(Pubkey::from(a.owner));
    acct.set_executable(a.executable);
    acct.set_rent_epoch(a.rent_epoch);
    acct.set_data_from_slice(a.data);
    acct
}

/// One transaction's outcome, as it crosses back to Go.
pub struct Outcome {
    /// The first signature, which is what clients name the transaction by.
    pub signature: [u8; 64],
    /// Whether the transaction was processed, so its fee was charged and its
    /// dedup key is spent, whatever its status.
    pub processed: bool,
    pub status: Result<(), TransactionError>,
    /// Lamports debited from the fee payer. Zero when not processed.
    pub fee: u64,
    pub units: u64,
    /// `compute_unit_price_in_microlamports`, which
    /// `getRecentPrioritizationFees` reports. It is not the ranking key, only
    /// monotone in it. The RPC reports the price because a wallet feeds the
    /// number back into `setComputeUnitPrice`.
    pub compute_unit_price: u64,
    /// What `getTransaction` serves: logs, inner instructions and return
    /// data always, with native balances.
    pub meta: Meta,
}

#[derive(Default)]
pub struct Meta {
    pub pre_balances: Vec<u64>,
    pub post_balances: Vec<u64>,
    pub logs: Vec<String>,
    pub inner_instructions: solana_message::inner_instruction::InnerInstructionsList,
    pub return_data: Option<(Pubkey, Vec<u8>)>,
    /// The token balances solana-svm collects before and after the
    /// transaction, which getTransaction renders.
    pub pre_token_balances: Vec<SvmTokenInfo>,
    pub post_token_balances: Vec<SvmTokenInfo>,
    /// `cost().units`, this chain's cost of the transaction against
    /// MAX_BLOCK_UNITS, which getTransaction reports as costUnits.
    pub cost_units: u64,
}

pub enum ExecError {
    /// A sysvar cache slot is empty after the fill. The machine halts, and the
    /// name says which writer is broken.
    MissingSysvar(&'static str),
    /// No feature set has been configured. Go configures the engine from
    /// `svmmeta` at startup and after InitGenesis, so reaching this means a
    /// startup path skipped that call.
    Unconfigured,
}

/// How many worker threads run a block's conflict groups. Zero, the default,
/// means the machine's available parallelism.
static THREADS: AtomicUsize = AtomicUsize::new(0);

/// Sets the worker count. Go calls it once at startup.
pub fn set_threads(n: usize) {
    THREADS.store(n, Ordering::Relaxed);
}

pub fn workers() -> usize {
    match THREADS.load(Ordering::Relaxed) {
        0 => std::thread::available_parallelism().map(|n| n.get()).unwrap_or(1),
        n => n,
    }
}

/// Everything a conflict group reads and nothing it writes, so one of these is
/// shared by every worker.
struct Ctx<'a> {
    height: u64,
    keys: &'a [[u8; 64]],
    already: &'a [u8],
    base: &'a Arena,
    child: &'a TransactionBatchProcessor<Linear>,
    env: &'a TransactionProcessingEnvironment,
    fs: &'a FeatureSet,
    window: &'a HashSet<Hash>,
    next_durable_nonce: DurableNonce,
    starved: &'a [bool],
}

/// Runs one block. `txs` are the transactions as admission sanitized them in
/// the plan phase, taken out of the block. `keys` are their dedup keys,
/// `recent_blockhash || message_hash`. `already[i]` is nonzero when Go's dedup
/// set holds key i from an earlier block. `dropped` are the keys Go cut at the
/// arena ceiling. They are absent from `accounts`, not from state.
/// Returns per-transaction outcomes, the write set in commit order, and each
/// transaction's conflict group.
#[allow(clippy::too_many_arguments)]
pub fn execute_block(
    height: u64,
    blockhash: [u8; 32],
    txs: Vec<Option<RuntimeTransaction<SanitizedTransaction>>>,
    loaded: &[Option<LoadedAddresses>],
    keys: &[[u8; 64]],
    already: &[u8],
    accounts: HashMap<Pubkey, AccountSharedData>,
    dropped: &HashSet<Pubkey>,
) -> Result<(Vec<Outcome>, Vec<(Pubkey, AccountSharedData)>, Vec<u32>), ExecError> {
    let root = root().ok_or(ExecError::Unconfigured)?;
    let child = root.processor.new_from(height, epoch_of(height));
    let base = Arena(accounts);
    child.reset_and_fill_sysvar_cache_entries(&base);
    check_sysvar_slots(&child)?;

    let fs = &root.feature_set;
    let blockhash = Hash::new_from_array(blockhash);
    // The acceptance window: this block's hash at age 0, and the 150 entries of
    // RecentBlockhashes at ages 1 to 150. This is Agave's
    // `age <= MAX_PROCESSING_AGE`, read from committed state, not a second queue.
    #[allow(deprecated)]
    let window: HashSet<Hash> = std::iter::once(blockhash)
        .chain(child.sysvar_cache().get_recent_blockhashes().unwrap().iter().map(|e| e.blockhash))
        .collect();
    let env = environment(&child, height, blockhash, fs);

    let count = txs.len();
    let mut outcomes: Vec<Outcome> =
        txs.iter().map(|tx| blank(tx.as_ref().map(|tx| *tx.signature().as_array()).unwrap_or([0; 64]))).collect();

    // Sanitize the whole block first, because the conflict predicate reads the
    // sanitized access set. A sanitize failure never reaches the SVM, as
    // upstream a batch that fails `RuntimeTransaction::try_create` never reaches
    // it. A transaction that phase 1 did not admit stays not processed. It
    // pays no fee, writes nothing and spends no dedup key.
    let mut sanitized: Vec<Option<RuntimeTransaction<SanitizedTransaction>>> = txs
        .into_iter()
        .enumerate()
        .map(|(i, tx)| {
            let tx = tx?;
            // Admission sanitized a message that names no table against the
            // loader this phase gives it, `Enabled` and empty, because
            // `Disabled` refuses every v0 message. So it is this phase's own
            // sanitized transaction, kept and not built again.
            if tx.num_lookup_tables() == 0 {
                return Some(tx);
            }
            // Phase 2 put the resolved addresses here, and a message that names
            // a table is sanitized again over them. A block that names no
            // table skips phase 2, so `loaded` is empty.
            let addresses = match loaded.get(i) {
                Some(Some(addresses)) => SimpleAddressLoader::Enabled(addresses.clone()),
                Some(None) => return None,
                None => SimpleAddressLoader::Enabled(LoadedAddresses::default()),
            };
            match RuntimeTransaction::<SanitizedTransaction>::try_create(
                tx.to_versioned_transaction(),
                MessageHash::Precomputed(*tx.message_hash()),
                None,
                addresses,
                &root.reserved,
            ) {
                // The duplicate half of `validate_account_locks` can run only
                // here. A static key that a table also names is not visible
                // until phase 2 resolves. Admission already checked the count
                // against the placeholders. Running both checks again costs
                // nothing and keeps the two call sites the same call.
                Ok(rtx) => match crate::check::validate_account_locks(rtx.message()) {
                    Ok(()) => Some(rtx),
                    Err(e) => {
                        outcomes[i].status = Err(e);
                        None
                    }
                },
                Err(e) => {
                    outcomes[i].status = Err(e);
                    None
                }
            }
        })
        .collect();

    // Every block budget, in block order, over what the block sanitized, with
    // each transaction priced as it resolved. This is Agave's tracker on the
    // block's units, each account's share and the block's allocation. Both
    // proposal handlers counted each transaction at the same price against the
    // same resolution, so a block they accepted never trips it. It is the
    // backstop where they could disagree, at a height whose gates flip in
    // BeginBlock after the proposal check. Like the program budget below, it
    // costs the transaction its place and does not halt every machine. The
    // transaction is not processed, pays no fee, writes nothing, has no
    // record and spends no dedup key.
    let mut spent = crate::cost::CostTracker::default();
    for (i, tx) in sanitized.iter_mut().enumerate() {
        let Some(rtx) = tx.as_ref() else { continue };
        if let Err(e) = spent.try_add(&crate::cost::cost(rtx, fs), &crate::check::writable_keys(rtx.message())) {
            outcomes[i].status = Err(e);
            *tx = None;
        }
    }

    // What happens to a transaction that names a key the arena ceiling
    // dropped. If the key is the fee payer or the nonce account, there is
    // nothing to charge. The transaction is then not processed, like one over
    // a block budget. Otherwise it is starved, and its loaded-accounts limit
    // becomes zero. Anza's loader then charges the fee and fails it at the fee
    // payer, the first account it counts, with
    // `MaxLoadedAccountsDataSizeExceeded`.
    let mut starved = vec![false; count];
    for (i, tx) in sanitized.iter_mut().enumerate() {
        let Some(rtx) = tx.as_ref() else { continue };
        let payer_dropped =
            dropped.contains(rtx.fee_payer()) || rtx.get_durable_nonce().is_some_and(|n| dropped.contains(n));
        if payer_dropped {
            outcomes[i].status = Err(TransactionError::WouldExceedMaxBlockCostLimit);
            *tx = None;
        } else {
            starved[i] = rtx.account_keys().iter().any(|k| dropped.contains(k));
        }
    }

    // The distinct executable text this block references, counted in block
    // order. A transaction whose new programs would take the block over
    // budget fails before anything is loaded or compiled. It pays no fee, like
    // a transaction that does not load. A starved transaction runs no program,
    // so it spends none of the budget and cannot fail here instead of paying
    // its fee.
    let (programs, over) = crate::programs::budget(
        &base.0,
        sanitized.iter().zip(&starved).map(|(t, starved)| {
            t.as_ref().filter(|_| !starved).map(|rtx| rtx.account_keys().iter().copied().collect::<Vec<_>>())
        }),
        crate::programs::max_block_program_text(),
    );
    for i in over {
        outcomes[i].status = Err(TransactionError::WouldExceedMaxBlockCostLimit);
        sanitized[i] = None;
    }
    // Machine-local, and the reason the budget can be as large as it is.
    crate::programs::prewarm(
        &root,
        &base,
        height,
        &programs,
        crate::programs::CACHE_BYTES.load(std::sync::atomic::Ordering::Relaxed),
    );

    let groups = components(&sanitized);
    crate::stats::set_groups(&groups);
    let mut group_of = vec![0u32; count];
    for (g, members) in groups.iter().enumerate() {
        for &i in members {
            group_of[i] = g as u32;
        }
    }

    // Move each group's transactions into one contiguous run, so a batch inside
    // a group is a sub-slice rather than a copy.
    let mut work: Vec<RuntimeTransaction<SanitizedTransaction>> = Vec::with_capacity(count);
    let mut work_index: Vec<usize> = Vec::with_capacity(count);
    let mut runs: Vec<std::ops::Range<usize>> = Vec::with_capacity(groups.len());
    for members in &groups {
        let start = work.len();
        for &i in members {
            if let Some(rtx) = sanitized[i].take() {
                work.push(rtx);
                work_index.push(i);
            }
        }
        runs.push(start..work.len());
    }

    let ctx = Ctx {
        height,
        keys,
        already,
        base: &base,
        child: &child,
        env: &env,
        fs,
        window: &window,
        next_durable_nonce: DurableNonce::from_blockhash(&blockhash),
        starved: &starved,
    };
    let done = run_groups(&ctx, &work, &work_index, &runs);

    // Groups share no account, so their write sets are disjoint, and
    // concatenating them in group order is a canonical merge. Go applies the
    // result in order, as for a block run as one sequence.
    let mut writes = Vec::new();
    for (group_outcomes, group_writes) in done {
        for (i, outcome) in group_outcomes {
            outcomes[i] = outcome;
        }
        writes.extend(group_writes);
    }

    // The block-end cache hook, after every group has finished. It is
    // machine-local. Nothing here is read back into a result.
    crate::programs::end_block(&root.processor.global_program_cache, &root.fork_graph.read().unwrap(), height);
    Ok((outcomes, writes, group_of))
}

type GroupOut = (Vec<(usize, Outcome)>, Vec<(Pubkey, AccountSharedData)>);

/// Runs `count` units of work across `n` threads, and returns the results in
/// unit order. Each thread pulls the next unit from a shared cursor.
///
/// The collecting thread resumes a worker's panic and does not catch it. A
/// panic reports a host condition, not a transaction condition, so it must
/// halt the machine and never become a result. Resuming carries it to the FFI
/// boundary's `catch_unwind`, which returns SVM_ERR_PANIC, and Go commits
/// nothing.
pub(crate) fn fan_out<T: Send>(n: usize, count: usize, one: impl Fn(usize) -> T + Sync) -> Vec<T> {
    if n <= 1 {
        return (0..count).map(one).collect();
    }
    let next = AtomicUsize::new(0);
    let mut done: Vec<(usize, T)> = std::thread::scope(|scope| {
        let handles: Vec<_> = (0..n)
            .map(|_| {
                scope.spawn(|| {
                    let mut mine = Vec::new();
                    loop {
                        let unit = next.fetch_add(1, Ordering::Relaxed);
                        if unit >= count {
                            break;
                        }
                        mine.push((unit, one(unit)));
                    }
                    mine
                })
            })
            .collect();
        let mut all = Vec::with_capacity(count);
        for handle in handles {
            match handle.join() {
                Ok(mine) => all.extend(mine),
                Err(panic) => std::panic::resume_unwind(panic),
            }
        }
        all
    });
    done.sort_by_key(|(unit, _)| *unit);
    done.into_iter().map(|(_, out)| out).collect()
}

/// Runs the block's conflict groups, in parallel when there is more than one.
fn run_groups(
    ctx: &Ctx,
    work: &[RuntimeTransaction<SanitizedTransaction>],
    work_index: &[usize],
    runs: &[std::ops::Range<usize>],
) -> Vec<GroupOut> {
    fan_out(workers().min(runs.len()), runs.len(), |g| {
        let r = runs[g].clone();
        run_group(ctx, &work[r.clone()], &work_index[r])
    })
}

fn blank(signature: [u8; 64]) -> Outcome {
    Outcome {
        signature,
        processed: false,
        status: Ok(()),
        fee: 0,
        units: 0,
        compute_unit_price: 0,
        meta: Meta::default(),
    }
}

/// Union-find over transaction indices. The lower index always wins, so a
/// component's representative is its first transaction.
struct Union(Vec<usize>);

impl Union {
    fn find(&mut self, mut i: usize) -> usize {
        while self.0[i] != i {
            self.0[i] = self.0[self.0[i]];
            i = self.0[i];
        }
        i
    }

    fn union(&mut self, a: usize, b: usize) {
        let (a, b) = (self.find(a), self.find(b));
        if a != b {
            self.0[a.max(b)] = a.min(b);
        }
    }
}

/// Conflict groups: the connected components of
///
/// ```text
/// conflict(A, B)  iff  write(A) INTERSECT (write(B) UNION read(B))  non-empty
///                 or   write(B) INTERSECT (write(A) UNION read(A))  non-empty
/// ```
///
/// Only read-read sharing is safe. A write-read pair, such as deploy then
/// invoke, must share a group, so the reader cannot run first.
///
/// Components partition the block. Which transactions share a group, and how
/// many groups there are, do not depend on arrival order. Two groups share no
/// writable account, so they can run in parallel. Inside a group, the order
/// stays arrival order.
///
/// The access set is the sanitized message's, the same one execution locks on.
/// So `is_writable` has already demoted invoked program ids and reserved keys.
/// A transaction that phase 1 did not admit, or that does not sanitize, touches
/// nothing and is its own group.
///
/// A v0 transaction's access set includes the addresses phase 2 resolved.
/// `try_create` ran with `SimpleAddressLoader::Enabled`, and `account_keys()`
/// spans the static keys and the loaded ones. Two transactions that share an
/// account through a table land in the same group. That is why the
/// grouping happens after sanitization.
fn components(txs: &[Option<RuntimeTransaction<SanitizedTransaction>>]) -> Vec<Vec<usize>> {
    let mut uf = Union((0..txs.len()).collect());
    // For each account, the last transaction to write it, and the readers since.
    // Dropping those readers on a write is safe. The write has already joined
    // them to its own component, and each later transaction that touches the
    // account joins the writer.
    let mut writer: HashMap<Pubkey, usize> = HashMap::new();
    let mut readers: HashMap<Pubkey, Vec<usize>> = HashMap::new();
    for (i, tx) in txs.iter().enumerate() {
        let Some(tx) = tx else { continue };
        for (k, key) in tx.account_keys().iter().enumerate() {
            let previous = writer.get(key).copied();
            if let Some(w) = previous {
                uf.union(i, w);
            }
            if tx.is_writable(k) {
                for r in readers.remove(key).unwrap_or_default() {
                    uf.union(i, r);
                }
                writer.insert(*key, i);
            } else {
                readers.entry(*key).or_default().push(i);
            }
        }
    }
    let mut at: HashMap<usize, usize> = HashMap::new();
    let mut groups: Vec<Vec<usize>> = Vec::new();
    for i in 0..txs.len() {
        let root = uf.find(i);
        let g = *at.entry(root).or_insert_with(|| {
            groups.push(Vec::new());
            groups.len() - 1
        });
        groups[g].push(i);
    }
    groups
}

/// The base accounts with one group's writes laid over them. `None` is an
/// account the group deleted. Groups write disjoint accounts, so a group never
/// needs to see another's overlay.
struct View<'a> {
    base: &'a Arena,
    local: HashMap<Pubkey, Option<AccountSharedData>>,
}

/// One group's view answers exactly as the arena below it. The four overrides
/// do not depend on accounts, and `base` is the same `Arena` for every group in
/// the block.
impl InvokeContextCallback for View<'_> {
    fn get_epoch_stake(&self) -> u64 {
        self.base.get_epoch_stake()
    }

    fn get_epoch_stake_for_vote_account(&self, vote_address: &Pubkey) -> u64 {
        self.base.get_epoch_stake_for_vote_account(vote_address)
    }

    fn is_precompile(&self, program_id: &Pubkey) -> bool {
        self.base.is_precompile(program_id)
    }

    fn process_precompile(
        &self,
        program_id: &Pubkey,
        data: &[u8],
        instruction_datas: Vec<&[u8]>,
    ) -> Result<(), PrecompileError> {
        self.base.process_precompile(program_id, data, instruction_datas)
    }
}

impl TransactionProcessingCallback for View<'_> {
    fn get_account_shared_data(&self, key: &Pubkey) -> Option<(AccountSharedData, u64)> {
        match self.local.get(key) {
            Some(local) => local.as_ref().map(|a| (a.clone(), 0)),
            None => self.base.0.get(key).map(|a| (a.clone(), 0)),
        }
    }
    fn inspect_account(&self, _: &Pubkey, _: AccountState, _: bool) {}
}

/// Runs one conflict group serially, in arrival order, as a sequence of
/// conflict-free batches. `txs` is the group's contiguous run of the block's
/// transactions and `index` their block positions, so a batch is a sub-slice of
/// `txs` rather than a copy of it.
fn run_group(ctx: &Ctx, txs: &[RuntimeTransaction<SanitizedTransaction>], index: &[usize]) -> GroupOut {
    let mut g = Group {
        view: View { base: ctx.base, local: HashMap::new() },
        outcomes: Vec::with_capacity(txs.len()),
        writes: Vec::new(),
        write_index: HashMap::new(),
        write_locks: HashSet::new(),
        read_locks: HashSet::new(),
        spent: HashSet::new(),
        pending: HashSet::new(),
        checks: Vec::new(),
        start: 0,
    };
    for (at, rtx) in txs.iter().enumerate() {
        let key = ctx.keys[index[at]];
        // A transaction whose accounts conflict with the pending batch, or
        // whose dedup key is already in it, waits for that batch to commit.
        // Agave gets the same order from account locks, which never let two
        // conflicting transactions share a batch, and from committing each
        // batch to the status cache. The rule also bounds memory. Each result
        // in a batch holds its own copy of the accounts it wrote, so a block of
        // buffer writes to one 10 MiB account must not be one batch.
        if g.pending.contains(&key) || g.conflicts(rtx) {
            g.flush(ctx, txs, index, at);
        }
        let mut result = check(rtx, ctx.fs, ctx.window, &ctx.next_durable_nonce, &g.view, ctx.starved[index[at]]);
        // Two transactions with the same dedup key carry the same message, so
        // they share a writable fee payer and are always in this same group.
        if result.is_ok() && (ctx.already[index[at]] != 0 || g.spent.contains(&key)) {
            result = Err(TransactionError::AlreadyProcessed);
        }
        if result.is_ok() {
            g.pending.insert(key);
        }
        g.lock(rtx);
        let mut outcome = blank(*rtx.signature().as_array());
        // The declared price, read here because the sanitized transaction is
        // here. A transaction whose compute budget does not parse never
        // executes, so it bid nothing. cost.rs gives it the same answer.
        outcome.compute_unit_price = rtx
            .transaction_configuration(ctx.fs)
            .map(|c| c.compute_unit_price_in_microlamports())
            .unwrap_or(0);
        g.outcomes.push(outcome);
        g.checks.push(result);
    }
    g.flush(ctx, txs, index, txs.len());
    (index.iter().copied().zip(g.outcomes).collect(), g.writes)
}

fn environment(
    child: &TransactionBatchProcessor<Linear>,
    height: u64,
    blockhash: Hash,
    fs: &FeatureSet,
) -> TransactionProcessingEnvironment {
    TransactionProcessingEnvironment {
        blockhash,
        blockhash_lamports_per_signature: LAMPORTS_PER_SIGNATURE,
        alpenglow_migration_succeeded: false,
        epoch_total_stake: 0,
        feature_set: fs.runtime_features(),
        program_runtime_environments: ProgramRuntimeEnvironments::new(
            child.program_runtime_environment.clone(),
            // The epoch in which deployments made now become visible.
            child.program_runtime_environment_for_epoch(epoch_of(
                height.saturating_add(DELAY_VISIBILITY_SLOT_OFFSET),
            )),
        ),
        rent: rent(child),
    }
}

struct Group<'a> {
    view: View<'a>,
    /// Outcomes, parallel to the group's transactions.
    outcomes: Vec<Outcome>,
    /// The group's write set, one entry per account in first-write order,
    /// holding the account's last value. Go applies it in order, so the result
    /// is the same as applying every write.
    writes: Vec<(Pubkey, AccountSharedData)>,
    write_index: HashMap<Pubkey, usize>,
    /// The pending batch's locks.
    write_locks: HashSet<Pubkey>,
    read_locks: HashSet<Pubkey>,
    /// Dedup keys processed earlier in this group.
    spent: HashSet<[u8; 64]>,
    pending: HashSet<[u8; 64]>,
    checks: Vec<TransactionCheckResult>,
    /// Where the pending batch starts in the group's transactions.
    start: usize,
}

impl Group<'_> {
    fn conflicts(&self, tx: &RuntimeTransaction<SanitizedTransaction>) -> bool {
        tx.account_keys().iter().enumerate().any(|(i, key)| {
            self.write_locks.contains(key) || (tx.is_writable(i) && self.read_locks.contains(key))
        })
    }

    fn lock(&mut self, tx: &RuntimeTransaction<SanitizedTransaction>) {
        for (i, key) in tx.account_keys().iter().enumerate() {
            if tx.is_writable(i) {
                self.write_locks.insert(*key);
            } else {
                self.read_locks.insert(*key);
            }
        }
    }

    /// Executes the pending batch and lays its writes over the group's view, so
    /// the next batch loads what this one committed. The batch is
    /// `txs[self.start..end]`, a sub-slice of the group's own transactions.
    fn flush(
        &mut self,
        ctx: &Ctx,
        txs: &[RuntimeTransaction<SanitizedTransaction>],
        index: &[usize],
        end: usize,
    ) {
        let batch = &txs[self.start..end];
        if batch.is_empty() {
            return;
        }
        let out = ctx.child.load_and_execute_sanitized_transactions(
            &self.view,
            batch,
            std::mem::take(&mut self.checks),
            ctx.env,
            &replay_config(),
        );
        for (key, account) in collect_writes(batch, &out.processing_results) {
            let live = (account.lamports() != 0).then(|| account.clone());
            self.view.local.insert(key, live);
            match self.write_index.get(&key) {
                Some(&at) => self.writes[at].1 = account,
                None => {
                    self.write_index.insert(key, self.writes.len());
                    self.writes.push((key, account));
                }
            }
        }
        let (mut pre, mut post, mut pre_tokens, mut post_tokens) = match out.balance_collector {
            Some(c) => {
                let (pre, post, pre_tokens, post_tokens) = c.into_vecs();
                (pre.into_iter(), post.into_iter(), pre_tokens.into_iter(), post_tokens.into_iter())
            }
            None => (Vec::new().into_iter(), Vec::new().into_iter(), Vec::new().into_iter(), Vec::new().into_iter()),
        };
        let mut deployed: HashMap<Pubkey, Arc<ProgramCacheEntry>> = HashMap::new();
        for (at, result) in (self.start..end).zip(out.processing_results) {
            let o = &mut self.outcomes[at];
            let (pre, post) = (pre.next().unwrap_or_default(), post.next().unwrap_or_default());
            let (pre_tokens, post_tokens) = (pre_tokens.next().unwrap_or_default(), post_tokens.next().unwrap_or_default());
            match result {
                Ok(processed) => {
                    o.processed = true;
                    o.status = processed.status();
                    o.fee = processed.fee_details().total_fee();
                    o.units = processed.executed_units();
                    o.meta.pre_balances = pre;
                    o.meta.post_balances = post;
                    o.meta.pre_token_balances = pre_tokens;
                    o.meta.post_token_balances = post_tokens;
                    o.meta.cost_units = crate::cost::cost(&batch[at - self.start], ctx.fs).units;
                    if let ProcessedTransaction::Executed(e) = processed {
                        if e.was_successful() {
                            deployed.extend(e.programs_modified_by_tx.iter().map(|(k, v)| (*k, v.clone())));
                        }
                        let details = e.execution_details;
                        o.meta.logs = details.log_messages.unwrap_or_default();
                        o.meta.inner_instructions = details.inner_instructions.unwrap_or_default();
                        o.meta.return_data = details.return_data.map(|r| (r.program_id, r.data));
                    }
                    self.spent.insert(ctx.keys[index[at]]);
                }
                Err(e) => o.status = Err(e),
            }
        }
        // The batch's deployments reach the global cache when the batch
        // commits, which is when Agave merges them. Only a transaction that
        // writes a program's account can modify its entry. Each transaction
        // that reads the program conflicts with that write. So no other group
        // looks up the key while this group changes it.
        if !deployed.is_empty() {
            ctx.child
                .global_program_cache
                .write()
                .unwrap()
                .merge(&ctx.child.program_runtime_environment, ctx.height, &deployed);
        }
        self.start = end;
        self.pending.clear();
        self.write_locks.clear();
        self.read_locks.clear();
    }
}

/// A transaction's load list for simulation, and its own static keys.
///
/// The first is the list that a block of this one transaction would load, in
/// this order: the nine sysvar ids, the fee payer and its nonce account, then
/// the rest in message order. Go cuts it at 64 MiB, and a block's list at 512
/// MiB. The second is the static keys alone, in message order. The address
/// index uses them, because an entry per sysvar would add nine entries per
/// transaction for addresses that nobody asks about.
///
/// Both come from one parse because this is the only Solana message parser in
/// the system. Go must not grow a second one to learn which accounts a stored
/// transaction named.
pub fn transaction_keys(bytes: &[u8]) -> Result<(Vec<[u8; 32]>, Vec<[u8; 32]>), ()> {
    let tx: VersionedTransaction = wincode::deserialize(bytes).map_err(|_| ())?;
    tx.sanitize().map_err(|_| ())?;
    let keys = tx.message.static_account_keys();
    let statics: Vec<[u8; 32]> = keys.iter().map(|k| k.to_bytes()).collect();
    let first = tx.message.instructions().first().map(|ix| (ix.program_id_index, ix.accounts.as_slice(), ix.data.as_slice()));
    let load = crate::block::load_list_of(std::iter::once((keys, crate::block::nonce_of(keys, first), None)));
    Ok((load, statics))
}

/// What `simulateTransaction` returns.
pub struct Simulation {
    pub processed: bool,
    pub status: Result<(), TransactionError>,
    pub units: u64,
    pub fee: u64,
    pub loaded_accounts_data_size: u32,
    pub logs: Vec<String>,
    pub return_data: Option<(Pubkey, Vec<u8>)>,
    /// The addresses a v0 message's tables resolved to, which Agave's
    /// response names as `loadedAddresses`.
    pub loaded: LoadedAddresses,
    /// The inner instructions, the balances and the transaction's accounts
    /// after it ran, which Agave's simulation also records and returns. The
    /// inner instructions are None unless the transaction executed.
    pub inner_instructions: Option<solana_message::inner_instruction::InnerInstructionsList>,
    pub balances: Option<(Vec<u64>, Vec<u64>)>,
    pub pre_token_balances: Vec<SvmTokenInfo>,
    pub post_token_balances: Vec<SvmTokenInfo>,
    pub post_accounts: Vec<(Pubkey, AccountSharedData)>,
}

/// Agave's `Bank::simulate_transaction_unchecked` on its own child processor,
/// filled from its own arena, so it never touches an open block. The write set
/// is discarded.
///
/// It runs as the next block. `height` is that block's height, the arena holds
/// the sysvars its begin-block writer wrote, and the root is the one that
/// `configure_next` built for it. `blockhash` is the committed block's hash.
/// `replaceRecentBlockhash` writes it, and the age window starts at it. `own`
/// stands for the next block's own hash, which no header hashes to. The durable
/// nonce that the block would advance a nonce to derives from it.
///
/// `dropped` are the keys Go's 64 MiB cut left unloaded. A transaction that
/// names one is starved exactly as a block's is.
///
/// `processed` tells whether Go's replay set holds the transaction's dedup key.
/// It stands in for Agave's status cache, as `already` does for a block.
#[allow(clippy::too_many_arguments)]
pub fn simulate(
    height: u64,
    blockhash: [u8; 32],
    own: [u8; 32],
    bytes: &[u8],
    sig_verify: bool,
    replace_recent_blockhash: bool,
    processed: bool,
    accounts: HashMap<Pubkey, AccountSharedData>,
    dropped: &HashSet<Pubkey>,
) -> Result<Simulation, ExecError> {
    let root = next_root(height).ok_or(ExecError::Unconfigured)?;
    let not_processed = |e| Simulation {
        processed: false,
        status: Err(e),
        units: 0,
        fee: 0,
        loaded_accounts_data_size: 0,
        logs: Vec::new(),
        return_data: None,
        loaded: LoadedAddresses::default(),
        inner_instructions: None,
        balances: None,
        pre_token_balances: Vec::new(),
        post_token_balances: Vec::new(),
        post_accounts: Vec::new(),
    };
    // Not exact, unlike admission. Agave's RPC caps the raw length and then
    // parses with `wincode::deserialize`. `decodeTransaction` in
    // `rpc/send.go` caps it the same way.
    let Ok(mut tx) = wincode::deserialize::<VersionedTransaction>(bytes) else {
        return Ok(not_processed(TransactionError::SanitizeFailure));
    };
    let blockhash = Hash::new_from_array(blockhash);
    let own = Hash::new_from_array(own);
    if replace_recent_blockhash {
        tx.message.set_recent_blockhash(blockhash);
    }
    if sig_verify && tx.verify_with_results().iter().any(|ok| !ok) {
        return Ok(not_processed(TransactionError::SignatureFailure));
    }
    let fs = &root.feature_set;
    let child = root.processor.new_from(height, epoch_of(height));
    let arena = Arena(accounts);
    child.reset_and_fill_sysvar_cache_entries(&arena);
    check_sysvar_slots(&child)?;
    // A v0 message's tables resolve against the state given to the
    // simulation, as Agave's simulation sanitizes against its bank. Go read
    // the tables and the accounts they name into the arena.
    let loaded = match tx.message.address_table_lookups() {
        Some(lookups) if !lookups.is_empty() => {
            // Checked present by `check_sysvar_slots`.
            let slot_hashes = SlotHashes::new(child.sysvar_cache().get_slot_hashes().unwrap().slot_hashes());
            let tables = crate::resolve::Tables::new(
                slot_hashes,
                lookups.iter().filter_map(|l| {
                    arena.0.get(&l.account_key).map(|a| (l.account_key.to_bytes(), *a.owner(), a.data().to_vec()))
                }),
            );
            match crate::resolve::load_addresses(&tables, height, lookups) {
                Ok(loaded) => loaded,
                Err(e) => return Ok(not_processed(e.into())),
            }
        }
        _ => LoadedAddresses::default(),
    };
    let loader = SimpleAddressLoader::Enabled(loaded.clone());
    let rtx = match RuntimeTransaction::<SanitizedTransaction>::try_create(
        tx,
        MessageHash::Compute,
        None,
        loader,
        &root.reserved,
    ) {
        Ok(rtx) => rtx,
        Err(e) => return Ok(not_processed(e)),
    };
    // Agave builds a simulation's batch with `validate_account_locks`, so a
    // duplicate key or a 129th key makes the transaction not processed, with
    // that error. The error goes to the processor as the check result, as
    // Agave's lock result does, so the balances are still collected.
    let locks = crate::check::validate_account_locks(rtx.message());
    let starved = rtx.account_keys().iter().any(|k| dropped.contains(k));
    let declared = rtx.transaction_configuration(fs).map(|c| c.loaded_accounts_data_size_limit);

    // The committed block's hash at age 0, then RecentBlockhashes, up to
    // MAX_PROCESSING_AGE minus MAX_TRANSACTION_FORWARDING_DELAY. The next
    // block's RecentBlockhashes opens with the committed hash, so the window
    // runs from the committed hash back 143 blocks. This is Agave's simulation
    // rule, counted from the block the transaction can execute in.
    #[allow(deprecated)]
    let window: HashSet<Hash> = std::iter::once(blockhash)
        .chain(
            child.sysvar_cache().get_recent_blockhashes().unwrap().iter()
                .take(solana_clock::MAX_PROCESSING_AGE - solana_clock::MAX_TRANSACTION_FORWARDING_DELAY)
                .map(|e| e.blockhash),
        )
        .collect();
    // `check_transactions` is the age check and then the status cache,
    // so an expired transaction is BlockhashNotFound whether or not it was
    // processed.
    let check = locks
        .and_then(|()| check(&rtx, fs, &window, &DurableNonce::from_blockhash(&own), &arena, starved))
        .and_then(|details| if processed { Err(TransactionError::AlreadyProcessed) } else { Ok(details) });
    let env = environment(&child, height, own, fs);
    // Agave's simulation config: everything recorded, the strict nonce size
    // check, and no-ops dropped. The inner instructions are always recorded.
    // The RPC serves them only when asked, which is what Agave's flag decides.
    // The deployment slot check is the bank's own flag. Agave sets it for a
    // cache that may not have seen a deployment. No block merges into a
    // simulation root's cache, so an entry deployed before the program's
    // deployment slot is reloaded, not run.
    let config = TransactionProcessingConfig {
        check_program_deployment_slot: true,
        limit_to_load_programs: true,
        drop_on_failure: false,
        all_or_nothing: false,
        strict_nonce_size_check: true,
        drop_noop_transactions: true,
        recording_config: solana_svm::transaction_processor::ExecutionRecordingConfig {
            enable_cpi_recording: true,
            enable_log_recording: true,
            enable_return_data_recording: true,
            enable_transaction_balance_recording: true,
        },
        ..TransactionProcessingConfig::default()
    };
    let number_of_accounts = rtx.account_keys().len();
    let mut out = child.load_and_execute_sanitized_transactions(&arena, &[rtx], vec![check], &env, &config);
    let (balances, pre_token_balances, post_token_balances) = match out.balance_collector.take() {
        Some(c) => {
            let (mut pre, mut post, mut pre_tokens, mut post_tokens) = c.into_vecs();
            let balances = pre.pop().zip(post.pop());
            (balances, pre_tokens.pop().unwrap_or_default(), post_tokens.pop().unwrap_or_default())
        }
        None => (None, Vec::new(), Vec::new()),
    };
    Ok(match out.processing_results.pop() {
        Some(Ok(processed)) => {
            let units = processed.executed_units();
            let mut loaded_size = processed.loaded_accounts_data_size();
            // Here a starved transaction fails at the fee payer under a zero
            // limit. Agave fails partway under the declared limit. With
            // SIMD-0186's fee-only semantics, Agave reports the smaller of what
            // it counted and that limit, which is the limit.
            if starved && fs.snapshot().define_ltds_fee_only_semantics {
                loaded_size = declared.unwrap_or(loaded_size);
            }
            let fee = processed.fee_details().total_fee();
            let status = processed.status();
            let (logs, return_data, inner_instructions, post_accounts) = match processed {
                ProcessedTransaction::Executed(e) => (
                    e.execution_details.log_messages.unwrap_or_default(),
                    e.execution_details.return_data.map(|r| (r.program_id, r.data)),
                    Some(e.execution_details.inner_instructions.unwrap_or_default()),
                    e.loaded_transaction.accounts.into_iter().take(number_of_accounts).collect(),
                ),
                // SIMD-0290's no-op, unreachable while relax_fee_payer_constraint
                // is off. Agave logs nothing for one.
                ProcessedTransaction::FeesOnly(_) | ProcessedTransaction::NoOp(_) => (Vec::new(), None, None, Vec::new()),
            };
            Simulation {
                processed: true,
                status,
                units,
                fee,
                loaded_accounts_data_size: loaded_size,
                logs,
                return_data,
                loaded,
                inner_instructions,
                balances,
                pre_token_balances,
                post_token_balances,
                post_accounts,
            }
        }
        Some(Err(e)) => Simulation { balances, pre_token_balances, post_token_balances, ..not_processed(e) },
        None => not_processed(TransactionError::InvalidProgramForExecution),
    })
}

/// `getFeeForMessage`, as `Bank::get_fee_for_message` computes it over a
/// message sanitized the way agave sanitizes it. It skips the blockhash queue
/// check, which belongs to replay protection. Machine-local and handle-free,
/// like simulate.
///
/// A message that does not sanitize is an error, which the RPC reports as
/// invalid params. A message whose compute budget does not parse has no fee,
/// which the RPC reports as null. Both match Agave.
pub fn fee_for_message(bytes: &[u8]) -> Result<Option<u64>, ()> {
    let root = root().ok_or(())?;
    // wincode, because `getFeeForMessage` takes the bytes that
    // `Message::serialize` writes, which agave decodes with wincode.
    let message: VersionedMessage = wincode::deserialize(bytes).map_err(|_| ())?;
    // A v0 message that names a lookup table is priced without loading it.
    // The fee does not depend on the addresses, and this entry point reads no
    // state (`crate::check::placeholder_loader`).
    let loader = crate::check::placeholder_loader(&message);
    let message = SanitizedVersionedMessage::try_from(message).map_err(|_| ())?;
    let message = SanitizedMessage::try_new(message, loader, &root.reserved).map_err(|_| ())?;
    Ok(TransactionConfiguration::try_from_sanitized_message(&message, &root.feature_set)
        .ok()
        .map(|config| {
            solana_fee::calculate_fee(
                &message,
                LAMPORTS_PER_SIGNATURE,
                config.priority_fee_lamports,
                FeeFeatures {},
            )
        }))
}

/// The budget one transaction executes under: its own compute unit limit and
/// heap, and the instruction stack depth the feature set decides.
///
/// This is the only place a gate reaches the CPI nesting limit. The SVM takes
/// the depth from the `CheckedTransactionDetails` its caller builds, not from
/// the feature set in its own environment. The value travels from
/// `CheckedTransactionDetails` to `LoadedTransaction::compute_budget` to
/// `TransactionContext::new`. If this function ignored the gate, activating
/// `raise_cpi_nesting_limit_to_8` would change the feature account and nothing
/// else.
///
/// The depth is 5, or 9 with SIMD-0268 active, which is eight nested CPIs.
fn execution_budget(
    config: &TransactionConfiguration,
    fs: &FeatureSet,
) -> solana_compute_budget::compute_budget::SVMTransactionExecutionBudget {
    solana_compute_budget::compute_budget::SVMTransactionExecutionBudget {
        compute_unit_limit: u64::from(config.compute_unit_limit),
        heap_size: config.updated_heap_bytes,
        ..solana_compute_budget::compute_budget::SVMTransactionExecutionBudget::new_with_defaults(
            fs.snapshot().raise_cpi_nesting_limit_to_8,
        )
    }
}

/// Ports `Bank::check_age_and_compute_budget_limits`. It applies the V1
/// filter, then compute budget limits, then the blockhash age or a valid
/// durable nonce.
/// The status cache check is the caller's. The Bank's optional
/// `compute_budget` override is always `None` on a validator. A `starved`
/// transaction gets a zero loaded-accounts limit.
fn check(
    tx: &RuntimeTransaction<SanitizedTransaction>,
    fs: &FeatureSet,
    window: &HashSet<Hash>,
    next_durable_nonce: &DurableNonce,
    accounts: &impl TransactionProcessingCallback,
    starved: bool,
) -> TransactionCheckResult {
    if !fs.snapshot().enable_tx_v1 && tx.version() == TransactionVersion::Number(1) {
        return Err(TransactionError::UnsupportedVersion);
    }
    let config = tx.transaction_configuration(fs)?;
    let fee_details = calculate_fee_details(
        tx,
        LAMPORTS_PER_SIGNATURE,
        config.priority_fee_lamports,
        FeeFeatures {},
    );
    let budget = SVMTransactionExecutionAndFeeBudgetLimits {
        budget: execution_budget(&config, fs),
        loaded_accounts_data_size_limit: if starved { 0 } else { config.loaded_accounts_data_size_limit },
        fee_details,
    };
    let recent = tx.recent_blockhash();
    if window.contains(recent) {
        return Ok(CheckedTransactionDetails::new(None, budget));
    }
    // `check_nonce_transaction_validity` with the replay path's
    // `strict_nonce_size_check: false`.
    if recent != next_durable_nonce.as_hash() {
        if let Some(address) = tx.get_durable_nonce() {
            if let Some((account, _)) = accounts.get_account_shared_data(address) {
                if solana_nonce_account::verify_nonce_account(&account, recent).is_some() {
                    return Ok(CheckedTransactionDetails::new(Some(*address), budget));
                }
            }
        }
    }
    Err(TransactionError::BlockhashNotFound)
}

fn rent(child: &TransactionBatchProcessor<Linear>) -> Rent {
    // Checked present by `check_sysvar_slots`.
    Rent::clone(&child.sysvar_cache().get_rent().unwrap())
}

#[allow(deprecated)]
pub fn check_sysvar_slots(child: &TransactionBatchProcessor<Linear>) -> Result<(), ExecError> {
    let cache = child.sysvar_cache();
    let slots: [(&'static str, bool); 8] = [
        ("Clock", cache.get_clock().is_ok()),
        ("EpochSchedule", cache.get_epoch_schedule().is_ok()),
        ("EpochRewards", cache.get_epoch_rewards().is_ok()),
        ("Rent", cache.get_rent().is_ok()),
        ("SlotHashes", cache.get_slot_hashes().is_ok()),
        ("StakeHistory", cache.get_stake_history().is_ok()),
        ("LastRestartSlot", cache.get_last_restart_slot().is_ok()),
        ("RecentBlockhashes", cache.get_recent_blockhashes().is_ok()),
    ];
    match slots.iter().find(|(_, ok)| !ok) {
        Some((name, _)) => Err(ExecError::MissingSysvar(name)),
        None => Ok(()),
    }
}

/// The write-set port of Agave, for all four result cases. A successful
/// transaction stores the accounts that pass three filters: writable, touched,
/// and not a program the transaction only invoked. Without the third, program
/// accounts are overwritten. A failed or fees-only transaction stores its
/// rollback accounts. An unprocessed one stores nothing.
///
/// No execution can separate the third filter from write demotion, because
/// upstream demotes an invoked program id to readonly before this runs. So the
/// test checks the predicate directly.
fn writes_back(writable: bool, touched: bool, invoked: bool, instruction_account: bool) -> bool {
    writable && touched && !(invoked && !instruction_account)
}

fn collect_writes(
    txs: &[RuntimeTransaction<SanitizedTransaction>],
    results: &[TransactionProcessingResult],
) -> Vec<(Pubkey, AccountSharedData)> {
    let mut writes = Vec::new();
    for (tx, result) in txs.iter().zip(results) {
        let Ok(processed) = result else { continue };
        match processed {
            ProcessedTransaction::Executed(e) if e.execution_details.status.is_ok() => {
                let loaded = &e.loaded_transaction;
                for (i, (key, account)) in
                    (0..tx.account_keys().len()).zip(loaded.accounts.iter())
                {
                    if !writes_back(
                        tx.is_writable(i),
                        loaded.touched_flags[i],
                        tx.is_invoked(i),
                        tx.is_instruction_account(i),
                    ) {
                        continue;
                    }
                    writes.push((*key, account.clone()));
                }
            }
            ProcessedTransaction::Executed(e) => {
                for (key, account) in &e.loaded_transaction.rollback_accounts {
                    writes.push((*key, account.clone()));
                }
            }
            ProcessedTransaction::FeesOnly(f) => {
                for (key, account) in &f.rollback_accounts {
                    writes.push((*key, account.clone()));
                }
            }
            // A no-op writes nothing, as Agave's account saver stores nothing
            // for one. It is unreachable while `relax_fee_payer_constraint`
            // is off.
            ProcessedTransaction::NoOp(_) => {}
        }
    }
    writes
}

/// `configure` writes a global, so a test that turns a gate on takes this
/// first. Without it, a parallel test's `configure(&[])` can turn the gate off
/// in the middle of an assertion. It ignores poisoning, because a panicking
/// test has already failed and the next one reconfigures before it reads.
#[cfg(test)]
pub fn config_lock() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    LOCK.lock().unwrap_or_else(|e| e.into_inner())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn epochs_are_432_000_slots_from_slot_0() {
        assert_eq!(epoch_of(0), 0);
        assert_eq!(epoch_of(431_999), 0);
        assert_eq!(epoch_of(432_000), 1);
        assert_eq!(epoch_of(864_000), 2);
    }

    /// `arena_accounts` decodes every pushed account into the map the SVM's
    /// callback serves. It does not depend on state size, so unlike the store
    /// read it can be timed here.
    #[test]
    #[ignore]
    fn arena_ingest_cost_per_account() {
        const N: usize = 143_198;
        let mut value = vec![0u8; 56 + 165];
        value[0..8].copy_from_slice(&2_039_280u64.to_le_bytes());
        value[16..20].copy_from_slice(&165u32.to_le_bytes());
        let mut w = crate::abi::Writer::new(crate::abi::KIND_EXECUTE_REQ, 0);
        for i in 0..N {
            let mut k = [0u8; 32];
            k[..8].copy_from_slice(&(i as u64).wrapping_mul(0x9E37_79B9_7F4A_7C15).to_le_bytes());
            w.push(&[&k[..], &(value.as_ptr() as u64).to_le_bytes(), &(value.len() as u64).to_le_bytes()].concat());
        }
        let buf = w.finish();
        let mut best = std::time::Duration::MAX;
        for _ in 0..5 {
            let r = crate::abi::Reader::new(&buf, crate::abi::KIND_EXECUTE_REQ).unwrap();
            let t = std::time::Instant::now();
            let map = unsafe { arena_accounts(&r, 0..r.len()) }.unwrap();
            best = best.min(t.elapsed());
            assert_eq!(map.len(), N);
            drop(map);
        }
        println!("arena_ingest accounts={N} best_ms={:.1} per_account_us={:.3}",
            best.as_secs_f64() * 1e3, best.as_secs_f64() * 1e6 / N as f64);
    }

    /// A transaction that writes `writes` and reads `reads`, with the first
    /// key the fee payer. Unsigned: nothing here verifies signatures.
    fn tx_for(writes: &[Pubkey], reads: &[Pubkey]) -> Option<RuntimeTransaction<SanitizedTransaction>> {
        use solana_message::{legacy::Message, MessageHeader};
        let mut account_keys = writes.to_vec();
        account_keys.extend_from_slice(reads);
        let message = VersionedMessage::Legacy(Message {
            header: MessageHeader {
                num_required_signatures: 1,
                num_readonly_signed_accounts: 0,
                num_readonly_unsigned_accounts: reads.len() as u8,
            },
            account_keys,
            recent_blockhash: Hash::default(),
            instructions: vec![],
        });
        let tx = VersionedTransaction {
            signatures: vec![Default::default()],
            message,
        };
        RuntimeTransaction::<SanitizedTransaction>::try_create(
            tx,
            MessageHash::Compute,
            None,
            SimpleAddressLoader::Disabled,
            &HashSet::default(),
        )
        .ok()
    }

    /// Group membership as a set of sets of the caller's own labels, so two
    /// partitions can be compared without depending on how the groups are
    /// numbered or on the order the members arrived in.
    fn partition(labels: &[usize], groups: &[Vec<usize>]) -> HashSet<Vec<usize>> {
        groups
            .iter()
            .map(|members| {
                let mut named: Vec<usize> = members.iter().map(|&i| labels[i]).collect();
                named.sort_unstable();
                named
            })
            .collect()
    }

    /// Work order is the unit order, whatever the thread count, so the merge
    /// of the groups is canonical and not a race.
    #[test]
    fn fan_out_returns_units_in_order() {
        let want: Vec<usize> = (0..50).map(|i| i * 2).collect();
        for n in [1, 2, 4, 8, 50, 100] {
            assert_eq!(fan_out(n, 50, |i| i * 2), want, "{n} threads");
        }
    }

    /// A panic inside a group is a host condition. It must reach the caller,
    /// which halts, and must never be swallowed into a result. Every other
    /// worker is joined on the way out.
    #[test]
    fn a_panic_in_a_group_reaches_the_caller() {
        let hook = std::panic::take_hook();
        std::panic::set_hook(Box::new(|_| {}));
        let caught = std::panic::catch_unwind(|| {
            fan_out(4, 64, |unit| {
                assert!(unit != 37, "group 37");
                unit
            })
        });
        std::panic::set_hook(hook);
        let panic = caught.expect_err("a panicking group returned a value");
        let message = panic
            .downcast_ref::<String>()
            .map(String::as_str)
            .or_else(|| panic.downcast_ref::<&str>().copied())
            .unwrap_or_default();
        assert!(message.contains("group 37"), "lost the diagnostic: {message}");
    }

    /// Groups are components, not conflict-free batches. Conflicting
    /// transactions share a group, and only read-read sharing keeps two
    /// transactions apart.
    #[test]
    fn conflict_groups_are_connected_components() {
        let k: Vec<Pubkey> = (0..4).map(|_| Pubkey::new_unique()).collect();
        let one = |g: Vec<Vec<usize>>| g.len();

        assert_eq!(one(components(&[tx_for(&[k[0]], &[]), tx_for(&[k[1]], &[])])), 2, "disjoint writes");
        assert_eq!(one(components(&[tx_for(&[k[0]], &[]), tx_for(&[k[0]], &[])])), 1, "write-write");
        // The deploy-then-invoke case: the second only reads what the first
        // writes, and it must not be free to run first.
        assert_eq!(one(components(&[tx_for(&[k[0]], &[]), tx_for(&[k[1]], &[k[0]])])), 1, "write-read");
        assert_eq!(
            one(components(&[tx_for(&[k[0]], &[k[2]]), tx_for(&[k[1]], &[k[2]])])),
            2,
            "read-read sharing is the only safe sharing"
        );
        // A writes X, B writes X and Y, C writes Y. That is one component,
        // though A and C do not conflict with each other.
        assert_eq!(
            one(components(&[tx_for(&[k[0]], &[]), tx_for(&[k[0], k[1]], &[]), tx_for(&[k[1]], &[])])),
            1,
            "conflict is transitive through B"
        );
        // A read after a write joins the writer, and the writer has already
        // joined every reader before it.
        assert_eq!(
            one(components(&[tx_for(&[k[0]], &[k[2]]), tx_for(&[k[1]], &[k[2]]), tx_for(&[k[2]], &[])])),
            1,
            "a later writer joins every earlier reader"
        );
        // A transaction phase 1 did not admit touches nothing.
        assert_eq!(one(components(&[None, None])), 2, "an unadmitted transaction is its own group");
    }

    #[test]
    fn the_partition_does_not_depend_on_arrival_order() {
        // Eight clusters of three accounts, five transactions each, plus one
        // account every transaction reads and none writes. Read-read sharing
        // must not pull the clusters together.
        let keys: Vec<Pubkey> = (0..25).map(|_| Pubkey::new_unique()).collect();
        let shared = keys[24];
        let specs: Vec<(Vec<Pubkey>, Vec<Pubkey>)> = (0..40usize)
            .map(|i| {
                let (cluster, j) = (i / 5, i % 5);
                let key = |n: usize| keys[cluster * 3 + n % 3];
                (vec![key(j)], vec![key(j + 1), shared])
            })
            .collect();
        let build = |order: &[usize]| {
            let txs: Vec<_> = order.iter().map(|&i| tx_for(&specs[i].0, &specs[i].1)).collect();
            partition(order, &components(&txs))
        };

        let forward: Vec<usize> = (0..specs.len()).collect();
        let mut reversed = forward.clone();
        reversed.reverse();
        // An arbitrary fixed shuffle, so the test is not two orders apart.
        let mut shuffled = forward.clone();
        let mut seed = 0x243f_6a88_85a3_08d3u64;
        for i in (1..shuffled.len()).rev() {
            seed = seed.wrapping_mul(6364136223846793005).wrapping_add(1442695040888963407);
            shuffled.swap(i, (seed >> 33) as usize % (i + 1));
        }

        let expected = build(&forward);
        assert_eq!(expected.len(), 8, "one group per cluster");
        assert_eq!(build(&reversed), expected, "reversed");
        assert_eq!(build(&shuffled), expected, "shuffled");
    }

    /// Checks the gate ids and lamports-per-byte values in x/svm/gates.go, for
    /// the bank-side read sites this chain ports, against the crate the engine
    /// builds its feature set from. Go holds no copy of Anza's table that this
    /// test does not check. An engine bump that moves an id fails here, not at
    /// a gate flip.
    #[test]
    fn the_ported_gate_ids_are_the_crates() {
        use agave_feature_set as gate;
        // Agave's order, with the safeguard last.
        let rent: [(&str, u64, solana_pubkey::Pubkey); 6] = [
            ("4a6f7o7iTcA8hRDCrPLkSatnt5Ykxiu36wo5p1Tt12wC", gate::set_lamports_per_byte_to_6333::LAMPORTS_PER_BYTE, gate::set_lamports_per_byte_to_6333::id()),
            ("61BtM7BkDEE8Yq5fskEVAQT9mYA8qCejJWoLe5apqg81", gate::set_lamports_per_byte_to_5080::LAMPORTS_PER_BYTE, gate::set_lamports_per_byte_to_5080::id()),
            ("rntCigrTppP5JdZz7K8TyN9sMzLdAcXp8SejYpVpX6D", gate::set_lamports_per_byte_to_2575::LAMPORTS_PER_BYTE, gate::set_lamports_per_byte_to_2575::id()),
            ("rntD7invRBswCAdKtRsh1G4psKjrPdS3BKqtnA78C7N", gate::set_lamports_per_byte_to_1322::LAMPORTS_PER_BYTE, gate::set_lamports_per_byte_to_1322::id()),
            ("rntTjNZ9boq8owDxjGVFHPfWNQPDaKiM5JcjxmDGg47", gate::set_lamports_per_byte_to_696::LAMPORTS_PER_BYTE, gate::set_lamports_per_byte_to_696::id()),
            ("rnt8ZQpz2HYhX3DkYBDGjJS1a36mYq69oXka7JrhEdi", gate::set_lamports_per_byte_to_6960::LAMPORTS_PER_BYTE, gate::set_lamports_per_byte_to_6960::id()),
        ];
        let expected = [6_333u64, 5_080, 2_575, 1_322, 696, 6_960];
        for (i, (id, per_byte, key)) in rent.iter().enumerate() {
            assert_eq!(key.to_string(), *id, "rent gate {i}");
            assert_eq!(*per_byte, expected[i], "rent gate {i} lamports per byte");
        }
        assert_eq!(gate::enable_tx_v1::id().to_string(), "txv1aq4pp281K9um3tnPgkfX8UqtFT6wcVW3hNezGLL");
        assert_eq!(gate::raise_block_limits_to_100m::id().to_string(), "P1BCUMpAC7V2GRBRiJCNUgpMyWZhoqt3LKo712ePqsz");
        assert_eq!(gate::raise_cpi_nesting_limit_to_8::id().to_string(), "6TkHkRmP7JZy1fdM6fg5uXn76wChQBWGokHBJzrLB3mj");
    }

    #[test]
    fn block_path_uses_agaves_replay_config() {
        let c = replay_config();
        assert!(!c.limit_to_load_programs);
        assert!(!c.drop_on_failure);
        assert!(!c.all_or_nothing);
        assert!(!c.strict_nonce_size_check);
    }

    /// Classifies every id in the pinned `agave-feature-set` by Anza's own
    /// projection, for `genesis/pin-features.py`. An id is `svm` when
    /// activating it alone changes `runtime_features()`, `bank` when it
    /// changes only the snapshot, and `cleaned` when it changes neither.
    #[test]
    #[ignore]
    fn print_feature_classes() {
        fn bytes(s: &solana_svm_feature_set::SVMFeatureSet) -> Vec<u8> {
            // SVMFeatureSet is a struct of bools: size 1, align 1, no padding.
            unsafe {
                std::slice::from_raw_parts(
                    s as *const _ as *const u8,
                    std::mem::size_of::<solana_svm_feature_set::SVMFeatureSet>(),
                )
            }
            .to_vec()
        }
        let none = FeatureSet::default();
        let mut ids: Vec<_> = agave_feature_set::FEATURE_NAMES.iter().collect();
        ids.sort_by_key(|(id, _)| id.to_string());
        for (id, description) in ids {
            let mut one = FeatureSet::default();
            one.activate(id, 0);
            let class = if bytes(&one.runtime_features()) != bytes(&none.runtime_features()) {
                "svm"
            } else if one.snapshot() != none.snapshot() {
                "bank"
            } else {
                "cleaned"
            };
            println!("FEATURE\t{id}\t{class}\t{description}");
        }
    }

    #[test]
    fn the_cpi_nesting_gate_sets_the_stack_depth() {
        let depth = |active: &[(Pubkey, u64)]| {
            let mut fs = FeatureSet::default();
            for (id, height) in active {
                fs.activate(id, *height);
            }
            let config = TransactionConfiguration {
                compute_unit_limit: 200_000,
                updated_heap_bytes: 32 * 1024,
                priority_fee_lamports: 0,
                loaded_accounts_data_size_limit: 64 * 1024 * 1024,
            };
            let budget = execution_budget(&config, &fs);
            // The two the caller set travel through untouched.
            assert_eq!(budget.compute_unit_limit, 200_000);
            assert_eq!(budget.heap_size, 32 * 1024);
            budget.max_instruction_stack_depth
        };
        assert_eq!(depth(&[]), 5, "the limit before SIMD-0268");
        assert_eq!(
            depth(&[(agave_feature_set::raise_cpi_nesting_limit_to_8::id(), 1)]),
            9,
            "one top-level instruction and eight nested CPIs",
        );
    }

    /// getFeeForMessage charges 5,000 per signature plus the priority fee,
    /// compute unit price times limit in micro-lamports, as Agave does.
    #[test]
    fn fee_for_message_includes_the_priority_fee() {
        let _guard = config_lock();
        configure(&[]);
        use solana_message::{compiled_instruction::CompiledInstruction, legacy::Message, MessageHeader};
        let message = |instructions: Vec<CompiledInstruction>| {
            bincode::serialize(&VersionedMessage::Legacy(Message {
                header: MessageHeader {
                    num_required_signatures: 1,
                    num_readonly_signed_accounts: 0,
                    num_readonly_unsigned_accounts: 1,
                },
                account_keys: vec![Pubkey::new_unique(), compute_budget::ID],
                recent_blockhash: Hash::default(),
                instructions,
            }))
            .unwrap()
        };
        let mut limit = vec![2u8];
        limit.extend_from_slice(&200_000u32.to_le_bytes());
        let mut price = vec![3u8];
        price.extend_from_slice(&1_000_000u64.to_le_bytes());
        let priced = message(vec![
            CompiledInstruction::new_from_raw_parts(1, limit, vec![]),
            CompiledInstruction::new_from_raw_parts(1, price, vec![]),
        ]);
        assert_eq!(fee_for_message(&priced), Ok(Some(5_000 + 200_000)));
        assert_eq!(fee_for_message(&message(vec![])), Ok(Some(5_000)));
        assert_eq!(fee_for_message(&[1, 2, 3]), Err(()));

        // A v0 message that names a lookup table is priced without loading it.
        // The fee is signature counts and the priority fee, and a program id
        // can never come from a table, so nothing here needs the addresses.
        use solana_message::v0;
        let v0 = bincode::serialize(&VersionedMessage::V0(v0::Message {
            header: MessageHeader {
                num_required_signatures: 1,
                num_readonly_signed_accounts: 0,
                num_readonly_unsigned_accounts: 1,
            },
            account_keys: vec![Pubkey::new_unique(), compute_budget::ID],
            recent_blockhash: Hash::default(),
            instructions: vec![],
            address_table_lookups: vec![v0::MessageAddressTableLookup {
                account_key: Pubkey::new_unique(),
                writable_indexes: vec![0, 1],
                readonly_indexes: vec![2],
            }],
        }))
        .unwrap();
        assert_eq!(fee_for_message(&v0), Ok(Some(5_000)));
    }

    /// The write set's three filters, as a truth table. The chain tests cover
    /// all four result cases, but execution cannot tell the invoked-only
    /// filter apart from write demotion.
    #[test]
    fn the_write_set_applies_three_filters() {
        // writable, touched, invoked, instruction account
        assert!(writes_back(true, true, false, false), "an ordinary written account");
        assert!(!writes_back(false, true, false, false), "a readonly account");
        assert!(!writes_back(true, false, false, false), "an untouched account");
        assert!(!writes_back(true, true, true, false), "a program the transaction invoked");
        assert!(writes_back(true, true, true, true), "a program that is also an instruction account");
    }

    #[test]
    #[allow(deprecated)]
    fn go_sysvar_layouts_decode_to_the_pinned_types() {
        let mut rent = 6_960u64.to_le_bytes().to_vec();
        rent.extend_from_slice(&[0, 0, 0, 0, 0, 0, 240, 63, 50]);
        let mut schedule = vec![0u8; 33];
        schedule[0..8].copy_from_slice(&432_000u64.to_le_bytes());
        schedule[8..16].copy_from_slice(&432_000u64.to_le_bytes());
        let mut clock = vec![0u8; 40];
        for (off, v) in [(0, 7u64), (8, 100), (16, 0), (24, 1), (32, 135)] {
            clock[off..off + 8].copy_from_slice(&v.to_le_bytes());
        }
        let mut slot_hashes = vec![0u8; 8 + 512 * 40];
        slot_hashes[0..8].copy_from_slice(&1u64.to_le_bytes());
        slot_hashes[8..16].copy_from_slice(&6u64.to_le_bytes());
        slot_hashes[16..48].copy_from_slice(&[9u8; 32]);
        let mut recent = vec![0u8; 8 + 150 * 40];
        recent[0..8].copy_from_slice(&1u64.to_le_bytes());
        recent[8..40].copy_from_slice(&[9u8; 32]);
        recent[40..48].copy_from_slice(&5_000u64.to_le_bytes());

        let accounts: HashMap<Pubkey, Vec<u8>> = [
            (SYSVAR_IDS[0], clock),
            (SYSVAR_IDS[1], schedule),
            (SYSVAR_IDS[2], vec![0u8; 81]),
            (SYSVAR_IDS[3], rent),
            (SYSVAR_IDS[4], slot_hashes),
            (SYSVAR_IDS[5], vec![0u8; 8 + 512 * 32]),
            (SYSVAR_IDS[6], vec![0u8; 8]),
            (SYSVAR_IDS[8], recent),
        ]
        .into_iter()
        .collect();
        let mut cache = solana_program_runtime::sysvar_cache::SysvarCache::default();
        cache.fill_missing_entries(|key, set| {
            if let Some(d) = accounts.get(key) {
                set(d)
            }
        });

        assert_eq!(*cache.get_rent().unwrap(), Rent::default());
        let s = cache.get_epoch_schedule().unwrap();
        assert_eq!((s.slots_per_epoch, s.warmup, s.first_normal_slot), (432_000, false, 0));
        assert_eq!(s.get_leader_schedule_epoch(7), 1, "Clock.leader_schedule_epoch is epoch + 1");
        let c = cache.get_clock().unwrap();
        assert_eq!((c.slot, c.epoch_start_timestamp, c.leader_schedule_epoch, c.unix_timestamp), (7, 100, 1, 135));
        assert!(!cache.get_epoch_rewards().unwrap().active);
        assert_eq!(cache.get_last_restart_slot().unwrap().last_restart_slot, 0);
        assert!(cache.get_stake_history().unwrap().is_empty());
        let sh = cache.get_slot_hashes().unwrap();
        assert_eq!(sh.len(), 1);
        assert_eq!(sh.get(&6), Some(&Hash::new_from_array([9; 32])));
        let rb = cache.get_recent_blockhashes().unwrap();
        assert_eq!(rb.len(), 1);
        assert_eq!(rb[0].blockhash, Hash::new_from_array([9; 32]));
        assert_eq!(rb[0].fee_calculator.lamports_per_signature, 5_000);
    }

    /// The instruction-data blobs are the output of Anza's own constructors
    /// over fixed vectors. Each one passed Anza's verifier when it was
    /// generated. ed25519 is RFC 8032 test vector 2. The malleable one is CCTV
    /// vector 5, where R is the identity point. `verify_strict` rejects it,
    /// and a plain `verify` would accept it. secp256k1 and secp256r1 are one
    /// fixed key each, signing "svm precompile vector".
    const ED25519_VALID: &str = "01003000ffff1000ffff70000100ffff3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c0072";
    const ED25519_MALLEABLE: &str = "01003000ffff1000ffff70001000ffff10eb7c3acfb2bed3e0d6ab89bf5a3d6afddd1176ce4812e38d9fd485058fdb1f00000000000000000000000000000000000000000000000000000000000000009472a69cd9a701a50d130ed52189e2455b23767db52cacb8716fb896ffeeac0965643235353139766563746f72732033";
    const SECP256K1_VALID: &str = "012000000c0000610015000076979e41b566d656524e8c64bd684a7cf1f1eaa1667622e1b94f76aa91a346e511a12777e4c737a3074185881dcc37572c87394f55e4b600564627f0d68e3ec16dd4696e53fe828ffe447d21afa1e4c41610321c0173766d20707265636f6d70696c6520766563746f72";
    const SECP256R1_VALID: &str = "01003100ffff1000ffff71001500ffff03798953e7e8134fdf3c139f63d3fbccc252a28b6ca5059e618374a81231240f3fc251469f178835e7c3ba38ee77e704e303ede6c2b42f0b256b4d5b0dead56cd375c2eddc2962b25268db2c13ee0118c436ee3ce9c8cb4f6832f0908ffdbe52fe73766d20707265636f6d70696c6520766563746f72";

    fn unhex(s: &str) -> Vec<u8> {
        (0..s.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&s[i..i + 2], 16).expect("test vector is lower-case hex"))
            .collect()
    }

    /// The three verification instructions, through the callback the block path
    /// runs them on. A valid signature verifies, a corrupted one does not, and
    /// an id that is not a precompile is not claimed as one.
    #[test]
    fn the_arena_verifies_the_three_precompiles() {
        let arena = Arena(HashMap::new());
        let cases = [
            ("ed25519", solana_sdk_ids::ed25519_program::ID, ED25519_VALID),
            ("secp256k1", solana_sdk_ids::secp256k1_program::ID, SECP256K1_VALID),
            ("secp256r1", solana_sdk_ids::secp256r1_program::ID, SECP256R1_VALID),
        ];
        for (name, id, vector) in cases {
            assert!(arena.is_precompile(&id), "{name} is a precompile");
            let data = unhex(vector);
            assert_eq!(
                arena.process_precompile(&id, &data, vec![&data]),
                Ok(()),
                "{name} valid signature"
            );
            // The last byte of every vector is the last byte of the signed
            // message, so flipping it is a signature over something else.
            let mut wrong = data.clone();
            *wrong.last_mut().unwrap() ^= 1;
            assert!(
                arena.process_precompile(&id, &wrong, vec![&wrong]).is_err(),
                "{name} over a changed message must not verify"
            );
        }

        let malleable = unhex(ED25519_MALLEABLE);
        assert_eq!(
            arena.process_precompile(&solana_sdk_ids::ed25519_program::ID, &malleable, vec![&malleable]),
            Err(PrecompileError::InvalidSignature),
            "verify_strict rejects the CCTV vector a plain verify would accept"
        );

        assert!(!arena.is_precompile(&solana_sdk_ids::system_program::ID), "the System program is not a precompile");
        let data = unhex(ED25519_VALID);
        assert_eq!(
            arena.process_precompile(&solana_sdk_ids::system_program::ID, &data, vec![&data]),
            Err(PrecompileError::InvalidPublicKey),
            "an id that is not a precompile falls through rather than verifying"
        );
    }

    /// Every entry of Anza's table is ungated at this pin. So the callback can
    /// answer `|_| true` and pass the verifiers a feature set that nothing
    /// reads. A crate bump that adds a gated precompile fails here, and does
    /// not verify under the wrong rules.
    #[test]
    fn every_precompile_is_ungated() {
        let table = agave_precompiles::get_precompiles();
        assert_eq!(table.len(), 3, "the precompile table is three verifiers");
        let ids = [
            "KeccakSecp256k11111111111111111111111111111",
            "Ed25519SigVerify111111111111111111111111111",
            "Secp256r1SigVerify1111111111111111111111111",
        ];
        for (i, precompile) in table.iter().enumerate() {
            assert_eq!(precompile.program_id.to_string(), ids[i], "precompile {i}");
            assert!(precompile.feature.is_none(), "precompile {i} grew a feature gate");
        }
    }

    /// The other half of the same guard. The verifiers ignore the feature set
    /// they get, so `VERIFIER_GATES` cannot change a result. If a bump makes
    /// one of them read it, the two answers stop agreeing.
    #[test]
    fn precompile_verification_ignores_the_gates() {
        let all = FeatureSet::all_enabled();
        for (name, id, vector, valid) in [
            ("ed25519", solana_sdk_ids::ed25519_program::ID, ED25519_VALID, true),
            ("ed25519 malleable", solana_sdk_ids::ed25519_program::ID, ED25519_MALLEABLE, false),
            ("secp256k1", solana_sdk_ids::secp256k1_program::ID, SECP256K1_VALID, true),
            ("secp256r1", solana_sdk_ids::secp256r1_program::ID, SECP256R1_VALID, true),
        ] {
            let data = unhex(vector);
            let precompile = agave_precompiles::get_precompile(&id, |_| true).unwrap();
            let pinned = precompile.verify(&data, &[&data], &VERIFIER_GATES);
            assert_eq!(pinned.is_ok(), valid, "{name} under the gates the engine passes");
            assert_eq!(pinned, precompile.verify(&data, &[&data], &all), "{name} read the feature set");
        }
    }

    /// Both epoch stake syscalls answer zero, written out and not inherited, on
    /// the arena and on the view a group runs against. This chain has no vote
    /// accounts, so zero is the only true answer. If the trait default were
    /// inherited, an Agave bump that changed it would change a consensus input
    /// with no diff here to review.
    #[test]
    fn the_epoch_stake_syscalls_answer_zero() {
        let arena = Arena(HashMap::new());
        let vote = Pubkey::new_unique();
        assert_eq!(arena.get_epoch_stake(), 0);
        assert_eq!(arena.get_epoch_stake_for_vote_account(&vote), 0);
        let view = View { base: &arena, local: HashMap::new() };
        assert_eq!(view.get_epoch_stake(), 0);
        assert_eq!(view.get_epoch_stake_for_vote_account(&vote), 0);
    }

    /// The builtin table, and the ZK ElGamal proof row in particular. The row
    /// is unconditional, because the table has no gate column. A gate column
    /// would reclassify `zk_elgamal_proof_program_enabled` from `cleaned` to
    /// `svm`, and give governance the power to unregister a builtin. The
    /// program reads its own counter-gates from the feature set the engine
    /// passes, which `the_zk_program_runs_at_the_pin` tests.
    #[test]
    fn the_builtin_table_is_six_unconditional_rows() {
        let expected = [
            ("11111111111111111111111111111111", "solana_system_program"),
            ("BPFLoader1111111111111111111111111111111111", "solana_bpf_loader_program"),
            ("BPFLoader2111111111111111111111111111111111", "solana_bpf_loader_program"),
            ("BPFLoaderUpgradeab1e11111111111111111111111", "solana_bpf_loader_upgradeable_program"),
            ("ComputeBudget111111111111111111111111111111", "compute_budget_program"),
            ("ZkE1Gama1Proof11111111111111111111111111111", "zk_elgamal_proof_program"),
        ];
        assert_eq!(BUILTINS.len(), expected.len());
        for (i, (id, name, _)) in BUILTINS.iter().enumerate() {
            assert_eq!((id.to_string().as_str(), *name), expected[i], "builtin {i}");
        }
        // A registered builtin is in the root's cache whatever the gates say,
        // which is what "no gate column" means.
        for gates in [vec![], vec![(agave_feature_set::zk_elgamal_proof_program_enabled::id(), 0u64)]] {
            let root = build_root(&gates);
            let cache = root.processor.global_program_cache.read().unwrap();
            for (id, _, _) in BUILTINS {
                assert!(cache.get_flattened_entries_for_tests().iter().any(|(key, _)| *key == id),
                    "{id} is not registered with gates {gates:?}");
            }
        }
    }

    /// The program checks `disable_zk_elgamal_proof_program && !reenable`
    /// against the feature set the engine hands it. Both gates are active in
    /// the pinned set, so the pair is false and the program runs. Solana
    /// reversed a gate by adding another, not by deactivating it in place. A
    /// set with only the first gate would turn the program off.
    #[test]
    fn the_zk_program_runs_at_the_pin() {
        let pinned = build_root(&[
            (agave_feature_set::disable_zk_elgamal_proof_program::id(), 0),
            (agave_feature_set::reenable_zk_elgamal_proof_program::id(), 0),
        ]);
        let features = pinned.feature_set.runtime_features();
        assert!(features.disable_zk_elgamal_proof_program && features.reenable_zk_elgamal_proof_program);
        assert!(
            !(features.disable_zk_elgamal_proof_program && !features.reenable_zk_elgamal_proof_program),
            "the program's own check refuses to run at the pinned gate set"
        );
    }

    /// A simulation of the block in flight does not see what that block
    /// deploys. It runs the entry of the block path for the code that committed
    /// state holds. P is an upgradeable program with the code of memo-v1,
    /// deployed at height 1, and the block path has compiled it. Block H
    /// upgrades P to the code of Serum v3. A simulation of H sends P a memo. It
    /// runs memo-v1 and succeeds in three cases: its root was named before the
    /// block ran, after the block ran, or so late that the next block has
    /// compiled the upgrade. The last case is real, because the first request
    /// of an RPC node after a commit can come while the block runs. Serum
    /// refuses the memo. The entry of the upgrade is a delay-visibility
    /// tombstone at H, which refuses to run at all.
    #[test]
    fn a_simulation_does_not_see_what_the_block_in_flight_deploys() {
        use solana_loader_v3_interface::{get_program_data_address, instruction::upgrade, state::UpgradeableLoaderState};
        use solana_message::Message;
        use solana_transaction::Transaction;
        let _guard = config_lock();
        let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("..");
        let read = |name: &str| std::fs::read(dir.join(name)).unwrap_or_else(|_| panic!("{name}: run make fetch-programs"));
        let (memo, serum) = (read("genesis/programs/memo-v1.so"), read("programs/serum-v3.so"));
        let height = crate::conformance::SLOT;
        let (payer, program, buffer) = (Pubkey::new_unique(), Pubkey::new_unique(), Pubkey::new_unique());
        let programdata = get_program_data_address(&program);
        let account = |owner: Pubkey, executable: bool, data: Vec<u8>| {
            AccountSharedData::from(solana_account::Account {
                lamports: 10_000_000_000,
                data,
                owner,
                executable,
                rent_epoch: u64::MAX,
            })
        };
        // A loader state, then the code, then zeros up to `len`.
        let state = |loader_state: UpgradeableLoaderState, elf: &[u8], len: usize| {
            let mut data = bincode::serialize(&loader_state).unwrap();
            data.extend_from_slice(elf);
            data.resize(len.max(data.len()), 0);
            account(bpf_loader_upgradeable::ID, false, data)
        };
        let mut committed: HashMap<Pubkey, AccountSharedData> =
            crate::conformance::sysvar_accounts(crate::conformance::RENT_PER_BYTE_YEAR)
                .into_iter()
                .map(|(k, a)| (k, a.into()))
                .collect();
        committed.insert(payer, account(solana_sdk_ids::system_program::ID, false, vec![]));
        committed.insert(
            bpf_loader_upgradeable::ID,
            account(solana_sdk_ids::native_loader::ID, true, b"solana_bpf_loader_upgradeable_program".to_vec()),
        );
        let mut p = state(UpgradeableLoaderState::Program { programdata_address: programdata }, &[], 0);
        p.set_executable(true);
        committed.insert(program, p);
        // Room for the upgrade's code.
        let len = UpgradeableLoaderState::size_of_programdata(serum.len());
        let deployed = UpgradeableLoaderState::ProgramData { slot: 1, upgrade_authority_address: Some(payer) };
        committed.insert(programdata, state(deployed, &memo, len));
        let blockhash = Hash::new_from_array([5; 32]);

        configure(&[]);
        let block = root().unwrap();
        // The block before compiled P from committed state.
        crate::programs::prewarm(&block, &Arena(committed.clone()), height - 1, &[program], 0);
        let entry = |root: &Arc<Root>, slot: u64| {
            root.processor.global_program_cache.read().unwrap().get_flattened_entries_for_tests().into_iter()
                .find(|(k, e)| *k == program && e.deployment_slot == slot)
                .map(|(_, e)| e)
        };
        let compiled = entry(&block, 1).expect("the block path compiled P");

        let memo_tx = || {
            let mut message = Message::new(&[solana_instruction::Instruction::new_with_bytes(program, b"memo", vec![])], Some(&payer));
            message.recent_blockhash = blockhash;
            VersionedTransaction::from(Transaction::new_unsigned(message))
        };
        let simulate_memo = || {
            let bytes = wincode::serialize(&memo_tx()).unwrap();
            let sim = simulate(height, [5; 32], [6; 32], &bytes, false, true, false, committed.clone(), &HashSet::new());
            sim.unwrap_or_else(|_| panic!("the simulation did not run"))
        };

        configure_next(height, &[]);
        let next = next_root(height).unwrap();
        assert!(Arc::ptr_eq(&entry(&next, 1).unwrap(), &compiled), "the next block's root holds the block path's entry");

        // Block H upgrades P.
        let mut accounts = committed.clone();
        accounts.insert(buffer, state(UpgradeableLoaderState::Buffer { authority_address: Some(payer) }, &serum, 0));
        let mut message = Message::new(&[upgrade(&program, &buffer, &payer, &payer)], Some(&payer));
        message.recent_blockhash = blockhash;
        let tx = RuntimeTransaction::<SanitizedTransaction>::try_create(
            VersionedTransaction::from(Transaction::new_unsigned(message)),
            MessageHash::Compute,
            None,
            SimpleAddressLoader::Enabled(LoadedAddresses::default()),
            &block.reserved,
        )
        .unwrap();
        let Ok((outcomes, writes, _)) =
            execute_block(height, [5; 32], vec![Some(tx)], &[], &[[1; 64]], &[0], accounts, &HashSet::new())
        else {
            panic!("the block did not run");
        };
        assert_eq!(outcomes[0].status, Ok(()), "the upgrade: {:?}", outcomes[0].meta.logs);
        assert!(entry(&block, height).is_some(), "the block merged the upgrade at H");

        // The block merges the upgrade uncompiled. The first block after it
        // that invokes P compiles it.
        let mut after = committed.clone();
        after.extend(writes);
        let next_block = || {
            let tx = RuntimeTransaction::<SanitizedTransaction>::try_create(
                memo_tx(),
                MessageHash::Compute,
                None,
                SimpleAddressLoader::Enabled(LoadedAddresses::default()),
                &block.reserved,
            )
            .unwrap();
            let ran = execute_block(height + 1, [5; 32], vec![Some(tx)], &[], &[[2; 64]], &[0], after.clone(), &HashSet::new());
            assert!(ran.is_ok_and(|(outcomes, _, _)| outcomes[0].processed && outcomes[0].status.is_err()), "Serum refuses the memo");
            let compiled = block.processor.global_program_cache.read().unwrap().get_flattened_entries();
            assert!(compiled.iter().any(|(k, _, e)| *k == program && e.deployment_slot == height), "H+1 compiled the upgrade");
        };

        for when in ["named before the block", "named after the block", "named after the next block compiled the upgrade"] {
            match when {
                "named before the block" => {}
                "named after the block" => configure_next(height, &[]),
                _ => {
                    next_block();
                    configure_next(height, &[]);
                }
            }
            let next = next_root(height).unwrap();
            let sim = simulate_memo();
            assert_eq!(sim.status, Ok(()), "{when}: {:?}", sim.logs);
            assert!(entry(&next, height).is_none(), "{when}: the upgrade reached the simulation's root");
            assert!(Arc::ptr_eq(&entry(&next, 1).unwrap(), &compiled), "{when}: the simulation compiled P again");
        }
    }
}
