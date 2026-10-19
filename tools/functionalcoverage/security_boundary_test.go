//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFunctionalCoverageRootedFileSelectionAndStreams(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "proof")
	writeExclusive(file, []byte("bounded-proof"), 0600)
	if string(readFile(file, 64)) != "bounded-proof" || hashFile(file, 64) != sum([]byte("bounded-proof")) {
		t.Fatal("valid bounded file read changed")
	}
	if got, e := readBounded(file, 64); e != nil || got != "bounded-proof" {
		t.Fatal("bounded stream changed")
	}
	expectContract(t, func() { readFile(file, 2) })
	expectContract(t, func() { hashFile(file, 2) })
	if _, e := readBounded(file, 2); e == nil {
		t.Fatal("oversize stream admitted")
	}
	expectContract(t, func() { writeExclusive(file, []byte("replacement"), 0600) })
	if string(readFile(file, 64)) != "bounded-proof" {
		t.Fatal("exclusive write changed existing file")
	}
	dir := filepath.Join(base, "directory")
	if e := makeEvidenceDirectory(dir); e != nil {
		t.Fatal(e)
	}
	if e := makeEvidenceDirectory(dir); e == nil {
		t.Fatal("existing directory silently accepted")
	}
	for _, path := range []string{"relative", "/", base + "/../escape", "/proc/self/environ", "/proc/001/environ", "/proc/1/root", "/proc/1/stat/extra"} {
		if _, _, e := evidenceLocation(path); e == nil {
			t.Fatal("invalid path selection admitted")
		}
	}
	if root, name, e := evidenceLocation("/proc/123/environ"); e != nil || root != "/proc" || name != "123/environ" {
		t.Fatal("fixed positive PID member rejected")
	}
	store := "/nix/store/" + strings.Repeat("0", 32) + "-go"
	if root, name, e := evidenceLocation(store + "/bin/go"); e != nil || root != store || name != "bin/go" {
		t.Fatal("store tool root narrowed to bin")
	}
	config := store + ".json"
	if root, name, e := evidenceLocation(config); e != nil || root != "/nix/store" || name != filepath.Base(config) {
		t.Fatal("top-level immutable config rejected")
	}
	link := filepath.Join(base, "link")
	if e := os.Symlink(file, link); e != nil {
		t.Fatal(e)
	}
	expectContract(t, func() { readFile(link, 64) })
	ancestor := filepath.Join(base, "alias")
	if e := os.Symlink(dir, ancestor); e != nil {
		t.Fatal(e)
	}
	if _, _, e := evidenceLocation(filepath.Join(ancestor, "new")); e == nil {
		t.Fatal("symlink ancestor admitted")
	}
	fifo := filepath.Join(base, "fifo")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	if f, e := openEvidenceFile(fifo, os.O_RDONLY, 0); e == nil {
		_ = f.Close()
		t.Fatal("special stream opened")
	}
	if _, _, e := evidenceLocation(filepath.Join(base, "nul\x00name")); e == nil {
		t.Fatal("NUL selection admitted")
	}
}
func TestFunctionalCoverageOpenedRootBoundsAndOwnedToolLinks(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	if e := os.MkdirAll(filepath.Join(inside, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(filepath.Join(inside, "share/go/bin"), 0700); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(inside, "share/go/bin/go")
	if e := os.WriteFile(target, []byte("owned-tool"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("../share/go/bin/go", filepath.Join(inside, "bin/go")); e != nil {
		t.Fatal(e)
	}
	root, e := os.OpenRoot(inside)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	f, e := openRootedEvidence(root, "bin/go", os.O_RDONLY, 0, true)
	if e != nil {
		t.Fatal("legitimate internal store-style link denied")
	}
	body, e := io.ReadAll(f)
	closeErr := f.Close()
	if e != nil || closeErr != nil || string(body) != "owned-tool" {
		t.Fatal("linked tool bytes changed")
	}
	if f, e := openRootedEvidence(root, "bin/go", os.O_RDONLY, 0, false); e == nil {
		_ = f.Close()
		t.Fatal("local evidence symlink admitted")
	}
	outside := filepath.Join(base, "outside")
	if e := os.WriteFile(outside, []byte("private-sentinel"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("../../outside", filepath.Join(inside, "bin/out")); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"../outside", "/outside", "bin/out", ".", "bin/../bin/go"} {
		if f, e := openRootedEvidence(root, name, os.O_RDONLY, 0, true); e == nil {
			_ = f.Close()
			t.Fatal("root escape/noncanonical child admitted")
		}
	}
	for _, row := range []struct {
		flag int
		mode os.FileMode
	}{{os.O_WRONLY | os.O_TRUNC, 0600}, {os.O_CREATE | os.O_EXCL | os.O_WRONLY, 0666}} {
		if f, e := openRootedEvidence(root, "new", row.flag, row.mode, false); e == nil {
			_ = f.Close()
			t.Fatal("unsafe write admitted")
		}
	}
	moved := filepath.Join(base, "moved")
	if e := os.Rename(inside, moved); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(inside, 0700); e != nil {
		t.Fatal(e)
	}
	f, e = openRootedEvidence(root, "proof", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write([]byte("old-root")); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(moved, "proof")); e != nil {
		t.Fatal("opened root lost inode identity")
	}
	if _, e := os.Stat(filepath.Join(inside, "proof")); !os.IsNotExist(e) {
		t.Fatal("write followed replacement path")
	}
}
func TestFunctionalCoverageCheckedWriteCountDeniesInvalidWriter(t *testing.T) {
	if checkedWriteCount(4, 4) != 4 || checkedWriteCount(0, 0) != 0 {
		t.Fatal("valid writer count changed")
	}
	for _, row := range [][2]int{{-1, 4}, {3, 4}, {5, 4}} {
		expectContract(t, func() { checkedWriteCount(row[0], row[1]) })
	}
}
func TestFunctionalCoveragePrivateDirectoryModeRepairsRestrictiveCreation(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "scratch")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(dir, 0000); e != nil {
		t.Fatal(e)
	}
	if e := secureScratchDirectory(dir); e != nil {
		t.Fatal(e)
	}
	info, e := os.Lstat(dir)
	if e != nil || info.Mode().Perm() != 0700 {
		t.Fatal("owner-only directory mode not restored")
	}
	file := filepath.Join(base, "file")
	if e := os.WriteFile(file, []byte("private"), 0600); e != nil {
		t.Fatal(e)
	}
	if secureScratchDirectory(file) == nil {
		t.Fatal("regular file treated as scratch directory")
	}
	link := filepath.Join(base, "link")
	if e := os.Symlink(dir, link); e != nil {
		t.Fatal(e)
	}
	if secureScratchDirectory(link) == nil || secureScratchDirectory("relative") == nil {
		t.Fatal("unbound scratch directory admitted")
	}
}
func TestFunctionalCoveragePinnedCommandShapesAreClosed(t *testing.T) {
	c := config{Go: "/pinned/bin/go", Seed: "/pinned/seed", Gawk: "/pinned/bin/gawk", Policy: "/pinned/source/tools/coverage.awk", Nix: "/pinned/bin/nix", ProducerInstallable: "fixed-producer"}
	work := "/owned/scratch"
	rows := []struct {
		name, exe string
		args      []string
	}{
		{"go-tool-mode-off", c.Go, goToolSetupArgv()}, {"go-environment", c.Go, goEnvironmentArgv()}, {"inventory", c.Go, []string{"list", "-json", "./..."}}, {"coverage", c.Go, coverageArgv(filepath.Join(work, "coverage.out"))},
		{"seed-install", filepath.Join(c.Seed, "bin/cache-seed"), []string{"install", filepath.Join(c.Seed, "cache"), filepath.Join(work, "go-cache")}}, {"policy", c.Gawk, []string{"-f", c.Policy, filepath.Join(work, "coverage.out")}}, {"realize", c.Nix, []string{"build", "--extra-experimental-features", "nix-command flakes", "--offline", "--no-write-lock-file", "--no-link", "--json", c.ProducerInstallable}},
		{"consume-policy", c.Gawk, []string{"-f", c.Policy, "/nix/store/" + strings.Repeat("0", 32) + "-artifact/coverage.out"}},
	}
	for _, row := range rows {
		if !commandShape(c, work, row.name, row.exe, row.args) {
			t.Fatal("canonical command denied")
		}
		if commandShape(c, work, row.name, "/bin/sh", row.args) || commandShape(c, work, "unknown", row.exe, row.args) || commandShape(c, work, row.name, row.exe, append(append([]string{}, row.args...), "--injected")) {
			t.Fatal("unbound command admitted")
		}
	}
	if commandShape(c, work, "realize", c.Nix, []string{"build", "--offline", "--no-write-lock-file", "--no-link", "--json", c.ProducerInstallable}) || commandShape(c, work, "realize", c.Nix, []string{"build", "--extra-experimental-features", "nix-command flakes impure-derivations", "--offline", "--no-write-lock-file", "--no-link", "--json", c.ProducerInstallable}) {
		t.Fatal("missing or broadened private Nix features admitted")
	}
	for _, args := range [][]string{{"rev-parse", "--show-toplevel"}, {"ls-files", "--stage", "--full-name", "-z"}, {"cat-file", "--batch"}} {
		if !gitShape(args) || gitShape(append(args, "--filters")) {
			t.Fatal("Git metadata selector boundary incorrect")
		}
	}
	prefix := "/nix/store/" + strings.Repeat("0", 32) + "-fresh/bin/"
	if !legacyPath(prefix+"endlessfs-test-coverage-fresh") || legacyPath("/bin/sh") || legacyPath(prefix+"sh") {
		t.Fatal("legacy executable selection incorrect")
	}
	expectContract(t, func() { requirePinnedExecutable("/bin/sh") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expectContract(t, func() {
		canonicalCommand(ctx, c, work, "inventory", c.Go, []string{"list", "-json", "./..."}, environment(c, work), work)
	})
	expectContract(t, func() {
		canonicalCommand(context.Background(), c, work, "inventory", c.Go, []string{"test", "-run", "subset"}, environment(c, work), work)
	})
}
func TestFunctionalCoverageSuccessMarkerHasOnlyBoundFields(t *testing.T) {
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	base := receipt{Passed: true, FunctionalAdequate: true, FunctionalTestsExecuted: true, InputSHA256: strings.Repeat("a", 64), RunnerSHA256: strings.Repeat("b", 64), ProducerStartedUTC: start, ProducerFinishedUTC: start.Add(time.Second), Events: eventSummary{Adequate: true, Packages: 2, Roots: 3, Nodes: 4, AllowedSkips: 2}, ProfileSHA256: "private-sentinel", Inputs: inputIdentity{Config: config{Legacy: "private-sentinel"}}}
	body := producerSuccessMarker(base)
	var marker producerMarker
	decodeJSON(body, &marker)
	if len(body) > 2048 || strings.Contains(string(body), "private-sentinel") || marker.Schema != "endlessfs-functional-producer-success-v1" || !marker.FreshTests || marker.ResourceQualified || marker.PerformanceQualified || marker.PhysicalQualified || marker.ReleaseQualified || marker.InputSHA256 != base.InputSHA256 || marker.RunnerSHA256 != base.RunnerSHA256 || !marker.Started.Equal(start) || !marker.Finished.Equal(base.ProducerFinishedUTC) || marker.Packages != 2 || marker.Roots != 3 || marker.Nodes != 4 || marker.AllowedSkips != 2 {
		t.Fatal("success marker lost binding/privacy")
	}
	var fields map[string]json.RawMessage
	decodeJSON(body, &fields)
	if len(fields) != 14 {
		t.Fatal("success marker schema expanded")
	}
	for _, kind := range []string{"failure", "not-executed", "no-inventory", "bad-skips", "resource", "clock"} {
		r := base
		switch kind {
		case "failure":
			r.Passed = false
		case "not-executed":
			r.FunctionalTestsExecuted = false
		case "no-inventory":
			r.Events.Adequate = false
		case "bad-skips":
			r.Events.AllowedSkips = 0
		case "resource":
			r.ResourceProfileEligible = true
		case "clock":
			r.ProducerFinishedUTC = start
		}
		expectContract(t, func() { producerSuccessMarker(r) })
	}
	if !reflect.DeepEqual(goToolSetupArgv(), []string{"telemetry", "off"}) {
		t.Fatal("security qualification changed Go tool policy")
	}
}

func TestFunctionalCoverageImmutableReadSelectionStaysInStore(t *testing.T) {
	original := "/nix/store/" + strings.Repeat("0", 32) + "-tool-alias/bin/nix"
	target := "/nix/store/" + strings.Repeat("1", 32) + "-tool/bin/nix"
	calls := 0
	resolve := func(path string) (string, error) {
		calls++
		if path != original {
			t.Fatal("original selection changed")
		}
		return target, nil
	}
	if got, e := resolveImmutableRead(original, resolve); e != nil || got != target || calls != 1 {
		t.Fatal("immutable absolute tool alias rejected")
	}
	for _, path := range []string{"relative", "/tmp/tool", "/nix/store/../outside", "/nix/store/" + strings.Repeat("0", 32) + "-tool/bin/../nix", original + "\x00"} {
		calls = 0
		if _, e := resolveImmutableRead(path, resolve); e == nil || calls != 0 {
			t.Fatal("noncanonical/nonstore original resolved")
		}
	}
	if got, e := resolveImmutableRead(original, func(path string) (string, error) { return path, nil }); e != nil || got != original {
		t.Fatal("unchanged immutable selection rejected")
	}
	for _, resolved := range []string{"relative", "/tmp/tool", "/nix/storex/" + filepath.Base(target), "/nix/store/../outside", target + "/../escape", target + "\x00"} {
		if _, e := resolveImmutableRead(original, func(string) (string, error) { return resolved, nil }); e == nil {
			t.Fatal("resolved target escaped immutable store")
		}
	}
	if _, e := resolveImmutableRead(original, func(string) (string, error) { return "", errContract }); e == nil {
		t.Fatal("failed resolution admitted")
	}
}
