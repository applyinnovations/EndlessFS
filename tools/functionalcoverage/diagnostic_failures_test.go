package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFunctionalCoverageDiagnosticFailureSiteIsClosedAndActual(t *testing.T) {
	file, line, ok := helperFailureFrame(runtime.Frame{File: module + "/tools/functionalcoverage/inventory.go", Function: "main.validateEvents", Line: 344})
	if !ok || file != 4 || line != 344 {
		t.Fatal("closed source site lost")
	}
	for _, f := range []runtime.Frame{
		{File: "/PRIVATE/path.go", Function: "main.validateEvents", Line: 1},
		{File: module + "/tools/functionalcoverage/inventory.go", Function: "PRIVATE.secret", Line: 1},
		{File: module + "/tools/functionalcoverage/main.go", Function: "main.must", Line: 1},
		{File: module + "/tools/functionalcoverage/inventory.go", Function: "main.validateEvents", Line: 10001},
	} {
		if file, line, ok := helperFailureFrame(f); ok || file != 0 || line != 0 {
			t.Fatal("arbitrary source frame admitted")
		}
	}
	var output bytes.Buffer
	ctx := newHelperDiagnostic(context.Background(), &output, "produce")
	path := filepath.Join(t.TempDir(), "PRIVATE-profile-name")
	if e := os.WriteFile(path, []byte("invalid-header\n"), 0600); e != nil {
		t.Fatal(e)
	}
	caught := false
	func() {
		defer func() { caught = recover() != nil }()
		func() {
			defer finishHelperDiagnostic(ctx)
			setHelperStage(ctx, diagnosticProfile)
			validateProfile(path, map[string]string{})
		}()
	}()
	if !caught {
		t.Fatal("real malformed profile unexpectedly valid")
	}
	rows := diagnosticRows(t, output.Bytes())
	last := rows[len(rows)-1]
	if !last.FailureSiteObserved || last.FailureFile != 4 || last.FailureLine == 0 || last.Stage != "profile_validation" || last.Result != "failed" {
		t.Fatal("actual strict validator site not captured")
	}
	if bytes.Contains(output.Bytes(), []byte("PRIVATE-profile-name")) {
		t.Fatal("runtime path escaped source projection")
	}
}
func TestFunctionalCoverageDiagnosticKnownFailuresHashOnlyStaticRoots(t *testing.T) {
	pkg := module + "/internal/e2e"
	root := "TestStaticRoot"
	secret := "PRIVATE_SENTINEL/path/user/cap/body"
	var raw bytes.Buffer
	for i := 0; i < 10; i++ {
		b, e := json.Marshal(map[string]any{"Time": "2026-10-19T00:00:00Z", "Action": "fail", "Package": pkg, "Test": root + "/" + secret, "Elapsed": 1})
		if e != nil {
			t.Fatal(e)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "raw.jsonl")
	if e := os.WriteFile(path, raw.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	var output bytes.Buffer
	ctx := newHelperDiagnostic(context.Background(), &output, "produce")
	recordKnownCoverageFailures(ctx, path, []packageInventory{{Package: pkg, Roots: []string{root}}})
	rows := diagnosticRows(t, output.Bytes())
	if len(rows) != 9 {
		t.Fatal("failure record cap not enforced")
	}
	for _, r := range rows[:8] {
		if r.Event != "child_fail" || r.PackageSHA256 != sum([]byte(pkg)) || r.RootSHA256 != sum([]byte(pkg+"\x00"+root)) {
			t.Fatal("known static membership digest incorrect")
		}
	}
	last := rows[len(rows)-1]
	if !last.ScanComplete || last.FailureRecords != 10 || last.FailureRecordsReported != 8 || last.ScanLines != 10 {
		t.Fatal("failure scan omission/accounting hidden")
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), pkg) || strings.Contains(output.String(), root) {
		t.Fatal("raw child/package/root information escaped")
	}
	var partial bytes.Buffer
	ctx = newHelperDiagnostic(context.Background(), &partial, "produce")
	recordKnownCoverageFailures(ctx, path, nil)
	rows = diagnosticRows(t, partial.Bytes())
	if len(rows) != 1 || rows[0].ScanComplete || rows[0].FailureRecordsReported != 0 || rows[0].Result != "partial" {
		t.Fatal("unknown inventory credited as complete")
	}
	// Go1.26 can attach FailedBuild to a normal package-fail event.
	// Its arbitrary value is neither an inventory identity nor safe output.
	failedBuild, e := json.Marshal(map[string]any{"Time": "2026-10-19T00:00:00Z", "Action": "fail", "Package": pkg, "FailedBuild": secret})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, append(failedBuild, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	var buildOutput bytes.Buffer
	ctx = newHelperDiagnostic(context.Background(), &buildOutput, "produce")
	recordKnownCoverageFailures(ctx, path, []packageInventory{{Package: pkg, Roots: []string{root}}})
	rows = diagnosticRows(t, buildOutput.Bytes())
	if len(rows) != 1 || rows[0].ScanComplete || rows[0].FailureRecords != 1 || rows[0].FailureRecordsReported != 0 || rows[0].Result != "partial" {
		t.Fatal("build failure credited as a complete static test scan")
	}
	if strings.Contains(buildOutput.String(), secret) {
		t.Fatal("private build identity escaped")
	}
}
