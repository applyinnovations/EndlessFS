//go:build linux || darwin

package imagegen

import (
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"os"
	"runtime"
	"syscall"
)

func workerResources(state *os.ProcessState) telemetry.WorkerResources {
	resources := telemetry.WorkerResources{CPUSeconds: (state.UserTime() + state.SystemTime()).Seconds(), ExitCode: state.ExitCode(), PeakBytes: -1}
	if usage, ok := state.SysUsage().(*syscall.Rusage); ok {
		resources.PeakBytes = usage.Maxrss
		if runtime.GOOS == "linux" {
			resources.PeakBytes *= 1024
		}
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok {
		resources.Signaled = status.Signaled()
	}
	return resources
}
