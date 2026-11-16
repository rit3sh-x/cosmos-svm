//go:build devnet

package main

import (
	"fmt"
	"time"

	"github.com/rit3sh-x/cosmos-svm/rpc"
)

// FaucetConfig holds the faucet's machine-local settings. Only a devnet
// binary writes them to app.toml, because a production binary has no
// faucet.
type FaucetConfig struct {
	FaucetKey         string `mapstructure:"faucet-key" usage:"the faucet's keypair file; unset runs no faucet"`
	FaucetMaxLamports uint64 `mapstructure:"faucet-max-lamports" usage:"the largest single airdrop, in lamports; 0 is 10 SOL"`
	FaucetLimit       any    `mapstructure:"faucet-limit" flag:"-"`
}

const faucetTemplate = `
# The faucet's keypair, in solana-keygen's format: a JSON array of the 64
# keypair bytes. The key is the account, so there is no faucet address to set.
# Unset means this machine runs no faucet and requestAirdrop is not served,
# which is the normal state of every machine in a devnet but one. Set to a
# file that is missing, malformed, the wrong length or readable by group or
# other and the binary refuses to start.
#
#   solana-keygen new --no-bip39-passphrase -o ~/.svmd/config/faucet_key.json
#   solana-keygen pubkey ~/.svmd/config/faucet_key.json   # the genesis row
faucet-key = "{{ .SVM.FaucetKey }}"

# The largest single airdrop, in lamports. 0 is the shipped default of 10 SOL.
faucet-max-lamports = {{ .SVM.FaucetMaxLamports }}

# The rate limit, one row per window, evaluated against both the caller's
# address and the recipient's. A request is refused if either the request
# count or the lamport volume would be exceeded, because every granted request
# costs the faucet the same 5,000-lamport fee whatever its size, so a volume
# cap alone does not bound what the faucet actually spends. With no rows the
# shipped defaults are 5 requests and 20 SOL a minute, 200 requests and 200 SOL
# a day. Loopback is exempt from the caller half.
#
# [[svm.faucet-limit]]
# window = "1m"
# requests = 5
# lamports = 20000000000
`

// requireFaucet loads the key and the rate limits before anything starts, so
// svmd reports a bad key or limit at start, while the operator watches.
func requireFaucet(cfg SVMConfig) error {
	limits, err := faucetLimits(cfg.FaucetLimit)
	if err != nil {
		return err
	}
	return rpc.LoadFaucet(cfg.FaucetKey, limits, cfg.FaucetMaxLamports)
}

// faucetLimits reads the [[svm.faucet-limit]] rows. Viper returns the array
// of tables as []any of map[string]any, and this function decodes it.
func faucetLimits(raw any) ([]rpc.FaucetLimit, error) {
	rows, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("faucet-limit is not a list of tables: see app.toml")
	}
	out := make([]rpc.FaucetLimit, 0, len(rows))
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("faucet-limit row %v is not a table", r)
		}
		text, _ := m["window"].(string)
		d, err := time.ParseDuration(text)
		if err != nil {
			return nil, fmt.Errorf("faucet-limit window %q: %w", text, err)
		}
		requests, err := field(m, "requests")
		if err != nil {
			return nil, err
		}
		lamports, err := field(m, "lamports")
		if err != nil {
			return nil, err
		}
		out = append(out, rpc.FaucetLimit{Window: d, Requests: requests, Lamports: lamports})
	}
	return out, nil
}

func field(m map[string]any, name string) (uint64, error) {
	switch v := m[name].(type) {
	case int64:
		if v < 0 {
			return 0, fmt.Errorf("faucet-limit %s is %d", name, v)
		}
		return uint64(v), nil
	case int:
		if v < 0 {
			return 0, fmt.Errorf("faucet-limit %s is %d", name, v)
		}
		return uint64(v), nil
	default:
		return 0, fmt.Errorf("faucet-limit %s is missing or not a number", name)
	}
}
