//go:build !linux

package imagegen

import "os/exec"

func protectRawDecoderChild(command *exec.Cmd) { _ = command }
