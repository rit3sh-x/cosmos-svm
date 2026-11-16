package rpc

import (
	"bufio"
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// TestTheScanSlotsDefaultToAgavesCount proves the default is one slot per four
// CPUs, never fewer than one, as Agave's --rpc-blocking-threads default is.
func TestTheScanSlotsDefaultToAgavesCount(t *testing.T) {
	for procs, want := range map[int]int{1: 1, 4: 1, 7: 1, 8: 2} {
		if got := scanSlots(procs); got != want {
			t.Errorf("GOMAXPROCS %d gives %d scan slots, want %d", procs, got, want)
		}
	}
	_, s := server(t)
	if got := s.SetMaxScans(0); got != scanSlots(runtime.GOMAXPROCS(0)) {
		t.Fatalf("zero keeps %d slots, want the default", got)
	}
	if got := s.SetMaxScans(3); got != 3 {
		t.Fatalf("an explicit 3 gives %d slots", got)
	}
}

// TestAQueuedScanLeavesWithItsClient proves scans queue for the one slot,
// the gauge counts them, and a client that hangs up leaves the queue. A point
// read is answered while the slot is held, and a batch takes the slot per
// entry.
func TestAQueuedScanLeavesWithItsClient(t *testing.T) {
	_, s := server(t)
	s.SetMaxScans(1)
	// Two waiting scans and the point read each hold an HTTP slot.
	s.SetThreads(3)
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(srv.Close)
	system := base58.Encode(svm.SystemProgram[:])
	scanBody := encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{system, map[string]any{"encoding": "base64", "filters": []any{map[string]any{"dataSize": 7}}}}})
	post := func(ctx context.Context, body []byte) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var out bytes.Buffer
		_, err = out.ReadFrom(resp.Body)
		return out.Bytes(), err
	}
	gauge := func() float64 {
		var m dto.Metric
		_ = scansWaiting.Write(&m)
		return m.GetGauge().GetValue()
	}
	waiting := func(want float64) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); gauge() != want; {
			if time.Now().After(deadline) {
				t.Fatalf("%v scans waiting, want %v", gauge(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	s.scans <- struct{}{}
	release := sync.OnceFunc(func() { <-s.scans })
	t.Cleanup(release)
	ctx, hangUp := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() { _, err := post(ctx, scanBody); left <- err }()
	waiting(1)
	stayed := make(chan []byte, 1)
	go func() { out, _ := post(context.Background(), scanBody); stayed <- out }()
	waiting(2)

	payer := tc.Pub(tc.Key(1))
	point, err := post(context.Background(), encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "getAccountInfo",
		"params": []any{base58.Encode(payer[:]), map[string]string{"encoding": "base64"}}}))
	if err != nil || !bytes.Contains(point, []byte(`"lamports"`)) {
		t.Fatalf("a point read while the slot was held was answered %s, %v", point, err)
	}

	hangUp()
	if err := <-left; err == nil {
		t.Fatal("the client that hung up was answered")
	}
	waiting(1)
	release()
	if out := <-stayed; !bytes.Contains(out, []byte(`"result":[]`)) {
		t.Fatalf("the client that stayed was answered %s", out)
	}
	waiting(0)

	// A batch queues its scanning entry for the slot like any other call.
	s.scans <- struct{}{}
	batch := make(chan []byte, 1)
	go func() {
		out, _ := post(context.Background(), []byte("["+string(scanBody)+","+string(scanBody)+"]"))
		batch <- out
	}()
	waiting(1)
	<-s.scans
	if out := <-batch; bytes.Count(out, []byte(`"result":[]`)) != 2 {
		t.Fatalf("a batch of two scans was answered %s", out)
	}
	waiting(0)
}

// TestASlotIsHeldUntilItsAnswerIsWritten checks that an HTTP request holds one
// of the rpc-threads slots until its answer is written. A scan also holds its
// rpc-max-scans slot inside it for that time. A client that reads none of a
// large answer holds both slots until rpc-write-timeout cuts it. A client
// that hangs up while it waits for a slot leaves.
func TestASlotIsHeldUntilItsAnswerIsWritten(t *testing.T) {
	c, s := server(t)
	if got := s.SetThreads(0); got != runtime.GOMAXPROCS(0) {
		t.Fatalf("zero keeps %d HTTP slots, want one per CPU", got)
	}
	big := bigAccounts(t, c, 2)
	s.SetWriteTimeout(500 * time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(srv.Close)
	req := func(method string, params ...any) string {
		return string(encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}))
	}
	base64 := map[string]any{"encoding": "base64"}
	// Two 10 MiB accounts, each named twice, make 56 MB of answer. That is
	// more than the loopback socket buffers hold, so a client that reads
	// nothing stalls the write.
	keys := []string{base58.Encode(big[0][:]), base58.Encode(big[1][:])}
	huge := req("getMultipleAccounts", append(keys, keys...), base64)
	// The scan of the system program finds both (28 MB). With a data slice
	// of zero bytes, it answers in a few hundred bytes.
	system := base58.Encode(svm.SystemProgram[:])
	scan := req("getProgramAccounts", system, base64)
	small := req("getProgramAccounts", system, map[string]any{"encoding": "base64", "dataSlice": map[string]int{"offset": 0, "length": 0}})
	point := req("getSlot")

	stall := func(body string) net.Conn {
		t.Helper()
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	cut := func(conn net.Conn) {
		t.Helper()
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
		}
		if err == nil {
			t.Fatal("a client that read nothing for the write timeout was sent its whole answer")
		}
	}
	post := func(ctx context.Context, body string) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); {
			if time.Now().After(deadline) {
				t.Fatalf("still not: %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	gauge := func(g prometheus.Gauge) float64 {
		var m dto.Metric
		_ = g.Write(&m)
		return m.GetGauge().GetValue()
	}

	// One HTTP slot, held by a client that reads nothing.
	s.SetThreads(1)
	stalled := stall(huge)
	until("the stalled request holds the slot", func() bool { return len(s.threads) == 1 })
	ctx, hangUp := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() { _, err := post(ctx, point); left <- err }()
	stayed := make(chan []byte, 1)
	go func() { out, _ := post(context.Background(), point); stayed <- out }()
	until("two requests wait", func() bool { return gauge(requestsWaiting) == 2 })
	hangUp()
	if err := <-left; err == nil {
		t.Fatal("the client that hung up was answered")
	}
	until("one request waits", func() bool { return gauge(requestsWaiting) == 1 })
	if out := <-stayed; !bytes.Contains(out, []byte(`"result"`)) {
		t.Fatalf("the request that waited was answered %s", out)
	}
	cut(stalled)

	// Two HTTP slots and one scan slot: a scan holds its slot while it writes.
	s.SetThreads(2)
	s.SetMaxScans(1)
	stalled = stall(scan)
	until("the stalled scan holds the scan slot", func() bool { return len(s.scans) == 1 })
	go func() { out, _ := post(context.Background(), small); stayed <- out }()
	until("a scan waits", func() bool { return gauge(scansWaiting) == 1 })
	if out := <-stayed; !bytes.Contains(out, []byte(`"result":[{`)) {
		t.Fatalf("the scan that waited was answered %.200s", out)
	}
	cut(stalled)
	until("nothing waits", func() bool { return gauge(requestsWaiting) == 0 && gauge(scansWaiting) == 0 })
}

// TestABatchIsCutWhenItsWritesSpendTheBudget checks the write budget. An
// answer may spend rpc-write-timeout on writes in total, not that much for
// each entry. The client reads one byte a millisecond and gets the time for
// two and a half entries. Each entry fits alone, and the third entry is cut.
func TestABatchIsCutWhenItsWritesSpendTheBudget(t *testing.T) {
	_, s := server(t)
	clock := &stubClock{}
	s.writeClock = clock
	var calls []string
	for id := 1; id <= 3; id++ {
		calls = append(calls, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"getSlot"}`, id))
	}
	batch := []byte("[" + strings.Join(calls, ",") + "]")
	read := func(budget time.Duration) string {
		s.SetWriteTimeout(budget)
		client := &slowReader{ResponseRecorder: httptest.NewRecorder(), clock: clock, perByte: time.Millisecond}
		s.answer(context.Background(), batch, "", client)
		return client.Body.String()
	}
	whole := read(time.Hour)
	if !strings.HasPrefix(whole, "[") || !strings.HasSuffix(whole, "]\n") || strings.Count(whole, `"result"`) != 3 {
		t.Fatalf("the batch was answered %q", whole)
	}
	// All entries have the same length. Each entry is written with its
	// separator.
	entry := (len(whole) - len("]\n")) / 3
	cut := read(time.Duration(entry*5/2) * time.Millisecond)
	if !strings.HasPrefix(whole, cut) || !strings.Contains(cut, `"id":2}`) || strings.Contains(cut, `"id":3}`) {
		t.Fatalf("with two and a half entries of budget the batch was written as %q, of %q", cut, whole)
	}
}

// slowReader is a client that takes perByte, on clock, to read each byte. As a
// socket does, it fails a write that would end after its deadline.
type slowReader struct {
	*httptest.ResponseRecorder
	clock    *stubClock
	perByte  time.Duration
	deadline time.Time
}

func (c *slowReader) SetWriteDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

func (c *slowReader) Write(p []byte) (int, error) {
	if c.clock.Now().Add(time.Duration(len(p)) * c.perByte).After(c.deadline) {
		c.clock.advance(max(c.deadline.Sub(c.clock.Now()), 0))
		return 0, os.ErrDeadlineExceeded
	}
	c.clock.advance(time.Duration(len(p)) * c.perByte)
	return c.ResponseRecorder.Write(p)
}

// TestAScanPastItsByteLimitStopsAsAgaves checks the scan cap, which is Agave's
// --accounts-index-scan-results-limit-mb. Each kept account counts its data
// plus 96 bytes. A scan whose sum is more than the limit stops with Agave's
// -32012. A scan whose sum is equal to the limit gets an answer.
func TestAScanPastItsByteLimitStopsAsAgaves(t *testing.T) {
	_, s := server(t)
	if s.SetScanLimit(3); s.scanLimit != 3<<20 {
		t.Fatalf("3 MiB is a limit of %d bytes", s.scanLimit)
	}
	scan := call{v2: true, id: []byte("1"), method: "getProgramAccounts",
		params: encode([]any{base58.Encode(svm.SystemProgram[:]), map[string]any{"encoding": "base64"}})}
	s.SetScanLimit(0)
	var rows []struct{ Account struct{ Space int } }
	raw, _ := json.Marshal(mustCall(t, s, context.Background(), scan), json.Deterministic(true))
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		t.Fatalf("the system program's scan answered %s", raw)
	}
	sum := 0
	for _, r := range rows {
		sum += r.Account.Space + 96
	}
	s.scanLimit = sum
	mustCall(t, s, context.Background(), scan)
	s.scanLimit = sum - 1
	resp := callOne(s, context.Background(), scan, "")
	if resp.Error == nil || resp.Error.Code != -32012 || resp.Error.Message != "scan aborted: The accumulated scan results exceeded the limit" {
		t.Fatalf("a scan one byte past its limit answered %+v", resp.Error)
	}
}
