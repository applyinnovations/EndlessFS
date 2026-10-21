package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func expectContract(t *testing.T, fn func()) {
	t.Helper()
	denied := false
	func() { defer func() { denied = recover() != nil }(); fn() }()
	if !denied {
		t.Fatal("expected closed contract denial")
	}
}
func fixtureInventory() []packageInventory {
	return []packageInventory{
		{Package: module + "/internal/e2e", Roots: []string{"TestProof", "TestE2EUploadSourceHardwareQualification"}},
		{Package: module + "/cmd/endlessfs", Roots: []string{"TestGenerateCurrentMigrationFixtures"}},
	}
}
func fixtureEvents() []map[string]any {
	a, b := module+"/internal/e2e", module+"/cmd/endlessfs"
	return []map[string]any{
		{"Action": "build-output", "ImportPath": a, "Output": "# split diagnostic"}, {"Action": "build-output", "ImportPath": a, "Output": " complete\n"},
		{"Package": a, "Action": "start"}, {"Package": b, "Action": "start"}, {"Package": a, "Action": "run", "Test": "TestProof"}, {"Package": a, "Action": "pause", "Test": "TestProof"},
		{"Package": b, "Action": "run", "Test": "TestGenerateCurrentMigrationFixtures"}, {"Package": b, "Action": "skip", "Test": "TestGenerateCurrentMigrationFixtures"}, {"Package": b, "Action": "pass"},
		{"Package": a, "Action": "cont", "Test": "TestProof"}, {"Package": a, "Action": "run", "Test": "TestProof/foo/bar"}, {"Package": a, "Action": "pass", "Test": "TestProof/foo/bar"}, {"Package": a, "Action": "pass", "Test": "TestProof"},
		{"Package": a, "Action": "run", "Test": "TestE2EUploadSourceHardwareQualification"}, {"Package": a, "Action": "skip", "Test": "TestE2EUploadSourceHardwareQualification"}, {"Package": a, "Action": "pass"},
	}
}
func writeEvents(t *testing.T, rows []map[string]any) string {
	t.Helper()
	var text strings.Builder
	for _, row := range rows {
		if !strings.HasPrefix(row["Action"].(string), "build-") {
			row["Time"] = "2026-10-19T00:00:00Z"
		}
		b, e := json.Marshal(row)
		if e != nil {
			t.Fatal(e)
		}
		text.Write(b)
		text.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "events.jsonl")
	if e := os.WriteFile(p, []byte(text.String()), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestFunctionalCoverageObservedInventoryAndSafeRoundTrip(t *testing.T) {
	path := writeEvents(t, fixtureEvents())
	safe := filepath.Join(t.TempDir(), "safe.jsonl")
	got := validateEvents(path, fixtureInventory(), safe)
	if !got.Adequate || got.Packages != 2 || got.Roots != 3 || got.Nodes != 4 || got.AllowedSkips != 2 || got.BuildEvents != 2 || got.LifecycleRecords != 14 {
		t.Fatal("complete lifecycle not proved")
	}
	validateSafeInventory(safe, fixtureInventory(), got)
	body := readFile(safe, maxSafeEvents)
	if strings.Contains(string(body), "foo/bar") || strings.Contains(string(body), "diagnostic") {
		t.Fatal("raw dynamic output escaped")
	}
	var lines [][]byte
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		lines = append(lines, []byte(line))
	}
	for _, kind := range []string{"missing", "late", "unknown", "skip"} {
		t.Run(kind, func(t *testing.T) {
			changed := append([][]byte(nil), lines...)
			switch kind {
			case "missing":
				changed = changed[:len(changed)-1]
			case "late":
				changed = append(changed, changed[len(changed)-1])
			case "unknown":
				changed[0] = []byte(`{"Ordinal":1,"Package":"foreign","Action":"start"}`)
			case "skip":
				changed[9] = []byte(strings.Replace(string(changed[9]), `"pass"`, `"skip"`, 1))
			}
			file := filepath.Join(t.TempDir(), "bad.jsonl")
			var all []byte
			for _, line := range changed {
				all = append(all, line...)
				all = append(all, '\n')
			}
			if e := os.WriteFile(file, all, 0600); e != nil {
				t.Fatal(e)
			}
			expectContract(t, func() { validateSafeInventory(file, fixtureInventory(), got) })
		})
	}
}
func TestFunctionalCoverageOriginalEventDenials(t *testing.T) {
	for _, kind := range []string{"missing-terminal", "unexpected-skip", "late-output", "orphan-child", "failed-build", "cached-fragments", "unfinished-build-fragment"} {
		t.Run(kind, func(t *testing.T) {
			rows := fixtureEvents()
			switch kind {
			case "missing-terminal":
				rows = rows[:len(rows)-1]
			case "unexpected-skip":
				rows[11]["Action"] = "skip"
			case "late-output":
				rows = append(rows[:13], append([]map[string]any{{"Package": module + "/internal/e2e", "Action": "output", "Test": "TestProof", "Output": "late\n"}}, rows[13:]...)...)
			case "orphan-child":
				rows[10]["Test"] = "TestUnknown/child"
			case "failed-build":
				rows[0]["Action"] = "build-fail"
			case "cached-fragments":
				rows = append(rows[:11], append([]map[string]any{{"Package": module + "/internal/e2e", "Action": "output", "Test": "TestProof/foo/bar", "Output": "(cac"}, {"Package": module + "/internal/e2e", "Action": "output", "Test": "TestProof/foo/bar", "Output": "hed)"}}, rows[11:]...)...)
			case "unfinished-build-fragment":
				rows[1]["Output"] = " no newline"
			}
			expectContract(t, func() { validateEvents(writeEvents(t, rows), fixtureInventory()) })
		})
	}
	for _, line := range []string{`{"Action":"start","Action":"pass","Package":"p","Time":"2026-10-19T00:00:00Z"}`, `{"Action":"start","package":"p","Time":"2026-10-19T00:00:00Z"}`, `{"Action":"start","Package":"p","Time":null}`, `{"Action":"start","Package":"p","Time":"not-a-time"}`, `{"Action":"build-output","ImportPath":"p","Package":"","Output":"x\\n"}`, `{"Action":"attr","Package":"p","Time":"2026-10-19T00:00:00Z","Key":"unsupported"}`} {
		expectContract(t, func() { decodeEvent([]byte(line)) })
	}
}
func TestFunctionalCoverageNoTestPackages(t *testing.T) {
	pkg := module + "/internal/notests"
	for _, terminal := range []string{"pass", "skip"} {
		inventory := append(fixtureInventory(), packageInventory{Package: pkg})
		rows := append(fixtureEvents(), map[string]any{"Package": pkg, "Action": "start"}, map[string]any{"Package": pkg, "Action": "output", "Output": "? no test files\n"}, map[string]any{"Package": pkg, "Action": terminal})
		if validateEvents(writeEvents(t, rows), inventory).Packages != 3 {
			t.Fatal("empty package not accounted")
		}
	}
	inv := append(fixtureInventory(), packageInventory{Package: pkg, Roots: []string{"TestMustRun"}})
	rows := append(fixtureEvents(), map[string]any{"Package": pkg, "Action": "start"}, map[string]any{"Package": pkg, "Action": "skip"})
	expectContract(t, func() { validateEvents(writeEvents(t, rows), inv) })
}
func TestFunctionalCoverageProfileAndClosureDenials(t *testing.T) {
	p := filepath.Join(t.TempDir(), "coverage.out")
	source := map[string]string{"internal/sample.go": strings.Repeat("a", 64)}
	good := "mode: atomic\n" + module + "/internal/sample.go:1.1,2.2 3 1\n"
	if e := os.WriteFile(p, []byte(good), 0600); e != nil {
		t.Fatal(e)
	}
	if validateProfile(p, source) != uint64(len(good)) {
		t.Fatal("profile mismatch")
	}
	for _, bad := range []string{strings.Replace(good, "atomic", "set", 1), strings.Replace(good, "sample.go", "unknown.go", 1), strings.Replace(good, "2.2", "0.2", 1), strings.Replace(good, "3 1", "3 -1", 1), "mode: atomic\n"} {
		if e := os.WriteFile(p, []byte(bad), 0600); e != nil {
			t.Fatal(e)
		}
		expectContract(t, func() { validateProfile(p, source) })
	}
	a, b := "/nix/store/00000000000000000000000000000000-a", "/nix/store/11111111111111111111111111111111-b"
	nodes := []closureNode{{Path: a, NarHash: "sha256-synthetic-a", NarSize: 1, References: []string{b}}, {Path: b, NarHash: "sha256-synthetic-b", NarSize: 2}}
	if len(validateClosure(nodes, []string{a + "/bin/tool"})) != 64 {
		t.Fatal("closure missing")
	}
	expectContract(t, func() { validateClosure(nodes[:1], []string{a}) })
	expectContract(t, func() { validateClosure(append(nodes, nodes[0]), []string{a}) })
}
func TestFunctionalCoverageEnvironmentEligibility(t *testing.T) {
	c := config{Native: "/pinned/bin/cc"}
	base := map[string]string{"ENDLESSFS_LOCAL_FUNCTIONAL_CACHE": "1", "GOENV": "off"}
	clone := func() map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	for _, name := range []string{"GOFLAGS", "CGO_ENABLED", "ENDLESSFS_RUN_E2E", "ENDLESSFS_CHROMIUM", "ENDLESSFS_CHROMIUM_NO_SANDBOX", "ENDLESSFS_INTERNAL_RAW_DECODER", "ENDLESSFS_TEST_RAW_DECODER", "ENDLESSFS_COVERAGE_PROFILE"} {
		m := clone()
		m[name] = "arbitrary-not-recorded"
		if !environmentEligible(c, m) {
			t.Fatal("always overwritten/output-only field rejected")
		}
	}
	for _, name := range runnerMarkers {
		m := clone()
		m[name] = ""
		if environmentEligible(c, m) {
			t.Fatal("present runner marker admitted")
		}
	}
	for _, name := range []string{"GIT_CONFIG", "ENDLESSFS_SMART_RECOVERY_EVIDENCE", "ENDLESSFS_CPU_PROFILE", "GOMAXPROCS", "GOMEMLIMIT", "GODEBUG", "GOEXPERIMENT", "GOCOVERDIR", "LD_PRELOAD"} {
		m := clone()
		m[name] = "unsupported"
		if environmentEligible(c, m) {
			t.Fatal("noncanonical selector admitted")
		}
	}
	for _, pair := range [][2]string{{"GOENV", "/custom"}, {"GOWORK", "/work"}, {"GOTOOLCHAIN", "auto"}, {"CC", "foreign"}} {
		m := clone()
		m[pair[0]] = pair[1]
		if environmentEligible(c, m) {
			t.Fatal("custom Go configuration admitted")
		}
	}
}
func TestFunctionalCoverageDefaultGoConfigIsMetadataOnly(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home}
	if !defaultGoConfigEligible(env) {
		t.Fatal("absent selected default rejected")
	}
	dir := filepath.Join(home, ".config/go")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "env")
	if e := os.WriteFile(p, nil, 0600); e != nil {
		t.Fatal(e)
	}
	if !defaultGoConfigEligible(env) {
		t.Fatal("zero-size config rejected")
	}
	if e := os.WriteFile(p, []byte("secret-body"), 0000); e != nil {
		t.Fatal(e)
	}
	if defaultGoConfigEligible(env) {
		t.Fatal("nonempty config admitted")
	}
	env["GOENV"] = "off"
	if !defaultGoConfigEligible(env) {
		t.Fatal("explicit disabled config rejected")
	}
}
func TestFunctionalCoverageTreeGuardNeverReadsUnknownBodies(t *testing.T) {
	for _, kind := range []string{"file", "ignored-directory", "symlink", "same-size-source"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			known := filepath.Join(root, "known.go")
			if e := os.WriteFile(known, []byte("aaaa"), 0600); e != nil {
				t.Fatal(e)
			}
			s, e := os.Stat(known)
			if e != nil {
				t.Fatal(e)
			}
			expected := manifest{"known.go": {SHA256: sum([]byte("aaaa")), Bytes: 4}}
			switch kind {
			case "file":
				if e := os.WriteFile(filepath.Join(root, ".env"), []byte("private"), 0000); e != nil {
					t.Fatal(e)
				}
			case "ignored-directory":
				if e := os.Mkdir(filepath.Join(root, ".cache"), 0700); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				if e := os.Symlink("/private", filepath.Join(root, "unknown")); e != nil {
					t.Fatal(e)
				}
			case "same-size-source":
				if e := os.WriteFile(known, []byte("bbbb"), 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Chtimes(known, s.ModTime(), s.ModTime()); e != nil {
					t.Fatal(e)
				}
			}
			reads := 0
			expectContract(t, func() {
				verifyTree(root, expected, func(path string, max int64) string {
					if filepath.Base(path) != "known.go" {
						t.Fatal("unknown body opened")
					}
					reads++
					return hashFile(path, max)
				})
			})
			if kind == "same-size-source" && reads != 1 {
				t.Fatal("same-size/mtime change bypassed hash")
			}
		})
	}
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "a.go"), []byte("a"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(root, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, ".git", "private-admin"), []byte("private"), 0000); e != nil {
		t.Fatal(e)
	}
	expected := manifest{"a.go": {SHA256: sum([]byte("a")), Bytes: 1}}
	if verifyTree(root, expected, hashFile) != canonical(expected) {
		t.Fatal("exact root administration exception failed")
	}
}
func TestFunctionalCoverageIndexGeometryAndModes(t *testing.T) {
	expected := manifest{"a.go": {Bytes: 1}}
	oid := strings.Repeat("a", 40)
	good := "100644 " + oid + " 0\ta.go\x00"
	if len(parseIndex([]byte(good), expected)) != 1 {
		t.Fatal("stage0 rejected")
	}
	for _, bad := range []string{strings.Replace(good, " 0\t", " 1\t", 1), strings.Replace(good, "a.go", ".env", 1), strings.Replace(good, "100644", "120000", 1), good + good, strings.TrimSuffix(good, "\x00")} {
		expectContract(t, func() { parseIndex([]byte(bad), expected) })
	}
}
func TestFunctionalCoverageIdentityIncludesAllSemanticFields(t *testing.T) {
	base := inputIdentity{Schema: schema, Profile: "canonical", FullSource: manifest{"fixture": {SHA256: sum([]byte("aaaa")), Bytes: 4}}, Vendor: manifest{"dependency": {SHA256: "a"}}, Tools: map[string]string{"go": "a"}, Argv: coverageArgv("<OWNED_PROFILE>"), EnvironmentPolicy: []string{"GOENV=off"}}
	for _, name := range []string{"fixture", "vendor", "tool", "flags", "env", "policy", "runner", "raw-flake"} {
		b, _ := json.Marshal(base)
		var changed inputIdentity
		decodeJSON(b, &changed)
		switch name {
		case "fixture":
			changed.FullSource["fixture"] = fileIdentity{SHA256: sum([]byte("bbbb")), Bytes: 4}
		case "vendor":
			changed.Vendor["dependency"] = fileIdentity{SHA256: "b"}
		case "tool":
			changed.Tools["go"] = "b"
		case "flags":
			changed.Argv[0] = "changed"
		case "env":
			changed.EnvironmentPolicy[0] = "GOENV=changed"
		case "policy":
			changed.PolicySHA256 = "b"
		case "runner":
			changed.HelperSHA256 = "b"
		case "raw-flake":
			changed.FullSource["flake.nix"] = fileIdentity{SHA256: "b"}
		}
		if canonical(base) == canonical(changed) {
			t.Fatal("identity missed selected input")
		}
	}
}
func TestFunctionalCoverageDisabledEffectiveGOENVAndOutputOnlyExport(t *testing.T) {
	c := config{GoRoot: "/go/share/go", GoVersion: "1.26.9", Native: "/native/bin/cc"}
	w := "/build/owned"
	got := map[string]string{"GOROOT": c.GoRoot, "GOVERSION": "go1.26.9", "GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1", "CGO_ENABLED": "0", "CC": c.Native, "CXX": "/native/bin/c++", "GOENV": "", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "-mod=vendor", "GOCACHE": filepath.Join(w, "go-cache"), "GOTELEMETRY": "off"}
	if !effectiveEnvironmentValid(c, w, got) {
		t.Fatal("pinned effective GOENV rejected")
	}
	got["GOENV"] = "off"
	if effectiveEnvironmentValid(c, w, got) {
		t.Fatal("incorrect effective GOENV accepted")
	}
	delete(got, "GOENV")
	if effectiveEnvironmentValid(c, w, got) {
		t.Fatal("missing effective field accepted")
	}
	for _, row := range environment(c, w) {
		if strings.HasPrefix(row, "ENDLESSFS_COVERAGE_PROFILE=") || strings.HasPrefix(row, "GOMAXPROCS=") {
			t.Fatal("caller output/resource selector leaked")
		}
	}
}
func TestFunctionalCoverageNoOuterStatusFallback(t *testing.T) {
	for _, code := range []int{0, 1, 125, 137} {
		fallback, realize := 0, 0
		got := dispatch(consumeActions{Eligible: func() bool { return true }, Fallback: func() int { fallback++; return 0 }, RealizeAndValidate: func() int { realize++; return code }})
		if got != code || fallback != 0 || realize != 1 {
			t.Fatal("post-boundary failure retried")
		}
	}
	fallback, realize := 0, 0
	if dispatch(consumeActions{Eligible: func() bool { return false }, Fallback: func() int { fallback++; return 7 }, RealizeAndValidate: func() int { realize++; return 0 }}) != 7 || fallback != 1 || realize != 0 {
		t.Fatal("prestart decline did not run exact fresh once")
	}
}
func TestFunctionalCoverageChildContract(t *testing.T) {
	if os.Getenv("FUNCTIONAL_CONTROL_CHILD") == "exit125" {
		if e := os.WriteFile(os.Getenv("FUNCTIONAL_CONTROL_MARKER"), []byte("ran\n"), 0600); e != nil {
			os.Exit(2)
		}
		os.Exit(125)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	fallback := 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := dispatch(consumeActions{Eligible: func() bool { return true }, Fallback: func() int { fallback++; return 0 }, RealizeAndValidate: func() int {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFunctionalCoverageChildContract$")
		cmd.Env = append(os.Environ(), "FUNCTIONAL_CONTROL_CHILD=exit125", "FUNCTIONAL_CONTROL_MARKER="+marker)
		err := cmd.Run()
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode()
		}
		return 1
	}})
	if got != 125 || fallback != 0 || string(readFile(marker, 16)) != "ran\n" {
		t.Fatal("real child125 crossed fallback boundary")
	}
}
func TestFunctionalCoverageCanonicalArgumentsDoNotSelectSubset(t *testing.T) {
	if !reflect.DeepEqual(coverageArgv("p"), []string{"test", "-timeout=30m", "./...", "-count=1", "-covermode=atomic", "-coverpkg=./...", "-coverprofile=p", "-json"}) {
		t.Fatal("full workload flags changed")
	}
}
func TestFunctionalCoveragePartialPublicationAndClosedJSON(t *testing.T) {
	for _, kind := range []string{"missing", "extra", "symlink"} {
		root := t.TempDir()
		for _, n := range []string{"receipt.json", "coverage.out", "coverage-summary.txt", "events.jsonl"} {
			if kind == "missing" && n == "events.jsonl" {
				continue
			}
			if kind == "symlink" && n == "events.jsonl" {
				if e := os.Symlink("/private", filepath.Join(root, n)); e != nil {
					t.Fatal(e)
				}
				continue
			}
			if e := os.WriteFile(filepath.Join(root, n), nil, 0600); e != nil {
				t.Fatal(e)
			}
		}
		if kind == "extra" {
			if e := os.WriteFile(filepath.Join(root, "raw-output"), []byte("private"), 0600); e != nil {
				t.Fatal(e)
			}
		}
		expectContract(t, func() { artifactFiles(root) })
	}
	for _, body := range []string{`{"Schema":"a","Schema":"b"}`, `{"unexpected":true}`, `{} {}`} {
		expectContract(t, func() { var v config; decodeJSON([]byte(body), &v) })
	}
}
func TestFunctionalCoverageExportIsOutputOnlyAndRejectsSourceDrift(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	if e := os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(parent, "coverage.out")
	exportProfile(path, source, []byte("mode: atomic\n"))
	if string(readFile(path, 64)) != "mode: atomic\n" {
		t.Fatal("export did not copy validated bytes")
	}
	expectContract(t, func() { validateExport(filepath.Join(source, "coverage.out"), source) })
	expectContract(t, func() { validateExport("relative", source) })
	before := guardSnapshot{Root: source, TreeSHA256: "a", IndexSHA256: "b", EnvironmentSHA256: "c"}
	after := before
	after.TreeSHA256 = "d"
	fallback := 0
	expectContract(t, func() {
		dispatch(consumeActions{Eligible: func() bool { return true }, Fallback: func() int { fallback++; return 0 }, RealizeAndValidate: func() int {
			if !before.Equal(after) {
				panic(errContract)
			}
			return 0
		}})
	})
	if fallback != 0 {
		t.Fatal("post-run drift retried fresh")
	}
}
func TestFunctionalCoverageFlakeIntegrationHasOneLegacyBodyAndNoStatusRetry(t *testing.T) {
	body, e := os.ReadFile(filepath.Join("..", "..", "flake.nix"))
	if e != nil {
		t.Fatal("coverage input flake missing")
	}
	text := string(body)
	for _, required := range []string{"coverageText = ''", "text = coverageText;", "exec ${pkgs.nix}/bin/nix run --offline --no-write-lock-file", "path:${self.outPath}#apps.${system}.local-functional-coverage-consumer", "go test -timeout=30m ./... -count=1 -covermode=atomic -coverpkg=./... -coverprofile=\"$profile\"", "install -m 0644 \"$profile\" \"$ENDLESSFS_COVERAGE_PROFILE\""} {
		if !strings.Contains(text, required) {
			t.Fatalf("shared original body/lazy dispatch contract missing: %q", required)
		}
	}
	if !regexp.MustCompile(`\+\s+coverageText\s*\)`).MatchString(text) {
		t.Fatal("shared original coverage body must close the public task expression")
	}
	if strings.Contains(text, "functional_coverage_status") || strings.Contains(text, "125)") {
		t.Fatal("outer status retry protocol restored")
	}
}
func TestFunctionalCoverageFreshInventoryRejectsOmittedPackageFileOrRoot(t *testing.T) {
	fresh := []packageInventory{
		{Package: module + "/internal/e2e", Roots: []string{"TestE2ESmartRecoveryProductionCardinalityWarmTransfersOnlyRemainingFiles", "TestProof"}, TestFiles: map[string]string{"proof_test.go": "bound-proof", "smart_test.go": "bound-smart"}},
		{Package: module + "/tools/example", Roots: []string{"TestOther"}, TestFiles: map[string]string{"other_test.go": "bound-other"}},
	}
	requireInventoryMatch(fresh, fresh)
	for _, kind := range []string{"package", "file", "root", "mandatory-root", "changed-file-hash", "extra-root"} {
		t.Run(kind, func(t *testing.T) {
			body, e := json.Marshal(fresh)
			if e != nil {
				t.Fatal(e)
			}
			var recorded []packageInventory
			decodeJSON(body, &recorded)
			switch kind {
			case "package":
				recorded = recorded[:1]
			case "file":
				delete(recorded[0].TestFiles, "proof_test.go")
			case "root":
				recorded[0].Roots = recorded[0].Roots[:1]
			case "mandatory-root":
				recorded[0].Roots = recorded[0].Roots[1:]
			case "changed-file-hash":
				recorded[0].TestFiles["proof_test.go"] = "different"
			case "extra-root":
				recorded[0].Roots = append(recorded[0].Roots, "TestUnexpected")
			}
			expectContract(t, func() { requireInventoryMatch(fresh, recorded) })
		})
	}
	incomplete := []packageInventory{{Package: module + "/internal/e2e", Roots: []string{"TestProof"}}}
	expectContract(t, func() { requireInventoryMatch(incomplete, incomplete) })
}
func TestFunctionalCoverageRootAncestorsMustBeSafe(t *testing.T) {
	for _, kind := range []string{"nonsticky-writable", "symlink", "sticky-owned-leaf"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			ancestor := filepath.Join(base, "ancestor")
			leaf := filepath.Join(ancestor, "leaf")
			if e := os.MkdirAll(leaf, 0700); e != nil {
				t.Fatal(e)
			}
			candidate := leaf
			switch kind {
			case "nonsticky-writable":
				if e := os.Chmod(ancestor, 0777); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				link := filepath.Join(base, "link")
				if e := os.Symlink(ancestor, link); e != nil {
					t.Fatal(e)
				}
				candidate = filepath.Join(link, "leaf")
			case "sticky-owned-leaf":
				if e := os.Chmod(ancestor, 0777|os.ModeSticky); e != nil {
					t.Fatal(e)
				}
			}
			got := safeParents(candidate, false)
			if got != (kind == "sticky-owned-leaf") {
				t.Fatal("unsafe ancestor admitted or sticky owned-leaf geometry rejected")
			}
			if kind == "sticky-owned-leaf" {
				file := filepath.Join(leaf, "code.go")
				if e := os.WriteFile(file, []byte("a"), 0600); e != nil {
					t.Fatal(e)
				}
				expected := manifest{"code.go": {SHA256: sum([]byte("a")), Bytes: 1}}
				if verifyTree(leaf, expected, hashFile) != canonical(expected) {
					t.Fatal("owned nonshared leaf proof missing")
				}
			}
		})
	}
}
func TestFunctionalCoverageUIDComparisonHasNoNarrowing(t *testing.T) {
	if !sameUID(42, 42) || sameUID(42, -1) || sameUID(42, 43) || sameUID(^uint64(0), -1) {
		t.Fatal("UID ownership comparison incorrect")
	}
	if sameUID(0, 1<<32) {
		t.Fatal("high UID narrowed into another owner")
	}
}
func TestFunctionalCoverageCanceledEligibilityNeverLaunchesWork(t *testing.T) {
	for _, kind := range []string{"before-eligibility", "during-denial", "during-success"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "before-eligibility" {
				cancel()
			}
			eligibleCalls, freshCalls, producerCalls := 0, 0, 0
			code := dispatch(consumeActions{Context: ctx, Eligible: func() bool { eligibleCalls++; cancel(); return kind == "during-success" }, Fallback: func() int { freshCalls++; return 0 }, RealizeAndValidate: func() int { producerCalls++; return 0 }})
			expectedEligibility := 1
			if kind == "before-eligibility" {
				expectedEligibility = 0
			}
			if code == 0 || eligibleCalls != expectedEligibility || freshCalls != 0 || producerCalls != 0 {
				t.Fatal("canceled eligibility launched fresh or producer work")
			}
		})
	}
}

// The original all-package coverpkg profile repeats instrumented blocks across
// test packages; retained full-suite profiles exceed 64MiB before deduplication.
// Keep those original bytes and counters rather than truncating the evidence.
func TestFunctionalCoverageAggregateProfileCapacity(t *testing.T) {
	if maxProfile != 128<<20 {
		t.Fatal("profile byte limit changed without reviewed capacity evidence")
	}
	path := filepath.Join(t.TempDir(), "coverage.out")
	line := []byte(module + "/internal/drive/profile.go:1.1,1.2 1 1\n")
	body := append([]byte("mode: atomic\n"), bytes.Repeat(line, (65<<20)/len(line)+1)...)
	writeExclusive(path, body, 0600)
	if got := validateProfile(path, map[string]string{"internal/drive/profile.go": "declared-source-digest"}); got != uint64(len(body)) {
		t.Fatal("aggregate coverage profile lost original bytes")
	}
	if got := readFile(path, maxProfile); !bytes.Equal(got, body) {
		t.Fatal("bounded profile read changed bytes")
	}
	exported := filepath.Join(t.TempDir(), "export.out")
	exportProfile(exported, filepath.Dir(path), body)
	if hashFile(exported, maxProfile) != sum(body) {
		t.Fatal("aggregate profile export changed bytes")
	}
	oversize := filepath.Join(t.TempDir(), "oversize.out")
	f, e := os.OpenFile(oversize, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate((128 << 20) + 1); e != nil {
		_ = f.Close()
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	// A direct bounded read proves size denial independently of the sparse
	// file's invalid contents; parser rejection alone would mask a broken cap.
	expectContract(t, func() { readFile(oversize, maxProfile) })
}

func TestFunctionalCoverageSourceManifestBindsIsolationCodeAndModes(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"tools/functionalcoverage", "vendor"} {
		if os.MkdirAll(filepath.Join(root, directory), 0700) != nil {
			t.Fatal("manifest fixture directory")
		}
	}
	path := filepath.Join(root, "tools/functionalcoverage/namespace_protocol.go")
	if os.WriteFile(path, []byte("synthetic isolation policy v1"), 0600) != nil || os.WriteFile(filepath.Join(root, "vendor/module.go"), []byte("synthetic vendor"), 0600) != nil {
		t.Fatal("manifest fixture")
	}
	before := sourceManifest(root, true)
	if len(before) != 1 || !coverageMember("tools/functionalcoverage/namespace_protocol.go") || coverageMember("docs/private.md") || before["tools/functionalcoverage/namespace_protocol.go"].Executable {
		t.Fatal("source membership or mode binding")
	}
	if len(sourceManifest(root, false)) != 2 || len(hashes(before)) != 1 {
		t.Fatal("vendor isolation or hashes incomplete")
	}
	if os.WriteFile(path, []byte("synthetic isolation policy v2"), 0700) != nil || os.Chmod(path, 0700) != nil {
		t.Fatal("changed fixture")
	}
	after := sourceManifest(root, true)
	if reflect.DeepEqual(before, after) || hashes(before)["tools/functionalcoverage/namespace_protocol.go"] == hashes(after)["tools/functionalcoverage/namespace_protocol.go"] || !after["tools/functionalcoverage/namespace_protocol.go"].Executable {
		t.Fatal("changed isolation code/mode reused source identity")
	}
	if os.Symlink(path, filepath.Join(root, "link.go")) != nil {
		t.Fatal("symlink fixture")
	}
	expectContract(t, func() { sourceManifest(root, false) })
}
