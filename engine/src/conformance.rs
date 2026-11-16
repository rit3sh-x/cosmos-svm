//! The instruction-level differential.
//!
//! The oracle is `solana-svm`'s own conformance harness at the engine's exact
//! version, behind `dev-context-only-utils`. So there is one build, one
//! lockfile and one VM. A transaction-level diff against Agave's `Bank` would
//! compare that crate to itself. Everything this engine adds sits outside it.
//! The harness holds the fixture still, so the callback, the program cache and
//! the sysvar cache can be compared:
//!
//! ```text
//! held constant   the fixture: accounts, instruction, cu_avail, feature set
//! varied          callback, program cache, sysvar cache
//! reference       DefaultCallback, Anza's builtin table, a cache filled
//!                 directly from the fixture's sysvar accounts
//! ours            the execute arena, our builtin table, and the sysvar cache
//!                 the engine's own block path fills
//! ```
//!
//! Account loading is outside it on purpose. `execute_instr` takes accounts from
//! the fixture, so a green run says nothing about whether Go read the right
//! account from the store.

use crate::runtime::{Arena, BUILTINS, SLOTS_PER_EPOCH, build_root};
use solana_account::{Account, AccountSharedData};
use solana_instruction::{AccountMeta, Instruction};
use solana_program_runtime::loaded_programs::ProgramCacheForTxBatch;
use solana_program_runtime::program_cache_entry::ProgramCacheEntry;
use solana_program_runtime::sysvar_cache::SysvarCache;
use solana_pubkey::Pubkey;
use solana_svm::conformance::callback::DefaultCallback;
use solana_svm::conformance::instr::context::InstrContext;
use solana_svm::conformance::instr::effects::InstrEffects;
use solana_svm::conformance::instr::harness::execute_instr_with_callback;
use solana_svm::conformance::programs::new_program_cache_with_builtins;
use solana_svm_feature_set::SVMFeatureSet;
use std::collections::HashMap;
use std::sync::Arc;

/// The height every fixture runs at. Any slot works. It is held constant, so the
/// two sides cannot differ over it.
pub(crate) const SLOT: u64 = 7;

/// A fixture is plain Rust that names the accounts and the instruction. The
/// budget is the harness default. Fixtures need no file format, recorder or
/// loader.
struct Fixture {
    name: &'static str,
    accounts: Vec<(Pubkey, Account)>,
    instruction: Instruction,
}

fn key(seed: u8) -> Pubkey {
    Pubkey::new_from_array([seed; 32])
}

fn funded(seed: u8, lamports: u64) -> (Pubkey, Account) {
    (
        key(seed),
        Account { lamports, owner: solana_sdk_ids::system_program::ID, ..Account::default() },
    )
}

/// The sysvar accounts x/svm writes every block (x/svm/sysvars.go), in the
/// layouts `runtime::tests::go_sysvar_layouts_decode_to_the_pinned_types`
/// pins. Both sides read their sysvars from these bytes. Only the path to them
/// differs.
pub(crate) fn sysvar_accounts(rent_per_byte_year: u64) -> Vec<(Pubkey, Account)> {
    let mut rent = rent_per_byte_year.to_le_bytes().to_vec();
    rent.extend_from_slice(&[0, 0, 0, 0, 0, 0, 240, 63, 50]);
    let mut schedule = vec![0u8; 33];
    schedule[0..8].copy_from_slice(&SLOTS_PER_EPOCH.to_le_bytes());
    schedule[8..16].copy_from_slice(&SLOTS_PER_EPOCH.to_le_bytes());
    let mut clock = vec![0u8; 40];
    for (off, v) in [(0, SLOT), (8, 100), (16, 0), (24, 1), (32, 135)] {
        clock[off..off + 8].copy_from_slice(&v.to_le_bytes());
    }
    let mut slot_hashes = vec![0u8; 8 + 512 * 40];
    slot_hashes[0..8].copy_from_slice(&1u64.to_le_bytes());
    slot_hashes[8..16].copy_from_slice(&(SLOT - 1).to_le_bytes());
    slot_hashes[16..48].copy_from_slice(&[9u8; 32]);
    let mut recent = vec![0u8; 8 + 150 * 40];
    recent[0..8].copy_from_slice(&1u64.to_le_bytes());
    recent[8..40].copy_from_slice(&[9u8; 32]);
    recent[40..48].copy_from_slice(&5_000u64.to_le_bytes());

    let data = [
        (crate::runtime::SYSVAR_IDS[0], clock),
        (crate::runtime::SYSVAR_IDS[1], schedule),
        (crate::runtime::SYSVAR_IDS[2], vec![0u8; 81]),
        (crate::runtime::SYSVAR_IDS[3], rent),
        (crate::runtime::SYSVAR_IDS[4], slot_hashes),
        (crate::runtime::SYSVAR_IDS[5], vec![0u8; 8 + 512 * 32]),
        (crate::runtime::SYSVAR_IDS[6], vec![0u8; 8]),
        (crate::runtime::SYSVAR_IDS[8], recent),
    ];
    data.into_iter()
        .map(|(id, bytes)| {
            (
                id,
                Account {
                    lamports: 1,
                    data: bytes,
                    owner: solana_sdk_ids::sysvar::ID,
                    rent_epoch: u64::MAX,
                    executable: false,
                },
            )
        })
        .collect()
}

/// The default Rent lamports_per_byte_year, which x/svm also writes.
pub(crate) const RENT_PER_BYTE_YEAR: u64 = 6_960;

fn system(data: Vec<u8>, metas: Vec<AccountMeta>) -> Instruction {
    Instruction { program_id: solana_sdk_ids::system_program::ID, accounts: metas, data }
}

/// The corpus. Every fixture runs on every variant on every run. There is no
/// subset per change and no nightly run.
fn corpus() -> Vec<Fixture> {
    let payer = key(1);
    let dest = key(2);
    let created = key(3);
    let mut out = Vec::new();

    let mut transfer = vec![2u8, 0, 0, 0];
    transfer.extend_from_slice(&1_000_000u64.to_le_bytes());
    out.push(Fixture {
        name: "system transfer",
        accounts: vec![funded(1, 10_000_000_000), funded(2, 0)],
        instruction: system(
            transfer.clone(),
            vec![AccountMeta::new(payer, true), AccountMeta::new(dest, false)],
        ),
    });

    let mut over = vec![2u8, 0, 0, 0];
    over.extend_from_slice(&u64::MAX.to_le_bytes());
    out.push(Fixture {
        name: "system transfer beyond the balance",
        accounts: vec![funded(1, 10), funded(2, 0)],
        instruction: system(
            over,
            vec![AccountMeta::new(payer, true), AccountMeta::new(dest, false)],
        ),
    });

    out.push(Fixture {
        name: "system transfer from an unsigned payer",
        accounts: vec![funded(1, 10_000_000_000), funded(2, 0)],
        instruction: system(
            transfer,
            vec![AccountMeta::new(payer, false), AccountMeta::new(dest, false)],
        ),
    });

    // CreateAccount's rent-exemption check is in the transaction loading path,
    // not the instruction. So the nonce fixture below is the one that reads
    // Rent from the sysvar cache.
    for (name, lamports) in
        [("system create account", 1_113_600u64), ("system create account below rent", 1u64)]
    {
        let mut data = vec![0u8; 4];
        data.extend_from_slice(&lamports.to_le_bytes());
        data.extend_from_slice(&64u64.to_le_bytes());
        data.extend_from_slice(solana_sdk_ids::system_program::ID.as_ref());
        out.push(Fixture {
            name,
            accounts: vec![funded(1, 10_000_000_000), funded(3, 0)],
            instruction: system(
                data,
                vec![AccountMeta::new(payer, true), AccountMeta::new(created, true)],
            ),
        });
    }

    let mut allocate = vec![8u8, 0, 0, 0];
    allocate.extend_from_slice(&128u64.to_le_bytes());
    out.push(Fixture {
        name: "system allocate",
        accounts: vec![funded(3, 10_000_000_000)],
        instruction: system(allocate, vec![AccountMeta::new(created, true)]),
    });

    let mut assign = vec![1u8, 0, 0, 0];
    assign.extend_from_slice(solana_sdk_ids::bpf_loader::ID.as_ref());
    out.push(Fixture {
        name: "system assign",
        accounts: vec![funded(3, 10_000_000_000)],
        instruction: system(assign, vec![AccountMeta::new(created, true)]),
    });

    // A durable nonce reads RecentBlockhashes, the other sysvar Go writes
    // every block.
    let mut nonce = vec![6u8, 0, 0, 0]; // InitializeNonceAccount
    nonce.extend_from_slice(payer.as_ref());
    // Exactly the rent-exempt minimum for 80 bytes, so a Rent this chain does
    // not use flips the instruction's verdict.
    let mut nonce_account = Account {
        lamports: (128 + 80) * RENT_PER_BYTE_YEAR,
        data: vec![0u8; 80],
        owner: solana_sdk_ids::system_program::ID,
        ..Account::default()
    };
    nonce_account.rent_epoch = 0;
    out.push(Fixture {
        name: "system initialize nonce account",
        accounts: vec![(created, nonce_account)],
        instruction: system(
            nonce,
            vec![
                AccountMeta::new(created, false),
                AccountMeta::new_readonly(crate::runtime::SYSVAR_IDS[8], false),
                AccountMeta::new_readonly(crate::runtime::SYSVAR_IDS[3], false),
            ],
        ),
    });

    out.push(Fixture {
        name: "compute budget unit limit",
        accounts: vec![funded(1, 10_000_000_000)],
        instruction: Instruction {
            program_id: solana_sdk_ids::compute_budget::ID,
            accounts: vec![],
            data: {
                let mut d = vec![2u8];
                d.extend_from_slice(&300_000u32.to_le_bytes());
                d
            },
        },
    });

    out.push(Fixture {
        name: "an instruction the system program does not know",
        accounts: vec![funded(1, 10_000_000_000)],
        instruction: system(vec![99, 0, 0, 0], vec![AccountMeta::new(payer, true)]),
    });

    out
}

/// context builds the fixture's `InstrContext`. It appends the sysvar
/// accounts, so both sides see one account list. It adds the program's own
/// account, because the v4.3.0 harness takes the loader from it. Every program
/// here is a builtin, whose account the native loader owns, as genesis writes
/// it.
fn context(fixture: &Fixture, features: SVMFeatureSet, rent_per_byte_year: u64) -> InstrContext {
    let mut accounts = fixture.accounts.clone();
    let program = fixture.instruction.program_id;
    if !accounts.iter().any(|(k, _)| *k == program) {
        accounts.push((
            program,
            Account {
                lamports: 1,
                data: Vec::new(),
                owner: solana_sdk_ids::native_loader::ID,
                rent_epoch: u64::MAX,
                executable: true,
            },
        ));
    }
    accounts.extend(sysvar_accounts(rent_per_byte_year));
    InstrContext::new_with_default_budget(features, accounts, fixture.instruction.clone())
}

/// The reference side: Anza's callback, Anza's builtin table, and a sysvar
/// cache filled straight from the fixture's accounts.
fn reference(ctx: &InstrContext) -> InstrEffects {
    let mut cache = new_program_cache_with_builtins(SLOT);
    let accounts: HashMap<Pubkey, Vec<u8>> =
        ctx.accounts.iter().map(|(k, a)| (*k, a.data.clone())).collect();
    let mut sysvars = SysvarCache::default();
    sysvars.fill_missing_entries(|key, set| {
        if let Some(data) = accounts.get(key) {
            set(data)
        }
    });
    execute_instr_with_callback(ctx, &DefaultCallback, &mut cache, &sysvars)
}

/// The engine's side: the execute arena as the account source and the
/// callback, the engine's builtin table, and the sysvar cache the block path
/// fills.
fn ours(ctx: &InstrContext, gates: &[(Pubkey, u64)]) -> InstrEffects {
    let root = build_root(gates);
    let child = root.processor.new_from(SLOT, SLOT / SLOTS_PER_EPOCH);
    let arena = Arena(
        ctx.accounts
            .iter()
            .map(|(k, a)| (*k, AccountSharedData::from(a.clone())))
            .collect::<HashMap<_, _>>(),
    );
    child.reset_and_fill_sysvar_cache_entries(&arena);

    let mut cache = ProgramCacheForTxBatch::default();
    cache.set_slot_for_tests(SLOT);
    for (id, _name, register) in BUILTINS {
        cache.replenish(id, Arc::new(ProgramCacheEntry::new_builtin(0, register)));
    }
    let sysvars = child.sysvar_cache();
    execute_instr_with_callback(ctx, &arena, &mut cache, &sysvars)
}

/// diff names the first field where the two `InstrEffects` differ, because a
/// bare "fixture N failed" is not actionable.
fn diff(reference: &InstrEffects, ours: &InstrEffects) -> Option<String> {
    if reference.result != ours.result {
        return Some(format!("result: reference {:?}, ours {:?}", reference.result, ours.result));
    }
    if reference.custom_err != ours.custom_err {
        return Some(format!(
            "custom_err: reference {:?}, ours {:?}",
            reference.custom_err, ours.custom_err
        ));
    }
    if reference.cu_avail != ours.cu_avail {
        return Some(format!(
            "cu_avail: reference {}, ours {}",
            reference.cu_avail, ours.cu_avail
        ));
    }
    if reference.return_data != ours.return_data {
        return Some(format!(
            "return_data: reference {:?}, ours {:?}",
            reference.return_data, ours.return_data
        ));
    }
    if reference.resulting_accounts.len() != ours.resulting_accounts.len() {
        return Some(format!(
            "resulting_accounts: reference {} accounts, ours {}",
            reference.resulting_accounts.len(),
            ours.resulting_accounts.len()
        ));
    }
    for (pubkey, want) in &reference.resulting_accounts {
        let Some(got) = ours.get_account(pubkey) else {
            return Some(format!("resulting_accounts: ours is missing {pubkey}"));
        };
        if let Some(field) = account_diff(want, got) {
            return Some(format!("resulting_accounts[{pubkey}]: {field}"));
        }
    }
    if reference.logs != ours.logs {
        // Both sides run the same crate build, so a log divergence is a real
        // signal about the callback or the caches, even though Firedancer's
        // tooling drops the field.
        return Some(format!("logs: reference {:?}, ours {:?}", reference.logs, ours.logs));
    }
    None
}

fn account_diff(want: &Account, got: &Account) -> Option<String> {
    if want.lamports != got.lamports {
        return Some(format!("lamports: reference {}, ours {}", want.lamports, got.lamports));
    }
    if want.owner != got.owner {
        return Some(format!("owner: reference {}, ours {}", want.owner, got.owner));
    }
    if want.executable != got.executable {
        return Some(format!(
            "executable: reference {}, ours {}",
            want.executable, got.executable
        ));
    }
    if want.rent_epoch != got.rent_epoch {
        return Some(format!("rent_epoch: reference {}, ours {}", want.rent_epoch, got.rent_epoch));
    }
    if want.data.len() != got.data.len() {
        return Some(format!(
            "data: reference {} bytes, ours {}",
            want.data.len(),
            got.data.len()
        ));
    }
    for (i, (a, b)) in want.data.iter().zip(&got.data).enumerate() {
        if a != b {
            return Some(format!("data: first differs at byte {i}, reference {a:#04x}, ours {b:#04x}"));
        }
    }
    None
}

/// The gate set genesis pins, read from the file x/svm embeds, so the
/// differential runs the gates the chain runs.
fn genesis_gates() -> Vec<(Pubkey, u64)> {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).parent().unwrap();
    let text = std::fs::read_to_string(root.join("genesis/features.json"))
        .expect("genesis/features.json");
    // The gates genesis activates although the pin has them off, read from the
    // same file x/svm reads. Without this, the oracle would run a different
    // feature set from the chain, which a differential must never do.
    let exceptions = std::fs::read_to_string(root.join("genesis/exceptions.json"))
        .expect("genesis/exceptions.json");
    let exceptions: Vec<&str> = exceptions
        .split("\"id\": \"")
        .skip(1)
        .filter_map(|e| e.split('"').next())
        .collect();
    let mut out = Vec::new();
    let mut found = 0;
    for entry in text.split("{\n   \"id\"").skip(1) {
        let id = entry.split('"').nth(1).expect("id");
        let class = entry.split("\"class\": \"").nth(1).and_then(|s| s.split('"').next());
        let exception = exceptions.contains(&id);
        found += usize::from(exception);
        // A bank-side exception, such as a rent cut, is no SVMFeatureSet field,
        // so the differential has nothing to run it under.
        if class == Some("svm") && (entry.contains("\"active_at_pin\": true") || exception) {
            out.push((id.parse::<Pubkey>().expect("feature id"), 0u64));
        }
    }
    assert!(!out.is_empty(), "no active svm gates parsed from genesis/features.json");
    assert!(
        !exceptions.is_empty() && found == exceptions.len(),
        "a genesis exception is missing from genesis/features.json, so the differential's gate set cannot carry it"
    );
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The whole corpus, on every feature-set variant, every run. The variants
    /// are the chain's own gate set and no gates at all, so a gate-sensitive
    /// difference between the two sides has somewhere to show up.
    #[test]
    fn the_differential_runs_the_whole_corpus() {
        let variants: [(&str, Vec<(Pubkey, u64)>); 2] =
            [("genesis gates", genesis_gates()), ("no gates", Vec::new())];
        let corpus = corpus();
        let mut runs = 0;
        for (variant, gates) in &variants {
            let root = build_root(gates);
            let features = root.feature_set.runtime_features();
            for fixture in &corpus {
                let ctx = context(fixture, features, RENT_PER_BYTE_YEAR);
                if let Some(field) = diff(&reference(&ctx), &ours(&ctx, gates)) {
                    panic!(
                        "differential: {} under {variant}, engine digest {}\n  {field}",
                        fixture.name,
                        crate::ENGINE_DIGEST
                    );
                }
                runs += 1;
            }
        }
        // Every run reports the digest it ran under.
        println!(
            "differential: {} fixtures x {} feature sets = {runs} runs, engine digest {}",
            corpus.len(),
            variants.len(),
            crate::ENGINE_DIGEST
        );
    }

    /// The differential catches a broken sysvar fill. A cache filled with a
    /// Rent this chain does not use makes a fixture that reads Rent diverge,
    /// and the report names the first differing field.
    #[test]
    fn a_broken_sysvar_fill_is_caught() {
        let gates = genesis_gates();
        let features = build_root(&gates).feature_set.runtime_features();
        let fixture = corpus()
            .into_iter()
            .find(|f| f.name == "system initialize nonce account")
            .expect("the fixture that reads Rent");
        let good = context(&fixture, features, RENT_PER_BYTE_YEAR);
        assert_eq!(diff(&reference(&good), &ours(&good, &gates)), None);

        // The engine's fill reads this Rent, and the reference side keeps the
        // chain's. Only the fill differs.
        let broken = context(&fixture, features, RENT_PER_BYTE_YEAR * 1_000);
        let report = diff(&reference(&good), &ours(&broken, &gates))
            .expect("a sysvar fill reading the wrong Rent went unnoticed");
        assert!(report.starts_with("result:"), "the report names {report}, not the first field");

        // A fill that drops a slot never reaches the differential, because the
        // engine's own assertion rejects it first and halts the machine. The
        // harness would panic on the empty slot.
        let root = build_root(&gates);
        let child = root.processor.new_from(SLOT, SLOT / SLOTS_PER_EPOCH);
        let short: HashMap<Pubkey, AccountSharedData> = good
            .accounts
            .iter()
            .filter(|(k, _)| *k != crate::runtime::SYSVAR_IDS[3])
            .map(|(k, a)| (*k, AccountSharedData::from(a.clone())))
            .collect();
        child.reset_and_fill_sysvar_cache_entries(&Arena(short));
        assert!(
            crate::runtime::check_sysvar_slots(&child).is_err(),
            "a fill that dropped the Rent sysvar passed the engine's assertion"
        );
    }

    /// The gate set the differential runs is the one genesis pins, not
    /// `all_enabled`.
    #[test]
    fn the_feature_variant_is_the_genesis_gate_set() {
        let gates = genesis_gates();
        let pinned = build_root(&gates).feature_set.runtime_features();
        assert_ne!(
            bytes(&pinned),
            bytes(&SVMFeatureSet::all_enabled()),
            "the genesis gate set is not every gate"
        );
        assert_ne!(
            bytes(&pinned),
            bytes(&build_root(&[]).feature_set.runtime_features()),
            "the genesis gate set activates nothing"
        );
    }

    /// SVMFeatureSet is a struct of bools: size 1, align 1, no padding.
    fn bytes(set: &SVMFeatureSet) -> Vec<u8> {
        unsafe {
            std::slice::from_raw_parts(
                set as *const SVMFeatureSet as *const u8,
                std::mem::size_of::<SVMFeatureSet>(),
            )
        }
        .to_vec()
    }
}
