//go:build linux && amd64

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func backendAvailable() bool { return true }

type boundedLog struct {
	mu             sync.Mutex
	File           *os.File
	Bytes, Limit   int64
	Failed, Capped bool
	Cancel         context.CancelFunc
}

func (w *boundedLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if int64(len(p)) > w.Limit-w.Bytes {
		w.Capped = true
		w.Cancel()
		return 0, errContract
	}
	n, e := w.File.Write(p)
	w.Bytes += int64(n)
	if e != nil || n != len(p) {
		w.Failed = true
		w.Cancel()
		return n, errContract
	}
	return n, nil
}
func runCommand(ctx context.Context, c config, work, name, exe string, args, env []string, dir string, limit int64) commandResult {
	r := commandResult{Argv: append([]string{exe}, args...)}
	diagnosticComplete := false
	defer func() {
		if !diagnosticComplete {
			recordHelperCommand(ctx, name, r, false)
		}
	}()
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := canonicalCommand(childCtx, c, work, name, exe, args, env, dir)
	a, e := openEvidenceFile(filepath.Join(work, "private-"+name+".stdout"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(e)
	b, e := openEvidenceFile(filepath.Join(work, "private-"+name+".stderr"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		_ = a.Close()
		panic(errContract)
	}
	x, y := &boundedLog{File: a, Limit: limit, Cancel: cancel}, &boundedLog{File: b, Limit: limit, Cancel: cancel}
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = x
	cmd.Stderr = y
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var killed atomic.Bool
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		killed.Store(true)
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if e == syscall.ESRCH {
			return nil
		}
		return e
	}
	cmd.WaitDelay = 2 * time.Second
	r.StartedUTC = time.Now().UTC()
	e = cmd.Run()
	r.FinishedUTC = time.Now().UTC()
	r.PhysicalNanoseconds = r.FinishedUTC.Sub(r.StartedUTC).Nanoseconds()
	if e != nil {
		r.ExitCode = 1
		if exit, ok := e.(*exec.ExitError); ok {
			r.ExitCode = exit.ExitCode()
		}
	}
	ca, cb := a.Close(), b.Close()
	r.Capped = x.Capped || y.Capped
	r.IOFailed = x.Failed || y.Failed || ca != nil || cb != nil
	r.GroupKillRequested = killed.Load()
	r.StdoutBytes, r.StderrBytes = x.Bytes, y.Bytes
	r.StdoutSHA256 = hashFile(filepath.Join(work, "private-"+name+".stdout"), limit)
	r.StderrSHA256 = hashFile(filepath.Join(work, "private-"+name+".stderr"), limit)
	r.Cleanup = observeAndCleanScoped(filepath.Join(work, "tmp"), isolatedConsumer(ctx))
	recordHelperCommand(ctx, name, r, true)
	diagnosticComplete = true
	return r
}
func produce(ctx context.Context, c config) {
	defer finishHelperDiagnostic(ctx)
	setHelperStage(ctx, diagnosticProducerAttributes)
	if c.Legacy != "" {
		panic(errContract)
	}
	// Nix's structured attributes also contain builder bookkeeping; validate the
	// consumed closure/output fields, while not guessing a private Nix schema.
	raw := readFile(os.Getenv("NIX_ATTRS_JSON_FILE"), 16<<20)
	var attrs nixAttributes
	d := json.NewDecoder(bytesReader(raw))
	d.UseNumber()
	uniqueJSON(d, 0)
	must(json.Unmarshal(raw, &attrs))
	if len(attrs.Outputs) != 1 || !storePath(attrs.Outputs["out"]) {
		panic(errContract)
	}
	out := attrs.Outputs["out"]
	setHelperStage(ctx, diagnosticOutputDirectory)
	must(makeEvidenceDirectory(out))
	setHelperStage(ctx, diagnosticScratch)
	work := scratch()
	defer os.RemoveAll(work)
	defer finishHelperDiagnostic(ctx)
	setHelperStage(ctx, diagnosticInputIdentity)
	r := receipt{Schema: schema, Profile: c.Profile, Inputs: identify(c), Commands: map[string]commandResult{}, ProducerStartedUTC: time.Now().UTC(), RuntimeProvenance: runtimeMetadata()}
	r.InputSHA256 = canonical(r.Inputs)
	r.RunnerSHA256 = r.Inputs.HelperSHA256
	r.Closure = attrs.Closure
	setHelperStage(ctx, diagnosticClosure)
	r.ClosureSHA256 = validateClosure(attrs.Closure, closureRoots(c))
	setHelperStage(ctx, diagnosticSelfIdentity)
	self, e := os.Executable()
	must(e)
	if hashFile(self, 128<<20) != r.RunnerSHA256 {
		panic(errContract)
	}
	fixture := filepath.Join(work, "source")
	setHelperStage(ctx, diagnosticFixtureCopy)
	copyTree(c.CoverageSource, fixture)
	copyTree(c.Vendor, filepath.Join(fixture, "vendor"))
	env := environment(c, work)
	setHelperStage(ctx, diagnosticGoSetup)
	initializeGoToolMode(ctx, c, work, fixture, env, r.Commands)
	setHelperStage(ctx, diagnosticSeedInstall)
	r.Commands["seed-install"] = runCommand(ctx, c, work, "seed-install", filepath.Join(c.Seed, "bin/cache-seed"), []string{"install", filepath.Join(c.Seed, "cache"), filepath.Join(work, "go-cache")}, env, fixture, 1<<20)
	if !commandPassed(r.Commands["seed-install"]) {
		panic(errContract)
	}
	setHelperStage(ctx, diagnosticInventory)
	r.Inventory = discoverInventory(ctx, c, work, fixture, env, r.Commands)
	profile := filepath.Join(work, "coverage.out")
	setHelperStage(ctx, diagnosticCoverage)
	r.FunctionalTestsExecuted = true
	r.Commands["coverage"] = runCommand(ctx, c, work, "coverage", c.Go, coverageArgv(profile), env, fixture, maxRaw)
	if !commandPassed(r.Commands["coverage"]) {
		recordKnownCoverageFailures(ctx, filepath.Join(work, "private-coverage.stdout"), r.Inventory)
		panic(errContract)
	}
	setHelperStage(ctx, diagnosticEvents)
	r.Events = validateEvents(filepath.Join(work, "private-coverage.stdout"), r.Inventory, filepath.Join(work, "safe-events.jsonl"))
	setHelperStage(ctx, diagnosticProfile)
	r.ProfileBytes = validateProfile(profile, hashes(r.Inputs.CoverageSource))
	r.ProfileSHA256 = hashFile(profile, maxProfile)
	r.SafeEventsSHA256 = hashFile(filepath.Join(work, "safe-events.jsonl"), maxSafeEvents)
	setHelperStage(ctx, diagnosticPolicy)
	r.Commands["policy"] = runCommand(ctx, c, work, "policy", c.Gawk, []string{"-f", c.Policy, profile}, env, fixture, 1<<20)
	if !commandPassed(r.Commands["policy"]) {
		panic(errContract)
	}
	r.PolicyOutputSHA256 = hashFile(filepath.Join(work, "private-policy.stdout"), 1<<20)
	setHelperStage(ctx, diagnosticFinalIdentity)
	if !reflect.DeepEqual(identify(c), r.Inputs) || !reflect.DeepEqual(sourceManifest(fixture, true), r.Inputs.CoverageSource) || !reflect.DeepEqual(sourceManifest(filepath.Join(fixture, "vendor"), false), r.Inputs.Vendor) || hashFile(self, 128<<20) != r.RunnerSHA256 {
		panic(errContract)
	}
	r.FunctionalAdequate = r.Events.Adequate
	r.Passed = r.FunctionalAdequate
	r.ProducerFinishedUTC = time.Now().UTC()
	if !r.Passed {
		panic(errContract)
	}
	// Raw stdout/stderr are NEVER published. Nix only registers the completed
	// derivation when this helper exits0; consumer independently validates files.
	setHelperStage(ctx, diagnosticPublishProfile)
	writeExclusive(filepath.Join(out, "coverage.out"), readFile(profile, maxProfile), 0644)
	setHelperStage(ctx, diagnosticPublishEvents)
	writeExclusive(filepath.Join(out, "events.jsonl"), readFile(filepath.Join(work, "safe-events.jsonl"), maxSafeEvents), 0644)
	setHelperStage(ctx, diagnosticPublishSummary)
	writeExclusive(filepath.Join(out, "coverage-summary.txt"), readFile(filepath.Join(work, "private-policy.stdout"), 1<<20), 0644)
	setHelperStage(ctx, diagnosticPublishReceipt)
	writeJSON(filepath.Join(out, "receipt.json"), r)
	// Nix registration canonicalizes public store file/directory modes; there is
	// no need to broaden private build permissions before successful publication.
	setHelperStage(ctx, diagnosticSuccessMarker)
	marker := producerSuccessMarker(r)
	n, e := fmt.Printf("FUNCTIONAL_COVERAGE_PRODUCER_COMPLETE=%s\n", marker)
	must(e)
	checkedWriteCount(n, len("FUNCTIONAL_COVERAGE_PRODUCER_COMPLETE=")+len(marker)+1)
	setHelperStage(ctx, diagnosticComplete)
	setHelperSuccess(ctx)
}
