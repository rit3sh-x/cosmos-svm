package svm

import (
	"slices"
	"testing"

	"cosmossdk.io/log/v2"
	dbm "github.com/cosmos/cosmos-db"
)

func openIndex(t *testing.T) *TokenIndex {
	t.Helper()
	ti, err := OpenTokenIndex(dbm.NewMemDB(), log.NewNopLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := ti.Write(1, nil); err != nil {
		t.Fatal(err)
	}
	return ti
}

func entry(owner, mint [32]byte) TokenEntry {
	return TokenEntry{Owner: owner, Mint: mint, Token: true}
}

func k(b ...byte) [32]byte {
	var out [32]byte
	copy(out[32-len(b):], b)
	return out
}

// TestAnAccountReopenedWithAnotherMintMoves proves that an account closed and
// opened again with another mint leaves the first mint's keyspace, as an owner
// change leaves the owner's.
func TestAnAccountReopenedWithAnotherMintMoves(t *testing.T) {
	ti := openIndex(t)
	acc, owner, first, second := k(1), k(2), k(3), k(4)
	if err := ti.Write(2, []AccountWrite{{Pubkey: acc, TokenEntry: entry(owner, first)}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ti.MintAccounts(first); !slices.Equal(got, [][32]byte{acc}) {
		t.Fatalf("the first mint holds %x", got)
	}
	// Closed: not a token account any more.
	if err := ti.Write(3, []AccountWrite{{Pubkey: acc, Before: entry(owner, first)}}); err != nil {
		t.Fatal(err)
	}
	// Opened again with the second mint.
	if err := ti.Write(4, []AccountWrite{{Pubkey: acc, TokenEntry: entry(owner, second)}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ti.MintAccounts(first); len(got) != 0 {
		t.Fatalf("the first mint still holds %x", got)
	}
	if got, _ := ti.MintAccounts(second); !slices.Equal(got, [][32]byte{acc}) {
		t.Fatalf("the second mint holds %x", got)
	}
	// And in one write, without a close between.
	if err := ti.Write(5, []AccountWrite{{Pubkey: acc, TokenEntry: entry(owner, first), Before: entry(owner, second)}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ti.MintAccounts(second); len(got) != 0 {
		t.Fatalf("a mint change in place left the second mint holding %x", got)
	}
}

// TestAPrefixEndingInFFIsListed proves that an owner or a mint whose last byte
// is 0xff lists its accounts. The end key of such a prefix must not wrap to
// below the start.
func TestAPrefixEndingInFFIsListed(t *testing.T) {
	ti := openIndex(t)
	owner, mint := k(9, 0xff), k(0xff, 0xff)
	acc := k(5)
	if err := ti.Write(2, []AccountWrite{{Pubkey: acc, TokenEntry: entry(owner, mint)}}); err != nil {
		t.Fatal(err)
	}
	if got, err := ti.Accounts(owner); err != nil || !slices.Equal(got, [][32]byte{acc}) {
		t.Fatalf("an owner ending in 0xff holds %x, %v", got, err)
	}
	if got, err := ti.MintAccounts(mint); err != nil || !slices.Equal(got, [][32]byte{acc}) {
		t.Fatalf("a mint ending in 0xff holds %x, %v", got, err)
	}
	// The neighbour after it is not swept in.
	next := k(10, 0)
	if err := ti.Write(3, []AccountWrite{{Pubkey: k(6), TokenEntry: entry(next, k(1))}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ti.Accounts(owner); len(got) != 1 {
		t.Fatalf("the next owner's account was listed under 0xff: %x", got)
	}
}

// TestTheTotalMovesByEachBlock proves that each block adds to the running total
// what its accounts gained and takes what they lost. The height before stays
// readable while committed state is a block behind.
func TestTheTotalMovesByEachBlock(t *testing.T) {
	ti := openIndex(t)
	ti.mu.Lock()
	ti.total = 1_000
	ti.mu.Unlock()
	var d Lamports
	d.Move(100, 40) // a payer that paid
	d.Move(0, 50)   // an account created
	d.Move(20, 0)   // an account closed
	if err := ti.follow(2, d); err != nil {
		t.Fatal(err)
	}
	if got, _ := ti.Total(); got != 1_000+90-120 {
		t.Fatalf("the total is %d, want %d", got, 1_000+90-120)
	}
	if got, err := ti.TotalAt(1); err != nil || got != 1_000 {
		t.Fatalf("the total at the height before is %d, %v", got, err)
	}
	if _, err := ti.TotalAt(0); err == nil {
		t.Fatal("a total two heights back was answered")
	}
}

// follow is Write with a lamport change.
func (t *TokenIndex) follow(height uint64, d Lamports) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.write(height, nil, d)
}

// The token programs are SPL Token and Token-2022, and no other.
func TestTheTokenProgramsAreTheTwo(t *testing.T) {
	if !IsTokenProgram(SplToken) || !IsTokenProgram(SplToken2022) || IsTokenProgram(SystemProgram) {
		t.Fatal("IsTokenProgram is not exactly SPL Token and Token-2022")
	}
}

// Token-2022's valid_account_data: an initialized account at the base length,
// or an extended one whose type byte says account, at any length but a
// multisig's. The same type byte makes an extended mint, under Token-2022 only.
func TestTheTokenAccountRuleIsToken2022s(t *testing.T) {
	data := func(n int, typ byte) []byte {
		d := make([]byte, n)
		d[45], d[108] = 1, 1 // a mint's is_initialized, an account's state
		if n > 165 {
			d[165] = typ
		}
		return d
	}
	for _, c := range []struct {
		n    int
		typ  byte
		want bool
	}{{165, 0, true}, {166, 2, true}, {166, 1, false}, {355, 2, false}, {164, 0, false}} {
		if got := ValidTokenAccountData(data(c.n, c.typ)); got != c.want {
			t.Errorf("%d bytes of type %d: valid %v, want %v", c.n, c.typ, got, c.want)
		}
	}
	if ValidTokenAccountData(make([]byte, 165)) {
		t.Error("an uninitialized account is valid")
	}
	if _, ok := ParseTokenMint(SplToken2022, data(166, 1)); !ok {
		t.Error("an extended Token-2022 mint is not a mint")
	}
	if _, ok := ParseTokenMint(SplToken, data(166, 1)); ok {
		t.Error("an extended mint is a mint under SPL Token")
	}
}
