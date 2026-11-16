package rpc

import (
	"context"
	"encoding/base64"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cosmossdk.io/log/v2"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"

	tc "github.com/rit3sh-x/cosmos-svm/internal/testchain"
)

// TestOnlyALoopbackProxyNamesTheCaller proves that X-Forwarded-For names the
// caller only on a request from loopback, which is the gateway's, and only by
// its last address, which the proxy added. A client elsewhere that sends the
// header is limited by its own address.
func TestOnlyALoopbackProxyNamesTheCaller(t *testing.T) {
	for _, tt := range []struct{ remote, forwarded, want string }{
		{"203.0.113.7:4000", "198.51.100.9", "203.0.113.7:4000"},
		{"127.0.0.1:4000", "", "127.0.0.1:4000"},
		{"127.0.0.1:4000", "198.51.100.9", "198.51.100.9"},
		{"127.0.0.1:4000", "10.0.0.1, 198.51.100.9", "198.51.100.9"},
		{"[::1]:4000", "2001:db8::1", "2001:db8::1"},
		{"127.0.0.1:4000", " , ", "127.0.0.1:4000"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = tt.remote
		if tt.forwarded != "" {
			r.Header.Set("X-Forwarded-For", tt.forwarded)
		}
		if got := caller(r); got != tt.want {
			t.Errorf("from %s with X-Forwarded-For %q the caller is %q, want %q", tt.remote, tt.forwarded, got, tt.want)
		}
	}
}

// httpHeaders lists every header that the HTTP layer sets.
var httpHeaders = []string{"Content-Type", "Allow", "Accept", "Vary", "Access-Control-Allow-Origin",
	"Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Max-Age"}

func corsHeaders(origin string, more ...string) map[string]string {
	h := map[string]string{"Vary": "origin", "Access-Control-Allow-Methods": "OPTIONS, POST",
		"Access-Control-Allow-Origin": origin, "Access-Control-Max-Age": "86400"}
	for i := 0; i+1 < len(more); i += 2 {
		h[more[i]] = more[i+1]
	}
	return h
}

func merged(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}

// TestTheHTTPLayerIsAgaves checks the status, headers and body for each origin
// setting, HTTP method, path, Origin, requested headers and content type. The
// expected answers are those of jsonrpc-http-server 18.0.0 and Agave's
// middleware.
func TestTheHTTPLayerIsAgaves(t *testing.T) {
	_, s := server(t)
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(srv.Close)

	const (
		call       = `{"jsonrpc":"2.0","id":1,"method":"nope"}`
		notFound   = `{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":1}` + "\n"
		ourHost    = "rpc.example:8899"
		listed     = "https://app.example"
		textPlain  = "text/plain; charset=utf-8"
		jsonUTF8   = "application/json; charset=utf-8"
		optionsSet = "OPTIONS, POST"
	)
	jsonType := map[string]string{"Content-Type": jsonUTF8}
	plainText := map[string]string{"Content-Type": textPlain}
	options := map[string]string{"Allow": optionsSet, "Accept": "application/json"}
	list := Origins{Exact: []string{listed}}
	for _, tt := range []struct {
		name    string
		origins Origins
		method  string
		path    string
		headers map[string]string
		body    string
		status  int
		want    map[string]string
		answer  string
	}{
		{"any: a cross-origin call is echoed", AnyOrigin, "POST", "/", map[string]string{"Origin": "http://example.com"}, call, 200, merged(jsonType, corsHeaders("http://example.com")), notFound},
		{"any: the echo is Origin::parse's", AnyOrigin, "POST", "/", map[string]string{"Origin": "HTTPS://Example.COM:0443/x"}, call, 200, merged(jsonType, corsHeaders("https://example.com:443")), notFound},
		{"any: no scheme reads as http", AnyOrigin, "POST", "/", map[string]string{"Origin": "example.com:80"}, call, 200, merged(jsonType, corsHeaders("http://example.com:80")), notFound},
		{"any: null is refused", AnyOrigin, "POST", "/", map[string]string{"Origin": "null"}, call, 403, plainText, textOriginRefused},
		{"any: no Origin has no CORS headers", AnyOrigin, "POST", "/", nil, call, 200, jsonType, notFound},
		{"any: the same host has no CORS headers", AnyOrigin, "POST", "/", map[string]string{"Origin": "http://" + ourHost}, call, 200, jsonType, notFound},
		{"any: a preflight", AnyOrigin, "OPTIONS", "/", map[string]string{"Origin": "http://example.com", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type, solana-client,x-a"}, "", 200,
			merged(jsonType, options, corsHeaders("http://example.com", "Access-Control-Allow-Headers", "content-type, solana-client, x-a")), ""},
		{"any: OPTIONS with no Origin", AnyOrigin, "OPTIONS", "/", nil, "", 200, merged(jsonType, options), ""},
		{"any: text/plain is 415", AnyOrigin, "POST", "/", map[string]string{"Content-Type": "text/plain"}, call, 415, plainText, textContentType},
		{"any: no content type is 415", AnyOrigin, "POST", "/", map[string]string{"Content-Type": ""}, call, 415, plainText, textContentType},
		{"any: a JSON spelling in another case", AnyOrigin, "POST", "/", map[string]string{"Content-Type": "Application/JSON;charset=UTF-8"}, call, 200, jsonType, notFound},
		{"any: any path takes JSON-RPC", AnyOrigin, "POST", "/a/b", nil, call, 200, jsonType, notFound},
		{"any: GET is 405", AnyOrigin, "GET", "/", nil, "", 405, plainText, textMethod},
		{"any: a refusal still carries CORS headers", AnyOrigin, "PUT", "/", map[string]string{"Origin": "http://example.com"}, "", 405, merged(plainText, corsHeaders("http://example.com")), textMethod},
		{"any: over 50 KiB is 413", AnyOrigin, "POST", "/", nil, call + strings.Repeat(" ", maxBodyBytes), 413, plainText, textTooLarge},
		{"any: exactly 50 KiB is read", AnyOrigin, "POST", "/", nil, call + strings.Repeat(" ", maxBodyBytes-len(call)), 200, jsonType, notFound},
		{"any: a body that is not UTF-8 is 400", AnyOrigin, "POST", "/", nil, "{\"a\":\"\xc3\x28\"}", 400, plainText, "utf-8 encoding error at byte 6 in request body"},
		{"any: a notification is an empty 200", AnyOrigin, "POST", "/", nil, `{"jsonrpc":"2.0","method":"getSlot"}`, 200, jsonType, ""},
		{"list: a listed origin is echoed", list, "POST", "/", map[string]string{"Origin": listed}, call, 200, merged(jsonType, corsHeaders(listed)), notFound},
		{"list: an unlisted origin is refused", list, "POST", "/", map[string]string{"Origin": "https://evil.example"}, call, 403, plainText, textOriginRefused},
		{"list: a preflight from an unlisted origin", list, "OPTIONS", "/", map[string]string{"Origin": "https://evil.example"}, "", 403, merged(plainText, options), textOriginRefused},
		{"list: null is refused", list, "POST", "/", map[string]string{"Origin": "null"}, call, 403, plainText, textOriginRefused},
		{"list: no Origin is answered", list, "POST", "/", nil, call, 200, jsonType, notFound},
		{"list: the same host is answered", list, "POST", "/", map[string]string{"Origin": "http://" + ourHost}, call, 200, jsonType, notFound},
		{"empty: a cross-origin call is refused", Origins{}, "POST", "/", map[string]string{"Origin": listed}, call, 403, plainText, textOriginRefused},
		{"empty: no Origin is answered", Origins{}, "POST", "/", nil, call, 200, jsonType, notFound},
		{"/health has no content type and no CORS headers", AnyOrigin, "GET", "/health", map[string]string{"Origin": "http://example.com"}, "", 200, nil, "ok"},
		{"/health answers before an unlisted origin is refused", list, "POST", "/health", map[string]string{"Origin": "https://evil.example"}, "", 200, nil, "ok"},
		{"no genesis archive", AnyOrigin, "GET", "/genesis.tar.bz2", nil, "", 404, nil, ""},
		{"no snapshot", AnyOrigin, "GET", "/snapshot.tar.bz2", nil, "", 404, nil, ""},
		{"no incremental snapshot", AnyOrigin, "POST", "/incremental-snapshot.tar.bz2", nil, "", 404, nil, ""},
		{"no snapshot archive", AnyOrigin, "GET", "/snapshot-100-AbC9.tar.zst", nil, "", 404, nil, ""},
		{"no incremental archive", AnyOrigin, "GET", "/incremental-snapshot-100-200-x.tar.lz4", nil, "", 404, nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s.SetOrigins(tt.origins)
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Host = ourHost
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tt.headers {
				if v == "" {
					req.Header.Del(k)
					continue
				}
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.status || string(body) != tt.answer {
				t.Errorf("answered %d %q, want %d %q", resp.StatusCode, body, tt.status, tt.answer)
			}
			for _, h := range httpHeaders {
				if got, want := strings.Join(resp.Header.Values(h), ", "), tt.want[h]; got != want {
					t.Errorf("%s is %q, want %q", h, got, want)
				}
			}
		})
	}
}

// TestAnUnlistedOriginRunsNothing checks that the origin refusal comes before
// the method runs. A sendTransaction from an unlisted origin reaches no pool,
// and one from a listed origin does.
func TestAnUnlistedOriginRunsNothing(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	mp := &mempool{}
	s := New(c.App, mp, log.NewNopLogger())
	c.Block()
	s.SetOrigins(Origins{Exact: []string{"https://app.example"}})
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(srv.Close)
	faucet := tc.Key(1)
	send := func(origin string, amount uint64) int {
		tx := tc.Tx(c.Latest(), faucet, nil, tc.Transfer(faucet, tc.Pub(tc.Key(2)), amount))
		body := encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendTransaction",
			"params": []any{base64.StdEncoding.EncodeToString(tx), map[string]any{"encoding": "base64", "skipPreflight": true}}})
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := send("https://evil.example", 1); code != 403 || len(mp.sent) != 0 {
		t.Fatalf("an unlisted origin was answered %d and %d transactions reached the pool", code, len(mp.sent))
	}
	if code := send("https://app.example", 2); code != 200 || len(mp.sent) != 1 {
		t.Fatalf("a listed origin was answered %d and %d transactions reached the pool", code, len(mp.sent))
	}
}

// TestTheWebsocketHandshakeFollowsTheOrigins checks the handshake under each
// origin setting. Agave accepts every handshake, which is the default here. A
// list refuses an unlisted origin with 403 before it takes a connection slot.
func TestTheWebsocketHandshakeFollowsTheOrigins(t *testing.T) {
	_, s := server(t)
	s.SetLimits(1, 1, 1<<10)
	srv := httptest.NewServer(http.HandlerFunc(s.serveWS))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	same := "http://" + strings.TrimPrefix(srv.URL, "http://")
	list := Origins{Exact: []string{"https://app.example"}}
	for _, tt := range []struct {
		origins Origins
		origin  string
		ok      bool
	}{
		{AnyOrigin, "https://evil.example", true},
		{AnyOrigin, "null", true},
		{AnyOrigin, "", true},
		{list, "https://app.example", true},
		{list, "https://evil.example", false},
		{list, "null", false},
		{list, "", true},
		{list, same, true},
		{Origins{}, "https://app.example", false},
		{Origins{}, same, true},
	} {
		s.SetOrigins(tt.origins)
		h := http.Header{}
		if tt.origin != "" {
			h.Set("Origin", tt.origin)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(url, h)
		if tt.ok {
			if err != nil {
				t.Fatalf("%+v, origin %q: refused: %v", tt.origins, tt.origin, err)
			}
			// The one slot is free again after the server reads the close. A
			// refused handshake never takes the slot, so only this path waits.
			conn.Close()
			within(t, 30*time.Second, "the closed connection to be released", func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.live == 0
			})
			continue
		}
		if err == nil {
			conn.Close()
			t.Fatalf("%+v, origin %q: accepted", tt.origins, tt.origin)
		}
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 403 || string(body) != textOriginRefused {
			t.Fatalf("%+v, origin %q: answered %d %q", tt.origins, tt.origin, resp.StatusCode, body)
		}
	}
}

func TestTheOriginsSetting(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want Origins
		err  bool
	}{
		{"*", AnyOrigin, false},
		{" * ", AnyOrigin, false},
		{"", Origins{}, false},
		{"https://a.example, http://localhost:3000", Origins{Exact: []string{"https://a.example", "http://localhost:3000"}}, false},
		{"https://a.example/", Origins{}, true},
		{"a.example", Origins{}, true},
		{"null", Origins{}, true},
		{"https://*.example", Origins{}, false},
		{"https://a.example?x", Origins{}, true},
		{"https://u@a.example", Origins{}, true},
	} {
		got, err := ParseOrigins(tt.in)
		if tt.err != (err != nil) {
			t.Errorf("%q: %v", tt.in, err)
			continue
		}
		if tt.err {
			continue
		}
		if tt.in == "https://*.example" {
			// The parser accepts it as a name and matches it only exactly,
			// with no wildcard.
			if got.listed("https://a.example") {
				t.Errorf("%q matched as a wildcard", tt.in)
			}
			continue
		}
		if got.Any != tt.want.Any || strings.Join(got.Exact, ",") != strings.Join(tt.want.Exact, ",") {
			t.Errorf("%q read as %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

// catchingUp is a node that reports itself catching up.
type catchingUp struct{ *mempool }

func (catchingUp) Status(context.Context) (*coretypes.ResultStatus, error) {
	return &coretypes.ResultStatus{SyncInfo: coretypes.SyncInfo{CatchingUp: true}}, nil
}

// TestHealthIsGetHealthsCheck checks the /health text in each of Agave's three
// states.
func TestHealthIsGetHealthsCheck(t *testing.T) {
	c := tc.Start(t, tc.Pub(tc.Key(1)), tc.Options{})
	s := New(c.App, nil, log.NewNopLogger())
	s.peers.every = 0
	c.Block()
	if got := s.healthText(); got != "ok" {
		t.Fatalf("a machine with no peers is %q", got)
	}
	peers := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "cometbft_consensus_peer_height", Help: "stand-in for the consensus reactor's per-peer heights"}, []string{"peer_id"})
	prometheus.MustRegister(peers)
	defer prometheus.Unregister(peers)
	peers.WithLabelValues("a").Set(float64(uint64(c.App.LastBlockHeight()) + healthCheckBlockDistance + 1))
	if got := s.healthText(); got != "behind" {
		t.Fatalf("a machine behind its peers is %q", got)
	}
	s.node = catchingUp{&mempool{}}
	if got := s.healthText(); got != "unknown" {
		t.Fatalf("a machine catching up is %q", got)
	}
}
