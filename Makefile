# cosmos-svm: the Solana VM on the Cosmos SDK.
# Run `make help` to list targets.

SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --no-builtin-rules --no-print-directory

# Lockfile resolution is part of the engine's consensus configuration.
# All Cargo commands use --locked to prevent dependency changes.
CARGO_BUILD := cargo build --release --locked

# Go's test cache does not account for changes to the cgo-linked engine.
# Disable it so tests always run against the current build.
GO_TEST := go test -count=1
BIN := bin

# Lint inputs, listed by path so the linters never walk target/ or node_modules/.
GO_DIRS := app bench cmd ffi genesis internal rpc tools x
SHELL_SCRIPTS := $(wildcard scripts/*.sh bench/*.sh bench/*/*.sh rpc/accept/*.sh rpc/testdata/*/*.sh) \
	rpc/accept/shim/solana-test-validator

.PHONY: help setup prebuild build devnet engine svmd svm-tpu quic-client fetch-programs proto golden inventory transcript divergences \
	test lint lint-go lint-shell \
	accept-rpc accept-clients accept-devnet accept-rollback accept-agreement accept-consensus accept-retention accept-ibc accept-er accept-settle accept-tuktuk \
	bench release release-preflight clean

help: ## List available targets
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@/ { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

##@ Setup (run `nix develop` first)

setup: fetch-programs build devnet prebuild ## Fetch pinned programs and build binaries and acceptance dependencies

prebuild: ## Build pinned third-party suites into target/third-party; use PARTS to select suites
	scripts/prebuild.sh $(PARTS)

##@ Build

build: svmd svm-tpu ## Build svmd and the QUIC ingress binary

devnet: engine svm-tpu ## Build svmd-devnet with the development faucet
	@mkdir -p $(BIN)
	go build -tags devnet -o $(BIN)/svmd-devnet ./cmd/svmd

engine: ## Build the Rust engine static library used by svmd
	$(CARGO_BUILD)

svmd: engine ## Build the standard node binary
	@mkdir -p $(BIN)
	go build -o $(BIN)/svmd ./cmd/svmd

svm-tpu: ## Build svm-tpu from its own Agave workspace
	$(CARGO_BUILD) --manifest-path tpu/Cargo.toml
	install -D tpu/target/release/svm-tpu $(BIN)/svm-tpu

quic-client: ## Build the QUIC client used by svm-tpu integration tests
	$(CARGO_BUILD) --manifest-path tools/quic-client/Cargo.toml

fetch-programs: ## Fetch programs into genesis/programs and programs/, verifying SHA-256 hashes
	go run ./genesis/fetch

##@ Code generation

proto: ## Regenerate Go code from proto/svm/v1/tx.proto
	scripts/gen-proto.sh

inventory: ## Regenerate Agave's RPC method inventory from the pinned solana-rpc crate
	tools/rpc-inventory.py

# The transcript is solana-test-validator 4.3.0's answers to every request
# that rpc/testdata/transcript/gen_corpus.py lists. Recording needs that
# validator on PATH and takes about half an hour.
transcript: engine ## Re-record the Agave transcript from solana-test-validator
	rpc/testdata/transcript/record.sh

divergences: engine ## Rewrite the transcript's divergences ledger from a replay
	TRANSCRIPT_DIFFS=$(CURDIR)/target/transcript-diffs.json $(GO_TEST) ./rpc -run TestTheChainAnswersTheAgaveTranscript
	rpc/testdata/transcript/gen_divergences.py target/transcript-diffs.json

# Golden files capture output from Anza's crates and verify this chain's
# renderers.
golden: engine ## Regenerate the parsed-account, transaction, and instruction goldens
	$(GO_TEST) -tags golden -run TestWriteGoldenFixtures ./rpc
	cargo run --quiet --locked --manifest-path tools/parsed-oracle/Cargo.toml -- \
		rpc/testdata/parsed_fixtures.json rpc/testdata/parsed_golden.json
	cargo run --quiet --locked --manifest-path tools/tx-oracle/Cargo.toml -- \
		rpc/testdata/tx_fixtures.json rpc/testdata/tx_golden.json
	cargo run --quiet --locked --manifest-path tools/tx-oracle/Cargo.toml -- instructions \
		rpc/testdata/ix_fixtures.json rpc/testdata/ix_golden.json

##@ Test

# Test the release profile because panic = "unwind" affects consensus behavior.
# Require QUIC binaries so missing integration-test dependencies fail explicitly.
test: engine svm-tpu quic-client fetch-programs ## Run Rust, Go, and devnet tests
	scripts/check-engine-deps.sh
	cargo test --release --locked -- --nocapture
	cargo test --release --locked --manifest-path tpu/Cargo.toml
	SVM_REQUIRE_QUIC=1 $(GO_TEST) ./...
	SVM_REQUIRE_QUIC=1 $(GO_TEST) -tags devnet ./...
	bench/bench.sh list >/dev/null

lint: lint-go lint-shell ## Run all CI linters

lint-go: ## Check Go formatting and vet packages with every build configuration
	@out=$$(gofmt -l $(GO_DIRS)); [ -z "$$out" ] || { echo "not gofmt-clean:"; echo "$$out"; exit 1; }
	go vet ./...
	go vet -tags devnet ./...
	go vet -tags golden ./rpc
	cd bench/storage && go vet ./...

lint-shell: ## Run ShellCheck on repository scripts and the validator shim
	shellcheck $(SHELL_SCRIPTS)

##@ Acceptance (requires flake tools and `make prebuild`; see docs/runbook.md)

accept-rpc: engine svm-tpu fetch-programs ## Test Solana CLI, SPL Token, and web3.js against a local chain
	rpc/accept/rpc.sh

accept-clients: engine svm-tpu fetch-programs ## Run pinned client suites, including the Explorer
	rpc/accept/clients.sh

accept-devnet: engine svm-tpu ## Test solana airdrop against a devnet node
	rpc/accept/devnet.sh

accept-rollback: engine svm-tpu fetch-programs ## Test svmd rollback on a Pebble home and a restart through the block it took back
	rpc/accept/rollback.sh

accept-agreement: engine ## Test four in-process validators across 5,000 mixed blocks
	SVM_AGREEMENT_BLOCKS=5000 $(GO_TEST) ./app -run TestFourValidatorsAgreeOnMixedBlocks -v -timeout 1800s

accept-consensus: engine svm-tpu ## Test validator restarts, joining, crashes, and full blocks
	rpc/accept/consensus.sh

accept-retention: engine svm-tpu ## Test the evidence window across more than 2,100 blocks
	rpc/accept/retention.sh

accept-ibc: engine svm-tpu ## Test token transfers to and from an ibc-go chain using Hermes
	rpc/accept/ibc.sh

accept-er: engine ## Test MagicBlock apps and suites against Agave; use PARTS to select suites
	rpc/accept/er.sh $(PARTS)

accept-settle: engine ## Test Hyperlane, SP1, RISC Zero, and Squads integrations against Agave
	rpc/accept/settle.sh $(PARTS)

accept-tuktuk: engine ## Test tuktuk suites and crank-turner flows against Agave
	rpc/accept/tuktuk.sh $(PARTS)

##@ Benchmarks (see bench/README.md)

bench: ## Run benchmarks; ARGS selects a suite, defaulting to microbenchmarks
	bench/bench.sh $(or $(ARGS),run micro)

##@ Release

# Run the release prerequisites one at a time. Each must pass before the next
# starts.
.NOTPARALLEL: release
release: release-preflight test accept-rpc accept-devnet accept-agreement accept-consensus accept-er accept-settle accept-tuktuk ## Run all release checks and build artifacts
	scripts/release-build.sh

release-preflight: ## Require a clean tree and a tagged HEAD
	scripts/release-preflight.sh

##@ Housekeeping

# clean keeps the acceptance and benchmark results (rpc/accept/results and
# bench/results).
clean: ## Remove build, test, and acceptance artifacts
	cargo clean
	cargo clean --manifest-path tpu/Cargo.toml
	cargo clean --manifest-path tools/quic-client/Cargo.toml
	cargo clean --manifest-path tools/parsed-oracle/Cargo.toml
	cargo clean --manifest-path tools/tx-oracle/Cargo.toml
	cargo clean --manifest-path tools/rpc-client-suite/Cargo.toml
	cargo clean --manifest-path bench/svm/Cargo.toml
	rm -rf $(BIN) dist
	rm -rf rpc/accept/clients/node_modules rpc/accept/er/node_modules rpc/accept/settle/node_modules
	rm -f genesis/programs/*.so programs/*.so
	find . -name __pycache__ -type d -prune -exec rm -rf {} +
