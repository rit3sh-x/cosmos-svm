//! The ranking key, and the test that chose it.
//!
//! `priority` is Agave's scheduler formula with one substitution. Agave's
//! numerator is the validator's deposit, which burns to zero, so here the
//! numerator is the total fee the payer pays. The denominator is this chain's
//! own cost model, so its write-lock, allocation and deploy terms all lower a
//! transaction's rank.
//!
//! Go ranks candidates in `x/svm/proposal.go`, where the sort must happen.
//! This function is here because the formula belongs beside the cost model it
//! divides by. The test below is the measurement that chose it.

/// `R * 1_000_000 / (C + 1)`, where `R` is the total fee and `C` is the cost
/// model's charge. Saturating, as Agave's is.
pub fn priority(total_fee: u64, units: u64) -> u64 {
    total_fee.saturating_mul(1_000_000) / (units + 1)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cost::{self, Cost, MAX_BLOCK_UNITS};
    use crate::runtime::LAMPORTS_PER_SIGNATURE;
    use agave_feature_set::FeatureSet;
    use solana_fee::FeeFeatures;
    use solana_hash::Hash;
    use solana_message::{
        compiled_instruction::CompiledInstruction, legacy::Message, MessageHeader,
        SimpleAddressLoader, VersionedMessage,
    };
    use solana_pubkey::Pubkey;
    use solana_runtime_transaction::{
        runtime_transaction::RuntimeTransaction, transaction_meta::TransactionMeta,
    };
    use solana_sdk_ids::{compute_budget, system_program};
    use solana_transaction::{
        sanitized::{MessageHash, SanitizedTransaction},
        versioned::VersionedTransaction,
    };
    use std::collections::HashSet;

    /// One offered transaction, priced by the real cost model and the real fee
    /// path. `bytes` is the wire size, so the byte budget binds too.
    #[derive(Clone, Copy)]
    struct Candidate {
        kind: &'static str,
        cost: Cost,
        total_fee: u64,
        priority_fee: u64,
        declared_cu: u32,
        bytes: u64,
    }

    impl Candidate {
        /// The price per declared unit. `getRecentPrioritizationFees` reports
        /// it, and nothing ranks by it.
        fn cu_price(&self) -> u64 {
            (self.priority_fee as u128)
                .saturating_mul(1_000_000)
                .checked_div(self.declared_cu as u128)
                .and_then(|x| u64::try_from(x).ok())
                .unwrap_or(0)
        }

        /// The chosen key.
        fn p_total(&self) -> u64 {
            priority(self.total_fee, self.cost.units)
        }

        /// The same key without the mandatory base fee in the numerator, so only
        /// the bid counts. It looks cleaner, and the test below shows it hands
        /// the block to an attacker.
        fn p_priority(&self) -> u64 {
            priority(self.priority_fee, self.cost.units)
        }
    }

    fn candidate(
        kind: &'static str,
        writable: usize,
        readonly: &[Pubkey],
        ixs: Vec<CompiledInstruction>,
        fs: &FeatureSet,
    ) -> Candidate {
        let mut keys = vec![Pubkey::new_unique()];
        keys.extend((0..writable).map(|_| Pubkey::new_unique()));
        keys.extend_from_slice(readonly);
        let tx = VersionedTransaction {
            signatures: vec![Default::default()],
            message: VersionedMessage::Legacy(Message {
                header: MessageHeader {
                    num_required_signatures: 1,
                    num_readonly_signed_accounts: 0,
                    num_readonly_unsigned_accounts: readonly.len() as u8,
                },
                account_keys: keys,
                recent_blockhash: Hash::default(),
                instructions: ixs,
            }),
        };
        let bytes = wincode::serialized_size(&tx).unwrap();
        let rtx = RuntimeTransaction::<SanitizedTransaction>::try_create(
            tx,
            MessageHash::Compute,
            None,
            SimpleAddressLoader::Disabled,
            &HashSet::new(),
        )
        .unwrap();
        let config = rtx.transaction_configuration(fs).unwrap();
        let fee = solana_fee::calculate_fee_details(
            &rtx,
            LAMPORTS_PER_SIGNATURE,
            config.priority_fee_lamports,
            FeeFeatures {},
        );
        Candidate {
            kind,
            cost: cost::cost(&rtx, fs),
            total_fee: fee.total_fee(),
            priority_fee: config.priority_fee_lamports,
            declared_cu: config.compute_unit_limit,
            bytes,
        }
    }

    /// `[tag, value_le]`, the ComputeBudget encoding.
    fn limit(units: u32) -> CompiledInstruction {
        CompiledInstruction::new_from_raw_parts(
            0,
            [&[2u8][..], &units.to_le_bytes()].concat(),
            vec![],
        )
    }

    fn price(microlamports: u64) -> CompiledInstruction {
        CompiledInstruction::new_from_raw_parts(
            0,
            [&[3u8][..], &microlamports.to_le_bytes()].concat(),
            vec![],
        )
    }

    fn transfer_ix(program: u8, accounts: Vec<u8>) -> CompiledInstruction {
        CompiledInstruction::new_from_raw_parts(
            program,
            [2u32.to_le_bytes().as_slice(), &1u64.to_le_bytes()].concat(),
            accounts,
        )
    }

    trait WithProgram {
        fn with_program(self, index: u8) -> Self;
    }

    impl WithProgram for CompiledInstruction {
        fn with_program(mut self, index: u8) -> Self {
            self.program_id_index = index;
            self
        }
    }

    /// An ordinary one-lamport transfer that declares 3,000 units and bids
    /// `microlamports` per unit.
    fn transfer(kind: &'static str, microlamports: u64, fs: &FeatureSet) -> Candidate {
        candidate(
            kind,
            1,
            &[compute_budget::ID, system_program::ID],
            vec![
                limit(3_000).with_program(2),
                price(microlamports).with_program(2),
                transfer_ix(3, vec![0, 1]),
            ],
            fs,
        )
    }

    /// The shape that ordering by declared units invites. It takes 20 write
    /// locks, which the block pays `cost::WRITE_LOCK_UNITS` each for, behind a
    /// declared limit of one unit, which that ordering divides by.
    fn lock_hog(microlamports: u64, fs: &FeatureSet) -> Candidate {
        let writable = 19;
        candidate(
            "lock-hog",
            writable,
            &[compute_budget::ID, system_program::ID],
            vec![
                limit(1).with_program(writable as u8 + 1),
                price(microlamports).with_program(writable as u8 + 1),
                transfer_ix(writable as u8 + 2, (0..=writable as u8).collect()),
            ],
            fs,
        )
    }

    /// Propagation budget, mirrored from `x/svm/proposal.go`.
    const MAX_BLOCK_BYTES: u64 = 8_388_608;

    #[derive(Default)]
    struct Packed {
        included: usize,
        units: u64,
        priority_fees: u64,
        hogs: usize,
        hog_units: u64,
        bidders: usize,
    }

    /// `Proposals.Prepare`'s loop, which defers what does not fit and keeps
    /// going.
    fn pack(order: &[Candidate]) -> Packed {
        let (mut units, mut bytes, mut allocated) = (0u64, 0u64, 0u64);
        let mut p = Packed::default();
        for c in order {
            if units + c.cost.units > MAX_BLOCK_UNITS
                || bytes + c.bytes > MAX_BLOCK_BYTES
                || allocated + c.cost.allocated_bytes > cost::MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA
            {
                continue;
            }
            units += c.cost.units;
            bytes += c.bytes;
            allocated += c.cost.allocated_bytes;
            p.included += 1;
            p.priority_fees += c.priority_fee;
            match c.kind {
                "lock-hog" => {
                    p.hogs += 1;
                    p.hog_units += c.cost.units;
                }
                "bidder" => p.bidders += 1,
                _ => {}
            }
        }
        p.units = units;
        p
    }

    /// One over-subscribed block, packed four ways.
    #[test]
    fn total_fee_over_cost_excludes_the_attacker_and_carries_more_traffic() {
        let fs = FeatureSet::default();

        // One lamport of priority buys the attack shape the same cu_price as a
        // bidder paying three thousand, while it costs more than twice as
        // much.
        let plain = transfer("plain", 0, &fs);
        let bid = transfer("bidder", 1_000_000, &fs);
        let hog = lock_hog(1_000_000, &fs);
        assert_eq!(bid.cu_price(), hog.cu_price(), "the two bids look identical per declared unit");
        assert!(
            hog.cost.units > 2 * bid.cost.units,
            "the attack shape costs {} against {}",
            hog.cost.units,
            bid.cost.units
        );
        assert!(
            plain.p_total() > hog.p_total(),
            "an unpriced transfer must outrank a one-microlamport lock hog: {} against {}",
            plain.p_total(),
            hog.p_total()
        );
        assert_eq!(plain.p_priority(), 0, "the base fee is the whole numerator of a zero bid");

        // Three blocks of honest traffic, a thin band of genuine bidders, and
        // an attacker who could fill a block alone. The attack is interleaved,
        // not appended, so arrival order meets it on the same terms as the fee
        // orderings.
        let mut offered = Vec::new();
        for i in 0..6_000u64 {
            offered.push(transfer("plain", 0, &fs));
            if i % 20 == 0 {
                offered.push(transfer("bidder", (i / 20 + 1) * 17_000, &fs));
            }
            if i % 4 == 0 {
                offered.push(lock_hog(1_000_000, &fs));
            }
        }
        let bidders = offered.iter().filter(|c| c.kind == "bidder").count();

        let sorted = |key: fn(&Candidate) -> u64| {
            let mut v = offered.clone();
            v.sort_by_key(|c| std::cmp::Reverse(key(c)));
            v
        };
        let arrival = pack(&offered);
        let by_cu_price = pack(&sorted(Candidate::cu_price));
        let by_total = pack(&sorted(Candidate::p_total));
        let by_priority = pack(&sorted(Candidate::p_priority));

        // The two rejected orderings hand most of the block to the attacker.
        for (name, p) in [("cu_price", &by_cu_price), ("priority_fee/cost", &by_priority)] {
            let share = p.hog_units as f64 / p.units as f64;
            assert!(share > 0.8, "ordering by {name} gives the attacker {:.1}%", 100.0 * share);
        }

        // The chosen ordering excludes the attacker, admits every bidder, and
        // carries more transactions than arrival order.
        assert_eq!(by_total.hogs, 0, "the attacker won {} slots", by_total.hogs);
        assert_eq!(by_total.bidders, bidders, "not every bidder was admitted");
        assert!(
            by_total.included > arrival.included * 12 / 10,
            "fee order carried {} against arrival order's {}",
            by_total.included,
            arrival.included
        );
        assert!(
            by_total.priority_fees * 1000 > arrival.priority_fees * 1000 * 15,
            "fee order collected {} against arrival order's {}",
            by_total.priority_fees,
            arrival.priority_fees
        );
    }

    #[test]
    fn a_full_block_clears_at_one_price_whatever_shape_fills_it() {
        let fs = FeatureSet::default();
        let plain = transfer("plain", 0, &fs);
        let hog = lock_hog(0, &fs);

        let block = |fee: u64, units: u64| (MAX_BLOCK_UNITS as f64) * fee as f64 / units as f64;
        let honest = block(plain.total_fee, plain.cost.units);
        let cheap = block(hog.total_fee, hog.cost.units);
        assert!(
            honest / cheap > 2.0,
            "under arrival order a block of lock hogs costs {cheap:.0} against {honest:.0}"
        );

        let level = (plain.p_total() as u128 * (hog.cost.units as u128 + 1) / 1_000_000) as u64;
        let ranked = block(level, hog.cost.units);
        assert!(
            (ranked - honest).abs() / honest < 0.001,
            "drawn level, a block of lock hogs costs {ranked:.0} against {honest:.0}"
        );
    }
}
