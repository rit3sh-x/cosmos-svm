package ffi

import (
	"os"
	"testing"
)

// TestMain configures the engine, which blocks need, and the next block at
// height 1, which admission needs. These tests use no gates.
func TestMain(m *testing.M) {
	if err := Configure(nil); err != nil {
		panic(err)
	}
	if err := ConfigureNext(1, nil); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
