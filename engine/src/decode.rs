//! Takes a stored transaction apart into the fields that Agave's `json` and
//! `accounts` renderings name, so Go renders them without a second message
//! parser. It is machine-local. It reads no state, and its layout can change
//! in any release.

use agave_reserved_account_keys::ReservedAccountKeys;
use solana_message::{
    v0::{LoadedAddresses, LoadedMessage},
    VersionedMessage,
};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::collections::HashSet;
use std::sync::LazyLock;

/// `ReservedAccountKeys::new_all_activated()`, which Agave's renderer uses
/// whatever the bank's feature set.
static RESERVED: LazyLock<HashSet<Pubkey>> =
    LazyLock::new(|| ReservedAccountKeys::new_all_activated().active);

pub const FLAG_SIGNER: u8 = 1;
pub const FLAG_WRITABLE: u8 = 2;

/// The record, little-endian with explicit lengths:
///
/// ```text
///   u8 version                0 legacy, 1 + n for version n
///   u8 x 3                    the header
///   [u8; 32]                  recent blockhash, a v1 message's lifetime specifier
///   u32 n, n * [u8; 64]       signatures
///   u32 n, n * [u8; 32]       static account keys
///   u32 n, per instruction: u8 program index, u32 n, accounts, u32 len, data
///   u32 n, per lookup: [u8; 32] table, u32 n, writable indexes, u32 n, readonly indexes
///   u32 n, n * u8             FLAG_SIGNER | FLAG_WRITABLE per key, the
///                             loaded addresses after the static keys
///   v1 only, four options: u8 0 | u8 1 and the value, u64 priority fee then
///                             u32 compute unit limit, loaded accounts data
///                             size limit and heap size
/// ```
///
/// The flags are Agave's `parse_legacy_message_accounts`,
/// `parse_v0_message_accounts` and `parse_v1_message_accounts`, over the loaded
/// addresses the record stored. None for bytes that are not a transaction.
pub fn decode(bytes: &[u8], loaded: &LoadedAddresses) -> Option<Vec<u8>> {
    let tx: VersionedTransaction = wincode::deserialize(bytes).ok()?;
    let message = &tx.message;
    let len = |out: &mut Vec<u8>, n: usize| out.extend_from_slice(&(n as u32).to_le_bytes());
    let mut out = Vec::with_capacity(bytes.len() * 2);
    out.push(match message {
        VersionedMessage::Legacy(_) => 0,
        VersionedMessage::V0(_) => 1,
        VersionedMessage::V1(_) => 2,
    });
    let header = message.header();
    out.extend_from_slice(&[
        header.num_required_signatures,
        header.num_readonly_signed_accounts,
        header.num_readonly_unsigned_accounts,
    ]);
    out.extend_from_slice(message.recent_blockhash().as_ref());
    len(&mut out, tx.signatures.len());
    for s in &tx.signatures {
        out.extend_from_slice(s.as_ref());
    }
    let keys = message.static_account_keys();
    len(&mut out, keys.len());
    for k in keys {
        out.extend_from_slice(k.as_ref());
    }
    len(&mut out, message.instructions().len());
    for ix in message.instructions() {
        out.push(ix.program_id_index);
        len(&mut out, ix.accounts.len());
        out.extend_from_slice(&ix.accounts);
        len(&mut out, ix.data.len());
        out.extend_from_slice(&ix.data);
    }
    let lookups = message.address_table_lookups().unwrap_or_default();
    len(&mut out, lookups.len());
    for l in lookups {
        out.extend_from_slice(l.account_key.as_ref());
        len(&mut out, l.writable_indexes.len());
        out.extend_from_slice(&l.writable_indexes);
        len(&mut out, l.readonly_indexes.len());
        out.extend_from_slice(&l.readonly_indexes);
    }
    let flag = |signer: bool, writable: bool| {
        (if signer { FLAG_SIGNER } else { 0 }) | (if writable { FLAG_WRITABLE } else { 0 })
    };
    let flags: Vec<u8> = match message {
        VersionedMessage::Legacy(m) => (0..keys.len())
            .map(|i| flag(m.is_signer(i), m.is_maybe_writable_with_reserved_addresses(i, Some(&*RESERVED))))
            .collect(),
        VersionedMessage::V0(m) => {
            let loaded = LoadedMessage::new_borrowed(m, loaded, &RESERVED);
            (0..loaded.account_keys().len()).map(|i| flag(loaded.is_signer(i), loaded.is_writable(i))).collect()
        }
        VersionedMessage::V1(m) => (0..keys.len())
            .map(|i| flag(m.is_signer(i), m.is_maybe_writable_with_reserved_addresses(i, Some(&*RESERVED))))
            .collect(),
    };
    len(&mut out, flags.len());
    out.extend_from_slice(&flags);
    if let VersionedMessage::V1(m) = message {
        let c = &m.config;
        let mut option = |value: Option<Vec<u8>>| match value {
            Some(v) => {
                out.push(1);
                out.extend_from_slice(&v);
            }
            None => out.push(0),
        };
        option(c.priority_fee.map(|v| v.to_le_bytes().to_vec()));
        option(c.compute_unit_limit.map(|v| v.to_le_bytes().to_vec()));
        option(c.loaded_accounts_data_size_limit.map(|v| v.to_le_bytes().to_vec()));
        option(c.heap_size.map(|v| v.to_le_bytes().to_vec()));
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_hash::Hash;
    use solana_message::{compiled_instruction::CompiledInstruction, Message, MessageHeader};

    #[test]
    fn a_reserved_key_and_a_program_id_are_not_writable() {
        let payer = Pubkey::new_unique();
        let program = Pubkey::new_unique();
        let sysvar = solana_sdk_ids::sysvar::clock::id();
        let message = Message {
            header: MessageHeader { num_required_signatures: 1, num_readonly_signed_accounts: 0, num_readonly_unsigned_accounts: 0 },
            account_keys: vec![payer, sysvar, program],
            recent_blockhash: Hash::new_from_array([7; 32]),
            instructions: vec![CompiledInstruction { program_id_index: 2, accounts: vec![0, 1], data: vec![9] }],
        };
        let tx = VersionedTransaction { signatures: vec![Default::default()], message: VersionedMessage::Legacy(message) };
        let bytes = wincode::serialize(&tx).unwrap();
        let out = decode(&bytes, &LoadedAddresses::default()).unwrap();
        assert_eq!(out[0], 0);
        assert_eq!(&out[4..36], &[7; 32]);
        // version, header, blockhash, one signature, three keys, one
        // instruction of 1 + 4 + 2 + 4 + 1 bytes, no lookups.
        let flags_at = 1 + 3 + 32 + 4 + 64 + 4 + 96 + 4 + 12 + 4;
        assert_eq!(&out[flags_at..], &[3, 0, 0, 0, FLAG_SIGNER | FLAG_WRITABLE, 0, 0]);
    }
}
