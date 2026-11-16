//! The jsonParsed oracle.
//!
//! Reads a fixture file of accounts and writes, for each one, the
//! `{program, parsed, space}` envelope Agave's own `solana-account-decoder`
//! produces for it, or null where Agave falls back to base64. The Go parsers
//! in `rpc/parsed.go` must reproduce the output byte for byte.
//!
//!     cargo run --manifest-path tools/parsed-oracle/Cargo.toml -- \
//!         rpc/testdata/parsed_fixtures.json rpc/testdata/parsed_golden.json

use {
    base64::{prelude::BASE64_STANDARD, Engine},
    serde::{Deserialize, Serialize},
    solana_account::Account,
    solana_account_decoder::{
        encode_ui_account,
        parse_account_data::{AccountAdditionalDataV3, SplTokenAdditionalDataV2},
        UiAccountData, UiAccountEncoding,
    },
    solana_pubkey::Pubkey,
    spl_generic_token::is_known_spl_token_id,
    spl_token_2022_interface::{
        extension::{
            interest_bearing_mint::InterestBearingConfig,
            scaled_ui_amount::ScaledUiAmountConfig, BaseStateWithExtensions, StateWithExtensions,
        },
        state::Mint,
    },
    std::{env, fs},
};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Fixture {
    name: String,
    pubkey: String,
    owner: String,
    /// The account's data, base64.
    data: String,
    /// The mint account's data, base64, for a token account. Absent where the
    /// method would find no mint, which is how a token account falls back.
    #[serde(default)]
    mint: Option<String>,
    /// `bank.clock().unix_timestamp` at the call, which the interest-bearing
    /// and scaled UI amount extensions read.
    #[serde(default)]
    unix_timestamp: i64,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Golden {
    name: String,
    /// The `{program, parsed, space}` envelope, or null where Agave falls
    /// back to base64.
    parsed: Option<serde_json::Value>,
}

fn key(s: &str) -> Pubkey {
    let raw = bs58::decode(s).into_vec().expect("base58");
    Pubkey::try_from(raw.as_slice()).expect("32 bytes")
}

fn additional_data(mint: &[u8], unix_timestamp: i64) -> Option<SplTokenAdditionalDataV2> {
    let mint = StateWithExtensions::<Mint>::unpack(mint).ok()?;
    Some(SplTokenAdditionalDataV2 {
        decimals: mint.base.decimals,
        interest_bearing_config: mint
            .get_extension::<InterestBearingConfig>()
            .map(|x| (*x, unix_timestamp))
            .ok(),
        scaled_ui_amount_config: mint
            .get_extension::<ScaledUiAmountConfig>()
            .map(|x| (*x, unix_timestamp))
            .ok(),
    })
}

fn main() {
    let args: Vec<String> = env::args().collect();
    if args.len() != 3 {
        eprintln!("usage: parsed-oracle <fixtures.json> <golden.json>");
        std::process::exit(2);
    }
    let fixtures: Vec<Fixture> =
        serde_json::from_slice(&fs::read(&args[1]).expect("read fixtures")).expect("parse fixtures");

    let mut golden = Vec::with_capacity(fixtures.len());
    for f in &fixtures {
        let owner = key(&f.owner);
        let account = Account {
            lamports: 1,
            data: BASE64_STANDARD.decode(&f.data).expect("base64 data"),
            owner,
            executable: false,
            rent_epoch: u64::MAX,
        };
        let extra = f
            .mint
            .as_ref()
            .filter(|_| is_known_spl_token_id(&owner))
            .and_then(|m| additional_data(&BASE64_STANDARD.decode(m).expect("base64 mint"), f.unix_timestamp))
            .map(|data| AccountAdditionalDataV3 {
                spl_token_additional_data: Some(data),
            });
        let ui = encode_ui_account(
            &key(&f.pubkey),
            &account,
            UiAccountEncoding::JsonParsed,
            extra,
            None,
        );
        golden.push(Golden {
            name: f.name.clone(),
            parsed: match ui.data {
                UiAccountData::Json(parsed) => Some(serde_json::to_value(parsed).expect("json")),
                _ => None,
            },
        });
    }

    let mut out = serde_json::to_string_pretty(&golden).expect("serialise");
    out.push('\n');
    fs::write(&args[2], out).expect("write golden");
}
