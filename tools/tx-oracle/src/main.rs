//! The transaction rendering oracle.
//!
//! Reads a fixture file of stored transactions with their meta and writes, for
//! each one and each encoding it names, what Anza's own
//! `solana-transaction-status` 4.3.0 renders: `getTransaction`'s
//! `EncodedConfirmedTransactionWithStatusMeta`, and `getBlock`'s
//! `UiConfirmedBlock` for a block fixture. Each meta first goes through the
//! blockstore's protobuf and back, as Agave's RPC reads it.
//! The Go renderer must reproduce it, with numbers normalised as
//! rpc/parsed_golden_test.go normalises them.
//!
//!     cargo run --manifest-path tools/tx-oracle/Cargo.toml -- \
//!         rpc/testdata/tx_fixtures.json rpc/testdata/tx_golden.json
//!
//! With `instructions` first, it reads instruction fixtures instead and
//! writes what `parse_ui_instruction` renders for each.

use {
    base64::{prelude::BASE64_STANDARD, Engine},
    serde::{Deserialize, Serialize},
    solana_account_decoder::parse_token::token_amount_to_ui_amount_v3,
    solana_account_decoder::parse_account_data::SplTokenAdditionalDataV2,
    solana_message::{compiled_instruction::CompiledInstruction, v0::LoadedAddresses},
    solana_pubkey::Pubkey,
    solana_transaction::versioned::VersionedTransaction,
    solana_transaction_error::TransactionError,
    solana_transaction_status::{
        BlockEncodingOptions, ConfirmedBlock, ConfirmedTransactionWithStatusMeta, InnerInstruction,
        InnerInstructions, TransactionDetails, TransactionStatusMeta, TransactionTokenBalance,
        TransactionWithStatusMeta, UiTransactionEncoding, VersionedTransactionWithStatusMeta,
    },
    std::{env, fs, str::FromStr},
};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct TokenBalance {
    account_index: u8,
    mint: String,
    owner: String,
    program_id: String,
    amount: u64,
    decimals: u8,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Inner {
    program_id_index: u8,
    accounts: Vec<u8>,
    /// base64
    data: String,
    stack_height: u32,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Meta {
    /// The TransactionError as Agave's RPC renders it, or null.
    err: Option<serde_json::Value>,
    fee: u64,
    pre_balances: Vec<u64>,
    post_balances: Vec<u64>,
    inner: Vec<Vec<Inner>>,
    logs: Vec<String>,
    pre_token_balances: Vec<TokenBalance>,
    post_token_balances: Vec<TokenBalance>,
    loaded_writable: Vec<String>,
    loaded_readonly: Vec<String>,
    return_program: Option<String>,
    /// base64
    return_data: Option<String>,
    compute_units_consumed: u64,
    cost_units: u64,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Tx {
    /// The wire bytes, base64.
    tx: String,
    meta: Meta,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Fixture {
    name: String,
    slot: u64,
    block_time: i64,
    index: u32,
    #[serde(flatten)]
    tx: Tx,
    encodings: Vec<String>,
    max_supported_transaction_version: Option<u8>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct BlockFixture {
    name: String,
    previous_blockhash: String,
    blockhash: String,
    parent_slot: u64,
    block_time: i64,
    block_height: u64,
    transactions: Vec<Tx>,
    encodings: Vec<String>,
    details: Vec<String>,
    max_supported_transaction_version: Option<u8>,
}

#[derive(Deserialize)]
struct Fixtures {
    transactions: Vec<Fixture>,
    blocks: Vec<BlockFixture>,
}

#[derive(Serialize)]
struct Golden {
    name: String,
    encoding: String,
    /// The rendering, or the error Anza's encoder returned.
    rendered: Option<serde_json::Value>,
    error: Option<String>,
}

fn key(s: &str) -> Pubkey {
    Pubkey::from_str(s).expect("pubkey")
}

fn token_balances(list: &[TokenBalance]) -> Vec<TransactionTokenBalance> {
    list.iter()
        .map(|b| TransactionTokenBalance {
            account_index: b.account_index,
            mint: b.mint.clone(),
            ui_token_amount: token_amount_to_ui_amount_v3(b.amount, &SplTokenAdditionalDataV2::with_decimals(b.decimals)),
            owner: b.owner.clone(),
            program_id: b.program_id.clone(),
        })
        .collect()
}

fn with_meta(t: &Tx) -> VersionedTransactionWithStatusMeta {
    let bytes = BASE64_STANDARD.decode(&t.tx).expect("base64");
    let transaction: VersionedTransaction = wincode::deserialize(&bytes).expect("transaction");
    let m = &t.meta;
    let status = match &m.err {
        None => Ok(()),
        Some(v) => Err(serde_json::from_value::<TransactionError>(v.clone()).expect("error")),
    };
    let inner = m
        .inner
        .iter()
        .enumerate()
        .filter(|(_, list)| !list.is_empty())
        .map(|(i, list)| InnerInstructions {
            index: i as u8,
            instructions: list
                .iter()
                .map(|ix| InnerInstruction {
                    instruction: CompiledInstruction {
                        program_id_index: ix.program_id_index,
                        accounts: ix.accounts.clone(),
                        data: BASE64_STANDARD.decode(&ix.data).expect("data"),
                    },
                    stack_height: Some(ix.stack_height),
                })
                .collect(),
        })
        .collect();
    let return_data = m.return_program.as_ref().map(|p| solana_transaction_context::transaction::TransactionReturnData {
        program_id: key(p),
        data: BASE64_STANDARD.decode(m.return_data.as_deref().unwrap_or_default()).expect("return data"),
    });
    let meta = TransactionStatusMeta {
        status,
        fee: m.fee,
        pre_balances: m.pre_balances.clone(),
        post_balances: m.post_balances.clone(),
        inner_instructions: Some(inner),
        log_messages: Some(m.logs.clone()),
        pre_token_balances: Some(token_balances(&m.pre_token_balances)),
        post_token_balances: Some(token_balances(&m.post_token_balances)),
        rewards: Some(vec![]),
        loaded_addresses: LoadedAddresses {
            writable: m.loaded_writable.iter().map(|k| key(k)).collect(),
            readonly: m.loaded_readonly.iter().map(|k| key(k)).collect(),
        },
        return_data,
        compute_units_consumed: Some(m.compute_units_consumed),
        cost_units: Some(m.cost_units),
    };
    let stored = solana_storage_proto::convert::generated::TransactionStatusMeta::from(meta);
    VersionedTransactionWithStatusMeta {
        transaction,
        meta: TransactionStatusMeta::try_from(stored).expect("the blockstore's round trip"),
    }
}

fn encoding(name: &str) -> UiTransactionEncoding {
    serde_json::from_value(serde_json::Value::String(name.to_string())).expect("encoding")
}

fn details(name: &str) -> TransactionDetails {
    serde_json::from_value(serde_json::Value::String(name.to_string())).expect("details")
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct InstructionFixture {
    name: String,
    keys: Vec<String>,
    program_id_index: u8,
    accounts: Vec<u8>,
    /// base64
    data: String,
    stack_height: Option<u32>,
}

#[derive(Serialize)]
struct InstructionGolden {
    name: String,
    rendered: serde_json::Value,
}

/// Pretty JSON with every non-ASCII character escaped, so a golden file is
/// plain ASCII and parses to the same value. serde_json writes non-ASCII only
/// inside strings, where `\uXXXX` is the same character.
fn ascii_json<T: Serialize>(value: &T) -> String {
    let mut out = String::new();
    for c in serde_json::to_string_pretty(value).expect("json").chars() {
        if c.is_ascii() {
            out.push(c);
        } else {
            for unit in c.encode_utf16(&mut [0u16; 2]) {
                out.push_str(&format!("\\u{unit:04x}"));
            }
        }
    }
    out
}

fn instructions(input: &str, output: &str) {
    let fixtures: Vec<InstructionFixture> =
        serde_json::from_slice(&fs::read(input).expect("read fixtures")).expect("fixtures");
    let out: Vec<InstructionGolden> = fixtures
        .iter()
        .map(|f| {
            let keys: Vec<Pubkey> = f.keys.iter().map(|k| key(k)).collect();
            let account_keys = solana_message::AccountKeys::new(&keys, None);
            let ix = CompiledInstruction {
                program_id_index: f.program_id_index,
                accounts: f.accounts.clone(),
                data: BASE64_STANDARD.decode(&f.data).expect("data"),
            };
            let rendered = solana_transaction_status::parse_ui_instruction(&ix, &account_keys, f.stack_height);
            InstructionGolden { name: f.name.clone(), rendered: serde_json::to_value(rendered).expect("json") }
        })
        .collect();
    fs::write(output, ascii_json(&out)).expect("write golden");
}

fn main() {
    let args: Vec<String> = env::args().collect();
    if args[1] == "instructions" {
        return instructions(&args[2], &args[3]);
    }
    let fixtures: Fixtures = serde_json::from_slice(&fs::read(&args[1]).expect("read fixtures")).expect("fixtures");
    let mut out = Vec::new();
    for f in &fixtures.transactions {
        for name in &f.encodings {
            let confirmed = ConfirmedTransactionWithStatusMeta {
                slot: f.slot,
                tx_with_meta: TransactionWithStatusMeta::Complete(with_meta(&f.tx)),
                block_time: Some(f.block_time),
                index: f.index,
            };
            let (rendered, error) = match confirmed.encode(encoding(name), f.max_supported_transaction_version) {
                Ok(v) => (Some(serde_json::to_value(v).expect("json")), None),
                Err(e) => (None, Some(e.to_string())),
            };
            out.push(Golden { name: f.name.clone(), encoding: name.clone(), rendered, error });
        }
    }
    for b in &fixtures.blocks {
        for name in &b.encodings {
            for d in &b.details {
                let block = ConfirmedBlock {
                    previous_blockhash: b.previous_blockhash.clone(),
                    blockhash: b.blockhash.clone(),
                    parent_slot: b.parent_slot,
                    transactions: b.transactions.iter().map(|t| TransactionWithStatusMeta::Complete(with_meta(t))).collect(),
                    rewards: vec![],
                    num_partitions: None,
                    block_time: Some(b.block_time),
                    block_height: Some(b.block_height),
                };
                let options = BlockEncodingOptions {
                    transaction_details: details(d),
                    show_rewards: true,
                    max_supported_transaction_version: b.max_supported_transaction_version,
                };
                let (rendered, error) = match block.encode_with_options(encoding(name), options) {
                    Ok(v) => (Some(serde_json::to_value(v).expect("json")), None),
                    Err(e) => (None, Some(e.to_string())),
                };
                out.push(Golden { name: format!("{}/{d}", b.name), encoding: name.clone(), rendered, error });
            }
        }
    }
    fs::write(&args[2], ascii_json(&out)).expect("write golden");
}
