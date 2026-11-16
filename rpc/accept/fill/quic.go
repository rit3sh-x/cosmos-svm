package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// quic sends a signed transfer with tools/quic-client to the tpuQuic that the
// machine at url advertises, and the transfer must land. With after above
// zero, quic reads the address first. It then waits until the chain moves at
// least after blocks and the slot leader changes, and sends to the address it
// read, to show that a client that holds a cluster snapshot still lands.
func quic(url, client string, nonce, after int) error {
	var nodes []struct {
		TPUQuic *string `json:"tpuQuic"`
	}
	if err := call(url, "getClusterNodes", []any{}, &nodes); err != nil {
		return err
	}
	if len(nodes) == 0 || nodes[0].TPUQuic == nil {
		return fmt.Errorf("%s advertises no tpuQuic", url)
	}
	tpu := *nodes[0].TPUQuic
	var leader string
	if err := call(url, "getSlotLeader", []any{}, &leader); err != nil {
		return err
	}
	if after > 0 {
		var start uint64
		if err := call(url, "getSlot", []any{}, &start); err != nil {
			return err
		}
		for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
			if time.Now().After(deadline) {
				return fmt.Errorf("the leader of %s did not change in five minutes", url)
			}
			var now uint64
			var current string
			if call(url, "getSlot", []any{}, &now) != nil || call(url, "getSlotLeader", []any{}, &current) != nil {
				continue
			}
			if now >= start+uint64(after) && current != leader {
				break
			}
		}
	}
	blockhash, err := latestBlockhash(url)
	if err != nil {
		return err
	}
	tx := tc.Tx(blockhash, payer(nonce), nil, tc.Transfer(payer(nonce), tc.Pub(tc.Key(byte(100+nonce))), 1_000_000+uint64(nonce)))
	file := filepath.Join(os.TempDir(), fmt.Sprintf("svm-quic-%d-%d", os.Getpid(), nonce))
	if err := os.WriteFile(file, tx, 0o600); err != nil {
		return err
	}
	defer os.Remove(file)
	if out, err := exec.Command(client, "send", tpu, file).CombinedOutput(); err != nil {
		return fmt.Errorf("quic-client: %w: %s", err, out)
	}
	sig := base58.Encode(tx[1:65])
	failure, err := landed(url, sig)
	if err != nil {
		return err
	}
	if failure != "null" {
		return fmt.Errorf("%s landed with %s", sig, failure)
	}
	fmt.Printf("quic: %s landed through %s's tpuQuic %s (leader %s when it was read)\n", sig, url, tpu, leader)
	return nil
}
