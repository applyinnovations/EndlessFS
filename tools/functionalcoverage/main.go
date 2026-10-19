// Canonical functional evidence only. This helper never credits a reused
// artifact as fresh browser, resource, physical-replica or performance evidence.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

const schema = "endlessfs-canonical-functional-coverage-v1"
const module = "github.com/applyinnovations/endlessfs"
const maxRaw = 256 << 20

// Aggregate coverpkg profiles retain repeated blocks from every test package.
// Existing complete profiles exceed 64MiB; keep their original bytes bounded.
const maxProfile = 128 << 20
const maxSafeEvents = 64 << 20

var errContract = errors.New("functional_evidence_inadequate")

type config struct {
	Schema, Profile, FullSource, CoverageSource, Vendor                                       string
	Go, GoRoot, GoVersion, Native, Browser, Decoder, Gawk, Bash, Seed, Policy, Path, Nix, Git string
	Helper, ProducerInstallable, Legacy, NamespaceLauncher                                    string
}
type fileIdentity struct {
	SHA256     string
	Bytes      int64
	Executable bool
}
type manifest map[string]fileIdentity
type closureNode struct {
	Path       string   `json:"path"`
	NarHash    string   `json:"narHash"`
	NarSize    uint64   `json:"narSize"`
	References []string `json:"references"`
}
type nixAttributes struct {
	Outputs map[string]string `json:"outputs"`
	Closure []closureNode     `json:"closure"`
}
type commandResult struct {
	Argv                                 []string
	StartedUTC, FinishedUTC              time.Time
	PhysicalNanoseconds                  int64
	ExitCode                             int
	StdoutBytes, StderrBytes             int64
	StdoutSHA256, StderrSHA256           string
	Capped, IOFailed, GroupKillRequested bool
	Cleanup                              cleanupObservation
}
type inputIdentity struct {
	Schema, Profile                    string
	Config                             config
	FullSource, CoverageSource, Vendor manifest
	HelperSHA256, PolicySHA256         string
	Tools                              map[string]string
	Argv, EnvironmentPolicy            []string
	GoToolSetupPolicy                  string
	GoToolSetupArgv                    []string
	ConsumerIsolationPolicy            string
	ConsumerLauncherArgv               []string
}
type receipt struct {
	Schema, Profile, InputSHA256, RunnerSHA256, ClosureSHA256                                      string
	Passed, FunctionalAdequate, FunctionalTestsExecuted                                            bool
	ProducerStartedUTC, ProducerFinishedUTC                                                        time.Time
	ResourceProfileEligible, PerformanceQualified, ReleaseQualified, FreshPhysicalReplicaQualified bool
	RuntimeProvenance                                                                              runtimeProvenance
	Inputs                                                                                         inputIdentity
	Closure                                                                                        []closureNode
	Commands                                                                                       map[string]commandResult
	Inventory                                                                                      []packageInventory
	Events                                                                                         eventSummary
	ProfileSHA256, SafeEventsSHA256, PolicyOutputSHA256                                            string
	ProfileBytes                                                                                   uint64
}
type runtimeProvenance struct {
	GOOS, GOARCH, GoVersion     string
	NumCPU, EffectiveGOMAXPROCS int
	ResourceReason              string
}

func must(e error) {
	if e != nil {
		panic(errContract)
	}
}
func sum(b []byte) string    { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func canonical(v any) string { b, e := json.Marshal(v); must(e); return sum(b) }
func readFile(path string, max int64) []byte {
	f, e := openEvidenceFile(path, os.O_RDONLY, 0)
	must(e)
	info, e := f.Stat()
	must(e)
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > max {
		must(f.Close())
		panic(errContract)
	}
	b, e := io.ReadAll(io.LimitReader(f, max+1))
	closeErr := f.Close()
	must(e)
	must(closeErr)
	if int64(len(b)) != info.Size() || int64(len(b)) > max {
		panic(errContract)
	}
	return b
}
func uniqueJSON(d *json.Decoder, depth int) {
	if depth > 64 {
		panic(errContract)
	}
	t, e := d.Token()
	must(e)
	delim, ok := t.(json.Delim)
	if !ok {
		return
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			must(e)
			name, ok := k.(string)
			if !ok || seen[name] {
				panic(errContract)
			}
			seen[name] = true
			uniqueJSON(d, depth+1)
		}
		t, e = d.Token()
		must(e)
		if t != json.Delim('}') {
			panic(errContract)
		}
	case '[':
		for d.More() {
			uniqueJSON(d, depth+1)
		}
		t, e = d.Token()
		must(e)
		if t != json.Delim(']') {
			panic(errContract)
		}
	default:
		panic(errContract)
	}
}
func decodeJSON(b []byte, v any) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	uniqueJSON(d, 0)
	var tail any
	if d.Decode(&tail) != io.EOF {
		panic(errContract)
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	must(d.Decode(v))
	if d.Decode(&tail) != io.EOF {
		panic(errContract)
	}
}
func readJSON(path string, max int64, v any) { decodeJSON(readFile(path, max), v) }
func writeJSON(path string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	must(e)
	if len(b) > 4<<20 {
		panic(errContract)
	}
	writeExclusive(path, append(b, '\n'), 0600)
}
func writeExclusive(path string, b []byte, mode os.FileMode) {
	f, e := openEvidenceFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	must(e)
	n, e := f.Write(b)
	c := f.Close()
	must(e)
	must(c)
	if n != len(b) {
		panic(errContract)
	}
}
func hashFile(path string, max int64) string {
	f, e := openEvidenceFile(path, os.O_RDONLY, 0)
	must(e)
	s, e := f.Stat()
	must(e)
	if !s.Mode().IsRegular() || s.Size() < 0 || s.Size() > max {
		must(f.Close())
		panic(errContract)
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, max+1))
	c := f.Close()
	must(e)
	must(c)
	if n != s.Size() || n > max {
		panic(errContract)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func readBounded(path string, max int) (string, error) {
	f, e := openEvidenceFile(path, os.O_RDONLY, 0)
	if e != nil {
		return "", e
	}
	b, e := io.ReadAll(io.LimitReader(f, int64(max)+1))
	closeErr := f.Close()
	if e != nil || closeErr != nil || len(b) > max {
		return "", errContract
	}
	return string(b), nil
}
func storePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && strings.HasPrefix(path, "/nix/store/") && len(strings.Split(strings.TrimPrefix(path, "/nix/store/"), "/")[0]) >= 34
}
func sourceManifest(root string, omitVendor bool) manifest {
	out := manifest{}
	var total int64
	must(filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		name, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		name = filepath.ToSlash(name)
		if omitVendor && name == "vendor" && d.IsDir() {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		s, e := d.Info()
		if e != nil || !s.Mode().IsRegular() {
			return errContract
		}
		total += s.Size()
		if len(out) >= 50000 || s.Size() > 16<<20 || total > 2<<30 {
			return errContract
		}
		out[name] = fileIdentity{hashFile(path, 16<<20), s.Size(), s.Mode()&0111 != 0}
		return nil
	}))
	if len(out) == 0 {
		panic(errContract)
	}
	return out
}
func hashes(m manifest) map[string]string {
	out := map[string]string{}
	for n, v := range m {
		out[n] = v.SHA256
	}
	return out
}
func coverageMember(n string) bool {
	return n == "go.mod" || n == "go.sum" || n == "flake.nix" || strings.HasPrefix(n, "cmd/") || strings.HasPrefix(n, "internal/") || strings.HasPrefix(n, "tools/")
}
func identify(c config) inputIdentity {
	validateConfig(c)
	full := sourceManifest(c.FullSource, false)
	coverage := sourceManifest(c.CoverageSource, false)
	want := manifest{}
	for n, v := range full {
		if coverageMember(n) {
			want[n] = v
		}
	}
	if !reflect.DeepEqual(want, coverage) {
		panic(errContract)
	}
	tools := map[string]string{}
	for n, p := range map[string]string{"go": c.Go, "native": c.Native, "browser": c.Browser, "decoder": c.Decoder, "gawk": c.Gawk, "bash": c.Bash, "nix": c.Nix, "git": c.Git, "namespace-launcher": c.NamespaceLauncher, "seed-helper": filepath.Join(c.Seed, "bin/cache-seed"), "seed-inputs": filepath.Join(c.Seed, "seed-inputs.json")} {
		tools[n] = hashFile(p, 128<<20)
	}
	return inputIdentity{Schema: schema, Profile: c.Profile, Config: c, FullSource: full, CoverageSource: coverage, Vendor: sourceManifest(c.Vendor, false), HelperSHA256: hashFile(c.Helper, 128<<20), PolicySHA256: hashFile(c.Policy, 1<<20), Tools: tools, Argv: coverageArgv("<OWNED_PROFILE>"), EnvironmentPolicy: environment(c, "<OWNED_SCRATCH>"), GoToolSetupPolicy: goToolSetupPolicy, GoToolSetupArgv: append([]string{c.Go}, goToolSetupArgv()...), ConsumerIsolationPolicy: consumerIsolationPolicy, ConsumerLauncherArgv: namespaceArgv(c, nil)}
}
func validateConfig(c config) {
	if c.Schema != schema || c.Profile != "linux-amd64-canonical-functional-v1" {
		panic(errContract)
	}
	for _, p := range []string{c.FullSource, c.CoverageSource, c.Vendor, c.Go, c.GoRoot, c.Native, c.Browser, c.Decoder, c.Gawk, c.Bash, c.Seed, c.Policy, c.Helper, c.Nix, c.Git, c.NamespaceLauncher} {
		if !storePath(p) {
			panic(errContract)
		}
	}
	if c.GoVersion == "" || c.GoRoot != filepath.Join(filepath.Dir(filepath.Dir(c.Go)), "share/go") || c.Policy != filepath.Join(c.CoverageSource, "tools/coverage.awk") {
		panic(errContract)
	}
	if c.ProducerInstallable != "path:"+c.FullSource+"#packages.x86_64-linux.local-functional-coverage" {
		panic(errContract)
	}
}
func environment(c config, w string) []string {
	return []string{
		"PATH=" + c.Path, "HOME=" + filepath.Join(w, "home"), "XDG_CONFIG_HOME=" + filepath.Join(w, "home/.config"), "XDG_CACHE_HOME=" + filepath.Join(w, "xdg-cache"), "XDG_RUNTIME_DIR=" + filepath.Join(w, "runtime"),
		"TMPDIR=" + filepath.Join(w, "tmp"), "GOTMPDIR=" + filepath.Join(w, "tmp"), "GOCACHE=" + filepath.Join(w, "go-cache"), "GOMODCACHE=" + filepath.Join(w, "go-mod-cache"),
		"GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=vendor", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOAMD64=v1",
		"CC=" + c.Native, "CXX=" + filepath.Join(filepath.Dir(c.Native), "c++"), "LANG=C", "LC_ALL=C", "TZ=UTC",
		"ENDLESSFS_RUN_E2E=1", "ENDLESSFS_CHROMIUM=" + c.Browser, "ENDLESSFS_CHROMIUM_NO_SANDBOX=1", "ENDLESSFS_INTERNAL_RAW_DECODER=" + c.Decoder, "ENDLESSFS_TEST_RAW_DECODER=" + c.Decoder,
	}
}
func coverageArgv(profile string) []string {
	return []string{"test", "-timeout=30m", "./...", "-count=1", "-covermode=atomic", "-coverpkg=./...", "-coverprofile=" + profile, "-json"}
}
func commandPassed(r commandResult) bool {
	return r.ExitCode == 0 && !r.Capped && !r.IOFailed && r.Cleanup.Adequate && r.Cleanup.InitialScanComplete && r.Cleanup.FinalScanComplete && r.Cleanup.NaturalZero && r.Cleanup.FinalZero
}
func effectiveEnvironmentValid(c config, w string, got map[string]string) bool {
	want := map[string]string{"GOROOT": c.GoRoot, "GOVERSION": "go" + c.GoVersion, "GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1", "CGO_ENABLED": "0", "CC": c.Native, "CXX": filepath.Join(filepath.Dir(c.Native), "c++"), "GOENV": "", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "-mod=vendor", "GOCACHE": filepath.Join(w, "go-cache"), "GOTELEMETRY": "off"}
	return reflect.DeepEqual(want, got)
}
func copyTree(from, to string) {
	must(filepath.WalkDir(from, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		n, e := filepath.Rel(from, p)
		if e != nil {
			return e
		}
		dst := filepath.Join(to, n)
		if d.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		s, e := d.Info()
		if e != nil || !s.Mode().IsRegular() {
			return errContract
		}
		writeExclusive(dst, readFile(p, 16<<20), 0600|s.Mode().Perm()&0111)
		return os.Chtimes(dst, s.ModTime(), s.ModTime())
	}))
}
func validateClosure(nodes []closureNode, roots []string) string {
	if len(nodes) == 0 || len(nodes) > 20000 {
		panic(errContract)
	}
	known := map[string]bool{}
	for _, n := range nodes {
		if !storePath(n.Path) || known[n.Path] || n.NarHash == "" || len(n.NarHash) > 128 || n.NarSize == 0 {
			panic(errContract)
		}
		known[n.Path] = true
	}
	for _, n := range nodes {
		for _, ref := range n.References {
			if !known[ref] {
				panic(errContract)
			}
		}
	}
	for _, root := range roots {
		found := false
		for p := range known {
			if root == p || strings.HasPrefix(root, p+"/") {
				found = true
			}
		}
		if !found {
			panic(errContract)
		}
	}
	ordered := append([]closureNode(nil), nodes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	return canonical(ordered)
}
func closureRoots(c config) []string {
	return []string{c.FullSource, c.CoverageSource, c.Vendor, c.Go, c.Native, c.Browser, c.Decoder, c.Gawk, c.Bash, c.Seed, c.Policy, c.Helper, c.Nix, c.Git, c.NamespaceLauncher}
}
func runtimeMetadata() runtimeProvenance {
	return runtimeProvenance{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(), NumCPU: runtime.NumCPU(), EffectiveGOMAXPROCS: runtime.GOMAXPROCS(0), ResourceReason: "functional_only_resource_profile_not_qualified"}
}
func bytesReader(body []byte) *bytes.Reader { return bytes.NewReader(body) }
func safeMain() (status int) {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(os.Stderr, "functional coverage: failed_closed_no_retry")
			status = 1
		}
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	role := "unknown"
	if len(os.Args) > 1 {
		role = os.Args[1]
	}
	if role == "consume-isolated" {
		role = "consume"
	}
	ctx = newHelperDiagnostic(ctx, os.Stderr, role)
	defer finishHelperDiagnostic(ctx)
	setHelperStage(ctx, diagnosticArguments)
	if len(os.Args) < 3 {
		panic(errContract)
	}
	if !storePath(os.Args[2]) {
		panic(errContract)
	}
	setHelperStage(ctx, diagnosticConfig)
	var c config
	readJSON(os.Args[2], 4<<20, &c)
	validateConfig(c)
	ctx = context.WithValue(ctx, consumerConfigKey{}, os.Args[2])
	switch os.Args[1] {
	case "consume":
		return consume(ctx, c, os.Args[3:])
	case "consume-isolated":
		return consumeIsolated(ctx, c, os.Args[3:])
	case "produce":
		if len(os.Args) != 3 {
			panic(errContract)
		}
		produce(ctx, c)
		return 0
	default:
		panic(errContract)
	}
}
func main() { os.Exit(safeMain()) }
