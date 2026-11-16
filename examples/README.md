# Example files for the chain and every service beside it

This directory has one file for each thing that an operator runs. The header of each file gives the
install steps and the runbook section that explains it. To use a file:

1. Copy the file.
2. Fill in the values marked `<...>`.
3. Install the file where its header says.

The two `app.toml` files are the configuration of `svmd` itself. Each other file is a service that
runs beside a machine of the chain.

| File | What it runs | Runbook |
| --- | --- | --- |
| [`app.toml`](app.toml) | The `app.toml` of `svmd` for a production network | [Section 4](../docs/runbook.md#4-apptoml) |
| [`app.devnet.toml`](app.devnet.toml) | The `app.toml` of `svmd` for a devnet, with the faucet | [Section 4](../docs/runbook.md#4-apptoml) |
| [`svmd.service`](svmd.service) | A machine of the chain, under cosmovisor | [Sections 5 and 7](../docs/runbook.md#5-join-an-existing-chain) |
| [`prometheus.yml`](prometheus.yml), [`prometheus-alerts.yml`](prometheus-alerts.yml) | The scrape of a machine's metrics, and the alerts that runbook section 9 asks for | [Section 9](../docs/runbook.md#9-metrics) |
| [`gateway.nginx.conf`](gateway.nginx.conf) | The gateway, one endpoint for this chain's RPC and the RPC of the EVM chain beside it | [Section 4](../docs/runbook.md#4-apptoml) |
| [`hermes.toml`](hermes.toml), [`hermes.service`](hermes.service) | Hermes, the IBC relayer, beside the relayer node | [Sections 4 and 5](../docs/runbook.md#5-join-an-existing-chain) |
| [`ephemeral-validator.toml`](ephemeral-validator.toml), [`ephemeral-validator.service`](ephemeral-validator.service) | MagicBlock's ephemeral validator, which runs ephemeral rollups on this chain | [Section 12](../docs/runbook.md#12-ephemeral-rollups) |
| [`vrf-oracle.service`](vrf-oracle.service) | MagicBlock's VRF oracle, for apps on the base chain and the ephemeral rollup | [Section 12](../docs/runbook.md#12-ephemeral-rollups) |
| [`hyperlane-relayer.service`](hyperlane-relayer.service), [`hyperlane-validator.service`](hyperlane-validator.service) | Hyperlane's agents, for the SVM rollups that settle here | [Section 13](../docs/runbook.md#13-settlement) |
| [`tuktuk-crank-turner.service`](tuktuk-crank-turner.service) | Helium's crank turner, which runs the tasks apps queue on tuktuk | [Section 14](../docs/runbook.md#14-cranks) |

## One convention for every unit

The services are binaries from other projects. Each takes its settings in its own way:

- `svmd` and Hermes take flags and a file.
- MagicBlock's validator takes a file and environment variables.
- The VRF oracle and tuktuk's turner take flags or environment variables.
- Hyperlane's agents take a JSON file and environment variables.

The units do not follow the habit of each tool. They follow one rule:

- A value that changes from machine to machine is an `Environment=` line.
- A secret is in an `EnvironmentFile=` that only the user of the service can read.
- A tool that takes a configuration file gets one here, and the unit names it.
- `ExecStart` carries only the flags that a tool cannot take in another way.

So `vrf-oracle` and `tuktuk-crank-turner` run with no flags, although both accept them. `svmd` and
Hermes run with flags, because they take those settings only as flags.

WARNING: Do not put a secret on a command line, because `ps` shows it. No unit here does this.

The Hyperlane units are templates, because the agent configuration is Hyperlane's own format for
your network.

## What needs no service

Pyth Core and Squads v4 are programs in genesis. You use them through their own SDKs against the RPC
of this chain (runbook sections 10 and 13). A live Pyth feed is the pusher of whoever brings a Pyth
API key.

## Licenses

The ephemeral validator and the VRF oracle are under the Business Source License 1.1 until their
change dates. Hermes, Hyperlane's agents and tuktuk's turner are under Apache-2.0.

WARNING: Check that you have the right to run the ephemeral validator and the VRF oracle in
production ([`genesis/licenses`](../genesis/licenses/README.md)).
