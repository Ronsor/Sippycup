//go:build !unix

package libsippycup

import (
	"errors"
	"os/exec"
)

// configureChildProcess uses CommandContext's direct-child cancellation where
// Unix process groups and inherited control descriptors are unavailable.
func configureChildProcess(cmd *exec.Cmd, control string) error {
	if control == "json_fd" {
		return errors.New("json_fd control requires a Unix platform")
	}

	return nil
}
