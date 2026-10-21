//go:build linux && amd64

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Only exact freshly owned TMPDIR inheritance establishes ownership. No argv,
// environment body, PID, start time, or executable pathname is published.
type cleanupObservation struct {
	Adequate, NaturalZero, FinalZero            bool
	Scope                                       string
	InitialOwned, FinalOwned, PidfdKillRequests uint64
	InitialKinds                                map[string]uint64
	Failure                                     string
	InitialScanComplete, FinalScanComplete      bool
	ScanStage, ScanReason                       string
	ReapedChildren                              uint64
	NamespaceIsolated                           bool
}
type ownedProcess struct {
	PID   int
	Start string
}

func processEnvironment(path string) ([]byte, error) {
	f, e := openEvidenceFile(path, os.O_RDONLY, 0)
	if e != nil {
		return nil, nativeScanFailure("environment_open", e)
	}
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	closeErr := f.Close()
	if e != nil {
		return nil, nativeScanFailure("environment_read", e)
	}
	if closeErr != nil {
		return nil, nativeScanFailure("environment_close", closeErr)
	}
	if len(b) > 1<<20 {
		return nil, nativeScanFailure("environment_bound", errContract)
	}
	return b, nil
}
func ownedMarker(body []byte, tmp string) bool {
	if len(body) > 1<<20 || len(body) > 0 && body[len(body)-1] != 0 {
		return false
	}
	want := []byte("TMPDIR=" + tmp)
	count := 0
	for _, field := range bytes.Split(body, []byte{0}) {
		if bytes.Equal(field, want) {
			count++
		}
	}
	return count == 1
}
func processStart(body string) (string, error) {
	cut := strings.LastIndex(body, ") ")
	if cut < 0 {
		return "", errContract
	}
	fields := strings.Fields(body[cut+2:])
	if len(fields) < 20 {
		return "", errContract
	}
	if _, e := strconv.ParseUint(fields[19], 10, 64); e != nil {
		return "", errContract
	}
	return fields[19], nil
}
func processKind(exe string) string {
	switch filepath.Base(strings.TrimSuffix(exe, " (deleted)")) {
	case "chromium", "chrome", "chrome-headless-shell", "chrome_crashpad_handler":
		return "browser"
	case "go", "compile", "link", "vet", "asm":
		return "go-tool"
	case "dcraw_emu":
		return "decoder"
	default:
		return "other-owned-native"
	}
}
func scanOwnedScoped(tmp string, private bool) ([]ownedProcess, map[string]uint64, error) {
	if !filepath.IsAbs(tmp) || filepath.Clean(tmp) != tmp {
		return nil, nil, errContract
	}
	entries, e := os.ReadDir("/proc")
	if e != nil {
		return nil, nil, nativeScanFailure("proc_inventory", e)
	}
	if len(entries) > 32768 {
		return nil, nil, nativeScanFailure("proc_bound", errContract)
	}
	out := []ownedProcess{}
	kinds := map[string]uint64{}
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid <= 0 {
			continue
		}
		if private && pid == os.Getpid() {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		info, e := os.Stat(base)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, nil, nativeScanFailure("process_metadata", e)
		}
		raw, ok := info.Sys().(*syscall.Stat_t)
		uid := os.Geteuid()
		if !ok || uid < 0 {
			return nil, nil, errContract
		}
		if uint64(raw.Uid) != uint64(uid) {
			if private {
				return nil, nil, nativeScanFailure("scope_identity", errContract)
			}
			continue
		}
		env, e := processEnvironment(filepath.Join(base, "environ"))
		if errors.Is(e, os.ErrNotExist) || errors.Is(e, syscall.ESRCH) {
			continue
		}
		if e != nil {
			return nil, nil, e
		}
		if !ownedMarker(env, tmp) {
			if private {
				return nil, nil, nativeScanFailure("scope_marker", errContract)
			}
			continue
		}
		stat, e := readBounded(filepath.Join(base, "stat"), 64<<10)
		if errors.Is(e, os.ErrNotExist) || errors.Is(e, syscall.ESRCH) {
			continue
		}
		if e != nil {
			return nil, nil, nativeScanFailure("owned_stat", e)
		}
		start, e := processStart(stat)
		if e != nil {
			return nil, nil, nativeScanFailure("owned_identity", e)
		}
		exe, e := os.Readlink(filepath.Join(base, "exe"))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, nil, nativeScanFailure("owned_executable", e)
		}
		out = append(out, ownedProcess{pid, start})
		kinds[processKind(exe)]++
		if len(out) > 1024 {
			return nil, nil, nativeScanFailure("owned_bound", errContract)
		}
	}
	return out, kinds, nil
}
func killOwned(item ownedProcess, tmp string) error {
	// pidfd_open / pidfd_send_signal are Linux-amd64 syscalls. Opening first and
	// rechecking UID/start/marker prevents signaling a reused or unrelated PID.
	fd, _, errno := syscall.RawSyscall(434, uintptr(item.PID), 0, 0)
	if errno == syscall.ESRCH {
		return nil
	}
	if errno != 0 {
		return errno
	}
	defer syscall.Close(int(fd))
	base := filepath.Join("/proc", strconv.Itoa(item.PID))
	info, e := os.Stat(base)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	raw, ok := info.Sys().(*syscall.Stat_t)
	uid := os.Geteuid()
	if !ok || uid < 0 || uint64(raw.Uid) != uint64(uid) {
		return errContract
	}
	stat, e := readBounded(filepath.Join(base, "stat"), 64<<10)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	start, e := processStart(stat)
	if e != nil || start != item.Start {
		return errContract
	}
	env, e := processEnvironment(filepath.Join(base, "environ"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil || !ownedMarker(env, tmp) {
		return errContract
	}
	_, _, errno = syscall.RawSyscall6(424, fd, uintptr(syscall.SIGKILL), 0, 0, 0, 0)
	if errno == syscall.ESRCH {
		return nil
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// The isolated consumer is PID1. Reap only after exec.Cmd.Wait, while no
// direct command wait can race this bounded nonblocking orphan drain.
func reapNamespaceChildren() (uint64, error) {
	var reaped uint64
	for reaped < 1024 {
		var status syscall.WaitStatus
		pid, e := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if e == syscall.ECHILD || e == nil && pid == 0 {
			return reaped, nil
		}
		if e != nil {
			return reaped, nativeScanFailure("orphan_reap", e)
		}
		if pid <= 0 {
			return reaped, nativeScanFailure("orphan_reap", errContract)
		}
		reaped++
	}
	return reaped, nativeScanFailure("orphan_bound", errContract)
}

type nativeCleanupActions struct {
	Scan func() ([]ownedProcess, map[string]uint64, error)
	Kill func(ownedProcess) error
	Reap func() (uint64, error)
}

func observeAndCleanScoped(tmp string, private bool) cleanupObservation {
	return observeCleanup(private, nativeCleanupActions{
		Scan: func() ([]ownedProcess, map[string]uint64, error) { return scanOwnedScoped(tmp, private) },
		Kill: func(item ownedProcess) error { return killOwned(item, tmp) },
		Reap: reapNamespaceChildren,
	})
}
func observeCleanup(private bool, a nativeCleanupActions) (r cleanupObservation) {
	r.Scope = "same-uid-exact-owned-tmpdir-marker-processes"
	r.NamespaceIsolated = private
	if private {
		r.Scope = consumerIsolationPolicy
	}
	r.InitialKinds = map[string]uint64{}
	r.Failure = "cleanup_observation_inadequate"
	reap := func() error {
		if !private {
			return nil
		}
		count, e := a.Reap()
		r.ReapedChildren += count
		return e
	}
	if e := reap(); e != nil {
		setCleanupFailure(&r, e)
		return r
	}
	initial, kinds, e := a.Scan()
	if e != nil {
		setCleanupFailure(&r, e)
		return r
	}
	r.InitialScanComplete = true
	r.InitialOwned = uint64(len(initial))
	r.InitialKinds = kinds
	r.NaturalZero = len(initial) == 0
	for _, item := range initial {
		r.PidfdKillRequests++
		if e := a.Kill(item); e != nil {
			setCleanupFailure(&r, nativeScanFailure("pidfd_cleanup", e))
			return r
		}
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for checks := 0; checks < 121; checks++ {
		r.FinalScanComplete = false
		if e := reap(); e != nil {
			setCleanupFailure(&r, e)
			return r
		}
		remaining, _, e := a.Scan()
		if e != nil {
			setCleanupFailure(&r, e)
			return r
		}
		r.FinalScanComplete = true
		r.FinalOwned = uint64(len(remaining))
		if len(remaining) == 0 {
			r.FinalZero = true
			r.Adequate = true
			r.Failure = ""
			return r
		}
		select {
		case <-deadline.C:
			return r
		case <-tick.C:
		}
	}
	return r
}
