//! The wire format across the FFI boundary.
//!
//! A buffer that crosses the boundary contains no pointers. cgo forbids storing
//! a Go pointer in C memory, and it forbids passing Go memory that contains Go
//! pointers. A `struct {ptr, len}` array that describes account data is
//! therefore either illegal or a copy per account. A single flat arena is
//! legal and is one allocation, and both sides read its records in place.
//!
//! No serialisation format is needed. bincode, protobuf and flatbuffers move
//! structured data between programs that ship separately. Here Go and Rust are
//! one binary, built and versioned together. There is no other version to stay
//! compatible with, so the cheapest correct framing is enough.
//!
//! ```text
//!   header (32 bytes)      magic, version, kind, handle, count, total_len
//!   offset table           (count + 1) * u64, absolute, ascending
//!   records                record i is buf[off[i] .. off[i + 1]]
//! ```
//!
//! Everything is little-endian and 8-byte aligned. The trailing offset makes
//! every record length implicit, so no record needs its own length prefix and
//! a reader can slice without parsing.
//!
//! Two kinds of record break the no-pointer rule. The account record of the
//! execute and simulate requests holds a pubkey, then the address and length
//! of the value in Go's block cache. The proposal request's record holds the
//! address and length of one transaction. Go pins both for the call, so the
//! payload crosses without a second Go copy.

use solana_pubkey::Pubkey;
use std::mem::size_of;

pub const MAGIC: u32 = 0x304d5653; // "SVM0"
pub const VERSION: u16 = 0;
pub const HEADER_LEN: usize = 32;

// Buffer kinds. Request and response are distinct, so a mixed-up call is caught
// at the boundary, not misparsed into plausible garbage.
pub const KIND_PLAN_REQ: u16 = 1;
pub const KIND_PLAN_RESP: u16 = 2;
pub const KIND_RESOLVE_REQ: u16 = 3;
pub const KIND_RESOLVE_RESP: u16 = 4;
pub const KIND_EXECUTE_REQ: u16 = 5;
pub const KIND_EXECUTE_RESP: u16 = 6;
pub const KIND_CONFIGURE_REQ: u16 = 7;
pub const KIND_KEYS_RESP: u16 = 8;
pub const KIND_SIMULATE_REQ: u16 = 9;
pub const KIND_SIMULATE_RESP: u16 = 10;
pub const KIND_CHECK_RESP: u16 = 11;
pub const KIND_STATS_RESP: u16 = 12;
pub const KIND_COMMITTED_REQ: u16 = 13;
pub const KIND_DECODE_RESP: u16 = 14;
pub const KIND_DROPPED_RESP: u16 = 15;
pub const KIND_PROPOSAL_REQ: u16 = 16;

/// Fixed head of an account record, followed by `data_len` bytes.
///
/// The pubkey leads, and everything after it is byte for byte the value stored
/// under that pubkey in `svm0 .. svm7`. Go builds a record by appending the
/// key, then the stored value, with no transcoding.
///
/// ```text
///   [0..32)   pubkey
///   [32..40)  lamports     u64 LE     <- stored value starts here
///   [40..48)  rent_epoch   u64 LE
///   [48..52)  data_len     u32 LE
///   [52]      executable   u8
///   [53..56)  zero
///   [56..88)  owner
///   [88..)    data
/// ```
pub const ACCOUNT_HEAD_LEN: usize = 88;

/// An execute or simulate request's account record: the pubkey, then the
/// address and the length of its stored value, u64 LE each.
pub const PINNED_LEN: usize = 48;

#[derive(Debug)]
pub enum AbiError {
    TooShort,
    BadMagic,
    BadVersion,
    BadKind,
    BadOffsets,
}

/// A borrowed view over an arena. It holds no owned data. Every accessor
/// returns a subslice of the caller's buffer, so a read allocates nothing.
pub struct Reader<'a> {
    buf: &'a [u8],
    pub kind: u16,
    pub handle: u64,
    /// Kind-specific scalar. `plan` uses it for the number of leading records
    /// that are address lookup table accounts. When it is zero, Go skips
    /// phase 2.
    pub aux: u32,
    count: usize,
    offsets: &'a [u8],
}

fn u32_at(b: &[u8], off: usize) -> u32 {
    u32::from_le_bytes(b[off..off + 4].try_into().unwrap())
}

fn u64_at(b: &[u8], off: usize) -> u64 {
    u64::from_le_bytes(b[off..off + 8].try_into().unwrap())
}

impl<'a> Reader<'a> {
    pub fn new(buf: &'a [u8], expect_kind: u16) -> Result<Self, AbiError> {
        if buf.len() < HEADER_LEN {
            return Err(AbiError::TooShort);
        }
        if u32_at(buf, 0) != MAGIC {
            return Err(AbiError::BadMagic);
        }
        if u16::from_le_bytes(buf[4..6].try_into().unwrap()) != VERSION {
            return Err(AbiError::BadVersion);
        }
        let kind = u16::from_le_bytes(buf[6..8].try_into().unwrap());
        if kind != expect_kind {
            return Err(AbiError::BadKind);
        }
        let handle = u64_at(buf, 8);
        let count = u32_at(buf, 16) as usize;
        let aux = u32_at(buf, 20);
        let total = u64_at(buf, 24) as usize;
        if total != buf.len() {
            return Err(AbiError::TooShort);
        }

        let table_start = HEADER_LEN;
        let table_len = (count + 1)
            .checked_mul(size_of::<u64>())
            .ok_or(AbiError::BadOffsets)?;
        let table_end = table_start
            .checked_add(table_len)
            .ok_or(AbiError::BadOffsets)?;
        if buf.len() < table_end {
            return Err(AbiError::TooShort);
        }
        let offsets = &buf[table_start..table_end];

        // Validate once, here, so every later `record()` is a plain slice with
        // no bounds reasoning. Offsets must ascend and stay inside the buffer.
        let mut prev = table_end;
        for i in 0..=count {
            let off = u64_at(offsets, i * 8) as usize;
            if off < prev || off > buf.len() {
                return Err(AbiError::BadOffsets);
            }
            prev = off;
        }

        Ok(Reader {
            buf,
            kind,
            handle,
            aux,
            count,
            offsets,
        })
    }

    pub fn len(&self) -> usize {
        self.count
    }

    pub fn is_empty(&self) -> bool {
        self.count == 0
    }

    /// Record `i`. Infallible for `i < len()` because `new` validated the table.
    pub fn record(&self, i: usize) -> &'a [u8] {
        let start = u64_at(self.offsets, i * 8) as usize;
        let end = u64_at(self.offsets, (i + 1) * 8) as usize;
        &self.buf[start..end]
    }

    /// Record `i` as Go's `PushSized` wrote it: a u32 length, then exactly
    /// that many bytes. `record` would also return the padding to 8. None
    /// when the length runs past the record.
    pub fn sized(&self, i: usize) -> Option<&'a [u8]> {
        let r = self.record(i);
        let n = u32::from_le_bytes(r.get(..4)?.try_into().ok()?) as usize;
        r.get(4..4 + n)
    }

    pub fn iter(&self) -> impl Iterator<Item = &'a [u8]> + '_ {
        (0..self.count).map(move |i| self.record(i))
    }
}

/// Builds an arena. Records are appended, then `finish` writes the header and
/// offset table in front of them. The body is one allocation that grows
/// amortised, never one allocation per record.
pub struct Writer {
    kind: u16,
    handle: u64,
    aux: u32,
    offsets: Vec<u64>,
    body: Vec<u8>,
}

impl Writer {
    pub fn new(kind: u16, handle: u64) -> Self {
        Writer {
            kind,
            handle,
            aux: 0,
            offsets: Vec::new(),
            body: Vec::new(),
        }
    }

    pub fn with_capacity(kind: u16, handle: u64, records: usize, bytes: usize) -> Self {
        Writer {
            kind,
            handle,
            aux: 0,
            offsets: Vec::with_capacity(records + 1),
            body: Vec::with_capacity(bytes),
        }
    }

    pub fn set_aux(&mut self, aux: u32) {
        self.aux = aux;
    }

    pub fn set_handle(&mut self, handle: u64) {
        self.handle = handle;
    }

    pub fn push(&mut self, record: &[u8]) {
        self.offsets.push(self.body.len() as u64);
        self.body.extend_from_slice(record);
        pad(&mut self.body);
    }

    /// Appends an account record: fixed head then data.
    pub fn push_account(
        &mut self,
        pubkey: &[u8; 32],
        owner: &[u8; 32],
        lamports: u64,
        rent_epoch: u64,
        executable: bool,
        data: &[u8],
    ) {
        self.offsets.push(self.body.len() as u64);
        put_account(&mut self.body, pubkey, owner, lamports, rent_epoch, executable, data);
    }

    /// Seals the arena into an owned boxed slice, ready to hand to C.
    pub fn finish(mut self) -> Box<[u8]> {
        let count = self.offsets.len();
        let base = table_end(count);
        let total = base + self.body.len();

        let mut out = Vec::with_capacity(total);
        put_header(&mut out, self.kind, self.handle, count, self.aux, total);
        for off in &self.offsets {
            out.extend_from_slice(&(base as u64 + off).to_le_bytes());
        }
        out.extend_from_slice(&(total as u64).to_le_bytes());
        out.append(&mut self.body);
        debug_assert_eq!(out.len(), total);
        out.into_boxed_slice()
    }
}

/// Builds an arena when the record count and the body length are known before
/// the first record. It makes one allocation of the exact size. It writes the
/// header and the offset table in place, and it writes each record at its
/// final position, so `finish` copies nothing. The execute response uses it.
/// That body holds the write set of the block, and `Writer` would hold the body
/// twice at `finish`.
/// The simulate response uses it for the accounts that it carries.
pub struct ExactWriter {
    out: Vec<u8>,
    count: usize,
    next: usize,
}

impl ExactWriter {
    /// `body` is the sum of every record's `padded` length.
    pub fn new(kind: u16, handle: u64, aux: u32, count: usize, body: usize) -> Self {
        let base = table_end(count);
        let total = base + body;
        let mut out = Vec::with_capacity(total);
        put_header(&mut out, kind, handle, count, aux, total);
        // Each table entry is set when its record starts. The last entry is
        // the end of the arena.
        out.resize(base - size_of::<u64>(), 0);
        out.extend_from_slice(&(total as u64).to_le_bytes());
        ExactWriter { out, count, next: 0 }
    }

    /// `write` appends one record at the end of the buffer. Then the record
    /// gets its padding.
    pub fn record(&mut self, write: impl FnOnce(&mut Vec<u8>)) {
        let at = HEADER_LEN + self.next * size_of::<u64>();
        let off = self.out.len() as u64;
        self.out[at..at + size_of::<u64>()].copy_from_slice(&off.to_le_bytes());
        self.next += 1;
        write(&mut self.out);
        pad(&mut self.out);
    }

    /// Seals the arena. It is already the exact size, so the boxed slice is the
    /// same allocation.
    pub fn finish(self) -> Box<[u8]> {
        assert_eq!(self.next, self.count, "not the record count the arena was sized for");
        assert_eq!(self.out.len(), self.out.capacity(), "the body is not the length it was sized for");
        self.out.into_boxed_slice()
    }
}

/// Returns the start of the body of an arena of `count` records.
fn table_end(count: usize) -> usize {
    HEADER_LEN + (count + 1) * size_of::<u64>()
}

fn put_header(out: &mut Vec<u8>, kind: u16, handle: u64, count: usize, aux: u32, total: usize) {
    out.extend_from_slice(&MAGIC.to_le_bytes());
    out.extend_from_slice(&VERSION.to_le_bytes());
    out.extend_from_slice(&kind.to_le_bytes());
    out.extend_from_slice(&handle.to_le_bytes());
    out.extend_from_slice(&(count as u32).to_le_bytes());
    out.extend_from_slice(&aux.to_le_bytes());
    out.extend_from_slice(&(total as u64).to_le_bytes());
    debug_assert_eq!(out.len(), HEADER_LEN);
}

/// Returns the bytes that a record of `len` bytes takes in a body, with its
/// padding.
pub fn padded(len: usize) -> usize {
    len.next_multiple_of(8)
}

/// Zero-pads the body to 8 bytes, so a record head can be read without
/// unaligned loads. Each record starts on an 8-byte boundary, so padding the
/// body also pads the record.
fn pad(body: &mut Vec<u8>) {
    while !body.len().is_multiple_of(8) {
        body.push(0);
    }
}

/// Appends an account record: the fixed head, then the data, then padding.
pub fn put_account(
    body: &mut Vec<u8>,
    pubkey: &[u8; 32],
    owner: &[u8; 32],
    lamports: u64,
    rent_epoch: u64,
    executable: bool,
    data: &[u8],
) {
    body.extend_from_slice(pubkey);
    body.extend_from_slice(&lamports.to_le_bytes());
    body.extend_from_slice(&rent_epoch.to_le_bytes());
    body.extend_from_slice(&(data.len() as u32).to_le_bytes());
    body.push(executable as u8);
    body.extend_from_slice(&[0u8; 3]);
    body.extend_from_slice(owner);
    debug_assert_eq!(body.len() % 8, 0);
    body.extend_from_slice(data);
    pad(body);
}

/// A borrowed account, read in place out of an arena record.
pub struct AccountRef<'a> {
    pub pubkey: [u8; 32],
    pub owner: [u8; 32],
    pub lamports: u64,
    pub rent_epoch: u64,
    pub executable: bool,
    pub data: &'a [u8],
}

/// Reads a pinned byte record through its pointer. The record holds the
/// address and the length, u64 each. Each one is a transaction of a proposal.
/// Go pins it for the call and does not copy it, as it does for an account.
///
/// # Safety
/// The record's address must point to its length in readable bytes that
/// outlive `'a`.
pub unsafe fn pinned_bytes<'a>(rec: &[u8]) -> Option<&'a [u8]> {
    if rec.len() < 16 {
        return None;
    }
    let (ptr, len) = (u64_at(rec, 0) as usize as *const u8, u64_at(rec, 8) as usize);
    if ptr.is_null() || len == 0 {
        return None;
    }
    Some(std::slice::from_raw_parts(ptr, len))
}

/// A record of pubkeys, 32 bytes each with nothing between them. None when the
/// length is not a whole number of keys.
pub fn pubkeys(rec: &[u8]) -> Option<Vec<Pubkey>> {
    if !rec.len().is_multiple_of(32) {
        return None;
    }
    Some(
        rec.chunks_exact(32)
            .map(|k| Pubkey::new_from_array(k.try_into().unwrap()))
            .collect(),
    )
}

impl<'a> AccountRef<'a> {
    pub fn parse(rec: &'a [u8]) -> Option<Self> {
        if rec.len() < 32 {
            return None;
        }
        Self::stored(rec[0..32].try_into().ok()?, &rec[32..])
    }

    /// Reads a pinned record's value through its pointer.
    ///
    /// # Safety
    /// The record's address must point to its length in readable bytes that
    /// outlive `'a`. Go pins each value until the call returns.
    pub unsafe fn pinned(rec: &'a [u8]) -> Option<Self> {
        if rec.len() < PINNED_LEN {
            return None;
        }
        let ptr = u64_at(rec, 32) as usize as *const u8;
        if ptr.is_null() {
            return None;
        }
        let value = std::slice::from_raw_parts(ptr, u64_at(rec, 40) as usize);
        Self::stored(rec[0..32].try_into().ok()?, value)
    }

    /// Parses a stored value, which is an account record without its pubkey.
    fn stored(pubkey: [u8; 32], v: &'a [u8]) -> Option<Self> {
        let head = ACCOUNT_HEAD_LEN - 32;
        if v.len() < head {
            return None;
        }
        let end = head.checked_add(u32_at(v, 16) as usize)?;
        if v.len() < end {
            return None;
        }
        Some(AccountRef {
            pubkey,
            lamports: u64_at(v, 0),
            rent_epoch: u64_at(v, 8),
            executable: v[20] != 0,
            owner: v[24..56].try_into().ok()?,
            data: &v[head..end],
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_trip_records() {
        let mut w = Writer::new(KIND_PLAN_REQ, 7);
        w.push(b"one");
        w.push(b"");
        w.push(&[9u8; 300]);
        let buf = w.finish();

        let r = Reader::new(&buf, KIND_PLAN_REQ).unwrap();
        assert_eq!(r.handle, 7);
        assert_eq!(r.len(), 3);
        // Records are padded to 8 bytes, so compare prefixes, not whole slices.
        // The padding gives aligned reads.
        assert_eq!(&r.record(0)[..3], b"one");
        assert_eq!(&r.record(2)[..300], &[9u8; 300]);
    }

    #[test]
    fn round_trip_account() {
        let mut w = Writer::new(KIND_EXECUTE_REQ, 1);
        w.push_account(&[1u8; 32], &[2u8; 32], 42, u64::MAX, true, &[7u8; 165]);
        let buf = w.finish();

        let r = Reader::new(&buf, KIND_EXECUTE_REQ).unwrap();
        let a = AccountRef::parse(r.record(0)).unwrap();
        assert_eq!(a.pubkey, [1u8; 32]);
        assert_eq!(a.owner, [2u8; 32]);
        assert_eq!(a.lamports, 42);
        assert_eq!(a.rent_epoch, u64::MAX);
        assert!(a.executable);
        assert_eq!(a.data, &[7u8; 165]);
    }

    #[test]
    fn records_are_zero_padded_to_eight() {
        let mut w = Writer::new(KIND_EXECUTE_REQ, 1);
        w.push(b"one");
        w.push(&[5u8; 8]);
        w.push_account(&[1u8; 32], &[2u8; 32], 0, 0, false, &[7u8; 3]);
        let buf = w.finish();

        let r = Reader::new(&buf, KIND_EXECUTE_REQ).unwrap();
        assert_eq!(r.record(0), b"one\0\0\0\0\0");
        assert_eq!(r.record(1), &[5u8; 8]);
        assert_eq!(r.record(2).len(), ACCOUNT_HEAD_LEN + 8);
        assert_eq!(&r.record(2)[ACCOUNT_HEAD_LEN..], &[7, 7, 7, 0, 0, 0, 0, 0]);
    }

    #[test]
    fn pinned_account_reads_through_its_pointer() {
        let mut v = [0u8; ACCOUNT_HEAD_LEN - 32 + 5];
        v[0..8].copy_from_slice(&42u64.to_le_bytes());
        v[16..20].copy_from_slice(&5u32.to_le_bytes());
        v[20] = 1;
        v[24..56].copy_from_slice(&[2u8; 32]);
        v[56..].copy_from_slice(b"hello");
        let rec = [&[1u8; 32][..], &(v.as_ptr() as u64).to_le_bytes(), &(v.len() as u64).to_le_bytes()].concat();

        let a = unsafe { AccountRef::pinned(&rec) }.unwrap();
        assert_eq!(a.pubkey, [1u8; 32]);
        assert_eq!(a.owner, [2u8; 32]);
        assert_eq!(a.lamports, 42);
        assert!(a.executable);
        assert_eq!(a.data, b"hello");
        assert_eq!(a.data.as_ptr(), v[56..].as_ptr(), "read in place, not copied");

        let short = [&rec[..40], &4u64.to_le_bytes()].concat();
        assert!(unsafe { AccountRef::pinned(&short) }.is_none(), "a length shorter than the head");
    }

    #[test]
    fn a_pubkey_record_is_whole_32_byte_keys() {
        let two = [[1u8; 32], [2u8; 32]].concat();
        let keys = [[1u8; 32], [2u8; 32]].map(Pubkey::new_from_array).to_vec();
        assert_eq!(pubkeys(&two), Some(keys));
        assert_eq!(pubkeys(&[]), Some(vec![]));
        assert_eq!(pubkeys(&two[..63]), None);
        assert_eq!(pubkeys(&[0; 33]), None);
    }

    #[test]
    fn wrong_kind_is_rejected() {
        let buf = Writer::new(KIND_PLAN_REQ, 0).finish();
        assert!(matches!(
            Reader::new(&buf, KIND_EXECUTE_REQ),
            Err(AbiError::BadKind)
        ));
    }

    #[test]
    fn truncation_is_rejected() {
        let mut w = Writer::new(KIND_PLAN_REQ, 0);
        w.push(&[1u8; 64]);
        let buf = w.finish();
        assert!(Reader::new(&buf[..buf.len() - 8], KIND_PLAN_REQ).is_err());
        assert!(Reader::new(&buf[..4], KIND_PLAN_REQ).is_err());
    }
}
