//! Runs Agave's own RpcClient against a running chain and calls every method
//! of the inventory once. RpcClient deserializes each answer into Agave's
//! types, so an answer whose shape differs fails here. A method this chain
//! refuses must refuse with the code Agave's refusal carries.
//!
//!     rpc-client-suite <url> <payer.json> <mint> <token account> <owner> <delegate>

use {
    solana_account_decoder_client_types::UiAccountEncoding,
    solana_commitment_config::CommitmentConfig,
    solana_keypair::{read_keypair_file, Keypair},
    solana_pubkey::Pubkey,
    solana_rpc_client::rpc_client::{GetConfirmedSignaturesForAddress2Config, RpcClient},
    solana_rpc_client_api::{
        client_error::{Error as ClientError, ErrorKind},
        config::{
            RpcAccountInfoConfig, RpcBlockConfig, RpcLargestAccountsConfig, RpcProgramAccountsConfig,
            RpcSimulateTransactionConfig, RpcTransactionConfig,
        },
        request::{RpcError, TokenAccountsFilter},
    },
    solana_signer::Signer,
    solana_transaction::Transaction,
    solana_transaction_status_client_types::{TransactionDetails, UiTransactionEncoding},
    std::{env, process::exit, str::FromStr},
};

fn code(e: &ClientError) -> Option<i64> {
    match e.kind() {
        ErrorKind::RpcError(RpcError::RpcResponseError { code, .. }) => Some(*code),
        _ => None,
    }
}

struct Suite {
    failed: Vec<String>,
}

impl Suite {
    fn ok<T: std::fmt::Debug>(&mut self, name: &str, result: Result<T, ClientError>) -> Option<T> {
        match result {
            Ok(v) => {
                println!("ok      {name}");
                Some(v)
            }
            Err(e) => {
                println!("FAIL    {name}: {e}");
                self.failed.push(name.to_string());
                None
            }
        }
    }

    fn refused<T: std::fmt::Debug>(&mut self, name: &str, result: Result<T, ClientError>, want: i64) {
        match result {
            Err(e) if code(&e) == Some(want) => println!("refused {name}: {want}"),
            other => {
                println!("FAIL    {name}: {other:?}, want the {want} refusal");
                self.failed.push(name.to_string());
            }
        }
    }
}

fn main() {
    let args: Vec<String> = env::args().collect();
    let client = RpcClient::new_with_commitment(args[1].clone(), CommitmentConfig::confirmed());
    let payer = read_keypair_file(&args[2]).expect("payer keypair");
    let key = |s: &str| Pubkey::from_str(s).expect("pubkey");
    let (mint, token_account, owner, delegate) = (key(&args[3]), key(&args[4]), key(&args[5]), key(&args[6]));
    let mut s = Suite { failed: vec![] };

    // A transfer of its own, so the history methods have a signature to read.
    let blockhash = client.get_latest_blockhash().expect("blockhash");
    let recipient = Keypair::new().pubkey();
    let transfer = Transaction::new_signed_with_payer(
        &[solana_system_interface::instruction::transfer(&payer.pubkey(), &recipient, 2_000_000_000)],
        Some(&payer.pubkey()),
        &[&payer],
        blockhash,
    );
    let signature = s.ok("send_and_confirm_transaction", client.send_and_confirm_transaction(&transfer)).expect("transfer");
    let slot = s.ok("get_slot", client.get_slot()).unwrap_or_default();

    s.ok("get_account", client.get_account(&payer.pubkey()));
    s.ok("get_ui_account_with_config", client.get_ui_account_with_config(&mint, RpcAccountInfoConfig {
        encoding: Some(UiAccountEncoding::JsonParsed),
        ..RpcAccountInfoConfig::default()
    }));
    s.ok("get_balance", client.get_balance(&recipient));
    s.ok("get_block_height", client.get_block_height());
    let transaction_slot = s
        .ok("get_signature_statuses", client.get_signature_statuses(&[signature]))
        .and_then(|r| r.value[0].clone())
        .map(|status| status.slot)
        .unwrap_or(slot);
    for (name, encoding) in [
        ("get_block_with_config json", UiTransactionEncoding::Json),
        ("get_block_with_config jsonParsed", UiTransactionEncoding::JsonParsed),
        ("get_block_with_config base64", UiTransactionEncoding::Base64),
    ] {
        s.ok(name, client.get_block_with_config(transaction_slot, RpcBlockConfig {
            encoding: Some(encoding),
            transaction_details: Some(TransactionDetails::Full),
            max_supported_transaction_version: Some(0),
            ..RpcBlockConfig::default()
        }));
    }
    s.ok("get_block_with_config accounts", client.get_block_with_config(transaction_slot, RpcBlockConfig {
        transaction_details: Some(TransactionDetails::Accounts),
        max_supported_transaction_version: Some(0),
        ..RpcBlockConfig::default()
    }));
    s.ok("get_block_production", client.get_block_production());
    s.ok("get_block_time", client.get_block_time(transaction_slot));
    s.ok("get_blocks", client.get_blocks(1, Some(transaction_slot)));
    s.ok("get_blocks_with_limit", client.get_blocks_with_limit(1, 10));
    s.ok("get_cluster_nodes", client.get_cluster_nodes());
    s.ok("get_epoch_info", client.get_epoch_info());
    s.ok("get_epoch_schedule", client.get_epoch_schedule());
    s.ok("get_fee_for_message", client.get_fee_for_message(&transfer.message));
    s.ok("get_first_available_block", client.get_first_available_block());
    s.ok("get_genesis_hash", client.get_genesis_hash());
    s.ok("get_health", client.get_health());
    s.refused("get_highest_snapshot_slot", client.get_highest_snapshot_slot(), -32008);
    s.ok("get_identity", client.get_identity());
    s.ok("get_inflation_governor", client.get_inflation_governor());
    s.ok("get_inflation_rate", client.get_inflation_rate());
    // Before the first epoch boundary the reward block is not reached yet.
    s.refused("get_inflation_reward", client.get_inflation_reward(&[payer.pubkey()], None), -32004);
    s.ok("get_largest_accounts_with_config", client.get_largest_accounts_with_config(RpcLargestAccountsConfig::default()));
    s.ok("get_leader_schedule", client.get_leader_schedule(None));
    s.ok("get_max_retransmit_slot", client.get_max_retransmit_slot());
    s.ok("get_max_shred_insert_slot", client.get_max_shred_insert_slot());
    s.ok("get_minimum_balance_for_rent_exemption", client.get_minimum_balance_for_rent_exemption(165));
    s.ok("get_multiple_accounts", client.get_multiple_accounts(&[payer.pubkey(), mint, recipient]));
    s.ok("get_program_ui_accounts_with_config", client.get_program_ui_accounts_with_config(
        &solana_pubkey::pubkey!("Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo"),
        RpcProgramAccountsConfig::default(),
    ));
    s.ok("get_recent_performance_samples", client.get_recent_performance_samples(Some(5)));
    s.ok("get_recent_prioritization_fees", client.get_recent_prioritization_fees(&[payer.pubkey()]));
    s.ok("get_signatures_for_address", client.get_signatures_for_address(&payer.pubkey()));
    s.ok("get_signatures_for_address_with_config", client.get_signatures_for_address_with_config(
        &payer.pubkey(),
        GetConfirmedSignaturesForAddress2Config { limit: Some(2), ..GetConfirmedSignaturesForAddress2Config::default() },
    ));
    s.ok("get_slot_leaders", client.get_slot_leaders(transaction_slot, 4));
    s.refused("get_stake_minimum_delegation", client.get_stake_minimum_delegation(), -32601);
    s.ok("supply", client.supply());
    s.ok("get_token_account_balance", client.get_token_account_balance(&token_account));
    s.ok("get_token_accounts_by_delegate", client.get_token_accounts_by_delegate(&delegate, TokenAccountsFilter::Mint(mint)));
    s.ok("get_token_accounts_by_owner", client.get_token_accounts_by_owner(&owner, TokenAccountsFilter::Mint(mint)));
    s.ok("get_token_largest_accounts", client.get_token_largest_accounts(&mint));
    s.ok("get_token_supply", client.get_token_supply(&mint));
    for (name, encoding) in [
        ("get_transaction_with_config json", UiTransactionEncoding::Json),
        ("get_transaction_with_config jsonParsed", UiTransactionEncoding::JsonParsed),
        ("get_transaction_with_config base58", UiTransactionEncoding::Base58),
        ("get_transaction_with_config base64", UiTransactionEncoding::Base64),
        ("get_transaction_with_config binary", UiTransactionEncoding::Binary),
    ] {
        s.ok(name, client.get_transaction_with_config(&signature, RpcTransactionConfig {
            encoding: Some(encoding),
            max_supported_transaction_version: Some(0),
            ..RpcTransactionConfig::default()
        }));
    }
    s.ok("get_transaction_count", client.get_transaction_count());
    s.ok("get_version", client.get_version());
    s.ok("get_vote_accounts", client.get_vote_accounts());
    s.ok("is_blockhash_valid", client.is_blockhash_valid(&blockhash, CommitmentConfig::confirmed()));
    s.ok("minimum_ledger_slot", client.minimum_ledger_slot());
    s.ok("get_feature_activation_slot", client.get_feature_activation_slot(
        &solana_pubkey::pubkey!("txv1aq4pp281K9um3tnPgkfX8UqtFT6wcVW3hNezGLL"),
    ));
    s.ok("simulate_transaction_with_config", client.simulate_transaction_with_config(&transfer, RpcSimulateTransactionConfig {
        replace_recent_blockhash: true,
        inner_instructions: true,
        ..RpcSimulateTransactionConfig::default()
    }));

    if s.failed.is_empty() {
        println!("PASS");
    } else {
        println!("{} failed: {}", s.failed.len(), s.failed.join(", "));
        exit(1);
    }
}
