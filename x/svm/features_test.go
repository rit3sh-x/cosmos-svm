package svm

import (
	json "encoding/json/v2"
	"slices"
	"testing"

	"github.com/rit3sh-x/cosmos-svm/genesis"
)

func TestGenesisFeatureAccounts(t *testing.T) {
	var pinned struct {
		Gates []pinnedGate `json:"gates"`
	}
	if err := json.Unmarshal(genesis.Features, &pinned); err != nil {
		t.Fatal(err)
	}
	g := DefaultGenesis()
	accounts := map[string]bool{}
	for _, a := range g.Accounts {
		if a.Owner == "Feature111111111111111111111111111111111111" {
			accounts[a.Pubkey] = true
		}
	}
	// 236 active svm and cleaned gates at the pin, plus the four named genesis
	// exceptions: SIMD-0392, the two rent cuts and SIMD-0385. v4.3.0 cleaned
	// three active svm gates and four active bank gates. svmmeta holds the
	// active svm gates and the three bank-side exceptions.
	if len(accounts) != 240 {
		t.Fatalf("%d feature accounts, want 240", len(accounts))
	}
	if len(g.Gates) != 36 {
		t.Fatalf("%d gates for svmmeta, want 36", len(g.Gates))
	}
	bankActive := 0
	for _, p := range pinned.Gates {
		if p.Class == "bank" && p.ActiveAtPin {
			bankActive++
			if accounts[p.ID] {
				t.Errorf("bank-side gate %s has a feature account", p.ID)
			}
		}
	}
	if bankActive != 6 {
		t.Fatalf("%d bank-side gates active at the pin, want 6", bankActive)
	}
	const (
		raiseBlockLimitsTo100m      = "P1BCUMpAC7V2GRBRiJCNUgpMyWZhoqt3LKo712ePqsz"
		disableSbpfV0V1V2Deployment = "B8JJXCy5amZyWG9r7EnUYLwzXSXTxG7GZ1qZ1qggo83g"
		getSysvarSyscallEnabled     = "CLCoTADvV64PSrnR6QXty6Fwrt9Xc6EdxSJE4wLRePjq"
	)
	if accounts[raiseBlockLimitsTo100m] {
		t.Error("raise_block_limits_to_100m has a feature account")
	}
	if slices.Contains(g.Gates, disableSbpfV0V1V2Deployment) || accounts[disableSbpfV0V1V2Deployment] {
		t.Error("disable_sbpf_v0_v1_v2_deployment is on at genesis")
	}
	if !slices.Contains(g.Gates, getSysvarSyscallEnabled) {
		t.Error("get_sysvar_syscall_enabled is off at genesis")
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestGenesisRejectsGateWithoutAccount proves that a gate svmmeta would call
// active must have a feature account that agrees.
func TestGenesisRejectsGateWithoutAccount(t *testing.T) {
	g := DefaultGenesis()
	g.Gates = append(g.Gates, "B8JJXCy5amZyWG9r7EnUYLwzXSXTxG7GZ1qZ1qggo83g")
	if err := g.Validate(); err == nil {
		t.Fatal("a gate with no feature account validated")
	}
}

// TestTheGenesisExceptionsAreNamed proves that four gates are on at genesis
// although the pin has them off: SIMD-0392, the two rent cuts mainnet took
// after the pin, and SIMD-0385. No other gate deviates. A reviewed file names
// the exceptions, so the pinned file stays an exact read of mainnet.
func TestTheGenesisExceptionsAreNamed(t *testing.T) {
	want := map[string]string{
		"BY4JhHLahVzS9ynfDz4exzGPbVXhFmJvEyMWsXbDBqME": "svm",  // SIMD-0392
		"4a6f7o7iTcA8hRDCrPLkSatnt5Ykxiu36wo5p1Tt12wC": "bank", // SIMD-0437-1
		"61BtM7BkDEE8Yq5fskEVAQT9mYA8qCejJWoLe5apqg81": "bank", // SIMD-0437-2
		"txv1aq4pp281K9um3tnPgkfX8UqtFT6wcVW3hNezGLL":  "bank", // SIMD-0385
	}
	got := GenesisExceptions()
	if len(got) != len(want) {
		t.Fatalf("the genesis exceptions are %v, want SIMD-0392, SIMD-0437-1, SIMD-0437-2 and SIMD-0385", got)
	}
	var pinned struct {
		Gates []pinnedGate `json:"gates"`
	}
	if err := json.Unmarshal(genesis.Features, &pinned); err != nil {
		t.Fatal(err)
	}
	g := DefaultGenesis()
	for _, id := range got {
		class, ok := want[id]
		if !ok {
			t.Fatalf("%s is a genesis exception this test does not name", id)
		}
		for _, p := range pinned.Gates {
			if p.ID != id {
				continue
			}
			if p.ActiveAtPin {
				t.Fatalf("the pinned file now has %s active; the exception should be removed", id)
			}
			if p.Class != class {
				t.Fatalf("%s is class %q, want %s", id, p.Class, class)
			}
		}
		if !slices.Contains(g.Gates, id) {
			t.Fatalf("%s is not in the genesis gate set", id)
		}
		if !slices.ContainsFunc(g.Accounts, func(a GenesisAccount) bool { return a.Pubkey == id }) {
			t.Fatalf("%s has no feature account, so the gate set and the accounts disagree", id)
		}
	}
}

// TestTheFourGatesWithNoReadSiteAreRefused proves that ValidTarget refuses
// four inactive svm gates. An inactive svm gate is a valid governance target
// only if a read site for its field exists in the crates this chain compiles.
// These four have none. Three are vote-account features, which are out of
// scope for this chain. The fourth has no branch left, because
// MAX_TX_ACCOUNT_LOCKS is a plain constant at this pin.
func TestTheFourGatesWithNoReadSiteAreRefused(t *testing.T) {
	rejected := map[string]string{
		"3HcSrCTGXTUnrTueHi4DAwNuMxZSsm5xui2Ax3mgxHqf": "SIMD-0232 custom_commission_collector",
		"B1ockRevenueSharing111111111111111111111111":  "SIMD-0123 block_revenue_sharing",
		"VoteAccount1nitia1izeV211111111111111111111":  "SIMD-0464 vote_account_initialize_v2",
		"9LZdXeKGeBV6hRLdxS1rHbHoEUsKqesCC2ZAPTPKJAbK": "increase_tx_account_lock_limit",
	}
	var pinned struct {
		Gates []pinnedGate `json:"gates"`
	}
	if err := json.Unmarshal(genesis.Features, &pinned); err != nil {
		t.Fatal(err)
	}
	for id, name := range rejected {
		seen := false
		for _, p := range pinned.Gates {
			if p.ID != id {
				continue
			}
			seen = true
			if p.Class != "svm" {
				t.Errorf("%s is class %q, want svm: the read-site rule applies only to svm gates", name, p.Class)
			}
			if p.ActiveAtPin {
				t.Errorf("%s is active at the pin, so it is not a target at all and this row is stale", name)
			}
		}
		if !seen {
			t.Errorf("%s is not in the pinned set", name)
		}
		if ValidTarget(id) {
			t.Errorf("%s is a valid governance target; activating it would write an account claiming behaviour this chain does not run", name)
		}
	}
}

// A feature account's data is bincode Some(height), and None or another
// length is no activation.
func TestFeatureDataIsBincodeSome(t *testing.T) {
	if !slices.Equal(featureData(258), []byte{1, 2, 1, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("featureData(258) is %v", featureData(258))
	}
	if at, ok := activatedAt(featureData(258)); !ok || at != 258 {
		t.Fatalf("activatedAt read %d, %v", at, ok)
	}
	if _, ok := activatedAt(make([]byte, featureLen)); ok {
		t.Fatal("None reads as an activation")
	}
	if _, ok := activatedAt(featureData(258)[:featureLen-1]); ok {
		t.Fatal("short data reads as an activation")
	}
}
