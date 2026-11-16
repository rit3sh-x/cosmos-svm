//go:build !devnet

package rpc

// devnetMethod is the hook a devnet build serves its extra method through.
// A production build has no extra method, no faucet key and no code that
// signs.
func devnetMethod(string, string) (handler, bool) { return nil, false }

// devnetReady runs once per Server. A production build has nothing to
// attach to one.
func devnetReady(*Server) {}
