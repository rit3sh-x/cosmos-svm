package app_test

import (
	"crypto/ed25519"
	"encoding/binary"
	"slices"
	"testing"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// loaderIx returns a loader v3 instruction. data is a bincode
// UpgradeableLoaderInstruction.
func loaderIx(accounts []tc.Meta, data []byte) tc.Instruction {
	return tc.Instruction{Program: svm.LoaderV3, Accounts: accounts, Data: data}
}

func createAccount(payer, account ed25519.PrivateKey, space uint64, owner [32]byte) tc.Instruction {
	data := binary.LittleEndian.AppendUint32(nil, 0)
	data = binary.LittleEndian.AppendUint64(data, svm.MinimumBalance(int(space)))
	data = binary.LittleEndian.AppendUint64(data, space)
	data = append(data, owner[:]...)
	return tc.Instruction{Program: svm.SystemProgram, Accounts: []tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Signer(tc.Pub(account), true)}, Data: data}
}

// newBuffer creates a buffer for size bytes of ELF and initializes it.
func newBuffer(c *tc.Chain, payer, buffer ed25519.PrivateKey, size int) []byte {
	return tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{buffer},
		createAccount(payer, buffer, uint64(37+size), svm.LoaderV3), // Buffer metadata is 37 bytes
		loaderIx([]tc.Meta{tc.Writable(tc.Pub(buffer)), tc.Readonly(tc.Pub(payer))}, binary.LittleEndian.AppendUint32(nil, 0)),
	)
}

// writes fills a buffer with elf, 900 bytes per transaction.
func writes(c *tc.Chain, payer, buffer ed25519.PrivateKey, elf []byte) [][]byte {
	var txs [][]byte
	for off := 0; off < len(elf); off += 900 {
		chunk := elf[off:min(off+900, len(elf))]
		data := binary.LittleEndian.AppendUint32(nil, 1)
		data = binary.LittleEndian.AppendUint32(data, uint32(off))
		data = binary.LittleEndian.AppendUint64(data, uint64(len(chunk)))
		data = append(data, chunk...)
		txs = append(txs, tc.Tx(c.Latest(), payer, nil,
			loaderIx([]tc.Meta{tc.Writable(tc.Pub(buffer)), tc.Signer(tc.Pub(payer), false)}, data)))
	}
	return txs
}

// deployProgram returns a DeployWithMaxDataLen transaction. It creates the
// program account and gives the loader the filled buffer.
func deployProgram(c *tc.Chain, payer, buffer, program ed25519.PrivateKey, size int) []byte {
	programData := svm.ProgramDataAddress(tc.Pub(program))
	return tc.Tx(c.Latest(), payer, []ed25519.PrivateKey{program},
		createAccount(payer, program, 36, svm.LoaderV3),
		loaderIx([]tc.Meta{tc.Signer(tc.Pub(payer), true), tc.Writable(programData),
			tc.Writable(tc.Pub(program)), tc.Writable(tc.Pub(buffer)),
			tc.Readonly(svm.RentID), tc.Readonly(svm.ClockID),
			tc.Readonly(svm.SystemProgram), tc.Signer(tc.Pub(payer), false)},
			binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), uint64(size))),
	)
}

// TestDeployInvokeAndUpgrade proves that a loader v3 program deployed through
// buffer writes runs from the next block. An upgrade in its deploy block is
// refused, and an upgrade in a later block succeeds.
func TestDeployInvokeAndUpgrade(t *testing.T) {
	var elf []byte
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == memoV4 {
			elf = p.ELF
		}
	}
	payer := tc.Key(1)
	first, second := tc.Key(30), tc.Key(31)
	program := tc.Key(32)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()

	res, _ := c.Block(newBuffer(c, payer, first, len(elf)), newBuffer(c, payer, second, len(elf)))
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("buffer %d: code %d", i, r.Code)
		}
	}
	res, _ = c.Block(append(writes(c, payer, first, elf), writes(c, payer, second, elf)...)...)
	for i, r := range res {
		if r.Code != 0 {
			t.Fatalf("buffer write %d: code %d", i, r.Code)
		}
	}

	programData := svm.ProgramDataAddress(tc.Pub(program))
	sysvars := []tc.Meta{tc.Readonly(svm.RentID), tc.Readonly(svm.ClockID)}
	deploy := deployProgram(c, payer, first, program, len(elf))
	upgrade := func(note byte) []byte {
		return tc.Tx(c.Latest(), payer, nil,
			loaderIx(append(append([]tc.Meta{tc.Writable(programData), tc.Writable(tc.Pub(program)), tc.Writable(tc.Pub(second)), tc.Writable(tc.Pub(payer))},
				sysvars...), tc.Signer(tc.Pub(payer), false)), binary.LittleEndian.AppendUint32(nil, 3)),
			// A compute unit limit keeps the two upgrade messages distinct.
			tc.Instruction{Program: key(t, "ComputeBudget111111111111111111111111111111"),
				Data: binary.LittleEndian.AppendUint32([]byte{2}, 200_000+uint32(note))},
		)
	}
	res, _ = c.Block(deploy, upgrade(1))
	if res[0].Code != 0 {
		t.Fatalf("DeployWithMaxDataLen: code %d", res[0].Code)
	}
	if res[1].Code != 1 {
		t.Fatalf("upgrade in the deploy block: code %d, want 1 (rejected by the loader)", res[1].Code)
	}

	res, _ = c.Block(
		tc.Tx(c.Latest(), payer, nil, memo(tc.Pub(program), "hello from a deployed program")),
		tc.Tx(c.Latest(), payer, nil, memo(tc.Pub(program), string([]byte{0xff, 0xfe}))),
	)
	if res[0].Code != 0 {
		t.Fatalf("invoking the deployed memo program: code %d", res[0].Code)
	}
	if res[1].Code != 1 {
		t.Fatalf("the deployed memo program accepted invalid UTF-8: code %d", res[1].Code)
	}
	if res, _ = c.Block(upgrade(2)); res[0].Code != 0 {
		t.Fatalf("upgrade in a later block: code %d", res[0].Code)
	}
}

// TestAProgramChangedInABlockDoesNotRunLaterInIt proves that a program upgraded
// or closed in a block does not run later in that block. Agave merges a
// batch's program deployments into the global cache when the batch commits, so
// a later batch in the same slot finds the program changed.
func TestAProgramChangedInABlockDoesNotRunLaterInIt(t *testing.T) {
	var elf []byte
	for _, p := range tc.ManifestPrograms(t) {
		if p.Address == memoV4 {
			elf = p.ELF
		}
	}
	payer := tc.Key(1)
	first, second := tc.Key(30), tc.Key(31)
	program := tc.Key(32)
	c := tc.Start(t, tc.Pub(payer), tc.Options{})
	c.Block()
	for _, block := range [][][]byte{
		{newBuffer(c, payer, first, len(elf)), newBuffer(c, payer, second, len(elf))},
		append(writes(c, payer, first, elf), writes(c, payer, second, elf)...),
		{deployProgram(c, payer, first, program, len(elf))},
	} {
		res, _ := c.Block(block...)
		for i, r := range res {
			if r.Code != 0 {
				t.Fatalf("height %d, transaction %d: code %d", c.Height, i, r.Code)
			}
		}
	}
	invoke := func(text string) []byte {
		return tc.Tx(c.Latest(), payer, nil, memo(tc.Pub(program), text))
	}
	// failedWith reports whether the transaction at index in the latest
	// block failed with the loader's log line.
	failedWith := func(index uint32, line string) bool {
		records, err := c.App.History.AtHeight(uint64(c.Height))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.Index == index && r.Status == 1 {
				return slices.Contains(r.Meta.Logs, line)
			}
		}
		return false
	}
	if res, _ := c.Block(invoke("deployed")); res[0].Code != 0 {
		t.Fatalf("invoking the deployed program: code %d", res[0].Code)
	}

	programData := svm.ProgramDataAddress(tc.Pub(program))
	upgrade := tc.Tx(c.Latest(), payer, nil,
		loaderIx([]tc.Meta{tc.Writable(programData), tc.Writable(tc.Pub(program)), tc.Writable(tc.Pub(second)),
			tc.Writable(tc.Pub(payer)), tc.Readonly(svm.RentID), tc.Readonly(svm.ClockID), tc.Signer(tc.Pub(payer), false)},
			binary.LittleEndian.AppendUint32(nil, 3)))
	res, _ := c.Block(upgrade, invoke("after the upgrade, same block"))
	if res[0].Code != 0 {
		t.Fatalf("upgrade: code %d", res[0].Code)
	}
	if res[1].Code != 1 || !failedWith(1, "Program is not deployed") {
		t.Fatalf("invoking the program later in its upgrade block: code %d, want 1 and \"Program is not deployed\"", res[1].Code)
	}
	if res, _ := c.Block(invoke("the block after the upgrade")); res[0].Code != 0 {
		t.Fatalf("invoking the upgraded program in the next block: code %d", res[0].Code)
	}

	closeProgram := tc.Tx(c.Latest(), payer, nil,
		loaderIx([]tc.Meta{tc.Writable(programData), tc.Writable(tc.Pub(payer)), tc.Signer(tc.Pub(payer), false), tc.Writable(tc.Pub(program))},
			binary.LittleEndian.AppendUint32(nil, 5)))
	res, _ = c.Block(closeProgram, invoke("after the close, same block"))
	if res[0].Code != 0 {
		t.Fatalf("close: code %d", res[0].Code)
	}
	// A closed program has no deployment slot, so the Agave v4.3.0 loader
	// leaves it out of the batch cache.
	if res[1].Code != 1 || !failedWith(1, "Program is not cached") {
		t.Fatalf("invoking the program later in its close block: code %d, want 1 and \"Program is not cached\"", res[1].Code)
	}
}
