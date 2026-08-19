//go:build linux

package e2e

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestBrowserCommandDropsToNonRoot(t *testing.T) {
	command := exec.Command("true")
	configureBrowserCommand(command, 1000)
	if command.SysProcAttr == nil || command.SysProcAttr.Credential == nil {
		t.Fatal("browser command has no non-root credential")
	}
	credential := command.SysProcAttr.Credential
	if credential.Uid != 1000 || credential.Gid != 1000 {
		t.Fatalf("browser credential = %d:%d, want 1000:1000", credential.Uid, credential.Gid)
	}
	if len(credential.Groups) != 1 || credential.Groups[0] != 1000 {
		t.Fatalf("browser supplementary groups = %v, want only 1000", credential.Groups)
	}
	if !command.SysProcAttr.Setpgid {
		t.Fatal("browser command must retain process-group cleanup")
	}
}

func configureBrowserCommand(command *exec.Cmd, uid uint32) {
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uid,
			Gid:    uid,
			Groups: []uint32{uid},
		},
		Setpgid: true,
	}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}
