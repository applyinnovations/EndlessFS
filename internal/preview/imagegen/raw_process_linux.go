//go:build linux

package imagegen

import (
	"os/exec"
	"syscall"
)

func protectRawDecoderChild(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
