//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestReloadSignals(t *testing.T) {
	requests, stop := reloadSignals()
	defer stop()
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}

	defer process.Release()
	if err := process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}

	select {
	case received := <-requests:
		if received != syscall.SIGHUP {
			t.Fatalf("unexpected signal: %v", received)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not request a reload")
	}
}
