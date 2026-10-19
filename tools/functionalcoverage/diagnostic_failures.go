package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
)

// Diagnostic-only scan AFTER coverage already failed. It cannot turn failure
// into success and never publishes raw Go Output or dynamic subtest names.
func recordKnownCoverageFailures(ctx context.Context, path string, inventory []packageInventory) {
	d := helperDiagnosticFor(ctx)
	if d == nil {
		return
	}
	var lines, failures, reported uint64
	complete := false
	defer func() {
		_ = recover() // Raw parser/IO/panic values are deliberately never inspected.
		d.mu.Lock()
		defer d.mu.Unlock()
		result := "partial"
		if complete {
			result = "complete"
		}
		d.emitLocked(helperDiagnosticRow{Event: "child_fail_summary", Result: result, FailureRecords: failures, FailureRecordsReported: reported, ScanLines: lines, ScanComplete: complete}, false)
	}()
	known := map[string]map[string]bool{}
	for _, p := range inventory {
		roots := map[string]bool{}
		for _, root := range p.Roots {
			roots[root] = true
		}
		known[p.Package] = roots
	}
	f, e := openEvidenceFile(path, os.O_RDONLY, 0)
	must(e)
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	info, e := f.Stat()
	must(e)
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxRaw {
		panic(errContract)
	}
	limited := &io.LimitedReader{R: f, N: maxRaw + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	invalid := false
	for scanner.Scan() {
		lines++
		if lines > 2000000 {
			panic(errContract)
		}
		e := decodeEvent(scanner.Bytes())
		if e.Action != "fail" && e.Action != "build-fail" {
			continue
		}
		failures++
		roots, ok := known[e.Package]
		if !ok || e.Action == "build-fail" || e.FailedBuild != "" {
			invalid = true
			continue
		}
		root := ""
		if e.Test != "" {
			root = strings.SplitN(e.Test, "/", 2)[0]
			if !roots[root] {
				invalid = true
				continue
			}
		}
		if reported >= 8 {
			continue
		}
		row := helperDiagnosticRow{Event: "child_fail", Result: "failed", PackageSHA256: sum([]byte(e.Package))}
		if root != "" {
			row.RootSHA256 = sum([]byte(e.Package + "\x00" + root))
		}
		d.mu.Lock()
		d.emitLocked(row, false)
		d.mu.Unlock()
		reported++
	}
	must(scanner.Err())
	if limited.N == 0 {
		panic(errContract)
	}
	must(f.Close())
	closed = true
	complete = !invalid
}
