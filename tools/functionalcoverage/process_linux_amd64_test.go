//go:build linux && amd64

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFunctionalCoverageExactOwnedNativeMarker(t *testing.T) {
	marker := "/build/unique-owned/tmp"
	if !ownedMarker([]byte("A=B\x00TMPDIR="+marker+"\x00"), marker) {
		t.Fatal("owned marker missing")
	}
	for _, body := range []string{"TMPDIR=" + marker, "TMPDIR=" + marker + "-foreign\x00", "TMPDIR=" + marker + "\x00TMPDIR=" + marker + "\x00", "TOKEN=private\x00"} {
		if ownedMarker([]byte(body), marker) {
			t.Fatal("foreign marker accepted")
		}
	}
	if value, e := processStart("123 (safe tricky) name) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19"); e != nil || value != "19" {
		t.Fatal("start-time binding failed")
	}
}

// Use synthetic bytes only: both bounded success and opening/oversize failures
// must retain the closed observation stage before generic classification.
func TestFunctionalCoverageSyntheticEnvironmentReadIsBoundedAndClassified(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "synthetic-environment")
	body := []byte("TMPDIR=/synthetic/owned\x00PRIVATE_SENTINEL=never-log\x00")
	if os.WriteFile(path, body, 0600) != nil {
		t.Fatal("environment fixture")
	}
	got, e := processEnvironment(path)
	if e != nil || !bytes.Equal(got, body) {
		t.Fatal("bounded synthetic read failed")
	}
	if os.WriteFile(path, bytes.Repeat([]byte{'x'}, (1<<20)+1), 0600) != nil {
		t.Fatal("oversize fixture")
	}
	if _, e := processEnvironment(path); e == nil {
		t.Fatal("oversized environment accepted")
	} else {
		var r cleanupObservation
		setCleanupFailure(&r, e)
		if r.ScanStage != "environment_bound" || r.ScanReason != "invalid" || strings.Contains(e.Error(), root) {
			t.Fatal("bounded read class/privacy lost")
		}
	}
	if os.Remove(path) != nil {
		t.Fatal("remove fixture")
	}
	if _, e := processEnvironment(path); e == nil {
		t.Fatal("missing environment accepted")
	} else {
		var r cleanupObservation
		setCleanupFailure(&r, e)
		if r.ScanStage != "environment_open" || r.ScanReason != "not_found" || strings.Contains(e.Error(), root) {
			t.Fatal("open failure class/privacy lost")
		}
	}
}
