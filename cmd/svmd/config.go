package main

import (
	"fmt"
	"reflect"
	"time"

	serverconfig "github.com/cosmos/cosmos-sdk/server/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/rit3sh-x/cosmos-svm/rpc"
)

// SVMConfig is the [svm] section of app.toml. Each field names, defaults and
// describes one setting that svmd adds to the SDK's. defaultSVMConfig holds the
// defaults, and svmTemplate renders app.toml from them. addSVMFlags adds a start
// flag for each field from its tags, and readConfig reads the file and the
// flags. No setting is consensus. Two machines that differ in all of them reach
// the same app hash.
type SVMConfig struct {
	IndexMaxBytes     uint64 `mapstructure:"index-max-bytes" usage:"the history record's byte budget; required"`
	ProgramCacheBytes uint64 `mapstructure:"program-cache-bytes" usage:"the program cache's budget in resident bytes; 0 is 1.5 GiB"`
	Threads           int    `mapstructure:"threads" usage:"threads that compile and run a block; 0 is one per core"`

	RPCAddress        string        `mapstructure:"rpc-address" usage:"Solana JSON-RPC listen address"`
	WSAddress         string        `mapstructure:"ws-address" usage:"Solana websocket listen address"`
	RPCMaxSimulations int           `mapstructure:"rpc-max-simulations" usage:"simulateTransaction calls that load accounts at once"`
	RPCMaxScans       int           `mapstructure:"rpc-max-scans" usage:"full scans of committed state at once; 0 is one per four CPUs"`
	RPCThreads        int           `mapstructure:"rpc-threads" usage:"HTTP requests answered at once, each until its answer is written; 0 is one per CPU, Agave's default"`
	RPCWriteTimeout   time.Duration `mapstructure:"rpc-write-timeout" usage:"how long one answer, a batch included, may spend writing before its client is cut"`
	RPCScanLimitMB    int           `mapstructure:"rpc-scan-results-limit-mb" usage:"MiB of accounts one scan may keep before it stops with Agave's -32012; 0 is no limit"`
	RPCCORSOrigins    string        `mapstructure:"rpc-cors-origins" usage:"origins a browser may call both ports from: *, or a comma-separated list of scheme://host[:port]"`

	WSMaxConnections   int `mapstructure:"ws-max-connections" usage:"concurrent websocket connections"`
	WSMaxSubscriptions int `mapstructure:"ws-max-subscriptions-per-connection" usage:"subscriptions one websocket connection may hold"`
	WSMaxQueueBytes    int `mapstructure:"ws-max-queue-bytes" usage:"undelivered notification bytes one websocket connection may hold"`

	TPUQuicAddress   string `mapstructure:"tpu-quic-address" usage:"QUIC transaction port HOST:PORT; empty is off"`
	PublicTPUAddress string `mapstructure:"public-tpu-address" usage:"QUIC address getClusterNodes advertises, when not the bind address"`
	TPUQuicBinary    string `mapstructure:"tpu-quic-binary" usage:"the svm-tpu binary, when not beside svmd"`
	TPUPerPeer       int    `mapstructure:"tpu-max-connections-per-unstaked-peer" usage:"QUIC connections from one unstaked IP"`
	TPUPerIPMinute   int    `mapstructure:"tpu-max-connections-per-ipaddr-per-minute" usage:"new QUIC connections from one IP per minute"`
	TPUMaxUnstaked   int    `mapstructure:"tpu-max-unstaked-connections" usage:"open unstaked QUIC connections"`

	// The faucet's settings, in a devnet build only.
	FaucetConfig `mapstructure:",squash"`
}

func defaultSVMConfig() SVMConfig {
	return SVMConfig{
		RPCAddress:         "127.0.0.1:8899",
		WSAddress:          "127.0.0.1:8900",
		RPCMaxSimulations:  1,
		RPCWriteTimeout:    rpc.DefaultWriteTimeout,
		RPCCORSOrigins:     "*",
		WSMaxConnections:   rpc.DefaultWSMaxConnections,
		WSMaxSubscriptions: rpc.DefaultWSMaxSubscriptions,
		WSMaxQueueBytes:    rpc.DefaultWSMaxQueueBytes,
		TPUPerPeer:         rpc.DefaultTPUConnectionsPerPeer,
		TPUPerIPMinute:     rpc.DefaultTPUConnectionsPerIPMinute,
		TPUMaxUnstaked:     rpc.DefaultTPUMaxUnstakedConnections,
	}
}

// appConfig holds the SDK sections and [svm]. svmd renders app.toml from it.
type appConfig struct {
	serverconfig.Config `mapstructure:",squash"`
	SVM                 SVMConfig `mapstructure:"svm"`
}

// readConfig returns [svm] as the operator set it. A start flag overrides
// app.toml, and the default fills each setting that neither names.
func readConfig(v *viper.Viper) (SVMConfig, error) {
	c := struct {
		SVM SVMConfig `mapstructure:"svm"`
	}{defaultSVMConfig()}
	if err := v.Unmarshal(&c); err != nil {
		return SVMConfig{}, fmt.Errorf("reading [svm] in app.toml: %w", err)
	}
	return c.SVM, nil
}

// addSVMFlags adds a start flag svm.<key> for each [svm] setting, with the
// field's default and usage. It skips a field tagged flag:"-", which is a table
// that no flag can express.
func addSVMFlags(cmd *cobra.Command) {
	var add func(reflect.Value, reflect.Type)
	add = func(v reflect.Value, t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.Anonymous {
				add(v.Field(i), field.Type)
				continue
			}
			if field.Tag.Get("flag") == "-" {
				continue
			}
			name, usage := "svm."+field.Tag.Get("mapstructure"), field.Tag.Get("usage")
			switch x := v.Field(i).Interface().(type) {
			case string:
				cmd.Flags().String(name, x, usage)
			case int:
				cmd.Flags().Int(name, x, usage)
			case time.Duration:
				cmd.Flags().Duration(name, x, usage)
			case uint64:
				cmd.Flags().Uint64(name, x, usage)
			default:
				panic(fmt.Sprintf("svmd: [svm] %s has a type no flag takes", name))
			}
		}
	}
	d := defaultSVMConfig()
	add(reflect.ValueOf(d), reflect.TypeOf(d))
}

// svmTemplate is the [svm] section of app.toml. svmd renders each value from
// the config that it writes the file with.
const svmTemplate = `
[svm]

# The history record's byte budget. Required: the binary refuses to start at 0.
index-max-bytes = {{ .SVM.IndexMaxBytes }}

# The program cache's budget in resident bytes. 0 is the engine's default of
# 1.5 GiB, which holds the sampled mainnet working set with room inside the
# published 20 GB minimum. Raise it if the machine has the memory.
program-cache-bytes = {{ .SVM.ProgramCacheBytes }}

# How many threads compile a block's cold programs before it runs and run its
# conflict groups. 0 is one per core. Four is the published minimum.
threads = {{ .SVM.Threads }}

# The Solana JSON-RPC and websocket listen addresses. Loopback serves a
# validator's own tooling; an RPC full node binds where its clients reach it.
rpc-address = "{{ .SVM.RPCAddress }}"
ws-address = "{{ .SVM.WSAddress }}"

# How many simulateTransaction calls load accounts at once. Each holds up to
# 128 MiB, so this costs 128 MiB times its value; a call that finds every slot
# taken waits. One serves a validator's own tooling on loopback. An RPC full
# node raises it and owns the memory.
rpc-max-simulations = {{ .SVM.RPCMaxSimulations }}

# How many full scans of committed state run at once:
# getProgramAccounts, and the token and supply scans. Each holds a database
# snapshot and reads every account. 0 is Agave's default, one per four CPUs and
# never less than one; a scan that finds every slot taken waits. An RPC machine
# that serves the Explorer or an indexer raises it.
rpc-max-scans = {{ .SVM.RPCMaxScans }}

# How many HTTP requests are answered at once. A request holds its slot until
# its answer is written. A scan or a simulation takes its own slot inside it.
# 0 is the Agave default, one per CPU. A request that finds every slot taken
# waits.
rpc-threads = {{ .SVM.RPCThreads }}

# The total time that one answer may spend on writes. A batch is one answer.
# The time that its calls run or wait does not count. A client that reads
# more slowly is cut, and its slot is freed. At 30s, a 1 GB answer needs a
# client that reads about 35 MB/s.
rpc-write-timeout = "{{ .SVM.RPCWriteTimeout }}"

# How many MiB of accounts one scan may keep before it stops with the Agave
# error -32012 "scan aborted". This applies to getProgramAccounts and the
# token methods. Each account counts its data plus 96 bytes, as on Agave. 0 is
# no limit, which is the Agave default.
rpc-scan-results-limit-mb = {{ .SVM.RPCScanLimitMB }}

# Which origins a browser may call the Solana JSON-RPC and websocket from.
# "*" is Agave's answer: any page may call this machine. A comma-separated list
# of scheme://host[:port] answers only those pages, on both ports, and refuses
# any other with 403; "" refuses every page from another host. Clients that are
# not browsers send no Origin and are not affected.
rpc-cors-origins = "{{ .SVM.RPCCORSOrigins }}"

# The websocket's three bounds: connections at once, subscriptions on one
# connection, and the notification bytes one connection may hold undelivered.
# Their product is the machine's ceiling.
ws-max-connections = {{ .SVM.WSMaxConnections }}
ws-max-subscriptions-per-connection = {{ .SVM.WSMaxSubscriptions }}
ws-max-queue-bytes = {{ .SVM.WSMaxQueueBytes }}

# The QUIC port: solana program deploy without --use-rpc sends its writes here,
# to the address getClusterNodes gives. Empty is off, and svmd then starts
# without the svm-tpu binary. Turn it on only on a machine that serves the
# Solana JSON-RPC to clients, and open the UDP port to them.
tpu-quic-address = "{{ .SVM.TPUQuicAddress }}"
# The address getClusterNodes advertises, when the bind address is not the one
# clients reach. Required with an unspecified bind such as 0.0.0.0.
public-tpu-address = "{{ .SVM.PublicTPUAddress }}"
# The svm-tpu binary, when it is not beside svmd.
tpu-quic-binary = "{{ .SVM.TPUQuicBinary }}"
# Agave's bounds for an unstaked sender, with Agave's names and defaults.
# Raise the first two only for clients that share one address.
tpu-max-connections-per-unstaked-peer = {{ .SVM.TPUPerPeer }}
tpu-max-connections-per-ipaddr-per-minute = {{ .SVM.TPUPerIPMinute }}
tpu-max-unstaked-connections = {{ .SVM.TPUMaxUnstaked }}
` + faucetTemplate
