//! Per-block engine state, and the handle that names it.
//!
//! One slot holds the block in flight. BaseApp serializes ABCI calls, and
//! `TxRunner::Run` drives plan, resolve, execute and release in order within
//! one call. A second open block is an error, so the slot returns
//! `SVM_ERR_BUSY` for it. A slab keyed by handle would accept concurrent
//! blocks, which the chain never runs.
//!
//! The handle is a token, not a pointer. Go never holds a pointer into Rust
//! state. The engine rejects a token that does not match the open block, so a
//! stale plan fails and does not execute.

use crate::abi;
use rayon::prelude::*;
use solana_message::v0::LoadedAddresses;
use solana_pubkey::Pubkey;
use solana_runtime_transaction::runtime_transaction::RuntimeTransaction;
use solana_sdk_ids::system_program;
use solana_svm_transaction::svm_message::SVMStaticMessage;
use solana_transaction::sanitized::SanitizedTransaction;
use solana_transaction_error::TransactionError;
use std::collections::HashSet;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

/// Parsed state that must survive from `plan` to `execute` within one block.
#[derive(Default)]
pub struct Block {
    pub height: u64,
    /// This block's CometBFT header hash, the environment blockhash.
    pub blockhash: [u8; 32],
    /// One entry per transaction in the block, in block order. Each entry is
    /// the transaction as admission sanitized it, with placeholder addresses
    /// for any table it names (`check::placeholder_loader`). A message that
    /// names no table is sanitized exactly as phase 3 sanitizes it, so phase 3
    /// uses it and does not parse the bytes again. `None` is a transaction that
    /// failed admission here. `ProcessProposal` refuses a block that carries
    /// one, but this block may not have passed this node's `ProcessProposal`.
    /// So the block records the transaction as not processed and does not halt
    /// the machine.
    pub txs: Vec<Option<RuntimeTransaction<SanitizedTransaction>>>,
    /// The signature-result key of each transaction that phase 1 read. The
    /// block forgets them at release, because its transactions commit with it.
    pub signature_keys: Vec<crate::signatures::Key>,
    /// The transactions that phase 1 did not admit, by index in block order,
    /// with the engine's error. Go counts and logs them.
    pub dropped: Vec<(u32, String)>,
    /// Per transaction, the dedup key `recent_blockhash || message_hash`, at
    /// full width, not upstream's 20 bytes. Zero for a transaction that was not
    /// admitted, which spends no key.
    pub dedup_keys: Vec<[u8; 64]>,
    /// The address lookup table accounts that v0 transactions name. Empty for
    /// a block that names no table, such as a legacy-only block, so Go skips
    /// phase 2.
    pub alt_keys: Vec<[u8; 32]>,
    /// Per transaction, the addresses phase 2 resolved. Empty until phase 2
    /// runs. It stays empty for a block that names no table, where no
    /// transaction loads addresses. `None` is a transaction whose table did not
    /// resolve. The block drops it exactly as it drops an unadmitted one.
    pub loaded: Vec<Option<LoadedAddresses>>,
    /// Whether phase 3 has run. Phase 3 takes the transactions out of the
    /// block, so the engine refuses a second execute on the same handle and
    /// does not run it over nothing.
    pub executed: bool,
}

impl Block {
    /// The load list, in the order Go loads it and cuts it at the arena
    /// ceiling. The order decides which keys survive the cut. It is a pure
    /// function of the block.
    ///
    /// 1. The nine sysvars, which Go never counts.
    /// 2. Every fee payer and its durable-nonce account, in block order, so a
    ///    transaction that loses any other account can still be charged.
    /// 3. Every other key at its first use: transactions in block order, and
    ///    in each the static keys before the addresses its tables resolved.
    ///    Anza loads them in this order.
    ///
    /// Before phase 2, `loaded` is empty, and a block that names no table
    /// never fills it. So the list is final unless the block names a table.
    pub fn load_list(&self) -> Vec<[u8; 32]> {
        load_list_of(self.txs.iter().enumerate().filter_map(|(i, tx)| {
            let tx = tx.as_ref()?;
            let keys = tx.static_account_keys();
            let first = tx.instructions_iter().next().map(|ix| (ix.program_id_index, ix.accounts, ix.data));
            Some((keys, nonce_of(keys, first), self.loaded.get(i).and_then(Option::as_ref)))
        }))
    }
}

/// `Block::load_list` over the admitted transactions as three facts each, in
/// block order: the static keys, the nonce account the first instruction
/// advances, and the addresses phase 2 resolved. `transaction_keys` builds a
/// one-transaction list through it from a parsed message, so the two cannot
/// disagree.
pub fn load_list_of<'a>(
    txs: impl Iterator<Item = (&'a [Pubkey], Option<&'a Pubkey>, Option<&'a LoadedAddresses>)>,
) -> Vec<[u8; 32]> {
    let txs: Vec<_> = txs.collect();
    let mut seen = HashSet::new();
    let mut list = Vec::new();
    let mut push = |k: &Pubkey| {
        if seen.insert(k.to_bytes()) {
            list.push(k.to_bytes());
        }
    };
    crate::runtime::SYSVAR_IDS.iter().for_each(&mut push);
    for (keys, nonce, _) in &txs {
        keys.first().into_iter().chain(*nonce).for_each(&mut push);
    }
    for (keys, _, loaded) in &txs {
        keys.iter().for_each(&mut push);
        if let Some(loaded) = loaded {
            loaded.writable.iter().chain(&loaded.readonly).for_each(&mut push);
        }
    }
    list
}

/// The account a durable-nonce transaction advances, by Anza's
/// `get_durable_nonce`, which requires it to be a static key. `keys` are the
/// static keys, and `first` is the first instruction's program index, account
/// indexes and data. It skips that function's writability test. That is safe,
/// because a key it names that is not the nonce account only loads earlier.
pub fn nonce_of<'a>(keys: &'a [Pubkey], first: Option<(u8, &[u8], &[u8])>) -> Option<&'a Pubkey> {
    let (program, accounts, data) = first?;
    if keys.get(usize::from(program)) != Some(&system_program::ID) || data.get(..4) != Some(&4u32.to_le_bytes()[..]) {
        return None;
    }
    keys.get(usize::from(*accounts.first()?))
}

struct Slot {
    token: u64,
    block: Block,
}

static SLOT: Mutex<Option<Slot>> = Mutex::new(None);
static NEXT_TOKEN: AtomicU64 = AtomicU64::new(1);

/// Takes the slot for a new block. It fails if a block is already open. An
/// unreleased block is a bug in the Go driver, not a condition to recover from
/// silently.
pub fn open(block: Block) -> Result<u64, ()> {
    let mut slot = SLOT.lock().map_err(|_| ())?;
    if slot.is_some() {
        return Err(());
    }
    let token = NEXT_TOKEN.fetch_add(1, Ordering::Relaxed);
    *slot = Some(Slot { token, block });
    Ok(token)
}

/// Runs `f` against the open block, but only if `handle` still names it.
///
/// A token from a previous block fails here. It does not produce a plausible
/// result against the wrong accounts.
pub fn with<T>(handle: u64, f: impl FnOnce(&mut Block) -> T) -> Result<T, ()> {
    let mut slot = SLOT.lock().map_err(|_| ())?;
    match slot.as_mut() {
        Some(s) if s.token == handle => Ok(f(&mut s.block)),
        _ => Err(()),
    }
}

/// Drops the open block if `handle` names it. It is idempotent. Releasing a
/// handle twice is not an error, because the Go side releases from a defer.
pub fn release(handle: u64) {
    let block = match SLOT.lock() {
        Ok(mut slot) if slot.as_ref().is_some_and(|s| s.token == handle) => slot.take().map(|s| s.block),
        _ => None,
    };
    if let Some(block) = block {
        crate::signatures::forget(&block.signature_keys);
    }
}

/// Deserializes one block's transactions and collects the keys that phase 1
/// returns to Go.
pub fn plan(height: u64, blockhash: [u8; 32], req: &abi::Reader) -> Result<Block, ()> {
    use crate::signatures::{self, Event, Handler};
    let root = crate::runtime::root().ok_or(())?;
    // Deserialize, sanitize and verify in parallel. Each transaction is
    // independent and the predicate is pure. So the result does not depend on
    // the order, and the parallel map adds no determinism risk.
    //
    // Phase 1 holds the store for the whole pass and only reads it. `CheckTx`,
    // the only other user here, waits on the same ABCI connection.
    let store = signatures::lock();
    let results = &*store;
    let planned: Vec<_> = (0..req.len())
        .into_par_iter()
        .map(|i| {
            // FinalizeBlock has the final say on what ProcessProposal checked,
            // so it runs the same admission here in full. A transaction whose
            // exact bytes already passed ed25519 on this node skips that check
            // and no other. A transaction with no stored result gets the
            // check, so the store changes the time and never the answer.
            // Admission parses, sanitizes and hashes the message. The block
            // keeps the transaction and its dedup key and does not repeat that
            // work.
            let Some(bytes) = req.sized(i) else {
                return (None, false, Err(TransactionError::SanitizeFailure));
            };
            let key = signatures::key(bytes);
            let hit = results.holds(&key);
            let mode = if hit { crate::check::Mode::HashOnly } else { crate::check::Mode::Full };
            let admitted = crate::check::check_tx_keeping(&root, bytes, mode)
                .map(|(admission, tx)| (tx, admission.dedup_key))
                .map_err(|crate::check::CheckError::Transaction(e)| e);
            (Some(key), hit, admitted)
        })
        .collect();
    drop(store);
    let hits = planned.iter().filter(|(_, hit, _)| *hit).count();
    let keys: Vec<signatures::Key> = planned.iter().filter_map(|(key, _, _)| *key).collect();
    signatures::count(Event::Hit, Handler::FinalizeBlock, hits);
    signatures::count(Event::Miss, Handler::FinalizeBlock, keys.len() - hits);
    // This step does not count the block budgets. The price of a message that
    // names a table depends on how its tables resolve, and phase 2 has not run
    // yet. So `runtime::execute_block` counts every budget once.
    let mut dropped = Vec::new();
    let (txs, dedup_keys): (Vec<_>, Vec<_>) = planned
        .into_iter()
        .enumerate()
        .map(|(i, (_, _, p))| match p {
            Ok((tx, key)) => (Some(tx), key),
            Err(e) => {
                dropped.push((i as u32, format!("{e:?}")));
                (None, [0u8; 64])
            }
        })
        .unzip();

    // Key collection stays serial, in block order, so it cannot depend on the
    // order the parallel section finished in.
    let mut alt_keys: Vec<[u8; 32]> = Vec::new();
    let mut seen_alt: HashSet<[u8; 32]> = HashSet::new();

    for tx in txs.iter().flatten() {
        for lookup in tx.message_address_table_lookups() {
            let k = lookup.account_key.to_bytes();
            if seen_alt.insert(k) {
                alt_keys.push(k);
            }
        }
    }
    Ok(Block {
        height,
        blockhash,
        txs,
        dedup_keys,
        signature_keys: keys,
        dropped,
        alt_keys,
        loaded: Vec::new(),
        executed: false,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_message::{compiled_instruction::CompiledInstruction, legacy, v0, MessageHeader, VersionedMessage};
    use solana_transaction::{sanitized::MessageHash, versioned::VersionedTransaction};

    /// The message as admission sanitizes it, against placeholder addresses.
    fn tx(message: VersionedMessage) -> Option<RuntimeTransaction<SanitizedTransaction>> {
        let loader = crate::check::placeholder_loader(&message);
        let tx = VersionedTransaction { signatures: vec![Default::default()], message };
        Some(RuntimeTransaction::try_create(tx, MessageHash::Compute, None, loader, &HashSet::new()).unwrap())
    }

    fn header(readonly_unsigned: u8) -> MessageHeader {
        MessageHeader {
            num_required_signatures: 1,
            num_readonly_signed_accounts: 0,
            num_readonly_unsigned_accounts: readonly_unsigned,
        }
    }

    #[test]
    fn the_load_list_is_sysvars_then_payers_then_first_use() {
        let k = |n: u8| Pubkey::new_from_array([n; 32]);
        let (a, b, c, nonce, x, y, z, program) = (k(1), k(2), k(3), k(4), k(5), k(6), k(7), k(8));
        let plain = VersionedMessage::Legacy(legacy::Message {
            header: header(2),
            account_keys: vec![a, x, solana_sdk_ids::sysvar::clock::ID, program],
            recent_blockhash: Default::default(),
            instructions: vec![CompiledInstruction { program_id_index: 3, accounts: vec![1, 2], data: vec![] }],
        });
        let advance = VersionedMessage::Legacy(legacy::Message {
            header: header(1),
            account_keys: vec![b, nonce, system_program::ID],
            recent_blockhash: Default::default(),
            instructions: vec![CompiledInstruction {
                program_id_index: 2,
                accounts: vec![1, 0],
                data: 4u32.to_le_bytes().to_vec(),
            }],
        });
        let tabled = VersionedMessage::V0(v0::Message {
            header: header(0),
            account_keys: vec![c, y],
            recent_blockhash: Default::default(),
            instructions: vec![],
            address_table_lookups: vec![],
        });
        let blk = Block {
            height: 1,
            blockhash: [0; 32],
            txs: vec![tx(plain), None, tx(advance), tx(tabled)],
            dedup_keys: vec![[0; 64]; 4],
            signature_keys: vec![],
            dropped: vec![],
            alt_keys: vec![],
            loaded: vec![
                Some(Default::default()),
                None,
                Some(Default::default()),
                Some(v0::LoadedAddresses { writable: vec![z], readonly: vec![x] }),
            ],
            executed: false,
        };
        let mut want: Vec<[u8; 32]> = crate::runtime::SYSVAR_IDS.iter().map(|k| k.to_bytes()).collect();
        want.extend([a, b, nonce, c, x, program, system_program::ID, y, z].map(|k| k.to_bytes()));
        assert_eq!(blk.load_list(), want);
    }
}
