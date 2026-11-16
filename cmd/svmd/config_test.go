package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	serverconfig "github.com/cosmos/cosmos-sdk/server/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var writeExample = flag.Bool("write-example", false, "rewrite examples/app.toml, or examples/app.devnet.toml under -tags devnet")

// fields visits every [svm] setting, and in a devnet build the faucet's too.
func fields(v reflect.Value, visit func(key string, f reflect.Value, field reflect.StructField)) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Anonymous {
			fields(v.Field(i), visit)
			continue
		}
		visit(t.Field(i).Tag.Get("mapstructure"), v.Field(i), t.Field(i))
	}
}

// TestEverySettingIsInAppTomlAndReadBack proves that app.toml carries every
// [svm] setting, that readConfig reads each one back, and that each setting has
// a start flag with the default as its value. Each written value differs from
// its default, so a missing setting comes back as its default and fails.
func TestEverySettingIsInAppTomlAndReadBack(t *testing.T) {
	want := defaultSVMConfig()
	i := 0
	fields(reflect.ValueOf(&want).Elem(), func(key string, f reflect.Value, field reflect.StructField) {
		i++
		switch f.Kind() {
		case reflect.String:
			f.SetString("set-" + key)
		case reflect.Int:
			f.SetInt(int64(1000 + i))
		case reflect.Uint64:
			f.SetUint(uint64(2000 + i))
		case reflect.Int64:
			f.SetInt(int64(time.Duration(3000+i) * time.Second))
		}
	})
	path := filepath.Join(t.TempDir(), "app.toml")
	serverconfig.SetConfigTemplate(serverconfig.DefaultConfigTemplate + svmTemplate)
	serverconfig.WriteConfigFile(path, &appConfig{Config: *shippedConfig(), SVM: want})
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	got, err := readConfig(v)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("app.toml read back as\n%+v\nwritten from\n%+v", got, want)
	}

	cmd := &cobra.Command{}
	addSVMFlags(cmd)
	defaults := defaultSVMConfig()
	fields(reflect.ValueOf(defaults), func(key string, f reflect.Value, field reflect.StructField) {
		if field.Tag.Get("flag") == "-" {
			return
		}
		flag := cmd.Flags().Lookup("svm." + key)
		if flag == nil || field.Tag.Get("usage") == "" {
			t.Errorf("svm.%s has no start flag or no usage", key)
		} else if flag.DefValue != fmt.Sprint(f.Interface()) {
			t.Errorf("svm.%s defaults to %q, the config to %v", key, flag.DefValue, f.Interface())
		}
	})
}

// TestTheExampleAppTomlIsWhatInitWrites proves that the file in examples/ is
// the app.toml that `svmd init` writes. Each build has its own file, and the
// devnet file carries the faucet's settings.
func TestTheExampleAppTomlIsWhatInitWrites(t *testing.T) {
	name := "app.toml"
	if faucetTemplate != "" {
		name = "app.devnet.toml"
	}
	example := filepath.Join("..", "..", "examples", name)
	path := filepath.Join(t.TempDir(), "app.toml")
	serverconfig.SetConfigTemplate(serverconfig.DefaultConfigTemplate + svmTemplate)
	serverconfig.WriteConfigFile(path, &appConfig{Config: *shippedConfig(), SVM: defaultSVMConfig()})
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if *writeExample {
		if err := os.WriteFile(example, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("examples/%s is stale; rerun go test ./cmd/svmd -run TestTheExampleAppTomlIsWhatInitWrites -write-example, with -tags devnet for the devnet file", name)
	}
}
