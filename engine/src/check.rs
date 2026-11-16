//! Admission: a port of `Bank::verify_transaction_with_serialized_message`.
//! Pure, reentrant and handle-free, so `CheckTx` on the mempool connection
//! can run it while a block executes. `CheckTx` on New runs it in full.
//! `ProcessProposal` and `CheckTx` on Recheck run it hash-only, and
//! `ProcessProposal` then checks the signatures that it holds no result for.
//! Phase 1 runs it in full, or hash-only for bytes whose signatures already
//! passed on this node.

use crate::{cost, resolve, runtime};
use solana_account::AccountSharedData;
use solana_clock::Slot;
use solana_fee::{calculate_fee_details, FeeFeatures};
use solana_hash::Hash;
use solana_message::{
    v0::LoadedAddresses, SanitizedMessage, SimpleAddressLoader, VersionedMessage,
};
use solana_pubkey::Pubkey;
use solana_rent::Rent;
use solana_runtime_transaction::{runtime_transaction::RuntimeTransaction, transaction_meta::TransactionMeta};
use solana_svm::{account_loader::validate_fee_payer, transaction_error_metrics::TransactionErrorMetrics};
use solana_svm_transaction::svm_message::{SVMMessage, SVMStaticMessage};
use solana_transaction::{
    sanitized::{MessageHash, SanitizedTransaction, MAX_TX_ACCOUNT_LOCKS},
    versioned::{TransactionVersion, VersionedTransaction},
};
use solana_transaction_error::TransactionError;
use std::collections::HashSet;
use std::sync::atomic::{AtomicU64, Ordering};

/// Full verification checks signatures. Hash-only does not.
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    Full,
    HashOnly,
}

/// Transactions whose signatures admission verified. A test reads it to show
/// that recheck verifies none, and that phase 1 reuses what `CheckTx` New
/// verified.
pub static SIGNATURE_VERIFICATIONS: AtomicU64 = AtomicU64::new(0);

pub struct Admission {
    /// The cost, against placeholders for a message that names a table. That
    /// cost can only grow when the tables resolve, by the deploy term. So a
    /// block counts such a message at `resolved_admission`'s cost.
    pub cost: cost::Cost,
    /// Base plus priority, which is the number `getFeeForMessage` returns and
    /// the numerator of the ranking key. Agave's numerator is the validator's
    /// deposit.
    pub total_fee: u64,
    /// `recent_blockhash || message_hash`, the dedup key.
    pub dedup_key: [u8; 64],
    pub fee_payer: Pubkey,
    /// The account the first instruction advances when it is a durable nonce
    /// transaction (`SVMMessage::get_durable_nonce`, the block path's test),
    /// so an expired blockhash may still be a valid nonce.
    pub nonce_account: Option<Pubkey>,
    /// The lookup tables a v0 message names, in message order, which the
    /// state half reads.
    pub tables: Vec<Pubkey>,
    /// The accounts the transaction writes, which the per-account limit counts,
    /// when the message names no table. Empty when it does, because
    /// `resolved_admission` answers which accounts a table supplies.
    pub writable: Vec<Pubkey>,
}

/// What the state half of admission reads. Go reads each account from
/// committed state and passes it in, so admission itself reads no state.
pub struct Committed {
    pub rent: Rent,
    /// The fee payer, `None` when no account holds its address.
    pub payer: Option<AccountSharedData>,
    /// Set when the transaction's blockhash is past the window. It holds that
    /// blockhash and the nonce account it must be the durable nonce of.
    pub nonce: Option<(Hash, Option<AccountSharedData>)>,
    /// Set when the message names lookup tables. It holds the height of the
    /// block it is admitted for, and the tables and `SlotHashes` that block's
    /// phase 2 will resolve against.
    pub tables: Option<(Slot, resolve::Tables)>,
}

#[derive(Debug)]
pub enum CheckError {
    Transaction(TransactionError),
}

/// The state half's refusals, one per answer Go gives.
#[derive(Debug, PartialEq, Eq)]
pub enum CommittedError {
    /// A lookup table does not resolve, or the resolved message names an
    /// account twice. `check_tx` refuses such a message with the same error.
    Message(TransactionError),
    /// The blockhash is past the window and the transaction is not a durable
    /// nonce its nonce account holds and its authority signed.
    Nonce(TransactionError),
    /// The fee payer cannot pay.
    Payer(TransactionError),
}

impl From<TransactionError> for CheckError {
    fn from(e: TransactionError) -> Self {
        CheckError::Transaction(e)
    }
}

/// A v0 message's lookups resolved to placeholder addresses.
///
/// Admission and `getFeeForMessage` both need a sanitized message, and neither
/// may read state. Admission runs on the mempool connection beside an open
/// block, and `getFeeForMessage` on an RPC goroutine. Neither answer depends on
/// which addresses a table names. Agave's cost depends on how many of them are
/// writable, not which. It charges a write lock per writable slot, demoted or
/// not. A fee is signature counts and the priority fee. A program id never
/// comes from a table. `v0::Message::sanitize` enforces that so that "static
/// analysis on program instructions can be performed without loading on-chain
/// data from a bank".
///
/// So these placeholders stand in for resolution and do not approximate it,
/// with one exception. This chain's deploy term reads which addresses a table
/// supplies, because a table can supply the loader. That is why a block counts
/// such a message at `resolved_admission`'s cost. Phase 2 does the real
/// resolution. The state half of admission runs the same resolution against
/// the state the next block starts from.
///
/// Always `Enabled`, and empty for a message that names no table. `Disabled`
/// refuses every v0 message as UnsupportedVersion, with tables or not. That
/// includes the table-free v0 message that @solana/kit sends by default.
pub fn placeholder_loader(message: &VersionedMessage) -> SimpleAddressLoader {
    let mut loaded = LoadedAddresses::default();
    let mut next = 0u32;
    let mut placeholder = || {
        next += 1;
        let mut k = [0xffu8; 32];
        k[..4].copy_from_slice(&next.to_le_bytes());
        Pubkey::new_from_array(k)
    };
    for lookup in message.address_table_lookups().unwrap_or_default() {
        loaded.writable.extend(lookup.writable_indexes.iter().map(|_| placeholder()));
        loaded.readonly.extend(lookup.readonly_indexes.iter().map(|_| placeholder()));
    }
    SimpleAddressLoader::Enabled(loaded)
}

/// `validate_account_locks` (agave), ported and not linked, for the reason
/// `cost.rs` ports the cost model. The crate it lives in is a validator crate
/// and does not resolve against the pinned runtime. Only the order of the two
/// answers is written here. The order is upstream's, so a message that is both
/// over the limit and duplicated is `TooManyAccountLocks`. The duplicate half
/// is Anza's own `SanitizedMessage::has_duplicates`, which reads the static
/// and loaded keys together.
///
/// The count is static, so `check_tx` answers it against the placeholders.
/// Duplicates between a static key and a table's key are visible only after
/// the tables resolve. So the state half runs this again against the addresses
/// the next block's phase 2 will load. `runtime::execute_block` runs it once
/// more against the addresses it loaded.
pub fn validate_account_locks(message: &SanitizedMessage) -> Result<(), TransactionError> {
    if message.account_keys().len() > MAX_TX_ACCOUNT_LOCKS {
        Err(TransactionError::TooManyAccountLocks)
    } else if message.has_duplicates() {
        Err(TransactionError::AccountLoadedTwice)
    } else {
        Ok(())
    }
}

/// Whether ed25519 accepts every signature of `bytes`. It is the one check
/// that `Full` admission makes and `HashOnly` skips. `ProcessProposal` runs it
/// on a transaction that already passed `HashOnly` admission.
pub fn verify_signatures(bytes: &[u8]) -> bool {
    let Ok(tx) = wincode::deserialize_exact::<VersionedTransaction>(bytes) else {
        return false;
    };
    SIGNATURE_VERIFICATIONS.fetch_add(1, Ordering::Relaxed);
    tx.verify_and_hash_message().is_ok()
}

pub fn check_tx(root: &runtime::Root, bytes: &[u8], mode: Mode) -> Result<Admission, CheckError> {
    check_tx_keeping(root, bytes, mode).map(|(admission, _)| admission)
}

/// `check_tx`, keeping the sanitized transaction it built. The block's plan
/// phase holds it, to avoid a second parse of the same bytes. A message that
/// names no table is sanitized here exactly as phase 3 sanitizes it. A message
/// that names one is sanitized again in phase 3, over its resolved addresses.
pub fn check_tx_keeping(
    root: &runtime::Root,
    bytes: &[u8],
    mode: Mode,
) -> Result<(Admission, RuntimeTransaction<SanitizedTransaction>), CheckError> {
    let fs = &root.feature_set;
    // wincode, not bincode, because wincode is Agave's transaction wire
    // format. A V1 transaction has no other encoding, and legacy and v0 bytes
    // are the same under both.
    //
    // Exact, as Agave's leader parses a packet. `TransactionFrame::try_new`
    // refuses bytes past the transaction. Bytes after it would ride the pool
    // and the block at the price of the transaction before them.
    let tx: VersionedTransaction =
        wincode::deserialize_exact(bytes).map_err(|_| TransactionError::SanitizeFailure)?;

    // Discard v1 transactions until the feature gate is active.
    let enable_tx_v1 = fs.snapshot().enable_tx_v1;
    if !enable_tx_v1 && tx.version() == TransactionVersion::Number(1) {
        return Err(TransactionError::UnsupportedVersion.into());
    }
    let max_size = match tx.version() {
        TransactionVersion::Number(1) if enable_tx_v1 => solana_message::v1::MAX_TRANSACTION_SIZE,
        _ => 1232, // PACKET_DATA_SIZE, solana-packet-4.2.0
    };
    // Bound the bytes passed in, as `sanitize_transaction_size` bounds
    // `view.data().len()`.
    if bytes.len() > max_size {
        return Err(TransactionError::SanitizeFailure.into());
    }
    if tx.message.instructions().len() > solana_transaction_context::MAX_INSTRUCTION_TRACE_LENGTH {
        return Err(TransactionError::SanitizeFailure.into());
    }
    let message_hash = match mode {
        Mode::Full => {
            SIGNATURE_VERIFICATIONS.fetch_add(1, Ordering::Relaxed);
            tx.verify_and_hash_message()?
        }
        Mode::HashOnly => VersionedMessage::hash_raw_message(&tx.message.serialize()),
    };
    let mut dedup_key = [0u8; 64];
    dedup_key[..32].copy_from_slice(tx.message.recent_blockhash().as_ref());
    dedup_key[32..].copy_from_slice(message_hash.as_ref());

    let loader = placeholder_loader(&tx.message);
    let rtx = RuntimeTransaction::<SanitizedTransaction>::try_create(
        tx,
        MessageHash::Precomputed(message_hash),
        None,
        loader,
        &root.reserved,
    )?;
    validate_account_locks(rtx.message())?;
    // The block refuses a compute budget it cannot configure before anything
    // runs (`runtime::check`), so it is never processed and pays nothing.
    // Agave drops it at receive. Admitted, it would ride the block for
    // nothing, at a cost of zero.
    let config = rtx.transaction_configuration(fs)?;
    let total_fee =
        calculate_fee_details(&rtx, runtime::LAMPORTS_PER_SIGNATURE, config.priority_fee_lamports, FeeFeatures {})
            .total_fee();
    let cost = cost::cost(&rtx, fs);
    // Agave's tracker refuses a transaction that costs more than an account
    // may spend in a block, before it reads any account. Such a transaction
    // fits no block. Admitted, it would ride the pool until its blockhash
    // expired. Only a transaction that claims precompile signatures its bytes
    // cannot hold costs this much (`cost::MAX_TRANSACTION_UNITS`). Resolving a
    // table can only add to a cost, so this refuses nothing that would fit
    // after resolution. The state half holds a resolved message to the same
    // share.
    if cost.units > cost::MAX_WRITABLE_ACCOUNT_UNITS {
        return Err(TransactionError::WouldExceedMaxAccountCostLimit.into());
    }
    let tables: Vec<Pubkey> = rtx.message_address_table_lookups().map(|lookup| *lookup.account_key).collect();
    let admission = Admission {
        cost,
        total_fee,
        dedup_key,
        fee_payer: *rtx.fee_payer(),
        nonce_account: rtx.get_durable_nonce().copied(),
        writable: if tables.is_empty() { writable_keys(rtx.message()) } else { Vec::new() },
        tables,
    };
    Ok((admission, rtx))
}

/// The accounts a sanitized message writes, as Agave's cost tracker reads
/// them. That is every key that `is_writable` keeps after it demotes reserved
/// keys and invoked programs. The conflict groups read the same predicate
/// (`runtime::components`).
pub fn writable_keys(message: &SanitizedMessage) -> Vec<Pubkey> {
    message.account_keys().iter().enumerate().filter(|(i, _)| message.is_writable(*i)).map(|(_, k)| *k).collect()
}

/// The cost a block counts for a message that names lookup tables, and the
/// accounts it writes. The tables resolve against `tables` as phase 2 of the
/// block at that slot will resolve them.
pub struct Resolved {
    pub cost: cost::Cost,
    pub writable: Vec<Pubkey>,
}

/// Prices a message that names lookup tables as the block will, over the
/// resolved message, with demotion read against the resolved keys. It is
/// refused, as the state half refuses it, when the tables do not resolve or
/// the resolved message names an account twice. The block does not process
/// that transaction, and the limits do not count it.
pub fn resolved_admission(root: &runtime::Root, tx: &[u8], tables: &(Slot, resolve::Tables)) -> Result<Resolved, TransactionError> {
    let rtx = resolved(tx, Some(tables), &root.reserved)?;
    validate_account_locks(rtx.message())?;
    Ok(Resolved { cost: cost::cost(&rtx, &root.feature_set), writable: writable_keys(rtx.message()) })
}

/// The state half of admission. It makes the checks the block makes against
/// state before a transaction runs, here against the state the next block
/// starts from.
/// Agave's leader runs each check before it buffers a transaction, or records
/// only the transactions that pass. So a bid it ranks is one the block
/// processes. Here the pool ranks by the declared fee. Without these checks, a
/// transaction that passed admission and failed one of them would ride the
/// block unprocessed and unpaid, ahead of the transactions that pay. In the
/// block's order:
///
/// 1. The message's lookup tables resolve, as phase 2 resolves them
///    (`resolve::load_addresses`), and the resolved message names no account
///    twice.
/// 2. A blockhash past the window is the durable nonce its nonce account
///    holds, and the nonce authority signed the nonce instruction, as
///    solana-svm's `validate_transaction_nonce` requires.
/// 3. The fee payer passes solana-svm's own `validate_fee_payer` for `fee`.
///
/// Only a message that names a table or rides a nonce is parsed again. Every
/// other transaction needs only its payer, which `check_tx` returned.
pub fn check_committed(root: &runtime::Root, tx: &[u8], fee: u64, mut state: Committed) -> Result<(), CommittedError> {
    if state.tables.is_some() || state.nonce.is_some() {
        let rtx = resolved(tx, state.tables.as_ref(), &root.reserved).map_err(CommittedError::Message)?;
        validate_account_locks(rtx.message()).map_err(CommittedError::Message)?;
        // A table can add the deploy term, so a message that names one is held
        // to an account's share at its resolved cost, as `check_tx` holds
        // every message at the cost it can see.
        if state.tables.is_some() && cost::cost(&rtx, &root.feature_set).units > cost::MAX_WRITABLE_ACCOUNT_UNITS {
            return Err(CommittedError::Message(TransactionError::WouldExceedMaxAccountCostLimit));
        }
        if let Some((blockhash, account)) = &state.nonce {
            authorized_nonce(rtx.message(), blockhash, account.as_ref()).map_err(CommittedError::Nonce)?;
        }
    }
    let payer = state.payer.as_mut().ok_or(CommittedError::Payer(TransactionError::AccountNotFound))?;
    validate_fee_payer(
        payer,
        0,
        &mut TransactionErrorMetrics::default(),
        &state.rent,
        fee,
        root.feature_set.snapshot().relax_post_exec_min_balance_check,
    )
    .map_err(CommittedError::Payer)
}

/// The transaction `check_tx` admitted, with its lookup tables resolved
/// against `tables`, as the block sanitizes it (`runtime::execute_block`). A
/// message that names a table is refused when no table was read for it.
fn resolved(
    tx: &[u8],
    tables: Option<&(Slot, resolve::Tables)>,
    reserved: &HashSet<Pubkey>,
) -> Result<RuntimeTransaction<SanitizedTransaction>, TransactionError> {
    let tx: VersionedTransaction = wincode::deserialize_exact(tx).map_err(|_| TransactionError::SanitizeFailure)?;
    let lookups = tx.message.address_table_lookups().unwrap_or(&[]);
    let loaded = match tables {
        _ if lookups.is_empty() => LoadedAddresses::default(),
        Some((slot, tables)) => resolve::load_addresses(tables, *slot, lookups)?,
        None => return Err(TransactionError::AddressLookupTableNotFound),
    };
    RuntimeTransaction::<SanitizedTransaction>::try_create(
        tx,
        MessageHash::Compute,
        None,
        SimpleAddressLoader::Enabled(loaded),
        reserved,
    )
}

/// `validate_transaction_nonce` without the advance, which only the block
/// does. Agave's bank checks the first half before it executes. solana-svm
/// checks the second when it loads, and marks the transaction not processed.
fn authorized_nonce(message: &SanitizedMessage, blockhash: &Hash, account: Option<&AccountSharedData>) -> Result<(), TransactionError> {
    let nonce = account
        .and_then(|a| solana_nonce_account::verify_nonce_account(a, blockhash))
        .ok_or(TransactionError::BlockhashNotFound)?;
    if message.get_ix_signers(0).any(|signer| signer == &nonce.authority) {
        Ok(())
    } else {
        Err(TransactionError::BlockhashNotFound)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_account::WritableAccount;
    use solana_message::{compiled_instruction::CompiledInstruction, legacy, v0, v1, MessageHeader};
    use solana_sdk_ids::system_program;

    /// `enable_tx_v1`, SIMD-0385.
    const ENABLE_TX_V1: Pubkey =
        Pubkey::from_str_const("txv1aq4pp281K9um3tnPgkfX8UqtFT6wcVW3hNezGLL");

    fn legacy_tx(data_len: usize) -> Vec<u8> {
        wincode::serialize(&VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::Legacy(legacy::Message {
                header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 1 },
                account_keys: vec![Pubkey::new_unique(), Pubkey::new_unique()],
                recent_blockhash: Hash::default(),
                instructions: vec![CompiledInstruction::new_from_raw_parts(1, vec![7; data_len], vec![0])],
            }),
        })
        .unwrap()
    }

    fn v1_tx(data_len: usize) -> Vec<u8> {
        wincode::serialize(&VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::V1(v1::Message {
                header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 1 },
                config: Default::default(),
                lifetime_specifier: Hash::default(),
                account_keys: vec![Pubkey::new_unique(), Pubkey::new_unique()],
                instructions: vec![CompiledInstruction::new_from_raw_parts(1, vec![7; data_len], vec![0])],
            }),
        })
        .unwrap()
    }

    /// A legacy transaction whose static keys are `keys` then the System
    /// program, which is what it invokes.
    fn keyed_tx(keys: &[Pubkey]) -> Vec<u8> {
        let mut account_keys = keys.to_vec();
        account_keys.push(system_program::ID);
        let program = (account_keys.len() - 1) as u8;
        wincode::serialize(&VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::Legacy(legacy::Message {
                header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 1 },
                account_keys,
                recent_blockhash: Hash::default(),
                instructions: vec![CompiledInstruction::new_from_raw_parts(program, vec![7], vec![0])],
            }),
        })
        .unwrap()
    }

    /// The same, as a v0 message whose one lookup is `lookup`.
    fn v0_tx(keys: &[Pubkey], lookup: v0::MessageAddressTableLookup) -> Vec<u8> {
        let mut account_keys = keys.to_vec();
        account_keys.push(system_program::ID);
        let program = (account_keys.len() - 1) as u8;
        wincode::serialize(&VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::V0(v0::Message {
                header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 1 },
                account_keys,
                recent_blockhash: Hash::default(),
                instructions: vec![CompiledInstruction::new_from_raw_parts(program, vec![7], vec![0])],
                address_table_lookups: vec![lookup],
            }),
        })
        .unwrap()
    }

    /// One table supplying `loaded` readonly addresses, so the key count can
    /// pass 128 inside the packet limit.
    fn table_tx_with(keys: &[Pubkey], loaded: usize) -> Vec<u8> {
        v0_tx(
            keys,
            v0::MessageAddressTableLookup {
                account_key: Pubkey::new_unique(),
                writable_indexes: Vec::new(),
                readonly_indexes: (0..loaded as u8).collect(),
            },
        )
    }

    fn table_tx(loaded: usize) -> Vec<u8> {
        table_tx_with(&[Pubkey::new_unique()], loaded)
    }

    fn rejected(bytes: &[u8]) -> Option<TransactionError> {
        match check_tx(&runtime::root().unwrap(), bytes, Mode::HashOnly) {
            Ok(_) => None,
            Err(CheckError::Transaction(e)) => Some(e),
        }
    }

    /// The half that admission can answer. The count is static, so a
    /// transaction over 128 keys is refused on the mempool connection before
    /// it reaches a block. At 128 keys it is not refused.
    ///
    /// The keys come through a table, not static keys, because 128 static keys
    /// are 4,096 bytes and the packet limit is 1,232. Only a v0 transaction can
    /// reach the lock limit.
    #[test]
    fn a_transaction_naming_129_accounts_is_refused_at_admission() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        // Two static keys, so the table supplies the rest.
        assert_eq!(rejected(&table_tx(126)), None, "128 keys is the limit, not one under it");
        assert_eq!(rejected(&table_tx(127)), Some(TransactionError::TooManyAccountLocks));
        // Far over the limit.
        assert_eq!(rejected(&table_tx(250)), Some(TransactionError::TooManyAccountLocks));
    }

    /// The duplicate half against static keys alone, which is all `check_tx`
    /// can see. The static-against-table case needs the resolved addresses and
    /// is the state half's, below.
    #[test]
    fn a_duplicate_static_key_is_refused_at_admission() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let twice = Pubkey::new_unique();
        assert_eq!(
            rejected(&keyed_tx(&[twice, Pubkey::new_unique(), twice])),
            Some(TransactionError::AccountLoadedTwice)
        );
        assert_eq!(rejected(&keyed_tx(&[twice, Pubkey::new_unique(), Pubkey::new_unique()])), None);
    }

    /// Upstream's order. A message that is both over the limit and duplicated
    /// is `TooManyAccountLocks`. Both errors are consensus-visible, and a
    /// client that matches on them would otherwise see a different one here.
    #[test]
    fn the_limit_is_answered_before_the_duplicate() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let twice = Pubkey::new_unique();
        assert_eq!(rejected(&table_tx_with(&[twice, twice], 200)), Some(TransactionError::TooManyAccountLocks));
    }

    /// 1,232 bytes is the legacy and v0 limit.
    #[test]
    fn a_1233_byte_transaction_is_rejected() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        // Measured at a data length whose ShortU16 prefix is already two bytes.
        let base = legacy_tx(200).len() - 200;
        let at_limit = legacy_tx(1232 - base);
        assert_eq!(at_limit.len(), 1232);
        assert_eq!(rejected(&at_limit), None);
        let over = legacy_tx(1233 - base);
        assert_eq!(over.len(), 1233);
        assert_eq!(rejected(&over), Some(TransactionError::SanitizeFailure));
    }

    #[test]
    fn a_v1_transaction_is_rejected_while_enable_tx_v1_is_off() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        assert_eq!(rejected(&v1_tx(0)), Some(TransactionError::UnsupportedVersion));
        // The version is refused before the size is looked at, so a V1
        // transaction that would fit either limit is refused the same way.
        assert_eq!(rejected(&v1_tx(2000)), Some(TransactionError::UnsupportedVersion));
    }

    /// With the gate on, the V1 limit is 4,096 bytes, and every other version
    /// keeps 1,232.
    #[test]
    fn the_gate_raises_the_limit_for_v1_alone() {
        let _guard = runtime::config_lock();
        runtime::configure(&[(ENABLE_TX_V1, 1)]);

        let base = v1_tx(0).len();
        let at_limit = v1_tx(4096 - base);
        assert_eq!(at_limit.len(), 4096);
        assert_eq!(rejected(&at_limit), None);
        let over = v1_tx(4097 - base);
        assert_eq!(over.len(), 4097);
        assert_eq!(rejected(&over), Some(TransactionError::SanitizeFailure));

        let base = legacy_tx(200).len() - 200;
        assert_eq!(rejected(&legacy_tx(1232 - base)), None);
        assert_eq!(rejected(&legacy_tx(1233 - base)), Some(TransactionError::SanitizeFailure));

        runtime::configure(&[]);
    }

    /// A legacy transaction whose one instruction is the Compute Budget
    /// instruction `data`.
    fn budget_tx(data: Vec<u8>) -> Vec<u8> {
        wincode::serialize(&VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::Legacy(legacy::Message {
                header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 1 },
                account_keys: vec![Pubkey::new_unique(), solana_sdk_ids::compute_budget::ID],
                recent_blockhash: Hash::default(),
                instructions: vec![CompiledInstruction::new_from_raw_parts(1, data, vec![])],
            }),
        })
        .unwrap()
    }

    /// A compute budget the block's `check` cannot configure is refused at
    /// admission with the error the block refuses it with, so it never reaches
    /// the pool or a proposal. Otherwise it would be admitted at a fee and a
    /// cost of zero, and ride the block unprocessed. Both modes share every
    /// step after the hash, and `ProcessProposal` runs this one.
    #[test]
    fn a_compute_budget_the_block_cannot_configure_is_refused() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let heap = |bytes: u32| budget_tx([&[1u8][..], &bytes.to_le_bytes()].concat());
        let loaded = |bytes: u32| budget_tx([&[4u8][..], &bytes.to_le_bytes()].concat());
        assert_eq!(rejected(&heap(32 * 1024)), None);
        assert_eq!(
            rejected(&heap(33)),
            Some(TransactionError::InstructionError(0, solana_instruction_error::InstructionError::InvalidInstructionData))
        );
        assert_eq!(rejected(&loaded(64 * 1024)), None);
        assert_eq!(rejected(&loaded(0)), Some(TransactionError::InvalidLoadedAccountsDataSizeLimit));
    }

    fn system_account(lamports: u64) -> Option<AccountSharedData> {
        Some(AccountSharedData::new(lamports, 0, &system_program::ID))
    }

    /// An initialized nonce account holding `durable`, whose authority is
    /// `authority`. Its data is the version and state tags, the authority, the
    /// nonce, then the fee calculator.
    fn nonce_account(authority: &Pubkey, durable: &Hash) -> AccountSharedData {
        let mut account = AccountSharedData::new(Rent::default().minimum_balance(80), 80, &system_program::ID);
        let data = account.data_as_mut_slice();
        data[0..4].copy_from_slice(&1u32.to_le_bytes());
        data[4..8].copy_from_slice(&1u32.to_le_bytes());
        data[8..40].copy_from_slice(authority.as_ref());
        data[40..72].copy_from_slice(durable.as_ref());
        data[72..80].copy_from_slice(&5_000u64.to_le_bytes());
        account
    }

    /// A transaction from `signer` whose first instruction advances `nonce`,
    /// with `signer` as the instruction's authority, riding `blockhash`.
    fn nonce_tx(signer: Pubkey, nonce: Pubkey, blockhash: Hash) -> Vec<u8> {
        wincode::serialize(&VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::Legacy(legacy::Message {
                header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 2 },
                account_keys: vec![signer, nonce, solana_sdk_ids::sysvar::recent_blockhashes::ID, system_program::ID],
                recent_blockhash: blockhash,
                // SystemInstruction::AdvanceNonceAccount: nonce, RecentBlockhashes, authority.
                instructions: vec![CompiledInstruction::new_from_raw_parts(3, 4u32.to_le_bytes().to_vec(), vec![1, 2, 0])],
            }),
        })
        .unwrap()
    }

    /// The state half's input for a transaction whose blockhash is in the
    /// window and that names no table. Only the payer is set.
    fn state(payer: Option<AccountSharedData>) -> Committed {
        Committed { rent: Rent::default(), payer, nonce: None, tables: None }
    }

    /// The payer is checked by solana-svm's own `validate_fee_payer`, for the
    /// fee the transaction declares.
    #[test]
    fn a_payer_must_pay_the_fee_it_declares() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let min = Rent::default().minimum_balance(0);
        let tx = legacy_tx(0);
        let pays = |fee, payer| check_committed(&runtime::root().unwrap(), &tx, fee, state(payer));
        let refused = |e| Err(CommittedError::Payer(e));
        assert_eq!(pays(5_000, None), refused(TransactionError::AccountNotFound));
        assert_eq!(pays(5_000, system_account(4_999)), refused(TransactionError::InsufficientFundsForFee));
        assert_eq!(pays(5_000, system_account(5_000)), Ok(()), "a payer may pay down to zero");
        assert_eq!(pays(5_000, system_account(min + 5_000)), Ok(()));
        assert_eq!(
            pays(5_001, system_account(min + 5_000)),
            refused(TransactionError::InsufficientFundsForRent { account_index: 0 }),
            "a rent-exempt payer may not pay below its minimum"
        );
        let token = AccountSharedData::new(min + 5_000, 165, &Pubkey::new_unique());
        assert_eq!(pays(5_000, Some(token)), refused(TransactionError::InvalidAccountForFee));
    }

    /// A blockhash past the window is admitted only as the durable nonce its
    /// nonce account holds, and only when the nonce authority signed the nonce
    /// instruction, which is what solana-svm's `validate_transaction_nonce`
    /// requires before it processes one.
    #[test]
    fn an_expired_blockhash_must_be_a_nonce_its_authority_signed() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let (authority, account) = (Pubkey::new_unique(), Pubkey::new_unique());
        let durable = Hash::new_from_array([7; 32]);
        let held = Some(nonce_account(&authority, &durable));
        let rides = |signer, blockhash, held| {
            let nonce = Some((blockhash, held));
            check_committed(&runtime::root().unwrap(), &nonce_tx(signer, account, blockhash), 5_000, Committed { nonce, ..state(system_account(1_000_000_000)) })
        };
        let refused = Err(CommittedError::Nonce(TransactionError::BlockhashNotFound));
        assert_eq!(rides(authority, durable, held.clone()), Ok(()));
        assert_eq!(rides(authority, Hash::new_from_array([8; 32]), held.clone()), refused, "a hash the account does not hold");
        assert_eq!(rides(authority, durable, None), refused, "an account that does not exist");
        assert_eq!(rides(Pubkey::new_unique(), durable, held), refused, "a signer that is not the nonce authority");
    }

    /// A v0 message's tables resolve at admission as the next block's
    /// phase 2 will resolve them, and the resolved message names no account
    /// twice. Otherwise a table that does not exist, or a key named twice
    /// through one, would ride the block unprocessed and unpaid.
    #[test]
    fn a_message_must_resolve_and_name_each_account_once() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let (payer, table, stranger) = (Pubkey::new_unique(), Pubkey::new_unique(), Pubkey::new_unique());
        let holding = |address: Pubkey| {
            let data = solana_address_lookup_table_interface::state::AddressLookupTable {
                meta: Default::default(),
                addresses: std::borrow::Cow::Owned(vec![address]),
            }
            .serialize_for_tests()
            .unwrap();
            resolve::Tables::new(Default::default(), [(table.to_bytes(), solana_sdk_ids::address_lookup_table::ID, data)])
        };
        let naming = |table: Pubkey, index: u8| {
            v0_tx(&[payer], v0::MessageAddressTableLookup { account_key: table, writable_indexes: Vec::new(), readonly_indexes: vec![index] })
        };
        let resolves = |tx: Vec<u8>, tables: Option<resolve::Tables>| {
            check_committed(&runtime::root().unwrap(), &tx, 5_000, Committed { tables: tables.map(|t| (5, t)), ..state(system_account(1_000_000_000)) })
        };
        let refused = |e| Err(CommittedError::Message(e));
        assert_eq!(resolves(naming(table, 0), Some(holding(stranger))), Ok(()));
        assert_eq!(
            resolves(naming(table, 1), Some(holding(stranger))),
            refused(TransactionError::InvalidAddressLookupTableIndex),
            "an index past the table's end"
        );
        assert_eq!(
            resolves(naming(Pubkey::new_unique(), 0), Some(holding(stranger))),
            refused(TransactionError::AddressLookupTableNotFound),
            "a table that does not exist"
        );
        assert_eq!(
            resolves(naming(table, 0), Some(holding(payer))),
            refused(TransactionError::AccountLoadedTwice),
            "the payer named statically and through the table"
        );
    }

    /// A transaction that costs more than an account may spend in a block fits
    /// no block, and admission refuses it with the error Agave's tracker gives
    /// it. Only unbacked precompile claims cost that much
    /// (`cost::MAX_TRANSACTION_UNITS`). Fifteen instructions that claim 255
    /// secp256k1 signatures each cost that much, and one does not.
    #[test]
    fn a_transaction_over_the_account_limit_is_refused() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let claiming = |n: usize| {
            wincode::serialize(&VersionedTransaction {
                signatures: vec![Default::default()],
                message: VersionedMessage::Legacy(legacy::Message {
                    header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 1 },
                    account_keys: vec![Pubkey::new_unique(), solana_sdk_ids::secp256k1_program::ID],
                    recent_blockhash: Hash::default(),
                    instructions: vec![CompiledInstruction::new_from_raw_parts(1, vec![255], vec![]); n],
                }),
            })
            .unwrap()
        };
        assert_eq!(rejected(&claiming(1)), None);
        assert_eq!(rejected(&claiming(15)), Some(TransactionError::WouldExceedMaxAccountCostLimit));
    }

    /// The limit counts what the resolved message writes, after Agave's demotion.
    /// Admission answers a message that names no table. A message that names
    /// one is answered against the tables. An index can supply a writable
    /// account, a reserved key it supplies is demoted, and a key named twice is
    /// refused as the block refuses it.
    #[test]
    fn the_limit_counts_what_the_resolved_message_writes() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let (payer, hot, table) = (Pubkey::new_unique(), Pubkey::new_unique(), Pubkey::new_unique());
        let clock = solana_sdk_ids::sysvar::clock::ID;
        let admitted = |tx: &[u8]| check_tx(&runtime::root().unwrap(), tx, Mode::HashOnly).expect("admitted");

        // Every key keyed_tx names is in a writable slot but the System program.
        assert_eq!(admitted(&keyed_tx(&[payer, hot, clock])).writable, vec![payer, hot], "a sysvar is demoted");

        let lookup = v0::MessageAddressTableLookup { account_key: table, writable_indexes: vec![0, 1], readonly_indexes: Vec::new() };
        let tabled = v0_tx(&[payer], lookup);
        assert!(admitted(&tabled).writable.is_empty(), "which accounts a table supplies is not admission's to say");

        let holding = |addresses: Vec<Pubkey>| {
            let data = solana_address_lookup_table_interface::state::AddressLookupTable {
                meta: Default::default(),
                addresses: std::borrow::Cow::Owned(addresses),
            }
            .serialize_for_tests()
            .unwrap();
            (5, resolve::Tables::new(Default::default(), [(table.to_bytes(), solana_sdk_ids::address_lookup_table::ID, data)]))
        };
        let writes = |addresses| resolved_admission(&runtime::root().unwrap(), &tabled, &holding(addresses)).map(|r| r.writable);
        assert_eq!(writes(vec![hot, clock]), Ok(vec![payer, hot]));
        assert_eq!(writes(vec![hot, payer]), Err(TransactionError::AccountLoadedTwice));
    }

    /// The state half holds a message that names a table to an account's
    /// share at its resolved cost. Twelve secp256k1 instructions claiming 255
    /// signatures each keep the message under the share as admission sees it,
    /// and a table that supplies the loader a wrapper calls adds the 64 MiB
    /// deploy term and takes it over.
    #[test]
    fn a_resolved_cost_over_the_account_limit_is_refused() {
        let _guard = runtime::config_lock();
        runtime::configure(&[]);
        let (payer, wrapper, table) = (Pubkey::new_unique(), Pubkey::new_unique(), Pubkey::new_unique());
        let mut instructions = vec![CompiledInstruction::new_from_raw_parts(1, vec![255], vec![]); 12];
        instructions.push(CompiledInstruction::new_from_raw_parts(2, vec![], vec![3]));
        let tx = wincode::serialize(&VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::V0(v0::Message {
                header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 2 },
                account_keys: vec![payer, solana_sdk_ids::secp256k1_program::ID, wrapper],
                recent_blockhash: Hash::default(),
                instructions,
                address_table_lookups: vec![v0::MessageAddressTableLookup {
                    account_key: table,
                    writable_indexes: Vec::new(),
                    readonly_indexes: vec![0],
                }],
            }),
        })
        .unwrap();
        let placeholder = check_tx(&runtime::root().unwrap(), &tx, Mode::HashOnly).expect("admitted").cost.units;
        assert!(placeholder <= cost::MAX_WRITABLE_ACCOUNT_UNITS);
        let holding = |address: Pubkey| {
            let data = solana_address_lookup_table_interface::state::AddressLookupTable {
                meta: Default::default(),
                addresses: std::borrow::Cow::Owned(vec![address]),
            }
            .serialize_for_tests()
            .unwrap();
            resolve::Tables::new(Default::default(), [(table.to_bytes(), solana_sdk_ids::address_lookup_table::ID, data)])
        };
        let admits = |address| {
            check_committed(&runtime::root().unwrap(), &tx, 5_000, Committed { tables: Some((5, holding(address))), ..state(system_account(1_000_000_000)) })
        };
        assert_eq!(admits(Pubkey::new_unique()), Ok(()));
        assert_eq!(
            admits(solana_sdk_ids::bpf_loader_upgradeable::ID),
            Err(CommittedError::Message(TransactionError::WouldExceedMaxAccountCostLimit))
        );
        let resolved = resolved_admission(&runtime::root().unwrap(), &tx, &(5, holding(solana_sdk_ids::bpf_loader_upgradeable::ID))).expect("resolves");
        assert!(resolved.cost.units > cost::MAX_WRITABLE_ACCOUNT_UNITS && resolved.cost.units > placeholder);
    }
}
