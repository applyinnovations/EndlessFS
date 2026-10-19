package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// This regression uses only the ORIGINAL API so it can run red against frozen
// f43. It performs no Go/Nix child work and never opens a private config body.
// Copy this file alone for the before run; the original emits only a generic
// failed_closed line, while the proposed version emits a closed arguments stage.
func TestFunctionalCoverageFailureHasClosedStageRegression(t *testing.T) {
	originalArgs, originalStderr := os.Args, os.Stderr
	capture, e := os.OpenFile(filepath.Join(t.TempDir(), "closed-diagnostic.log"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { os.Args = originalArgs; os.Stderr = originalStderr; _ = capture.Close() }()
	sentinel := "PRIVATE_SENTINEL/path/user/token/capability/body"
	os.Args = []string{"functionalcoverage", "produce", "/not-store/" + sentinel}
	os.Stderr = capture
	code := safeMain()
	os.Args = originalArgs
	os.Stderr = originalStderr
	if code != 1 {
		t.Fatal("invalid invocation changed failure semantics")
	}
	if e = capture.Close(); e != nil {
		t.Fatal(e)
	}
	body, e := os.ReadFile(capture.Name())
	if e != nil {
		t.Fatal(e)
	}
	if len(body) > 8<<10 || bytes.Contains(body, []byte(sentinel)) {
		t.Fatal("private caller text or unbounded diagnostics escaped")
	}
	for _, required := range []string{"FUNCTIONAL_COVERAGE_DIAGNOSTIC=", `"role":"produce"`, `"stage":"arguments"`, `"event":"terminal"`, `"result":"failed"`} {
		if !bytes.Contains(body, []byte(required)) {
			t.Fatal("failure lacks safe closed stage evidence")
		}
	}
}
