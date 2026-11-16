package rpc

import (
	"bytes"
	"encoding/binary"
	json "encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cosmossdk.io/log/v2"

	"github.com/gorilla/websocket"
	"github.com/mr-tron/base58"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// server starts a chain with one committed block and returns it with its RPC
// server.
func server(t *testing.T) (*tc.Chain, *Server) {
	t.Helper()
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	c.Block()
	return c, s
}

// tryDial is dial for a test that expects the handshake to be refused.
func tryDial(t *testing.T, s *Server) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.serveWS))
	t.Cleanup(srv.Close)
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	return conn, resp, err
}

func live(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// TestTheCeilingIsAProductOfTwoCaps checks the caps that New ships, then hits
// each one. The subscription cap stays at its real 1,024. The test lowers the
// connection cap to 2, because 1,024 live sockets prove nothing that 3 do not.
func TestTheCeilingIsAProductOfTwoCaps(t *testing.T) {
	_, s := server(t)
	if s.maxConns != 1024 || s.maxSubs != 1024 || s.maxQueueBytes != 256<<10 {
		t.Fatalf("the shipped ceiling is %d x %d subscriptions and %d bytes",
			s.maxConns, s.maxSubs, s.maxQueueBytes)
	}

	// Distinct params each, since the same params twice on one connection
	// are one subscription.
	p := dial(t, s)
	account := func(i int) string {
		var k [32]byte
		binary.LittleEndian.PutUint64(k[:], uint64(i)+1)
		return base58.Encode(k[:])
	}
	for i := 0; i < s.maxSubs; i++ {
		p.subscribe("accountSubscribe", account(i))
	}
	p.next++
	if err := p.conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": p.next, "method": "accountSubscribe",
		"params": []any{account(s.maxSubs)}}); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Error *rpcError `json:"error"`
	}
	p.read(&resp)
	// This is Agave's own string. It does not say which cap was hit, or that
	// the refusal is not an internal fault.
	if resp.Error == nil || resp.Error.Code != codeInternal ||
		resp.Error.Message != "Internal Error: Subscription refused. Node subscription limit reached" {
		t.Fatalf("the subscribe over the cap was answered %+v", resp.Error)
	}

	s.SetLimits(2, 0, 0)
	if live(s) != 1 {
		t.Fatalf("%d connections are live before the second", live(s))
	}
	if _, _, err := tryDial(t, s); err != nil {
		t.Fatalf("the second connection was refused: %v", err)
	}
	conn, refusal, err := tryDial(t, s)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the connection over the cap was upgraded")
	}
	// The server refuses before the upgrade, because no JSON-RPC channel
	// exists yet to carry a refusal.
	if refusal == nil || refusal.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the connection over the cap got %v", refusal)
	}
}

// TestAnUnsubscribeOwnsWhatItNames checks that a subscription id belongs to
// the connection that holds it. Another connection's id gets the same refusal
// as an id that nobody holds.
func TestAnUnsubscribeOwnsWhatItNames(t *testing.T) {
	c, s := server(t)
	faucet := tc.Key(1)
	dest := tc.Pub(tc.Key(21))

	mine := dial(t, s)
	yours := dial(t, s)
	id := mine.subscribe("accountSubscribe", base58.Encode(dest[:]))

	if err := yours.unsubscribeError("accountUnsubscribe", id); err == nil ||
		err.Code != codeInvalidParams || err.Message != "Invalid subscription id." {
		t.Fatalf("another connection cancelled %d and was answered %v", id, err)
	}
	// The subscription that the other connection failed to cancel is still live.
	if res, _ := c.Block(tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, dest, svm.MinimumBalance(0)))); res[0].Code != 0 {
		t.Fatalf("the transfer committed as %d", res[0].Code)
	}
	if err := s.notify(uint64(c.Height)); err != nil {
		t.Fatal(err)
	}
	if got := mine.collect(1); len(got[accountNotification]) != 1 {
		t.Fatalf("the surviving subscription produced %v", counts(got))
	}
	if !mine.unsubscribe("accountUnsubscribe", id) {
		t.Fatal("the holder could not cancel its own subscription")
	}
}

// TestASignatureSubscriptionIsCancellable checks that signatureUnsubscribe
// removes the watcher. Otherwise a signature that never lands stays held until
// the connection closes.
func TestASignatureSubscriptionIsCancellable(t *testing.T) {
	_, s := server(t)
	p := dial(t, s)
	sig := make([]byte, 64)
	sig[0] = 7
	id := p.subscribe("signatureSubscribe", base58.Encode(sig))

	s.mu.Lock()
	held := len(s.watchers)
	s.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d signatures are watched, want the one subscribed", held)
	}
	if !p.unsubscribe("signatureUnsubscribe", id) {
		t.Fatalf("signatureUnsubscribe did not remove %d", id)
	}
	s.mu.Lock()
	held = len(s.watchers)
	s.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d signatures are still watched after the unsubscribe", held)
	}
	if err := p.unsubscribeError("signatureUnsubscribe", id); err == nil || err.Code != codeInvalidParams {
		t.Fatalf("cancelling %d twice was answered %v", id, err)
	}
}

// TestTheReadLimitsAreAgaves checks Agave's read limits: 4,096 bytes for a
// websocket message and 50 KiB for an HTTP body.
func TestTheReadLimitsAreAgaves(t *testing.T) {
	_, s := server(t)
	p := dial(t, s)
	big := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "slotSubscribe", "params": []any{strings.Repeat("x", 5<<10)}}
	if err := p.conn.WriteJSON(big); err != nil {
		t.Fatal(err)
	}
	_ = p.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := p.conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		t.Fatalf("a 5 kB message closed the connection with %v, want status 1009", err)
	}

	srv := httptest.NewServer(newHTTPHandler(s))
	t.Cleanup(srv.Close)
	if code, _ := post(t, srv.URL, bytes.Repeat([]byte("y"), 60<<10)); code != 413 {
		t.Fatalf("a 60 kB body was answered %d, want 413", code)
	}
	if code, body := post(t, srv.URL, []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot"}`)); code != 200 || !strings.Contains(body, `"result"`) {
		t.Fatalf("an ordinary request was answered %d %s", code, body)
	}
}

// TestANotificationIsNeverAnswered checks that a call without an id gets no
// answer. web3.js sends `{"jsonrpc":"2.0","method":"ping"}` every five seconds
// with no id.
func TestANotificationIsNeverAnswered(t *testing.T) {
	_, s := server(t)
	p := dial(t, s)
	for i := 0; i < 3; i++ {
		if err := p.conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "ping"}); err != nil {
			t.Fatal(err)
		}
	}
	// subscribe fails if the next frame is an error, so this passes only if
	// no ping got an answer.
	p.subscribe("slotSubscribe")

	srv := httptest.NewServer(newHTTPHandler(s))
	t.Cleanup(srv.Close)
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"getSlot"}`,
		`[{"jsonrpc":"2.0","method":"getSlot"},{"jsonrpc":"2.0","method":"ping"}]`,
	} {
		if code, out := post(t, srv.URL, []byte(body)); code != 200 || out != "" {
			t.Fatalf("%s was answered %d %q, want 200 and an empty body", body, code, out)
		}
	}
	// An explicit null id is not an absent one, and is still answered.
	if code, out := post(t, srv.URL, []byte(`{"jsonrpc":"2.0","id":null,"method":"getSlot"}`)); code != 200 || !strings.Contains(out, `"result"`) {
		t.Fatalf("an explicit null id was answered %d %q", code, out)
	}
}

// TestADeadPeerIsReapedAndALiveOneIsNot checks that the server closes a silent
// peer and keeps a peer that only reads. The blocking Rust PubsubClient only
// reads. It survives only because the server pings and its library answers.
func TestADeadPeerIsReapedAndALiveOneIsNot(t *testing.T) {
	_, s := server(t)
	s.deadlines.idle, s.deadlines.ping = 300*time.Millisecond, 100*time.Millisecond
	reader := dial(t, s)
	frames := make(chan []byte, 8)
	go func() {
		for {
			_ = reader.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, b, err := reader.conn.ReadMessage()
			if err != nil {
				close(frames)
				return
			}
			frames <- b
		}
	}()
	_, _, err := tryDial(t, s)
	if err != nil {
		t.Fatal(err)
	}

	// Three idle timeouts pass with no writes from either client. The server
	// reaps the silent peer by then or soon after.
	time.Sleep(3 * s.deadlines.idle)
	for deadline := time.Now().Add(10 * time.Second); live(s) > 1 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if got := live(s); got != 1 {
		t.Fatalf("%d connections are live, want the reader's alone", got)
	}

	// The reader is still served, which the live count alone does not prove.
	if err := reader.conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "slotSubscribe"}); err != nil {
		t.Fatal(err)
	}
	select {
	case b, ok := <-frames:
		if !ok {
			t.Fatal("the reading peer was closed")
		}
		var resp struct {
			Result *uint64 `json:"result"`
		}
		if err := json.Unmarshal(b, &resp); err != nil || resp.Result == nil {
			t.Fatalf("the reading peer was answered %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reading peer was not answered")
	}
}

func newHTTPHandler(s *Server) http.Handler { return http.HandlerFunc(s.serveHTTP) }

func post(t *testing.T, url string, body []byte) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, strings.TrimSpace(string(out))
}

// TestAConnectionThatSendsNoRequestIsHungUpOn checks the header deadline. The
// other limits count what a connection sends, so a silent peer reaches none of
// them, and net/http has no default header timeout.
func TestAConnectionThatSendsNoRequestIsHungUpOn(t *testing.T) {
	_, s := server(t)
	s.deadlines.header = 200 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := s.bounded(ln.Addr().String(), newHTTPHandler(s))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Half a request, which is what a slow loris sends and then holds.
	if _, err := conn.Write([]byte("POST / HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("the connection was still open after %s: %v", s.deadlines.header, err)
	}
}
