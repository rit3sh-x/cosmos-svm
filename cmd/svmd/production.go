//go:build !devnet

package main

// FaucetConfig is empty in a production build. A production build has no
// faucet, so app.toml has no faucet setting.
type FaucetConfig struct{}

const faucetTemplate = ""

// requireFaucet does nothing in a production build, which has no faucet key.
// A devnet build checks its key file here at start.
func requireFaucet(SVMConfig) error { return nil }
