# Licenses of the programs genesis reads from mainnet

When you build genesis, the build reads these programs from mainnet. It checks each program against
its pin in [`../manifest.json`](../manifest.json). This repository does not store their bytes.

## On every network

| Program | Pin | License | Text |
| --- | --- | --- | --- |
| Squads v4 `SQDS4ep6...` | executable sha256 `d4866083...2fac`, the build of `64af733` on `solanafoundation/solana-verifiable-build:1.18.16` | AGPL-3.0 | [`squads.md`](squads.md) |
| Pyth Core, both id sets | the `pyth` rows of [`../manifest.json`](../manifest.json) | Apache-2.0 | [pyth-crosschain](https://github.com/pyth-network/pyth-crosschain/blob/main/LICENSE) |
| tuktuk `tuktukUrfh...` and cron `cronAjRZn...` | mainnet's bytes, executable sha256 `a30a79f6...f080` and `3247399a...dbdf` | Apache-2.0 | [tuktuk](https://github.com/helium/tuktuk/blob/main/LICENSE) |

The Squads repository is under AGPL-3.0 from commit `316913a` (2024-10-11). The tree of `64af733`, on
the release line, still holds an older BSL 1.1 file. This chain follows the current license.

## Turned on by a flag

The `svmd init` flags `--ephemeral-validator` and `--vrf-oracle` put these programs in genesis.
Their pins are the `ephemeral-rollup` and `vrf` rows of the manifest.

| Program | Pin | License | Change date (then MIT) | Text |
| --- | --- | --- | --- | --- |
| Delegation program `DELeGGvX...` | 3a6fbe21, executable sha256 `6bc6d727...6a35` | BSL 1.1 | 2027-12-01 | [`delegation-program.md`](delegation-program.md) |
| Committor `ComtrB2K...` | mainnet's bytes, executable sha256 `e0a0a7d4...a2e2` | BSL 1.1, in `magicblock-validator` | 2027-12-01 | [`magicblock-validator.md`](magicblock-validator.md) |
| SPL Noop `noopb9bk...` | mainnet's bytes, executable sha256 `1bec855d...4818` | Apache-2.0 | none | [solana-program-library](https://github.com/solana-labs/solana-program-library/blob/master/LICENSE) |
| Session keys `KeyspM2s...` | mainnet's bytes, executable sha256 `c4e35525...0779` | MIT in its repository, GPL-3.0-or-later in its published crates | none | [gum-program-library](https://github.com/magicblock-labs/gum-program-library/blob/main/LICENSE) |
| VRF `Vrf1RNUj...` (`--vrf-oracle`) | mainnet's bytes, executable sha256 `3a883421...70d3` | BSL 1.1 | 2029-12-01 | [`vrf.md`](vrf.md) |

The ephemeral validator itself is not in genesis. The operator runs it. It is under BSL 1.1, with
the same text and the same change date as the committor, 2027-12-01.

The VRF oracle is under BSL 1.1, with the VRF text and its change date, 2029-12-01. The VRF text is
the one in force when the pinned build went to mainnet. The VRF `v0.4.1` tag still carries an older
text that gives 2027-12-01.

WARNING: Before a production deployment, check that you have the right to use each BSL component
that you turn on. If you do not have that right, get a commercial license from MagicBlock. BSL 1.1
lets you use the work in non-production. This page and the flag do not give you a right that the
license does not give.
