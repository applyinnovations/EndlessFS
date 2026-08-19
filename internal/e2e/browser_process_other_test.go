//go:build !linux

package e2e

import "os/exec"

func configureBrowserCommand(_ *exec.Cmd, _ uint32) {}
