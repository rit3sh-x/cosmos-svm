package rpc

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// These tests pin `docs/compatibility.md` to the code. genesis/check-manifest.py
// and genesis/check-features.py check the rows that come from mainnet. These
// tests check the constants that this chain sets itself. Every failure names
// the page row to change.

const page = "../docs/compatibility.md"

func compatibilityPage(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("docs/compatibility.md is the page every other row points at: %v", err)
	}
	// The page is wrapped prose and a claim can cross a line break, so each
	// run of whitespace collapses to one space.
	return strings.Join(strings.Fields(string(b)), " ")
}

// mustSay fails the test if text does not contain claim. row names the page
// section that states the constant, so a failure sends the reader to one place.
func mustSay(t *testing.T, text, row, claim string) {
	t.Helper()
	if !strings.Contains(text, claim) {
		t.Errorf("docs/compatibility.md %s no longer says %q; the constant moved and the page did not", row, claim)
	}
}

// commas formats a number the way the page writes one.
func commas(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// engineConst reads one `pub const NAME: u64 = value;` from the cost model.
// The page quotes the engine, so the test checks the page against the engine.
func engineConst(t *testing.T, name string) uint64 {
	t.Helper()
	b, err := os.ReadFile("../engine/src/cost.rs")
	if err != nil {
		t.Fatalf("read the cost model: %v", err)
	}
	m := regexp.MustCompile(`(?m)^pub const ` + name + `: u64 = ([0-9_]+);`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("engine/src/cost.rs no longer declares %s; docs/compatibility.md section 1 is written around it", name)
	}
	v, err := strconv.ParseUint(strings.ReplaceAll(m[1], "_", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheCompatibilityPageQuotesTheBlockLimits(t *testing.T) {
	text := compatibilityPage(t)
	write := engineConst(t, "WRITE_LOCK_UNITS")
	name := engineConst(t, "NAME_UNITS")
	block := engineConst(t, "MAX_BLOCK_UNITS")
	bytes := engineConst(t, "MAX_BLOCK_ACCOUNTS_DATA_SIZE_DELTA")

	mustSay(t, text, "section 1's write lock row", "| A write lock | "+commas(write)+" units |")
	mustSay(t, text, "section 1's name row", "| Naming an account | "+commas(name)+" units |")
	mustSay(t, text, "section 1's block row", "| Units per block | "+commas(block)+" |")
	mustSay(t, text, "section 1's per-account row",
		"| Units per writable account | "+commas(engineConst(t, "MAX_WRITABLE_ACCOUNT_UNITS"))+" |")
	mustSay(t, text, "section 1's allocation row", "| Allocated bytes per block | "+commas(bytes)+" |")
	mustSay(t, text, "section 1's allocation row",
		fmt.Sprintf("`bytes / %d` units", engineConst(t, "ALLOCATED_BYTES_PER_UNIT")))
	mustSay(t, text, "section 1's deploy row",
		fmt.Sprintf("`max(declared, %s) / %d` units",
			commas(engineConst(t, "DEPLOY_BYTES_FLOOR")), engineConst(t, "DEPLOY_BYTES_PER_UNIT")))

	// These two derived numbers go stale without notice. A written account is
	// also a named one, so each writable account costs both terms.
	mustSay(t, text, "section 1's write lock row",
		"A block holds "+commas(block/(write+name))+" writable accounts")
	mustSay(t, text, "section 1's names paragraph",
		"names 128 accounts pays "+commas(128*name)+" units")
}

func TestTheCompatibilityPageQuotesTheRentThisChainCharges(t *testing.T) {
	text := compatibilityPage(t)
	perByte := svm.MinimumBalance(1) - svm.MinimumBalance(0)
	mustSay(t, text, "section 2's opening",
		fmt.Sprintf("`Rent.lamports_per_byte` is %s here", commas(perByte)))
	mustSay(t, text, "section 7's rent row",
		fmt.Sprintf("`Rent.lamports_per_byte` is %s.", commas(perByte)))
	mustSay(t, text, "section 2's floor paragraph",
		"is "+commas(svm.MinimumBalance(0))+" lamports here")
}

// TestTheCompatibilityPageClassifiesEveryAgaveMethod checks that each method
// and subscription in Agave's inventory has a row. A row says "served" exactly
// when rpc/server.go serves it. The page contains the phrase that each
// transcript ledger divergence names.
func TestTheCompatibilityPageClassifiesEveryAgaveMethod(t *testing.T) {
	text := compatibilityPage(t)
	rows := map[string][2]string{}
	row := regexp.MustCompile("\\| `(\\w+)` \\| (?:(minimal|bank|accounts|accounts-scan|full) \\| )?([^|]+) \\|")
	for _, m := range row.FindAllStringSubmatch(text, -1) {
		rows[m[1]] = [2]string{m[2], strings.TrimSpace(m[3])}
	}

	inv := loadInventory(t)
	for _, m := range inv.Methods {
		r, ok := rows[m.Name]
		if !ok {
			t.Errorf("docs/compatibility.md section 5 has no row for %s, which Agave %s serves", m.Name, inv.Agave)
			continue
		}
		if r[0] != m.Set {
			t.Errorf("section 5 puts %s in the %q set; Agave registers it in %q", m.Name, r[0], m.Set)
		}
		_, served := methods[m.Name]
		if strings.HasPrefix(r[1], "served") != served {
			t.Errorf("section 5 says %s is %q, and rpc/server.go serving it is %v", m.Name, r[1], served)
		}
	}
	for _, s := range inv.Subscriptions {
		r, ok := rows[s]
		if !ok {
			t.Errorf("docs/compatibility.md section 5 has no row for %s, which Agave %s serves", s, inv.Agave)
			continue
		}
		_, served := subscriptions[s]
		if strings.HasPrefix(r[1], "served") != served {
			t.Errorf("section 5 says %s is %q, and rpc/server.go serving it is %v", s, r[1], served)
		}
	}

	var ledger []divergence
	readJSON(t, transcriptDir+"/divergences.json", &ledger)
	for _, e := range ledger {
		phrase := strings.Join(strings.Fields(e.Page), " ")
		if phrase == "" || !strings.Contains(text, phrase) {
			t.Errorf("the ledger keeps %s at %s, and docs/compatibility.md does not say %q", e.Case, e.Path, e.Page)
		}
	}
}
