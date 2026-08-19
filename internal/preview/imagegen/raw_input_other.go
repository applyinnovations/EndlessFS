//go:build !linux

package imagegen

import "os"

func anonymousRawInput(data []byte) (*os.File, string, error) {
	file, err := os.CreateTemp("", "endlessfs-raw-*")
	if err != nil {
		return nil, "", err
	}
	name := file.Name()
	if err := os.Remove(name); err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = file.Close()
		return nil, "", err
	}
	return file, "/dev/fd/3", nil
}
