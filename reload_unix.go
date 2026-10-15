//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// reloadSignals coalesces repeated SIGHUP requests while a reload is in progress.
func reloadSignals() (<-chan os.Signal, func()) {
	requests := make(chan os.Signal, 1)
	signal.Notify(requests, syscall.SIGHUP)

	return requests, func() { signal.Stop(requests) }
}
