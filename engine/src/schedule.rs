//! The phase-3 response, and the simulate response, which is built the same
//! way.
//!
//! Conflict partitioning lives in `runtime`, where the access sets that
//! execution locks on already exist. A second copy of the predicate, reading a
//! second copy of the access set, would be one more thing that must stay
//! bit-identical forever.

use crate::abi;
use crate::block::Block;
use crate::runtime;
use solana_account::{AccountSharedData, ReadableAccount};
use solana_message::v0::LoadedAddresses;
use solana_pubkey::Pubkey;
use solana_svm_transaction::svm_message::SVMStaticMessage;
use solana_transaction::versioned::TransactionVersion;
use std::collections::HashSet;

/// Per-transaction result record: status, group, compute units, fee, the
/// transaction's first signature, which the RPC history record is keyed by,
/// and the compute unit price it declared.
///
/// Status is 0 for success, 1 for a processed transaction that failed, and 2
/// for one not processed. A failed transaction's fee was charged and its dedup
/// key is spent. The fee is what the payer was debited, zero when the
/// transaction was not processed, so Go can check the supply invariant without
/// a counter.
const RESULT_HEAD: usize = 96;

fn result(o: &runtime::Outcome, group: u32) -> [u8; RESULT_HEAD] {
    let status: u32 = match (o.processed, &o.status) {
        (true, Ok(())) => 0,
        (true, Err(_)) => 1,
        (false, _) => 2,
    };
    let mut rec = [0u8; RESULT_HEAD];
    rec[0..4].copy_from_slice(&status.to_le_bytes());
    rec[4..8].copy_from_slice(&group.to_le_bytes());
    rec[8..16].copy_from_slice(&o.units.to_le_bytes());
    rec[16..24].copy_from_slice(&o.fee.to_le_bytes());
    rec[24..88].copy_from_slice(&o.signature);
    rec[88..96].copy_from_slice(&o.compute_unit_price.to_le_bytes());
    rec
}

/// The target of a record encoder. It is the response itself, or a `usize`
/// that counts the bytes of the record. One encoder writes into both, so the
/// code that fills the response also sets its size.
pub trait Out {
    fn put(&mut self, bytes: &[u8]);
}

impl Out for Vec<u8> {
    fn put(&mut self, bytes: &[u8]) {
        self.extend_from_slice(bytes);
    }
}

impl Out for usize {
    fn put(&mut self, bytes: &[u8]) {
        *self += bytes.len();
    }
}

fn put_len(out: &mut impl Out, n: usize) {
    out.put(&(n as u32).to_le_bytes());
}

/// The data that a meta record needs from its transaction: the version (0 for
/// legacy, 1 + n for version n) and the static keys of the message.
type Head = (u8, Vec<[u8; 32]>);

/// A transaction's metadata record, little-endian with explicit lengths,
/// because the arena pads records with zeros:
///
/// ```text
///   u8 version                0 legacy, 1 + n for version n
///   u32 n, n * u64            pre balances
///   u32 n, n * u64            post balances
///   u32 0 | u32 1, [u8; 32] program, u32 len, data     return data
///   u32 outer, per outer: u32 inner, per inner:
///       u8 program index, u8 stack height, u32 n, accounts, u32 len, data
///   u32 n, per log line: u32 len, bytes
///   u32 n, n * [u8; 32]       loaded writable addresses
///   u32 n, n * [u8; 32]       loaded readonly addresses
///   u32 len, bytes            the rendered TransactionError, empty on success
///   u64                       cost units, `cost().units`
///   pre token balances, then post, each as `push_token_balances` writes them
///   u32 n, n * [u8; 32]       the message's static keys
/// ```
///
/// A new field goes at the end, so no existing field moves. That is why the
/// error, the cost, the token balances and the static keys come last. The error
/// is the JSON that Agave's RPC emits, produced by `render_error`, and Go
/// forwards it without parsing it.
///
/// The loaded addresses are what a lookup table resolved for this transaction.
/// Empty for a legacy transaction, and empty for a v0 one that names no table.
/// The static keys are the message's own, in message order. With the loaded
/// addresses, they are every account the transaction named. The history's
/// address index uses them, with no second parse of the transaction.
///
/// `head` is the version and the static keys. It is None for a transaction
/// that phase 1 did not admit.
fn push_meta(
    rec: &mut impl Out,
    o: &runtime::Outcome,
    head: Option<&Head>,
    loaded: Option<&LoadedAddresses>,
) {
    let (version, statics) = head.map(|(v, s)| (*v, s.as_slice())).unwrap_or((0, &[]));
    let meta = &o.meta;
    rec.put(&[version]);
    for balances in [&meta.pre_balances, &meta.post_balances] {
        put_len(rec, balances.len());
        for b in balances {
            rec.put(&b.to_le_bytes());
        }
    }
    match &meta.return_data {
        Some((program, data)) => {
            put_len(rec, 1);
            rec.put(program.as_ref());
            put_len(rec, data.len());
            rec.put(data);
        }
        None => put_len(rec, 0),
    }
    push_inner(rec, &meta.inner_instructions);
    put_len(rec, meta.logs.len());
    for line in &meta.logs {
        put_len(rec, line.len());
        rec.put(line.as_bytes());
    }
    for addresses in [
        loaded.map(|l| l.writable.as_slice()).unwrap_or(&[]),
        loaded.map(|l| l.readonly.as_slice()).unwrap_or(&[]),
    ] {
        put_len(rec, addresses.len());
        for address in addresses {
            rec.put(address.as_ref());
        }
    }
    let error = render_error(&o.status);
    put_len(rec, error.len());
    rec.put(&error);
    rec.put(&meta.cost_units.to_le_bytes());
    push_token_balances(rec, &meta.pre_token_balances);
    push_token_balances(rec, &meta.post_token_balances);
    put_len(rec, statics.len());
    for key in statics {
        rec.put(key);
    }
}

/// Inner instructions: u32 outer, per outer: u32 inner, per inner: u8 program
/// index, u8 stack height, u32 n, accounts, u32 len, data.
pub fn push_inner(rec: &mut impl Out, list: &solana_message::inner_instruction::InnerInstructionsList) {
    put_len(rec, list.len());
    for inner in list {
        put_len(rec, inner.len());
        for ix in inner {
            rec.put(&[ix.instruction.program_id_index, ix.stack_height]);
            put_len(rec, ix.instruction.accounts.len());
            rec.put(&ix.instruction.accounts);
            put_len(rec, ix.instruction.data.len());
            rec.put(&ix.instruction.data);
        }
    }
}

/// Token balances as solana-svm collects them: u32 n, per balance: u8 account
/// index, [u8; 32] mint, [u8; 32] owner, [u8; 32] program id, u64 amount, u8
/// decimals.
pub fn push_token_balances(rec: &mut impl Out, list: &[solana_svm::transaction_balances::SvmTokenInfo]) {
    put_len(rec, list.len());
    for b in list {
        rec.put(&[b.account_index]);
        rec.put(b.mint.as_ref());
        rec.put(b.owner.as_ref());
        rec.put(b.program_id.as_ref());
        rec.put(&b.amount.to_le_bytes());
        rec.put(&[b.decimals]);
    }
}

/// The error as Agave's RPC renders it, or empty on success.
///
/// Only Anza's `Serialize` derive defines the shape of `err`. It is a public
/// wire format with no specification other than that code. So the engine
/// renders it, and Go forwards the bytes verbatim, as it forwards log lines.
/// Rendering it in Go would need a hand-kept table of 37 transaction errors and
/// about 50 instruction errors, plus a bincode enum decoder. That table would
/// drift silently on each crate bump.
///
/// Both paths call this one function, so they cannot disagree about the shape.
/// The type bounds the value, not a limit. No variant carries unbounded or
/// program-controlled data, and the longest rendering is 67 bytes.
pub fn render_error(status: &Result<(), solana_transaction_error::TransactionError>) -> Vec<u8> {
    match status {
        Ok(()) => Vec::new(),
        Err(e) => serde_json::to_vec(e).unwrap_or_default(),
    }
}

pub enum ExecuteError {
    BadInput,
    MissingSysvar(&'static str),
    Unconfigured,
}

/// Builds the phase-3 response: results first, then one metadata record per
/// transaction (`push_meta`), then the write set in commit order. Go applies
/// the writes in that order, so a later write to the same account wins, as
/// it does when Agave commits a batch.
///
/// Record 0 of the request is the already-processed bitmap, one byte per
/// transaction. The accounts follow as pinned records. The last record holds
/// each key Go dropped at the arena ceiling, 32 bytes each, and is empty when
/// Go dropped nothing.
///
/// # Safety
/// As `runtime::arena_accounts`, for every record between the first and the
/// last.
pub unsafe fn execute(handle: u64, blk: &mut Block, accounts: &abi::Reader) -> Result<Box<[u8]>, ExecuteError> {
    let n = accounts.len();
    if blk.executed || n < 2 || accounts.record(0).len() < blk.txs.len() {
        return Err(ExecuteError::BadInput);
    }
    let already = &accounts.record(0)[..blk.txs.len()];
    let dropped: HashSet<Pubkey> = abi::pubkeys(accounts.record(n - 1))
        .ok_or(ExecuteError::BadInput)?
        .into_iter()
        .collect();
    let map = runtime::arena_accounts(accounts, 1..n - 1).map_err(|_| ExecuteError::BadInput)?;

    // Read what each meta record needs of its transaction before execution
    // takes the transactions. That is the version, and the static keys that
    // the history's address index is built from.
    let heads: Vec<Option<Head>> = blk
        .txs
        .iter()
        .map(|tx| {
            tx.as_ref().map(|tx| {
                let version = match tx.version() {
                    TransactionVersion::Number(n) => n + 1,
                    TransactionVersion::Legacy(_) => 0,
                };
                (version, tx.static_account_keys().iter().map(|k| k.to_bytes()).collect())
            })
        })
        .collect();
    blk.executed = true;
    let (outcomes, writes, group_of) = runtime::execute_block(
        blk.height,
        blk.blockhash,
        std::mem::take(&mut blk.txs),
        &blk.loaded,
        &blk.dedup_keys,
        already,
        map,
        &dropped,
    )
    .map_err(|e| match e {
        runtime::ExecError::MissingSysvar(name) => ExecuteError::MissingSysvar(name),
        runtime::ExecError::Unconfigured => ExecuteError::Unconfigured,
    })?;

    Ok(response(handle, outcomes, &group_of, &heads, &blk.loaded, writes))
}

/// Builds the response in one allocation of its exact size. First it gets the
/// length of each record. For a meta record, it runs `push_meta` into a count.
/// Then it writes each record at its final position. It consumes the write
/// set, so it drops each written account when that account is in the response.
/// At the peak, the engine holds the written bytes twice: in its accounts and
/// in the response. `Writer` held them three times.
fn response(
    handle: u64,
    outcomes: Vec<runtime::Outcome>,
    group_of: &[u32],
    heads: &[Option<Head>],
    loaded: &[Option<LoadedAddresses>],
    writes: Vec<(Pubkey, AccountSharedData)>,
) -> Box<[u8]> {
    let n = outcomes.len();
    let mut body = n * RESULT_HEAD;
    for (i, (o, head)) in outcomes.iter().zip(heads).enumerate() {
        let mut len = 0usize;
        push_meta(&mut len, o, head.as_ref(), loaded.get(i).and_then(Option::as_ref));
        body += abi::padded(len);
    }
    for (_, acct) in &writes {
        body += abi::padded(abi::ACCOUNT_HEAD_LEN + acct.data().len());
    }

    let mut w = abi::ExactWriter::new(abi::KIND_EXECUTE_RESP, handle, n as u32, 2 * n + writes.len(), body);
    for (i, o) in outcomes.iter().enumerate() {
        w.record(|out| out.extend_from_slice(&result(o, group_of[i])));
    }
    for (i, (o, head)) in outcomes.into_iter().zip(heads).enumerate() {
        w.record(|out| push_meta(out, &o, head.as_ref(), loaded.get(i).and_then(Option::as_ref)));
    }
    for (key, acct) in writes {
        w.record(|out| {
            abi::put_account(
                out,
                &key.to_bytes(),
                &acct.owner().to_bytes(),
                acct.lamports(),
                acct.rent_epoch(),
                acct.executable(),
                acct.data(),
            )
        });
    }
    w.finish()
}

/// Writes record 0 of the simulate response, up to its accounts, into the
/// response or into a count. `accounts` is the number of accounts that follow.
/// `svm_simulate` in `lib.rs` has the layout.
fn simulation_head(out: &mut impl Out, sim: &runtime::Simulation, accounts: usize) {
    let status: u32 = match (sim.processed, &sim.status) {
        (true, Ok(())) => 0,
        (true, Err(_)) => 1,
        (false, _) => 2,
    };
    out.put(&status.to_le_bytes());
    out.put(&sim.loaded_accounts_data_size.to_le_bytes());
    out.put(&sim.units.to_le_bytes());
    out.put(&sim.fee.to_le_bytes());
    let error = render_error(&sim.status);
    put_len(out, error.len());
    out.put(&error);
    let display = sim.status.as_ref().err().map(ToString::to_string).unwrap_or_default();
    put_len(out, display.len());
    out.put(display.as_bytes());
    let (writable, readonly) = (&sim.loaded.writable, &sim.loaded.readonly);
    put_len(out, writable.len());
    put_len(out, readonly.len());
    for key in writable.iter().chain(readonly) {
        out.put(key.as_ref());
    }
    match &sim.balances {
        Some((pre, post)) => {
            out.put(&[1]);
            for balances in [pre, post] {
                put_len(out, balances.len());
                for b in balances {
                    out.put(&b.to_le_bytes());
                }
            }
        }
        None => out.put(&[0]),
    }
    push_token_balances(out, &sim.pre_token_balances);
    push_token_balances(out, &sim.post_token_balances);
    match &sim.inner_instructions {
        Some(inner) => {
            out.put(&[1]);
            push_inner(out, inner);
        }
        None => out.put(&[0]),
    }
    put_len(out, accounts);
}

/// Builds the simulate response the same way as the execute response. It gets
/// the size first and makes one allocation of the exact size. It drops each
/// account when that account is in the response. `sim.post_accounts` holds
/// only the accounts that the call asked for.
pub fn simulation_response(mut sim: runtime::Simulation) -> Box<[u8]> {
    let post = std::mem::take(&mut sim.post_accounts);
    let mut head = 0usize;
    simulation_head(&mut head, &sim, post.len());
    head += post.iter().map(|(_, a)| abi::ACCOUNT_HEAD_LEN + a.data().len()).sum::<usize>();
    let ret = sim.return_data.as_ref().map_or(0, |(_, data)| 36 + data.len());
    let logs: usize = sim.logs.iter().map(|line| abi::padded(4 + line.len())).sum();
    let body = abi::padded(head) + abi::padded(ret) + logs;

    let mut w = abi::ExactWriter::new(abi::KIND_SIMULATE_RESP, 0, 0, 2 + sim.logs.len(), body);
    w.record(|out| {
        simulation_head(out, &sim, post.len());
        for (key, account) in post {
            out.put(key.as_ref());
            out.put(&account.lamports().to_le_bytes());
            out.put(&account.rent_epoch().to_le_bytes());
            put_len(out, account.data().len());
            out.put(&[account.executable() as u8, 0, 0, 0]);
            out.put(account.owner().as_ref());
            out.put(account.data());
        }
    });
    w.record(|out| {
        if let Some((program, data)) = &sim.return_data {
            out.put(program.as_ref());
            put_len(out, data.len());
            out.put(data);
        }
    });
    for line in &sim.logs {
        w.record(|out| {
            put_len(out, line.len());
            out.put(line.as_bytes());
        });
    }
    w.finish()
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_instruction_error::InstructionError;
    use solana_message::compiled_instruction::CompiledInstruction;
    use solana_message::inner_instruction::InnerInstruction;
    use solana_svm::transaction_balances::SvmTokenInfo;
    use solana_transaction_error::TransactionError;

    /// The response as the old path built it, through `Writer`. The buffer
    /// grows by appending, and `finish` copies it once more.
    fn written(
        handle: u64,
        outcomes: &[runtime::Outcome],
        group_of: &[u32],
        heads: &[Option<Head>],
        loaded: &[Option<LoadedAddresses>],
        writes: &[(Pubkey, AccountSharedData)],
    ) -> Box<[u8]> {
        let mut w = abi::Writer::new(abi::KIND_EXECUTE_RESP, handle);
        w.set_aux(outcomes.len() as u32);
        for (i, o) in outcomes.iter().enumerate() {
            w.push(&result(o, group_of[i]));
        }
        for (i, (o, head)) in outcomes.iter().zip(heads).enumerate() {
            let mut rec = Vec::new();
            push_meta(&mut rec, o, head.as_ref(), loaded.get(i).and_then(Option::as_ref));
            w.push(&rec);
        }
        for (key, acct) in writes {
            w.push_account(
                &key.to_bytes(),
                &acct.owner().to_bytes(),
                acct.lamports(),
                acct.rent_epoch(),
                acct.executable(),
                acct.data(),
            );
        }
        w.finish()
    }

    /// Returns transaction `i`. Every fourth transaction has an empty meta, and
    /// the others set every meta field. The lengths change with `i`, so the
    /// padding changes. All three statuses occur.
    fn outcome(i: usize) -> runtime::Outcome {
        let key = |b: usize| Pubkey::new_from_array([b as u8; 32]);
        let mut meta = runtime::Meta::default();
        if !i.is_multiple_of(4) {
            meta.pre_balances = (0..i as u64 % 5).collect();
            meta.post_balances = (0..i as u64 % 7).map(|b| b * 3).collect();
            meta.logs = (0..i % 6).map(|l| "log ".repeat(l) + &i.to_string()).collect();
            meta.inner_instructions = vec![
                vec![InnerInstruction {
                    instruction: CompiledInstruction { program_id_index: 2, accounts: vec![0, 1, 3], data: vec![9; i % 11] },
                    stack_height: 2,
                }],
                vec![],
            ];
            meta.return_data = (i % 3 == 1).then(|| (key(i), vec![7; i % 13]));
            let token = SvmTokenInfo { account_index: 1, mint: key(1), amount: i as u64, owner: key(2), program_id: key(3), decimals: 6 };
            meta.pre_token_balances = vec![token.clone(); i % 3];
            meta.post_token_balances = vec![token; i % 2];
            meta.cost_units = 1_000 + i as u64;
        }
        runtime::Outcome {
            signature: [i as u8; 64],
            processed: i % 5 != 4,
            status: match i % 3 {
                0 => Ok(()),
                1 => Err(TransactionError::InstructionError(0, InstructionError::Custom(i as u32))),
                _ => Err(TransactionError::BlockhashNotFound),
            },
            fee: 5_000 * i as u64,
            units: 150 * i as u64,
            compute_unit_price: i as u64,
            meta,
        }
    }

    fn account(i: usize, len: usize) -> (Pubkey, AccountSharedData) {
        let acct = solana_account::Account {
            lamports: i as u64 * 1_000_003,
            data: (0..len).map(|b| (b * 31 + i) as u8).collect(),
            owner: Pubkey::new_from_array([(i % 7) as u8; 32]),
            executable: i % 2 == 1,
            rent_epoch: u64::MAX - i as u64,
        };
        (Pubkey::new_from_array([i as u8; 32]), acct.into())
    }

    /// The response has the same bytes as the `Writer` output. The cases are an
    /// empty block, a thousand small accounts, accounts of 10 MiB, and meta
    /// records of every shape. The meta records include transactions that
    /// phase 1 did not admit, and transactions with and without lookup tables.
    #[test]
    fn the_response_is_built_at_its_exact_size_and_its_bytes_do_not_change() {
        let cases: [(&str, usize, Vec<_>); 4] = [
            ("an empty block", 0, vec![]),
            ("many small accounts", 3, (0..1_000).map(|i| account(i, i % 300)).collect()),
            ("large accounts", 1, vec![account(1, 10 << 20), account(2, (10 << 20) + 3), account(3, (1 << 20) + 1), account(4, 0)]),
            ("meta records", 40, vec![account(5, 165)]),
        ];
        for (name, txs, writes) in cases {
            let outcomes: Vec<_> = (0..txs).map(outcome).collect();
            let group_of: Vec<u32> = (0..txs as u32).map(|g| g / 2).collect();
            let heads: Vec<Option<Head>> = (0..txs)
                .map(|i| (i % 7 != 6).then(|| ((i % 2) as u8, (0..i % 5).map(|k| [k as u8; 32]).collect())))
                .collect();
            // Shorter than the block, as before phase 2 or in a block of only
            // legacy transactions.
            let loaded: Vec<_> = (0..txs / 2)
                .map(|i| {
                    (i % 2 == 0).then(|| LoadedAddresses {
                        writable: (0..i % 3).map(|k| Pubkey::new_from_array([k as u8; 32])).collect(),
                        readonly: (0..i % 4).map(|k| Pubkey::new_from_array([k as u8 + 9; 32])).collect(),
                    })
                })
                .collect();

            let records = 2 * txs + writes.len();
            let old = written(11, &outcomes, &group_of, &heads, &loaded, &writes);
            let new = response(11, outcomes, &group_of, &heads, &loaded, writes);
            assert!(
                old == new,
                "{name}: {} bytes, the Writer's {}, first difference at {:?}",
                new.len(),
                old.len(),
                old.iter().zip(new.iter()).position(|(a, b)| a != b)
            );
            let r = abi::Reader::new(&new, abi::KIND_EXECUTE_RESP).unwrap();
            assert_eq!((r.len(), r.aux as usize), (records, txs), "{name}");
        }
    }

    /// The simulate response as the old `svm_simulate` built it, through
    /// `Writer`. The head grows by appending, then it is copied into the body,
    /// and `finish` copies it once more.
    fn simulation_written(sim: &runtime::Simulation) -> Box<[u8]> {
        let mut w = abi::Writer::new(abi::KIND_SIMULATE_RESP, 0);
        let status: u32 = match (sim.processed, &sim.status) {
            (true, Ok(())) => 0,
            (true, Err(_)) => 1,
            (false, _) => 2,
        };
        let error = render_error(&sim.status);
        let display = match &sim.status {
            Ok(()) => String::new(),
            Err(e) => e.to_string(),
        };
        let (writable, readonly) = (&sim.loaded.writable, &sim.loaded.readonly);
        let mut head = Vec::with_capacity(40 + error.len() + display.len() + 32 * (writable.len() + readonly.len()));
        head.extend_from_slice(&status.to_le_bytes());
        head.extend_from_slice(&sim.loaded_accounts_data_size.to_le_bytes());
        head.extend_from_slice(&sim.units.to_le_bytes());
        head.extend_from_slice(&sim.fee.to_le_bytes());
        head.extend_from_slice(&(error.len() as u32).to_le_bytes());
        head.extend_from_slice(&error);
        head.extend_from_slice(&(display.len() as u32).to_le_bytes());
        head.extend_from_slice(display.as_bytes());
        head.extend_from_slice(&(writable.len() as u32).to_le_bytes());
        head.extend_from_slice(&(readonly.len() as u32).to_le_bytes());
        for key in writable.iter().chain(readonly.iter()) {
            head.extend_from_slice(key.as_ref());
        }
        match &sim.balances {
            Some((pre, post)) => {
                head.push(1);
                for balances in [pre, post] {
                    head.extend_from_slice(&(balances.len() as u32).to_le_bytes());
                    for b in balances {
                        head.extend_from_slice(&b.to_le_bytes());
                    }
                }
            }
            None => head.push(0),
        }
        push_token_balances(&mut head, &sim.pre_token_balances);
        push_token_balances(&mut head, &sim.post_token_balances);
        match &sim.inner_instructions {
            Some(inner) => {
                head.push(1);
                push_inner(&mut head, inner);
            }
            None => head.push(0),
        }
        head.extend_from_slice(&(sim.post_accounts.len() as u32).to_le_bytes());
        for (key, account) in &sim.post_accounts {
            head.extend_from_slice(key.as_ref());
            head.extend_from_slice(&account.lamports().to_le_bytes());
            head.extend_from_slice(&account.rent_epoch().to_le_bytes());
            head.extend_from_slice(&(account.data().len() as u32).to_le_bytes());
            head.push(account.executable() as u8);
            head.extend_from_slice(&[0; 3]);
            head.extend_from_slice(account.owner().as_ref());
            head.extend_from_slice(account.data());
        }
        w.push(&head);
        match &sim.return_data {
            Some((program, data)) => {
                w.push(&[program.as_ref(), &(data.len() as u32).to_le_bytes(), data.as_slice()].concat())
            }
            None => w.push(&[]),
        }
        for line in &sim.logs {
            w.push(&[&(line.len() as u32).to_le_bytes()[..], line.as_bytes()].concat());
        }
        w.finish()
    }

    /// Returns simulation `i`, with `accounts` after the run. All three
    /// statuses occur, with and without balances, inner instructions and
    /// return data. The lengths of the logs and the lookup addresses change
    /// with `i`, so the padding changes.
    fn simulation(i: usize, accounts: Vec<(Pubkey, AccountSharedData)>) -> runtime::Simulation {
        let key = |b: usize| Pubkey::new_from_array([b as u8; 32]);
        let token = SvmTokenInfo { account_index: 1, mint: key(1), amount: i as u64, owner: key(2), program_id: key(3), decimals: 6 };
        runtime::Simulation {
            processed: i % 4 != 3,
            status: match i % 3 {
                0 => Ok(()),
                1 => Err(TransactionError::InstructionError(0, InstructionError::Custom(i as u32))),
                _ => Err(TransactionError::BlockhashNotFound),
            },
            units: 150 * i as u64,
            fee: 5_000 * i as u64,
            loaded_accounts_data_size: 1_000 * i as u32,
            logs: (0..i % 6).map(|l| "log\0 ".repeat(l) + &i.to_string()).collect(),
            return_data: (i % 2 == 1).then(|| (key(i), vec![7; i % 13])),
            loaded: LoadedAddresses { writable: (0..i % 3).map(key).collect(), readonly: (0..i % 4).map(|k| key(k + 9)).collect() },
            inner_instructions: (i % 3 != 2).then(|| {
                vec![
                    vec![InnerInstruction {
                        instruction: CompiledInstruction { program_id_index: 2, accounts: vec![0, 1, 3], data: vec![9; i % 11] },
                        stack_height: 2,
                    }],
                    vec![],
                ]
            }),
            balances: (i % 5 != 4).then(|| ((0..i as u64 % 5).collect(), (0..i as u64 % 7).map(|b| b * 3).collect())),
            pre_token_balances: vec![token.clone(); i % 3],
            post_token_balances: vec![token; i % 2],
            post_accounts: accounts,
        }
    }

    /// The simulate response has the same bytes as the `Writer` output. The
    /// cases have no accounts, accounts of 10 MiB, many small accounts, and one
    /// account, over every shape of the other fields.
    #[test]
    fn the_simulate_response_is_built_at_its_exact_size_and_its_bytes_do_not_change() {
        let accounts = |i: usize| match i % 4 {
            0 => vec![],
            1 => vec![account(1, 10 << 20), account(2, (10 << 20) + 3), account(3, 0)],
            2 => (0..50).map(|a| account(a, a % 300)).collect(),
            _ => vec![account(5, 165)],
        };
        for i in 0..24 {
            let old = simulation_written(&simulation(i, accounts(i)));
            let new = simulation_response(simulation(i, accounts(i)));
            assert!(
                old == new,
                "simulation {i}: {} bytes, the Writer's {}, first difference at {:?}",
                new.len(),
                old.len(),
                old.iter().zip(new.iter()).position(|(a, b)| a != b)
            );
            let r = abi::Reader::new(&new, abi::KIND_SIMULATE_RESP).unwrap();
            assert_eq!(r.len(), 2 + i % 6, "simulation {i}");
        }
    }

    /// The rendering is Anza's derive. These strings are what an Agave RPC
    /// returns for the same failures, and nothing in this repository decides
    /// them. A crate bump that changes the shape of a client-visible field
    /// shows up here.
    ///
    /// It also pins two properties of the design. The value does
    /// not depend on whether the transaction was processed, so a status-2
    /// failure carries its error exactly as a status-1 one does. And the type
    /// bounds it, because no variant carries unbounded or program-controlled
    /// data.
    #[test]
    fn the_rendered_error_is_anzas_own_json() {
        assert!(render_error(&Ok(())).is_empty(), "success renders nothing at all");

        for (status, want) in [
            (TransactionError::BlockhashNotFound, r#""BlockhashNotFound""#),
            (TransactionError::AccountLoadedTwice, r#""AccountLoadedTwice""#),
            (TransactionError::TooManyAccountLocks, r#""TooManyAccountLocks""#),
            (
                TransactionError::InstructionError(0, InstructionError::Custom(1)),
                r#"{"InstructionError":[0,{"Custom":1}]}"#,
            ),
            (
                TransactionError::InstructionError(255, InstructionError::BorshIoError),
                r#"{"InstructionError":[255,"BorshIoError"]}"#,
            ),
            (
                TransactionError::InsufficientFundsForRent { account_index: 1 },
                r#"{"InsufficientFundsForRent":{"account_index":1}}"#,
            ),
            (
                TransactionError::DuplicateInstruction(3),
                r#"{"DuplicateInstruction":3}"#,
            ),
        ] {
            assert_eq!(String::from_utf8(render_error(&Err(status))).unwrap(), want);
        }

        // The longest rendering either enum can produce. It needs no bound of
        // its own. There is at most one per transaction, and block.max_bytes
        // already bounds the transaction count.
        let longest = render_error(&Err(TransactionError::InstructionError(
            255,
            InstructionError::BuiltinProgramsMustConsumeComputeUnits,
        )));
        assert_eq!(longest.len(), 67);
    }
}
