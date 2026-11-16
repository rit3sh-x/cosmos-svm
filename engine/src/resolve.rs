//! Phase 2: address lookup table resolution.
//!
//! Every rule here is Anza's. The engine supplies the accounts and the current
//! slot. The status machine, the deactivation cool-down and the same-slot
//! extension rule stay in `AddressLookupTable::lookup_iter`.
//!
//! The port is `Accounts::load_lookup_table_addresses_into` and
//! `Bank::load_addresses_from_ref`, against the same table decoder at `=3.2.0`.

use crate::abi;
use crate::block::Block;
use solana_address_lookup_table_interface::{error::AddressLookupError, state::AddressLookupTable};
use solana_clock::Slot;
use solana_message::v0::{LoadedAddresses, MessageAddressTableLookup};
use solana_pubkey::Pubkey;
use solana_slot_hashes::SlotHashes;
use solana_svm_transaction::svm_message::SVMStaticMessage;
use solana_transaction_error::AddressLoaderError;
use std::collections::HashMap;

fn into_address_loader_error(err: AddressLookupError) -> AddressLoaderError {
    match err {
        AddressLookupError::LookupTableAccountNotFound => AddressLoaderError::LookupTableAccountNotFound,
        AddressLookupError::InvalidAccountOwner => AddressLoaderError::InvalidAccountOwner,
        AddressLookupError::InvalidAccountData => AddressLoaderError::InvalidAccountData,
        AddressLookupError::InvalidLookupIndex => AddressLoaderError::InvalidLookupIndex,
    }
}

/// The table accounts Go loaded, by key, and the `SlotHashes` it wrote this
/// block.
pub struct Tables {
    accounts: HashMap<[u8; 32], (Pubkey, Vec<u8>)>,
    slot_hashes: SlotHashes,
}

impl Tables {
    /// Builds the tables a simulation resolves against from its `SlotHashes`
    /// and the accounts given to it, by key, owner and data.
    pub fn new(slot_hashes: SlotHashes, accounts: impl IntoIterator<Item = ([u8; 32], Pubkey, Vec<u8>)>) -> Self {
        Tables {
            accounts: accounts.into_iter().map(|(key, owner, data)| (key, (owner, data))).collect(),
            slot_hashes,
        }
    }
}

/// One table's addresses, appended to `loaded`.
///
/// A port of agave. The owner check comes before the decode, so a non-table
/// account is `InvalidAccountOwner`, not a decode failure. Both `lookup_iter`
/// calls run before either result is consumed, so a transaction that names one
/// bad index fails whole.
fn load_one(
    tables: &Tables,
    current_slot: Slot,
    lookup: &MessageAddressTableLookup,
    loaded: &mut LoadedAddresses,
) -> Result<Slot, AddressLookupError> {
    let (owner, data) = tables
        .accounts
        .get(&lookup.account_key.to_bytes())
        .ok_or(AddressLookupError::LookupTableAccountNotFound)?;
    if owner != &solana_sdk_ids::address_lookup_table::ID {
        return Err(AddressLookupError::InvalidAccountOwner);
    }
    let table = AddressLookupTable::deserialize(data).map_err(|_| AddressLookupError::InvalidAccountData)?;

    let writable = table.lookup_iter(current_slot, &lookup.writable_indexes, &tables.slot_hashes)?;
    let readonly = table.lookup_iter(current_slot, &lookup.readonly_indexes, &tables.slot_hashes)?;
    for address in writable {
        loaded.writable.push(address.ok_or(AddressLookupError::InvalidLookupIndex)?);
    }
    for address in readonly {
        loaded.readonly.push(address.ok_or(AddressLookupError::InvalidLookupIndex)?);
    }
    Ok(table.meta.deactivation_slot)
}

/// Every table one transaction names, in message order, or the first error.
///
/// A port of agave, without the minimum deactivation slot. Upstream uses it
/// only for its transaction cache, and this chain has no equivalent.
pub fn load_addresses(
    tables: &Tables,
    current_slot: Slot,
    lookups: &[MessageAddressTableLookup],
) -> Result<LoadedAddresses, AddressLoaderError> {
    let mut loaded = LoadedAddresses::default();
    for lookup in lookups {
        load_one(tables, current_slot, lookup, &mut loaded).map_err(into_address_loader_error)?;
    }
    Ok(loaded)
}

pub enum ResolveError {
    /// Record 0 was absent or did not decode as `SlotHashes`.
    NoSlotHashes,
    BadInput,
}

/// Builds `Tables` from the request. Record 0 is the `SlotHashes` account, and
/// records 1 onward are the table accounts, in the order the plan response
/// named them.
pub(crate) fn tables(req: &abi::Reader) -> Result<Tables, ResolveError> {
    if req.is_empty() {
        return Err(ResolveError::NoSlotHashes);
    }
    let slot_hashes: SlotHashes =
        bincode::deserialize(req.record(0)).map_err(|_| ResolveError::NoSlotHashes)?;
    let mut accounts = HashMap::with_capacity(req.len().saturating_sub(1));
    for rec in req.iter().skip(1) {
        let a = abi::AccountRef::parse(rec).ok_or(ResolveError::BadInput)?;
        accounts.insert(a.pubkey, (Pubkey::from(a.owner), a.data.to_vec()));
    }
    Ok(Tables { accounts, slot_hashes })
}

/// Resolves every v0 transaction's lookups and returns the whole load list
/// again, with each resolved address at its first use. It replaces the list
/// that phase 1 returned, which could not know these addresses.
///
/// A transaction whose tables do not resolve is dropped, and this is not fatal.
/// It is the same class as a transaction phase 1 did not admit, and the same
/// rule applies. `ProcessProposal` cannot catch it either, because resolution
/// reads committed state that the proposer's copy of the block does not fix.
pub fn resolve(handle: u64, blk: &mut Block, req: &abi::Reader) -> Result<Box<[u8]>, ResolveError> {
    let tables = tables(req)?;
    blk.loaded = Vec::with_capacity(blk.txs.len());
    for i in 0..blk.txs.len() {
        let lookups: Vec<MessageAddressTableLookup> = match &blk.txs[i] {
            Some(tx) => tx
                .message_address_table_lookups()
                .map(|l| MessageAddressTableLookup {
                    account_key: *l.account_key,
                    writable_indexes: l.writable_indexes.to_vec(),
                    readonly_indexes: l.readonly_indexes.to_vec(),
                })
                .collect(),
            None => Vec::new(),
        };
        if lookups.is_empty() {
            blk.loaded.push(Some(LoadedAddresses::default()));
            continue;
        }
        match load_addresses(&tables, blk.height, &lookups) {
            Ok(loaded) => blk.loaded.push(Some(loaded)),
            Err(_) => {
                blk.txs[i] = None;
                blk.dedup_keys[i] = [0u8; 64];
                blk.loaded.push(None);
            }
        }
    }

    let list = blk.load_list();
    let mut w = abi::Writer::with_capacity(abi::KIND_RESOLVE_RESP, handle, list.len(), list.len() * 32);
    w.set_aux(list.len() as u32);
    for key in &list {
        w.push(key);
    }
    Ok(w.finish())
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_address_lookup_table_interface::state::LookupTableMeta;
    use std::borrow::Cow;

    fn table(addresses: &[Pubkey], last_extended: Slot) -> Vec<u8> {
        let meta = LookupTableMeta { last_extended_slot: last_extended, ..LookupTableMeta::default() };
        let table = AddressLookupTable { meta, addresses: Cow::Owned(addresses.to_vec()) };
        table.serialize_for_tests().unwrap()
    }

    fn tables_with(key: Pubkey, owner: Pubkey, data: Vec<u8>) -> Tables {
        Tables {
            accounts: HashMap::from([(key.to_bytes(), (owner, data))]),
            slot_hashes: SlotHashes::default(),
        }
    }

    fn lookup(key: Pubkey, writable: Vec<u8>, readonly: Vec<u8>) -> MessageAddressTableLookup {
        MessageAddressTableLookup { account_key: key, writable_indexes: writable, readonly_indexes: readonly }
    }

    /// A lookup that resolves, and the split into writable and readonly that the
    /// message asks for rather than the table's own order.
    #[test]
    fn a_table_resolves_to_the_addresses_the_message_indexes() {
        let key = Pubkey::new_unique();
        let addresses: Vec<Pubkey> = (0..4).map(|_| Pubkey::new_unique()).collect();
        let t = tables_with(key, solana_sdk_ids::address_lookup_table::ID, table(&addresses, 0));
        let loaded = load_addresses(&t, 5, &[lookup(key, vec![2, 0], vec![3])]).unwrap();
        assert_eq!(loaded.writable, vec![addresses[2], addresses[0]]);
        assert_eq!(loaded.readonly, vec![addresses[3]]);
    }

    /// Anza's rule. An address appended in the current slot is not visible to
    /// a lookup in that slot. This is a determinism rule. It must hold here, or
    /// two validators can disagree about what a transaction touched.
    #[test]
    fn an_address_added_this_slot_is_not_visible_this_slot() {
        let key = Pubkey::new_unique();
        let addresses: Vec<Pubkey> = (0..3).map(|_| Pubkey::new_unique()).collect();
        let t = tables_with(key, solana_sdk_ids::address_lookup_table::ID, table(&addresses, 9));
        assert_eq!(
            load_addresses(&t, 9, &[lookup(key, vec![2], vec![])]),
            Err(AddressLoaderError::InvalidLookupIndex),
            "the table was extended in slot 9, so slot 9 cannot see the new address"
        );
        assert!(load_addresses(&t, 10, &[lookup(key, vec![2], vec![])]).is_ok(), "slot 10 can");
    }

    /// The four error cases, each mapped to the upstream name.
    #[test]
    fn the_failure_cases_carry_anzas_errors() {
        let key = Pubkey::new_unique();
        let addresses: Vec<Pubkey> = (0..2).map(|_| Pubkey::new_unique()).collect();
        let good = table(&addresses, 0);

        let missing = Tables { accounts: HashMap::new(), slot_hashes: SlotHashes::default() };
        assert_eq!(
            load_addresses(&missing, 5, &[lookup(key, vec![0], vec![])]),
            Err(AddressLoaderError::LookupTableAccountNotFound)
        );
        let wrong_owner = tables_with(key, solana_sdk_ids::system_program::ID, good.clone());
        assert_eq!(
            load_addresses(&wrong_owner, 5, &[lookup(key, vec![0], vec![])]),
            Err(AddressLoaderError::InvalidAccountOwner)
        );
        let junk = tables_with(key, solana_sdk_ids::address_lookup_table::ID, vec![9u8; 40]);
        assert_eq!(
            load_addresses(&junk, 5, &[lookup(key, vec![0], vec![])]),
            Err(AddressLoaderError::InvalidAccountData)
        );
        let short = tables_with(key, solana_sdk_ids::address_lookup_table::ID, good);
        assert_eq!(
            load_addresses(&short, 5, &[lookup(key, vec![7], vec![])]),
            Err(AddressLoaderError::InvalidLookupIndex)
        );
    }

    /// A resolve request without `SlotHashes` as record 0 is an error, never an
    /// empty default. An empty sysvar would not fail. It would collapse the
    /// deactivation cool-down to zero for every deactivating table.
    #[test]
    fn a_request_without_slot_hashes_is_rejected() {
        let empty = abi::Writer::new(abi::KIND_RESOLVE_REQ, 1).finish();
        let reader = abi::Reader::new(&empty, abi::KIND_RESOLVE_REQ).unwrap();
        assert!(matches!(tables(&reader), Err(ResolveError::NoSlotHashes)));

        let mut w = abi::Writer::new(abi::KIND_RESOLVE_REQ, 1);
        w.push(&[0xff; 7]);
        let junk = w.finish();
        let reader = abi::Reader::new(&junk, abi::KIND_RESOLVE_REQ).unwrap();
        assert!(matches!(tables(&reader), Err(ResolveError::NoSlotHashes)));
    }

    /// Times the engine's half of phase 2 for the two table shapes of the
    /// block-limits suite, at the 2026-11-03 prices. Like `resolve`, it takes
    /// the table accounts out of the request, decodes every table that each
    /// transaction names, and builds the load list of the block again. The Go
    /// half, one read for each table, is the `resolve` phase of the suite.
    /// Each figure is the time of one block: the median of 21 runs after two
    /// warm-ups, on the bench VM.
    ///
    /// `cargo test --release -p svm-engine resolve_cost -- --ignored --nocapture`
    #[test]
    #[ignore]
    fn resolve_cost() {
        // tr: 1,994 transactions of 28 one-address tables; tw: 153 of one
        // table of 125 writable addresses.
        for (shape, txs, per_tx, width) in [("tr", 1_994usize, 28usize, 1usize), ("tw", 153, 1, 125)] {
            let raw: Vec<([u8; 32], Vec<u8>)> = (0..txs * per_tx)
                .map(|_| {
                    let addresses: Vec<Pubkey> = (0..width).map(|_| Pubkey::new_unique()).collect();
                    (Pubkey::new_unique().to_bytes(), table(&addresses, 0))
                })
                .collect();
            let statics: Vec<[Pubkey; 3]> = (0..txs).map(|_| [Pubkey::new_unique(), solana_sdk_ids::system_program::ID, solana_sdk_ids::compute_budget::ID]).collect();
            let lookups: Vec<Vec<MessageAddressTableLookup>> = (0..txs)
                .map(|t| {
                    (0..per_tx)
                        .map(|i| {
                            let key = Pubkey::from(raw[t * per_tx + i].0);
                            if width == 1 {
                                lookup(key, vec![], vec![0])
                            } else {
                                lookup(key, (0..width as u8).collect(), vec![])
                            }
                        })
                        .collect()
                })
                .collect();
            let run = || {
                let tables = Tables {
                    accounts: raw
                        .iter()
                        .map(|(k, d)| (*k, (solana_sdk_ids::address_lookup_table::ID, d.clone())))
                        .collect(),
                    slot_hashes: SlotHashes::default(),
                };
                let loaded: Vec<LoadedAddresses> =
                    lookups.iter().map(|l| load_addresses(&tables, 5, l).unwrap()).collect();
                let list = crate::block::load_list_of(
                    statics.iter().zip(&loaded).map(|(k, l)| (&k[..], None, Some(l))),
                );
                std::hint::black_box(list.len())
            };
            let mut ms: Vec<f64> = (0..23)
                .map(|_| {
                    let start = std::time::Instant::now();
                    run();
                    start.elapsed().as_secs_f64() * 1e3
                })
                .skip(2)
                .collect();
            ms.sort_by(f64::total_cmp);
            println!(
                "exp=resolve-engine shape={shape} txs={txs} tables={} addresses={} block_ms={:.2}",
                txs * per_tx,
                txs * per_tx * width,
                ms[ms.len() / 2]
            );
        }
    }
}
