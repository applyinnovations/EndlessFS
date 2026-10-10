//go:build !linux && !darwin

package imagegen

import (
	"github.com/applyinnovations/endlessfs/internal/telemetry"
	"os"
)

func workerResources(state *os.ProcessState) telemetry.WorkerResources {
	return telemetry.WorkerResources{CPUSeconds: (state.UserTime() + state.SystemTime()).Seconds(), ExitCode: state.ExitCode(), PeakBytes: -1}
}
