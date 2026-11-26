//go:build windows

package tools

import "os/exec"

// setProcessGroup is a no-op on Windows: cancellation kills only the shell.
func setProcessGroup(*exec.Cmd) {}
