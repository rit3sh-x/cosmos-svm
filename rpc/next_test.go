package rpc

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"slices"
	"testing"

	"cosmossdk.io/log/v2"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// These tests check that a simulation runs as the next block runs. Each one
// simulates a transaction at the committed height N through the RPC. It then
// runs the same transaction in N+1 at N's header time and compares the two
// byte for byte.

const altProgram = "AddressLookupTab1e1111111111111111111111111"

// seen is what a client reads of one transaction: its error, logs, units,
// return data and fee, and the accounts it names after it ran.
type seen struct {
	err, logs, units, returnData, fee string
	accounts                          []string
}

// orNull returns a field that a response may leave out, or "null" when it is
// absent.
func orNull(m jsontext.Value) string {
	if len(m) == 0 {
		return "null"
	}
	return string(m)
}

// simulated simulates tx now through the RPC. It returns what a client reads,
// with the named accounts as they are after the run, and the slot of the
// answer's context.
func simulated(t *testing.T, s *Server, tx []byte, names ...[32]byte) (seen, uint64) {
	t.Helper()
	out, rerr := invoke(t, s, simulateTransaction, base64.StdEncoding.EncodeToString(tx), map[string]any{
		"encoding": "base64",
		"accounts": map[string]any{"addresses": base58Keys(names), "encoding": "base64"},
	})
	if rerr != nil {
		t.Fatalf("simulateTransaction: %v", rerr)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var got struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Err        jsontext.Value   `json:"err"`
			Logs       jsontext.Value   `json:"logs"`
			Units      jsontext.Value   `json:"unitsConsumed"`
			ReturnData jsontext.Value   `json:"returnData"`
			Fee        jsontext.Value   `json:"fee"`
			Accounts   []jsontext.Value `json:"accounts"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	v := got.Value
	r := seen{err: orNull(v.Err), logs: orNull(v.Logs), units: orNull(v.Units), returnData: orNull(v.ReturnData), fee: orNull(v.Fee)}
	for _, a := range v.Accounts {
		r.accounts = append(r.accounts, orNull(a))
	}
	return r, got.Context.Slot
}

// executed runs tx in the next block at the committed block's header time. It
// reads the result back as getTransaction and getMultipleAccounts serve it.
func executed(t *testing.T, c *tc.Chain, s *Server, tx []byte, names ...[32]byte) seen {
	t.Helper()
	c.HoldTime()
	c.Block(tx)
	// History serves only what the server was notified of.
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	sig := signatureOf(tx)
	out, rerr := invoke(t, s, getTransaction, base58.Encode(sig[:]), map[string]any{"encoding": "base64", "maxSupportedTransactionVersion": 0})
	if rerr != nil || out == nil {
		t.Fatalf("getTransaction: %v, %v", out, rerr)
	}
	raw, _ := json.Marshal(out, json.Deterministic(true))
	var got struct {
		Meta struct {
			Err        jsontext.Value `json:"err"`
			Logs       jsontext.Value `json:"logMessages"`
			Units      jsontext.Value `json:"computeUnitsConsumed"`
			ReturnData jsontext.Value `json:"returnData"`
			Fee        jsontext.Value `json:"fee"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	m := got.Meta
	r := seen{err: orNull(m.Err), logs: orNull(m.Logs), units: orNull(m.Units), returnData: orNull(m.ReturnData), fee: orNull(m.Fee)}
	out, rerr = invoke(t, s, getMultipleAccounts, base58Keys(names), map[string]any{"encoding": "base64"})
	if rerr != nil {
		t.Fatalf("getMultipleAccounts: %v", rerr)
	}
	raw, _ = json.Marshal(out, json.Deterministic(true))
	var accounts struct {
		Value []jsontext.Value `json:"value"`
	}
	if err := json.Unmarshal(raw, &accounts); err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts.Value {
		r.accounts = append(r.accounts, orNull(a))
	}
	return r
}

// agree fails unless the simulation and the execution read the same. It
// compares the accounts only when the simulation succeeds, because only then
// does it serve them. It skips the accounts at excused, which the test
// compares itself.
func agree(t *testing.T, sim, exec seen, excused ...int) {
	t.Helper()
	for _, f := range []struct{ name, sim, exec string }{
		{"err", sim.err, exec.err}, {"logs", sim.logs, exec.logs}, {"units", sim.units, exec.units},
		{"return data", sim.returnData, exec.returnData}, {"fee", sim.fee, exec.fee},
	} {
		if f.sim != f.exec {
			t.Fatalf("the simulation's %s is %s, the next block's %s", f.name, f.sim, f.exec)
		}
	}
	if sim.err != "null" {
		return
	}
	for i := range sim.accounts {
		if slices.Contains(excused, i) || sim.accounts[i] == exec.accounts[i] {
			continue
		}
		// The store deletes an account left with no lamports. A simulation
		// serves it as the transaction left it, as Agave's does.
		var left struct {
			Lamports uint64 `json:"lamports"`
		}
		if exec.accounts[i] == "null" && json.Unmarshal([]byte(sim.accounts[i]), &left) == nil && left.Lamports == 0 {
			continue
		}
		t.Fatalf("account %d simulated as %s, the next block left %s", i, sim.accounts[i], exec.accounts[i])
	}
}

// runNext simulates tx, runs it in the next block, and fails unless the two
// agree. It returns the simulation.
func runNext(t *testing.T, c *tc.Chain, s *Server, tx []byte, names ...[32]byte) seen {
	t.Helper()
	sim, _ := simulated(t, s, tx, names...)
	agree(t, sim, executed(t, c, s, tx, names...))
	return sim
}

// accountData is the data of an account as the RPC serves it in base64.
func accountData(t *testing.T, account string) []byte {
	t.Helper()
	var a struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(account), &a); err != nil || len(a.Data) != 2 {
		t.Fatalf("account %s: %v", account, err)
	}
	b, err := base64.StdEncoding.DecodeString(a.Data[0])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withoutData is an account as the RPC serves it, less its data.
func withoutData(t *testing.T, account string) string {
	t.Helper()
	var a map[string]jsontext.Value
	if err := json.Unmarshal([]byte(account), &a); err != nil {
		t.Fatal(err)
	}
	delete(a, "data")
	b, _ := json.Marshal(a, json.Deterministic(true))
	return string(b)
}

func programKey(t *testing.T, s string) [32]byte {
	t.Helper()
	k, err := decodeKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// tableIx builds an address lookup table instruction over the table, its
// authority, then extra. data is a u32 tag and its arguments.
func tableIx(t *testing.T, table [32]byte, authority ed25519.PrivateKey, data []byte, extra ...tc.Meta) tc.Instruction {
	return tc.Instruction{Program: programKey(t, altProgram),
		Accounts: append([]tc.Meta{tc.Writable(table), tc.Signer(tc.Pub(authority), false)}, extra...), Data: data}
}

// createTable builds CreateLookupTable at slot, paid by the authority. It
// returns the transaction and the table that the authority's key and slot
// derive.
func createTable(t *testing.T, blockhash [32]byte, authority ed25519.PrivateKey, slot uint64) ([]byte, [32]byte) {
	who := tc.Pub(authority)
	table, bump := svm.FindProgramAddressAndBump([][]byte{who[:], binary.LittleEndian.AppendUint64(nil, slot)}, programKey(t, altProgram))
	data := append(binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 0), slot), bump)
	return tc.Tx(blockhash, authority, nil, tableIx(t, table, authority, data, tc.Signer(who, true), tc.Readonly(svm.SystemProgram))), table
}

// extendTable is ExtendLookupTable with one address.
func extendTable(t *testing.T, blockhash [32]byte, authority ed25519.PrivateKey, table, address [32]byte) []byte {
	data := binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), 1)
	return tc.Tx(blockhash, authority, nil, tableIx(t, table, authority, append(data, address[:]...),
		tc.Signer(tc.Pub(authority), true), tc.Readonly(svm.SystemProgram)))
}

// TestALookupTableCreatedAtTheCurrentSlotPassesPreflight checks that a table
// created at slot N, the newest slot a client can read, passes preflight. A
// table derives from a slot in SlotHashes, and only the next block's
// SlotHashes holds N. Slot N+1 is still refused.
func TestALookupTableCreatedAtTheCurrentSlotPassesPreflight(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t)})
	c.Block()
	c.Block()
	s := New(c.App, &mempool{}, log.NewNopLogger())
	slot := uint64(c.Height)
	if out, rerr := invoke(t, s, methods["getSlot"]); rerr != nil || out != slot {
		t.Fatalf("getSlot: %v, %v, want %d", out, rerr, slot)
	}

	future, _ := createTable(t, c.Latest(), faucet, slot+1)
	var refused *rpcError
	if _, err := submit(s, future, map[string]any{}); !errors.As(err, &refused) || refused.Code != codeSendTransactionFailure ||
		refused.Message != "Transaction simulation failed: Error processing Instruction 0: invalid instruction data" {
		t.Fatalf("a table at slot %d: %v, want refused as Agave refuses it", slot+1, err)
	}

	create, table := createTable(t, c.Latest(), faucet, slot)
	if _, err := submit(s, create, map[string]any{}); err != nil {
		t.Fatalf("a table at slot %d failed preflight: %v", slot, err)
	}
	names := [][32]byte{table, tc.Pub(faucet)}
	sim, at := simulated(t, s, create, names...)
	if sim.err != "null" || at != slot {
		t.Fatalf("the simulation answered %s at slot %d, want success at %d", sim.err, at, slot)
	}
	agree(t, sim, executed(t, c, s, create, names...))
}

// TestASimulatedExtendIsTheNextBlocksAndItsAddressIsUsableAtOnce checks the
// slot that an extend writes. An extend writes the Clock's slot into the
// table, and an address extended at slot S resolves from S+1. The simulation
// writes N+1, and an address extended in the last committed block resolves in
// the next simulation.
func TestASimulatedExtendIsTheNextBlocksAndItsAddressIsUsableAtOnce(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t)})
	c.Block()
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	create, table := createTable(t, c.Latest(), faucet, uint64(c.Height))
	if res, _ := c.Block(create); res[0].Code != 0 {
		t.Fatalf("create: code %d, %s", res[0].Code, res[0].Log)
	}

	to := tc.Pub(tc.Key(2))
	sim := runNext(t, c, s, extendTable(t, c.Latest(), faucet, table, to), table, tc.Pub(faucet))
	if sim.err != "null" {
		t.Fatalf("the extend simulated with %s", sim.err)
	}
	// The table's meta: a u32 tag, the deactivation slot, then the slot of
	// the last extend.
	if got := binary.LittleEndian.Uint64(accountData(t, sim.accounts[0])[12:20]); got != uint64(c.Height) {
		t.Fatalf("the simulated extend wrote slot %d, want the next block's, %d", got, c.Height)
	}

	use := tc.TxV0(c.Latest(), faucet, nil, []tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: 0, Key: to}}}},
		tc.Transfer(faucet, to, svm.MinimumBalance(0)))
	if sim := runNext(t, c, s, use, tc.Pub(faucet), to); sim.err != "null" {
		t.Fatalf("a transfer through the address extended in the block before simulated with %s", sim.err)
	}
}

const nonceSize = 80

// newNonce builds a transaction that creates a nonce account and initializes
// it with the payer as its authority.
func newNonce(c *tc.Chain, payer, nonce ed25519.PrivateKey) []byte {
	create := binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 0), svm.MinimumBalance(nonceSize))
	create = append(binary.LittleEndian.AppendUint64(create, nonceSize), svm.SystemProgram[:]...)
	authority := tc.Pub(payer)
	return tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{nonce},
		tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Signer(tc.Pub(nonce), true)}, Data: create},
		tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Writable(tc.Pub(nonce)), tc.Readonly(svm.RecentBlockhashesID), tc.Readonly(svm.RentID)},
			Data: append(binary.LittleEndian.AppendUint32(nil, 6), authority[:]...)})
}

func advanceNonce(nonce [32]byte, authority ed25519.PrivateKey) tc.Instruction {
	return tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Writable(nonce), tc.Readonly(svm.RecentBlockhashesID), tc.Signer(tc.Pub(authority), false)},
		Data: binary.LittleEndian.AppendUint32(nil, 4)}
}

// durableNonce is solana-nonce's DurableNonce::from_blockhash, the value an
// advance writes in a block with that hash.
func durableNonce(blockhash [32]byte) [32]byte {
	return sha256.Sum256(append([]byte("DURABLE_NONCE"), blockhash[:]...))
}

// TestANonceAdvancedInTheBlockJustCommittedIsUsableAtOnce checks that a nonce
// that block N advanced can advance again in the next block. A simulation in
// N's context derives the same value and refuses, but the next block's hash
// derives another. The comparison excuses only the nonce value itself.
func TestANonceAdvancedInTheBlockJustCommittedIsUsableAtOnce(t *testing.T) {
	faucet, nonce := tc.Key(1), tc.Key(3)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	if res, _ := c.Block(newNonce(c, faucet, nonce)); res[0].Code != 0 {
		t.Fatalf("nonce create: code %d, %s", res[0].Code, res[0].Log)
	}
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, advanceNonce(tc.Pub(nonce), faucet))); res[0].Code != 0 {
		t.Fatalf("advance: code %d, %s", res[0].Code, res[0].Log)
	}
	stored, _ := c.Account(tc.Pub(nonce))
	if got := [32]byte(stored.Data[40:72]); got != durableNonce(c.Latest()) {
		t.Fatal("block N advanced the nonce to a value its hash does not derive")
	}

	to := tc.Pub(tc.Key(2))
	durable := tc.Tx([32]byte(stored.Data[40:72]), faucet, nil, advanceNonce(tc.Pub(nonce), faucet), tc.Transfer(faucet, to, svm.MinimumBalance(0)))
	names := [][32]byte{tc.Pub(nonce), tc.Pub(faucet), to}
	sim, _ := simulated(t, s, durable, names...)
	if sim.err != "null" {
		t.Fatalf("a nonce advanced in the block just committed simulated with %s", sim.err)
	}
	exec := executed(t, c, s, durable, names...)
	agree(t, sim, exec, 0)

	simNonce, execNonce := accountData(t, sim.accounts[0]), accountData(t, exec.accounts[0])
	if got := [32]byte(simNonce[40:72]); got != durableNonce(svm.NextOwnHash) {
		t.Fatal("the simulated advance did not write the value the next block's stand-in hash derives")
	}
	if got := [32]byte(execNonce[40:72]); got != durableNonce(c.Latest()) {
		t.Fatal("the next block's advance did not write the value its hash derives")
	}
	if !bytes.Equal(simNonce[:40], execNonce[:40]) || !bytes.Equal(simNonce[72:], execNonce[72:]) ||
		withoutData(t, sim.accounts[0]) != withoutData(t, exec.accounts[0]) {
		t.Fatalf("the nonce account differs beyond its value: %s and %s", sim.accounts[0], exec.accounts[0])
	}
}

func loaderIx(accounts []tc.Meta, data []byte) tc.Instruction {
	return tc.Instruction{Program: svm.LoaderV3, Accounts: accounts, Data: data}
}

func createAccount(payer, account ed25519.PrivateKey, space uint64, owner [32]byte) tc.Instruction {
	data := binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 0), svm.MinimumBalance(int(space)))
	data = append(binary.LittleEndian.AppendUint64(data, space), owner[:]...)
	return tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Signer(tc.Pub(account), true)}, Data: data}
}

// loadBuffer creates a loader v3 buffer and fills it with elf, a block of
// writes at a time.
func loadBuffer(t *testing.T, c *tc.Chain, payer, buffer ed25519.PrivateKey, elf []byte) {
	t.Helper()
	// Buffer metadata is 37 bytes.
	res, _ := c.Block(tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{buffer},
		createAccount(payer, buffer, uint64(37+len(elf)), svm.LoaderV3),
		loaderIx([]tc.Meta{tc.Writable(tc.Pub(buffer)), tc.Readonly(tc.Pub(payer))}, binary.LittleEndian.AppendUint32(nil, 0))))
	if res[0].Code != 0 {
		t.Fatalf("buffer: code %d, %s", res[0].Code, res[0].Log)
	}
	var txs [][]byte
	for off := 0; off < len(elf); off += 900 {
		chunk := elf[off:min(off+900, len(elf))]
		data := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, 1), uint32(off))
		data = append(binary.LittleEndian.AppendUint64(data, uint64(len(chunk))), chunk...)
		txs = append(txs, tc.Tx(c.Latest(), payer, nil, loaderIx([]tc.Meta{tc.Writable(tc.Pub(buffer)), tc.Signer(tc.Pub(payer), false)}, data)))
	}
	for len(txs) > 0 {
		n := min(len(txs), 100)
		res, _ := c.Block(txs[:n]...)
		for i, r := range res {
			if r.Code != 0 {
				t.Fatalf("buffer write %d: code %d, %s", i, r.Code, r.Log)
			}
		}
		txs = txs[n:]
	}
}

func manifestELF(t *testing.T, address string) []byte {
	t.Helper()
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == address {
			return p.ELF
		}
	}
	t.Fatalf("no %s in the manifest", address)
	return nil
}

// TestASimulationRunsAProgramAsItWasLastDeployed checks that a simulation runs
// a program as the last committed block left it, after a deploy and after an
// upgrade. A simulation has its own program cache, and no block merges into
// it. The upgrade swaps memo for the lookup table program, which refuses what
// memo accepts.
func TestASimulationRunsAProgramAsItWasLastDeployed(t *testing.T) {
	payer, first, second, program := tc.Key(1), tc.Key(30), tc.Key(31), tc.Key(32)
	memo, table := manifestELF(t, "Memo4c2pN8afCj432Lb7RMVKi9PbQnnW7ewFFaV3oAH"), manifestELF(t, altProgram)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	loadBuffer(t, c, payer, first, memo)
	loadBuffer(t, c, payer, second, table)

	programData := svm.ProgramDataAddress(tc.Pub(program))
	deploy := tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{program},
		createAccount(payer, program, 36, svm.LoaderV3),
		loaderIx([]tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Writable(programData), tc.Writable(tc.Pub(program)), tc.Writable(tc.Pub(first)),
			tc.Readonly(svm.RentID), tc.Readonly(svm.ClockID), tc.Readonly(svm.SystemProgram), tc.Signer(tc.Pub(payer), false)},
			binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), uint64(max(len(memo), len(table))))))
	if res, _ := c.Block(deploy); res[0].Code != 0 {
		t.Fatalf("deploy: code %d, %s", res[0].Code, res[0].Log)
	}
	invoke := func() []byte {
		return tc.Tx(c.Latest(), payer, nil, tc.Instruction{Program: tc.Pub(program), Data: []byte("hello")})
	}
	if sim := runNext(t, c, s, invoke(), tc.Pub(payer)); sim.err != "null" {
		t.Fatalf("the program deployed in the block before simulated with %s", sim.err)
	}

	upgrade := tc.Tx(c.Latest(), payer, nil, loaderIx([]tc.Meta{tc.Writable(programData), tc.Writable(tc.Pub(program)), tc.Writable(tc.Pub(second)),
		tc.Writable(tc.Pub(payer)), tc.Readonly(svm.RentID), tc.Readonly(svm.ClockID), tc.Signer(tc.Pub(payer), false)},
		binary.LittleEndian.AppendUint32(nil, 3)))
	if res, _ := c.Block(upgrade); res[0].Code != 0 {
		t.Fatalf("upgrade: code %d, %s", res[0].Code, res[0].Log)
	}
	if sim := runNext(t, c, s, invoke(), tc.Pub(payer)); sim.err == "null" {
		t.Fatal("the upgraded program simulated as the code it replaced")
	}
}

// TestATableClosesInTheFirstBlockThatMayCloseIt checks that a deactivated
// table closes after its deactivation slot leaves SlotHashes. The next block's
// SlotHashes drops a slot one block before block N's does. So the close
// simulates from the block before the one that runs it, and not a block earlier.
func TestATableClosesInTheFirstBlockThatMayCloseIt(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t)})
	c.Block()
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	create, table := createTable(t, c.Latest(), faucet, uint64(c.Height))
	if res, _ := c.Block(create); res[0].Code != 0 {
		t.Fatalf("create: code %d, %s", res[0].Code, res[0].Log)
	}
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tableIx(t, table, faucet, binary.LittleEndian.AppendUint32(nil, 3)))); res[0].Code != 0 {
		t.Fatalf("deactivate: code %d, %s", res[0].Code, res[0].Log)
	}
	deactivated := c.Height
	closeTable := func() []byte {
		return tc.Tx(c.Latest(), faucet, nil, tableIx(t, table, faucet, binary.LittleEndian.AppendUint32(nil, 4), tc.Writable(tc.Pub(faucet))))
	}
	// SlotHashes holds 512 slots. Block D+513's begins at D+512, so D has
	// left it, and D+513 is the first block that may close the table.
	for c.Height < deactivated+511 {
		c.Block()
	}
	if sim, _ := simulated(t, s, closeTable()); sim.err == "null" {
		t.Fatalf("the close simulated at height %d, a block before it may run", c.Height)
	}
	c.Block()
	if sim := runNext(t, c, s, closeTable(), table, tc.Pub(faucet)); sim.err != "null" {
		t.Fatalf("the close simulated with %s in the block before the first that may run it", sim.err)
	}
}

// TestAGateDueAtTheNextBlockIsActiveInItsSimulation checks that the next
// block's begin-block flips the gates due at it before any transaction runs. A
// rent cut due at N+1 prices the simulation, so a transfer of the new minimum
// passes. Block N's price refuses it.
func TestAGateDueAtTheNextBlockIsActiveInItsSimulation(t *testing.T) {
	const rentCut = "rntCigrTppP5JdZz7K8TyN9sMzLdAcXp8SejYpVpX6D" // SIMD-0437-3, 2,575 lamports a byte
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{})
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	// The next block commits the schedule, and that block becomes N.
	flip := c.Height + 2
	ctx := c.App.NewUncachedContext(false, cmtproto.Header{Height: c.Height})
	if err := c.App.Stores.ScheduleGates(ctx.MultiStore(), uint64(flip-svm.MinActivationDelay), uint64(flip), []string{rentCut}); err != nil {
		t.Fatal(err)
	}
	c.Block()

	// The rent-exempt minimum of an empty account is 128 bytes' rent.
	to := tc.Pub(tc.Key(2))
	pay := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, to, 128*2_575))
	if sim := runNext(t, c, s, pay, tc.Pub(faucet), to); sim.err != "null" {
		t.Fatalf("a transfer of the next block's minimum simulated with %s", sim.err)
	}
}

// TestTheSimulationOfAnEpochsFirstBlockIsInThatEpoch checks that the next
// block's Clock opens the epoch, at the committed block's time.
func TestTheSimulationOfAnEpochsFirstBlockIsInThatEpoch(t *testing.T) {
	const epochSlots = 432_000 // mainnet's EpochSchedule, in blocks here
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{InitialHeight: epochSlots - 2})
	c.Block()
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())

	to := tc.Pub(tc.Key(2))
	transfer := tc.Transfer(faucet, to, svm.MinimumBalance(0))
	transfer.Accounts = append(transfer.Accounts, tc.Readonly(svm.ClockID))
	sim := runNext(t, c, s, tc.Tx(c.Latest(), faucet, nil, transfer), tc.Pub(faucet), to, svm.ClockID)
	if sim.err != "null" {
		t.Fatalf("the transfer simulated with %s", sim.err)
	}
	// The Clock is five u64s: slot, epoch start timestamp, epoch, leader
	// schedule epoch and unix timestamp.
	clock := accountData(t, sim.accounts[2])
	if slot, epoch := binary.LittleEndian.Uint64(clock[0:8]), binary.LittleEndian.Uint64(clock[16:24]); slot != epochSlots || epoch != 1 {
		t.Fatalf("the simulated Clock is slot %d of epoch %d, want %d of 1", slot, epoch, epochSlots)
	}
	if start, now := binary.LittleEndian.Uint64(clock[8:16]), binary.LittleEndian.Uint64(clock[32:40]); start != now {
		t.Fatalf("the epoch starts at %d, want the block's time, %d", start, now)
	}
	if got := binary.LittleEndian.Uint64(clock[24:32]); got != 2 {
		t.Fatalf("the leader schedule epoch is %d, want 2", got)
	}
}

// TestAdmissionAndSimulationResolveTablesAlike checks that CheckTx and the
// simulation read the same next-block context. A transaction through a table
// is admitted exactly when it simulates, at three points:
//   - at an address extended in the last committed block
//   - during the table's deactivation
//   - at the block where the deactivation takes effect, when both refuse it
func TestAdmissionAndSimulationResolveTablesAlike(t *testing.T) {
	faucet := tc.Key(1)
	c := tc.Start(t, tc.Pub(faucet), tc.Options{Programs: tc.ManifestPrograms(t)})
	c.Block()
	c.Block()
	s := New(c.App, nil, log.NewNopLogger())
	create, table := createTable(t, c.Latest(), faucet, uint64(c.Height))
	if res, _ := c.Block(create); res[0].Code != 0 {
		t.Fatalf("create: code %d, %s", res[0].Code, res[0].Log)
	}
	to := tc.Pub(tc.Key(2))
	if res, _ := c.Block(extendTable(t, c.Latest(), faucet, table, to)); res[0].Code != 0 {
		t.Fatalf("extend: code %d, %s", res[0].Code, res[0].Log)
	}
	alike := func(want bool) {
		t.Helper()
		tx := tc.TxV0(c.Latest(), faucet, nil, []tc.Lookup{{Table: table, Writable: []tc.LookupEntry{{Index: 0, Key: to}}}},
			tc.Transfer(faucet, to, svm.MinimumBalance(0)))
		_, rerr := invoke(t, s, simulateTransaction, base64.StdEncoding.EncodeToString(tx), map[string]any{"encoding": "base64"})
		admitted := c.Check(c.Wrap(tx), false).Code == 0
		if (rerr == nil) != want || admitted != want {
			t.Fatalf("at height %d the table resolves in the simulation %v and in admission %v, want %v", c.Height, rerr == nil, admitted, want)
		}
	}
	alike(true)

	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tableIx(t, table, faucet, binary.LittleEndian.AppendUint32(nil, 3)))); res[0].Code != 0 {
		t.Fatalf("deactivate: code %d, %s", res[0].Code, res[0].Log)
	}
	// A deactivating table resolves until its deactivation slot leaves
	// SlotHashes, which block D+513's does.
	deactivated := c.Height
	for c.Height < deactivated+511 {
		c.Block()
	}
	alike(true)
	c.Block()
	alike(false)
}
