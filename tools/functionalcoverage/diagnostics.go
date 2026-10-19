package main

import (
	"context"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Diagnostic text is deliberately independent of config, errors, argv, source
// names, event Output and cleanup Scope/Failure strings. It is not a receipt.
const helperDiagnosticPrefix = "FUNCTIONAL_COVERAGE_DIAGNOSTIC="
const helperDiagnosticRows = 64
const helperDiagnosticRowBytes = 2048

type helperStage uint8

const (
	diagnosticArguments helperStage = iota
	diagnosticConfig
	diagnosticInputIdentity
	diagnosticProducerAttributes
	diagnosticOutputDirectory
	diagnosticScratch
	diagnosticClosure
	diagnosticSelfIdentity
	diagnosticFixtureCopy
	diagnosticGoSetup
	diagnosticSeedInstall
	diagnosticInventory
	diagnosticCoverage
	diagnosticEvents
	diagnosticProfile
	diagnosticPolicy
	diagnosticFinalIdentity
	diagnosticPublishProfile
	diagnosticPublishEvents
	diagnosticPublishSummary
	diagnosticPublishReceipt
	diagnosticSuccessMarker
	diagnosticEligibility
	diagnosticFallback
	diagnosticRealize
	diagnosticRealizeReply
	diagnosticConsumerInventory
	diagnosticArtifact
	diagnosticConsumerPolicy
	diagnosticFinalGuard
	diagnosticExport
	diagnosticComplete
	diagnosticNamespaceLaunch
	diagnosticNamespaceStartup
)

var helperStageNames = [...]string{"arguments", "config", "input_identity", "producer_attributes", "output_directory", "scratch", "closure", "self_identity", "fixture_copy", "go_setup", "seed_install", "inventory", "coverage", "event_validation", "profile_validation", "current_policy", "final_identity", "publish_profile", "publish_events", "publish_summary", "publish_receipt", "success_marker", "eligibility", "fallback_selection", "realize", "realize_reply", "consumer_inventory", "artifact_validation", "consumer_policy", "final_guard", "profile_export", "complete", "namespace_launch", "namespace_startup"}

type helperDiagnosticKey struct{}
type helperDiagnostic struct {
	mu                                             sync.Mutex
	writer                                         io.Writer
	role                                           string
	stage                                          helperStage
	start                                          time.Time
	rows, dropped                                  uint64
	invalid, ioFailed, finished, success, fallback bool
}
type helperDiagnosticRow struct {
	Schema                 string `json:"schema"`
	Role                   string `json:"role"`
	Stage                  string `json:"stage"`
	Event                  string `json:"event"`
	Result                 string `json:"result"`
	Command                string `json:"command,omitempty"`
	ElapsedNanoseconds     int64  `json:"elapsed_ns"`
	CommandObserved        bool   `json:"command_observed"`
	CommandComplete        bool   `json:"command_complete"`
	ExitCode               int    `json:"exit_code"`
	DurationNanoseconds    int64  `json:"duration_ns"`
	StdoutBytes            int64  `json:"stdout_bytes"`
	StderrBytes            int64  `json:"stderr_bytes"`
	Capped                 bool   `json:"capped"`
	IOFailed               bool   `json:"io_failed"`
	GroupKillRequested     bool   `json:"group_kill_requested"`
	CleanupObserved        bool   `json:"cleanup_observed"`
	CleanupAdequate        bool   `json:"cleanup_adequate"`
	NaturalZero            bool   `json:"natural_zero"`
	FinalZero              bool   `json:"final_zero"`
	InitialOwned           uint64 `json:"initial_owned"`
	FinalOwned             uint64 `json:"final_owned"`
	PidfdKillRequests      uint64 `json:"pidfd_kill_requests"`
	Dropped                uint64 `json:"diagnostic_dropped"`
	Invalid                bool   `json:"diagnostic_invalid"`
	DiagnosticIOFailed     bool   `json:"diagnostic_io_failed"`
	FailureFile            uint8  `json:"failure_file,omitempty"`
	FailureLine            int    `json:"failure_line,omitempty"`
	FailureSiteObserved    bool   `json:"failure_site_observed,omitempty"`
	PackageSHA256          string `json:"package_sha256,omitempty"`
	RootSHA256             string `json:"root_sha256,omitempty"`
	FailureRecords         uint64 `json:"failure_records,omitempty"`
	FailureRecordsReported uint64 `json:"failure_records_reported,omitempty"`
	ScanLines              uint64 `json:"scan_lines,omitempty"`
	ScanComplete           bool   `json:"scan_complete,omitempty"`
	InitialScanComplete    bool   `json:"initial_scan_complete"`
	FinalScanComplete      bool   `json:"final_scan_complete"`
	ScanStage              string `json:"cleanup_stage,omitempty"`
	ScanReason             string `json:"cleanup_reason,omitempty"`
	ReapedChildren         uint64 `json:"reaped_children"`
	NamespaceIsolated      bool   `json:"namespace_isolated"`
}

func newHelperDiagnostic(ctx context.Context, writer io.Writer, role string) context.Context {
	if role != "produce" && role != "consume" {
		role = "unknown"
	}
	d := &helperDiagnostic{writer: writer, role: role, start: time.Now()}
	return context.WithValue(ctx, helperDiagnosticKey{}, d)
}
func helperDiagnosticFor(ctx context.Context) *helperDiagnostic {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(helperDiagnosticKey{}).(*helperDiagnostic)
	return d
}
func (d *helperDiagnostic) emitLocked(row helperDiagnosticRow, terminal bool) {
	if d.finished {
		return
	}
	if !terminal && d.rows >= helperDiagnosticRows-1 {
		d.dropped++
		return
	}
	row.Schema = "endlessfs-functional-diagnostic-v1"
	row.Role = d.role
	if int(d.stage) >= len(helperStageNames) {
		row.Stage = "unknown"
		d.invalid = true
	} else {
		row.Stage = helperStageNames[d.stage]
	}
	row.ElapsedNanoseconds = time.Since(d.start).Nanoseconds()
	row.Dropped = d.dropped
	row.Invalid = d.invalid
	row.DiagnosticIOFailed = d.ioFailed
	body, e := json.Marshal(row)
	if e != nil || len(body)+len(helperDiagnosticPrefix)+1 > helperDiagnosticRowBytes {
		d.invalid = true
		d.dropped++
		return
	}
	line := append(append([]byte(helperDiagnosticPrefix), body...), '\n')
	n, e := d.writer.Write(line)
	d.rows++
	if e != nil || n != len(line) {
		d.ioFailed = true
	}
	if terminal {
		d.finished = true
	}
}
func setHelperStage(ctx context.Context, stage helperStage) {
	if d := helperDiagnosticFor(ctx); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.stage = stage
		d.emitLocked(helperDiagnosticRow{Event: "stage", Result: "entered"}, false)
	}
}
func setHelperSuccess(ctx context.Context) {
	if d := helperDiagnosticFor(ctx); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.success = true
	}
}
func setHelperFallback(ctx context.Context) {
	if d := helperDiagnosticFor(ctx); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.fallback = true
	}
}
func helperCommandName(name string) string {
	switch name {
	case "go-tool-mode-off", "go-environment", "seed-install", "inventory", "coverage", "policy", "realize", "consume-policy":
		return name
	default:
		return "unknown"
	}
}
func recordHelperCommand(ctx context.Context, name string, r commandResult, complete bool) {
	d := helperDiagnosticFor(ctx)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	row := helperDiagnosticRow{Event: "command", Result: "incomplete", Command: helperCommandName(name), CommandObserved: !r.StartedUTC.IsZero(), CommandComplete: complete, ExitCode: r.ExitCode, DurationNanoseconds: r.PhysicalNanoseconds, StdoutBytes: r.StdoutBytes, StderrBytes: r.StderrBytes, Capped: r.Capped, IOFailed: r.IOFailed, GroupKillRequested: r.GroupKillRequested, CleanupObserved: complete, CleanupAdequate: r.Cleanup.Adequate, NaturalZero: r.Cleanup.NaturalZero, FinalZero: r.Cleanup.FinalZero, InitialOwned: r.Cleanup.InitialOwned, FinalOwned: r.Cleanup.FinalOwned, PidfdKillRequests: r.Cleanup.PidfdKillRequests}
	row.InitialScanComplete, row.FinalScanComplete, row.ReapedChildren = r.Cleanup.InitialScanComplete, r.Cleanup.FinalScanComplete, r.Cleanup.ReapedChildren
	row.NamespaceIsolated = r.Cleanup.NamespaceIsolated
	row.ScanStage, row.ScanReason = closedCleanupStage(r.Cleanup.ScanStage), closedCleanupReason(r.Cleanup.ScanReason)
	if row.Command == "unknown" {
		d.invalid = true
	}
	if row.ExitCode < -1 || row.ExitCode > 255 {
		row.ExitCode = -1
		d.invalid = true
	}
	if row.DurationNanoseconds < 0 {
		row.DurationNanoseconds = 0
		d.invalid = true
	}
	if row.StdoutBytes < 0 {
		row.StdoutBytes = 0
		d.invalid = true
	}
	if row.StderrBytes < 0 {
		row.StderrBytes = 0
		d.invalid = true
	}
	if complete {
		row.Result = "failed"
		if commandPassed(r) {
			row.Result = "passed"
		}
	}
	d.emitLocked(row, false)
}

// This MUST be deferred directly: recover never formats or inspects its value.
// An inner registration after defer RemoveAll emits before private cleanup;
// the outer registration covers earlier failures. The same recorder ends once.
func finishHelperDiagnostic(ctx context.Context) {
	cause := recover()
	if d := helperDiagnosticFor(ctx); d != nil {
		d.mu.Lock()
		result := "failed"
		if cause == nil && d.success {
			result = "success"
		} else if cause == nil && d.fallback {
			result = "fallback_selected"
		} else if ctx.Err() == context.DeadlineExceeded {
			result = "deadline"
		} else if ctx.Err() == context.Canceled {
			result = "canceled"
		}
		row := helperDiagnosticRow{Event: "terminal", Result: result}
		if cause != nil {
			row.FailureFile, row.FailureLine, row.FailureSiteObserved = helperFailureLocation()
		}
		d.emitLocked(row, true)
		d.mu.Unlock()
	}
	if cause != nil {
		panic(cause)
	}
}

// This dictionary is local code provenance, not arbitrary stack disclosure.
// The runtime source/overlay hashes bind its numeric line numbers. No File or
// Function string is retained or formatted. must is skipped to find its caller.
func helperFailureFrame(frame runtime.Frame) (uint8, int, bool) {
	function := ""
	for _, prefix := range []string{"main.", module + "/tools/functionalcoverage."} {
		if strings.HasPrefix(frame.Function, prefix) {
			function = strings.TrimPrefix(frame.Function, prefix)
			break
		}
	}
	if function == "" || function == "must" || frame.Line < 1 || frame.Line > 10000 {
		return 0, 0, false
	}
	names := [...]string{"main.go", "execute_linux_amd64.go", "consume.go", "inventory.go", "guard.go", "go_tool_mode.go", "io_boundary.go", "command_boundary.go", "process_linux.go", "exec_unix.go", "owner_unix.go", "producer_marker.go", "namespace_linux_amd64.go", "namespace_protocol.go", "cleanup_diagnostics.go"}
	functions := [...]string{
		"|sum|canonical|readFile|uniqueJSON|decodeJSON|readJSON|writeJSON|writeExclusive|hashFile|readBounded|storePath|sourceManifest|hashes|coverageMember|identify|validateConfig|environment|coverageArgv|commandPassed|effectiveEnvironmentValid|copyTree|validateClosure|closureRoots|runtimeMetadata|bytesReader|safeMain|main|",
		"|backendAvailable|(*boundedLog).Write|runCommand|produce|",
		"|dispatch|scratch|consume|freshConsumptionInventory|requireInventoryMatch|artifactFiles|validateArtifact|validateSafeInventory|",
		"|testName|testSignature|within|discoverInventory|allowedSkip|decodeEvent|validateEvents|position|validateProfile|",
		"|envMap|environmentEligible|defaultGoConfigEligible|workspaceEligible|safeParents|gitEnv|gitArgs|smallGit|(*limitedBuffer).Write|parseIndex|verifyIndexBodies|verifyTree|guard|tryEligibility|validateExport|exportProfile|guardSnapshot.Equal|",
		"|goToolSetupArgv|goEnvironmentArgv|initializeGoToolMode|requireGoToolModeCommands|",
		"|evidenceLocation|openEvidenceRoot|openRootedEvidence|resolveImmutableRead|openEvidenceFile|makeEvidenceDirectory|checkedWriteCount|secureScratchDirectory|",
		"|requirePinnedExecutable|realizeArgv|commandShape|canonicalCommand|gitShape|pinnedGitCommand|legacyPath|",
		"|processEnvironment|ownedMarker|processStart|processKind|scanOwnedScoped|killOwned|observeAndCleanScoped|observeCleanup|reapNamespaceChildren|",
		"|execLegacy|", "|owned|", "|producerSuccessMarker|",
		"|namespaceExitStatus|launchIsolatedConsumer|namespaceParentHandshake|boundedNamespaceMetadata|namespaceStartupValid|namespaceChannel|namespaceChannelValid|consumeIsolated|forwardNamespaceDiagnostics|closedNamespaceDiagnostics|consumerDiagnosticAllowed|",
		"|isolatedConsumer|namespaceArguments|namespaceArgv|readNamespaceToken|writeNamespaceToken|namespaceFallbackAllowed|identityMapValid|namespaceFactsValid|",
		"|(*nativeCleanupError).Error|(*nativeCleanupError).Unwrap|nativeScanFailure|closedCleanupStage|closedCleanupReason|setCleanupFailure|",
	}
	base := function
	if cut := strings.Index(base, ".func"); cut >= 0 {
		suffix := base[cut+5:]
		if suffix == "" {
			return 0, 0, false
		}
		for _, r := range suffix {
			if r != '.' && (r < '0' || r > '9') {
				return 0, 0, false
			}
		}
		base = base[:cut]
	}
	for i, name := range names {
		if strings.HasSuffix(frame.File, "/tools/functionalcoverage/"+name) && strings.Contains(functions[i], "|"+base+"|") {
			return uint8(i + 1), frame.Line, true
		}
	}
	return 0, 0, false
}
func helperFailureLocation() (uint8, int, bool) {
	var pcs [64]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if file, line, ok := helperFailureFrame(frame); ok {
			return file, line, true
		}
		if !more {
			break
		}
	}
	return 0, 0, false
}
