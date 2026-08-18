//go:build linux

package imagegen

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAnonymousRawInputRejectsMemfdCreationFailure(t *testing.T) {
	input, _, err := anonymousRawInputWith([]byte("raw"), func() (int, error) {
		return -1, errors.New("create failed")
	}, func(uintptr) error {
		return nil
	})
	if err == nil || input != nil {
		t.Fatal("anonymous RAW input accepted a failed memory-file creation")
	}
}

func TestAnonymousRawInputRejectsMemfdWriteFailure(t *testing.T) {
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[1])
	input, _, err := anonymousRawInputWith([]byte("raw"), func() (int, error) {
		return pipe[0], nil
	}, func(uintptr) error {
		return nil
	})
	if err == nil || input != nil {
		t.Fatal("anonymous RAW input accepted an unwritable descriptor")
	}
}

func TestAnonymousRawInputRejectsMemfdSeekFailure(t *testing.T) {
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[0])
	input, _, err := anonymousRawInputWith([]byte("raw"), func() (int, error) {
		return pipe[1], nil
	}, func(uintptr) error {
		return nil
	})
	if err == nil || input != nil {
		t.Fatal("anonymous RAW input accepted an unseekable descriptor")
	}
}

func TestAnonymousRawInputRejectsMemfdSealFailure(t *testing.T) {
	input, _, err := anonymousRawInputWith([]byte("raw"), func() (int, error) {
		return unix.MemfdCreate("endlessfs-raw-test", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	}, func(uintptr) error {
		return errors.New("seal failed")
	})
	if err == nil || input != nil {
		t.Fatal("anonymous RAW input accepted an unsealed memory file")
	}
}
