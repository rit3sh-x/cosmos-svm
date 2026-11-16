package rpc

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cosmossdk.io/log/v2"
	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// BenchmarkAnswerHeapPeak reports heap-peak-MiB: how far the Go heap rose
// above idle while the HTTP path answered large accounts. The cases are a
// batch of ten getMultipleAccounts over four 10 MiB accounts (560 MB of
// answer), a getProgramAccounts scan that finds them, and 64 concurrent
// getAccountInfo calls over them. Each case runs in base64 and in
// base64+zstd. Run it on four CPUs of the reference machine, because the
// number of HTTP slots follows the CPU count:
//
//	taskset -c 0-3 go test -run '^$' -bench AnswerHeapPeak -benchtime 3x ./rpc
func BenchmarkAnswerHeapPeak(b *testing.B) {
	c := tc.Start(b, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	var names []string
	for _, k := range bigAccounts(b, c, 4) {
		names = append(names, base58.Encode(k[:]))
	}
	post := poster(b, s)
	cfg := map[string]any{"encoding": "base64"}
	var entries []string
	for i := range 10 {
		entries = append(entries, string(encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": "getMultipleAccounts", "params": []any{names, cfg}})))
	}
	batch := "[" + strings.Join(entries, ",") + "]"
	b.Run("batch", func(b *testing.B) { heapPeak(b, func() { post(batch) }) })
	scan := string(encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts", "params": []any{base58.Encode(svm.SystemProgram[:]), cfg}}))
	b.Run("scan", func(b *testing.B) { heapPeak(b, func() { post(scan) }) })
	for _, encoding := range []string{"base64", "base64+zstd"} {
		cfg := map[string]any{"encoding": encoding}
		b.Run("concurrent-"+encoding, func(b *testing.B) {
			heapPeak(b, func() {
				var wg sync.WaitGroup
				for i := range 64 {
					wg.Go(func() {
						post(string(encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": "getAccountInfo", "params": []any{names[i%len(names)], cfg}})))
					})
				}
				wg.Wait()
			})
		})
	}
}

// BenchmarkSimulationPeak reports the same figures for a transfer that also
// names six 10 MiB accounts as writable. The cases are simulateTransaction
// without accounts, simulateTransaction with accounts that name the six, and
// sendTransaction, whose preflight simulates the transfer. Run it as you run
// BenchmarkAnswerHeapPeak.
func BenchmarkSimulationPeak(b *testing.B) {
	faucet := tc.Key(1)
	c := tc.Start(b, tc.Pub(faucet), tc.Options{})
	s := New(c.App, &mempool{}, log.NewNopLogger())
	c.Block()
	ix := tc.Transfer(faucet, tc.Pub(tc.Key(2)), svm.MinimumBalance(0))
	var names []string
	for _, k := range bigAccounts(b, c, 6) {
		ix.Accounts = append(ix.Accounts, tc.Writable(k))
		names = append(names, base58.Encode(k[:]))
	}
	tx := base64.StdEncoding.EncodeToString(tc.Tx(c.Latest(), faucet, nil, ix))
	request := func(method string, cfg map[string]any) string {
		cfg["encoding"] = "base64"
		return string(encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": []any{tx, cfg}}))
	}
	simulate := request("simulateTransaction", map[string]any{})
	accounts := request("simulateTransaction", map[string]any{"accounts": map[string]any{"addresses": names, "encoding": "base64"}})
	send := request("sendTransaction", map[string]any{})

	// The transfer succeeds, so the answer holds the six accounts after the
	// run.
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(accounts))
	if err != nil {
		b.Fatal(err)
	}
	answer, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Contains(answer, []byte(`"err":null`)) || bytes.Count(answer, []byte(`"space":10485760`)) != 6 {
		b.Fatalf("simulated with accounts: %.200q, %v", answer, err)
	}

	post := poster(b, s)
	b.Run("simulate", func(b *testing.B) { heapPeak(b, func() { post(simulate) }) })
	b.Run("simulate-accounts", func(b *testing.B) { heapPeak(b, func() { post(accounts) }) })
	b.Run("preflight", func(b *testing.B) { heapPeak(b, func() { post(send) }) })
}

// poster serves s over HTTP and returns a client call that sends one body.
// The call reads the whole answer, and it fails b if the answer is not a
// result.
func poster(b *testing.B, s *Server) func(body string) {
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	b.Cleanup(srv.Close)
	return func(body string) {
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			b.Error(err)
			return
		}
		defer resp.Body.Close()
		head := make([]byte, 64)
		n, _ := io.ReadFull(resp.Body, head)
		if _, err := io.Copy(io.Discard, resp.Body); err != nil || !bytes.Contains(head[:n], []byte(`"result"`)) {
			b.Errorf("answered %q, %v", head[:n], err)
		}
	}
}

// heapPeak runs f once a round. Every 100 us, it samples the live and unswept
// Go heap, and the resident anonymous memory of the process, which includes
// the engine. It reports the largest rise of the heap above its level after a
// collection, and the largest rise of the resident memory above its level
// after the freed heap is returned. It also reports the mean time of f, as
// call-ms.
func heapPeak(b *testing.B, f func()) {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() int64 {
		metrics.Read(sample)
		return int64(sample[0].Value.Uint64())
	}
	var worst, worstRSS int64
	var took time.Duration
	rounds := 0
	for b.Loop() {
		debug.FreeOSMemory()
		idle, idleRSS := read(), rssAnon(b)
		var peak, peakRSS atomic.Int64
		done, sampled := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(sampled)
			tick := time.NewTicker(100 * time.Microsecond)
			defer tick.Stop()
			for {
				if v := read(); v > peak.Load() {
					peak.Store(v)
				}
				if v := rssAnon(b); v > peakRSS.Load() {
					peakRSS.Store(v)
				}
				select {
				case <-done:
					return
				case <-tick.C:
				}
			}
		}()
		start := time.Now()
		f()
		took += time.Since(start)
		rounds++
		close(done)
		<-sampled
		worst = max(worst, peak.Load()-idle)
		worstRSS = max(worstRSS, peakRSS.Load()-idleRSS)
	}
	b.ReportMetric(float64(worst)/(1<<20), "heap-peak-MiB")
	b.ReportMetric(float64(worstRSS)/(1<<20), "rss-peak-MiB")
	b.ReportMetric(float64(took.Microseconds())/1e3/float64(rounds), "call-ms")
}
