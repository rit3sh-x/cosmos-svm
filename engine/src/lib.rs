//! FFI boundary between the Cosmos SDK application (Go) and the SVM execution
//! engine (Rust).
//!
//! Two invariants:
//!
//! 1. Rust never writes Cosmos state. It returns proposed changes, and Go
//!    commits them.
//! 2. No panic crosses this boundary. Unwinding into C is undefined behaviour,
//!    and a crash on one bad transaction is a liveness failure. Every
//!    `extern "C"` function that can panic wraps its body in `catch_unwind`.
//!    The other functions only return a constant, load or store an atomic, or
//!    free a buffer that the engine gave out.

pub mod abi;
pub mod block;
pub mod check;
#[cfg(test)]
mod conformance;
pub mod cost;
pub mod decode;
pub mod digest;
pub mod priority;
pub mod runtime;
pub mod programs;
pub mod resolve;
pub mod schedule;
pub mod signatures;
pub mod stats;

use std::ffi::{c_char, c_int, CString};
use std::panic::{catch_unwind, AssertUnwindSafe};

pub const SVM_OK: c_int = 0;
/// The engine panicked and the unwind was contained. The caller must halt the
/// process and must not synthesize a transaction result. A panic reports a host
/// condition, which can differ between validators on the same input. A
/// `TransactionError` made from it forks the chain.
pub const SVM_ERR_PANIC: c_int = -1;
/// The handle does not name the open block. A stale plan, or none open.
pub const SVM_ERR_BAD_HANDLE: c_int = -2;
/// The request arena is malformed, or its transactions do not deserialise.
pub const SVM_ERR_BAD_INPUT: c_int = -3;
/// A block is already open. The Go driver failed to release the previous one.
pub const SVM_ERR_BUSY: c_int = -4;
/// A sysvar cache slot was empty after the fill. The response arena's one record
/// is the slot's name. The caller must halt the process. Go's sysvar writer or
/// genesis is broken, and every validator stops at the same height.
pub const SVM_ERR_SYSVAR: c_int = -5;
/// No feature set is configured with `svm_engine_configure`. The caller must
/// halt, because Go's startup skipped that call, not because the block is bad.
pub const SVM_ERR_UNCONFIGURED: c_int = -6;

/// ENGINE_DIGEST is the digest of the build inputs. build.rs computes it at
/// build time. Both enforcement points read this one constant. Genesis declares
/// it, and an upgrade handler that changes the engine must write it.
pub const ENGINE_DIGEST: &str = env!("SVM_ENGINE_DIGEST");

/// Writes the 32 raw bytes of `ENGINE_DIGEST` to a caller-owned buffer. The
/// startup assertion and InitGenesis compare these bytes. The function zeroes
/// the buffer first. A contained panic leaves an all-zero digest, which no
/// genesis declares, so the node refuses to start instead of aborting.
///
/// # Safety
/// `out` must point to 32 writable bytes.
#[no_mangle]
pub unsafe extern "C" fn svm_engine_digest(out: *mut u8) {
    let bytes = std::slice::from_raw_parts_mut(out, 32);
    bytes.fill(0);
    let _ = catch_unwind(AssertUnwindSafe(|| {
        for (i, byte) in bytes.iter_mut().enumerate() {
            let hex = &ENGINE_DIGEST[2 * i..2 * i + 2];
            *byte = u8::from_str_radix(hex, 16).expect("build.rs writes lower-case hex");
        }
    }));
}

/// The cheapest possible call. It measures the cost of one crossing.
#[no_mangle]
pub extern "C" fn svm_engine_noop() -> c_int {
    SVM_OK
}

/// Returns a string that proves solana-svm and solana-sbpf are linked and
/// callable. Returns null on failure. The caller must free it with
/// `svm_engine_string_free`.
#[no_mangle]
pub extern "C" fn svm_engine_version() -> *mut c_char {
    let result = catch_unwind(|| {
        // Touch a type from each crate so the linker has to resolve both.
        let vm_config = solana_sbpf::vm::Config::default();
        let processing_config =
            solana_svm::transaction_processor::TransactionProcessingConfig::default();
        format!(
            "svm-engine ok | sbpf max_call_depth={} | svm limit_to_load_programs={} | digest={}",
            vm_config.max_call_depth, processing_config.limit_to_load_programs, ENGINE_DIGEST
        )
    });

    match result {
        Ok(s) => CString::new(s)
            .map(CString::into_raw)
            .unwrap_or(std::ptr::null_mut()),
        Err(_) => std::ptr::null_mut(),
    }
}

/// Frees a string returned by this library. Safe to call with null.
///
/// # Safety
/// `ptr` must be null or a pointer previously returned by this library and not
/// yet freed.
#[no_mangle]
pub unsafe extern "C" fn svm_engine_string_free(ptr: *mut c_char) {
    if !ptr.is_null() {
        drop(CString::from_raw(ptr));
    }
}

/// The reason `wincode::deserialize` gives for bytes that are not a
/// `VersionedTransaction` (kind 0) or a `VersionedMessage` (kind 1). Agave's
/// RPC quotes it after "failed to deserialize {type}: ". Null when the bytes
/// deserialize. The caller frees it with `svm_engine_string_free`.
///
/// # Safety
/// `bytes` must point to `len` readable bytes.
#[no_mangle]
pub unsafe extern "C" fn svm_deserialize_error(kind: u8, bytes: *const u8, len: usize) -> *mut c_char {
    let bytes = req_slice(bytes, len);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let reason = match kind {
            0 => wincode::deserialize::<solana_transaction::versioned::VersionedTransaction>(bytes).err(),
            _ => wincode::deserialize::<solana_message::VersionedMessage>(bytes).err(),
        };
        reason.and_then(|e| CString::new(e.to_string()).ok())
    }));
    match result {
        Ok(Some(s)) => s.into_raw(),
        _ => std::ptr::null_mut(),
    }
}

/// Panics on purpose and returns `SVM_ERR_PANIC`. Tests call it to prove that
/// the boundary contains a panic and does not abort the process.
#[no_mangle]
pub extern "C" fn svm_engine_panic_test() -> c_int {
    match catch_unwind(|| -> c_int {
        panic!("intentional panic, exercising FFI unwind containment");
    }) {
        Ok(code) => code,
        Err(_) => SVM_ERR_PANIC,
    }
}

/// Reads a caller-owned request arena. Null or zero-length is an empty slice
/// rather than an error, so an empty block is not a special case.
///
/// # Safety
/// `req` must be null or point to `req_len` readable bytes for this call.
unsafe fn req_slice<'a>(req: *const u8, req_len: usize) -> &'a [u8] {
    if req.is_null() || req_len == 0 {
        &[]
    } else {
        std::slice::from_raw_parts(req, req_len)
    }
}

/// Hands a response arena to the caller.
fn emit(buf: Box<[u8]>, out: *mut *mut u8, out_len: *mut usize) {
    let len = buf.len();
    let ptr = Box::into_raw(buf) as *mut u8;
    unsafe {
        *out = ptr;
        *out_len = len;
    }
}

/// Phase 1. Parses the block's transactions, verifies them, and returns every
/// account key that must be loaded before execution. Each request record is
/// one transaction as `PushSized` writes it, so admission sees its exact bytes.
///
/// The response's `aux` field is the number of leading records that are address
/// lookup table accounts. Zero means the block is legacy-only, and Go skips
/// phase 2.
///
/// On success `out_handle` names the open block until `svm_block_release`.
///
/// # Safety
/// `blockhash` must point to 32 readable bytes. `req` must point to `req_len`
/// readable bytes. The three out-pointers must be non-null and writable.
#[no_mangle]
pub unsafe extern "C" fn svm_block_plan(
    height: u64,
    blockhash: *const u8,
    req: *const u8,
    req_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
    out_handle: *mut u64,
) -> c_int {
    let bytes = req_slice(req, req_len);
    if blockhash.is_null() {
        return SVM_ERR_BAD_INPUT;
    }
    let blockhash: [u8; 32] = *(blockhash as *const [u8; 32]);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let reader = match abi::Reader::new(bytes, abi::KIND_PLAN_REQ) {
            Ok(r) => r,
            Err(_) => return SVM_ERR_BAD_INPUT,
        };
        let blk = match block::plan(height, blockhash, &reader) {
            Ok(b) => b,
            Err(_) => return SVM_ERR_BAD_INPUT,
        };

        // Records: lookup table keys (aux of them), the load list, then one
        // 64-byte dedup key per transaction, in block order.
        let list = blk.load_list();
        let n = blk.alt_keys.len() + list.len() + blk.dedup_keys.len();
        let mut w = abi::Writer::with_capacity(abi::KIND_PLAN_RESP, 0, n, n * 64);
        w.set_aux(blk.alt_keys.len() as u32);
        for k in &blk.alt_keys {
            w.push(k);
        }
        for k in &list {
            w.push(k);
        }
        for k in &blk.dedup_keys {
            w.push(k);
        }

        let handle = match block::open(blk) {
            Ok(h) => h,
            Err(_) => return SVM_ERR_BUSY,
        };
        // The handle is in the response header too, so a buffer paired with the
        // wrong handle is detected, not silently accepted.
        w.set_handle(handle);
        emit(w.finish(), out, out_len);
        *out_handle = handle;
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Phase 2. Expands address lookup tables against the table accounts Go loaded.
/// Returns the whole load list again, now with the resolved addresses in their
/// first-use places (`Block::load_list`).
///
/// Go skips this call when phase 1 reports `aux == 0`.
///
/// # Safety
/// As `svm_block_plan`.
#[no_mangle]
pub unsafe extern "C" fn svm_block_resolve(
    handle: u64,
    req: *const u8,
    req_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let bytes = req_slice(req, req_len);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let reader = match abi::Reader::new(bytes, abi::KIND_RESOLVE_REQ) {
            Ok(r) => r,
            Err(_) => return SVM_ERR_BAD_INPUT,
        };
        let produced = block::with(handle, |blk| resolve::resolve(handle, blk, &reader));
        let produced = match produced {
            Ok(Ok(buf)) => Ok(buf),
            Ok(Err(resolve::ResolveError::NoSlotHashes)) => return SVM_ERR_BAD_INPUT,
            Ok(Err(resolve::ResolveError::BadInput)) => return SVM_ERR_BAD_INPUT,
            Err(()) => Err(()),
        };
        match produced {
            Ok(buf) => {
                emit(buf, out, out_len);
                SVM_OK
            }
            Err(_) => SVM_ERR_BAD_HANDLE,
        }
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Phase 3. Takes every account the block touches and returns per-transaction
/// results plus proposed write sets. Rust never writes Cosmos state.
///
/// The response's `aux` field is the number of leading records that are
/// transaction results. The rest are proposed account writes.
///
/// # Safety
/// As `svm_block_plan`, and every account record must point at a value Go
/// keeps pinned for this call.
#[no_mangle]
pub unsafe extern "C" fn svm_block_execute(
    handle: u64,
    req: *const u8,
    req_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let bytes = req_slice(req, req_len);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let reader = match abi::Reader::new(bytes, abi::KIND_EXECUTE_REQ) {
            Ok(r) => r,
            Err(_) => return SVM_ERR_BAD_INPUT,
        };
        let produced = block::with(handle, |blk| schedule::execute(handle, blk, &reader));
        match produced {
            Ok(Ok(buf)) => {
                emit(buf, out, out_len);
                SVM_OK
            }
            Ok(Err(schedule::ExecuteError::BadInput)) => SVM_ERR_BAD_INPUT,
            Ok(Err(schedule::ExecuteError::Unconfigured)) => SVM_ERR_UNCONFIGURED,
            Ok(Err(schedule::ExecuteError::MissingSysvar(name))) => {
                let mut w = abi::Writer::new(abi::KIND_EXECUTE_RESP, handle);
                w.push(name.as_bytes());
                emit(w.finish(), out, out_len);
                SVM_ERR_SYSVAR
            }
            Err(_) => SVM_ERR_BAD_HANDLE,
        }
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Configures the engine's feature set. Each record is a 32-byte feature
/// pubkey and its activation height, u64 little-endian, as the `active/` keys
/// of `svmmeta` hold them. Go calls it at startup and after InitGenesis,
/// before any block executes.
///
/// # Safety
/// `req` must point to `req_len` readable bytes.
#[no_mangle]
pub unsafe extern "C" fn svm_engine_configure(req: *const u8, req_len: usize) -> c_int {
    let bytes = req_slice(req, req_len);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let Some(active) = gate_records(bytes) else {
            return SVM_ERR_BAD_INPUT;
        };
        runtime::configure(&active);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Names the gate set of the block at `height`, the block after the committed
/// one. Simulations of that block run under it. The request has the layout of
/// `svm_engine_configure`'s. Go calls it once per committed height, when it
/// builds the next block's context.
///
/// # Safety
/// `req` must point to `req_len` readable bytes.
#[no_mangle]
pub unsafe extern "C" fn svm_engine_configure_next(height: u64, req: *const u8, req_len: usize) -> c_int {
    let bytes = req_slice(req, req_len);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let Some(active) = gate_records(bytes) else {
            return SVM_ERR_BAD_INPUT;
        };
        runtime::configure_next(height, &active);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Parses the gate records of a configure request. None for a malformed
/// request.
fn gate_records(bytes: &[u8]) -> Option<Vec<(solana_pubkey::Pubkey, u64)>> {
    let reader = abi::Reader::new(bytes, abi::KIND_CONFIGURE_REQ).ok()?;
    let mut active = Vec::with_capacity(reader.len());
    for rec in reader.iter() {
        if rec.len() < 40 {
            return None;
        }
        let id = solana_pubkey::Pubkey::new_from_array(rec[..32].try_into().unwrap());
        active.push((id, u64::from_le_bytes(rec[32..40].try_into().unwrap())));
    }
    Some(active)
}

/// Sets how many worker threads run a block's conflict groups. Zero means the
/// machine's available parallelism.
///
/// # Safety
/// Safe to call from any thread, but Go calls it once at startup, before any
/// block executes.
#[no_mangle]
pub extern "C" fn svm_engine_threads(threads: usize) {
    runtime::set_threads(threads);
}

/// Sets the program cache's byte budget. Zero means unbounded. The default is
/// 1.5 GiB of resident bytes. The budget is machine-local. Cache size is not
/// consensus, so an operator with more memory may raise it.
///
/// # Safety
/// Safe to call from any thread, but Go calls it once at startup, before any
/// block executes.
#[no_mangle]
pub extern "C" fn svm_engine_cache_bytes(bytes: u64) {
    programs::CACHE_BYTES.store(bytes, std::sync::atomic::Ordering::Relaxed);
}

/// Changes the block's program text budget, for tests only. `svmd` never calls
/// it. The budget is consensus, and without this call a test needs 400 MB of
/// program accounts to reach it.
#[no_mangle]
pub extern "C" fn svm_block_program_text_for_tests(bytes: u64) {
    programs::set_max_block_program_text_for_tests(bytes);
}

/// Forgets the configured feature set, like a process that has not configured
/// the engine yet. Tests only. The engine is process-global. Without this,
/// a test of a machine that joins by state sync inherits the configuration of
/// the chain that served the snapshot.
#[no_mangle]
pub extern "C" fn svm_engine_unconfigure_for_tests() {
    let _ = catch_unwind(runtime::unconfigure);
}

/// The bytes the program cache counts as held, which the budget bounds. This
/// is what its entries keep resident: the pc tables and emitted code of the
/// JIT, and the ELF and rodata copies of the executables. It is not the
/// power-of-two buckets that the JIT reserves. The count includes the caches
/// of the roots of the next blocks, and it counts each entry once. Zero before
/// the engine is configured, and zero if the read panics (a poisoned lock),
/// because a metric must not abort the process.
#[no_mangle]
pub extern "C" fn svm_program_cache_bytes() -> u64 {
    catch_unwind(programs::held_bytes).unwrap_or(0)
}

/// The machine-local counters: program cache, pre-warm misses, and the last
/// block's conflict groups and program text, packed as `stats::FIELDS`
/// little-endian u64 in one record. Go calls it after a block is committed.
///
/// It has its own arena kind and its own response, and returns nothing a
/// block's result depends on.
///
/// # Safety
/// The out-pointers must be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_engine_stats(out: *mut *mut u8, out_len: *mut usize) -> c_int {
    let result = catch_unwind(AssertUnwindSafe(|| {
        let mut record = Vec::with_capacity(stats::FIELDS * 8);
        for value in stats::snapshot() {
            record.extend_from_slice(&value.to_le_bytes());
        }
        let mut w = abi::Writer::new(abi::KIND_STATS_RESP, 0);
        w.push(&record);
        emit(w.finish(), out, out_len);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Admission for `CheckTx` and `ProcessProposal`. `mode` 0 verifies signatures
/// in full, and 1 hashes only. Mode 2 is for `CheckTx` New. It verifies in
/// full, and a check that passes puts the transaction's signature-result key
/// in the pending slot for `svm_record_verified`. The call is pure and
/// handle-free, except for that slot. It admits for the block at `height`,
/// under the root that `svm_engine_configure_next` named for it.
///
/// The response's record 0 is status u32 (0 admitted, 1 rejected), a durable
/// nonce flag u32, units u64, allocated bytes u64, the 64-byte dedup key, the
/// total fee u64, the fee payer, the nonce account or 32 zero bytes, then the
/// signature-result key, which is 32 zero bytes unless `mode` is 2. When
/// admitted, the arena's aux is the number of lookup tables the message names.
/// Records 1 onward are those tables in message order, and the records after
/// them are the accounts it writes. A message that names a table has no
/// written accounts here, because `svm_resolved_admission` counts it for the
/// block. When rejected, record 1 is the error.
///
/// # Safety
/// `tx` must point to `tx_len` readable bytes. The out-pointers must be
/// writable.
#[no_mangle]
pub unsafe extern "C" fn svm_check_tx(
    height: u64,
    tx: *const u8,
    tx_len: usize,
    mode: u8,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let bytes = req_slice(tx, tx_len);
    let (mode, keep) = match mode {
        0 => (check::Mode::Full, false),
        1 => (check::Mode::HashOnly, false),
        2 => (check::Mode::Full, true),
        _ => return SVM_ERR_BAD_INPUT,
    };
    let result = catch_unwind(AssertUnwindSafe(|| {
        let Some(root) = runtime::next_root(height) else {
            return SVM_ERR_UNCONFIGURED;
        };
        if keep {
            signatures::set_pending(None);
        }
        let mut head = Vec::with_capacity(192);
        let mut w = abi::Writer::new(abi::KIND_CHECK_RESP, 0);
        match check::check_tx(&root, bytes, mode) {
            Ok(a) => {
                let key = keep.then(|| signatures::key(bytes));
                if keep {
                    signatures::set_pending(key);
                }
                head.extend_from_slice(&0u32.to_le_bytes());
                head.extend_from_slice(&(a.nonce_account.is_some() as u32).to_le_bytes());
                head.extend_from_slice(&a.cost.units.to_le_bytes());
                head.extend_from_slice(&a.cost.allocated_bytes.to_le_bytes());
                head.extend_from_slice(&a.dedup_key);
                head.extend_from_slice(&a.total_fee.to_le_bytes());
                head.extend_from_slice(a.fee_payer.as_ref());
                head.extend_from_slice(a.nonce_account.unwrap_or_default().as_ref());
                head.extend_from_slice(&key.unwrap_or_default());
                w.push(&head);
                w.set_aux(a.tables.len() as u32);
                for key in a.tables.iter().chain(&a.writable) {
                    w.push(key.as_ref());
                }
            }
            Err(check::CheckError::Transaction(e)) => {
                head.extend_from_slice(&1u32.to_le_bytes());
                head.resize(192, 0);
                w.push(&head);
                w.push(format!("{e:?}").as_bytes());
            }
        }
        emit(w.finish(), out, out_len);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Keeps the result of the `CheckTx` New check that just passed, after Go's own
/// checks pass too. It keeps `key` only when the pending slot holds it, and it
/// empties the slot in both cases. So Go cannot record a key that the engine
/// did not just make. Returns 1 when the slot held `key`, else 0.
///
/// # Safety
/// `key` must point to 32 readable bytes.
#[no_mangle]
pub unsafe extern "C" fn svm_record_verified(key: *const u8) -> c_int {
    if key.is_null() {
        return SVM_ERR_BAD_INPUT;
    }
    let key: signatures::Key = *(key as *const [u8; 32]);
    catch_unwind(|| signatures::record_verified(&key) as c_int).unwrap_or(SVM_ERR_PANIC)
}

/// The cap of the pool tier of signature results. svmd sets it to two times
/// CometBFT's `mempool.size`. It is machine-local, because the store changes
/// no answer.
#[no_mangle]
pub extern "C" fn svm_signature_results_cap(entries: usize) {
    let _ = catch_unwind(|| signatures::set_cap(entries));
}

/// `ProcessProposal`'s signature check, after the cheap checks. Each record
/// of `req` is one SVM transaction of the proposal, in block order, as its
/// address and length, u64 each. Go pins it for the call
/// (`abi::pinned_bytes`). The engine hashes every transaction and looks each
/// key up in both tiers. It checks the misses until the first one whose
/// signatures fail. It keeps the misses that passed in the block tier, and
/// the hits too when no check failed. `out_failed` is the index of the first
/// transaction whose signatures fail, or `u64::MAX` when none does.
///
/// # Safety
/// `req` must point to `req_len` readable bytes. Each record's address must
/// point to its length in bytes that stay readable for the call. `out_failed`
/// must be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_proposal_signatures(req: *const u8, req_len: usize, out_failed: *mut u64) -> c_int {
    let bytes = req_slice(req, req_len);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let Ok(reader) = abi::Reader::new(bytes, abi::KIND_PROPOSAL_REQ) else {
            return SVM_ERR_BAD_INPUT;
        };
        let Some(txs) = (0..reader.len()).map(|i| abi::pinned_bytes(reader.record(i))).collect::<Option<Vec<_>>>() else {
            return SVM_ERR_BAD_INPUT;
        };
        *out_failed = signatures::check_proposal(&txs).map_or(u64::MAX, |i| i as u64);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// The height's `FinalizeBlock` is over, so the store forgets the block tier.
/// Go calls it at the end of every `FinalizeBlock`, with or without SVM
/// transactions.
#[no_mangle]
pub extern "C" fn svm_signature_results_end_height() {
    let _ = catch_unwind(signatures::end_height);
}

/// Phase 1's drops in the open block. There is one record for each
/// transaction that admission refused, in block order: the index u32, then
/// the engine's error. Go counts and logs them. Nothing here reaches a result.
///
/// # Safety
/// The out-pointers must be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_block_dropped(handle: u64, out: *mut *mut u8, out_len: *mut usize) -> c_int {
    let result = catch_unwind(AssertUnwindSafe(|| {
        let Ok(dropped) = block::with(handle, |blk| blk.dropped.clone()) else {
            return SVM_ERR_BAD_HANDLE;
        };
        let mut w = abi::Writer::new(abi::KIND_DROPPED_RESP, handle);
        for (index, error) in dropped {
            let mut record = index.to_le_bytes().to_vec();
            record.extend_from_slice(error.as_bytes());
            w.push(&record);
        }
        emit(w.finish(), out, out_len);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Empties the store of signature results, as a restart does. Tests only.
/// The engine is process-global. A test of three machines empties the one
/// store between them to model the store of each machine.
#[no_mangle]
pub extern "C" fn svm_signature_results_clear_for_tests() {
    let _ = catch_unwind(signatures::clear_for_tests);
}

/// The state half of admission: whether the block at `height` would process
/// `tx`, which `svm_check_tx` admitted. It checks against the accounts Go read
/// from that block's context, under that block's root. Pure and handle-free,
/// like `svm_check_tx`.
///
/// Record 0 of `req` is the Rent sysvar's data. Record 1 is the fee payer as an
/// account record, empty when the account does not exist. `aux` 1 means the
/// blockhash is past the window. Then record 2 is the blockhash, and record 3
/// is the nonce account, empty when it does not exist. `tables` is empty when
/// the message names no lookup table. Otherwise it is phase 2's request for
/// the block at `height`: the `SlotHashes` data that block will hold, then the
/// table accounts. A table that does not exist is absent.
///
/// The response's record 0 is status u32: 0 admissible, 1 the fee payer
/// cannot pay, 2 the nonce does not verify, 3 the message does not resolve.
/// When it is not 0, record 1 is the error.
///
/// # Safety
/// `tx` must point to `tx_len` readable bytes, `req` to `req_len` and `tables`
/// to `tables_len`. The out-pointers must be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_check_committed(
    height: u64,
    fee: u64,
    tx: *const u8,
    tx_len: usize,
    req: *const u8,
    req_len: usize,
    tables: *const u8,
    tables_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let (tx, bytes, tables) = (req_slice(tx, tx_len), req_slice(req, req_len), req_slice(tables, tables_len));
    let result = catch_unwind(AssertUnwindSafe(|| {
        let Ok(reader) = abi::Reader::new(bytes, abi::KIND_COMMITTED_REQ) else {
            return SVM_ERR_BAD_INPUT;
        };
        let nonce = reader.aux == 1;
        if reader.len() != if nonce { 4 } else { 2 } {
            return SVM_ERR_BAD_INPUT;
        }
        let Ok(rent) = bincode::deserialize::<solana_rent::Rent>(reader.record(0)) else {
            return SVM_ERR_BAD_INPUT;
        };
        let account = |i: usize| -> Result<Option<_>, ()> {
            match reader.record(i) {
                [] => Ok(None),
                rec => abi::AccountRef::parse(rec).map(|a| Some(runtime::shared(&a))).ok_or(()),
            }
        };
        let Ok(payer) = account(1) else {
            return SVM_ERR_BAD_INPUT;
        };
        let nonce = if nonce {
            let Ok(blockhash) = <[u8; 32]>::try_from(reader.record(2)) else {
                return SVM_ERR_BAD_INPUT;
            };
            let Ok(account) = account(3) else {
                return SVM_ERR_BAD_INPUT;
            };
            Some((solana_hash::Hash::new_from_array(blockhash), account))
        } else {
            None
        };
        let tables = if tables.is_empty() {
            None
        } else {
            match abi::Reader::new(tables, abi::KIND_RESOLVE_REQ).map(|r| resolve::tables(&r)) {
                Ok(Ok(tables)) => Some((height, tables)),
                _ => return SVM_ERR_BAD_INPUT,
            }
        };
        let Some(root) = runtime::next_root(height) else {
            return SVM_ERR_UNCONFIGURED;
        };
        let refused = match check::check_committed(&root, tx, fee, check::Committed { rent, payer, nonce, tables }) {
            Ok(()) => None,
            Err(check::CommittedError::Payer(e)) => Some((1u32, e)),
            Err(check::CommittedError::Nonce(e)) => Some((2, e)),
            Err(check::CommittedError::Message(e)) => Some((3, e)),
        };
        let mut w = abi::Writer::new(abi::KIND_CHECK_RESP, 0);
        match refused {
            None => w.push(&0u32.to_le_bytes()),
            Some((status, e)) => {
                w.push(&status.to_le_bytes());
                w.push(format!("{e:?}").as_bytes());
            }
        }
        emit(w.finish(), out, out_len);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// The engine block limits both proposal handlers enforce: `MAX_BLOCK_UNITS`,
/// `MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA` and the block program text budget. The
/// limit for each account is `svm_max_writable_account_units`.
///
/// # Safety
/// All three pointers must be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_block_limits(
    units: *mut u64,
    allocated_bytes: *mut u64,
    program_text: *mut u64,
) {
    *units = cost::MAX_BLOCK_UNITS;
    *allocated_bytes = cost::MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA;
    *program_text = programs::max_block_program_text();
}

/// `MAX_WRITABLE_ACCOUNT_UNITS`, the per-account limit both proposal handlers
/// enforce.
#[no_mangle]
pub extern "C" fn svm_max_writable_account_units() -> u64 {
    cost::MAX_WRITABLE_ACCOUNT_UNITS
}

/// How many transactions admission has verified signatures for.
#[no_mangle]
pub extern "C" fn svm_signature_verifications() -> u64 {
    check::SIGNATURE_VERIFICATIONS.load(std::sync::atomic::Ordering::Relaxed)
}

/// A transaction's load list for `simulateTransaction`, and its own static
/// keys, one 32-byte record each. Handle-free.
///
/// `aux` is the number of leading records that are the load list: the nine
/// sysvar ids, the fee payer and its nonce account, then the other static keys
/// in message order (`runtime::transaction_keys`). The records after them are
/// the static keys alone, in message order. The history's address index uses
/// them. One call and one parse give both answers.
///
/// # Safety
/// `tx` must point to `tx_len` readable bytes. The out-pointers must be
/// writable.
#[no_mangle]
pub unsafe extern "C" fn svm_transaction_keys(
    tx: *const u8,
    tx_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let bytes = req_slice(tx, tx_len);
    let result = catch_unwind(AssertUnwindSafe(|| match runtime::transaction_keys(bytes) {
        Ok((keys, statics)) => {
            let n = keys.len() + statics.len();
            let mut w = abi::Writer::with_capacity(abi::KIND_KEYS_RESP, 0, n, n * 32);
            w.set_aux(keys.len() as u32);
            for k in keys.iter().chain(statics.iter()) {
                w.push(k);
            }
            emit(w.finish(), out, out_len);
            SVM_OK
        }
        Err(()) => SVM_ERR_BAD_INPUT,
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// `simulateTransaction`. Handle-free: it runs on its own child processor and
/// never touches an open block. It runs as the next block, `height`, on the
/// root that `svm_engine_configure_next` named for it. `blockhash` is the
/// committed block's hash, and `own` stands for the next block's own hash.
/// Record 0 of the request is the transaction, and the accounts follow as
/// pinned records. The record before the last holds each key that Go's 64 MiB
/// cut dropped, 32 bytes each, and is empty when the cut dropped nothing. The
/// last record holds the keys of the accounts that the response carries after
/// the run, 32 bytes each, and is empty for none.
///
/// Flags: bit 0 verifies signatures, bit 1 replaces the recent blockhash with
/// `blockhash`, and bit 2 says the replay set holds the transaction.
///
/// Response record 0 holds the status (0 success, 1 failed, 2 not processed),
/// loaded account data size, compute units, fee, the rendered transaction
/// error and its display text, and the resolved writable and readonly
/// lookup-table addresses. Then come the lamport balances before and after
/// (when collected), the token balances before and after, the inner
/// instructions (if executed), and the accounts that the request named, after
/// execution, in message order. Record 1 holds return data (program ID,
/// length, data), or is empty. The other records hold log lines, each
/// length-prefixed. Errors stay in record 0, so return data stays at index 1.
/// `schedule::simulation_response` builds the response.
///
/// # Safety
/// `blockhash` and `own` must point to 32 readable bytes and `req` to
/// `req_len`. The out-pointers must be writable. Every account record must
/// point at a value Go keeps pinned for this call.
#[no_mangle]
#[allow(clippy::too_many_arguments)]
pub unsafe extern "C" fn svm_simulate(
    height: u64,
    blockhash: *const u8,
    own: *const u8,
    flags: u8,
    req: *const u8,
    req_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let bytes = req_slice(req, req_len);
    if blockhash.is_null() || own.is_null() {
        return SVM_ERR_BAD_INPUT;
    }
    let blockhash: [u8; 32] = *(blockhash as *const [u8; 32]);
    let own: [u8; 32] = *(own as *const [u8; 32]);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let reader = match abi::Reader::new(bytes, abi::KIND_SIMULATE_REQ) {
            Ok(r) if r.len() >= 3 => r,
            _ => return SVM_ERR_BAD_INPUT,
        };
        let n = reader.len();
        let (Some(dropped), Some(wanted)) =
            (abi::pubkeys(reader.record(n - 2)), abi::pubkeys(reader.record(n - 1)))
        else {
            return SVM_ERR_BAD_INPUT;
        };
        let dropped: std::collections::HashSet<solana_pubkey::Pubkey> =
            dropped.into_iter().collect();
        let Ok(accounts) = runtime::arena_accounts(&reader, 1..n - 2) else {
            return SVM_ERR_BAD_INPUT;
        };
        let mut sim = match runtime::simulate(height, blockhash, own, reader.record(0), flags & 1 != 0, flags & 2 != 0, flags & 4 != 0, accounts, &dropped) {
            Ok(sim) => sim,
            Err(runtime::ExecError::Unconfigured) => return SVM_ERR_UNCONFIGURED,
            Err(runtime::ExecError::MissingSysvar(_)) => return SVM_ERR_SYSVAR,
        };
        // The Agave bank keeps every account after the run, and the Agave RPC
        // reads only the accounts that the config names. So only those
        // accounts are copied out.
        let wanted: std::collections::HashSet<solana_pubkey::Pubkey> = wanted.into_iter().collect();
        sim.post_accounts.retain(|(key, _)| wanted.contains(key));
        emit(schedule::simulation_response(sim), out, out_len);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Decodes a stored transaction and the addresses its lookups resolved to into
/// one record in the layout of `decode::decode`. `loaded` holds
/// a little-endian u32 count of writable addresses, then the writable
/// addresses, then the readonly ones, 32 bytes each. Handle-free and
/// machine-local.
///
/// # Safety
/// `tx` must point to `tx_len` readable bytes and `loaded` to `loaded_len`.
/// The out-pointers must be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_decode_transaction(
    tx: *const u8,
    tx_len: usize,
    loaded: *const u8,
    loaded_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let bytes = req_slice(tx, tx_len);
    let loaded = req_slice(loaded, loaded_len);
    let result = catch_unwind(AssertUnwindSafe(|| {
        if loaded.len() < 4 {
            return SVM_ERR_BAD_INPUT;
        }
        let Some(keys) = abi::pubkeys(&loaded[4..]) else {
            return SVM_ERR_BAD_INPUT;
        };
        let writable = u32::from_le_bytes(loaded[..4].try_into().unwrap()) as usize;
        if writable > keys.len() {
            return SVM_ERR_BAD_INPUT;
        }
        let addresses = solana_message::v0::LoadedAddresses {
            writable: keys[..writable].to_vec(),
            readonly: keys[writable..].to_vec(),
        };
        match decode::decode(bytes, &addresses) {
            Some(record) => {
                let mut w = abi::Writer::new(abi::KIND_DECODE_RESP, 0);
                w.push(&record);
                emit(w.finish(), out, out_len);
                SVM_OK
            }
            None => SVM_ERR_BAD_INPUT,
        }
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// `getVersion`'s `feature-set`: the first four bytes of
/// `agave_feature_set::ID`, little-endian, as Agave's `solana_version::Version`
/// reports it. It names the gates this build knows, not the ones a chain
/// activated.
#[no_mangle]
pub extern "C" fn svm_feature_set_id() -> u32 {
    u32::from_le_bytes(agave_feature_set::ID.as_ref()[..4].try_into().unwrap())
}

/// The lookup tables a transaction names, one 32-byte record each, in message
/// order, so `simulateTransaction` knows which accounts to read for
/// `svm_lookup_addresses`. Handle-free.
///
/// # Safety
/// `tx` must point to `tx_len` readable bytes. The out-pointers must be
/// writable.
#[no_mangle]
pub unsafe extern "C" fn svm_lookup_tables(
    tx: *const u8,
    tx_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let bytes = req_slice(tx, tx_len);
    let result = catch_unwind(AssertUnwindSafe(|| {
        let Ok(tx) = wincode::deserialize::<solana_transaction::versioned::VersionedTransaction>(bytes) else {
            return SVM_ERR_BAD_INPUT;
        };
        let lookups = tx.message.address_table_lookups().unwrap_or(&[]);
        let mut w = abi::Writer::with_capacity(abi::KIND_KEYS_RESP, 0, lookups.len(), lookups.len() * 32);
        for lookup in lookups {
            w.push(lookup.account_key.as_ref());
        }
        emit(w.finish(), out, out_len);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// The addresses a transaction's lookup tables resolve to at `height`.
/// `simulateTransaction` loads them beside the message's own keys.
/// Handle-free. The request has phase 2's layout: record 0 is the `SlotHashes`
/// account's data, and records 1 onward are the table accounts Go read. A table
/// that does not exist is absent.
///
/// The response's record 0 is status u32 (0 resolved, 1 refused) and the
/// count of writable addresses u32. When resolved, the writable and then the
/// readonly addresses follow, one 32-byte record each. When refused, record 1
/// is the error's display text. Agave's simulation answers that error as
/// invalid params.
///
/// # Safety
/// `tx` must point to `tx_len` readable bytes and `req` to `req_len`. The
/// out-pointers must be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_lookup_addresses(
    height: u64,
    tx: *const u8,
    tx_len: usize,
    req: *const u8,
    req_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let (tx, req) = (req_slice(tx, tx_len), req_slice(req, req_len));
    let result = catch_unwind(AssertUnwindSafe(|| {
        let Ok(tx) = wincode::deserialize::<solana_transaction::versioned::VersionedTransaction>(tx) else {
            return SVM_ERR_BAD_INPUT;
        };
        let Ok(reader) = abi::Reader::new(req, abi::KIND_RESOLVE_REQ) else {
            return SVM_ERR_BAD_INPUT;
        };
        let Ok(tables) = resolve::tables(&reader) else {
            return SVM_ERR_BAD_INPUT;
        };
        let lookups = tx.message.address_table_lookups().unwrap_or(&[]);
        let mut w = abi::Writer::new(abi::KIND_CHECK_RESP, 0);
        match resolve::load_addresses(&tables, height, lookups) {
            Ok(loaded) => {
                w.push(&[0u32.to_le_bytes(), (loaded.writable.len() as u32).to_le_bytes()].concat());
                for key in loaded.writable.iter().chain(loaded.readonly.iter()) {
                    w.push(key.as_ref());
                }
            }
            Err(e) => {
                w.push(&[1u32.to_le_bytes(), 0u32.to_le_bytes()].concat());
                w.push(solana_transaction_error::TransactionError::from(e).to_string().as_bytes());
            }
        }
        emit(w.finish(), out, out_len);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// A message that names lookup tables, counted as the block at `height` will
/// count it: its cost over the resolved message, and the accounts it writes.
/// `svm_check_tx` answers both for every other message. Handle-free. The
/// request is phase 2's request for that block, as for `svm_lookup_addresses`.
///
/// The response's record 0 is status u32 (0 resolved, 1 refused). When
/// resolved, record 0 continues with units u64 and allocated bytes u64, and one
/// 32-byte record follows per account written. When refused, record 1 is the
/// error: the tables do not resolve, or the message names an account twice.
/// The block will not process such a message.
///
/// # Safety
/// `tx` must point to `tx_len` readable bytes and `req` to `req_len`. The
/// out-pointers must be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_resolved_admission(
    height: u64,
    tx: *const u8,
    tx_len: usize,
    req: *const u8,
    req_len: usize,
    out: *mut *mut u8,
    out_len: *mut usize,
) -> c_int {
    let (tx, req) = (req_slice(tx, tx_len), req_slice(req, req_len));
    let result = catch_unwind(AssertUnwindSafe(|| {
        let tables = match abi::Reader::new(req, abi::KIND_RESOLVE_REQ).map(|r| resolve::tables(&r)) {
            Ok(Ok(tables)) => (height, tables),
            _ => return SVM_ERR_BAD_INPUT,
        };
        let Some(root) = runtime::next_root(height) else {
            return SVM_ERR_UNCONFIGURED;
        };
        let mut w = abi::Writer::new(abi::KIND_CHECK_RESP, 0);
        match check::resolved_admission(&root, tx, &tables) {
            Ok(r) => {
                w.push(&[&0u32.to_le_bytes()[..], &r.cost.units.to_le_bytes(), &r.cost.allocated_bytes.to_le_bytes()].concat());
                for key in &r.writable {
                    w.push(key.as_ref());
                }
            }
            Err(e) => {
                w.push(&1u32.to_le_bytes());
                w.push(format!("{e:?}").as_bytes());
            }
        }
        emit(w.finish(), out, out_len);
        SVM_OK
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// `getFeeForMessage`. For a serialized `VersionedMessage` (wincode), writes
/// its fee to `out_fee` and sets `out_known`. It leaves `out_known` zero when
/// the message has no fee. Returns `SVM_ERR_BAD_INPUT` when the message does
/// not sanitize. Takes no block handle and holds no state, so it runs beside an
/// open block.
///
/// # Safety
/// `msg` must point to `msg_len` readable bytes. `out_fee` and `out_known` must
/// be writable.
#[no_mangle]
pub unsafe extern "C" fn svm_fee_for_message(
    msg: *const u8,
    msg_len: usize,
    out_fee: *mut u64,
    out_known: *mut u8,
) -> c_int {
    let bytes = req_slice(msg, msg_len);
    let result = catch_unwind(AssertUnwindSafe(|| match runtime::fee_for_message(bytes) {
        Ok(fee) => {
            *out_known = fee.is_some() as u8;
            *out_fee = fee.unwrap_or(0);
            SVM_OK
        }
        Err(()) => SVM_ERR_BAD_INPUT,
    }));
    result.unwrap_or(SVM_ERR_PANIC)
}

/// Drops the open block. Idempotent, so Go can call it from a defer.
#[no_mangle]
pub extern "C" fn svm_block_release(handle: u64) {
    let _ = catch_unwind(|| block::release(handle));
}

/// Frees a response arena. Safe with null.
///
/// # Safety
/// `ptr` must be null, or a pointer returned by one of the phase functions with
/// its matching `len`, not yet freed.
#[no_mangle]
pub unsafe extern "C" fn svm_buf_free(ptr: *mut u8, len: usize) {
    if !ptr.is_null() {
        drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(ptr, len)));
    }
}
