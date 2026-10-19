//go:build linux && amd64

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const namespaceStartupTimeout = 30 * time.Second

func namespaceExitStatus(err error) int {
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() > 0 {
		return exit.ExitCode()
	}
	return 1
}

func launchIsolatedConsumer(ctx context.Context, c config, args []string, root string) (int, bool) {
	configPath, _ := ctx.Value(consumerConfigKey{}).(string)
	if ctx.Err() != nil || len(args) != 0 || !storePath(configPath) || !filepath.IsAbs(root) || !safeParents(root, false) || filepath.Base(c.NamespaceLauncher) != "unshare" {
		return 1, false
	}
	requirePinnedExecutable(c.NamespaceLauncher)
	requirePinnedExecutable(c.Helper)
	work := scratch()
	defer os.RemoveAll(work)
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	capture, e := openEvidenceFile(filepath.Join(work, "private-namespace.stderr"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return 1, false
	}
	defer capture.Close()
	log := &boundedLog{File: capture, Limit: 1 << 20, Cancel: cancel}
	readyRead, readyWrite, e := namespaceChannel()
	if e != nil {
		return 1, false
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	continueRead, continueWrite, e := namespaceChannel()
	if e != nil {
		return 1, false
	}
	defer continueRead.Close()
	defer continueWrite.Close()
	// Fixed self metadata only; this handle proves separation without publishing
	// either namespace identifier or any process identity.
	parentNamespace, e := os.Open("/proc/self/ns/pid")
	if e != nil {
		return 1, false
	}
	defer parentNamespace.Close()
	// #nosec G204 G702 -- Validated immutable unshare and helper executables, fixed namespace/mapping/signal flags, immutable validated config path and no user arguments. No shell is involved.
	cmd := exec.CommandContext(childCtx, c.NamespaceLauncher, namespaceArguments(c, configPath, nil)...)
	cmd.Dir, cmd.Env = root, os.Environ()
	cmd.ExtraFiles = []*os.File{readyWrite, continueRead, parentNamespace}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Launcher errors remain private; only decoded closed helper diagnostics are
	// forwarded after completion. The immutable helper's stdout is its result.
	cmd.Stdout, cmd.Stderr = os.Stdout, log
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if e == syscall.ESRCH {
			return nil
		}
		return e
	}
	if e := cmd.Start(); e != nil {
		return 1, namespaceFallbackAllowed(ctx.Err() != nil, false, true, false)
	}
	_ = readyWrite.Close()
	_ = continueRead.Close()
	committed, readyErr := namespaceParentHandshake(ctx, readyRead, continueWrite)
	_ = continueWrite.Close()
	forced := errors.Is(readyErr, errNamespaceTimeout) || errors.Is(readyErr, errNamespaceTransport) || ctx.Err() != nil
	if forced {
		// An uncertain/forced startup stop never selects fallback. Normal startup
		// rejection closes the channel and lets unshare reap its isolated child.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	closeErr := capture.Close()
	diagnosticsValid := forwardNamespaceDiagnostics(readFile(filepath.Join(work, "private-namespace.stderr"), 1<<20))
	exit, exited := waitErr.(*exec.ExitError)
	// A signaled launcher did not complete its own child wait. Only its normal
	// return can prove that startup rejection finished before fresh fallback.
	childReaped := waitErr == nil || exited && exit.ExitCode() >= 0
	captureFailed := log.Capped || log.Failed || closeErr != nil || !diagnosticsValid
	if namespaceFallbackAllowed(ctx.Err() != nil, committed, childReaped, forced || captureFailed) {
		return 1, true
	}
	if readyErr != nil || ctx.Err() != nil || !committed || captureFailed {
		return 1, false
	}
	return namespaceExitStatus(waitErr), false
}

func namespaceParentHandshake(ctx context.Context, readyRead, continueWrite *os.File) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	stop := context.AfterFunc(ctx, func() { _ = readyRead.Close() })
	defer stop()
	if e := readyRead.SetReadDeadline(time.Now().Add(namespaceStartupTimeout)); e != nil {
		return false, errNamespaceTransport
	}
	if e := readNamespaceToken(readyRead, namespaceReady); e != nil {
		return false, e
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if e := continueWrite.SetWriteDeadline(time.Now().Add(namespaceStartupTimeout)); e != nil {
		return false, errNamespaceTransport
	}
	// The return value remains committed even on a short/failed CONTINUE write.
	return true, writeNamespaceToken(continueWrite, namespaceContinue)
}

func forwardNamespaceDiagnostics(body []byte) bool {
	return closedNamespaceDiagnostics(body, os.Stderr)
}
func closedNamespaceDiagnostics(body []byte, writer io.Writer) bool {
	rows := 0
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, helperDiagnosticPrefix) {
			continue
		}
		rows++
		if len(line) > helperDiagnosticRowBytes || rows > helperDiagnosticRows {
			return false
		}
		var row helperDiagnosticRow
		if json.Unmarshal([]byte(strings.TrimPrefix(line, helperDiagnosticPrefix)), &row) != nil || !consumerDiagnosticAllowed(row) {
			return false
		}
		// Re-encoding the closed struct drops any unknown fields. The originating
		// helper is the immutable selected binary, never caller-provided output.
		encoded, e := json.Marshal(row)
		if e != nil || len(encoded)+len(helperDiagnosticPrefix)+1 > helperDiagnosticRowBytes {
			return false
		}
		out := append(append([]byte(helperDiagnosticPrefix), encoded...), '\n')
		if n, e := writer.Write(out); e != nil || n != len(out) {
			return false
		}
	}
	return true
}
func consumerDiagnosticAllowed(row helperDiagnosticRow) bool {
	if row.Schema != "endlessfs-functional-diagnostic-v1" || row.Role != "consume" || row.ElapsedNanoseconds < 0 || row.DurationNanoseconds < 0 || row.StdoutBytes < 0 || row.StderrBytes < 0 || row.FailureLine < 0 || row.FailureLine > 10000 || row.FailureFile > 15 || row.PackageSHA256 != "" || row.RootSHA256 != "" {
		return false
	}
	known := false
	for _, stage := range helperStageNames {
		known = known || row.Stage == stage
	}
	if !known || row.Command != "" && helperCommandName(row.Command) != row.Command || closedCleanupStage(row.ScanStage) != row.ScanStage || closedCleanupReason(row.ScanReason) != row.ScanReason {
		return false
	}
	switch row.Event {
	case "stage", "command", "terminal":
	default:
		return false
	}
	switch row.Result {
	case "entered", "incomplete", "failed", "passed", "success", "fallback_selected", "deadline", "canceled":
	default:
		return false
	}
	return true
}

func boundedNamespaceMetadata(path string) ([]byte, error) {
	if path != "/proc/self/uid_map" && path != "/proc/self/gid_map" {
		return nil, errContract
	}
	// #nosec G304 G703 -- Exactly one of two fixed kernel self-metadata paths above; no caller-selected body is opened.
	f, e := os.Open(path)
	if e != nil {
		return nil, errContract
	}
	body, e := io.ReadAll(io.LimitReader(f, 4097))
	closeErr := f.Close()
	if e != nil || closeErr != nil || len(body) > 4096 {
		return nil, errContract
	}
	return body, nil
}

func namespaceStartupValid(parent *os.File) bool {
	if os.Getpid() != 1 || os.Getppid() != 0 || os.Getuid() != os.Geteuid() || os.Getgid() != os.Getegid() {
		return false
	}
	self, e := os.Readlink("/proc/self")
	if e != nil || self != "1" {
		return false
	}
	prior, e := parent.Stat()
	if e != nil {
		return false
	}
	current, e := os.Stat("/proc/self/ns/pid")
	if e != nil || os.SameFile(prior, current) {
		return false
	}
	parentLink, e := os.Readlink("/proc/self/fd/5")
	if e != nil {
		return false
	}
	uid, e := boundedNamespaceMetadata("/proc/self/uid_map")
	if e != nil {
		return false
	}
	gid, e := boundedNamespaceMetadata("/proc/self/gid_map")
	return e == nil && namespaceFactsValid(namespaceFacts{PID: os.Getpid(), PPID: os.Getppid(), UID: os.Getuid(), EUID: os.Geteuid(), GID: os.Getgid(), EGID: os.Getegid(), ProcSelf: self, ParentLink: parentLink, UIDMap: string(uid), GIDMap: string(gid), SeparatePIDNamespace: !os.SameFile(prior, current)})
}

// SOCK_SEQPACKET provides one atomic, bounded versioned message per direction.
// NONBLOCK makes the inherited endpoints compatible with Go's poll deadlines;
// CLOEXEC plus ExtraFiles restricts the exposed child descriptors to 3 and 4.
func namespaceChannel() (*os.File, *os.File, error) {
	fds, e := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if e != nil {
		return nil, nil, errNamespaceTransport
	}
	return os.NewFile(uintptr(fds[0]), "namespace-parent"), os.NewFile(uintptr(fds[1]), "namespace-child"), nil
}

func namespaceChannelValid(file *os.File) bool {
	info, e := file.Stat()
	if e != nil || info.Mode()&os.ModeSocket == 0 || !owned(info) {
		return false
	}
	raw, e := file.SyscallConn()
	if e != nil {
		return false
	}
	valid := false
	e = raw.Control(func(fd uintptr) {
		if fd > 1<<20 {
			return
		}
		kind, e := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_TYPE)
		if e != nil || kind != syscall.SOCK_SEQPACKET {
			return
		}
		peer, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		valid = e == nil && sameUID(uint64(peer.Uid), os.Geteuid()) && sameUID(uint64(peer.Gid), os.Getegid())
	})
	return e == nil && valid
}

func consumeIsolated(ctx context.Context, c config, args []string) int {
	setHelperStage(ctx, diagnosticNamespaceStartup)
	ready := os.NewFile(3, "namespace-ready")
	continuation := os.NewFile(4, "namespace-continue")
	parent := os.NewFile(5, "namespace-parent")
	if ready == nil || continuation == nil || parent == nil {
		return 1
	}
	defer ready.Close()
	defer continuation.Close()
	defer parent.Close()
	readyInfo, readyErr := ready.Stat()
	continueInfo, continueErr := continuation.Stat()
	if readyErr != nil || continueErr != nil || os.SameFile(readyInfo, continueInfo) || !namespaceChannelValid(ready) || !namespaceChannelValid(continuation) || !namespaceStartupValid(parent) || ctx.Err() != nil || len(args) != 0 {
		return 1
	}
	// Every unsupported selection is decided before announcing readiness. The
	// post-CONTINUE consume path may fail closed, but can never run legacy.
	if _, ok := tryEligibility(func() guardSnapshot {
		before := guard(ctx, c, sourceManifest(c.FullSource, false), os.Environ(), args)
		validateExport(envMap(os.Environ())["ENDLESSFS_COVERAGE_PROFILE"], before.Root)
		return before
	}); !ok {
		return 1
	}
	if e := ready.SetWriteDeadline(time.Now().Add(namespaceStartupTimeout)); e != nil {
		return 1
	}
	if e := writeNamespaceToken(ready, namespaceReady); e != nil {
		return 1
	}
	_ = ready.Close()
	if e := continuation.SetReadDeadline(time.Now().Add(namespaceStartupTimeout)); e != nil {
		return 1
	}
	if e := readNamespaceToken(continuation, namespaceContinue); e != nil || ctx.Err() != nil {
		return 1
	}
	_ = continuation.Close()
	_ = parent.Close()
	ctx = context.WithValue(ctx, isolatedConsumerKey{}, true)
	return consume(ctx, c, args)
}
