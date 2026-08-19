//go:build linux

package imagegen

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func anonymousRawInput(data []byte) (*os.File, string, error) {
	return anonymousRawInputWith(
		data,
		func() (int, error) {
			return unix.MemfdCreate("endlessfs-raw", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
		},
		func(descriptor uintptr) error {
			_, err := unix.FcntlInt(descriptor, unix.F_ADD_SEALS, unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_WRITE|unix.F_SEAL_SEAL)
			return err
		},
	)
}

func anonymousRawInputWith(data []byte, create func() (int, error), seal func(uintptr) error) (*os.File, string, error) {
	descriptor, err := create()
	if err != nil {
		return nil, "", err
	}
	file := os.NewFile(uintptr(descriptor), "endlessfs-raw")
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, "", fmt.Errorf("RAW memory file is unavailable")
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if err := seal(file.Fd()); err != nil {
		_ = file.Close()
		return nil, "", err
	}
	return file, "/proc/self/fd/3", nil
}
