package rpc

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The HTTP layer is Agave v4.3.0's: its request middleware, then
// jsonrpc-http-server 18.0.0, whose CORS is jsonrpc-server-utils 18.0.0.

// Origins is the cross-origin policy from [svm] rpc-cors-origins. Any is
// Agave's AllowOnly([Any]). Otherwise the server answers only an origin in
// Exact, with no wildcards.
type Origins struct {
	Any   bool
	Exact []string
}

// AnyOrigin is the default, as in Agave.
var AnyOrigin = Origins{Any: true}

// ParseOrigins reads the setting: "*", or a comma-separated list of
// scheme://host[:port]. An empty list refuses every cross-origin call.
func ParseOrigins(v string) (Origins, error) {
	if strings.TrimSpace(v) == "*" {
		return AnyOrigin, nil
	}
	var o Origins
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		u, err := url.Parse(e)
		if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(e, "?") {
			return Origins{}, fmt.Errorf("svm.rpc-cors-origins: %q is not scheme://host[:port], and only * or a list of them is", e)
		}
		o.Exact = append(o.Exact, e)
	}
	return o, nil
}

// listed reports whether the server answers a cross-origin request from
// origin. It never answers "null". Agave refuses it under Any, because its
// list holds no Null.
func (o Origins) listed(origin string) bool {
	if origin == "null" {
		return false
	}
	if o.Any {
		return true
	}
	for _, e := range o.Exact {
		if strings.EqualFold(e, origin) {
			return true
		}
	}
	return false
}

// crossOrigin is get_cors_allow_origin. No Origin, or one whose host is the
// Host header, needs no CORS headers. A listed origin is echoed as
// Origin::parse writes it back. Any other origin is refused.
func (o Origins) crossOrigin(origin, host string) (echo string, ok bool) {
	if origin == "" || sameHost(origin, host) {
		return "", true
	}
	if !o.listed(origin) {
		return "", false
	}
	return parsedOrigin(origin), true
}

// upgrade reports whether the server accepts a websocket handshake. Agave
// accepts every handshake, "null" included, which is Any here. A list refuses
// an unlisted origin from another host.
func (o Origins) upgrade(origin, host string) bool {
	return origin == "" || o.Any || sameHost(origin, host) || o.listed(origin)
}

func sameHost(origin, host string) bool {
	return host != "" && strings.HasSuffix(origin, host) && parsedHost(originHost(origin)) == host
}

// originHost returns what Origin::parse takes as the host part. That is the
// piece after the first "://", up to the next one.
func originHost(origin string) string {
	_, rest, found := strings.Cut(origin, "://")
	if !found {
		return origin
	}
	rest, _, _ = strings.Cut(rest, "://")
	return rest
}

// parsedOrigin is Origin::parse, then its string. The scheme is in lower case,
// and is http when there is none.
func parsedOrigin(origin string) string {
	scheme, _, found := strings.Cut(origin, "://")
	if !found {
		scheme = "http"
	}
	return strings.ToLower(scheme) + "://" + parsedHost(originHost(origin))
}

// parsedHost is Host::parse, then its string. It is in lower case and cut at
// the first '/'. A port that parses as a u16 is written as that number.
func parsedHost(h string) string {
	h, _, _ = strings.Cut(h, "/")
	h = strings.ToLower(h)
	name, port, found := strings.Cut(h, ":")
	if !found {
		return name
	}
	port, _, _ = strings.Cut(port, ":")
	// Rust's u16 parse takes one leading '+'.
	if n, err := strconv.ParseUint(strings.TrimPrefix(port, "+"), 10, 16); err == nil {
		port = strconv.FormatUint(n, 10)
	}
	return name + ":" + port
}

// visible is hyper's HeaderValue::to_str test. A value with a byte that is
// not visible ASCII or a tab reads as absent.
func visible(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; c != '\t' && (c < 0x20 || c >= 0x7f) {
			return false
		}
	}
	return true
}

func headerText(v string) string {
	if !visible(v) {
		return ""
	}
	return v
}

// requestedHeaders is Access-Control-Allow-Headers under AllowHeaders::Any.
// It holds every header the request names, split the same way as the Agave
// HTTP server splits them.
func requestedHeaders(r *http.Request) (string, bool) {
	var names []string
	for _, v := range r.Header.Values("Access-Control-Request-Headers") {
		if !visible(v) {
			continue
		}
		for _, part := range strings.Split(v, ", ") {
			names = append(names, strings.Split(part, ",")...)
		}
	}
	return strings.Join(names, ", "), len(names) > 0
}

// The refusals, each text/plain.
const (
	textOriginRefused   = "Origin of the request is not whitelisted. CORS headers would not be sent and any side-effects were cancelled as well.\n"
	textContentType     = "Supplied content type is not allowed. Content-Type: application/json is required\n"
	textMethod          = "Used HTTP Method is not allowed. POST or OPTIONS is required\n"
	textTooLarge        = "request body size exceeds allowed maximum"
	contentTypeJSON     = "application/json; charset=utf-8"
	allowedHTTPMethods  = "OPTIONS, POST"
	genesisDownloadPath = "/genesis.tar.bz2"
)

func plain(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

// snapshotArchive matches Agave's two snapshot archive names.
var snapshotArchive = regexp.MustCompile(`^/(snapshot-[[:digit:]]+-[[:alnum:]]+|incremental-snapshot-[[:digit:]]+-[[:digit:]]+-[[:alnum:]]+)\.(tar\.zst|tar\.lz4)$`)

// middleware answers the paths that Agave's request middleware takes before
// JSON-RPC, for any HTTP method and with no CORS headers. This chain has no
// Agave genesis archive and no Agave snapshot, because CometBFT state sync
// does both jobs. Those paths get Agave's answer for a file it does not hold,
// a 404 with no body. The /v0 supply paths answer getSupply's figures in
// Agave's text, or 404 while the total is unknown.
func (s *Server) middleware(w http.ResponseWriter, r *http.Request) bool {
	// hyper's path is the escaped one, so /heal%74h is not /health.
	switch p := r.URL.EscapedPath(); {
	case p == "/health":
		// Agave sets no content type, so Go must not sniff one.
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, s.healthText())
	case p == "/v0/circulating-supply" || p == "/v0/total-supply":
		text, ok := s.v0Supply(p)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, text)
	case p == genesisDownloadPath || p == "/snapshot.tar.bz2" || p == "/incremental-snapshot.tar.bz2" || snapshotArchive.MatchString(p):
		w.WriteHeader(http.StatusNotFound)
	default:
		return false
	}
	return true
}

// serveHTTP is jsonrpc-http-server's handler. It checks CORS first, then the
// HTTP method and the content type. Every answer, refusals included, carries
// the headers of set_response_headers.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if s.middleware(w, r) {
		return
	}
	echo, ok := s.origins.crossOrigin(headerText(r.Header.Get("Origin")), r.Host)
	h := w.Header()
	if r.Method == http.MethodOptions {
		h.Set("Allow", allowedHTTPMethods)
		h.Set("Accept", "application/json")
	}
	if echo != "" {
		h.Set("Vary", "origin")
		h.Set("Access-Control-Allow-Methods", allowedHTTPMethods)
		h.Set("Access-Control-Allow-Origin", echo)
		h.Set("Access-Control-Max-Age", "86400")
		if names, named := requestedHeaders(r); named {
			h.Set("Access-Control-Allow-Headers", names)
		}
	}
	switch {
	case !ok:
		plain(w, http.StatusForbidden, textOriginRefused)
	case r.Method == http.MethodPost && isJSON(r.Header.Get("Content-Type")):
		s.post(w, r)
	case r.Method == http.MethodPost:
		plain(w, http.StatusUnsupportedMediaType, textContentType)
	case r.Method == http.MethodOptions:
		h.Set("Content-Type", contentTypeJSON)
	default:
		plain(w, http.StatusMethodNotAllowed, textMethod)
	}
}

// isJSON accepts the three spellings that jsonrpc-http-server takes, in any case.
func isJSON(v string) bool {
	v = headerText(v)
	return strings.EqualFold(v, "application/json") || strings.EqualFold(v, "application/json; charset=utf-8") || strings.EqualFold(v, "application/json;charset=utf-8")
}

// post reads at most maxBodyBytes and answers the body. An answer ends with a
// newline. A body with nothing to answer gets a 200 with an empty body.
func (s *Server) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		plain(w, http.StatusRequestEntityTooLarge, textTooLarge)
		return
	case err != nil:
		return
	case !utf8.Valid(body):
		plain(w, http.StatusBadRequest, fmt.Sprintf("utf-8 encoding error at byte %d in request body", validUpTo(body)))
		return
	}
	// The request holds one of the rpc-threads slots until its answer is
	// written. Agave's --rpc-threads bounds every request in the same way. A
	// client that hangs up while it waits leaves the queue. The request takes
	// the slot after it reads the body, so a slow sender holds no slot.
	slots := s.threads
	requestsWaiting.Inc()
	select {
	case slots <- struct{}{}:
		requestsWaiting.Dec()
		defer func() { <-slots }()
	case <-r.Context().Done():
		requestsWaiting.Dec()
		return
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	s.answer(r.Context(), body, caller(r), w)
}

// caller returns the address that the devnet faucet limits a request by. The
// gateway is a proxy on this machine, so every request it passes on comes
// from loopback. The faucet does not limit loopback by address. A request
// from loopback that carries X-Forwarded-For came through such a proxy. Its
// caller is the last address in the header, which the proxy added. The
// gateway example sets the header to the client's address and drops any value
// that the client sent. A request from any other address is limited by that
// address, whatever headers it carries.
func caller(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return r.RemoteAddr
	}
	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return r.RemoteAddr
	}
	last := forwarded[len(forwarded)-1]
	if last = strings.TrimSpace(last[strings.LastIndexByte(last, ',')+1:]); last == "" {
		return r.RemoteAddr
	}
	return last
}

// validUpTo is Rust's Utf8Error::valid_up_to, the index where the first
// invalid sequence starts.
func validUpTo(b []byte) int {
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n == 1 {
			return i
		}
		i += n
	}
	return len(b)
}
