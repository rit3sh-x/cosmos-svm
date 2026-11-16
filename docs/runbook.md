# Validator runbook

This runbook is for operators. It gives the hardware, the settings, and the upgrade tasks for a validator on this chain.

This page gives no transactions-per-second figure. The block interval is the longer of `timeout_commit` and the block's own work, plus the consensus latency. Each operator sets `timeout_commit`, so a rate would show a configuration, not a chain property.

## 1. Hardware

| Item | Value | Reason |
|---|---|---|
| Architecture | x86_64 | The binary does not start on other architectures. |
| Cores | 4 | A thread pool compiles the cold programs of a block. The worst-case block assumes four threads. |
| Memory | 20 GB minimum | Includes the 1.5 GiB program cache. After two maximum program blocks, the cache and what its allocators keep take about 2.7 GiB. Pebble takes up to about 0.55 GiB of it, outside the Go heap, for the caches and memtables of the application database and its three sidecars. |
| Network | 25 Mbps | `block.max_bytes` assumes this speed. A slower link misses blocks. |
| Disk | About 568 bytes for each 165-byte account, plus snapshots | 10,000,000 accounts measured 5,419 MiB on Pebble. The database keeps two state versions and prunes at each block. The machine keeps the blocks that its two snapshots cover, 10,000 at the shipped `snapshot-interval`: about 20 MB at empty blocks, and up to 78 GiB at full blocks. |
| Disk, relayer node only | 100,000 blocks: about 200 MB (empty blocks) to 781 GiB (full blocks). 100 state versions: about 50 MB (quiet chain) to 3.9 GB (block limit). Also the transaction index, which has no purge. | Size the disk for expected activity, not for the small initial values. |

Ten million accounts use about 5.3 GiB of state. Add space for snapshots and Pebble compaction.

A validator that also serves RPC needs more cores. At the shipped 400 ms, a block comes every 0.4 s on an idle chain. A heavy RPC load beside consensus, such as a program deploy, can put a machine behind its peers. In the four-machine acceptance run, a 2-vCPU validator fell 129 blocks behind while it served one deploy. Serve public RPC from a machine that does not validate, or give the validator more cores than consensus and the store use.

## 2. Clock

Run a time synchronization service (`chronyd` or `systemd-timesyncd`) with a trusted source. Keep the clock offset below 500 ms.

NOTE: The binary does not enforce this. A startup check cannot find later drift or a lost time source. Monitor the offset and alert when it exceeds 500 ms.

Consensus sets chain time. Programs read the `Clock` sysvar, so one validator with a wrong clock cannot change the time that programs see.

## 3. config.toml

The binary writes these settings. Keep them when you edit `config.toml`.

- `consensus.timeout_commit = "400ms"`: the wait that starts when the votes to commit a block arrive, before the block runs. The next height starts when it ends. It is the whole block interval when the chain is idle. One validator measured 0.407 s a block at this value. A full block takes longer than 400 ms to run, so it hides the wait (section 1). At 400 ms an idle block takes about as long as a mainnet slot, so every window that a program counts in slots runs at mainnet's pace ([compatibility.md](compatibility.md) section 3). Do not set 1 s. The SDK changes a value equal to the CometBFT default of 1000 ms to 5 s. Use the same value on all machines. The chain does not enforce it, and a machine with a different value is early or late at each height.
- `consensus.timeout_propose = "3s"`: the CometBFT default. The binary sets it and does not start if the value is lower. You can increase it. A lower value can cause missed blocks under load.
- `mempool.max_txs_bytes = 8358477`: the pool must fit in one block, so fee sorting sees all transactions. If the pool is larger, sorting sees only the oldest part. The binary does not start if `max_txs_bytes + 3 x size` is more than 8,373,477 bytes. If you increase `mempool.size`, decrease `max_txs_bytes` by 3 bytes for each added transaction. The CometBFT default is 1 GiB, so update a `config.toml` that still has it.
- `instrumentation.prometheus = true`: starts the metrics server. CAUTION: If you disable it, you lose all chain metrics, and `getHealth` cannot see peers.
- `tx_index.indexer = "null"`: disables the CometBFT transaction index. The index has no purge, and the chain does not use it. The Solana RPC answers `getTransaction` and `getSignatureStatuses` from its own bounded index. CometBFT `/tx` and `/tx_search` return no results. Only the relayer node needs the index.

## 4. app.toml

`svmd init` writes all settings to `app.toml` with defaults and comments. Chain settings are under `[svm]`. Each has a flag, `--svm.<key>`, that replaces the file value. One struct in `cmd/svmd/config.go` defines each setting, default, and flag.

### Storage and pruning

- `iavl-cache-size = 500000`: applies to each store. Accounts use eight shards, so the total is 4,000,000 nodes. The value assumes 16 GB of memory, which is less than the 20 GB minimum. A lower value increases commit time. A higher value uses memory that the 20 GB minimum already gives to other consumers.
- `pruning = "custom"`, `pruning-keep-recent = "2"`, `pruning-interval = "10"`: gives the same result as `everything`. It is the only option that works with snapshots. Validation rejects `everything` when snapshots are on.
- `pruning-interval` has no effect. After `LoadLatestVersion`, the application sets it to 1. Pruning at each block keeps the node cache stable. The file value must pass validation, so it is 10. You can change it with no error and no effect.
- `app-db-backend = "pebbledb"`: the binary does not start with any other value. svmd opens the application database and its three sidecars on Pebble itself. The key tells the commands that svmd takes from the SDK where the application database is: `rollback`, `export`, `comet bootstrap-state` and the snapshot store. The CometBFT databases stay on its `db_backend`, goleveldb. A home that an older binary wrote on goleveldb does not open. Delete it, and start from genesis or a snapshot.
- `iavl-disable-fastnode = false` (default). The binary does not start if it is true. Account scans (`getProgramAccounts`, token index rebuild) stay complete during commit and prune, because IAVL reads the fast-node index. With fast nodes off, the scan walks the tree, and it stops with no error if pruning cuts it short. Account lookups also cost about two times more (108.60 us against 51.95 us). A worst-case block then takes about two times the 7.45 s that it is priced for.
- `inter-block-cache = false`: the SDK default is true. svmd writes false. The SDK cache keeps 1,000 values for each store, and it does not count their bytes. An account can have 10 MiB of data, so this cache can keep 10 GiB for each store. The node cache and the fast-node cache of IAVL already keep the values that a block reads. With the cache on, the measured blocks used the same memory, and some blocks took more time: a block of account lookups took 5 percent more. Do not set it to true.
- `min-retain-blocks = 2000`: the CometBFT block retention, equal to the evidence window. Every machine keeps this number of blocks. The snapshot interval cannot lower it. You do not need to set it.
- WARNING: Do not set `min-retain-blocks` below 2000. The SDK then prunes below the window once, at height 2,000, and the blocks do not return. Zero means "never prune". Set it higher to keep more blocks (RPC provider, relayer node).
- `snapshot-interval = 5000`, `snapshot-keep-recent = 2`: new machines join with these snapshots. At 400 ms blocks, 5,000 blocks is about 33 minutes. The two kept snapshots also set the oldest block that every machine keeps, 10,000 blocks back (section 1). If you turn snapshots off, this machine cannot be a source for new machines.

### Mempool

- `mempool.max-txs`: changes the mempool only. Keep `max-txs` positive. CAUTION: Do not set it to the SDK value `-1`. That value installs `NoOpMempool`. With a nil or no-op mempool, the SDK `ProcessProposal` always accepts, and the machine stops enforcing block limits.

### Indexes

- `[svm] index-max-bytes`: the binary does not start until you set it. It has no default, because it limits disk use and depends on the machine. It is the byte budget for the transaction history that answers `getTransaction`. When the budget is full, the machine drops the oldest heights. A purged transaction reads as null.
- A budget of 23,065,882,000 bytes, about 23.1 GB, keeps the history of the 2,000-block evidence window. Each block is the transfer block of the block-limits suite: 7,532 transfers at the shipped prices, which the history counts as 11,532,941 bytes. A block of default transfers holds 2,463 transfers and writes less history. On disk, the measured history used about 0.6 of the bytes that the budget counts. A larger budget keeps more heights and does not make a block slower. At 50 GiB, about 4,655 such blocks, a block of writes took 6.17 s and a block of these transfers took 3.55 s. At the 2,000-block budget, they took 6.32 s and 3.57 s. The difference is inside the noise of the measurement. The measurement does not include RPC reads of the older heights.
- The address index for `getSignaturesForAddress` is inside this budget. It is the largest part, with one 45-byte entry for each account that a transaction names. With 109-byte entries it was 76 percent of the size of each transaction without logs. At 45 bytes it is about 57 percent. This figure is scaled from that measurement and was not measured again. A budget keeps about two fifths of the history that the records alone would fill. For a budget sized for the records alone, make the value about 2.3 times larger to keep that window. If you keep the value, the window is shorter.
- Each record also stores the account keys that it names. This adds about one tenth to the bytes of a transfer, 144 bytes on 1,435, and the budget counts it.
- The token index has no setting and is outside this budget. It is in `data/svmtokens`. Each token account has an owner entry (65 bytes), a mint entry (65 bytes), and a reverse entry (97 bytes). That is about 227 bytes before database overhead. The index also keeps the total lamports of all accounts, which `getSupply` answers. Its size depends on the number of SPL Token and Token-2022 accounts, not chain age: about 454 MB at two million token accounts.
- You cannot tune the token index. A budget that fills would show a wallet some tokens and not others. If you delete the directory, the next block makes one pass over the account stores, and no answers are lost. During the pass, token methods and `getSupply` answer `-32005`.
- `getLargestAccounts` reads every account. It is one full scan of committed state. It takes about 127 s at ten million accounts and holds a database snapshot. The machine keeps each answer for two hours for each filter, as Agave does. A machine runs at most one such scan in two hours for each filter, in a slot of `rpc-max-scans`.

### Compute and memory

- `[svm] threads`: compiles the cold programs of a block. Four is the minimum, and the program budget assumes four threads. Scaling is 3.67x on four cores. Fewer cores do not stop the chain, but the worst block is slower.
- `[svm] program-cache-bytes` and `[svm] threads`: zero means the engine default (1.5 GiB cache, one worker for each core). The budget counts the bytes that the cached programs keep resident: the compiled code, its jump table and the copies of each ELF. The sampled mainnet working set, 164-172 MB of program text, keeps about 1.0 GiB resident. The cache stays in its budget while a block compiles, but it does not remove a program that the block uses. So one block of 400 MB of program text can hold about 2.3 GB until the block ends. If the machine has more memory, make the cache larger. A smaller cache means more compiles. Compiles cost the machine block time and cost the chain nothing.
- Simulations use the same budget. A simulation uses the programs that the blocks compiled. It compiles a program only if the blocks did not compile it. These programs get the part of the budget that the blocks do not use. `svm_program_cache_bytes` counts all of them.
- Neither setting is consensus. Machines with different budgets and thread counts get the same app hash. `app/cache_test.go` checks this with cold and warm caches, a restarted machine, and pools of one and four threads.
- `GOMEMLIMIT`: `svmd start` sets the Go memory limit to 14 GiB, unless the environment already sets `GOMEMLIMIT`. Without a limit, the collector lets the heap grow to two times its live size, and Go cannot see a cgroup limit. 20 GiB less the program cache at its peak, the execute arena at its peak, the engine and Pebble leaves about 15 GiB. The limit stays at 14 GiB, and the page cache gets the GiB that remains. It is machine-local, not consensus. On a bigger machine or with a bigger cache, set a higher value, for example `GOMEMLIMIT=30GiB svmd start`. On a machine below 20 GB, set a lower value. In the four-machine acceptance run, a 4 GiB machine at the default lost its validator to the kernel's OOM killer. The collector had no reason to run before the kernel did. The harness starts each of its processes with `GOMEMLIMIT=1GiB`.
- `MALLOC_ARENA_MAX` and `MALLOC_TRIM_THRESHOLD_`: svmd starts glibc malloc with 4 arenas and a trim threshold of 128 KiB, that is `MALLOC_ARENA_MAX=4` and `MALLOC_TRIM_THRESHOLD_=131072`. Pebble's caches and memtables and the engine's heap use glibc malloc. With the glibc defaults, glibc keeps much of the memory that they free. At ten million accounts, glibc held 913 to 1,184 MB with the defaults and 509 to 642 MB with these settings. The block time stayed in the range of the runs with the defaults. svmd sets a value only if the environment does not set it. To use a different value, set the variable in the environment, for example `MALLOC_ARENA_MAX=8 svmd start`. A value in `GLIBC_TUNABLES` is also the operator's value, and svmd does not change it. These settings are machine-local and not consensus.
- The limit keeps the process away from the OOM line. It does not protect the page cache. At ten million accounts, the live Go heap is 1.8 to 3.2 GiB on a normal block. Go holds about two times that, at most 6.6 GiB, so the limit does not operate. A block of large accounts is different. The IAVL node cache counts nodes, not bytes, so the live heap goes to 8.7 to 9.2 GiB, and Go holds about 14 GiB, at the limit. The collector then works harder, but garbage below 14 GiB still pushes the account database page cache out of memory. An account that a block names then costs more to read than the cost model charges.
- A node that catches up also holds the blocks it has fetched. CometBFT keeps up to 20 blocks for each peer until the node applies them, and a block is up to 8 MiB. svmd writes `max_num_inbound_peers = 10` in config.toml. Ten inbound and ten outbound peers then hold at most 3,200 MiB. The account database page cache gives up this memory while the node catches up. Raise the limit only on a machine with more memory than the published minimum.

Memory use on a 20 GiB validator, from block-limits runs at 10M accounts on a 20 GB cap and a validator-memory run:

| Consumer | MiB | Basis |
| --- | --- | --- |
| Page cache, account database | 7,472 | The remainder. The aged database is 24 to 27 GiB |
| Go heap and store path, 10M accounts | 6,718 | What Go holds at the worst normal block, `ldw-20g-aged`. The live heap is 3,165 |
| Program cache | 2,733 | `program-cache-bytes` of 1.5 GiB, full, and what the pool of sbpf and glibc keep after two blocks of 400 MB of new program text (`programs::tests::cache_peak_while_a_block_compiles`) |
| Execute arena | 1,536 | A block that writes back 512 MiB holds it three times |
| Page cache, blockstore, WAL, indexes | 728 | Measured |
| glibc: Pebble caches and memtables, malloc | 666 | Measured at `ldw-20g-aged` |
| Engine, RPC and the other parts of svmd | 606 | Measured peak |
| Kernel | 21 | Measured |

### RPC limits

All of these are machine-local, not consensus.

- `[svm] rpc-max-simulations = 1`: the number of `simulateTransaction` calls that load accounts at the same time. One simulation loads at most 64 MiB and holds it about twice over, so each slot costs 128 MiB on top of the program cache and the Go limit. If all slots are in use, a call waits for a free slot or a client disconnect. `svm_rpc_simulations_waiting` counts waiting calls.
- One simulation slot is enough for validator tools on the default loopback address, at hundreds of simulations each second. A public RPC node uses more slots. Eight slots use 1 GiB, which the account database page cache loses.
- `[svm] rpc-max-scans = 0`: the number of full scans of committed state that run at the same time. `getProgramAccounts` is such a scan. Each scan holds a database snapshot and reads every account: about 127 s at ten million accounts. Zero means the Agave default: one slot for each four CPUs, minimum one. On the 4-vCPU reference machine, this is one. `svmd` logs the value at start.
- If all scan slots are in use, a call waits for a free slot or a client disconnect. Point reads such as `getAccountInfo` do not wait for a scan slot. `svm_rpc_scans_waiting` counts waiting calls. An RPC machine that serves the Explorer or an indexer sets a higher value when `svm_rpc_scans_waiting` stays above zero.
- `[svm] rpc-threads = 0`: the number of HTTP requests that the machine answers at the same time. Zero means the Agave default: one for each CPU. `svmd` logs the value at start. A request holds its slot until the machine writes its answer. A scan or a simulation also takes its own slot inside it, and keeps that slot until its answer is written.
- If all request slots are in use, a request waits for a free slot or a client disconnect. So a point read waits only when all slots are in use. `svm_rpc_requests_waiting` counts waiting requests. If this value stays above zero, set a higher value. Each slot can hold one large answer in memory.
- `[svm] rpc-write-timeout = "30s"`: the time that the machine has to write one answer. A batch is one answer, and all its entries share this time. Only the time spent on writes counts. The time that a call runs, or waits for a slot, does not count. If the client reads too slowly, the machine closes the connection and frees the slot. At 30 s, a client must read a 1 GB answer at about 35 MB/s. If your clients read large answers over slow links, set a higher value.
- `[svm] rpc-scan-results-limit-mb = 0`: the most account data, in MiB, that one scan can keep. This is the Agave `--accounts-index-scan-results-limit-mb`. It applies to `getProgramAccounts`, `getTokenAccountsByOwner`, `getTokenAccountsByDelegate` and `getTokenLargestAccounts`. Each account that the scan keeps counts its data plus 96 bytes, as on Agave. When the sum is more than the limit, the scan stops. The client gets the Agave error `-32012`, `scan aborted: The accumulated scan results exceeded the limit`. Zero means no limit, which is the Agave default.
- A public RPC machine can set a scan limit to bound the memory of one scan. A scan of the System Program at ten million accounts can keep gigabytes.
- `[svm] rpc-cors-origins = "*"`: the web pages that a browser lets call this machine. With `*`, a page from any origin can call the Solana JSON-RPC and websocket, as with Agave. The Explorer and browser wallets need this. To allow only some pages, set a comma-separated list of `scheme://host[:port]`, for example `"https://explorer.example.com,http://localhost:3000"`. The list applies to both ports. A page from another origin gets `403`, and its request does not run.
- `""` refuses each page from another host. The CLI, Rust client, and Node clients send no `Origin` and are not affected. On a public RPC, a reverse proxy in front of `svmd` must pass `OPTIONS` requests and CORS headers. [`examples/gateway.nginx.conf`](../examples/gateway.nginx.conf) is one such proxy. It takes ports 8899 and 8900, so `svmd` behind it listens on `127.0.0.1:18899` and `127.0.0.1:18900`.

Three `[svm]` settings limit the websocket:

| Setting | Default | What it limits |
| --- | --- | --- |
| `ws-max-connections` | 1024 | Simultaneous websocket connections. The next connection gets HTTP 503 before the upgrade. |
| `ws-max-subscriptions-per-connection` | 1024 | Subscriptions for one connection. The next one gets the mainnet refusal string. |
| `ws-max-queue-bytes` | 262144 | Unsent notification bytes for each connection. Above this value, the machine closes that connection only. |

The machine limit is their product, not a fourth number. 1,024 x 1,024 is the 1,048,576 subscriptions that mainnet allows. 1,024 x 256 KB is its 256 MB notification queue. If you raise one value and lower the other, the limit stays the same and the balance changes.

More connections with fewer subscriptions each suits clients behind a proxy. Fewer connections with more subscriptions each suits an indexer. An indexer that needs more than 1,024 accounts must use `programSubscribe`, not a higher limit.

The machine closes a connection that sends nothing for 60 seconds. Every Solana client keeps its connection open. The blocking Rust client answers the ping that this machine sends every 54 seconds. A client does not see this rule. The rule removes a peer whose machine is gone.

### The QUIC port

`solana program deploy` without `--use-rpc` sends writes over QUIC, to the address from `getClusterNodes`. The port is off by default. Turn it on only on machines that serve Solana JSON-RPC to clients. A validator that serves no clients leaves it off. With the port off, `svmd` starts without the `svm-tpu` binary.

To turn the port on:

1. Put `svm-tpu` next to `svmd`, or set `tpu-quic-binary`.
2. In `app.toml`, set:

   ```toml
   [svm]
   tpu-quic-address = "0.0.0.0:PORT"
   public-tpu-address = "PUBLIC_IP:PORT"
   ```

   PORT is any free UDP port. Agave validators use 8000-10000.
3. Open UDP PORT to clients. QUIC does not pass through an HTTP reverse proxy.
4. Set `net.core.rmem_max` and `net.core.wmem_max` to 134217728 (the Agave recommendation).
5. Make the mempool gossip. Set `broadcast = true` and `type = "flood"` in `config.toml`. If you do not, `svmd` does not start, because each leader entry would name an ingress that reaches no other validator.
6. Increase `tpu-max-connections-per-unstaked-peer` and `tpu-max-connections-per-ipaddr-per-minute` only for clients that share one address. The per-IP limits are those of Agave. The limit of 200 transactions each second for each address is fixed.

`svmd` also does not start in these cases:

- the binary is missing,
- the bind address is unspecified and there is no public address,
- the advertised address is IPv6.

`svmd` starts `svm-tpu` as a child and restarts it when it stops. The first restart is after 1 s, and the wait doubles to a maximum of 30 s. While the child is down, `getClusterNodes` gives `tpuQuic: null`. The `svm-tpu` output is in the `svmd` log. A transaction from `svm-tpu` goes through the same admission as `sendTransaction` with `skipPreflight`. The sender gets no answer, as on Agave.

`svm-tpu` keeps at most 1,024 transactions that `svmd` did not read yet. When this queue is full, `svm-tpu` drops each new transaction, as Agave does. The queue uses at most 64 MiB. `svm_quic_total{event="total_handle_chunk_to_packet_send_full_err"}` counts the drops. If this count increases, the clients send transactions faster than `svmd` admits them.

### Fees and the Cosmos lane

- `minimum-gas-prices = "0.000001usvm"`: the fee minimum for the Cosmos lane. One `usvm` pays for a transaction of up to a million gas. The SVM path pays in lamports and never sees it. A Cosmos transaction ranks above every SVM transaction. An IBC transfer creates an account for the voucher receiver, so at a price of zero, any voucher holder could use the lane for free. `usvm` cannot be sent, so only operators can pay the minimum.
- The binary does not start in three cases: the price is zero, it does not name `usvm`, or it names another denom next to `usvm`. The fee check accepts any one denom on the list, so a voucher on the list would give the lane to its holders. The pool checks the minimum when a transaction arrives, not when a validator validates a block.
- A relayer uses an operator key, because only operators hold the fee denom. Every Cosmos transaction that an operator signs (proposals, votes, delegations) pays the minimum.
- An incoming transfer goes back unless its receiver holds `usvm`. A voucher is useful only to an account that can pay the minimum to move it, which means an operator account. The chain sends an error acknowledgement for a transfer to any other address, and the sender gets the tokens back on the other chain.

### gRPC

`[grpc] address = "localhost:9090"` is the SDK default. The operator relayer connects here. It serves the transaction service, the CometBFT service, and every module query, including IBC. It has no connection limit. Public clients use the Solana JSON-RPC, not gRPC.

WARNING: Do not bind gRPC to an address other than localhost, unless you accept the risk. On another address, it publishes all of these services.

More than one operator can relay one channel. CheckTx refuses a relay that another operator already made, with "packet messages are redundant". This costs the relayer and the block nothing. Hermes logs it and continues.

## 5. Join an existing chain

State sync is the only supported way to start a new machine.

CAUTION: Do not replay from genesis. The replay-protection set is an extension snapshot. A machine that replays without it cannot rebuild what it dropped.

WARNING: `trust_height` and `trust_hash` are a trust decision, not a chain fact. The machine checks the rest of the snapshot against them. Get them through a channel that you trust.

1. Get `trust_height` and `trust_hash` from a trusted operator, through a different channel.
2. Compare the pair with a second operator.
3. In `config.toml`, under `[statesync]`, set:

       enable = true
       rpc_servers = "<two RPC addresses of machines you trust>"
       trust_height = <a height at least some thousand blocks old>
       trust_hash = "<the block hash of that height>"
       trust_period = "168h0m0s"

4. Install the binary that ran at the snapshot height, not the newest binary. A snapshot names a release, because the engine digest is part of state. A machine that restores a height with the wrong binary does not start, and does not fork silently.
5. If an upgrade height is after the snapshot height, start under cosmovisor with the older binary in place. Cosmovisor changes the binary at the plan height. [`examples/svmd.service`](../examples/svmd.service) runs the machine under cosmovisor as a service. Section 7 says why to use cosmovisor.
6. After the first block, confirm that the machine agrees with the chain. The Solana RPC answers when the first block commits.

### The relayer node

An operator who relays runs Hermes next to a second machine. This machine is a full node, not a validator. It joins as above and serves the proofs and blocks that Hermes asks for. Hermes connects to it on localhost, where gRPC listens. A validator proves only its last three heights and keeps 2,000 blocks, which is not enough for Hermes.

In `config.toml`, set:

    [tx_index]
    indexer = "kv"

Hermes finds a packet sent from this chain with `tx_search` when it clears.

CAUTION: This index has no purge. When the machine exceeds its disk budget, delete the data and do state sync again.

In `app.toml`, set:

    pruning-keep-recent = "100"
    min-retain-blocks = 100000

- `pruning-keep-recent` is the Hermes `clear_interval`. Hermes proves a packet at the height of its event. If a relay does not land in 100 blocks, Hermes proves it again when it clears.
- `min-retain-blocks` covers the refresh of an idle client.

NOTE: After a restart, a machine stops state pruning until its next snapshot, and holds more data for a short time.

[`examples/hermes.toml`](../examples/hermes.toml) is a complete Hermes configuration for this chain and one counterparty, and [`examples/hermes.service`](../examples/hermes.service) runs it. In the Hermes `config.toml`, in the entry for this chain, set:

    compat_mode = '0.38'
    gas_price = { price = 0.000001, denom = 'usvm' }
    client_refresh_rate = '1/8'
    key_name = '<the key of an operator account>'

- `compat_mode`: Hermes knows no CometBFT version after 0.38.
- `gas_price`: the fee minimum.
- `client_refresh_rate`: at 1/8 of a 14-day trusting period, Hermes refreshes an idle client every 1.75 days. That is at most 75,600 blocks back, inside the 100,000.
- `key_name`: the key must hold `usvm`, so it must be an operator key.

## 6. Governance

The governance module account is the authority for `x/consensus`, `x/upgrade`, `x/auth`, `x/bank`, `x/staking`, `x/slashing`, and `x/svm`. Only a proposal can change a feature gate, a consensus parameter, or an upgrade plan.

The operators are the voters. `usvm` cannot be sent, so only accounts bonded at genesis hold it. A proposal, a deposit, and a vote all need it. There is no allow list to maintain.

| Parameter | Value | Why it differs from the SDK value |
| --- | --- | --- |
| `min_deposit` | 10000000usvm | Same number as the SDK, with the correct denom. The default `stake` would make every proposal impossible to vote on. |
| `expedited_min_deposit` | 20000000usvm | 2x, not 5x. At 5x, an operator with one open proposal could not pay for an emergency proposal. |
| `voting_period` | 24 h | The 432,000-block delay of a gate flip is 48 h on an idle chain. The SDK 48 h would leave no time to cancel. |
| `expedited_voting_period` | 12 h | Halves the time to undo a change. It does not make a flip faster. |
| `quorum` | 0.667 | Three of four operators. The SDK 0.334 would be two. |
| `min_initial_deposit_ratio` | 1.0 | Removes the deposit period. The vote starts at submission. |
| `burn_vote_veto` | false | The denom has a fixed supply and is also the permission. Nothing can replace a burn. |

Submit a proposal. `svmd` has no keyring and no `tx` subcommands, so use the tool that holds the operator key.

1. Build a `MsgSubmitProposal` with the messages to execute, the full deposit, and the proposer account.
2. Sign the transaction with the tool that holds the operator key.
3. Send it as a usual Cosmos transaction.

Pay the full deposit at submission. A partial deposit makes the submission fail, and the chain does not keep the proposal open for more funds. The transaction pays `ceil(0.000001 x gas)` usvm, which is 1 usvm up to a million gas. Governance uses the standard SDK gov module, with `cosmos.gov.v1.MsgSubmitProposal`, `MsgVote`, and `MsgDeposit`.

### Feature gates

A gate flip has this timeline:

1. An operator submits a proposal at block S.
2. The vote takes 24 h.
3. The proposal executes at block E.
4. At least 432,000 blocks pass.
5. Block H-1 is the last block for a cancel.
6. The begin-blocker turns the gates on at block H.

A feature-gate proposal carries `MsgScheduleGates` with:

- the governance module address as authority,
- the public keys of the features,
- a height at least 432,000 blocks after the block where the proposal executes.

The 432,000 blocks count from execution, not from submission. At the shipped `timeout_commit`, the 24 h vote is at most 216,000 blocks, because a block never takes less than 400 ms. Add both and a margin. The submission block plus 648,100 is safe.

CAUTION: A height inside the minimum makes the proposal pass and then fail at execution. Read the scheduled height back before you rely on it.

Cancel a gate. Send `MsgCancelGates` with the same height and gates. This removes a pending flip. It takes effect when it executes, so use the expedited path. `x/svm` applies a flip in the begin-blocker of block `H`, so the last block for a cancel is `H-1`.

A cancel that names a height with no scheduled gate, or a gate that nothing scheduled, fails at execution. Its error shows where the gate is scheduled. A cancel proposal that shows "passed" did cancel.

### A validator that stops, and a validator that signs twice

States: Bonded, Jailed, Tombstoned.

- Bonded to Jailed: the validator missed more than 250 of the last 500 blocks.
- Jailed to Bonded: `MsgUnjail`, after the machine catches up.
- Bonded to Tombstoned: the consensus key signs two different votes. To recover, unbond, then bond again with a new consensus key.

`x/slashing` counts the signatures of each validator over 500 blocks. If a validator is in the set for 500 blocks and misses more than 250 of the last 500, the chain jails it. Two blocks later it leaves the CometBFT validator set, so other operators do not wait for a `timeout_propose` at each of its turns. At 400 ms blocks, 250 blocks is about 100 s of an idle chain. A restart costs a few blocks and a coordinated upgrade costs none. The chain jails a machine that is down for minutes. The machine comes back with one transaction, below.

To unjail, send your own transaction. You do not need to wait. The jail lasts 1 ns, the minimum that the SDK accepts. `svmd` builds no transaction, so use the tool that holds the operator key.

CAUTION: Do not send `MsgUnjail` before the machine catches up. If the machine returns before it catches up, it misses blocks, and the chain jails it again after the new window.

1. Restart the machine.
2. Wait until the machine catches up.
3. Build `cosmos.slashing.v1beta1.MsgUnjail` with `validator_addr` set to your `cosmosvaloper1...` address.
4. Sign it with your operator account.
5. Send it. It pays the 1 usvm fee minimum.

The validator returns to the set two blocks after the block that carries the message, with a new 500-block window.

Tombstoning costs the consensus key. If a consensus key signs two different votes at one height and round, `x/evidence` tombstones it. The validator is jailed permanently, and the key never returns. You lose nothing else. Unbond the stake and bond it again from a validator with a new consensus key, at full weight.

WARNING: Never put the same `priv_validator_key.json` on two machines. This is the usual cause of a tombstone, for example a standby that starts before the primary stops.

Neither fault burns tokens. Both slash fractions are zero. The chain has no issuance, so voting power stays at the genesis bonds. A burn would not be a fine from earnings. It would permanently reduce the operator share in the validator set and in governance.

## 7. Upgrades

Run cosmovisor. An upgrade takes effect at a named height, in one block, on every machine at once.

CAUTION: Do not change binaries by hand at the upgrade height. If a person changes binaries by hand in that window, the chain can stop.

Each operator must be able to download from the release URL in the plan. Each plan has one URL, and the URL is part of the proposal. Check that you can download it when the proposal appears, not at the height.

A plan names exactly one artifact, the one for the network of this chain. A plan is chain state, so a devnet plan exists only in a devnet chain store. For this reason, the plan has no build-type field.

CAUTION: Download the artifact for the network of this chain. The other artifact passes the checksum, because it is a real binary. The machine then refuses to start on the network check. With this number of validators, that stops the chain but does not fork it.

Before the height, read the `.yml` next to the artifact to check which one you have:

    version: v1.2.3
    commit: <full git sha>
    target: linux/amd64
    network: production
    engine_digest: <64 hex characters>

Both artifacts of one release have the same `engine_digest`, because the engine is the same. A different digest would claim an execution difference that does not exist. The `network` field shows the difference. The file name also shows it, because a release never publishes a file named `svmd`.

WARNING: Never use `--unsafe-skip-upgrades` alone on this chain. One machine that skips causes an immediate fork. The upgrade handler writes the new engine digest into state. A machine that skips the handler keeps the old digest and disagrees on the next block, with an app hash mismatch and no warning. A skip is safe only if every validator skips the same plan together.

This binary has no upgrade handler. No network runs this chain yet, so every network starts on this binary at genesis, and no plan has anything to move. The first upgrade of a running network brings its own handler, which adopts the new engine.

Before you propose a release, compare the program manifest with mainnet:

    python3 genesis/check-manifest.py

It reads mainnet again and shows how mainnet differs from `genesis/manifest.json`. Memo v1 always shows a difference, because genesis takes its published v1.0.0 build. Any other difference is a program that mainnet upgraded. The command always exits 0, because a program upgrade on mainnet is news about mainnet, not a defect here. Read the output and decide.

## 8. Make a localnet

This is the shortest path from a clean checkout to a machine that makes blocks. You can also use it to test settings before you change a real deployment.

1. Build the engine and the two binaries:

       make build
       go build -o ./svmd ./cmd/svmd
       go build -tags devnet -o ./svmd-devnet ./cmd/svmd

2. Make the chain:

       ./svmd init my-validator --chain-id svm-local

3. Add the genesis programs. This step downloads the programs and checks each one against its pin:

       ./svmd add-genesis-programs genesis

4. In `~/.svmd/config/app.toml`, under `[svm]`, set `index-max-bytes` to a non-zero value. The binary does not start until you do.
5. Start the chain:

       ./svmd start

With no operator flags, steps 2 and 3 write a genesis with:

- this machine, bonded as the only validator,
- the seven pinned SPL programs,
- Pyth Core's two id sets and the three accounts that each set copies from mainnet,
- Squads v4 and its global config,
- the feature set as it was at the pinned slot.

WARNING: Use this genesis only for a test chain. Here the operator account comes from the machine consensus key, so the consensus key is the only key that can sign for it. A real deployment keeps the consensus key out of transactions.

### More than one operator

One machine builds the full genesis with one command. Every other machine gets the finished file. There is no gentx, no collection round, and no keyring.

1. Each operator gives the coordinator two values, the consensus public key and the account, and keeps the keys:

       ./svmd comet show-validator

2. The coordinator puts the values in a file, in a fixed order, in version control:

       {
         "validators": [
           {
             "consensus_pubkey": {"@type": "/cosmos.crypto.ed25519.PubKey", "key": "<base64>"},
             "account": "cosmos1...",
             "self_bond": "10000000",
             "free": "90000000",
             "moniker": "operator-0"
           }
         ]
       }

3. The coordinator makes the genesis:

       ./svmd init coordinator --chain-id svm-local --validators-file validators.json

Nothing sorts the list. If two coordinators use different orders, they get different bytes and see it in the hash.

Instead of the file, you can pass `--validator consensus-pubkey,account,self-bond,free,moniker` for each operator. This is faster for a localnet of two machines. In the flag, the consensus public key is the base64 key alone, because the JSON form contains the comma that separates the fields. The binary refuses a duplicate public key or account. It also refuses a self-bond too small for one unit of voting power.

`self_bond` and `free` are in `usvm`. The coordinator decides what a deployment mints and whether operators are equal.

### Compare the hash before the first start

`init` prints the genesis hash:

    genesis hash: <base58>

This is the string that `getGenesisHash` serves, so you can compare it with a running machine.

CAUTION: Do this check before the first start. CometBFT reads `config/genesis.json` only when its state database has no genesis document. If you edit that file on a machine that already started, the edit has no effect, and nothing tells you.

1. Copy the genesis file to every machine.
2. Compare the printed string on each machine.
3. Start each machine with the check on:

       ./svmd start --expected-genesis-hash <base58>

If the hash differs, the binary does not start and names both values. Without the check, the failure is an app hash disagreement at block 1 that does not mention genesis. The CometBFT handshake compares the block protocol version, chain ID, and channels. It does not compare genesis. Two machines with the same chain ID and different genesis connect and gossip first.

The Solana JSON-RPC starts next to the Cosmos RPC on `127.0.0.1:8899`, with websockets on `8900` (`rpc-address` and `ws-address` under `[svm]`). Unmodified Solana tools work with it:

    solana -u http://127.0.0.1:8899 block-height
    solana -u http://127.0.0.1:8899 balance <address>

Use a configuration file with `-C`, not `solana config set` alone, so a localnet does not change the global Solana configuration.

### Four machines, before a tag

    make accept-consensus

This target runs four real `svmd` processes on this machine, each with its own home and ports. They go through four events:

1. one validator restarts during the run,
2. a fifth machine joins by state sync,
3. the harness stops one of the four by force while the chain continues to commit,
4. a block at the configured `block.max_bytes`.

After each event, the harness compares the app hash of each machine at each height. It also checks that `--expected-genesis-hash` refuses a wrong genesis. The run takes about six minutes and runs five processes.

The harness reads the `config.toml` and `app.toml` that `init` wrote, so it is a shell harness, not a Go test. It changes four settings and gives the reason in place:

- the snapshot interval, so a joining machine does not wait half an hour,
- the mempool size, because a block at the byte limit holds more transactions than the default pool, with a lower byte limit to match,
- the program cache, because five processes share one machine,
- on one machine only, `timeout_commit`, set to 4 s so CheckTx can admit a full block between two proposals.

On one machine, the byte-limit case proves only that the chain can build, validate, and commit such a block. Propagation needs machines that do not share a kernel. To run the harness on five machines, set `SVM_CONSENSUS_HOSTS` to their five addresses. This machine must reach each address over ssh. No make target runs it.

### The fee market for senders

A priority fee buys a place in the block. The proposer packs a block in descending order of `total_fee * 1e6 / (cost + 1)`. The cost is the cost-model charge that the block budget uses. The key is lamports for each block unit, so a full block clears at about one price for any transaction shape.

Plan for these results:

- There is no age term and no reserved lane. If nobody pays less than a transaction, the chain never includes it. It leaves the blockhash window, and the sender sends it again with a higher fee. Solana does the same. A place in the block has a price. It is not a lottery.
- A real loaded-accounts limit is the cheapest way to rank higher. A transaction with no `SetLoadedAccountsDataSizeLimit` gets the 64 MiB maximum. That costs 16,384 units, which is 67 percent of the 24,352 units of a default transfer (compatibility page, section 1). At the same fee, a wallet that declares a real limit ranks above one that does not.
- `getRecentPrioritizationFees` gives the current rate over the last 150 blocks, as the minimum compute unit price of each block. It reports a compute unit price, not the ranking key, because `setComputeUnitPrice` takes that number. The two values move together. It does not filter by account and does not report an idle block.

The order is machine-local. A validator that packs by arrival order and one that packs by fee both make blocks that every other validator accepts. Validity never depends on the mempool.

## 9. Metrics

Scrape `instrumentation.prometheus_listen_addr` (`:26660` unless you changed it). There is no second port and no `[svm]` key. [`examples/prometheus.yml`](../examples/prometheus.yml) is the scrape, and [`examples/prometheus-alerts.yml`](../examples/prometheus-alerts.yml) holds the alerts below. Chain collectors use the same process-global registry as the CometBFT metrics server, so you get `cometbft_*` and `svm_*` together.

CometBFT already gives these metrics:

- `cometbft_consensus_height`,
- `block_interval_seconds`,
- `block_size_bytes`,
- `num_txs`,
- `rounds`,
- `missing_validators`,
- `validator_missed_blocks`,
- `peer_height` for each peer,
- `cometbft_state_block_processing_time`, which includes block execution.

This chain adds:

| Metric | Use |
| --- | --- |
| `svm_load_list_entries_total`, `svm_push_accounts_seconds` | Entries for each block and read time. A block that names accounts that it does not pay for shows here first. |
| `svm_arena_bytes`, `svm_arena_dropped_total` | What the last block loaded against the 512 MiB arena limit, and keys that blocks named past it. |
| `svm_rpc_simulations_waiting` | Simulations that wait for an `rpc-max-simulations` slot. A persistent queue means that you must raise the setting. |
| `svm_rpc_scans_waiting` | Full scans that wait for an `rpc-max-scans` slot. A persistent queue means that you must raise the setting. |
| `svm_rpc_requests_waiting` | HTTP requests that wait for an `rpc-threads` slot. A persistent queue means that you must raise the setting. |
| `svm_quic_total{event}`, `svm_quic_current{name}` | QUIC streamer counters: refused connections, connections over the rate, streams over 4,096 bytes, drops from a full queue, open connections. |
| `svm_ingress_total{path, outcome}` | Transactions given to the pool, by `path` (`rpc`, `quic`) and `outcome` (`admitted`, `rejected`, `duplicate`). `sendTransaction` answers the signature for each, as Agave does. A QUIC sender gets no answer. |
| `svm_admission_rejected_total{reason}` | Why the mempool refuses transactions: `rejected` (includes a compute budget or lookup table that the block would refuse), `blockhash_not_found`, `already_processed`, `unpayable`, and `host` (a failure of this machine, not of the transaction). |
| `svm_signature_results_total{event, handler}` | What the engine does with the results of signature checks. `CheckTx` keeps a result when a transaction goes into the pool (`written`, handler `check_tx`). `ProcessProposal` (`process_proposal`) and phase 1 of a block (`finalize_block`) use a result (`hit`) or check the signatures (`miss`). `ProcessProposal` keeps a result for each check that passes (`written`). The limit removes the oldest result (`evicted`). A result goes when its transaction commits or when its height ends (`forgotten`). |
| `svm_signature_results{tier}` | The results that the engine holds now. Tier `pool` is for the transactions in the pool. Its limit is two times `mempool.size`. Tier `block` is for the proposals of the current height. It is empty after each block. |
| `svm_signature_verifications_total` | Transactions whose signatures ed25519 checked. A transaction from the pool of this machine gets one check, at `CheckTx`. A transaction that this machine did not admit gets one check, in `ProcessProposal`. |
| `svm_proposals_refused_total{reason}` | Proposals that `ProcessProposal` refused, with the first reason: `decode`, `admission`, `budget`, `dedup`, `account_limit`, `cosmos` or `signature`. |
| `svm_block_admission_failed_total{reason}` | Transactions of a finalized block that phase 1 did not admit, with the reason `signature` or `admission`. Each gets status 2. |
| `svm_history_index_bytes`, `svm_history_purged_height` | The history index against `index-max-bytes`, and the oldest height that it still answers for. |
| `svm_history_compaction_failures_total` | Failed compactions of purged history. The purge stays. Only the disk space waits for the next compaction. A rising count means disk space that does not return. |
| `svm_dedup_entries` | Signatures in the replay set over the acceptance window. |
| `svm_block_units` against `svm_block_units_max` | How full the last block was. |
| `svm_block_program_text_bytes` against `svm_block_program_text_max_bytes` | The same for the program text budget. |
| `svm_program_cache_*` | Hits, misses, evictions, one-hit wonders, water level, and resident bytes against `program-cache-bytes`. |
| `svm_prewarm_misses_total` | Programs that a block compiled because the pre-warm pass did not hold them. |
| `svm_conflict_groups`, `svm_conflict_group_widest` | Parallelism of the last block. One group means serial execution. |

Set an alert for a full pool. Page when one of these is true at a height where `cometbft_mempool_rejected_txs` also increases:

- `cometbft_mempool_size` reaches `mempool.size`.
- `cometbft_mempool_size_bytes` is within 4,139 bytes (one maximum transaction) of `mempool.max_txs_bytes`.

A full pool refuses by arrival and removes nothing, so this alert means that transactions are lost. It is also the signal that the chain needs a pool of its own in the app. The counter alone is not enough, because it also counts a transaction that the pool already holds.

These are counters, not per-slot values. A Solana dashboard reads `program_cache_hits` per slot, because Agave resets its program cache statistics at each new bank. Nothing resets here, so read them as rates.

The hit rate of the signature results, for each handler, is near 1 on a validator that receives the transactions through its pool:

```
sum by (handler) (rate(svm_signature_results_total{event="hit"}[5m]))
  / sum by (handler) (rate(svm_signature_results_total{event=~"hit|miss"}[5m]))
```

A signature result goes when its transaction commits, not at recheck. Recheck can remove a transaction from the pool. The engine keeps its result until a block that carries the transaction commits, or until the limit removes the result. The results change the time that a block takes, not what the block does. A machine with no results, for example after a restart, checks each signature again and gets the same app hash.

`svm_program_cache_evictions_total` is a sum with no per-program label. A label for each evicted public key has no limit, and the person who deploys a program chooses the labels. That is the cheapest denial of service against an operator Prometheus.

None of these metrics is consensus. Machines that export different numbers get the same app hash. `app/metrics_test.go` checks this at each height, with one machine scraped.

### A refused proposal, and a transaction that phase 1 did not admit

[`examples/prometheus-alerts.yml`](../examples/prometheus-alerts.yml) has two alerts for these counters. Each fires on any increase in 10 minutes, for each machine and each reason. Group them in Alertmanager by the alert and the reason, so that one cause gives one notice:

```yaml
route:
  group_by: [alertname, reason]
```

`SVMProposalRefused` (warn) fires when `ProcessProposal` refuses a proposal. The round goes to the next proposer, and the chain continues. The log line `svm: proposal refused` gives the height, the block hash, the address of the proposer, the reason and the index of the transaction. It does not give the round, because CometBFT does not send it. The line never contains transaction bytes or signatures.

1. Read the reason.
2. If the reason is `signature` or `decode`, the proposer made a bad block. Find the validator from the proposer address, and tell its operator.
3. If the reason is `admission`, `budget`, `dedup`, `account_limit` or `cosmos`, find the cause before you blame the proposer. An honest proposer packs only what its own checks admit.
4. Look at the other validators. If only this machine refuses, the fault is probably on this machine. Compare its binary and its feature gates with the other machines.

`SVMBlockAdmissionFailed` (page) fires when phase 1 does not admit a transaction of a finalized block. The transaction gets status 2. It pays no fee, writes nothing and gets no record, and its replay key stays unspent. The rest of the block runs, and the chain does not stop. The log line `svm: block transaction not admitted` gives the height, the index of the transaction, the reason and the error of the engine.

1. Read the reason and the error.
2. Look for a `svm: proposal refused` line at the same height. If there is one, this machine refused the block, and two thirds of the voting power accepted it.
3. Find out if a feature gate explains the failed check. With your Cosmos client, list the passed proposals, and find a `MsgScheduleGates` for that height. Compare what the gate changes with the error.
4. If no gate explains it, compare the binary of this machine and its gates with the other machines.

This alert is an alarm. It is not a reason to stop consensus. Do not stop the machine.

### Health

`getHealth` on the Solana RPC returns `ok`, or the Agave error `-32005` with `data.numSlotsBehind`. It returns the error when this machine is more than eight blocks behind the highest known peer. It answers `ok` when it knows no peer. A machine cannot tell if it is alone or cut off, and a one-machine devnet is the usual case. It answers unhealthy with no number while CometBFT reports `catching_up`.

Peer heights come from `cometbft_consensus_peer_height`. If you turn instrumentation off, `getHealth` checks only `catching_up`.

`getGenesisHash` returns the sha256 of the genesis file that this machine started from. Compare it across machines before the first start. Section 8 tells why.

### Profile a node

Use these tools when a node is slow or uses too much memory. They read the running process and change nothing on the chain.

For the Go side, CometBFT serves Go pprof when `pprof_laddr` in `config.toml` has an address, for example `"localhost:6060"`. It is empty by default. When it has an address, run:

```
go tool pprof -top http://localhost:6060/debug/pprof/heap
go tool pprof -http :8081 'http://localhost:6060/debug/pprof/profile?seconds=30'
```

The first command prints the memory that each Go package holds. The second records CPU for 30 seconds and opens a flame graph in a browser.

The engine is Rust in the same process, and pprof does not see it. Use `perf`, which samples every thread. It comes with the `linux-tools` package of your kernel.

```
sudo perf record -F 99 -g -p "$(pgrep -n -x svmd)" -- sleep 30
sudo perf report --no-children
```

`-- sleep 30` sets the record time. For a flame graph, give the samples to a tool such as `inferno` (`cargo install inferno`):

```
sudo perf script | inferno-collapse-perf | inferno-flamegraph > svmd.svg
```

## 10. What this chain does not have

Each item is an intentional absence, not a gap. Read this before an incident.

- **No burn for a fault.** A validator that stops is jailed. A validator whose key signs twice is tombstoned. Neither loses stake.
- **No public validator set.** Only the operators named at genesis hold the bond denom. `MsgCreateValidator` from any other account fails, because the account has no coins.
- **No fee revenue.** The chain burns all SVM fees. A validator earns nothing on this chain.
- **No keyring and no gentx.** `svmd keys` does not exist, and `x/genutil` is not a module here. One command builds genesis from public keys and addresses. The operator account key never goes to this binary.
- **No faucet on a production binary.** A production build has no signing path and holds no key. It does not serve `requestAirdrop`, and no setting turns it on.
- **No CometBFT transaction index on a validator.** The relayer node keeps one.
- **No price feed.** The chain carries Pyth Core's programs, and no price service sends updates to it. A party that needs a live feed brings its own Pyth API key, because Hermes, Pyth's price service, needs a paid key. The party then pushes updates with Pyth's price pusher, or pulls them in its own transactions.
- **No replay from genesis.**

### Two binaries, and how to tell them apart

One commit builds two `svmd` binaries, a production binary and a devnet binary (`go build -tags devnet`). A release also gives `svm-tpu`, the QUIC port, which both builds use. Its manifest gives its version, commit, streamer, and sha256. Both `svmd` binaries report the same version and engine digest, for the reason in section 7.

The devnet build has a signing path and a key to sign with. The production build has neither.

Ask the binary:

    $ svmd version --long --output json
    ...
    "extra_info": {"engine_digest": "d0c645...", "network": "production"}

`network` is the compiled constant. `build_tags` is a string that the build writes, and it can be wrong. A release artifact has the same two fields in the `.yml` next to it, so you can read them without running anything.

The wrong binary does not start and names both networks. This is the backstop, not the only signal:

    svm: this is a production binary, the chain is a devnet chain

Both directions refuse. A production binary on a devnet chain does no damage, because it cannot sign. The binary refuses it anyway. If it did not, everything would appear to work, and a missing method would look like a bug.

A localnet usually needs the devnet build, because only that build has a faucet to fund accounts. A test run of a real deployment needs the production binary. `svmd version --long` reports which one you have.

### Run the devnet faucet

Only the devnet build has a faucet, and only the machine that holds a key serves it. Every other devnet machine answers `requestAirdrop` with `-32601`, as a production binary does.

WARNING: Do not copy one faucet key to every operator. A devnet is a chain, not a machine.

Fund two accounts, not one. Use a cold reserve (key never on the machine) and a hot faucet account (`faucet-key` in `app.toml`). Give the hot faucet the amount for one refill period, and keep the rest in the reserve. The chain cannot see the difference. The split is a convention that limits a theft.

The key is the account. There is no faucet address to set, so you cannot point the binary at the wrong account. The key format is `solana-keygen`. `svmd` has no subcommand that writes keys.

WARNING: Make the key file readable only by its owner. This key is the only private key in `svmd` that spends money.

1. Run these commands. They make the key, make it readable only by you, and print its public key:

       solana-keygen new --no-bip39-passphrase -o ~/.svmd/config/faucet_key.json
       chmod 600 ~/.svmd/config/faucet_key.json
       solana-keygen pubkey ~/.svmd/config/faucet_key.json

2. Give it its balance in the devnet genesis, before the first start:

       ./svmd-devnet add-genesis-account <pubkey> <sol>

   `developers.md` section 2 does this for the faucet and the reserve.
3. Put the key path in `faucet-key` under `[svm]` in `app.toml`.

The binary does not start if the key is missing, malformed, the wrong length, or readable by group or others. It names the path and the reason. This permission check is stricter than the CometBFT check on its own privval key. The difference is intentional, because this key spends money.

WARNING: A stolen faucet key is not only a loss of funds. Nothing mints, so the theft permanently ends the ability of this devnet to fund anyone.

To refill the hot account:

1. Make a System transfer from the reserve to the hot account.
2. Sign it offline.
3. Send it with `sendTransaction`.

To rotate the key after a theft:

1. Make a new keypair.
2. Send one transfer from the reserve to its public key.
3. Change the `faucet-key` line in `app.toml` to the new key.
4. Restart `svmd`.

You do not rewrite genesis or reset the chain. The stolen account becomes an empty account that nothing points to.

The limits are on by default. A caller gets 5 requests and 20 SOL each minute, and 200 requests and 200 SOL each day. One airdrop is at most 10 SOL. The faucet counts limits against the caller address and the receiver address. It counts requests and lamports. Each request costs the faucet the same 5,000-lamport fee for any size, so a lamport limit alone does not limit what the faucet spends. Loopback has no caller limit, so a test loop on the machine does not stop. A request through the gateway also comes from loopback. So for a request from loopback, the caller is the last address in its `X-Forwarded-For` header, which the gateway sets (`examples/gateway.nginx.conf`). If you put a proxy of your own on the machine, set this header in the same way. If you do not, its callers get no caller limit.

Each limit is a `[[svm.faucet-limit]]` table in `app.toml`. An operator can lower or raise the limits, or add an hourly limit between the two defaults. A refusal is a JSON-RPC error. It sends no transaction and costs the faucet nothing.

The limits do not cap what the faucet can lose. The real limit is the balance of the hot account. The limits give time and make casual abuse useless. Neither counter stops a caller that uses many addresses. A new keypair for each request defeats the receiver counter, and a keypair is free. That counter stops the retry loop of an honest client.

Set the alert at the floor, not at zero. The chain exports `svm_faucet_lamports` next to `svm_faucet_floor_lamports`. The account cannot go below its rent-exempt minimum, the floor (650,240 lamports at the genesis price). So the last airdrops before the faucet runs dry fail with `InsufficientFundsForRent`, while the balance still reads the floor. As the balance nears the floor, the fee payer check refuses at load with `InsufficientFundsForFee`. This costs nothing, because the chain does not commit a load failure and charges no fee. An alert at zero would say that funds are available while every request already fails.

## 11. Build from source

Every tool comes from `flake.nix`, at the version that this chain pins:

- Go 1.27,
- Rust 1.97.1 with `rustfmt` and `clippy`,
- a C toolchain with `make` and `perl`,
- the Solana CLI 4.3.0 with `solana-test-validator` and `cargo-build-sbf`,
- Anchor 0.31.1 and 1.0.2, as `anchor-0.31.1` and `anchor-1.0.2`,
- Node 22 with `pnpm` and `corepack`,
- `spl-token` 5.6.1,
- tuktuk's CLI and crank turner,
- Hermes 1.13.1,
- Playwright's Chromium,
- `shellcheck` and `protoc`.

CI enters the same shell.

1. [Install Nix](https://nixos.org/download/).
2. Turn flakes on.
3. In the repository, start the development shell:

       nix develop

   The first start downloads the tools and builds the three that come from crates.io. This takes
   about 20 minutes on four cores. A `.envrc` is there for `direnv`.

4. Set up once:

       make setup

   This runs four steps:

   - `make fetch-programs` downloads the genesis programs into `genesis/programs/`, and the
     programs that the tests deploy into `programs/`. It checks each one against its pin in
     `genesis/manifest.json` and `programs/manifest.json`. It reads a row that comes from mainnet
     from `https://api.mainnet-beta.solana.com`, and refuses it if its hash is not the pin.
   - `make build` and `make devnet` build `bin/svmd`, `bin/svm-tpu` and `bin/svmd-devnet`.
   - `make prebuild` builds every third-party suite that the acceptance runs use, once, into
     `target/third-party/`, at the pins in `scripts/pins.sh`. The suites are:
     - MagicBlock's example apps and test programs,
     - tuktuk's programs and examples,
     - the SP1 and RISC Zero verifiers, with Hyperlane's sources,
     - the Solana Explorer,
     - a stock ibc-go chain,
     - the JS clients of the harnesses, from their `pnpm-lock.yaml`.

   The first `make prebuild` takes about 25 minutes on four cores and needs network access. Set
   `SVM_THIRD_PARTY` to build somewhere else.

5. Build and test:

       make test

`make build` builds `svmd`, and `svm-tpu` from `tpu/`. `tpu/` has its own Cargo workspace and lockfile. The streamer never enters the engine lockfile or digest. The secp256r1 verifier links OpenSSL through the `openssl-vendored` feature, which is why `openssl-src` compiles OpenSSL from source during the first build.

These steps still reach the network at run time:

- `cargo-build-sbf` downloads Solana's platform tools into `~/.cache/solana` the first time it builds a program.
- `corepack` downloads the yarn version that a third-party suite pins.
- Each acceptance run copies the pinned programs from mainnet.
- The ephemeral rollup run also fetches MagicBlock's validator and VRF oracle releases. These are under the Business Source License 1.1, and the repository never keeps them.
- The Pyth replay reads mainnet transactions.

A run stops if a hash is not its pin.

Each `make accept-*` target checks that its tools are on the PATH and that `make prebuild` built its part. If the part is missing, the target builds it first. When a pin in `scripts/pins.sh` moves, the next run that asks for the part builds it again from nothing.

- `make accept-rpc` runs the unmodified `solana` CLI, `spl-token` and web3.js against a local chain.
- `make accept-clients` runs the pinned client suites and the Explorer. The first build of the Explorer is `make prebuild PARTS=explorer`, about two and a half minutes on four cores.
- `make accept-er` runs the ephemeral rollup acceptance run. It runs MagicBlock's example apps, a replay of Pyth price updates and MagicBlock's own test suites. It runs them two times, against `solana-test-validator` 4.3.0 and against this chain. A test that passes against Agave must pass against this chain, unless `rpc/accept/er/known.json` lists it. Give `PARTS=apps`, `PARTS=pyth` or `PARTS=suites` to run one part. The results of each chain and the verdict, `gate.txt`, go to `rpc/accept/results/er-*`.
- `make accept-settle` runs the settlement acceptance run in the same way. It runs Hyperlane's own sealevel e2e two times. The first run is as Hyperlane's CI runs it, on its own `solana-test-validator` 3.0.14. The second run is against this chain. It also runs SP1's and RISC Zero's fixture proofs and a Squads v4 upgrade. The e2e needs Docker, because it keeps a Postgres database in a container. Its first build of Hyperlane's agents takes about an hour on four cores. Give `PARTS=hyperlane`, `PARTS=proofs` or `PARTS=squads` to run one part.
- `make accept-tuktuk` runs tuktuk's own suite and the memo and cron flows with the crank turner, two times, judged against Agave. Give `PARTS=suite` or `PARTS=flows` to run one part.

Without Nix, the build needs the Rust toolchain in `rust-toolchain.toml`, a Go toolchain (the version in `go.mod`), a C compiler, `make` and `perl`.

NOTE: On a machine without `perl`, the build script of `openssl-src` fails with a message that does not mention OpenSSL.

The acceptance runs need the tools that `flake.nix` lists, at the versions that it pins. `make accept-clients` stops if `solana` is not 4.3.0 or `spl-token` is not 5.6.1. The Solana release bundle carries its own `spl-token`, which is not 5.6.1. Put the pinned one first on the PATH.

The verifiers are the Anza `agave-precompiles` crate, not a port. This chain accepts exactly what mainnet accepts, down to the crate version.

### What `cargo audit` reports

The tree has three advisories. This chain cannot fix any of them. Anza owns each pin, and a change would change what a signature check accepts. Mainnet runs all three.

- RUSTSEC-2022-0093, `ed25519-dalek` 1.0.1, fixed in 2.0. A double public key signing oracle. Only signing reaches it. The precompile only verifies and never makes a `Keypair`. The pin is the `=1.0.1` of `agave-precompiles`, and it is necessary. `verify_strict` of this crate sets the line between accept and refuse for every ed25519 signature that the chain accepts.
- RUSTSEC-2024-0344, `curve25519-dalek` 3.2.0, fixed in 4.1.3. Timing variation in `Scalar29::sub`. It matters only when the scalar is secret, and verification handles no secrets. This version comes with `ed25519-dalek` 1.0.1, so it moves when that pin moves. The tree also has 4.1.3, which the rest of the SVM uses.
- RUSTSEC-2025-0161, `libsecp256k1` 0.7.2, not maintained. The chain carries this crate through `solana-syscalls`, as mainnet does.

## 12. Ephemeral rollups

An ephemeral rollup validator runs transactions off this chain for the accounts that an app delegates to it. It commits their state back to this chain through MagicBlock's delegation program. This section tells you how to admit validators, what they cost, and what they earn.

### Admit your validators in genesis

WARNING: Admit only validators that you run. An admitted validator can do these things:

- It commits any data to an account that is delegated to it, with its signature only. No proof checks the data.
- It moves the lamports out of each escrow on the chain that is not delegated at that time, for each user. The delegation program checks only that the validator has a fees vault.

1. Make a keypair for each ephemeral validator that you will run.

2. Give each public key to `init`:

       ./svmd init my-validator --chain-id svm-local \
           --ephemeral-validator VALIDATOR_PUBKEY

   Repeat the flag for each validator. The flag is off by default, and both binaries have it.

3. Add the genesis programs, as for a localnet:

       ./svmd add-genesis-programs genesis

   With the flag, this step also reads four programs from mainnet and checks each one against its
   pin. The four are the delegation program, the committor, SPL Noop and session keys.

4. Fund the identity of each validator. The identity pays for each commit transaction. Use a
   genesis account or, on a devnet, the faucet.

`init` writes a validator fees vault for each key, and the protocol fees vault. Without the flag, genesis has none of the four programs and no vault. The programs have no upgrade authority, so nobody can add a vault later. To admit a different set of validators, make a new network.

The delegation program does not check that an app names an admitted validator. If an app names a validator that you did not admit, its account stays delegated until the timeout of the owner program: 9,000 slots. Here a slot is a block, so this is about an hour on an idle chain.

### The license

The delegation program, the committor and the ephemeral validator are under the Business Source License 1.1. Their change date is 2027-12-01. After that date, each is under the MIT license. Before a production deployment, check that you have the right to use each BSL component that you turn on. If you do not, get a commercial license from MagicBlock. This page and the flag do not give you a right that the license does not give. [`genesis/licenses`](../genesis/licenses/README.md) holds the license texts beside the pins.

### Set the commit price from the load of this chain

Set `[commit] compute-unit-price` in the configuration of the validator from `getRecentPrioritizationFees` on this chain. While blocks are not full, set it to 0. Increase it only when commits must outbid other transactions. This chain burns each fee, and a priority fee buys only a place in the block.

These costs are for validator v1.0.2 and the counter app. They are the same against `solana-test-validator` 4.3.0:

| Transaction that the validator pays | Fee at the default price | Fee at price 0 | Block units here |
| --- | --- | --- | --- |
| A commit of one account | 125,000 lamports | 5,000 lamports | 147,606 |
| A commit that also undelegates | 245,000 lamports | 5,000 lamports | 274,343 |

When the validator starts, it makes and delegates its magic fee vault. This costs 10,000 lamports of fees, 3,892,480 lamports of rent, and 449,682 block units.

The committor declares 120,000 compute units for each commit and uses about 1,213. A block counts what a transaction declares, so a commit costs 147,606 block units. A block of 60,000,000 units holds about 406 commits.

### What a validator earns

At undelegation, the delegation program charges the app's user a session fee of 3,000,000 lamports, plus 1,000,000 for each commit after the first. The fee comes from the rent of the account's delegation records, and it cannot be more than that rent.

- 90% of the fee goes to the validator fees vault, so a session gives at most 90% of its delegation rent.
- 10% goes to the protocol fees vault. Nobody can claim it on this chain, because the claim needs the program's upgrade authority, and there is none.

For the counter app, the delegation rent is 2,895,360 lamports, so each session gives 2,605,824 to the validator. A session breaks even at about 20 commits at the default price, and at about 520 commits at price 0.

The income goes to the validator fees vault, not to the identity. The validator claims it every 24 hours by default. 90% goes to the identity, and 10% goes to the protocol fees vault. The claim leaves the vault at its rent-exempt minimum.

### Admit your VRF oracles

MagicBlock's VRF program gives verifiable random numbers to apps on the base chain and on the rollup. An oracle that you run answers the requests.

WARNING: Admit only oracles that you run. The VRF program has no upgrade authority, so nobody can add an oracle later.

1. Get the VRF public key of each oracle from its keypair. `vrf-cli` takes the keypair as base58,
   the 64 bytes of the keypair file:

       vrf-cli -k ORACLE_KEYPAIR_BASE58 derive-pubkey

2. Give each oracle to `init`, as its identity and its VRF public key:

       ./svmd init my-validator --chain-id svm-local \
           --vrf-oracle ORACLE_PUBKEY:VRF_PUBKEY

   Repeat the flag for each oracle. The flag is off by default, and both binaries have it.

3. Do step 3 of the previous procedure. This step also reads the VRF program from mainnet.

4. Run the oracle at its v0.4.1 release, as [`examples/vrf-oracle.service`](../examples/vrf-oracle.service)
   runs it. Genesis registers it at slot 0.

5. Wait for block 200. Before it, the oracle cannot open its queues. On an idle chain, block 200 comes
   after about 80 seconds.

6. Open the queue of the base chain and the queue of the rollup (`InitializeOracleQueue`).

7. Delegate the queue of the rollup to your ephemeral validator (`DelegateOracleQueue`).

`init` writes the oracle list of the VRF program and the data of each oracle, as `ModifyOracles` does.

A delegated request on your own queue fails. The VRF program at mainnet's pin charges a fee on each queue that is not MagicBlock's default queue. The rollup refuses to move the lamports for that fee, so the request fails on the rollup. It fails in the same way against `solana-test-validator` 4.3.0. A request on the queue of the base chain works.

The VRF program and the VRF oracle are under the Business Source License 1.1 until 2029-12-01. Until then, use them only for tests and devnets, or get a commercial license from MagicBlock. [`genesis/licenses`](../genesis/licenses/README.md) holds the license text beside the pin.

## 13. Settlement

This chain is a settlement layer for SVM rollups. Settlement is in SVM programs that you deploy after genesis. No Cosmos module does settlement.

Genesis puts Squads v4 on every network, at `SQDS4ep65T869zMMBKyuUq6aD6EgTu8psMjkvj52pCf`, with mainnet's bytes. Mainnet's Squads has no upgrade authority, so its code does not change. Genesis writes the global config of Squads itself. The fee to make a multisig is 0, and the authority and the treasury are at an address that nobody can sign for, so nobody can ever set a fee on this chain.

### Deploy Hyperlane

1. Build Hyperlane's Sealevel programs from its tag `agents-v2.3.0` (`97cbefba`) with the Agave
   CLI 3.0.14 (`cargo build-sbf`).

2. Check the executable hash of each program against this table. The executable hash is the
   sha256 of the file without its trailing zero bytes, which `solana-verify` reports. The
   settlement acceptance run tests these builds.

   | Program | Executable sha256 |
   | --- | --- |
   | `hyperlane_sealevel_mailbox.so` | `9f3cd9cbb94bf129c173d423876d40fbf15b29d2d8d7bc1806f29958265af318` |
   | `hyperlane_sealevel_multisig_ism_message_id.so` | `8d348735b24024e0f30dfbbdc3cb2a2292443329ec24b1590abd07892fe78511` |
   | `hyperlane_sealevel_validator_announce.so` | `5a8c0e11c6dedf2b8526805617d500d2bf4847c93db83b60ebae1bf9c4d72da1` |
   | `hyperlane_sealevel_igp.so` | `127de85a0524edb9c491cbc96f3a45a7d62e20a31d8278d6fccbd4a73a0edf11` |
   | `hyperlane_sealevel_token.so` | `4328d5ce2c196433ea250b5fc14377f9810652879266c9ba65a773b70333c112` |
   | `hyperlane_sealevel_token_collateral.so` | `bf5ec19e1e369ffca3eefe4cf6b7ef102225f0287f912b1bafefbde521b0cb05` |
   | `hyperlane_sealevel_token_native.so` | `359a325bad86ca3bd4fd762dff75ffff3f4b271089126bd6472d93961bdf9d07` |
   | `hyperlane_sealevel_token_cross_collateral.so` | `531dcb1648ea2e7755a3fe00fe1f326ba0b5b293c0b455f97a1a437ca4e5c55f` |
   | `hyperlane_sealevel_composite_ism.so` | `5cd0ad669812e872a28a6b30c96f377d0dbe69cd91e0693c72903b21ab06efc8` |

3. Deploy each program with `solana program deploy`.

4. Give the network its own Hyperlane domain id.

5. Run Hyperlane's agents from the same tag. These are the validators that your ISM trusts, and a relayer.
   You operate these agents, as you operate the ephemeral validators.
   [`examples/hyperlane-relayer.service`](../examples/hyperlane-relayer.service) and
   [`examples/hyperlane-validator.service`](../examples/hyperlane-validator.service) are templates
   for the two.

### Keep the keys in a Squads vault

Each settlement program has two keys, its upgrade authority and an owner in its state. The upgrade authority can replace the code of the program.

WARNING: Protect both keys. The owner of a warp route can send bridged funds to a different place.

1. Connect the `@sqds/multisig` SDK or the Squads CLI to the RPC of this chain. The Squads web
   app is for Solana only.

2. Make a Squads multisig with that SDK or CLI.

3. Choose the members and the threshold. Use at least 2 of 3 members, on different devices.

4. Set a time lock, so that users can exit before an upgrade or a change of owner occurs.

5. Move the upgrade authority and the owner of each settlement program to the vault of the
   multisig. Hyperlane's client has a `squads` command for this. The settlement programs are the
   mailbox, the ISM, the IGP, the validator announce program, each warp route and the RISC Zero
   router.

### Validity proofs

- **RISC Zero.** Build the Groth16 verifier and the verifier router from `risc0-solana` v3.0.0 for your network, with your own program ids and `INITIAL_OWNER`. Then give the upgrade authority of the verifier to the PDA of the router. The router decides which verifier to use.
- **SP1.** `sp1-solana` (`4181cae`) is a library. You compile it into the program of your app, so there is no program to deploy and no key.

### What this chain does not do

- **No Cosmos module for settlement.** A token that comes through IBC stays an `ibc/` voucher in the bank module. It does not become an SPL token.
- **No data availability.** A rollup keeps its data on its own data availability layer. This chain does not store it.
- **No fault proofs.** The settled rollups use attestations or validity proofs. No optimistic fault game runs here.

## 14. Cranks

Helium's tuktuk is on every network. Genesis puts the tuktuk program, its cron program, mainnet's tuktuk config and both IDL accounts on the chain. An app queues a task on a task queue, and a crank turner runs the task when its trigger comes. The chain runs no cranks. A task runs only when a crank turner watches the chain, so run one.

### Run a crank turner

1. Install the crank turner and the CLI from crates.io, at the versions that the acceptance run
   pins:

       cargo install --locked tuktuk-crank-turner@0.2.31 tuktuk-cli@0.2.18

2. Make a keypair for the turner:

       solana-keygen new --no-bip39-passphrase -o /etc/svmd/turner.json

3. Fund the turner from an account that you hold, or from the faucet on a devnet. The turner pays
   the fee of each crank, 5,001 lamports, and receives the reward of the task.

4. Run the turner as a service, with a floor of 10,000 lamports.
   [`examples/tuktuk-crank-turner.service`](../examples/tuktuk-crank-turner.service) is a unit
   that points at the RPC of the same machine:

       install -m 644 examples/tuktuk-crank-turner.service /etc/systemd/system/
       systemctl enable --now tuktuk-crank-turner

   CAUTION: Keep `Restart=always` in the unit. The turner exits when the RPC closes the websocket
   without a handshake. The README of the turner says the same.

   The turner also uses the websocket. It takes the websocket address from the RPC address, so
   here it uses port 8900.

   The turner can run on a different machine, because it is only an RPC client. Set
   `TUKTUK__RPC_URL` to the RPC of any machine of the network, or to the gateway. Open the RPC
   port and the websocket port of that machine to the machine of the turner. These ports are
   `rpc-address` and `ws-address` under `[svm]` in `app.toml` (section 4). The turner needs no key
   of the chain, no `svmd` and no genesis file. It needs its own funded key.

5. Check that it runs. The turner serves metrics on port 8080. Set `TUKTUK__METRICS_PORT` to use
   a different port.

       curl -s http://127.0.0.1:8080/metrics | grep tuktuk

The turner is not part of `svmd`, and no validator must run one. One turner serves every queue on a network. Anyone may run more. The program pays the turner that lands a task first.

### What a crank costs and earns

- A task pays the crank reward of its queue, at least the minimum that the queue names. The turner above runs no task that pays less than 10,000 lamports.
- A `RunTaskV0` pays 5,001 lamports of fee. The turner adds a compute unit price of 1 to each transaction. It costs 58,428 units against the block, so a block holds about 1,000 cranks. A transaction that declares no compute budget costs about 232,000 units, because this chain counts the declared budget (section 1 of the compatibility page).
- A queue posts a deposit of 1 SOL. The deposit comes back when the queue closes.
- A task with a time trigger runs in the first block whose `Clock.unix_timestamp` reaches the time. The turner reads accounts at `processed` and simulates at `confirmed`. On this chain both are one level, so its first attempt lands. On Agave the first attempt often fails, and the turner tries again 30 to 90 s later.
- A cron job queues its transactions five minutes ahead. If an app adds the transactions after it creates the job, the job skips its first execution. Add the transactions first.
