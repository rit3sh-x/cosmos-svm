package genesis

import (
	"crypto/sha256"
	"os"
)

// Hash returns the sha256 of a finished genesis file. This chain uses it where
// Solana uses its genesis hash. `getGenesisHash` returns it, and an operator
// compares it across machines before the first start.
func Hash(path string) ([32]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}
