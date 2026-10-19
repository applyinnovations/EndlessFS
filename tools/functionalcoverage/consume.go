package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type consumeActions struct {
	Context            context.Context
	Eligible           func() bool
	Fallback           func() int
	RealizeAndValidate func() int
}

// There is one irrevocable boundary, entirely inside this process. Neither an
// outer nix-run status nor a child's arbitrary status can select fallback.
func dispatch(a consumeActions) int {
	canceled := func() bool { return a.Context != nil && a.Context.Err() != nil }
	if canceled() {
		return 1
	}
	eligible := a.Eligible()
	if canceled() {
		return 1
	}
	if !eligible {
		if canceled() {
			return 1
		}
		return a.Fallback()
	}
	if canceled() {
		return 1
	}
	return a.RealizeAndValidate()
}
func scratch() string {
	root, e := os.MkdirTemp("", "endlessfs-functional-")
	must(e)
	must(secureScratchDirectory(root))
	for _, name := range []string{"home/.config", "xdg-cache", "runtime", "tmp", "go-cache", "go-mod-cache"} {
		must(os.MkdirAll(filepath.Join(root, name), 0700))
	}
	return root
}
func consume(ctx context.Context, c config, args []string) int {
	defer finishHelperDiagnostic(ctx)
	if ctx.Err() != nil {
		return 1
	}
	if !storePath(c.Legacy) {
		panic(errContract)
	}
	expectedConfig := c
	expectedConfig.Legacy = ""
	setHelperStage(ctx, diagnosticInputIdentity)
	expected := identify(expectedConfig)
	env := os.Environ()
	export := envMap(env)["ENDLESSFS_COVERAGE_PROFILE"]
	var before guardSnapshot
	return dispatch(consumeActions{
		Context: ctx,
		Eligible: func() bool {
			setHelperStage(ctx, diagnosticEligibility)
			var ok bool
			before, ok = tryEligibility(func() guardSnapshot {
				s := guard(ctx, c, expected.FullSource, env, args)
				validateExport(export, s.Root)
				return s
			})
			return ok
		},
		Fallback: func() int {
			if ctx.Err() != nil || isolatedConsumer(ctx) {
				return 1
			}
			setHelperStage(ctx, diagnosticFallback)
			setHelperFallback(ctx)
			finishHelperDiagnostic(ctx)
			fmt.Fprintln(os.Stderr, "functional coverage: unsupported_local_profile_using_original_fresh_path")
			return execLegacy(c.Legacy, args, env)
		},
		RealizeAndValidate: func() int {
			if ctx.Err() != nil {
				return 1
			}
			if !isolatedConsumer(ctx) {
				setHelperStage(ctx, diagnosticNamespaceLaunch)
				status, fallback := launchIsolatedConsumer(ctx, c, args, before.Root)
				if fallback {
					setHelperStage(ctx, diagnosticFallback)
					setHelperFallback(ctx)
					finishHelperDiagnostic(ctx)
					fmt.Fprintln(os.Stderr, "functional coverage: unsupported_local_profile_using_original_fresh_path")
					return execLegacy(c.Legacy, args, env)
				}
				if status == 0 {
					setHelperStage(ctx, diagnosticFinalGuard)
					if !before.Equal(guard(ctx, c, expected.FullSource, os.Environ(), args)) {
						panic(errContract)
					}
					setHelperStage(ctx, diagnosticComplete)
					setHelperSuccess(ctx)
				}
				return status
			}
			// Any panic/error from this point is handled by safeMain as nonzero. Never
			// call legacy after attempting Nix realization, even when a child exits125.
			setHelperStage(ctx, diagnosticScratch)
			work := scratch()
			defer os.RemoveAll(work)
			defer finishHelperDiagnostic(ctx)
			childEnv := environment(c, work)
			setHelperStage(ctx, diagnosticRealize)
			command := runCommand(ctx, c, work, "realize", c.Nix, realizeArgv(c), childEnv, work, maxRaw)
			if !commandPassed(command) {
				panic(errContract)
			}
			setHelperStage(ctx, diagnosticRealizeReply)
			var result []struct {
				DrvPath string            `json:"drvPath"`
				Outputs map[string]string `json:"outputs"`
			}
			readJSON(filepath.Join(work, "private-realize.stdout"), 1<<20, &result)
			if len(result) != 1 || !storePath(result[0].DrvPath) || len(result[0].Outputs) != 1 || !storePath(result[0].Outputs["out"]) {
				panic(errContract)
			}
			inventoryStart := time.Now()
			setHelperStage(ctx, diagnosticConsumerInventory)
			freshInventory := freshConsumptionInventory(ctx, c, work, expected)
			inventoryNanoseconds := time.Since(inventoryStart).Nanoseconds()
			out := result[0].Outputs["out"]
			setHelperStage(ctx, diagnosticArtifact)
			r := validateArtifact(out, expected, freshInventory)
			// Current floor policy is executed on EVERY consumption, including hits.
			setHelperStage(ctx, diagnosticConsumerPolicy)
			policy := runCommand(ctx, c, work, "consume-policy", c.Gawk, []string{"-f", c.Policy, filepath.Join(out, "coverage.out")}, childEnv, work, 1<<20)
			if !commandPassed(policy) {
				panic(errContract)
			}
			setHelperStage(ctx, diagnosticFinalIdentity)
			if !reflect.DeepEqual(identify(expectedConfig), expected) {
				panic(errContract)
			}
			setHelperStage(ctx, diagnosticFinalGuard)
			after := guard(ctx, c, expected.FullSource, os.Environ(), args)
			if !before.Equal(after) {
				panic(errContract)
			}
			validateExport(export, before.Root)
			setHelperStage(ctx, diagnosticExport)
			exportProfile(export, before.Root, readFile(filepath.Join(out, "coverage.out"), maxProfile))
			fmt.Printf("REUSED canonical functional coverage; producer_start=%s producer_finish=%s recorded_validation_window_ns=%d current_inventory_metadata_ns=%d; fresh_browser=false resource_qualified=false physical_replica_qualified=false\n", r.ProducerStartedUTC.Format("2006-01-02T15:04:05.999999999Z07:00"), r.ProducerFinishedUTC.Format("2006-01-02T15:04:05.999999999Z07:00"), r.ProducerFinishedUTC.Sub(r.ProducerStartedUTC).Nanoseconds(), inventoryNanoseconds)
			setHelperStage(ctx, diagnosticComplete)
			setHelperSuccess(ctx)
			return 0
		},
	})
}
func freshConsumptionInventory(ctx context.Context, c config, work string, expected inputIdentity) []packageInventory {
	if ctx.Err() != nil {
		panic(errContract)
	}
	source := filepath.Join(work, "inventory-source")
	copyTree(c.CoverageSource, source)
	copyTree(c.Vendor, filepath.Join(source, "vendor"))
	if !reflect.DeepEqual(sourceManifest(source, true), expected.CoverageSource) || !reflect.DeepEqual(sourceManifest(filepath.Join(source, "vendor"), false), expected.Vendor) {
		panic(errContract)
	}
	commands := map[string]commandResult{}
	env := environment(c, work)
	initializeGoToolMode(ctx, c, work, source, env, commands)
	inventory := discoverInventory(ctx, c, work, source, env, commands)
	if !commandPassed(commands["inventory"]) || ctx.Err() != nil || !reflect.DeepEqual(sourceManifest(source, true), expected.CoverageSource) || !reflect.DeepEqual(sourceManifest(filepath.Join(source, "vendor"), false), expected.Vendor) {
		panic(errContract)
	}
	return inventory
}
func requireInventoryMatch(fresh, recorded []packageInventory) {
	if len(fresh) == 0 || !reflect.DeepEqual(fresh, recorded) {
		panic(errContract)
	}
	found := false
	for _, p := range fresh {
		if p.Package == module+"/internal/e2e" {
			for _, root := range p.Roots {
				if root == "TestE2ESmartRecoveryProductionCardinalityWarmTransfersOnlyRemainingFiles" {
					found = true
				}
			}
		}
	}
	if !found {
		panic(errContract)
	}
}
func artifactFiles(out string) {
	want := map[string]bool{"receipt.json": true, "coverage.out": true, "coverage-summary.txt": true, "events.jsonl": true}
	entries, e := os.ReadDir(out)
	must(e)
	if len(entries) != len(want) {
		panic(errContract)
	}
	for _, entry := range entries {
		if !want[entry.Name()] {
			panic(errContract)
		}
		info, e := entry.Info()
		must(e)
		if !info.Mode().IsRegular() {
			panic(errContract)
		}
	}
}
func validateArtifact(out string, expected inputIdentity, freshInventory []packageInventory) receipt {
	artifactFiles(out)
	var r receipt
	readJSON(filepath.Join(out, "receipt.json"), 4<<20, &r)
	if r.Schema != schema || r.Profile != expected.Profile || !r.Passed || !r.FunctionalAdequate || !r.FunctionalTestsExecuted || r.InputSHA256 != canonical(expected) || !reflect.DeepEqual(r.Inputs, expected) || r.RunnerSHA256 != expected.HelperSHA256 {
		panic(errContract)
	}
	requireInventoryMatch(freshInventory, r.Inventory)
	if r.ResourceProfileEligible || r.PerformanceQualified || r.ReleaseQualified || r.FreshPhysicalReplicaQualified || r.RuntimeProvenance.ResourceReason != "functional_only_resource_profile_not_qualified" {
		panic(errContract)
	}
	if r.ProducerStartedUTC.IsZero() || !r.ProducerFinishedUTC.After(r.ProducerStartedUTC) {
		panic(errContract)
	}
	if validateClosure(r.Closure, closureRoots(expected.Config)) != r.ClosureSHA256 {
		panic(errContract)
	}
	if len(r.Commands) != 6 {
		panic(errContract)
	}
	for _, name := range []string{"go-tool-mode-off", "go-environment", "seed-install", "inventory", "coverage", "policy"} {
		if !commandPassed(r.Commands[name]) {
			panic(errContract)
		}
	}
	requireGoToolModeCommands(expected.Config, r.Commands)
	coverage := r.Commands["coverage"]
	if len(coverage.Argv) != 9 || coverage.Argv[0] != expected.Config.Go || !strings.HasPrefix(coverage.Argv[7], "-coverprofile=") {
		panic(errContract)
	}
	want := append([]string{expected.Config.Go}, coverageArgv(strings.TrimPrefix(coverage.Argv[7], "-coverprofile="))...)
	if !reflect.DeepEqual(coverage.Argv, want) {
		panic(errContract)
	}
	for _, p := range r.Inventory {
		prefix := strings.TrimPrefix(p.Package, module+"/")
		if prefix == p.Package || len(p.TestFiles) > 10000 {
			panic(errContract)
		}
		for name, h := range p.TestFiles {
			if filepath.Base(name) != name || expected.CoverageSource[prefix+"/"+name].SHA256 != h {
				panic(errContract)
			}
		}
	}
	if !r.Events.Adequate || r.Events.AllowedSkips != 2 || r.Events.Packages != uint64(len(r.Inventory)) || r.ProfileBytes != validateProfile(filepath.Join(out, "coverage.out"), hashes(expected.CoverageSource)) {
		panic(errContract)
	}
	if hashFile(filepath.Join(out, "coverage.out"), maxProfile) != r.ProfileSHA256 || hashFile(filepath.Join(out, "events.jsonl"), maxSafeEvents) != r.SafeEventsSHA256 || r.SafeEventsSHA256 != r.Events.SafeInventorySHA256 || hashFile(filepath.Join(out, "coverage-summary.txt"), 1<<20) != r.PolicyOutputSHA256 {
		panic(errContract)
	}
	validateSafeInventory(filepath.Join(out, "events.jsonl"), r.Inventory, r.Events)
	return r
}
func validateSafeInventory(path string, inventory []packageInventory, wantSummary eventSummary) {
	type node struct {
		Parent, Root string
		Paused, Done bool
	}
	type pkg struct {
		Done  bool
		Nodes map[string]*node
		Roots map[string]bool
	}
	states := map[string]*pkg{}
	for _, p := range inventory {
		if states[p.Package] != nil {
			panic(errContract)
		}
		state := &pkg{Nodes: map[string]*node{}, Roots: map[string]bool{}}
		for _, root := range p.Roots {
			if state.Roots[root] {
				panic(errContract)
			}
			state.Roots[root] = true
		}
		states[p.Package] = state
	}
	f, e := openEvidenceFile(path, os.O_RDONLY, 0)
	must(e)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	var previous, records, nodes, roots, skips, packages uint64
	started := map[string]bool{}
	for scanner.Scan() {
		var row safeEvent
		decodeJSON(scanner.Bytes(), &row)
		records++
		if records > 2000000 || row.Ordinal <= previous {
			panic(errContract)
		}
		previous = row.Ordinal
		p := states[row.Package]
		if p == nil || p.Done || row.GoJSONElapsed < 0 {
			panic(errContract)
		}
		if row.Root == "" {
			if row.ParentSHA256 != "" || row.NodeSHA256 != sum([]byte(row.Package+"\x00")) {
				panic(errContract)
			}
			switch row.Action {
			case "start":
				if started[row.Package] {
					panic(errContract)
				}
				started[row.Package] = true
			case "pass", "skip":
				if !started[row.Package] || row.Action == "skip" && len(p.Roots) > 0 {
					panic(errContract)
				}
				for _, n := range p.Nodes {
					if !n.Done {
						panic(errContract)
					}
				}
				for root := range p.Roots {
					if n := p.Nodes[sum([]byte(row.Package+"\x00"+root))]; n == nil || !n.Done {
						panic(errContract)
					}
				}
				p.Done = true
				packages++
			default:
				panic(errContract)
			}
			continue
		}
		if !started[row.Package] || !p.Roots[row.Root] || len(row.NodeSHA256) != 64 {
			panic(errContract)
		}
		n := p.Nodes[row.NodeSHA256]
		isRoot := row.NodeSHA256 == sum([]byte(row.Package+"\x00"+row.Root))
		switch row.Action {
		case "run":
			if n != nil {
				panic(errContract)
			}
			if isRoot {
				if row.ParentSHA256 != "" {
					panic(errContract)
				}
				roots++
			} else {
				parent := p.Nodes[row.ParentSHA256]
				if parent == nil || parent.Done || parent.Paused || parent.Root != row.Root {
					panic(errContract)
				}
			}
			p.Nodes[row.NodeSHA256] = &node{Parent: row.ParentSHA256, Root: row.Root}
			nodes++
		case "pause":
			if n == nil || n.Done || n.Paused {
				panic(errContract)
			}
			n.Paused = true
		case "cont":
			if n == nil || n.Done || !n.Paused {
				panic(errContract)
			}
			n.Paused = false
		case "pass", "skip":
			if n == nil || n.Done || n.Paused {
				panic(errContract)
			}
			for _, child := range p.Nodes {
				if child.Parent == row.NodeSHA256 && !child.Done {
					panic(errContract)
				}
			}
			if row.Action == "skip" {
				if !isRoot || !allowedSkip(row.Package, row.Root) {
					panic(errContract)
				}
				skips++
			}
			n.Done = true
		default:
			panic(errContract)
		}
	}
	must(scanner.Err())
	for _, p := range states {
		if !p.Done {
			panic(errContract)
		}
	}
	if records != wantSummary.LifecycleRecords || nodes != wantSummary.Nodes || roots != wantSummary.Roots || skips != wantSummary.AllowedSkips || packages != wantSummary.Packages {
		panic(errContract)
	}
}
