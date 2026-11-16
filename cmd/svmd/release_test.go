package main

import (
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is two directories up from this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// reports runs `svmd version --long --output json` and parses the output.
func reports(t *testing.T, binary string) (map[string]any, map[string]string) {
	t.Helper()
	out, err := exec.Command(binary, "version", "--long", "--output", "json").Output()
	if err != nil {
		t.Fatalf("%s version: %v", binary, err)
	}
	var info struct {
		Version   string            `json:"version"`
		Commit    string            `json:"commit"`
		BuildTags string            `json:"build_tags"`
		Extra     map[string]string `json:"extra_info"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"version": info.Version, "commit": info.Commit, "build_tags": info.BuildTags}, info.Extra
}

// TestAReleaseProducesThreeBinariesAndThreeManifests runs the release script
// and checks the artifacts that it writes. Make runs the `release` target's
// prerequisites as a gate before the script, and this test covers only what
// the script writes after the gate.
func TestAReleaseProducesThreeBinariesAndThreeManifests(t *testing.T) {
	root := repoRoot(t)
	out := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "scripts", "release-build.sh"), out)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "VERSION=v9.9.9-test", "COMMIT=0123456789abcdef0123456789abcdef01234567")
	if body, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release-build.sh: %v\n%s", err, body)
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		// Like Agave, a release never ships two files with the same name and
		// different contents. The build flavour is in the basename.
		if e.Name() == "svmd" {
			t.Fatal("a release artifact is named svmd")
		}
	}
	want := []string{
		"svm-tpu-v9.9.9-test-linux-amd64",
		"svm-tpu-v9.9.9-test-linux-amd64.yml",
		"svmd-devnet-v9.9.9-test-linux-amd64",
		"svmd-devnet-v9.9.9-test-linux-amd64.yml",
		"svmd-v9.9.9-test-linux-amd64",
		"svmd-v9.9.9-test-linux-amd64.yml",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("the release produced %v", names)
	}

	// The script reads each manifest's fields from the binary it names, so a
	// manifest cannot describe a build that does not exist.
	digests := map[string]string{}
	for _, flavour := range []string{"svmd-v9.9.9-test-linux-amd64", "svmd-devnet-v9.9.9-test-linux-amd64"} {
		binary := filepath.Join(out, flavour)
		manifest := read(t, binary+".yml")
		info, extra := reports(t, binary)
		for key, got := range map[string]string{
			"version":       info["version"].(string),
			"commit":        info["commit"].(string),
			"network":       extra["network"],
			"engine_digest": extra["engine_digest"],
		} {
			if manifest[key] != got {
				t.Fatalf("%s: the manifest says %s %q, the binary says %q", flavour, key, manifest[key], got)
			}
		}
		if manifest["target"] != "linux/amd64" {
			t.Fatalf("%s: the manifest targets %q", flavour, manifest["target"])
		}
		digests[manifest["network"]] = manifest["engine_digest"]
	}
	if len(digests) != 2 {
		t.Fatalf("the two artifacts report one network: %v", digests)
	}
	if digests["production"] != digests["devnet"] {
		t.Fatalf("the two builds report different engine digests: %v", digests)
	}

	// svm-tpu's manifest names the release, the streamer it wraps and the
	// bytes it describes.
	tpu := filepath.Join(out, "svm-tpu-v9.9.9-test-linux-amd64")
	manifest := read(t, tpu+".yml")
	body, err := os.ReadFile(tpu)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	for key, want := range map[string]string{
		"version": "v9.9.9-test", "commit": "0123456789abcdef0123456789abcdef01234567", "target": "linux/amd64",
		"solana_streamer": "4.3.0", "sha256": hex.EncodeToString(sum[:]),
	} {
		if manifest[key] != want {
			t.Fatalf("svm-tpu's manifest says %s %q, want %q", key, manifest[key], want)
		}
	}
}

// read parses the manifest, which is flat "key: value" lines.
func read(t *testing.T, path string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("%s: %q is not a manifest line", path, line)
		}
		out[key] = value
	}
	return out
}

// TestTheNetworkDoesNotComeFromBuildTags proves that the reported network
// comes from the compiled constant, not from version.BuildTags. The test
// builds a production binary with BuildTags set to devnet, and the binary
// still reports production.
func TestTheNetworkDoesNotComeFromBuildTags(t *testing.T) {
	root := repoRoot(t)
	binary := filepath.Join(t.TempDir(), "svmd-lying")
	build := exec.Command("go", "build", "-o", binary,
		"-ldflags", "-X github.com/cosmos/cosmos-sdk/version.BuildTags=devnet", "./cmd/svmd")
	build.Dir = root
	if body, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, body)
	}
	info, extra := reports(t, binary)
	if info["build_tags"] != "devnet" {
		t.Fatalf("the ldflag did not take: build_tags is %q", info["build_tags"])
	}
	if extra["network"] != "production" {
		t.Fatalf("a production binary told it was devnet reports network %q", extra["network"])
	}
}

// TestMakeTestRunsTheDevnetHalf proves that `make test` runs the devnet tests.
// The faucet, its window table and its key handling are behind a build tag. The
// test checks the command as make expands it, so a variable cannot hide it.
func TestMakeTestRunsTheDevnetHalf(t *testing.T) {
	cmd := exec.Command("make", "-n", "test")
	cmd.Dir = repoRoot(t)
	plan, err := cmd.Output()
	if err != nil {
		t.Fatalf("make -n test: %v", err)
	}
	if !strings.Contains(string(plan), "go test -count=1 -tags devnet ./...") {
		t.Fatalf("make test does not run the devnet half:\n%s", plan)
	}
}
