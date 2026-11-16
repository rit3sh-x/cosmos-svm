package testchain_test

import (
	"bytes"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"testing"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// heapInUse is the live heap after two collections, which is what a finished
// chain still holds.
func heapInUse() int64 {
	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse)
}

// TestFinishedChainDoesNotLeak proves that a finished chain leaves no IAVL
// pruning goroutine and holds at most half of the heap it held alive. With
// asynchronous pruning, each store runs a goroutine that exits only when its
// tree closes. No test closes the trees, so the goroutines would keep a
// finished chain reachable.
func TestFinishedChainDoesNotLeak(t *testing.T) {
	// The first chain in a process also builds what every later one shares.
	t.Run("warm", func(t *testing.T) { tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{}).Block() })
	before := heapInUse()
	var alive int64
	t.Run("chain", func(t *testing.T) {
		c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
		for range 300 {
			c.Block()
		}
		alive = heapInUse()
		runtime.KeepAlive(c)
	})

	var profile bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&profile, 2); err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(profile.Bytes(), []byte("startPruning")); n > 0 {
		t.Fatalf("%d pruning goroutines outlived their chain", n)
	}
	held, chain := heapInUse()-before, alive-before
	if held > chain/2 {
		t.Fatalf("a finished chain still holds %d of the %d bytes it held alive", held, chain)
	}
	t.Logf("a 300-block chain: %d bytes alive, %d held after it finished", chain, held)
}
