//! Transaction cost, the unit the block limits count. It is
//! `solana-cost-model` 4.2.1's cost with four changes: the write-lock price,
//! and the allocation, deploy and name terms.
//!
//! The crate is ported, not linked, because it depends on
//! `solana-vote-program`, which does not resolve against the pinned runtime.
//! Each engine block limit is one constant here. Both proposal handlers and
//! block execution read them.

use agave_feature_set::FeatureSet;
use solana_compute_budget::compute_budget_limits::{
    DEFAULT_HEAP_COST, MAX_COMPUTE_UNIT_LIMIT, MAX_LOADED_ACCOUNTS_DATA_SIZE_BYTES,
};
use solana_pubkey::Pubkey;
use solana_runtime_transaction::transaction_meta::TransactionMeta;
use solana_sdk_ids::{bpf_loader_upgradeable, compute_budget, system_program};
use solana_svm_transaction::{
    instruction::SVMInstruction,
    svm_message::{SVMMessage, SVMStaticMessage},
};
use solana_system_interface::instruction::SystemInstruction;
use solana_transaction::sanitized::MAX_TX_ACCOUNT_LOCKS;
use solana_transaction_error::TransactionError;
use std::collections::HashMap;

/// Unscaled.
pub const MAX_BLOCK_UNITS: u64 = 60_000_000;
/// Solana's per-account limit at its own value. The transactions in one block
/// that write an account spend at most this much of the block between them,
/// which is 40 percent of `MAX_BLOCK_UNITS`.
pub const MAX_WRITABLE_ACCOUNT_UNITS: u64 = 24_000_000;
/// Solana's value is 300. This value comes from measured commitment at the 20
/// GB published minimum, on an aged database, with the phase-2 read and the
/// sidecar writes beside the shard commit. The worst block is the write shape
/// beside nine transactions that load 512 MiB. At 1,555 it took 7.69 s, so a
/// writable account has to cost 1,858 units with its name. 1,755 and the
/// name's 245 make 2,000, about 7 percent over that. At 1,755 the same block
/// takes 6.87 and 6.92 s. A writable account pays for the commit it causes,
/// and for what that commit costs the block's other reads.
pub const WRITE_LOCK_UNITS: u64 = 1_755;
/// Solana's value, guarding latency.
pub const MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA: u64 = 100_000_000;
/// Allocation costs `allocated_bytes / 2` units. Two is the 100 MB byte-limit
/// block at the published minimum. It makes the byte limit and the unit limit
/// bind at the same place.
pub const ALLOCATED_BYTES_PER_UNIT: u64 = 2;
/// A deploy-capable transaction costs `deploy_bytes / 11` units.
pub const DEPLOY_BYTES_PER_UNIT: u64 = 11;
/// The smallest measured program, spl_token, floors `deploy_bytes`.
pub const DEPLOY_BYTES_FLOOR: u64 = 108_600;
/// Every account a transaction names costs one store lookup, whether the
/// account exists or not. Solana has no such term, because its index is an
/// in-memory hash map and this chain's is a point read through IAVL, done
/// eagerly for every name.
///
/// A name has no single price. On an aged database at the 20 GB minimum it
/// costs about 8 us in a block that only reads and 30.25 us inside the write
/// block. The price is set against the second, `ceil(8.0537 * (30.25 +
/// 0.135))`. The 0.135 us is the engine's own arena decode of a key. The write
/// lock pays for the rest of the worst block.
pub const NAME_UNITS: u64 = 245;

// Unchanged from solana-cost-model-4.2.1
const COMPUTE_UNIT_TO_US_RATIO: u64 = 30;
const SIGNATURE_COST: u64 = COMPUTE_UNIT_TO_US_RATIO * 24;
const SECP256K1_VERIFY_COST: u64 = COMPUTE_UNIT_TO_US_RATIO * 223;
const ED25519_VERIFY_STRICT_COST: u64 = COMPUTE_UNIT_TO_US_RATIO * 80;
const SECP256R1_VERIFY_COST: u64 = COMPUTE_UNIT_TO_US_RATIO * 160;
const INSTRUCTION_DATA_BYTES_COST: u64 = 140 / COMPUTE_UNIT_TO_US_RATIO;
const ACCOUNT_DATA_COST_PAGE_SIZE: u64 = 32 * 1024;

// The four terms that turn bytes into units. `MAX_TRANSACTION_UNITS` and
// `cost` both call them, so the bound cannot go stale when a term changes.

/// Agave's `get_instructions_data_cost`: a unit per
/// `INSTRUCTION_DATA_BYTES_COST` bytes, rounded down.
const fn data_bytes_units(bytes: u64) -> u64 {
    bytes / INSTRUCTION_DATA_BYTES_COST
}

/// The allocation term: a unit per `ALLOCATED_BYTES_PER_UNIT` bytes, rounded
/// down.
const fn allocation_units(bytes: u64) -> u64 {
    bytes / ALLOCATED_BYTES_PER_UNIT
}

/// Agave's `calculate_loaded_accounts_data_size_cost`: the limit in whole
/// pages, each at the heap cost.
const fn loaded_accounts_units(limit: u64) -> u64 {
    limit.div_ceil(ACCOUNT_DATA_COST_PAGE_SIZE) * DEFAULT_HEAP_COST
}

/// The deploy term: a unit per `DEPLOY_BYTES_PER_UNIT` bytes, rounded down.
const fn deploy_units(bytes: u64) -> u64 {
    bytes / DEPLOY_BYTES_PER_UNIT
}

/// The most a transaction that can succeed costs, each term at the largest
/// value the runtime lets it reach. The bytes are a V1 transaction's 4,096,
/// above the 1,232 of legacy and v0, so the bound holds whether or not V1
/// is on.
///
/// Signatures are 64 bytes each. A precompile signature is charged for the
/// count its instruction claims in its first byte. But the precompile refuses
/// an instruction shorter than its claims: 11 bytes each for secp256k1, and 14
/// for ed25519 and secp256r1. So secp256k1's price per byte bounds all three.
/// Names are the static keys, the table indexes and the tables. The lock limit
/// caps the first two together, and every table loads at least one account.
///
/// A transaction can cost more only by claiming precompile signatures its own
/// bytes cannot hold. It fails wherever it runs, and Agave's tracker refuses
/// it from every block too.
pub const MAX_TRANSACTION_UNITS: u64 = {
    const BYTES: u64 = solana_message::v1::MAX_TRANSACTION_SIZE as u64;
    const LOCKS: u64 = MAX_TX_ACCOUNT_LOCKS as u64;
    const LOADED_BYTES: u64 = MAX_LOADED_ACCOUNTS_DATA_SIZE_BYTES.get() as u64;
    BYTES / 64 * SIGNATURE_COST
        + BYTES / 11 * SECP256K1_VERIFY_COST
        + LOCKS * WRITE_LOCK_UNITS
        + 2 * LOCKS * NAME_UNITS
        + data_bytes_units(BYTES)
        + allocation_units(
            solana_system_interface::MAX_PERMITTED_ACCOUNTS_DATA_ALLOCATIONS_PER_TRANSACTION as u64,
        )
        + MAX_COMPUTE_UNIT_LIMIT as u64
        + loaded_accounts_units(LOADED_BYTES)
        + deploy_units(LOADED_BYTES)
};
const _: () = assert!(MAX_TRANSACTION_UNITS <= MAX_WRITABLE_ACCOUNT_UNITS);

/// A transaction's cost: the units it spends of `MAX_BLOCK_UNITS` and the
/// bytes it spends of `MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Cost {
    pub units: u64,
    pub allocated_bytes: u64,
}

/// `CostModel::calculate_cost` with the four terms this chain changed or added.
///
/// Each of Agave's terms reads the message alone. A write lock is charged per
/// writable slot, whether or not the key in it is demoted, so a message costs
/// the same before and after its lookup tables resolve. The deploy term is the
/// one that reads the addresses a table supplies (`deploy_capable`). So the
/// cost a block counts for a message that names a table is this function over
/// the resolved message.
pub fn cost<Tx: TransactionMeta + SVMMessage>(tx: &Tx, fs: &FeatureSet) -> Cost {
    let details = tx.signature_details();
    let signature_cost = details
        .num_transaction_signatures()
        .saturating_mul(SIGNATURE_COST)
        .saturating_add(details.num_secp256k1_instruction_signatures().saturating_mul(SECP256K1_VERIFY_COST))
        .saturating_add(details.num_ed25519_instruction_signatures().saturating_mul(ED25519_VERIFY_STRICT_COST))
        .saturating_add(details.num_secp256r1_instruction_signatures().saturating_mul(SECP256R1_VERIFY_COST));
    let write_lock_cost = WRITE_LOCK_UNITS.saturating_mul(tx.num_write_locks());
    let data_bytes_cost = data_bytes_units(u64::from(tx.instruction_data_len()));
    // A transaction whose compute budget does not parse never executes, so it
    // costs nothing to execute.
    let (execution_cost, loaded_accounts_limit) = match tx.transaction_configuration(fs) {
        Ok(config) => (u64::from(config.compute_unit_limit), Some(config.loaded_accounts_data_size_limit)),
        Err(_) => (0, None),
    };
    let loaded_accounts_cost = loaded_accounts_limit
        .map(|bytes| loaded_accounts_units(u64::from(bytes)))
        .unwrap_or(0);
    let name_cost = NAME_UNITS.saturating_mul(num_names(tx));
    let allocated_bytes = allocated_bytes(tx, fs);
    let deploy_bytes = match loaded_accounts_limit {
        Some(limit) if deploy_capable(tx) => u64::from(limit).max(DEPLOY_BYTES_FLOOR),
        _ => 0,
    };
    Cost {
        units: signature_cost
            .saturating_add(write_lock_cost)
            .saturating_add(data_bytes_cost)
            .saturating_add(allocation_units(allocated_bytes))
            .saturating_add(execution_cost)
            .saturating_add(loaded_accounts_cost)
            .saturating_add(deploy_units(deploy_bytes))
            .saturating_add(name_cost),
        allocated_bytes,
    }
}

/// Every key phase 1 and phase 2 will put in this transaction's share of the
/// load list: the static keys, each lookup table's own address, and one per
/// index into it.
///
/// It is read from the message without resolving anything. So both proposal
/// handlers and admission charge it as they charge the other seven terms, and
/// none of them may read state (`check::placeholder_loader`).
fn num_names<Tx: SVMStaticMessage>(tx: &Tx) -> u64 {
    tx.message_address_table_lookups().fold(tx.static_account_keys().len() as u64, |n, lookup| {
        n.saturating_add(1)
            .saturating_add(lookup.writable_indexes.len() as u64)
            .saturating_add(lookup.readonly_indexes.len() as u64)
    })
}

/// `calculate_allocated_accounts_data_size`: the space System Program
/// instructions allocate, zero if any of them statically fails, clamped to
/// the runtime's per-transaction maximum.
fn allocated_bytes<Tx: SVMStaticMessage>(tx: &Tx, fs: &FeatureSet) -> u64 {
    let mut total = 0u64;
    for (program, ix) in tx.program_instructions_iter() {
        if program != &system_program::ID {
            continue;
        }
        let Ok(instruction) = solana_bincode::limited_deserialize::<SystemInstruction>(ix.data, 1232) else {
            return 0;
        };
        let space = match instruction {
            SystemInstruction::CreateAccount { space, .. }
            | SystemInstruction::CreateAccountWithSeed { space, .. }
            | SystemInstruction::Allocate { space }
            | SystemInstruction::AllocateWithSeed { space, .. } => space,
            SystemInstruction::CreateAccountAllowPrefund { space, .. } => {
                if !fs.snapshot().create_account_allow_prefund {
                    return 0;
                }
                space
            }
            SystemInstruction::Assign { .. }
            | SystemInstruction::Transfer { .. }
            | SystemInstruction::AdvanceNonceAccount
            | SystemInstruction::WithdrawNonceAccount(..)
            | SystemInstruction::InitializeNonceAccount(..)
            | SystemInstruction::AuthorizeNonceAccount(..)
            | SystemInstruction::UpgradeNonceAccount
            | SystemInstruction::AssignWithSeed { .. }
            | SystemInstruction::TransferWithSeed { .. } => 0,
        };
        if space > solana_system_interface::MAX_PERMITTED_DATA_LENGTH {
            return 0;
        }
        total = total.saturating_add(space);
    }
    total.min(solana_system_interface::MAX_PERMITTED_ACCOUNTS_DATA_ALLOCATIONS_PER_TRANSACTION as u64)
}

/// The upgradeable loader is among the account keys, and some top-level
/// instruction is neither System, Compute Budget, nor a loader instruction
/// that never compiles an ELF (InitializeBuffer 0, Write 1, SetAuthority 4,
/// Close 5, SetAuthorityChecked 7).
///
/// The account keys include the addresses a lookup table supplies. A CPI
/// callee may be any transaction account, so a table can carry the loader
/// a wrapper deploys through. The static keys alone would miss it.
fn deploy_capable<Tx: SVMMessage>(tx: &Tx) -> bool {
    if !tx.account_keys().iter().any(|key| key == &bpf_loader_upgradeable::ID) {
        return false;
    }
    !tx.program_instructions_iter().all(|(program, ix): (&Pubkey, SVMInstruction)| {
        program == &system_program::ID
            || program == &compute_budget::ID
            || (program == &bpf_loader_upgradeable::ID
                && ix.data.len() >= 4
                && matches!(u32::from_le_bytes(ix.data[..4].try_into().unwrap()), 0 | 1 | 4 | 5 | 7))
    })
}

/// Agave's `CostTracker::try_add` over this chain's three block limits, in
/// Agave's order: the block's units, the transaction alone against an
/// account's share, the block's allocation, then each account it writes.
/// A transaction that does not fit adds nothing, and the error is Agave's
/// for the limit it would pass.
///
/// FinalizeBlock keeps one for the block (`runtime::execute_block`), and
/// both proposal handlers keep the same counts in `x/svm/proposal.go`.
#[derive(Default)]
pub struct CostTracker {
    units: u64,
    allocated: u64,
    accounts: HashMap<Pubkey, u64>,
}

impl CostTracker {
    pub fn try_add<'a>(
        &mut self,
        cost: &Cost,
        writable: impl IntoIterator<Item = &'a Pubkey>,
    ) -> Result<(), TransactionError> {
        if cost.units > MAX_BLOCK_UNITS - self.units {
            return Err(TransactionError::WouldExceedMaxBlockCostLimit);
        }
        if cost.units > MAX_WRITABLE_ACCOUNT_UNITS {
            return Err(TransactionError::WouldExceedMaxAccountCostLimit);
        }
        if cost.allocated_bytes > MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA - self.allocated {
            return Err(TransactionError::WouldExceedAccountDataBlockLimit);
        }
        let mut applied = Vec::new();
        for key in writable {
            let spent = self.accounts.entry(*key).or_default();
            if *spent > MAX_WRITABLE_ACCOUNT_UNITS - cost.units {
                for key in applied {
                    *self.accounts.get_mut(key).unwrap() -= cost.units;
                }
                return Err(TransactionError::WouldExceedMaxAccountCostLimit);
            }
            *spent += cost.units;
            applied.push(key);
        }
        self.units += cost.units;
        self.allocated += cost.allocated_bytes;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_hash::Hash;
    use solana_message::{
        compiled_instruction::CompiledInstruction,
        legacy::Message,
        v0::{Message as V0Message, MessageAddressTableLookup},
        MessageHeader, SimpleAddressLoader, VersionedMessage,
    };
    use solana_runtime_transaction::runtime_transaction::RuntimeTransaction;
    use solana_transaction::{
        sanitized::{MessageHash, SanitizedTransaction},
        versioned::VersionedTransaction,
    };
    use std::collections::HashSet;

    /// One payer, `writable` further writable keys, then the `readonly` keys,
    /// and one instruction.
    fn units(writable: usize, readonly: &[Pubkey], ix: CompiledInstruction) -> Cost {
        let mut keys = vec![Pubkey::new_unique()];
        keys.extend((0..writable).map(|_| Pubkey::new_unique()));
        keys.extend_from_slice(readonly);
        let tx = VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::Legacy(Message {
                header: MessageHeader {
                    num_required_signatures: 1,
                    num_readonly_signed_accounts: 0,
                    num_readonly_unsigned_accounts: readonly.len() as u8,
                },
                account_keys: keys,
                recent_blockhash: Hash::default(),
                instructions: vec![ix],
            }),
        };
        let rtx = RuntimeTransaction::<SanitizedTransaction>::try_create(
            tx,
            MessageHash::Compute,
            None,
            SimpleAddressLoader::Disabled,
            &HashSet::new(),
        )
        .unwrap();
        cost(&rtx, &FeatureSet::default())
    }

    fn system(writable: usize, data: Vec<u8>) -> Cost {
        units(writable, &[system_program::ID], CompiledInstruction::new_from_raw_parts(writable as u8 + 1, data, vec![0, 1]))
    }

    fn transfer() -> Vec<u8> {
        [2u32.to_le_bytes().as_slice(), &1u64.to_le_bytes()].concat()
    }

    /// A v0 message with three writable static keys, the System program as
    /// the one readonly key, and the given lookups resolved the way admission
    /// resolves them, to placeholders, because the count is all a cost
    /// depends on.
    fn v0(lookups: &[MessageAddressTableLookup]) -> Cost {
        let mut keys: Vec<Pubkey> = (0..3).map(|_| Pubkey::new_unique()).collect();
        keys.push(system_program::ID);
        let message = VersionedMessage::V0(V0Message {
            header: MessageHeader {
                num_required_signatures: 1,
                num_readonly_signed_accounts: 0,
                num_readonly_unsigned_accounts: 1,
            },
            account_keys: keys,
            recent_blockhash: Hash::default(),
            instructions: vec![CompiledInstruction::new_from_raw_parts(3, transfer(), vec![0, 1])],
            address_table_lookups: lookups.to_vec(),
        });
        let loader = crate::check::placeholder_loader(&message);
        let rtx = RuntimeTransaction::<SanitizedTransaction>::try_create(
            VersionedTransaction { signatures: vec![Default::default()], message },
            MessageHash::Compute,
            None,
            loader,
            &HashSet::new(),
        )
        .unwrap();
        cost(&rtx, &FeatureSet::default())
    }

    /// The marginal writable account costs both terms, because eighteen more
    /// writable keys are also eighteen more names.
    #[test]
    fn a_writable_account_costs_a_write_lock_and_a_name() {
        assert_eq!(
            system(19, transfer()).units - system(1, transfer()).units,
            18 * (WRITE_LOCK_UNITS + NAME_UNITS)
        );
    }

    /// Every key counts once as a name: static keys whether written or not, the
    /// lookup table's own address, and one per index into it. None of the
    /// named accounts exist, on purpose. Nothing here reads state, and a name
    /// that finds nothing is charged the price of one that does.
    #[test]
    fn a_named_account_costs_a_name_whether_it_exists_or_not() {
        // Two static keys plus one readonly program, one instruction, no
        // tables: three names.
        let three = system(1, transfer());
        // The same transaction with two more readonly keys nothing invokes.
        let five = units(
            1,
            &[system_program::ID, Pubkey::new_unique(), Pubkey::new_unique()],
            CompiledInstruction::new_from_raw_parts(2, transfer(), vec![0, 1]),
        );
        assert_eq!(five.units - three.units, 2 * NAME_UNITS, "a readonly name that is never touched still pays");

        // A table index is a name whether it is writable or readonly. Four
        // static keys plus one table plus four indexes is nine names against
        // four plus one plus one, which is six, and two more write locks.
        let lookup = |writable: Vec<u8>, readonly: Vec<u8>| MessageAddressTableLookup {
            account_key: Pubkey::new_unique(),
            writable_indexes: writable,
            readonly_indexes: readonly,
        };
        let four = v0(&[lookup(vec![0, 1, 2], vec![7])]);
        let one = v0(&[lookup(vec![0], vec![])]);
        assert_eq!(four.units - one.units, 3 * NAME_UNITS + 2 * WRITE_LOCK_UNITS);

        // The table's own address is a name too, because resolution reads it.
        // Both of these name four addresses and lock nothing extra. The only
        // difference is that one of them reads two tables to do it.
        let two_tables = v0(&[lookup(vec![], vec![0, 1]), lookup(vec![], vec![2, 3])]);
        let one_table = v0(&[lookup(vec![], vec![0, 1, 2, 3])]);
        assert_eq!(two_tables.units - one_table.units, NAME_UNITS);
    }

    #[test]
    fn instruction_data_costs_a_unit_per_four_bytes_rounded_down() {
        assert_eq!(data_bytes_units(15), 3);
        assert_eq!(data_bytes_units(16), 4);
    }

    #[test]
    fn allocation_rounds_down() {
        assert_eq!(allocation_units(3 * ALLOCATED_BYTES_PER_UNIT - 1), 2);
        assert_eq!(allocation_units(3 * ALLOCATED_BYTES_PER_UNIT), 3);
    }

    /// A page begun is a whole page.
    #[test]
    fn the_loaded_limit_costs_eight_units_a_32_kib_page() {
        assert_eq!(loaded_accounts_units(0), 0);
        assert_eq!(loaded_accounts_units(1), 8);
        assert_eq!(loaded_accounts_units(32 * 1024 + 1), 16);
        assert_eq!(loaded_accounts_units(64 * 1024 * 1024), 16_384);
    }

    #[test]
    fn deploy_rounds_down() {
        assert_eq!(deploy_units(3 * DEPLOY_BYTES_PER_UNIT - 1), 2);
        assert_eq!(deploy_units(3 * DEPLOY_BYTES_PER_UNIT), 3);
    }

    #[test]
    fn allocation_costs_a_unit_per_allocated_bytes_per_unit() {
        let create = |space: u64| {
            let mut d = 0u32.to_le_bytes().to_vec();
            d.extend_from_slice(&1u64.to_le_bytes());
            d.extend_from_slice(&space.to_le_bytes());
            d.extend_from_slice(Pubkey::new_unique().as_ref());
            system(1, d)
        };
        let space = 6_000_000;
        let big = create(space);
        assert_eq!(big.allocated_bytes, space);
        assert_eq!(big.units - create(0).units, space / ALLOCATED_BYTES_PER_UNIT);
    }

    /// No limit is declared, so the loaded-data limit is the 64 MiB default.
    #[test]
    fn deploy_capable_transactions_pay_for_declared_bytes() {
        const DEPLOY_UNITS: u64 = 67_108_864 / 11;
        let loader = |tag: u32| {
            units(1, &[bpf_loader_upgradeable::ID], CompiledInstruction::new_from_raw_parts(2, [tag.to_le_bytes(), [0; 4]].concat(), vec![0, 1]))
        };
        // DeployWithMaxDataLen pays. Write does not.
        assert_eq!(loader(2).units - loader(1).units, DEPLOY_UNITS);
        // A wrapper program that carries the loader, as a CPI into Upgrade must,
        // pays. The same wrapper carrying another readonly key does not.
        let wrapper = Pubkey::new_unique();
        let wrapped = |carried: Pubkey| {
            units(1, &[carried, wrapper], CompiledInstruction::new_from_raw_parts(3, vec![3, 0, 0, 0], vec![0, 1, 2]))
        };
        assert_eq!(wrapped(bpf_loader_upgradeable::ID).units - wrapped(Pubkey::new_unique()).units, DEPLOY_UNITS);
    }

    /// Agave's tracker at the boundaries of its own tests: exactly a limit
    /// fits and a unit more does not, each account has a share of its own,
    /// a transaction refused on one account adds nothing to the accounts
    /// before it, and each limit refuses with Agave's error for it.
    #[test]
    fn the_tracker_is_agaves_try_add() {
        use TransactionError::*;
        let units = |units: u64| Cost { units, allocated_bytes: 0 };
        let (hot, warm, cold) = (Pubkey::new_unique(), Pubkey::new_unique(), Pubkey::new_unique());
        let mut spent = CostTracker::default();
        assert_eq!(spent.try_add(&units(MAX_WRITABLE_ACCOUNT_UNITS - 1), [&hot]), Ok(()));
        assert_eq!(spent.try_add(&units(1), [&hot]), Ok(()), "exactly the share fits");
        assert_eq!(spent.try_add(&units(1), [&hot]), Err(WouldExceedMaxAccountCostLimit), "a unit over does not");
        assert_eq!(spent.try_add(&units(MAX_WRITABLE_ACCOUNT_UNITS), [&warm]), Ok(()), "a second account has its own");
        assert_eq!(spent.try_add(&units(10), [&cold, &hot]), Err(WouldExceedMaxAccountCostLimit));
        assert_eq!(spent.try_add(&units(MAX_BLOCK_UNITS - 2 * MAX_WRITABLE_ACCOUNT_UNITS), [&cold]), Ok(()),
            "the refused transaction left the fresh account untouched, and the block is now full");
        assert_eq!(spent.try_add(&units(1), [&Pubkey::new_unique()]), Err(WouldExceedMaxBlockCostLimit));

        let mut alone = CostTracker::default();
        assert_eq!(alone.try_add(&units(MAX_WRITABLE_ACCOUNT_UNITS + 1), [&hot]), Err(WouldExceedMaxAccountCostLimit),
            "a transaction over the share on its own fits no account");
        let bytes = |allocated_bytes: u64| Cost { units: 1, allocated_bytes };
        assert_eq!(alone.try_add(&bytes(MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA), [&hot]), Ok(()));
        assert_eq!(alone.try_add(&bytes(1), [&cold]), Err(WouldExceedAccountDataBlockLimit));
    }

    /// A v0 message from one signer that invokes a wrapper program with one
    /// account its table supplies writable and one readonly. It is priced
    /// with the table holding `loaded`, writable then readonly, or against
    /// placeholders when `loaded` is `None`, as admission prices it.
    fn through_a_table(loaded: Option<(Pubkey, Pubkey)>) -> Cost {
        let wrapper = Pubkey::new_unique();
        let message = VersionedMessage::V0(V0Message {
            header: MessageHeader {
                num_required_signatures: 1,
                num_readonly_signed_accounts: 0,
                num_readonly_unsigned_accounts: 1,
            },
            account_keys: vec![Pubkey::new_unique(), wrapper],
            recent_blockhash: Hash::default(),
            instructions: vec![CompiledInstruction::new_from_raw_parts(1, vec![], vec![2, 3])],
            address_table_lookups: vec![MessageAddressTableLookup {
                account_key: Pubkey::new_unique(),
                writable_indexes: vec![0],
                readonly_indexes: vec![1],
            }],
        });
        let loader = match loaded {
            None => crate::check::placeholder_loader(&message),
            Some((writable, readonly)) => SimpleAddressLoader::Enabled(solana_message::v0::LoadedAddresses {
                writable: vec![writable],
                readonly: vec![readonly],
            }),
        };
        let reserved = HashSet::from([solana_sdk_ids::sysvar::clock::ID, bpf_loader_upgradeable::ID]);
        let rtx = RuntimeTransaction::<SanitizedTransaction>::try_create(
            VersionedTransaction { signatures: vec![Default::default()], message },
            MessageHash::Compute,
            None,
            loader,
            &reserved,
        )
        .unwrap();
        cost(&rtx, &FeatureSet::default())
    }

    /// Agave's terms read the message alone. A table that supplies a sysvar
    /// into a writable slot is still charged that slot's write lock, demoted
    /// or not, so it costs what placeholders cost. The deploy term is
    /// the one that reads what a table supplies, and a table that supplies the
    /// upgradeable loader makes the wrapper's call deploy-capable at the 64 MiB
    /// default it declares.
    #[test]
    fn only_the_deploy_term_reads_what_a_table_supplies() {
        let placeholders = through_a_table(None);
        let stranger = Pubkey::new_unique();
        assert_eq!(through_a_table(Some((solana_sdk_ids::sysvar::clock::ID, stranger))), placeholders);
        let deploy = MAX_LOADED_ACCOUNTS_DATA_SIZE_BYTES.get() as u64 / DEPLOY_BYTES_PER_UNIT;
        let loader = through_a_table(Some((stranger, bpf_loader_upgradeable::ID)));
        assert_eq!(loader.units, placeholders.units + deploy);
    }

    /// A legacy transaction from one signer whose further keys are `writable`
    /// and then `readonly`, carrying `ixs`.
    fn priced(writable: &[Pubkey], readonly: &[Pubkey], ixs: Vec<CompiledInstruction>) -> Cost {
        let mut keys = vec![Pubkey::new_unique()];
        keys.extend_from_slice(writable);
        keys.extend_from_slice(readonly);
        let tx = VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::Legacy(Message {
                header: MessageHeader {
                    num_required_signatures: 1,
                    num_readonly_signed_accounts: 0,
                    num_readonly_unsigned_accounts: readonly.len() as u8,
                },
                account_keys: keys,
                recent_blockhash: Hash::default(),
                instructions: ixs,
            }),
        };
        let rtx = RuntimeTransaction::<SanitizedTransaction>::try_create(
            tx,
            MessageHash::Compute,
            None,
            SimpleAddressLoader::Disabled,
            &HashSet::new(),
        )
        .unwrap();
        cost(&rtx, &FeatureSet::default())
    }

    /// One transaction with the five largest terms at their maxima costs no
    /// more than `MAX_TRANSACTION_UNITS`: 1,400,000 declared units, a deploy
    /// declaring 64 MiB of loaded data, and two 10 MiB allocations. The const
    /// assertion holds `MAX_TRANSACTION_UNITS` under the per-account limit. The
    /// only way past the limit is to claim precompile signatures the bytes do
    /// not carry.
    #[test]
    fn no_transaction_that_can_succeed_costs_more_than_an_account_may_spend() {
        let (a, b) = (Pubkey::new_unique(), Pubkey::new_unique());
        let create = |to: u8| {
            let mut d = 0u32.to_le_bytes().to_vec();
            d.extend_from_slice(&1u64.to_le_bytes());
            d.extend_from_slice(&solana_system_interface::MAX_PERMITTED_DATA_LENGTH.to_le_bytes());
            d.extend_from_slice(Pubkey::new_unique().as_ref());
            CompiledInstruction::new_from_raw_parts(3, d, vec![0, to])
        };
        let budget = |tag: u8, value: u32| CompiledInstruction::new_from_raw_parts(4, [&[tag][..], &value.to_le_bytes()].concat(), vec![]);
        let heavy = priced(
            &[a, b],
            &[system_program::ID, compute_budget::ID, bpf_loader_upgradeable::ID],
            vec![
                budget(2, MAX_COMPUTE_UNIT_LIMIT),
                budget(4, MAX_LOADED_ACCOUNTS_DATA_SIZE_BYTES.get()),
                create(1),
                create(2),
                // Upgrade, which compiles an ELF, so the transaction is deploy-capable.
                CompiledInstruction::new_from_raw_parts(5, 3u32.to_le_bytes().to_vec(), vec![]),
            ],
        );
        assert_eq!(heavy.allocated_bytes, solana_system_interface::MAX_PERMITTED_ACCOUNTS_DATA_ALLOCATIONS_PER_TRANSACTION as u64);
        assert!(heavy.units > MAX_BLOCK_UNITS / 4, "{} units is not the heavy transaction it means to be", heavy.units);
        assert!(heavy.units <= MAX_TRANSACTION_UNITS, "{} units against a bound of {MAX_TRANSACTION_UNITS}", heavy.units);

        // Fifteen secp256k1 instructions each claiming 255 signatures carry
        // none of them and are priced as 3,825 verifications. That is under the
        // block limit, so the block limit alone would admit it, and over an
        // account's share.
        let claims = vec![CompiledInstruction::new_from_raw_parts(1, vec![255], vec![]); 15];
        let unbacked = priced(&[], &[solana_sdk_ids::secp256k1_program::ID], claims);
        assert!(unbacked.units > MAX_WRITABLE_ACCOUNT_UNITS && unbacked.units < MAX_BLOCK_UNITS);
    }
}
