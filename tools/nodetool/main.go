// Command nodetool sets up and watches the svmd machines that the benchmark
// scenarios (bench/scenarios) and the acceptance harnesses (rpc/accept) run.
//
//	nodetool validators -out FILE -pubkey JSON (-genesis FILE | -account ADDR) [-bond N]
//	nodetool fund -genesis FILE [-payers FILE] [-account FILE] [-lamports N] [KEY...]
//	nodetool preload -genesis FILE [-program ID=SO]... [-account ID=FILE]...
//	nodetool set FILE KEY=VALUE...
//	nodetool sample -cgroup DIR -pid PID -phase FILE
//	nodetool height
//	nodetool pending
//	nodetool proven -path PATH -data HEX -height H
//	nodetool blocks < blockchain.json
//
// nodetool reaches svmd through CometBFT's RPC on 127.0.0.1:26657 and its
// Prometheus endpoint on 127.0.0.1:26660. A flag sets each address. A command
// that cannot answer exits 1.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 5 * time.Second}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "nodetool validators|fund|preload|set|sample|height|pending|proven|blocks|free-port")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	comet := fs.String("comet", "http://127.0.0.1:26657", "CometBFT RPC")
	var err error
	switch os.Args[1] {
	case "validators":
		out := fs.String("out", "", "the validators file to add the operator to")
		pubkey := fs.String("pubkey", "", "the output of svmd comet show-validator")
		genesis := fs.String("genesis", "", "genesis.json of the operator's first init, which names its account")
		account := fs.String("account", "", "the operator's account, in place of -genesis")
		bond := fs.String("bond", "10000000", "the operator's self bond")
		fs.Parse(os.Args[2:])
		err = validators(*out, *pubkey, *genesis, *account, *bond)
	case "fund":
		genesis := fs.String("genesis", "", "genesis.json to add the accounts to")
		payers := fs.String("payers", "", "one base58 public key a line")
		account := fs.String("account", "", "a whole account row, as JSON, to add as it is")
		lamports := fs.Uint64("lamports", 1_000_000_000_000, "lamports each payer gets")
		fs.Parse(os.Args[2:])
		err = fund(*genesis, *payers, *account, *lamports, fs.Args())
	case "preload":
		genesis := fs.String("genesis", "", "genesis.json to add the programs and accounts to")
		var programs, accounts list
		fs.Var(&programs, "program", "ID=FILE.so: a program at ID; repeatable")
		fs.Var(&accounts, "account", "ID=FILE: an account at ID, from `solana account --output json`; repeatable")
		replace := fs.Bool("replace", false, "a -program at an address genesis already seeds replaces it, and drops the genesis accounts it owns")
		fs.Parse(os.Args[2:])
		err = preload(*genesis, programs, accounts, *replace, os.Stderr)
	case "set":
		fs.Parse(os.Args[2:])
		if fs.NArg() < 2 {
			err = errors.New("set needs FILE and at least one KEY=VALUE")
			break
		}
		err = setFile(fs.Arg(0), fs.Args()[1:])
	case "sample":
		var s sources
		pid := fs.Int("pid", 0, "the svmd process")
		fs.StringVar(&s.cgroup, "cgroup", "", "the cgroup directory svmd runs in")
		fs.StringVar(&s.phase, "phase", "", "a file holding the phase name")
		fs.StringVar(&s.metrics, "metrics", "http://127.0.0.1:26660/metrics", "Prometheus endpoint")
		fs.Parse(os.Args[2:])
		s.smaps = fmt.Sprintf("/proc/%d/smaps_rollup", *pid)
		s.status = *comet + "/status"
		sample(s, os.Stdout)
	case "height":
		fs.Parse(os.Args[2:])
		var h string
		if h, err = height(*comet + "/status"); err == nil {
			fmt.Println(h)
		}
	case "pending":
		fs.Parse(os.Args[2:])
		var r struct {
			Result struct {
				N string `json:"n_txs"`
			} `json:"result"`
		}
		if err = getJSON(*comet+"/num_unconfirmed_txs", &r); err == nil {
			fmt.Println(r.Result.N)
		}
	case "proven":
		path := fs.String("path", "", "the ABCI query path, such as /store/bank/key")
		data := fs.String("data", "", "the query data, 0x and hex")
		h := fs.Int64("height", 0, "the height to prove at")
		fs.Parse(os.Args[2:])
		err = proven(*comet, *path, *data, *h)
	case "blocks":
		fs.Parse(os.Args[2:])
		err = blocks(os.Stdin, os.Stdout)
	case "free-port":
		udp := fs.Bool("udp", false, "a UDP port, in place of TCP")
		fs.Parse(os.Args[2:])
		err = freePort(*udp)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodetool:", err)
		os.Exit(1)
	}
}

// freePort prints a loopback port that the kernel has just handed out, so it
// was free a moment ago. Another process can still take it before the caller
// binds it.
func freePort(udp bool) error {
	var addr net.Addr
	if udp {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer c.Close()
		addr = c.LocalAddr()
	} else {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer l.Close()
		addr = l.Addr()
	}
	_, port, _ := net.SplitHostPort(addr.String())
	fmt.Println(port)
	return nil
}

// validators adds one operator to the file that `svmd init --validators-file`
// reads. It creates the file for the first operator, so a chain of N
// operators takes N calls. The account is the one that the operator's first
// init funded, read from that init's genesis, unless -account names another.
func validators(out, pubkey, genesis, account, bond string) error {
	if !jsontext.Value([]byte(pubkey)).IsValid() {
		return fmt.Errorf("-pubkey is not JSON: %q", pubkey)
	}
	if account == "" {
		if genesis == "" {
			return errors.New("name the operator's account with -genesis or -account")
		}
		var g struct {
			AppState struct {
				Staking struct {
					Delegations []struct {
						Delegator string `json:"delegator_address"`
					} `json:"delegations"`
				} `json:"staking"`
			} `json:"app_state"`
		}
		if err := readJSON(genesis, &g); err != nil {
			return err
		}
		if len(g.AppState.Staking.Delegations) == 0 {
			return fmt.Errorf("%s has no delegation", genesis)
		}
		account = g.AppState.Staking.Delegations[0].Delegator
	}
	type operator struct {
		Pubkey   jsontext.Value `json:"consensus_pubkey"`
		Account  string         `json:"account"`
		SelfBond string         `json:"self_bond"`
		Free     string         `json:"free"`
		Moniker  string         `json:"moniker"`
	}
	var file struct {
		Validators []operator `json:"validators"`
	}
	if err := readJSON(out, &file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file.Validators = append(file.Validators, operator{
		Pubkey:   jsontext.Value(pubkey),
		Account:  account,
		SelfBond: bond,
		Free:     "90000000",
		Moniker:  fmt.Sprintf("operator-%d", len(file.Validators)),
	})
	b, err := json.Marshal(file, json.Deterministic(true), jsontext.WithIndent(" "))
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(b, '\n'), 0o644)
}

// genesisAccount is one entry of app_state.svm.accounts.
type genesisAccount struct {
	Pubkey     string `json:"pubkey"`
	Lamports   uint64 `json:"lamports"`
	Owner      string `json:"owner"`
	Data       string `json:"data"`
	Executable bool   `json:"executable"`
	RentEpoch  uint64 `json:"rent_epoch"`
}

// fund adds a system account for every key, with the keys in the payers file
// first. It then adds the row in the account file unchanged.
func fund(genesis, payers, account string, lamports uint64, keys []string) error {
	if payers != "" {
		b, err := os.ReadFile(payers)
		if err != nil {
			return err
		}
		keys = append(strings.Fields(string(b)), keys...)
	}
	var rows []jsontext.Value
	for _, k := range keys {
		a, err := marshal(genesisAccount{k, lamports, "11111111111111111111111111111111", "", false, ^uint64(0)})
		if err != nil {
			return err
		}
		rows = append(rows, a)
	}
	if account != "" {
		b, err := os.ReadFile(account)
		if err != nil {
			return err
		}
		if !jsontext.Value(b).IsValid() {
			return fmt.Errorf("%s is not JSON", account)
		}
		rows = append(rows, bytes.TrimSpace(b))
	}
	if len(rows) == 0 {
		return errors.New("no account to add: name -payers, -account or keys")
	}
	b, err := os.ReadFile(genesis)
	if err != nil {
		return err
	}
	b, err = at(b, []string{"app_state", "svm", "accounts"}, func(old jsontext.Value) (jsontext.Value, error) {
		var accounts []jsontext.Value
		if err := json.Unmarshal(old, &accounts); err != nil {
			return nil, err
		}
		return marshal(append(accounts, rows...))
	})
	if err != nil {
		return fmt.Errorf("%s: %w", genesis, err)
	}
	return os.WriteFile(genesis, b, 0o644)
}

// list is a flag that may be given more than once.
type list []string

func (l *list) String() string     { return strings.Join(*l, " ") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// preload adds what the --bpf-program and --account flags of
// solana-test-validator load, so an acceptance run gives this chain the
// same programs and accounts as Agave (rpc/accept/shim).
//
// A program goes in under loader v3 with no upgrade authority, as every
// genesis program does. If the genesis already seeds a program at its ID,
// that program stays, because the run checks this chain's own program. With
// replace, the new program replaces the seeded one, and preload drops the
// genesis accounts that the seeded program owns. A suite's own build of
// a program that this chain carries starts from the same clean state as on
// Agave.
//
// An account goes in at ID, not at the pubkey its file names, as
// solana-test-validator does. If the genesis already holds an account there,
// that account stays for the same reason. The run checks the fees vaults that
// `svmd init --ephemeral-validator` writes.
func preload(genesis string, programs, accounts []string, replace bool, log io.Writer) error {
	b, err := os.ReadFile(genesis)
	if err != nil {
		return err
	}
	b, err = at(b, []string{"app_state", "svm"}, func(svm jsontext.Value) (jsontext.Value, error) {
		var s map[string]jsontext.Value
		if err := json.Unmarshal(svm, &s); err != nil {
			return nil, err
		}
		var progs, accts []jsontext.Value
		if err := json.Unmarshal(s["programs"], &progs); err != nil {
			return nil, fmt.Errorf("programs: %w", err)
		}
		if err := json.Unmarshal(s["accounts"], &accts); err != nil {
			return nil, fmt.Errorf("accounts: %w", err)
		}
		for _, p := range programs {
			id, file, ok := strings.Cut(p, "=")
			if !ok {
				return nil, fmt.Errorf("-program %q is not ID=FILE", p)
			}
			if i := index(progs, "address", id); i >= 0 {
				if !replace {
					fmt.Fprintf(log, "preload: keeping this chain's own program at %s\n", id)
					continue
				}
				progs = append(progs[:i], progs[i+1:]...)
				owned := 0
				accts = slices.DeleteFunc(accts, func(r jsontext.Value) bool {
					var m map[string]jsontext.Value
					if json.Unmarshal(r, &m) == nil && string(m["owner"]) == strconv.Quote(id) {
						owned++
						return true
					}
					return false
				})
				fmt.Fprintf(log, "preload: replacing this chain's own program at %s, dropping %d accounts it owns\n", id, owned)
			}
			elf, err := os.ReadFile(file)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(elf)
			row, err := marshal(struct {
				Address string `json:"address"`
				Loader  string `json:"loader"`
				SHA256  string `json:"sha256"`
				ELF     []byte `json:"elf"`
			}{id, "BPFLoaderUpgradeab1e11111111111111111111111", hex.EncodeToString(sum[:]), elf})
			if err != nil {
				return nil, err
			}
			progs = append(progs, row)
		}
		for _, a := range accounts {
			id, file, ok := strings.Cut(a, "=")
			if !ok {
				return nil, fmt.Errorf("-account %q is not ID=FILE", a)
			}
			var dump struct {
				Account struct {
					Lamports   uint64   `json:"lamports"`
					Data       []string `json:"data"`
					Owner      string   `json:"owner"`
					Executable bool     `json:"executable"`
					RentEpoch  uint64   `json:"rentEpoch"`
				} `json:"account"`
			}
			if err := readJSON(file, &dump); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			d := dump.Account
			if len(d.Data) != 2 || d.Data[1] != "base64" {
				return nil, fmt.Errorf("%s: data is not [BYTES, \"base64\"]", file)
			}
			if _, err := base64.StdEncoding.DecodeString(d.Data[0]); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			if index(accts, "pubkey", id) >= 0 {
				fmt.Fprintf(log, "preload: keeping this chain's own account at %s\n", id)
				continue
			}
			row, err := marshal(genesisAccount{id, d.Lamports, d.Owner, d.Data[0], d.Executable, d.RentEpoch})
			if err != nil {
				return nil, err
			}
			accts = append(accts, row)
		}
		if s["programs"], err = marshal(progs); err != nil {
			return nil, err
		}
		if s["accounts"], err = marshal(accts); err != nil {
			return nil, err
		}
		return marshal(s)
	})
	if err != nil {
		return fmt.Errorf("%s: %w", genesis, err)
	}
	return os.WriteFile(genesis, b, 0o644)
}

// index returns the position of the first row whose field is value, or -1.
func index(rows []jsontext.Value, field, value string) int {
	for i, r := range rows {
		var m map[string]jsontext.Value
		if json.Unmarshal(r, &m) == nil && string(m[field]) == strconv.Quote(value) {
			return i
		}
	}
	return -1
}

// at rewrites the value at a path of object keys with f. It decodes only the
// path, so every other value keeps the bytes that init wrote. A key that the
// document does not have is an error.
func at(doc jsontext.Value, path []string, f func(jsontext.Value) (jsontext.Value, error)) (jsontext.Value, error) {
	if len(path) == 0 {
		return f(doc)
	}
	var m map[string]jsontext.Value
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	child, ok := m[path[0]]
	if !ok {
		return nil, fmt.Errorf("no %q", path[0])
	}
	v, err := at(child, path[1:], f)
	if err != nil {
		return nil, err
	}
	m[path[0]] = v
	return marshal(m)
}

// marshal writes v as init wrote it.
func marshal(v any) (jsontext.Value, error) { return json.Marshal(v, json.Deterministic(true)) }

// setFile rewrites one value of a file for each KEY=VALUE pair. It writes the
// value as given, so a string carries its own quotes. In a .json file, KEY is
// a dotted path of object keys. In a TOML file, KEY rewrites the first line
// for that key anywhere in the file, and SECTION.KEY rewrites the first line
// for that key under the [SECTION] header. A key that the file does not have
// is an error, not a setting that is left out.
func setFile(path string, pairs []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	edit := set
	if strings.HasSuffix(path, ".json") {
		edit = setJSON
	}
	s := string(b)
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return fmt.Errorf("%q is not KEY=VALUE", p)
		}
		if s, err = edit(s, k, v); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

func set(s, key, value string) (string, error) {
	lo, hi := 0, len(s)
	if i := strings.LastIndex(key, "."); i >= 0 {
		section := "[" + key[:i] + "]"
		key = key[i+1:]
		h := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(section) + `$`).FindStringIndex(s)
		if h == nil {
			return "", fmt.Errorf("no %s section", section)
		}
		lo = h[1]
		if next := regexp.MustCompile(`(?m)^\[`).FindStringIndex(s[lo:]); next != nil {
			hi = lo + next[0]
		}
	}
	loc := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + ` = .*$`).FindStringIndex(s[lo:hi])
	if loc == nil {
		return "", fmt.Errorf("no %q line", key)
	}
	return s[:lo+loc[0]] + key + " = " + value + s[lo+loc[1]:], nil
}

func setJSON(s, path, value string) (string, error) {
	if !jsontext.Value([]byte(value)).IsValid() {
		return "", fmt.Errorf("%s: %q is not JSON", path, value)
	}
	out, err := at(jsontext.Value(s), strings.Split(path, "."), func(jsontext.Value) (jsontext.Value, error) {
		return jsontext.Value(value), nil
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return string(out), nil
}

// sources names where one sample line comes from.
type sources struct {
	cgroup, smaps, phase, metrics, status string
}

// sample writes one line each second until it is killed. It skips a second
// in which svmd cannot answer, so every line carries every field.
func sample(s sources, w io.Writer) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for now := time.Now(); ; now = <-t.C {
		if line, err := s.line(now); err == nil {
			fmt.Fprintln(w, line)
		}
	}
}

// line returns one sample. The cgroup's anon and file are what the cap
// charges, and smaps counts the process alone. Go's Sys minus HeapReleased is
// what the Go runtime holds. The process's anonymous memory minus that value
// is a lower bound on what cgo (the engine) allocated. The anonymous memory
// minus only the live heap and stacks is an upper bound. Sys counts address
// space that Go reserved and never touched, so it gives a bound, not the
// exact figure.
func (s sources) line(now time.Time) (string, error) {
	phase := "idle"
	if b, err := os.ReadFile(s.phase); err == nil {
		phase = strings.TrimSpace(string(b))
	}
	stat, err := fields(s.cgroup+"/memory.stat", 1)
	if err != nil {
		return "", err
	}
	sm, err := fields(s.smaps, 1024)
	if err != nil {
		return "", err
	}
	g, err := goMemstats(s.metrics)
	if err != nil {
		return "", err
	}
	h, err := height(s.status)
	if err != nil {
		return "", err
	}
	cur, err := os.ReadFile(s.cgroup + "/memory.current")
	if err != nil {
		return "", err
	}
	current, err := strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64)
	if err != nil {
		return "", err
	}
	need := func(m map[string]float64, keys ...string) error {
		for _, k := range keys {
			if _, ok := m[k]; !ok {
				return fmt.Errorf("no %s", k)
			}
		}
		return nil
	}
	if err := errors.Join(need(stat, "anon", "file"), need(sm, "Rss", "Anonymous"),
		need(g, "sys_bytes", "heap_released_bytes", "heap_inuse_bytes", "stack_inuse_bytes")); err != nil {
		return "", err
	}
	gosys := g["sys_bytes"] - g["heap_released_bytes"]
	golive := g["heap_inuse_bytes"] + g["stack_inuse_bytes"]
	return fmt.Sprintf("t=%d phase=%s height=%s cg_current_mb=%d cg_anon_mb=%d cg_file_mb=%d cg_kernel_mb=%d "+
		"rss_mb=%d rss_anon_mb=%d rss_file_mb=%d go_heap_inuse_mb=%d go_stack_mb=%d go_held_mb=%d "+
		"cgo_lo_mb=%d cgo_hi_mb=%d",
		now.Unix(), phase, h, current>>20, mb(stat["anon"]), mb(stat["file"]), mb(stat["kernel"]),
		mb(sm["Rss"]), mb(sm["Anonymous"]), mb(sm["Rss"]-sm["Anonymous"]),
		mb(g["heap_inuse_bytes"]), mb(g["stack_inuse_bytes"]), mb(gosys),
		mb(sm["Anonymous"]-gosys), mb(sm["Anonymous"]-golive)), nil
}

// mb truncates to whole bytes and then floors to MiB. The shift floors a
// negative bound too, so -1 byte reads as -1.
func mb(b float64) int64 { return int64(b) >> 20 }

// fields reads `NAME VALUE` or `NAME: VALUE kB` lines, each value times scale.
func fields(path string, scale float64) (map[string]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(f[0], ":")] = v * scale
	}
	return out, nil
}

// goMemstats returns the go_memstats_ family of a Prometheus endpoint, keyed
// by the rest of each name.
func goMemstats(endpoint string) (map[string]float64, error) {
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", endpoint, resp.Status)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), " ")
		if !ok || !strings.HasPrefix(name, "go_memstats_") {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			out[strings.TrimPrefix(name, "go_memstats_")] = v
		}
	}
	return out, sc.Err()
}

func height(status string) (string, error) {
	var r struct {
		Result struct {
			SyncInfo struct {
				Height string `json:"latest_block_height"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	if err := getJSON(status, &r); err != nil {
		return "", err
	}
	if r.Result.SyncInfo.Height == "" {
		return "", errors.New("no latest_block_height")
	}
	return r.Result.SyncInfo.Height, nil
}

// proven succeeds when svmd answers the query at height with a proof.
func proven(comet, path, data string, height int64) error {
	q := url.Values{}
	q.Set("path", strconv.Quote(path))
	q.Set("data", data)
	q.Set("height", strconv.FormatInt(height, 10))
	q.Set("prove", "true")
	var r struct {
		Result struct {
			Response struct {
				Code     uint32         `json:"code"`
				Log      string         `json:"log"`
				ProofOps map[string]any `json:"proofOps"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := getJSON(comet+"/abci_query?"+q.Encode(), &r); err != nil {
		return err
	}
	if res := r.Result.Response; res.Code != 0 || len(res.ProofOps) == 0 {
		return fmt.Errorf("height %d: code %d, %d proof fields: %s", height, res.Code, len(res.ProofOps), res.Log)
	}
	return nil
}

// blocks prints `height size transactions` for each block a /blockchain
// answer holds.
func blocks(r io.Reader, w io.Writer) error {
	var bc struct {
		Result struct {
			Metas []struct {
				Size   string `json:"block_size"`
				Txs    string `json:"num_txs"`
				Header struct {
					Height string `json:"height"`
				} `json:"header"`
			} `json:"block_metas"`
		} `json:"result"`
	}
	if err := json.UnmarshalRead(r, &bc); err != nil {
		return err
	}
	for _, m := range bc.Result.Metas {
		fmt.Fprintln(w, m.Header.Height, m.Size, m.Txs)
	}
	return nil
}

func getJSON(u string, v any) error {
	resp, err := client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.UnmarshalRead(resp.Body, v)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
