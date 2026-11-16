# shellcheck shell=bash
# shellcheck disable=SC2034 # every variable here is read by the script that sources this file
# Every third-party source and binary that the acceptance runs build or
# download, pinned in one place. scripts/prebuild.sh builds the sources once.
# The harnesses under rpc/accept read this file for what they fetch at run
# time. flake.nix pins the tools themselves: Go, Rust, the Solana CLI, both
# Anchor versions, Node with pnpm, spl-token, tuktuk's binaries and Hermes.
# Source this file after scripts/lib.sh. It sets variables and runs nothing.
#
# Move a pin in a change of its own, together with the run that proves it.

# MagicBlock's ephemeral rollups: the example apps as locked, the validator's
# repository for its test suites, and the two binaries under the Business
# Source License, which a run fetches and never keeps.
MB_EXAMPLES_REPO=https://github.com/magicblock-labs/magicblock-engine-examples
MB_EXAMPLES_COMMIT=e8393c60a82f73259a39abe807c6273a83659d0a
MB_VALIDATOR_REPO=https://github.com/magicblock-labs/magicblock-validator
MB_VALIDATOR_TAG=v1.0.2
MB_VALIDATOR_COMMIT=c175fb17d40cd686f94d5a3ff607596fda5fd500
MB_VALIDATOR_BIN_URL=https://github.com/magicblock-labs/magicblock-validator/releases/download/v1.0.2/ephemeral-validator-linux-x64-glibc
MB_VALIDATOR_BIN_SHA=b1718382a4104caffa8421f7aea13f10eaeb91153b1dd14cf7b53375e16f4085
VRF_ORACLE_URL=https://github.com/magicblock-labs/solana-vrf/releases/download/v0.4.1/vrf-oracle-linux-x64-glibc
VRF_ORACLE_SHA=b7a33dd9ce7298d55241354fdec6fe34dec1ebb8b4e93bf6b839c8846baa8cf5
MB_ANCHOR=anchor-1.0.2

# Helium's tuktuk: the repository that holds its suite and its examples, at a
# commit on main.
TUKTUK_REPO=https://github.com/helium/tuktuk
TUKTUK_COMMIT=2b83db5d35c4e53ab0010bc83334728d987594a9
TUKTUK_ANCHOR=anchor-0.31.1

# Settlement: Hyperlane's monorepo at the tag whose agents the e2e builds, its
# registry at the tag's .registryrc, and the two proof verifiers.
HL_REPO=https://github.com/hyperlane-xyz/hyperlane-monorepo
HL_TAG=agents-v2.3.0
HL_COMMIT=97cbefba2716c1de060ebd527f4a64fdfbc1c13d
HL_REGISTRY_REPO=https://github.com/hyperlane-xyz/hyperlane-registry
HL_REGISTRY_COMMIT=7db585d3f439e69e3a4680ca793bd97aa52e14eb
SP1_REPO=https://github.com/succinctlabs/sp1-solana
SP1_COMMIT=4181cae00d7493ede8a33066cb56683acb1dca72
R0_REPO=https://github.com/risc0/risc0-solana
R0_COMMIT=ee415935d04a948f27a346b563391900bdad6486 # risc0-solana v3.0.0

# The Solana Explorer the client suites drive.
EXPLORER_REPO=https://github.com/solana-foundation/explorer
EXPLORER_COMMIT=aa1b89bf5d5cf483a3d355e094a7a2aad4c85828

# The stock ibc-go chain the IBC harness relays to.
IBC_GO_REPO=https://github.com/cosmos/ibc-go
IBC_GO_TAG=v11.2.0
IBC_GO_COMMIT=cfc072e53eee42b2ab804cd4344ba610016f793c
