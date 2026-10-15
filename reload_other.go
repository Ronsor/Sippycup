//go:build !unix

package main

import "os"

// reloadSignals disables Unix signal reloads on other platforms.
func reloadSignals() (<-chan os.Signal, func()) {
	return nil, func() {}
}
