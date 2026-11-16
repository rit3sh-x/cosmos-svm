//! Vendored from `tests/mock_bank.rs` in `solana-svm` 4.2.1. The published
//! library does not export it.
//!
//! It gives a minimal `TransactionProcessingCallback`, to run
//! `TransactionBatchProcessor` without a Bank.
//!
//! Changes from upstream:
//! - `deploy_program_bytes` replaces path-based program loading. It takes the
//!   program bytes and an explicit program ID.
//! - The unused `override_feature_set` is removed.
//! - `inspect_account` is a no-op. Upstream's test-only locking and allocations
//!   would distort the benchmark results.

#![allow(unused)]

#[allow(deprecated)]
use solana_sysvar::recent_blockhashes::{Entry as BlockhashesEntry, RecentBlockhashes};
use {
    solana_account::{Account, AccountSharedData, ReadableAccount, WritableAccount},
    solana_clock::{Clock, Slot, UnixTimestamp},
    solana_epoch_schedule::EpochSchedule,
    solana_fee_structure::{FeeDetails, FeeStructure},
    solana_loader_v3_interface::{self as bpf_loader_upgradeable, state::UpgradeableLoaderState},
    solana_program_runtime::{
        execution_budget::{SVMTransactionExecutionBudget, SVMTransactionExecutionCost},
        invoke_context::InvokeContext,
        loaded_programs::{BlockRelation, ForkGraph, ProgramRuntimeEnvironment},
        program_cache_entry::ProgramCacheEntry,
        solana_sbpf::program::BuiltinFunctionDefinition,
    },
    solana_pubkey::Pubkey,
    solana_rent::Rent,
    solana_sdk_ids::{bpf_loader, bpf_loader_deprecated, compute_budget},
    solana_svm::transaction_processor::TransactionBatchProcessor,
    solana_svm_callback::{AccountState, InvokeContextCallback, TransactionProcessingCallback},
    solana_svm_feature_set::SVMFeatureSet,
    solana_svm_transaction::svm_message::SVMMessage,
    solana_svm_type_overrides::sync::{Arc, RwLock},
    solana_sysvar_id::SysvarId,
    std::{cmp::Ordering, collections::HashMap},
};

pub const EXECUTION_SLOT: u64 = 5; // The execution slot must be greater than the deployment slot
pub const EXECUTION_EPOCH: u64 = 2; // The execution epoch must be greater than the deployment epoch
pub const WALLCLOCK_TIME: i64 = 1704067200; // Arbitrarily Jan 1, 2024

pub struct MockForkGraph {}

impl ForkGraph for MockForkGraph {
    fn relationship(&self, a: Slot, b: Slot) -> BlockRelation {
        match a.cmp(&b) {
            Ordering::Less => BlockRelation::Ancestor,
            Ordering::Equal => BlockRelation::Equal,
            Ordering::Greater => BlockRelation::Descendant,
        }
    }
}

#[derive(Default, Clone)]
pub struct MockBankCallback {
    pub feature_set: SVMFeatureSet,
    pub account_shared_data: Arc<RwLock<HashMap<Pubkey, AccountSharedData>>>,
    #[allow(clippy::type_complexity)]
    pub inspected_accounts:
        Arc<RwLock<HashMap<Pubkey, Vec<(Option<AccountSharedData>, /* is_writable */ bool)>>>>,
}

impl InvokeContextCallback for MockBankCallback {}

impl TransactionProcessingCallback for MockBankCallback {
    fn get_account_shared_data(&self, pubkey: &Pubkey) -> Option<(AccountSharedData, Slot)> {
        self.account_shared_data
            .read()
            .unwrap()
            .get(pubkey)
            .map(|account| (account.clone(), 0))
    }

    // A no-op, unlike upstream. See the module header.
    fn inspect_account(&self, _address: &Pubkey, _account_state: AccountState, _is_writable: bool) {
    }
}

impl MockBankCallback {
    pub fn calculate_fee_details(message: &impl SVMMessage, prioritization_fee: u64) -> FeeDetails {
        let signature_count = message
            .num_transaction_signatures()
            .saturating_add(message.num_ed25519_signatures())
            .saturating_add(message.num_secp256k1_signatures())
            .saturating_add(message.num_secp256r1_signatures());

        FeeDetails::new(
            signature_count.saturating_mul(FeeStructure::default().lamports_per_signature),
            prioritization_fee,
        )
    }

    pub fn add_builtin(
        &self,
        batch_processor: &TransactionBatchProcessor<MockForkGraph>,
        program_id: Pubkey,
        name: &str,
        builtin: ProgramCacheEntry,
    ) {
        let account_data = AccountSharedData::from(Account {
            lamports: 5000,
            data: name.as_bytes().to_vec(),
            owner: solana_sdk_ids::native_loader::id(),
            executable: true,
            rent_epoch: 0,
        });

        self.account_shared_data
            .write()
            .unwrap()
            .insert(program_id, account_data);

        batch_processor.add_builtin(program_id, builtin);
    }

    pub fn configure_sysvars(&self) {
        // The sysvar cache needs these entries.
        let clock = Clock {
            slot: EXECUTION_SLOT,
            epoch_start_timestamp: WALLCLOCK_TIME.saturating_sub(10) as UnixTimestamp,
            epoch: EXECUTION_EPOCH,
            leader_schedule_epoch: EXECUTION_EPOCH,
            unix_timestamp: WALLCLOCK_TIME as UnixTimestamp,
        };

        let mut account_data = AccountSharedData::default();
        account_data.set_data(bincode::serialize(&clock).unwrap());
        self.account_shared_data
            .write()
            .unwrap()
            .insert(Clock::id(), account_data);

        let rent = Rent::default();

        let mut account_data = AccountSharedData::default();
        account_data.set_data(bincode::serialize(&rent).unwrap());
        self.account_shared_data
            .write()
            .unwrap()
            .insert(Rent::id(), account_data);

        // SystemInstruction::AdvanceNonceAccount asserts that RecentBlockhashes
        // is not empty, but it reads the blockhash from InvokeContext. The
        // sysvar does not need real entries.
        #[allow(deprecated)]
        let recent_blockhashes = vec![BlockhashesEntry::default()];

        let mut account_data = AccountSharedData::default();
        account_data.set_data(bincode::serialize(&recent_blockhashes).unwrap());
        #[allow(deprecated)]
        self.account_shared_data
            .write()
            .unwrap()
            .insert(RecentBlockhashes::id(), account_data);

        // A LoaderV3 deploy that is not mocked needs EpochSchedule.
        let epoch_schedule = EpochSchedule::without_warmup();

        let mut account_data = AccountSharedData::default();
        account_data.set_data(bincode::serialize(&epoch_schedule).unwrap());
        self.account_shared_data
            .write()
            .unwrap()
            .insert(EpochSchedule::id(), account_data);
    }
}

/// Upstream's `deploy_program` derives the program address from a seed and
/// reads the ELF from disk. This function takes both as arguments, so a program
/// dumped from mainnet deploys at its mainnet address.
///
/// The account layout is upstream's, unchanged: a LoaderV3 `Program` account
/// pointing at a `ProgramData` account that holds the metadata header followed
/// by the ELF.
pub fn deploy_program_bytes(
    program_id: Pubkey,
    elf: &[u8],
    deployment_slot: Slot,
    mock_bank: &MockBankCallback,
) {
    let rent = Rent::default();
    let program_data_account = bpf_loader_upgradeable::get_program_data_address(&program_id);

    let state = UpgradeableLoaderState::Program {
        programdata_address: program_data_account,
    };

    let mut account_data = AccountSharedData::default();
    let buffer = bincode::serialize(&state).unwrap();
    account_data.set_lamports(rent.minimum_balance(buffer.len()));
    account_data.set_owner(solana_sdk_ids::bpf_loader_upgradeable::id());
    account_data.set_executable(true);
    account_data.set_data(buffer);
    mock_bank
        .account_shared_data
        .write()
        .unwrap()
        .insert(program_id, account_data);

    let mut account_data = AccountSharedData::default();
    let state = UpgradeableLoaderState::ProgramData {
        slot: deployment_slot,
        upgrade_authority_address: None,
    };
    let mut header = bincode::serialize(&state).unwrap();
    let mut complement = vec![
        0;
        std::cmp::max(
            0,
            UpgradeableLoaderState::size_of_programdata_metadata().saturating_sub(header.len())
        )
    ];
    let mut buffer = elf.to_vec();
    header.append(&mut complement);
    header.append(&mut buffer);
    account_data.set_lamports(rent.minimum_balance(header.len()));
    account_data.set_owner(solana_sdk_ids::bpf_loader_upgradeable::id());
    account_data.set_data(header);
    mock_bank
        .account_shared_data
        .write()
        .unwrap()
        .insert(program_data_account, account_data);
}

pub fn register_builtins(
    mock_bank: &MockBankCallback,
    batch_processor: &TransactionBatchProcessor<MockForkGraph>,
) {
    const DEPLOYMENT_SLOT: u64 = 0;
    // LoaderV3 must be a loadable account, or programs do not execute.
    let loader_v3_name = "solana_bpf_loader_upgradeable_program";
    mock_bank.add_builtin(
        batch_processor,
        solana_sdk_ids::bpf_loader_upgradeable::id(),
        loader_v3_name,
        ProgramCacheEntry::new_builtin(
            DEPLOYMENT_SLOT,
            solana_bpf_loader_program::Entrypoint::register,
        ),
    );

    let loader_v1_name = "solana_bpf_loader_deprecated_program";
    mock_bank.add_builtin(
        batch_processor,
        bpf_loader_deprecated::id(),
        loader_v1_name,
        ProgramCacheEntry::new_builtin(
            DEPLOYMENT_SLOT,
            solana_bpf_loader_program::Entrypoint::register,
        ),
    );

    let loader_v2_name = "solana_bpf_loader_program";
    mock_bank.add_builtin(
        batch_processor,
        bpf_loader::id(),
        loader_v2_name,
        ProgramCacheEntry::new_builtin(
            DEPLOYMENT_SLOT,
            solana_bpf_loader_program::Entrypoint::register,
        ),
    );

    let system_program_name = "system_program";
    mock_bank.add_builtin(
        batch_processor,
        solana_system_program::id(),
        system_program_name,
        ProgramCacheEntry::new_builtin(
            DEPLOYMENT_SLOT,
            solana_system_program::system_processor::Entrypoint::register,
        ),
    );

    let compute_budget_program_name = "compute_budget_program";
    mock_bank.add_builtin(
        batch_processor,
        compute_budget::id(),
        compute_budget_program_name,
        ProgramCacheEntry::new_builtin(
            DEPLOYMENT_SLOT,
            solana_compute_budget_program::Entrypoint::register,
        ),
    );
}

/// Upstream's `create_custom_loader` hand-writes a `Config` and registers 13
/// syscalls. That is a test loader. It hardcodes `enable_stack_frame_gaps`,
/// `aligned_memory_mapping` and the sbpf version range instead of deriving them
/// from the feature set. It turns on register tracing and leaves out most of
/// the syscall table.
///
/// A benchmark compared with mainnet must run the environment mainnet runs, so
/// this calls Agave's own builder. `reject_deployment_of_broken_elfs` is true
/// and `debugging_features` is false, as on a validator.
pub fn create_runtime_environment(feature_set: &SVMFeatureSet) -> ProgramRuntimeEnvironment {
    solana_syscalls::create_program_runtime_environment(
        feature_set,
        &SVMTransactionExecutionBudget::default(),
        true,
        false,
    )
    .expect("failed to build program runtime environment")
}
