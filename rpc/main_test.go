package rpc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// steadyMalloc makes glibc unmap freed large allocations instead of keeping
// them in memory. This keeps memory use steady across test runs and lets the
// package tests fit within hosted runner limits.
const steadyMalloc = "MALLOC_MMAP_THRESHOLD_"

// TestMain reruns the test process with steadyMalloc set, because glibc reads
// this setting only at startup. It forwards the test arguments and output,
// and kills the child if the parent exits.
func TestMain(m *testing.M) {
	if os.Getenv(steadyMalloc) != "" {
		os.Exit(m.Run())
	}
	child := exec.Command(os.Args[0], os.Args[1:]...)
	child.Env = append(os.Environ(), steadyMalloc+"=131072")
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	err := child.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		os.Exit(exit.ExitCode())
	case err != nil:
		fmt.Fprintln(os.Stderr, "rpc tests:", err)
		os.Exit(2)
	}
}
