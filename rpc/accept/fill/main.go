// Command fill drives a block to its byte limit with transactions, and runs the
// other -mode steps of the acceptance harnesses. It runs on machines without a
// Node.js package tree.
//
// Each filler transaction uses one writable account and declares compute and
// loaded-account limits. Without these limits, the 200,000-unit and 64 MiB
// defaults allow only about 300 transactions per block.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The two programs that a filler transaction names beside its payer. Memo v1 is
// seeded from mainnet and takes arbitrary UTF-8, so it can carry the padding.
// ComputeBudget is a builtin.
var (
	memo         = mustKey("Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo")
	computeBudge = mustKey("ComputeBudget111111111111111111111111111111")
)

// packet is Solana's PACKET_DATA_SIZE, which admission enforces.
const packet = 1232

func mustKey(s string) [32]byte {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 32 {
		panic(s)
	}
	return [32]byte(b)
}

// payer returns the i'th filler account. fill uses many payers, not one,
// because the payer is the only writable account of a transaction. A block of
// transactions that share it is one conflict group.
func payer(i int) ed25519.PrivateKey {
	s := sha256.Sum256([]byte(fmt.Sprintf("svm-fill-%d", i)))
	return ed25519.NewKeyFromSeed(s[:])
}

func main() {
	mode := flag.String("mode", "send", "keys, to print the payer addresses genesis has to fund, keypair, to write one of them in the Solana CLI's own format, send, arena-genesis, to print the arena attack's lookup table as a genesis account, arena, to run that attack, unjail, to send the validator in -home's MsgUnjail, validator, to print whether it is tombstoned and jailed, its bond and the bond denom's supply, evidence, to broadcast that validator's equivocation at -height, propose-gate, to submit the 2,575 rent cut at -height through x/gov, vote, to vote yes on -proposal, proposal, to print its status, rent, to check -phase before or after the flip, v1, to check a V1 transfer, v1fill, to send maximum-size V1 transactions until -node's pool has no room under -cap for another and as many again, at most -count, or quic, to land a transfer through -url's tpuQuic")
	phase := flag.String("phase", "", "before or after, for mode=rent")
	count := flag.Int("count", 20000, "how many V1 transactions mode=v1fill sends at most")
	capBytes := flag.Int64("cap", 0, "the pool's byte cap, max_txs_bytes, which mode=v1fill fills")
	home := flag.String("home", "", "the validator home mode=unjail and mode=validator read the consensus key from")
	node := flag.String("node", "http://127.0.0.1:26657", "the CometBFT RPC mode=unjail and mode=validator use")
	chain := flag.String("chain", "", "the chain id mode=unjail and mode=evidence sign for")
	height := flag.Int64("height", 0, "the height mode=evidence equivocates at and mode=propose-gate schedules")
	proposal := flag.Uint64("proposal", 0, "the proposal mode=vote votes on")
	out := flag.String("out", "", "where mode=keypair writes the keypair")
	urls := flag.String("url", "http://127.0.0.1:8899", "comma separated Solana JSON-RPC endpoints, all of which receive every transaction")
	payers := flag.Int("payers", 256, "distinct fee payers, which is the block's conflict group count")
	target := flag.Int("bytes", 8_388_608, "block bytes to aim at, which is block.max_bytes")
	over := flag.Float64("over", 2.5, "multiple of the target to send, for a run where an earlier block drains part of the supply")
	units := flag.Uint("units", 0, "compute unit limit to declare; 0 asks the chain by simulating one transaction")
	client := flag.String("client", "", "tools/quic-client, for mode=quic")
	nonce := flag.Int("nonce", 0, "which payer and recipient mode=quic uses")
	after := flag.Int("after", 0, "for mode=quic, blocks to let pass, and a leader change, between reading tpuQuic and sending")
	flag.Parse()

	var err error
	switch *mode {
	case "quic":
		err = quic(strings.Split(*urls, ",")[0], *client, *nonce, *after)
	case "keypair":
		// The Solana CLI's keypair file is the 64 byte expanded secret as a
		// JSON array, which is what `solana -k` and `spl-token` read. It is
		// payer 0, so one genesis entry funds both the byte-limit fill and the
		// traffic that the harness drives through the CLI.
		key := payer(0)
		bytes := make([]int, len(key))
		for i, b := range key {
			bytes[i] = int(b)
		}
		var body []byte
		if body, err = json.Marshal(bytes, json.Deterministic(true)); err == nil {
			err = os.WriteFile(*out, body, 0o600)
		}
	case "keys":
		for i := 0; i < *payers; i++ {
			k := tc.Pub(payer(i))
			fmt.Println(base58.Encode(k[:]))
		}
	case "arena-genesis":
		err = json.MarshalWrite(os.Stdout, arenaGenesis(), json.Deterministic(true))
	case "unjail":
		err = unjail(*home, *node, *chain)
	case "validator":
		err = printValidator(*home, *node)
	case "evidence":
		err = equivocate(*home, *node, *chain, *height)
	case "vote":
		err = vote(*home, *node, *chain, *proposal)
	case "propose-gate":
		err = proposeGate(*home, *node, *chain, *height)
	case "proposal":
		err = proposalStatus(*node, *proposal)
	case "rent":
		err = rent(strings.Split(*urls, ",")[0], *phase)
	case "v1":
		err = v1(strings.Split(*urls, ",")[0])
	case "v1fill":
		err = v1fill(strings.Split(*urls, ","), *node, *payers, *count, *capBytes)
	case "arena":
		err = arena(strings.Split(*urls, ","))
	default:
		err = send(strings.Split(*urls, ","), *payers, *target, *over, uint32(*units))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fill:", err)
		os.Exit(1)
	}
}

// The arena attack. 52 accounts of 10 MiB are more than the 512 MiB that a
// block can load. One lookup table names them with 74 accounts that do not
// exist, so a transaction names 126 of them beside its payer and
// ComputeBudget, at a 32 KiB declared limit.
const (
	arenaBig   = 52
	arenaNames = 126
	arenaSize  = 10 << 20
)

func arenaKey(i int) ed25519.PrivateKey {
	s := sha256.Sum256([]byte(fmt.Sprintf("svm-arena-%d", i)))
	return ed25519.NewKeyFromSeed(s[:])
}

var arenaTable = tc.Pub(arenaKey(-1))

// arenaGenesis returns the table as a genesis account, already extended, in
// the layout that lookupTable in rpc/address_test.go writes. Genesis is
// cheaper than ExtendLookupTable, which fits about thirty addresses in a
// packet.
func arenaGenesis() svm.GenesisAccount {
	data := binary.LittleEndian.AppendUint32(nil, 1)
	data = binary.LittleEndian.AppendUint64(data, ^uint64(0))
	data = append(data, make([]byte, 56-len(data))...)
	for i := 0; i < arenaNames; i++ {
		k := tc.Pub(arenaKey(i))
		data = append(data, k[:]...)
	}
	return svm.GenesisAccount{
		Pubkey:    base58.Encode(arenaTable[:]),
		Lamports:  svm.MinimumBalance(len(data)),
		Owner:     "AddressLookupTab1e1111111111111111111111111",
		Data:      data,
		RentEpoch: ^uint64(0),
	}
}

// arena creates the 52 accounts, then sends a block of transactions that name
// all 126 through the table, and waits for each of them. Each must land fees
// only, with MaxLoadedAccountsDataSizeExceeded. Its own 32 KiB limit or the
// block's ceiling stopped it, and either way it paid.
func arena(urls []string) error {
	blockhash, err := latestBlockhash(urls[0])
	if err != nil {
		return err
	}
	// A transaction can allocate at most two 10 MiB accounts. The proposer
	// packs four of those transactions in a block, inside the 100 MB that a
	// block can allocate.
	var creates [][]byte
	for i := 0; i < arenaBig; i += 2 {
		from := payer(i / 2)
		create := func(to ed25519.PrivateKey) tc.Instruction {
			data := binary.LittleEndian.AppendUint32(nil, 0)
			data = binary.LittleEndian.AppendUint64(data, svm.MinimumBalance(arenaSize))
			data = binary.LittleEndian.AppendUint64(data, arenaSize)
			data = append(data, svm.SystemProgram[:]...)
			return tc.Instruction{Program: svm.SystemProgram, Data: data, Accounts: []tc.Meta{
				tc.Signer(tc.Pub(from), true), tc.Signer(tc.Pub(to), true)}}
		}
		creates = append(creates, tc.Tx(blockhash, from, []ed25519.PrivateKey{arenaKey(i), arenaKey(i + 1)},
			create(arenaKey(i)), create(arenaKey(i+1))))
	}
	start := time.Now()
	if err := land(urls, creates, func(err jsontext.Value) bool { return string(err) == "null" }); err != nil {
		return fmt.Errorf("creating the 10 MiB accounts: %w", err)
	}
	fmt.Printf("arena: %d accounts of %d bytes created in %.1fs\n", arenaBig, arenaSize, time.Since(start).Seconds())

	if blockhash, err = latestBlockhash(urls[0]); err != nil {
		return err
	}
	entries := make([]tc.LookupEntry, arenaNames)
	for i := range entries {
		entries[i] = tc.LookupEntry{Index: uint8(i), Key: tc.Pub(arenaKey(i))}
	}
	// About as many transactions as 60,000,000 units hold, at about 54,000
	// each. The unit limit differs for each transaction, so no two have the
	// same bytes.
	txs := make([][]byte, 1_000)
	for i := range txs {
		txs[i] = tc.TxV0(blockhash, payer(i%256), nil, []tc.Lookup{{Table: arenaTable, Readonly: entries}},
			tc.Instruction{Program: computeBudge, Data: binary.LittleEndian.AppendUint32([]byte{2}, uint32(1_000+i))},
			tc.Instruction{Program: computeBudge, Data: binary.LittleEndian.AppendUint32([]byte{4}, 32*1024)})
	}
	start = time.Now()
	if err := land(urls, txs, func(err jsontext.Value) bool {
		return string(err) == `"MaxLoadedAccountsDataSizeExceeded"`
	}); err != nil {
		return fmt.Errorf("the attack: %w", err)
	}
	fmt.Printf("arena: %d transactions naming %d accounts each at a 32 KiB limit, all fees only with MaxLoadedAccountsDataSizeExceeded, in %.1fs\n",
		len(txs), arenaNames, time.Since(start).Seconds())
	return nil
}

// land sends txs to every machine and waits up to five minutes for each one's
// status, which must satisfy want.
func land(urls []string, txs [][]byte, want func(jsontext.Value) bool) error {
	for _, url := range urls {
		for i := 0; i < len(txs); i += 20 {
			if _, _, err := post(url, txs[i:min(i+20, len(txs))]); err != nil {
				return err
			}
		}
	}
	pending := make([]string, len(txs))
	for i, tx := range txs {
		pending[i] = base58.Encode(tx[1:65])
	}
	for deadline := time.Now().Add(5 * time.Minute); len(pending) > 0; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d transactions never landed", len(pending), len(txs))
		}
		var out struct {
			Value []*struct {
				Err jsontext.Value `json:"err"`
			} `json:"value"`
		}
		batch := pending[:min(256, len(pending))]
		if err := call(urls[0], "getSignatureStatuses", []any{batch}, &out); err != nil {
			return err
		}
		var still []string
		for i, st := range out.Value {
			switch {
			case st == nil:
				still = append(still, batch[i])
			case !want(st.Err):
				return fmt.Errorf("transaction %s landed with %s", batch[i], st.Err)
			}
		}
		pending = append(still, pending[len(batch):]...)
	}
	return nil
}

func send(urls []string, payers, target int, over float64, units uint32) error {
	blockhash, err := latestBlockhash(urls[0])
	if err != nil {
		return err
	}
	if units == 0 {
		if units, err = simulate(urls[0], blockhash); err != nil {
			return err
		}
	}
	// One transaction built at the packet size, then the count that covers the
	// target. Every transaction is the same size, so the count is exact.
	count := int(float64(target)/float64(packet)*over) + 1
	txs := make([][]byte, count)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < count; i += 16 {
				txs[i] = build(payer(i%payers), blockhash, units, i)
			}
		}(w)
	}
	wg.Wait()
	for _, tx := range txs {
		if len(tx) != packet {
			return fmt.Errorf("built a %d byte transaction, want %d", len(tx), packet)
		}
	}

	// Every endpoint receives every transaction, all at once. On loopback this
	// repeats mempool gossip. Between machines it lets the proposer hold the
	// whole block, not only what reached it first. The sends must be
	// concurrent. A send to one machine and then the next takes longer than a
	// block, so the second machine gets transactions that the first has
	// already committed.
	start := time.Now()
	var ok, dup, bad atomic.Int64
	work := make(chan job)
	var pool sync.WaitGroup
	for w := 0; w < 64; w++ {
		pool.Add(1)
		go func() {
			defer pool.Done()
			for j := range work {
				accepted, duplicate, err := post(j.url, j.batch)
				ok.Add(int64(accepted))
				dup.Add(int64(duplicate))
				bad.Add(int64(len(j.batch) - accepted - duplicate))
				if err != nil && bad.Load() < 4 {
					fmt.Fprintln(os.Stderr, "fill:", err)
				}
			}
		}()
	}
	// 20 transactions of 1,232 bytes is about 33 kB of base64, inside the
	// 50 KiB body limit.
	for _, url := range urls {
		for i := 0; i < len(txs); i += 20 {
			work <- job{url, txs[i:min(i+20, len(txs))]}
		}
	}
	close(work)
	pool.Wait()
	fmt.Printf("fill: %d transactions of %d bytes, %d units each, to %d machines: %d accepted, %d already held, %d refused, in %.1fs\n",
		count, packet, units, len(urls), ok.Load(), dup.Load(), bad.Load(), time.Since(start).Seconds())
	if ok.Load() == 0 {
		return fmt.Errorf("every transaction was refused")
	}
	return nil
}

// build returns one filler: a memo padded so that the serialized transaction
// is exactly a packet, with the two ComputeBudget declarations that keep it
// cheap.
func build(key ed25519.PrivateKey, blockhash [32]byte, units uint32, nonce int) []byte {
	// The memo body carries the index so no two transactions in a block are
	// the same bytes, and no two share a signature.
	head := []byte(fmt.Sprintf("%d:", nonce))
	pad := func(n int) []byte {
		body := make([]byte, n)
		copy(body, head)
		for i := len(head); i < n; i++ {
			body[i] = 'a'
		}
		return body
	}
	tx := func(n int) []byte {
		return tc.Tx(blockhash, key, nil,
			tc.Instruction{Program: computeBudge, Data: binary.LittleEndian.AppendUint32([]byte{2}, units)},
			tc.Instruction{Program: computeBudge, Data: binary.LittleEndian.AppendUint32([]byte{4}, 32*1024)},
			tc.Instruction{Program: memo, Data: pad(n)},
		)
	}
	// The first pass measures the overhead, and the second pads to it. The
	// memo length prefix is ShortU16, so a size that crosses 128 or 16,384
	// moves the overhead. A third pass costs nothing and settles it.
	n := packet
	for i := 0; i < 3; i++ {
		n += packet - len(tx(n))
	}
	return tx(n)
}

// simulate asks the chain what one filler costs to run, so the declared limit
// is the price of the work, not a number written here. The cost model charges
// the declared limit as is. Too high a limit shrinks the block, and too low a
// limit makes every transaction fail.
func simulate(url string, blockhash [32]byte) (uint32, error) {
	probe := build(payer(0), blockhash, 200_000, 0)
	var out struct {
		Value struct {
			Err           jsontext.Value `json:"err"`
			UnitsConsumed uint64         `json:"unitsConsumed"`
		} `json:"value"`
	}
	if err := call(url, "simulateTransaction", []any{
		base64.StdEncoding.EncodeToString(probe),
		map[string]any{"encoding": "base64", "sigVerify": false},
	}, &out); err != nil {
		return 0, err
	}
	if len(out.Value.Err) != 0 && string(out.Value.Err) != "null" {
		return 0, fmt.Errorf("a filler transaction failed to simulate: %s", out.Value.Err)
	}
	if out.Value.UnitsConsumed == 0 {
		return 0, fmt.Errorf("simulateTransaction reported no units consumed")
	}
	// A tenth over what the memo cost, because the padding is identical in
	// every transaction but the index prefix is not.
	return uint32(out.Value.UnitsConsumed + out.Value.UnitsConsumed/10 + 16), nil
}

func latestBlockhash(url string) ([32]byte, error) {
	var out struct {
		Value struct {
			Blockhash string `json:"blockhash"`
		} `json:"value"`
	}
	if err := call(url, "getLatestBlockhash", []any{}, &out); err != nil {
		return [32]byte{}, err
	}
	raw, err := base58.Decode(out.Value.Blockhash)
	if err != nil || len(raw) != 32 {
		return [32]byte{}, fmt.Errorf("getLatestBlockhash returned %q", out.Value.Blockhash)
	}
	return [32]byte(raw), nil
}

// job is one batch bound for one machine.
type job struct {
	url   string
	batch [][]byte
}

type request struct {
	Version string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

func call(url, method string, params []any, into any) error {
	body, err := json.Marshal(request{"2.0", 1, method, params}, json.Deterministic(true))
	if err != nil {
		return err
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var out struct {
		Result jsontext.Value `json:"result"`
		Error  jsontext.Value `json:"error"`
	}
	if err := json.UnmarshalRead(res.Body, &out); err != nil {
		return err
	}
	if len(out.Error) != 0 {
		return fmt.Errorf("%s: %s", method, out.Error)
	}
	return json.Unmarshal(out.Result, into)
}

// post sends one batch and returns how many transactions the chain took and
// how many it already held. A duplicate is normal, not a refusal. Every
// machine gets every transaction, and the machines gossip them to each other
// at the same time.
func post(url string, batch [][]byte) (int, int, error) {
	reqs := make([]request, len(batch))
	for i, tx := range batch {
		reqs[i] = request{"2.0", i, "sendTransaction", []any{
			base64.StdEncoding.EncodeToString(tx),
			map[string]any{"encoding": "base64", "skipPreflight": true},
		}}
	}
	body, err := json.Marshal(reqs, json.Deterministic(true))
	if err != nil {
		return 0, 0, err
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, 0, err
	}
	var out []struct {
		Result string `json:"result"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, fmt.Errorf("%s: %s", res.Status, string(raw[:min(200, len(raw))]))
	}
	var accepted, duplicate int
	var first error
	for _, r := range out {
		switch {
		case r.Result != "":
			accepted++
		case strings.Contains(r.Error.Message, "already"):
			duplicate++
		case first == nil:
			first = fmt.Errorf("sendTransaction: %s", r.Error.Message)
		}
	}
	return accepted, duplicate, first
}
