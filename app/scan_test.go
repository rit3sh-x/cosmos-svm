package app_test

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/cosmos/cosmos-sdk/baseapp"
	pruningtypes "github.com/cosmos/cosmos-sdk/store/v2/pruning/types"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// pruned returns options for a chain on disk that keeps two versions besides
// the latest, as the shipped app.toml does. Like every test chain, it prunes
// inside Commit, not in IAVL's background goroutine. It keeps almost no node
// cache, so a node that pruning deletes is gone before the next block starts.
func pruned(accounts []svm.GenesisAccount) tc.Options {
	return tc.Options{Disk: true, Accounts: accounts, BaseApp: []func(*baseapp.BaseApp){
		baseapp.SetPruning(pruningtypes.NewCustomPruningOptions(2, 10)),
		baseapp.SetIAVLCacheSize(1),
	}}
}

// churn commits blocks that each create accounts across every shard until
// the version at height is pruned. Pruning has then deleted every node of that
// version that the new accounts orphaned, which is most of its inner nodes.
func churn(t *testing.T, c *tc.Chain, height uint64) {
	faucet := tc.Key(1)
	for block := 0; ; block++ {
		if _, err := c.App.CommitMultiStore().CacheMultiStoreWithVersion(int64(height)); err != nil {
			return
		}
		if block == 20 {
			t.Fatalf("height %d is still held %d blocks later, so pruning never reached it", height, block)
		}
		var txs [][]byte
		for i := range 32 {
			to := sha256.Sum256(binary.LittleEndian.AppendUint32([]byte("churn"), uint32(block*32+i)))
			txs = append(txs, tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, to, svm.MinimumBalance(0)+uint64(i))))
		}
		results, _ := c.Block(txs...)
		for i, res := range results {
			if res.Code != 0 {
				t.Fatalf("block %d transfer %d: code %d", block, i, res.Code)
			}
		}
	}
}

// TestAScanOutlivesThePruningOfItsVersion proves that a scan reads every
// account after its version is pruned. IAVL pins no version for an iterator,
// so an iterator on an older version walks the tree, and a pruned node ends
// the walk with no error. A scan opened at the latest version reads IAVL's
// fast-node index from a fixed database snapshot instead.
func TestAScanOutlivesThePruningOfItsVersion(t *testing.T) {
	const owned = 2_000
	program := tc.Pub(tc.Key(90))
	accounts := make([]svm.GenesisAccount, 0, owned)
	for i := 0; i < owned; i++ {
		pubkey := sha256.Sum256(binary.LittleEndian.AppendUint32([]byte("owned"), uint32(i)))
		accounts = append(accounts, svm.GenesisAccount{
			Pubkey: base58.Encode(pubkey[:]), Lamports: svm.MinimumBalance(8),
			Owner: base58.Encode(program[:]), Data: make([]byte, 8), RentEpoch: ^uint64(0),
		})
	}
	c := tc.Start(t, tc.Pub(tc.Key(1)), pruned(accounts))
	c.Block()
	_, height, scan, err := c.App.ScanLatest()
	if err != nil {
		t.Fatal(err)
	}
	churn(t, c, height)
	read := 0
	err = scan.Each(func(_ [32]byte, value []byte) error {
		a, err := ffi.DecodeAccount(value)
		if err == nil && a.Owner == program {
			read++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if read != owned {
		t.Fatalf("the scan of height %d read %d of the program's %d accounts after the height was pruned", height, read, owned)
	}
}
