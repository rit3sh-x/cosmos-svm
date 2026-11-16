# Architecture

This page shows the parts of the chain and the path of a transaction through them. It is for operators, client developers and contributors.

## 1. One process, two languages

Each machine runs one process, `svmd`. It holds these parts:

```mermaid
flowchart TB
    subgraph go["Go: svmd"]
        rpc["rpc/<br/>Solana JSON-RPC and websocket"]
        comet["CometBFT<br/>consensus, mempool, block store"]
        app["app/ and x/svm<br/>admission, block execution, indexes"]
        mods["Cosmos modules<br/>staking, gov, upgrade, slashing, evidence, IBC"]
        ffi["ffi/<br/>arena writer and reader"]
    end
    subgraph rust["Rust: libsvm_engine.a"]
        engine["engine/<br/>parse, verify, schedule, execute"]
        anza["Anza crates<br/>solana-svm, loaders, syscalls, verifiers"]
    end
    store[("IAVL stores<br/>svm0 to svm7, svmmeta,<br/>Cosmos module stores")]
    local[("Machine-local databases<br/>svmhistory, svmtokens")]
    rpc --> comet
    comet <--> app
    app --> mods
    app --> ffi
    ffi -- "flat byte arenas" --> engine
    engine --> anza
    app --> store
    app --> local
    rpc --> local
```

The engine is a static library that the Go binary links through cgo. Go and Rust exchange flat
byte arenas, not pointers to each other's objects.

The engine never writes state. It reads accounts that Go gives it and returns the changes. Go is the only part that commits. This is the main safety rule of the project. If you do not know where code belongs, ask which side of this rule the code can damage.

## 2. The path of a transaction

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant R as rpc
    participant M as CometBFT mempool
    participant X as x/svm
    participant E as engine
    participant S as State
    C->>R: sendTransaction
    R->>E: preflight simulation on committed state
    R->>M: wrap in MsgSVMTransaction, then broadcast
    M->>X: CheckTx
    X->>E: check_tx: decode, verify, limits, fee
    X->>E: check_committed: payer, nonce, lookup tables
    X-->>M: admit or refuse
    Note over M: The pool ranks by fee for each block unit
    M->>X: PrepareProposal on the proposer
    X-->>M: transactions in fee order, inside the block limits
    M->>X: ProcessProposal on every validator
    X->>E: check_tx for each transaction, the block limits, then the signatures
    M->>X: FinalizeBlock
    X->>E: phase 1 plan, phase 2 resolve, phase 3 execute
    E-->>X: results and the write set
    X->>S: write accounts, records and indexes
    M->>X: Commit
    X-->>R: notify subscriptions
```

1. **Send.** The RPC runs a preflight simulation, unless the client sets `skipPreflight`. Then the RPC wraps the Solana transaction in a Cosmos message, `MsgSVMTransaction`. It gives the message to the CometBFT mempool. Each handler finds the Solana transaction in the envelope by a byte match. It decodes only an envelope that does not match.
2. **Admit.** `CheckTx` has two halves. The first half needs only the transaction bytes. The second half reads committed state: the fee payer, the nonce account and the address lookup tables. Each check reads its own branch of one view of the height. All checks at that height share that view, and the view caches no read.
3. **Propose.** The proposer packs transactions in descending order of `total_fee * 1e6 / (cost + 1)` until a block limit stops it. It does not pack more fees for one payer than the payer can pay.
4. **Validate.** Every validator checks the proposal again with the first half of admission and with the block limits. This check reads no state, so all validators get the same answer. Last, it checks the signatures. It skips a transaction that this machine checked before, and refuses the whole proposal at the first bad signature.
5. **Execute.** The block runs in three engine phases.
6. **Commit.** Go writes the changed accounts to the IAVL stores. CometBFT commits the block. The block is final at once, so the RPC answers `finalized` for every commitment level.

## 3. The three phases of a block

```mermaid
flowchart LR
    txs["Block transactions"] --> plan
    subgraph engine["Engine"]
        plan["Phase 1: plan<br/>decode, list the accounts,<br/>make the replay keys"]
        resolve["Phase 2: resolve<br/>expand lookup tables"]
        exec["Phase 3: execute<br/>conflict groups in parallel"]
    end
    plan -- "load list" --> push
    push["Go reads the accounts<br/>at most 512 MiB"] --> resolve
    resolve --> exec
    exec -- "results and write set" --> write["Go writes the accounts,<br/>history and token index"]
```

- **Phase 1, plan.** The engine decodes each transaction and checks its signatures, unless this machine checked the same bytes before, at `CheckTx` or in `ProcessProposal`. It finds the accounts that the block names and gives Go one list without duplicates. It also gives Go one replay key for each transaction. Go marks each transaction that the chain processed before, so that the engine does not run it again.
- **Phase 2, resolve.** If a v0 message uses address lookup tables, the engine expands them. If a block has only legacy transactions, the engine skips this phase.
- **Go reads the accounts.** Go reads each account on the list from the IAVL stores, in a fixed order. It stops at 512 MiB. A transaction that names an account past that point fails with `MaxLoadedAccountsDataSizeExceeded`.
- **Phase 3, execute.** The engine puts the transactions in conflict groups. Two transactions conflict when one writes an account that the other reads or writes. Groups run in parallel, and each transaction runs on Anza's `solana-svm`.

The access list of a Solana transaction makes this possible. Each transaction declares every account that it reads or writes before it runs. The chain knows every conflict before it executes anything, so it never runs a transaction twice.

## 4. Where the state is

```mermaid
flowchart TB
    subgraph consensus["In the app hash: the same on every machine"]
        shards["svm0 to svm7<br/>one account for each key,<br/>shard = top 3 bits of the pubkey"]
        meta["svmmeta<br/>engine digest, feature gates,<br/>replay commitment, last block hash"]
        cosmos["Cosmos module stores<br/>auth, bank, staking, gov,<br/>upgrade, slashing, evidence, ibc"]
    end
    subgraph localdb["Machine-local: can differ between machines"]
        history["svmhistory<br/>transactions, blocks, address index,<br/>limited by index-max-bytes"]
        tokens["svmtokens<br/>token accounts by owner"]
        cache["program cache in memory"]
    end
```

- **Accounts.** Each SVM account is one value in one of eight IAVL stores. The top three bits of the public key select the store.
- **Singletons.** `svmmeta` holds values that are not accounts. No account scan can see them.
- **Machine-local data.** The history index answers `getTransaction` and `getSignaturesForAddress`. The token index answers `getTokenAccountsByOwner`. A machine can delete or rebuild them. They are not part of consensus.

Lamports are not a Cosmos bank denom. They exist only in SVM accounts. The Cosmos side uses `usvm`, which only operators hold.

## 5. How the chain adopts an engine

All validators must execute with the same engine. The build computes a digest of every input that can change execution and puts it in the binary.

```mermaid
flowchart LR
    inputs["engine/src, Cargo.lock,<br/>Cargo.toml files,<br/>rust-toolchain.toml"] --> digest["engine digest<br/>in the binary"]
    digest --> genesis["genesis: InitGenesis<br/>refuses a different digest"]
    digest --> upgrade["upgrade handler<br/>writes the new digest"]
    genesis --> state[("svmmeta")]
    upgrade --> state
    state --> startup["startup: refuses to run<br/>if the digests differ"]
```

That is why a new engine always comes with an `x/upgrade` plan.

CAUTION: A machine that skips the plan keeps the old digest. It stops at an app hash mismatch.

A feature gate is different. The pinned crate already knows every gate, so a governance message can turn one on at a block height with no new binary.

## 6. Who decides what

| Change | How it happens | Example |
| --- | --- | --- |
| New engine or chain code | Binary upgrade with an `x/upgrade` plan | A new engine digest |
| Turn on a Solana feature | Governance message, at least 432,000 blocks ahead | Transaction V1, rent cuts |
| CometBFT consensus parameter | Governance message to `x/consensus` | `block.max_bytes` |
| Cache sizes, threads, RPC limits | Each operator, in `app.toml` or flags | `program-cache-bytes` |
