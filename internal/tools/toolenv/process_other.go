//go:build !unix

package toolenv

import (
	"os"
	"os/exec"
)

func prepareCommand(cmd *exec.Cmd) {}

func interruptCommand(cmd *exec.Cmd) error {
	return cmd.Process.Signal(os.Interrupt)
}

func killCommand(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}
