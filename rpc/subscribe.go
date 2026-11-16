package rpc

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"strconv"
	"time"

	"github.com/mr-tron/base58"

	"github.com/rit3sh-x/cosmos-svm/ffi"
	"github.com/rit3sh-x/cosmos-svm/x/svm"
)

// The notification methods, which are also the registry's kinds.
const (
	accountNotification   = "accountNotification"
	programNotification   = "programNotification"
	logsNotification      = "logsNotification"
	slotNotification      = "slotNotification"
	slotsUpdates          = "slotsUpdatesNotification"
	rootNotification      = "rootNotification"
	signatureNotification = "signatureNotification"
)

// sub is one live subscription. The method decides which fields matter. key
// is the account or the program, and filters and cfg are the account
// rendering. mentions is the logs filter, and nil means every transaction.
type sub struct {
	conn     *wsConn
	id       uint64
	method   string
	key      [32]byte
	sig      [64]byte
	cfg      accountConfig
	filters  []matcher
	mentions map[[32]byte]bool
	// params is the subscription's identity. Every holder of the same params
	// shares one id, as Agave's tracker keys subscriptions by their params.
	params string
	// received asks for a receivedSignature notification before the
	// final one.
	received bool
	// landed is the status of a signature already committed when it was
	// subscribed to. The next commit notifies it.
	landed *signatureStatus
	// told is whether receivedSignature has been sent, which Agave sends once.
	told bool
	// at is the index of the subscription in the one index list that holds it.
	at int
}

// shared is one distinct subscription: its id, and how many connections
// hold it.
type shared struct {
	id      uint64
	holders int
}

// refused is Agave's answer to a subscribe over the cap, character for
// character. Agave maps TooManySubscriptions to a generic internal error with
// a fixed string. The code is wrong on mainnet too, so a client that matches
// the mainnet string matches here. The error does not say which cap was hit,
// because Agave does not say it either.
func refused() error {
	return &rpcError{Code: codeInternal, Message: "Internal Error: Subscription refused. Node subscription limit reached"}
}

// admit reserves a subscription slot on the connection. The caller holds
// s.mu. Agave reserves before it looks for an entry with the same params. So
// a connection at its cap is refused even an id it already holds.
func (s *Server) admit(c *wsConn) error {
	if len(c.subs) >= s.maxSubs {
		return refused()
	}
	return nil
}

// register files a subscription and returns its id.
//
// Ids count from 0. The same params share one id across connections. The
// same params twice on one connection are one subscription with one id,
// because Agave's connection map replaces the first token.
//
// Only an account or a program subscription holds the feed, because only
// those two read the write set. A machine whose subscribers all watch slots
// copies nothing. Each of those subscriptions holds its own key, so a block
// copies only the accounts that a subscriber watches.
func (s *Server) register(x *sub) uint64 {
	sh := s.shared[x.params]
	if sh == nil {
		sh = &shared{id: s.nextSub}
		s.nextSub++
		s.shared[x.params] = sh
	}
	x.id = sh.id
	if _, held := x.conn.subs[x.id]; held {
		return x.id
	}
	sh.holders++
	x.conn.subs[x.id] = x
	switch x.method {
	case accountNotification:
		s.accounts[x.key] = added(s.accounts[x.key], x)
		s.app.Feed.Hold(x.key, false)
	case programNotification:
		s.programs[x.key] = added(s.programs[x.key], x)
		s.app.Feed.Hold(x.key, true)
	case logsNotification:
		s.logs = added(s.logs, x)
	case slotNotification:
		s.slots = added(s.slots, x)
	case slotsUpdates:
		s.updates = added(s.updates, x)
	case rootNotification:
		s.roots = added(s.roots, x)
	case signatureNotification:
		s.watchers[x.sig] = added(s.watchers[x.sig], x)
		if x.received {
			if s.receivers++; s.receivers == 1 {
				s.app.Feed.OnReceived(s.received)
			}
		}
	}
	return x.id
}

// receipt is the signatures of one block, on their way from the runner's hook
// to Follow.
type receipt struct {
	height uint64
	sigs   [][64]byte
}

// maxReceipts is how many receipts can wait for Follow. A block takes at least
// 400 ms, and Follow takes each receipt when it arrives. A full queue means
// that Follow has stopped, so the newest receipt is dropped and the hook does
// not wait.
const maxReceipts = 64

// received is the receivedSignature hook. The runner calls it with a block's
// signatures after the block executes and before it commits. It runs inside
// FinalizeBlock, so it takes no lock. A client controls how long mu is held,
// through the subscriptions it makes, and a block must not wait for that.
func (s *Server) received(height uint64, sigs [][64]byte) {
	select {
	case s.receipts <- receipt{height, sigs}:
	default:
		s.logger.Error("receivedSignature notifications dropped: Follow is behind", "height", height)
	}
}

// receivedFor appends the receivedSignature notifications that a receipt
// owes. The caller holds s.mu.
func (s *Server) receivedFor(r receipt, sends []send) []send {
	var result jsontext.Value
	for _, sig := range r.sigs {
		for _, x := range s.watchers[sig] {
			if x.received && !x.told {
				x.told = true
				if result == nil {
					result = s.encode(r.height, notifyAt(r.height, "receivedSignature"))
				}
				if result != nil {
					sends = append(sends, send{x.conn, x.notify(result)})
				}
			}
		}
	}
	return sends
}

// unregister removes a subscription the connection holds. The caller holds
// s.mu. The lookup is in the connection's own map, so an id that belongs to
// another connection is not found.
func (s *Server) unregister(c *wsConn, id uint64) bool {
	x, ok := c.subs[id]
	if !ok {
		return false
	}
	delete(c.subs, id)
	if sh := s.shared[x.params]; sh != nil {
		if sh.holders--; sh.holders == 0 {
			delete(s.shared, x.params)
		}
	}
	switch x.method {
	case accountNotification:
		s.accounts[x.key] = without(s.accounts[x.key], x)
		if len(s.accounts[x.key]) == 0 {
			delete(s.accounts, x.key)
		}
		s.app.Feed.Release(x.key, false)
	case programNotification:
		s.programs[x.key] = without(s.programs[x.key], x)
		if len(s.programs[x.key]) == 0 {
			delete(s.programs, x.key)
		}
		s.app.Feed.Release(x.key, true)
	case logsNotification:
		s.logs = without(s.logs, x)
	case slotNotification:
		s.slots = without(s.slots, x)
	case slotsUpdates:
		s.updates = without(s.updates, x)
	case rootNotification:
		s.roots = without(s.roots, x)
	case signatureNotification:
		s.watchers[x.sig] = without(s.watchers[x.sig], x)
		if len(s.watchers[x.sig]) == 0 {
			delete(s.watchers, x.sig)
		}
		if x.received {
			if s.receivers--; s.receivers == 0 {
				s.app.Feed.OnReceived(nil)
			}
		}
	}
	return true
}

// added appends x to an index list and records its index.
func added(list []*sub, x *sub) []*sub {
	x.at = len(list)
	return append(list, x)
}

// without removes x from its index list. It moves the last subscription of
// the list into the place of x. A list can hold a million subscriptions, and
// a disconnect removes each subscription of the connection. A copy of the list
// for each removal would hold the registry lock for minutes. The list changes
// in place, so a caller that removes while it ranges over a list ranges over
// a copy.
func without(list []*sub, x *sub) []*sub {
	last := len(list) - 1
	list[x.at] = list[last]
	list[x.at].at = x.at
	list[last] = nil
	return list[:last]
}

// note is one notification, kept as its parts until its connection has room
// for it. The result is encoded once and shared by every subscription that it
// goes to. Only the id differs between them.
type note struct {
	method string
	id     uint64
	result jsontext.Value
}

// notify returns the subscription's notification of an encoded result.
func (x *sub) notify(result jsontext.Value) note {
	return note{method: x.method, id: x.id, result: result}
}

// The parts of a message around its method, result and id. Every message has
// Agave's Notification shape: jsonrpc, the method, and params with the result
// and the subscription id. The keys are in the order that json.Marshal gives a
// map, so a message is byte for byte the one json.Marshal makes.
const (
	noteMethod = `{"jsonrpc":"2.0","method":"`
	noteResult = `","params":{"result":`
	noteID     = `,"subscription":`
	noteEnd    = `}}`
)

// size returns the length of the message that bytes builds.
func (n note) size() int {
	var id [20]byte
	return len(noteMethod) + len(n.method) + len(noteResult) + len(n.result) + len(noteID) +
		len(strconv.AppendUint(id[:0], n.id, 10)) + len(noteEnd)
}

// bytes builds the message.
func (n note) bytes() []byte {
	b := make([]byte, 0, n.size())
	b = append(b, noteMethod...)
	b = append(b, n.method...)
	b = append(b, noteResult...)
	b = append(b, n.result...)
	b = append(b, noteID...)
	b = strconv.AppendUint(b, n.id, 10)
	return append(b, noteEnd...)
}

// encode returns a result as the bytes that its notifications share. If a
// result does not encode, encode logs the error and returns nil, and nobody
// gets that notification.
func (s *Server) encode(height uint64, result any) jsontext.Value {
	b, err := json.Marshal(result, json.Deterministic(true))
	if err != nil {
		s.logger.Error("svm notification", "height", height, "err", err)
		return nil
	}
	return b
}

// notifyAt wraps a value in a notification's context, which is the slot
// alone. Agave's notification context has no apiVersion.
func notifyAt(slot uint64, value any) map[string]any {
	return map[string]any{"context": map[string]uint64{"slot": slot}, "value": value}
}

// level is the commitment a subscription names, as its params key holds it.
// serde reads a level it does not know as none, which is finalized.
func level(commitment jsontext.Value) string {
	switch string(bytes.TrimSpace(commitment)) {
	case `"processed"`:
		return "processed"
	case `"confirmed"`:
		return "confirmed"
	}
	return "finalized"
}

// subscriptionKey parses a subscribe's key. A key it cannot parse is
// "Invalid Request: Invalid {thing} provided".
func subscriptionKey(raw string, size int, thing string) ([]byte, error) {
	b, fault := parseBase58(raw, size)
	if fault != "" {
		return nil, invalidParams("Invalid Request: Invalid %s provided", thing)
	}
	return b, nil
}

// subscribeConfig is the second parameter of every subscribe: the account
// rendering where one applies, plus a commitment.
//
// The three commitment levels collapse into one event. Every block here is
// final, so processed is never true while confirmed is not. svmd accepts a
// subscribe that names any of the three and fires it when the block commits.
// It does not echo the level, because no Agave notification payload has one.
type subscribeConfig struct {
	accountConfig
	contextConfig
	Filters     []accountFilter `json:"filters"`
	WithContext *bool           `json:"withContext"`
}

// accountSubscribe is Agave's account_subscribe. It reads the config, then the
// key, with binary as the default encoding. A notification ignores dataSlice. It
// carries Agave's text in place of base58 data over 128 bytes.
func accountSubscribe(s *Server, c *wsConn, params []jsontext.Value) (any, error) {
	var raw string
	if err := param(params, 0, &raw); err != nil {
		return nil, err
	}
	var cfg subscribeConfig
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	account, err := cfg.accountConfig.or("binary")
	if err != nil {
		return nil, err
	}
	key, err := subscriptionKey(raw, 32, "pubkey")
	if err != nil {
		return nil, err
	}
	pk := [32]byte(key)
	identity := fmt.Sprintf("account|%x|%s|%s|%v", pk, account.Encoding, level(cfg.Commitment), sliceOf(account))
	account.DataSlice, account.notification = nil, true
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admit(c); err != nil {
		return nil, err
	}
	return s.register(&sub{conn: c, method: accountNotification, key: pk, cfg: account, params: identity}), nil
}

func sliceOf(c accountConfig) string {
	if c.DataSlice == nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", c.DataSlice.Offset, c.DataSlice.Length)
}

// programSubscribe is Agave's program_subscribe. It checks the filters first,
// refused with the error's display, then the key. A subscribe with no filter
// is allowed, as on Agave. The connection's queue is the bound.
func programSubscribe(s *Server, c *wsConn, params []jsontext.Value) (any, error) {
	var raw string
	if err := param(params, 0, &raw); err != nil {
		return nil, err
	}
	var cfg subscribeConfig
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	account, err := cfg.accountConfig.or("binary")
	if err != nil {
		return nil, err
	}
	filters, err := matchers(cfg.Filters)
	if err != nil {
		// jsonrpc-core's Error Display, "Invalid params: " and the message.
		return nil, invalidParams("Invalid params: %s", asRPCError(err).Message)
	}
	key, err := subscriptionKey(raw, 32, "pubkey")
	if err != nil {
		return nil, err
	}
	pk := [32]byte(key)
	identity := fmt.Sprintf("program|%x|%v|%s|%s|%s|%v", pk, filters, account.Encoding, sliceOf(account),
		level(cfg.Commitment), cfg.WithContext != nil && *cfg.WithContext)
	account.DataSlice, account.notification = nil, true
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admit(c); err != nil {
		return nil, err
	}
	return s.register(&sub{conn: c, method: programNotification, key: pk, cfg: account, filters: filters, params: identity}), nil
}

// logsSubscribe takes Agave's RpcTransactionLogsFilter: "all", "allWithVotes",
// or {"mentions": [one address]}. Cosmos consensus has no Solana votes, so
// svmd serves allWithVotes as all, under its own id.
func logsSubscribe(s *Server, c *wsConn, params []jsontext.Value) (any, error) {
	var mentions map[[32]byte]bool
	var word, kind string
	if err := json.Unmarshal(params[0], &word, agaveInput); err == nil {
		if word != "all" && word != "allWithVotes" {
			return nil, invalidParams("Invalid params: unknown variant `%s`, expected one of `all`, `allWithVotes`, `mentions`.", word)
		}
		kind = word
	} else {
		var object map[string]jsontext.Value
		if err := json.Unmarshal(params[0], &object, agaveInput); err != nil || len(object) != 1 || object["mentions"] == nil {
			return nil, invalidParams("Invalid params: unknown variant, expected one of `all`, `allWithVotes`, `mentions`.")
		}
		var keys []string
		if err := json.Unmarshal(object["mentions"], &keys, agaveInput); err != nil {
			return nil, invalidParams("Invalid params: %v.", reason(err))
		}
		if len(keys) != 1 {
			return nil, invalidParams("Invalid Request: Only 1 address supported")
		}
		k, err := subscriptionKey(keys[0], 32, "mentions")
		if err != nil {
			return nil, err
		}
		mentions = map[[32]byte]bool{[32]byte(k): true}
		kind = fmt.Sprintf("mentions:%x", k)
	}
	var cfg contextConfig
	if err := param(params, 1, &cfg); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admit(c); err != nil {
		return nil, err
	}
	return s.register(&sub{conn: c, method: logsNotification, mentions: mentions,
		params: "logs|" + kind + "|" + level(cfg.Commitment)}), nil
}

func slotSubscribe(s *Server, c *wsConn, _ []jsontext.Value) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admit(c); err != nil {
		return nil, err
	}
	return s.register(&sub{conn: c, method: slotNotification, params: "slot"}), nil
}

// slotsUpdatesSubscribe is Agave's slot lifecycle stream.
func slotsUpdatesSubscribe(s *Server, c *wsConn, _ []jsontext.Value) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admit(c); err != nil {
		return nil, err
	}
	return s.register(&sub{conn: c, method: slotsUpdates, params: "slotsUpdates"}), nil
}

// rootSubscribe sends one bare height per commit, ascending, with no context.
func rootSubscribe(s *Server, c *wsConn, _ []jsontext.Value) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admit(c); err != nil {
		return nil, err
	}
	return s.register(&sub{conn: c, method: rootNotification, params: "root"}), nil
}

// unsubscribe removes the subscription the client names. It refuses an id
// this connection does not hold with Agave's own error. That includes an id
// that belongs to another connection.
func unsubscribe(s *Server, c *wsConn, params []jsontext.Value) (any, error) {
	var id uint64
	if err := param(params, 0, &id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unregister(c, id) {
		return nil, invalidSubscription()
	}
	return true, nil
}

func invalidSubscription() error {
	return &rpcError{Code: codeInvalidParams, Message: "Invalid subscription id."}
}

// writtenAccount renders an account from the block write set.
//
// A write that zeroes an account's lamports deletes it. Agave then notifies
// the default account, not the bytes it last held: "If the account is not
// found ... we will notify clients that the account no longer exists". The
// default's owner is the all-zero key, which renders as the system program.
//
// A panic while rendering fails this one notification and is logged. The
// notifier runs on Follow's goroutine, where nothing above it recovers. An
// unrecovered panic there ends the whole node, validator included.
func writtenAccount(pubkey [32]byte, a ffi.Account, cfg accountConfig, m *mintReader) (ui *uiAccount, err error) {
	defer func() {
		if r := recover(); r != nil {
			ui, err = nil, fmt.Errorf("rendering %s panicked: %v", base58.Encode(pubkey[:]), r)
		}
	}()
	if a.Lamports == 0 {
		a = ffi.Account{}
	}
	return renderAccount(pubkey, a, cfg, m)
}

// fanOut appends the account and program notifications that a height's write
// set owes. The caller holds s.mu.
//
// It makes one pass over the writes, with a map lookup each. Agave's notifier
// makes one pass over the subscriptions, with a store lookup each. The shapes
// are opposite because the engine already hands Go the write set to commit it.
func (s *Server) fanOut(height uint64, writes []svm.AccountValue, sends []send) []send {
	if len(s.accounts) == 0 && len(s.programs) == 0 {
		return sends
	}
	// A parsed token account names its mint. svmd reads the mint from
	// committed state, as Agave reads it from the bank. The reader is built
	// once, and only if a subscriber asked for a parsed body.
	var committed *mintReader
	mints := func(cfg accountConfig) *mintReader {
		if cfg.Encoding != "jsonParsed" {
			return nil
		}
		if committed == nil {
			ms, _, err := s.snapshot()
			if err != nil {
				return nil
			}
			committed = s.mints(ms, cfg, false)
		}
		return committed
	}
	for _, w := range writes {
		watchers, watched := s.accounts[w.Pubkey]
		a, err := ffi.DecodeAccount(w.Value)
		if err != nil {
			s.logger.Error("svm account notification", "height", height, "pubkey", base58.Encode(w.Pubkey[:]), "err", err)
			continue
		}
		owners := s.programs[a.Owner]
		if !watched && len(owners) == 0 {
			continue
		}
		// A notification drops dataSlice, so only the encoding makes two
		// renderings of one write differ. Each rendering is made once and
		// shared. Made once per subscription, a thousand subscribers to one
		// large account would hold a thousand copies of it before any queue
		// bound applied. Each result is also encoded once, for an account and
		// for a program.
		rendered := map[string]*uiAccount{}
		render := func(cfg accountConfig) *uiAccount {
			ui, done := rendered[cfg.Encoding]
			if !done {
				var err error
				if ui, err = writtenAccount(w.Pubkey, a, cfg, mints(cfg)); err != nil {
					s.logger.Error("svm account notification", "height", height, "err", err)
				}
				rendered[cfg.Encoding] = ui
			}
			return ui
		}
		type shape struct {
			encoding string
			keyed    bool
		}
		results := map[shape]jsontext.Value{}
		result := func(cfg accountConfig, keyed bool) jsontext.Value {
			k := shape{cfg.Encoding, keyed}
			b, done := results[k]
			if !done {
				if ui := render(cfg); ui != nil {
					var value any = ui
					if keyed {
						value = map[string]any{"pubkey": base58.Encode(w.Pubkey[:]), "account": ui}
					}
					b = s.encode(height, notifyAt(height, value))
				}
				results[k] = b
			}
			return b
		}
		for _, x := range watchers {
			if b := result(x.cfg, false); b != nil {
				sends = append(sends, send{x.conn, x.notify(b)})
			}
		}
		for _, x := range owners {
			if !matchAll(x.filters, a.Data) {
				continue
			}
			ui := render(x.cfg)
			if ui == nil {
				continue
			}
			// A token program's parsed stream drops what does not parse, as
			// get_parsed_token_accounts does.
			if x.cfg.Encoding == "jsonParsed" && svm.IsTokenProgram(x.key) && !parsedBody(ui) {
				continue
			}
			if b := result(x.cfg, true); b != nil {
				sends = append(sends, send{x.conn, x.notify(b)})
			}
		}
	}
	return sends
}

func matchAll(filters []matcher, data []byte) bool {
	for _, f := range filters {
		if !f.match(data) {
			return false
		}
	}
	return true
}

// logsFor appends the log notifications a record owes. The caller holds s.mu.
// The resolved address set costs one engine parse. It is paid once per
// record, and only if a mentions filter is live.
//
// Only a transaction that executed and recorded logs is sent. Agave's log
// collector holds nothing for one that failed before it ran.
func (s *Server) logsFor(height uint64, r svm.Record, sends []send) []send {
	if len(s.logs) == 0 || len(r.Meta.Logs) == 0 {
		return sends
	}
	var named map[[32]byte]bool
	var result jsontext.Value
	value := map[string]any{
		"signature": base58.Encode(r.Signature[:]),
		"err":       statusOf(r).Err,
		"logs":      nonNilStrings(r.Meta.Logs),
	}
	for _, x := range s.logs {
		if x.mentions != nil {
			if named == nil {
				addresses, err := r.Addresses()
				if err != nil {
					s.logger.Error("svm log notification", "height", height, "err", err)
					return sends
				}
				named = make(map[[32]byte]bool, len(addresses))
				for _, a := range addresses {
					named[a] = true
				}
			}
			if !mentioned(x.mentions, named) {
				continue
			}
		}
		if result == nil {
			if result = s.encode(height, notifyAt(height, value)); result == nil {
				return sends
			}
		}
		sends = append(sends, send{x.conn, x.notify(result)})
	}
	return sends
}

func mentioned(wanted, named map[[32]byte]bool) bool {
	for k := range wanted {
		if named[k] {
			return true
		}
	}
	return false
}

// updatesFor appends the four slot updates of a commit under instant
// finality. The block is complete, its bank is created on its parent, and it
// is confirmed and rooted at once. svmd never sends firstShredReceived,
// because there are no shreds. It never sends frozen, whose stats count PoH
// entries, or dead, because there are no forks. The timestamp is this
// machine's clock in milliseconds, as on Agave.
func (s *Server) updatesFor(height uint64, sends []send) []send {
	if len(s.updates) == 0 {
		return sends
	}
	now := uint64(time.Now().UnixMilli())
	events := []map[string]any{
		{"type": "completed", "slot": height, "timestamp": now},
		{"type": "createdBank", "slot": height, "parent": height - 1, "timestamp": now},
		{"type": "optimisticConfirmation", "slot": height, "timestamp": now},
		{"type": "root", "slot": height, "timestamp": now},
	}
	results := make([]jsontext.Value, 0, len(events))
	for _, e := range events {
		if b := s.encode(height, e); b != nil {
			results = append(results, b)
		}
	}
	for _, x := range s.updates {
		for _, b := range results {
			sends = append(sends, send{x.conn, x.notify(b)})
		}
	}
	return sends
}

// slotsFor appends the slot notification and the root notification. The slot
// notification is SlotInfo with no context wrapper. The root notification is
// the bare height. root is the height because every block here is final.
func (s *Server) slotsFor(height uint64, sends []send) []send {
	if len(s.slots) > 0 {
		if b := s.encode(height, map[string]any{"slot": height, "parent": height - 1, "root": height}); b != nil {
			for _, x := range s.slots {
				sends = append(sends, send{x.conn, x.notify(b)})
			}
		}
	}
	if len(s.roots) > 0 {
		if b := s.encode(height, height); b != nil {
			for _, x := range s.roots {
				sends = append(sends, send{x.conn, x.notify(b)})
			}
		}
	}
	return sends
}
