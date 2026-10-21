//go:build linux && amd64

package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNamespaceReadyDoesNotRequireLauncherEndpointEOF(t *testing.T) {
	fds, e := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if e != nil {
		t.Fatal("message channel setup")
	}
	r, w := os.NewFile(uintptr(fds[0]), "ready-parent"), os.NewFile(uintptr(fds[1]), "ready-child")
	defer r.Close()
	defer w.Close()
	if r.SetReadDeadline(time.Now().Add(200*time.Millisecond)) != nil || writeNamespaceToken(w, namespaceReady) != nil {
		t.Fatal("message channel deadline/write")
	}
	// unshare's waiting parent can retain the child's endpoint. A complete
	// message must allow CONTINUE without waiting for that endpoint to close.
	if readNamespaceToken(r, namespaceReady) != nil {
		t.Fatal("complete READY blocked on retained launcher endpoint")
	}
}

func TestNativeInitialObservationFailureNeverCreditsZero(t *testing.T) {
	for _, cause := range []error{os.ErrPermission, syscall.ESRCH, syscall.EIO, errContract} {
		calls := 0
		r := observeCleanup(false, nativeCleanupActions{Scan: func() ([]ownedProcess, map[string]uint64, error) {
			return nil, nil, nativeScanFailure("environment_open", cause)
		}, Kill: func(ownedProcess) error { calls++; return nil }})
		if r.Adequate || r.InitialScanComplete || r.FinalScanComplete || r.NaturalZero || r.FinalZero || calls != 0 || r.ScanStage != "environment_open" {
			t.Fatal("incomplete first scan credited zero or sent a signal")
		}
	}
}

func TestNativeFinalObservationFailureKeepsInitialEvidence(t *testing.T) {
	count := 0
	r := observeCleanup(false, nativeCleanupActions{Scan: func() ([]ownedProcess, map[string]uint64, error) {
		count++
		if count == 1 {
			return nil, map[string]uint64{}, nil
		}
		return nil, nil, nativeScanFailure("owned_stat", syscall.EIO)
	}})
	if !r.InitialScanComplete || !r.NaturalZero || r.FinalScanComplete || r.FinalZero || r.Adequate || r.ScanStage != "owned_stat" || r.ScanReason != "io_error" {
		t.Fatal("failed final scan became adequate")
	}
}

func TestNativeForcedCleanupCannotBecomeNaturalSuccess(t *testing.T) {
	for _, killFailure := range []bool{false, true} {
		count, kills := 0, 0
		r := observeCleanup(false, nativeCleanupActions{Scan: func() ([]ownedProcess, map[string]uint64, error) {
			count++
			if count == 1 {
				return []ownedProcess{{PID: 123, Start: "456"}}, map[string]uint64{"go-tool": 1}, nil
			}
			return nil, map[string]uint64{}, nil
		}, Kill: func(ownedProcess) error {
			kills++
			if killFailure {
				return os.ErrPermission
			}
			return nil
		}})
		if !r.InitialScanComplete || r.NaturalZero || r.InitialOwned != 1 || r.PidfdKillRequests != 1 || kills != 1 {
			t.Fatal("live owned native not retained")
		}
		if killFailure {
			if r.Adequate || r.FinalScanComplete || r.ScanStage != "pidfd_cleanup" {
				t.Fatal("failed cleanup credited")
			}
		} else if !r.Adequate || !r.FinalScanComplete || !r.FinalZero {
			t.Fatal("successful forced cleanup not observed")
		}
		if commandPassed(commandResult{Cleanup: r}) {
			t.Fatal("forced cleanup accepted as functional success")
		}
	}
}

func TestNamespaceOrphanReapOccursOnlyAtQuiescentBoundaries(t *testing.T) {
	var order []string
	r := observeCleanup(true, nativeCleanupActions{Reap: func() (uint64, error) { order = append(order, "reap"); return 1, nil }, Scan: func() ([]ownedProcess, map[string]uint64, error) {
		order = append(order, "scan")
		return nil, map[string]uint64{}, nil
	}})
	if !reflect.DeepEqual(order, []string{"reap", "scan", "reap", "scan"}) || !r.Adequate || !r.NaturalZero || !r.FinalZero || r.ReapedChildren != 2 || r.Scope != consumerIsolationPolicy {
		t.Fatal("orphan/scan order or scope changed")
	}
	r = observeCleanup(true, nativeCleanupActions{Reap: func() (uint64, error) { return 0, nativeScanFailure("orphan_reap", syscall.EIO) }})
	if r.Adequate || r.InitialScanComplete || r.ScanStage != "orphan_reap" {
		t.Fatal("failed reap credited an initial scan")
	}
	if _, e := reapNamespaceChildren(); e != nil {
		t.Fatal("empty quiescent orphan drain failed")
	}
}

func TestNativeDiagnosticClassesNeverPublishUnderlyingErrors(t *testing.T) {
	secret := "private/path/user-id/capability"
	err := nativeScanFailure(secret, errors.New(secret))
	if strings.Contains(err.Error(), secret) {
		t.Fatal("raw error exposed")
	}
	for _, cause := range []error{os.ErrPermission, os.ErrNotExist, syscall.ESRCH, syscall.EIO, errContract, errors.New(secret), nil} {
		var r cleanupObservation
		setCleanupFailure(&r, nativeScanFailure("environment_read", cause))
		if r.ScanStage != "environment_read" || strings.Contains(r.ScanReason, secret) {
			t.Fatal("unsafe scan reason")
		}
	}
	var r cleanupObservation
	setCleanupFailure(&r, errors.New(secret))
	if r.ScanStage != "unknown" || r.ScanReason != "other" {
		t.Fatal("unknown error escaped closed classification")
	}
	for i := 0; i < 256; i++ {
		if closedCleanupStage(secret+strings.Repeat("x", i)) != "unknown" || closedCleanupReason(secret+strings.Repeat("x", i)) != "unknown" {
			t.Fatal("unbounded cleanup cardinality")
		}
	}
}

func TestNamespaceChannelRejectsStreamsFilesAndClosedDescriptors(t *testing.T) {
	r, w, e := namespaceChannel()
	if e != nil {
		t.Fatal("message channel setup")
	}
	defer r.Close()
	defer w.Close()
	if !namespaceChannelValid(r) || !namespaceChannelValid(w) {
		t.Fatal("owned message endpoints rejected")
	}
	pr, pw, e := os.Pipe()
	if e != nil {
		t.Fatal("pipe fixture")
	}
	defer pr.Close()
	defer pw.Close()
	if namespaceChannelValid(pr) || namespaceChannelValid(pw) {
		t.Fatal("stream pipe accepted")
	}
	fds, e := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if e != nil {
		t.Fatal("stream socket fixture")
	}
	sr, sw := os.NewFile(uintptr(fds[0]), "stream-parent"), os.NewFile(uintptr(fds[1]), "stream-child")
	defer sr.Close()
	defer sw.Close()
	if namespaceChannelValid(sr) || namespaceChannelValid(sw) {
		t.Fatal("stream socket accepted")
	}
	f, e := os.CreateTemp(t.TempDir(), "not-channel")
	if e != nil {
		t.Fatal("regular fixture")
	}
	defer f.Close()
	if namespaceChannelValid(f) {
		t.Fatal("regular data file accepted as IPC")
	}
	_ = r.Close()
	if namespaceChannelValid(r) {
		t.Fatal("closed descriptor accepted")
	}
	if namespaceStartupValid(f) {
		t.Fatal("ordinary host process accepted as isolated PID1")
	}
	if _, e := boundedNamespaceMetadata("/private/customer"); e == nil {
		t.Fatal("arbitrary namespace metadata path opened")
	}
	for _, p := range []string{"/proc/self/uid_map", "/proc/self/gid_map"} {
		if b, e := boundedNamespaceMetadata(p); e != nil || len(b) == 0 {
			t.Fatal("kernel identity map unreadable")
		}
	}
}

func TestNamespaceLaunchRejectsUnvalidatedSelectionBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, fallback := launchIsolatedConsumer(ctx, config{}, nil, "/"); code == 0 || fallback {
		t.Fatal("canceled namespace launch accepted")
	}
	if code, fallback := launchIsolatedConsumer(context.Background(), config{}, nil, "/"); code == 0 || fallback {
		t.Fatal("unvalidated config namespace launch accepted")
	}
	if namespaceExitStatus(nil) != 0 || namespaceExitStatus(errors.New("private")) != 1 {
		t.Fatal("namespace exit status classification")
	}
}

func TestNamespaceParentHandshakeCommitsBeforeFailedContinueWrite(t *testing.T) {
	for _, failContinue := range []bool{false, true} {
		r, w, e := namespaceChannel()
		if e != nil {
			t.Fatal("ready pipe")
		}
		cr, cw, e := namespaceChannel()
		if e != nil {
			t.Fatal("continue pipe")
		}
		done := make(chan error, 1)
		go func() {
			e := writeNamespaceToken(w, namespaceReady)
			if e == nil && !failContinue {
				e = readNamespaceToken(cr, namespaceContinue)
			}
			done <- e
		}()
		if failContinue {
			_ = cr.Close()
		}
		committed, err := namespaceParentHandshake(context.Background(), r, cw)
		_ = cw.Close()
		if !committed || (err != nil) != failContinue || namespaceFallbackAllowed(false, committed, true, false) {
			t.Fatal("continue write lost irrevocable boundary")
		}
		if e := <-done; e != nil {
			t.Fatal("child protocol did not complete")
		}
		_ = r.Close()
		_ = w.Close()
		_ = cr.Close()
		_ = cw.Close()
	}
}

func TestNamespaceParentHandshakeRejectsEarlyExitAndCancellation(t *testing.T) {
	r, w, e := namespaceChannel()
	if e != nil {
		t.Fatal("ready pipe")
	}
	defer r.Close()
	_ = w.Close()
	cr, cw, e := namespaceChannel()
	if e != nil {
		t.Fatal("continue pipe")
	}
	defer cr.Close()
	defer cw.Close()
	if committed, err := namespaceParentHandshake(context.Background(), r, cw); committed || !errors.Is(err, errContract) {
		t.Fatal("early child exit committed realization")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if committed, err := namespaceParentHandshake(ctx, r, cw); committed || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation committed realization")
	}
	_ = r.Close()
	if committed, err := namespaceParentHandshake(context.Background(), r, cw); committed || !errors.Is(err, errNamespaceTransport) {
		t.Fatal("failed handshake descriptor accepted")
	}
}
