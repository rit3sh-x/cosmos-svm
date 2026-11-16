//! Shared SVM execution harness.
//!
//! Another benchmark measures account commitment in Cosmos IAVL. This crate
//! measures the execution that produces those writes, so the two costs can be
//! compared for parallel-execution trade-offs.
//!
//! - `benches/execution.rs`: Criterion execution timings (`make bench`).
//! - `src/main.rs`: compute units, allocations, program-cache memory, program
//!   size sweep, and thread scaling (`cargo run --release --locked` here).
//!
//! It measures only `load_and_execute_sanitized_transactions`. Signature
//! verification is not in it. IAVL account loading and commitment are measured
//! separately.

pub mod mock_bank;

use {
    mock_bank::{
        create_runtime_environment, deploy_program_bytes, register_builtins, MockBankCallback,
        MockForkGraph, EXECUTION_EPOCH, EXECUTION_SLOT,
    },
    solana_account::{AccountSharedData, ReadableAccount, WritableAccount},
    solana_compute_budget::compute_budget_limits::ComputeBudgetLimits,
    solana_fee_structure::FeeDetails,
    solana_hash::Hash,
    solana_instruction::{AccountMeta, Instruction},
    solana_keypair::Keypair,
    solana_program_runtime::loaded_programs::ProgramRuntimeEnvironments,
    solana_pubkey::Pubkey,
    solana_rent::Rent,
    solana_signer::Signer,
    solana_svm::{
        account_loader::{CheckedTransactionDetails, TransactionCheckResult},
        transaction_processor::{
            ExecutionRecordingConfig, TransactionBatchProcessor, TransactionProcessingConfig,
            TransactionProcessingEnvironment,
        },
    },
    solana_svm_feature_set::SVMFeatureSet,
    solana_svm_type_overrides::sync::{Arc, RwLock},
    solana_transaction::{sanitized::SanitizedTransaction, Transaction},
};

pub const LAMPORTS_PER_SIGNATURE: u64 = 5000;
pub const LAST_BLOCKHASH: Hash = Hash::new_from_array([7; 32]);

/// The real SPL Token program, dumped from mainnet at its real address.
pub const SPL_TOKEN_ELF: &[u8] = include_bytes!("../../../programs/spl_token.so");
pub const SPL_TOKEN_ID: Pubkey = Pubkey::from_str_const("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA");

/// An SPL Token `Account` is 165 bytes. The layout is fixed and public (see the
/// program's state module). Only the fields that Transfer reads are set.
pub const TOKEN_ACCOUNT_LEN: usize = 165;
pub const TOKEN_STATE_INITIALIZED: u8 = 1;

/// The batch sizes every grid runs: one transaction, a busy block's group, and
/// a block at the unit limit's order of magnitude.
pub const BATCHES: &[usize] = &[1, 1000, 10_000];

pub struct World {
    pub bank: MockBankCallback,
    #[allow(dead_code)]
    fork_graph: Arc<RwLock<MockForkGraph>>,
    pub processor: TransactionBatchProcessor<MockForkGraph>,
    pub env: TransactionProcessingEnvironment,
    pub config: TransactionProcessingConfig<'static>,
}

impl World {
    pub fn new() -> Self {
        let bank = MockBankCallback {
            feature_set: SVMFeatureSet::all_enabled(),
            ..Default::default()
        };
        let fork_graph = Arc::new(RwLock::new(MockForkGraph {}));
        let processor = TransactionBatchProcessor::new(
            EXECUTION_SLOT,
            EXECUTION_EPOCH,
            Arc::downgrade(&fork_graph),
            Some(create_runtime_environment(&SVMFeatureSet::all_enabled())),
        );

        bank.configure_sysvars();
        processor.fill_missing_sysvar_cache_entries(&bank);
        register_builtins(&bank, &processor);

        deploy_program_bytes(SPL_TOKEN_ID, SPL_TOKEN_ELF, 0, &bank);

        let env = TransactionProcessingEnvironment {
            blockhash: LAST_BLOCKHASH,
            blockhash_lamports_per_signature: LAMPORTS_PER_SIGNATURE,
            alpenglow_migration_succeeded: false,
            epoch_total_stake: 0,
            feature_set: SVMFeatureSet::all_enabled(),
            program_runtime_environments: ProgramRuntimeEnvironments::new(
                processor.program_runtime_environment_for_epoch(EXECUTION_EPOCH),
                processor.program_runtime_environment_for_epoch(EXECUTION_EPOCH),
            ),
            rent: Rent::default(),
        };

        // Recording is off. Logs and return data serve RPC, not consensus.
        // x/svm would turn them on only for a simulate call.
        let config = TransactionProcessingConfig {
            recording_config: ExecutionRecordingConfig {
                enable_log_recording: false,
                enable_return_data_recording: false,
                enable_cpi_recording: false,
                enable_transaction_balance_recording: false,
            },
            ..Default::default()
        };

        Self {
            bank,
            fork_graph,
            processor,
            env,
            config,
        }
    }

    pub fn insert(&self, key: Pubkey, account: AccountSharedData) {
        self.bank
            .account_shared_data
            .write()
            .unwrap()
            .insert(key, account);
    }

    /// Drops the SPL Token program from the cache, so the next batch pays ELF
    /// load plus verify plus JIT compile.
    pub fn evict_spl_token(&self) {
        self.processor
            .global_program_cache
            .write()
            .unwrap()
            .remove_programs(std::iter::once(SPL_TOKEN_ID));
    }
}

pub fn system_account(lamports: u64) -> AccountSharedData {
    let mut a = AccountSharedData::default();
    a.set_lamports(lamports);
    a.set_owner(solana_system_program::id());
    a
}

pub fn token_account(mint: &Pubkey, owner: &Pubkey, amount: u64) -> AccountSharedData {
    let mut data = vec![0u8; TOKEN_ACCOUNT_LEN];
    data[0..32].copy_from_slice(mint.as_ref());
    data[32..64].copy_from_slice(owner.as_ref());
    data[64..72].copy_from_slice(&amount.to_le_bytes());
    // delegate: COption::None at 72..108, already zero
    data[108] = TOKEN_STATE_INITIALIZED;
    // is_native: COption::None at 109..121, delegated_amount 121..129,
    // close_authority COption::None 129..165, all zero

    let mut a = AccountSharedData::default();
    a.set_lamports(Rent::default().minimum_balance(TOKEN_ACCOUNT_LEN));
    a.set_owner(SPL_TOKEN_ID);
    a.set_data(data);
    a
}

pub fn check_results(txs: &[SanitizedTransaction]) -> Vec<TransactionCheckResult> {
    check_results_with_limit(txs, ComputeBudgetLimits::default().compute_unit_limit)
}

/// The limit in `ComputeBudgetLimits::default()` is MAX_COMPUTE_UNIT_LIMIT
/// (1.4M), not mainnet's implicit 200k per transaction. The benchmark needs no
/// artificial cap. The self-check needs an exact one.
pub fn check_results_with_limit(
    txs: &[SanitizedTransaction],
    compute_unit_limit: u32,
) -> Vec<TransactionCheckResult> {
    let limits = ComputeBudgetLimits {
        compute_unit_limit,
        ..ComputeBudgetLimits::default()
    };
    txs.iter()
        .map(|tx| {
            // No precompiles in these batches, so the transaction signatures
            // are the whole count.
            let signature_count = tx.signatures().len() as u64;
            let budget = limits.get_compute_budget_and_limits(
                limits.loaded_accounts_bytes,
                FeeDetails::new(
                    signature_count.saturating_mul(LAMPORTS_PER_SIGNATURE),
                    limits.get_prioritization_fee(),
                ),
                true,
            );
            Ok(CheckedTransactionDetails::new(None, budget))
        })
        .collect()
}

/// `n` independent system-program lamport transfers. Independent because every
/// transaction has its own fee payer and its own destination.
pub fn build_transfers(world: &World, n: usize) -> Vec<SanitizedTransaction> {
    (0..n)
        .map(|_| {
            let payer = Keypair::new();
            let dest = Pubkey::new_unique();
            world.insert(payer.pubkey(), system_account(1_000_000_000));
            world.insert(dest, system_account(1_000_000));

            let ix = solana_system_interface::instruction::transfer(&payer.pubkey(), &dest, 1);
            let tx = Transaction::new_signed_with_payer(
                &[ix],
                Some(&payer.pubkey()),
                &[&payer],
                LAST_BLOCKHASH,
            );
            SanitizedTransaction::from_transaction_for_tests(tx)
        })
        .collect()
}

/// `n` independent SPL Token transfers against the real mainnet program,
/// `ix_per_tx` instructions in each transaction.
///
/// Varying `ix_per_tx` separates the fixed per-transaction cost from the marginal
/// cost of one more program invocation. That slope is what turns a compute-unit
/// count into a time estimate.
pub fn build_spl_multi(world: &World, n: usize, ix_per_tx: usize) -> Vec<SanitizedTransaction> {
    let mint = Pubkey::new_unique();
    (0..n)
        .map(|_| {
            let owner = Keypair::new();
            world.insert(owner.pubkey(), system_account(1_000_000_000));

            let instructions: Vec<Instruction> = (0..ix_per_tx)
                .map(|_| {
                    let src = Pubkey::new_unique();
                    let dst = Pubkey::new_unique();
                    world.insert(src, token_account(&mint, &owner.pubkey(), 1_000_000));
                    world.insert(dst, token_account(&mint, &owner.pubkey(), 0));

                    // SPL Token `Transfer` is tag 3 followed by a u64 amount.
                    let mut data = vec![3u8];
                    data.extend_from_slice(&1u64.to_le_bytes());
                    Instruction::new_with_bytes(
                        SPL_TOKEN_ID,
                        &data,
                        vec![
                            AccountMeta::new(src, false),
                            AccountMeta::new(dst, false),
                            AccountMeta::new_readonly(owner.pubkey(), true),
                        ],
                    )
                })
                .collect();

            let tx = Transaction::new_signed_with_payer(
                &instructions,
                Some(&owner.pubkey()),
                &[&owner],
                LAST_BLOCKHASH,
            );
            SanitizedTransaction::from_transaction_for_tests(tx)
        })
        .collect()
}

pub fn build_spl_transfers(world: &World, n: usize) -> Vec<SanitizedTransaction> {
    build_spl_multi(world, n, 1)
}

pub fn build_spl_4(world: &World, n: usize) -> Vec<SanitizedTransaction> {
    build_spl_multi(world, n, 4)
}

pub fn build_spl_16(world: &World, n: usize) -> Vec<SanitizedTransaction> {
    build_spl_multi(world, n, 16)
}

fn run_once(
    world: &World,
    tx: &SanitizedTransaction,
    cu_limit: u32,
) -> Result<(Vec<(Pubkey, AccountSharedData)>, u64), String> {
    let txs = std::slice::from_ref(tx);
    let checks = check_results_with_limit(txs, cu_limit);
    let out = world.processor.load_and_execute_sanitized_transactions(
        &world.bank,
        txs,
        checks,
        &world.env,
        &world.config,
    );
    match out.processing_results.into_iter().next().unwrap() {
        Ok(solana_svm::transaction_processing_result::ProcessedTransaction::Executed(e)) => {
            match e.execution_details.status {
                Ok(()) => Ok((
                    e.loaded_transaction.accounts,
                    e.execution_details.executed_units,
                )),
                Err(err) => Err(format!("{err:?}")),
            }
        }
        Ok(other) => Err(format!("fees-only: {:?}", other.status())),
        Err(e) => Err(format!("not processed: {e:?}")),
    }
}

fn post_accounts(
    world: &World,
    txs: &[SanitizedTransaction],
) -> Vec<Vec<(Pubkey, AccountSharedData)>> {
    let checks = check_results(txs);
    let out = world.processor.load_and_execute_sanitized_transactions(
        &world.bank,
        txs,
        checks,
        &world.env,
        &world.config,
    );
    out.processing_results
        .into_iter()
        .map(|r| match r.expect("not processed") {
            solana_svm::transaction_processing_result::ProcessedTransaction::Executed(e) => {
                assert!(
                    e.execution_details.status.is_ok(),
                    "execution failed: {:?}",
                    e.execution_details.status
                );
                println!("#   consumed {} CU", e.execution_details.executed_units);
                e.loaded_transaction.accounts
            }
            other => panic!("fees-only: {:?}", other.status()),
        })
        .collect()
}

fn amount_of(accounts: &[(Pubkey, AccountSharedData)], key: &Pubkey) -> u64 {
    let (_, a) = accounts.iter().find(|(k, _)| k == key).expect("account missing");
    u64::from_le_bytes(a.data()[64..72].try_into().unwrap())
}

fn lamports_of(accounts: &[(Pubkey, AccountSharedData)], key: &Pubkey) -> u64 {
    let (_, a) = accounts.iter().find(|(k, _)| k == key).expect("account missing");
    a.lamports()
}

/// Asserts the post-state of one transaction of each kind before any timing
/// runs: a native transfer that moves a lamport, and an SPL Token transfer that
/// moves a token. If either fails, the timings measure nothing, and this check
/// stops the run.
pub fn self_check() {
    println!("# self-check");

    let world = World::new();

    let payer = Keypair::new();
    let dest = Pubkey::new_unique();
    world.insert(payer.pubkey(), system_account(1_000_000_000));
    world.insert(dest, system_account(1_000_000));
    let ix = solana_system_interface::instruction::transfer(&payer.pubkey(), &dest, 1);
    let native = SanitizedTransaction::from_transaction_for_tests(Transaction::new_signed_with_payer(
        &[ix],
        Some(&payer.pubkey()),
        &[&payer],
        LAST_BLOCKHASH,
    ));

    println!("# system transfer:");
    let post = post_accounts(&world, std::slice::from_ref(&native));
    assert_eq!(
        lamports_of(&post[0], &dest),
        1_000_001,
        "system transfer did not move a lamport"
    );
    println!("#   dest lamports 1000000 -> {}", lamports_of(&post[0], &dest));

    let owner = Keypair::new();
    let mint = Pubkey::new_unique();
    let src = Pubkey::new_unique();
    let dst = Pubkey::new_unique();
    world.insert(owner.pubkey(), system_account(1_000_000_000));
    world.insert(src, token_account(&mint, &owner.pubkey(), 1_000_000));
    world.insert(dst, token_account(&mint, &owner.pubkey(), 0));

    let mut data = vec![3u8];
    data.extend_from_slice(&7u64.to_le_bytes());
    let ix = Instruction::new_with_bytes(
        SPL_TOKEN_ID,
        &data,
        vec![
            AccountMeta::new(src, false),
            AccountMeta::new(dst, false),
            AccountMeta::new_readonly(owner.pubkey(), true),
        ],
    );
    let spl = SanitizedTransaction::from_transaction_for_tests(Transaction::new_signed_with_payer(
        &[ix],
        Some(&owner.pubkey()),
        &[&owner],
        LAST_BLOCKHASH,
    ));

    println!("# spl-token transfer:");
    let post = post_accounts(&world, std::slice::from_ref(&spl));
    assert_eq!(amount_of(&post[0], &dst), 7, "spl transfer did not credit dst");
    assert_eq!(
        amount_of(&post[0], &src),
        1_000_000 - 7,
        "spl transfer did not debit src"
    );
    println!("#   dst amount 0 -> 7, src amount 1000000 -> 999993");

    // Pin the compute meter. The SPL transfer above reports a compute cost far
    // below the figure quoted for mainnet. To prove that the meter binds, the
    // same transaction must succeed at its reported cost and fail one unit
    // below it.
    let (_, units) = run_once(&world, &spl, u32::MAX).expect("uncapped run failed");
    let at_limit = run_once(&world, &spl, units as u32);
    let below_limit = run_once(&world, &spl, units as u32 - 1);
    assert!(at_limit.is_ok(), "failed at its own reported cost of {units} CU");
    assert!(
        below_limit.is_err(),
        "succeeded at {} CU, one below its reported cost: the meter is not binding",
        units - 1
    );
    println!(
        "#   compute meter pinned: ok at {} CU, {} at {} CU",
        units,
        below_limit.unwrap_err(),
        units - 1
    );
}

/// Runs the batch once and returns (successes, total CUs). It panics on any
/// failure, because a benchmark of failing transactions measures nothing. Each
/// grid calls it before it times or counts anything.
pub fn verify(world: &World, txs: &[SanitizedTransaction], checks: &[TransactionCheckResult]) -> (usize, u64) {
    let out = world.processor.load_and_execute_sanitized_transactions(
        &world.bank,
        txs,
        checks.to_vec(),
        &world.env,
        &world.config,
    );

    let mut ok = 0usize;
    let mut units = 0u64;
    for r in &out.processing_results {
        match r {
            Ok(processed) => {
                units = units.saturating_add(processed.executed_units());
                if processed.status().is_ok() {
                    ok += 1;
                } else {
                    panic!("transaction failed in bench setup: {:?}", processed.status());
                }
            }
            Err(e) => panic!("transaction not processed in bench setup: {e:?}"),
        }
    }
    (ok, units)
}
