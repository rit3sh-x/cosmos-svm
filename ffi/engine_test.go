package ffi

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestNoop(t *testing.T) {
	if got := Noop(); got != 0 {
		t.Fatalf("Noop() = %d, want 0", got)
	}
}

func TestVersionProvesBothCratesLinked(t *testing.T) {
	got, err := Version()
	if err != nil {
		t.Fatalf("Version() error: %v", err)
	}
	for _, want := range []string{"svm-engine ok", "sbpf max_call_depth=", "svm limit_to_load_programs="} {
		if !strings.Contains(got, want) {
			t.Errorf("Version() = %q, missing %q", got, want)
		}
	}
	t.Logf("engine reports: %s", got)
}

// TestPanicIsContainedNotFatal proves that the boundary contains an engine
// panic. If the test binary crashes instead of reporting, a panic reaches an
// extern "C" frame, which is undefined behavior. Containment gives memory
// safety only. svmd must still halt after a panic.
func TestPanicIsContainedNotFatal(t *testing.T) {
	if err := PanicTest(); err != ErrEnginePanic {
		t.Fatalf("PanicTest() = %v, want ErrEnginePanic", err)
	}
	if got := Noop(); got != 0 {
		t.Fatalf("engine unusable after contained panic: Noop() = %d", got)
	}
}

func BenchmarkCrossing(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Noop()
	}
}

// TestEngineStatsComeThroughTheirOwnEntryPoint proves that the counters have
// their own arena kind and their own response. A wall-clock or cache number
// cannot ride on the path that makes a block result.
func TestEngineStatsComeThroughTheirOwnEntryPoint(t *testing.T) {
	stats, err := EngineStats()
	if err != nil {
		t.Fatal(err)
	}
	// Before the engine is configured, the cache numbers are zero and the call
	// still answers. An operator who scrapes a new process sees this.
	if stats.ProgramCacheBytes != ProgramCacheBytes() {
		t.Fatalf("stats report %d cache bytes, the direct call %d", stats.ProgramCacheBytes, ProgramCacheBytes())
	}
}

// TestGoReadsTheStatsRecordWhereTheEnginePacksIt reads engine/src/stats.rs.
// EngineStats reads the record by position. So the signature results must
// start where the engine writes them, and the record must be as long as the
// engine packs it.
func TestGoReadsTheStatsRecordWhereTheEnginePacksIt(t *testing.T) {
	src, err := os.ReadFile("../engine/src/stats.rs")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{
		"SIGNATURE_RESULTS_AT": statsFixedFields,
		"FIELDS":               statsFixedFields + len(SignatureEvents)*len(SignatureHandlers) + len(SignatureTiers),
	} {
		m := regexp.MustCompile(`(?m)^(?:pub )?const ` + name + `: usize = (\d+);`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("engine/src/stats.rs no longer declares %s", name)
		}
		if got, _ := strconv.Atoi(string(m[1])); got != want {
			t.Errorf("engine/src/stats.rs has %s = %d, Go reads %d", name, got, want)
		}
	}
}
