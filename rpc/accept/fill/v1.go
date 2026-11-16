package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// maxV1 is a V1 transaction's size limit, solana_message::v1's
// MAX_TRANSACTION_SIZE. Legacy and v0 stay at the packet's 1,232.
const maxV1 = 4096

// v1Transfer returns the V1 transfer that mode=v1 sends. V1 carries its limits
// in the message and has no defaults, so the transfer names both: a transfer's
// units with room to spare, and a megabyte of loaded data.
func v1Transfer(blockhash [32]byte) []byte {
	return tc.TxV1(blockhash, tc.V1Config{ComputeUnitLimit: 10_000, LoadedAccountsDataSize: 1 << 20}, payer(0), nil,
		tc.Transfer(payer(0), tc.Pub(tc.Key(201)), 1_000_000))
}

// buildV1 returns a V1 filler of exactly size bytes: a memo padded to that
// size, with the compute and loaded-data limits inline in the message, where
// V1 carries them.
func buildV1(key ed25519.PrivateKey, blockhash [32]byte, units uint32, nonce, size int) []byte {
	head := []byte(fmt.Sprintf("%d:", nonce))
	tx := func(n int) []byte {
		body := bytes.Repeat([]byte{'a'}, n)
		copy(body, head)
		return tc.TxV1(blockhash, tc.V1Config{ComputeUnitLimit: units, LoadedAccountsDataSize: 32 * 1024}, key, nil,
			tc.Instruction{Program: memo, Data: body})
	}
	n := size
	for i := 0; i < 3; i++ {
		n += size - len(tx(n))
	}
	return tx(n)
}

func send64(url string, tx []byte) (string, error) {
	var sig string
	err := call(url, "sendTransaction", []any{base64.StdEncoding.EncodeToString(tx),
		map[string]any{"encoding": "base64", "skipPreflight": true}}, &sig)
	return sig, err
}

// landed waits for one signature's status and returns its error, or "null" for
// success.
func landed(url, sig string) (string, error) {
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		var out struct {
			Value []*struct {
				Err jsontext.Value `json:"err"`
			} `json:"value"`
		}
		if err := call(url, "getSignatureStatuses", []any{[]string{sig}}, &out); err != nil {
			return "", err
		}
		if len(out.Value) == 1 && out.Value[0] != nil {
			return string(out.Value[0].Err), nil
		}
	}
	return "", fmt.Errorf("%s did not land in two minutes", sig)
}

// rent is -mode rent, the two sides of the gate flip. The rent-exempt minimum
// of an empty account is 128 bytes at the Rent sysvar's price: 5,080 a byte
// from genesis and 2,575 from the flip.
func rent(url, phase string) error {
	perByte := map[string]uint64{"before": 5_080, "after": 2_575}[phase]
	if perByte == 0 {
		return fmt.Errorf("-phase is before or after, not %q", phase)
	}
	var got uint64
	if err := call(url, "getMinimumBalanceForRentExemption", []any{0}, &got); err != nil {
		return err
	}
	if got != 128*perByte {
		return fmt.Errorf("%s the flip: the rent-exempt minimum is %d, want %d at %d a byte", phase, got, 128*perByte, perByte)
	}
	fmt.Printf("%s the flip: the rent-exempt minimum is %d, %d a byte\n", phase, got, perByte)
	return nil
}

// v1 is -mode v1. enable_tx_v1 is active from genesis, so a V1 transfer lands.
// getTransaction answers it only to a client that asks for version 1.
// simulateTransaction decodes a 4,096-byte V1 and refuses a 4,097-byte one at
// decode, with Agave's message.
func v1(url string) error {
	blockhash, err := latestBlockhash(url)
	if err != nil {
		return err
	}
	sig, err := send64(url, v1Transfer(blockhash))
	if err != nil {
		return fmt.Errorf("a V1 transfer: %w", err)
	}
	if status, err := landed(url, sig); err != nil || status != "null" {
		return fmt.Errorf("the V1 transfer %s landed with %s (%v)", sig, status, err)
	}

	var ignored jsontext.Value
	err = call(url, "getTransaction", []any{sig, map[string]any{"encoding": "base64"}}, &ignored)
	if err == nil || !strings.Contains(err.Error(), "-32015") {
		return fmt.Errorf("getTransaction without maxSupportedTransactionVersion: %v, want -32015", err)
	}
	var got struct {
		Version jsontext.Value `json:"version"`
	}
	if err := call(url, "getTransaction", []any{sig, map[string]any{"encoding": "base64", "maxSupportedTransactionVersion": 1}}, &got); err != nil || string(got.Version) != "1" {
		return fmt.Errorf("getTransaction with maxSupportedTransactionVersion 1: version %s (%v)", got.Version, err)
	}

	for _, size := range []int{maxV1, maxV1 + 1} {
		tx := buildV1(payer(1), blockhash, 200_000, size, size)
		if len(tx) != size {
			return fmt.Errorf("built a %d byte V1, want %d", len(tx), size)
		}
		var res jsontext.Value
		err := call(url, "simulateTransaction", []any{base64.StdEncoding.EncodeToString(tx),
			map[string]any{"encoding": "base64", "sigVerify": false}}, &res)
		switch {
		case size == maxV1 && err != nil:
			return fmt.Errorf("simulateTransaction refused a %d byte V1: %w", size, err)
		case size > maxV1 && (err == nil || !strings.Contains(err.Error(), fmt.Sprintf("too large: %d bytes (max: %d bytes)", size, maxV1))):
			return fmt.Errorf("simulateTransaction on a %d byte V1: %v, want refused at decode", size, err)
		}
	}
	fmt.Printf("the V1 transfer %s landed; getTransaction answers it only with "+
		"maxSupportedTransactionVersion 1; simulateTransaction decodes %d bytes and refuses %d at decode\n",
		sig, maxV1, maxV1+1)
	return nil
}

// v1fill is -mode v1fill. It sends maximum-size V1 transactions at once while
// it samples node's pool, to show that the pool's byte cap holds under V1.
// sendTransaction answers with the signature whatever the pool says, as Agave
// does (rpc/send.go). The pool shows full by its own count: a sample with
// no room under capBytes for one more transaction. v1fill sends until a sample
// is full, then as many again while the pool holds there, at most count.
//
// It prints how many it sent, how many sends got a signature, how many samples
// were full, how many sends failed, the most transactions and bytes the pool
// held, and the seconds it took.
func v1fill(urls []string, node string, payers, count int, capBytes int64) error {
	blockhash, err := latestBlockhash(urls[0])
	if err != nil {
		return err
	}
	probe := buildV1(payer(0), blockhash, 400_000, 0, maxV1)
	var sim struct {
		Value struct {
			Err           jsontext.Value `json:"err"`
			UnitsConsumed uint64         `json:"unitsConsumed"`
		} `json:"value"`
	}
	if err := call(urls[0], "simulateTransaction", []any{base64.StdEncoding.EncodeToString(probe),
		map[string]any{"encoding": "base64", "sigVerify": false}}, &sim); err != nil {
		return err
	}
	if len(sim.Value.Err) != 0 && string(sim.Value.Err) != "null" {
		return fmt.Errorf("a V1 filler failed to simulate: %s", sim.Value.Err)
	}
	units := uint32(sim.Value.UnitsConsumed + sim.Value.UnitsConsumed/10 + 16)

	var peakTxs, peakBytes, full atomic.Int64
	done := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			var out struct {
				Result struct {
					Total      string `json:"total"`
					TotalBytes string `json:"total_bytes"`
				} `json:"result"`
			}
			if get(node+"/num_unconfirmed_txs?limit=1", &out) == nil {
				n, _ := strconv.ParseInt(out.Result.Total, 10, 64)
				b, _ := strconv.ParseInt(out.Result.TotalBytes, 10, 64)
				if n > peakTxs.Load() {
					peakTxs.Store(n)
				}
				if b > peakBytes.Load() {
					peakBytes.Store(b)
				}
				if n > 0 && b+b/n > capBytes {
					full.Add(1)
				}
			}
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()

	var ok, bad atomic.Int64
	work := make(chan []byte)
	var pool sync.WaitGroup
	for w := 0; w < 64; w++ {
		pool.Add(1)
		go func() {
			defer pool.Done()
			for tx := range work {
				for _, url := range urls {
					switch _, err := send64(url, tx); {
					case err == nil:
						ok.Add(1)
					case strings.Contains(err.Error(), "already"):
					default:
						if bad.Add(1) < 4 {
							fmt.Fprintln(os.Stderr, "fill:", err)
						}
					}
				}
			}
		}()
	}
	start := time.Now()
	sent := 0
	send := func() {
		work <- buildV1(payer(sent%payers), blockhash, units, sent, maxV1)
		sent++
	}
	for sent < count && full.Load() == 0 {
		send()
	}
	for filled := sent; sent < min(count, 2*filled); {
		send()
	}
	close(work)
	pool.Wait()
	time.Sleep(time.Second)
	close(done)
	<-sampled
	fmt.Printf("%d %d %d %d %d %d %.1f\n", sent, ok.Load(), full.Load(), bad.Load(), peakTxs.Load(), peakBytes.Load(),
		time.Since(start).Seconds())
	return nil
}
