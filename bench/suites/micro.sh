# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# The microbenchmarks. Go's testing.B times the FFI crossings, and benchstat
# summarizes them. Criterion times SVM execution and compares each run with the
# last one in bench/svm/target/criterion. `make bench` runs this suite.
about="FFI crossings (testing.B, benchstat) and SVM execution"
needs=(ffi criterion)

cells() {
    cell go - go test ./ffi -run '^$' -bench . -count 10
    cell go-benchstat - go run golang.org/x/perf/cmd/benchstat@v0.0.0-20260908200009-22c9c6c9d4da "$OUT/go.txt"
    cell svm-criterion - cargo bench --locked --manifest-path bench/svm/Cargo.toml --bench execution
}
