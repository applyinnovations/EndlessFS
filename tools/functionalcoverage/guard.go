package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

var overwritten = map[string]bool{"GOFLAGS": true, "CGO_ENABLED": true, "ENDLESSFS_RUN_E2E": true, "ENDLESSFS_CHROMIUM": true, "ENDLESSFS_CHROMIUM_NO_SANDBOX": true, "ENDLESSFS_INTERNAL_RAW_DECODER": true, "ENDLESSFS_TEST_RAW_DECODER": true}
var runnerMarkers = []string{"CI", "CONTINUOUS_INTEGRATION", "CI_SERVER", "CIRCLECI", "TRAVIS", "APPVEYOR", "DRONE", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "JENKINS_URL", "TEAMCITY_VERSION", "TF_BUILD", "TEKTON_HOME", "TEKTON_TASKRUN", "TEKTON_TASK_RUN", "TEKTON_PIPELINE_RUN", "KUBERNETES_SERVICE_HOST", "KUBERNETES_PORT"}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" {
			panic(errContract)
		}
		if _, ok := out[k]; ok {
			panic(errContract)
		}
		out[k] = v
	}
	return out
}
func environmentEligible(c config, env map[string]string) bool {
	if env["ENDLESSFS_LOCAL_FUNCTIONAL_CACHE"] != "1" {
		return false
	}
	for _, name := range runnerMarkers {
		if _, present := env[name]; present {
			return false
		}
	}
	for name, value := range env {
		if overwritten[name] {
			continue
		}
		if strings.HasPrefix(name, "TEKTON_") || strings.HasPrefix(name, "KUBERNETES_") {
			return false
		}
		if strings.HasPrefix(name, "GIT_") {
			return false
		}
		if strings.HasPrefix(name, "ENDLESSFS_") && name != "ENDLESSFS_LOCAL_FUNCTIONAL_CACHE" && name != "ENDLESSFS_COVERAGE_PROFILE" {
			return false
		}
		switch name {
		case "GOENV":
			if value != "off" {
				return false
			}
		case "GOWORK":
			if value != "" && value != "off" {
				return false
			}
		case "GOTOOLCHAIN":
			if value != "" && value != "local" {
				return false
			}
		case "GOOS":
			if value != "" && value != "linux" {
				return false
			}
		case "GOARCH":
			if value != "" && value != "amd64" {
				return false
			}
		case "GOAMD64":
			if value != "" && value != "v1" {
				return false
			}
		case "CC":
			if value != "" && value != c.Native {
				return false
			}
		case "CXX":
			if value != "" && value != filepath.Join(filepath.Dir(c.Native), "c++") {
				return false
			}
		case "GOPROXY", "GOSUMDB":
			if value != "" && value != "off" {
				return false
			}
		case "GOCACHE", "GOMODCACHE", "GOTMPDIR", "GOPATH":
			if value != "" && value != "off" && !filepath.IsAbs(value) {
				return false
			}
		case "GOMAXPROCS", "GOMEMLIMIT", "GOGC", "GODEBUG", "GOEXPERIMENT", "GOFIPS140", "GOTRACEBACK", "GORACE", "GOCOVERDIR", "GOCACHEPROG", "GOPACKAGESDRIVER", "GO_EXTLINK_ENABLED", "GO19CONCURRENTCOMPILATION", "GOTESTONLY_STDTEST", "GOTEST_SHARD_INDEX", "GOTEST_TOTAL_SHARDS", "GO_TEST_TIMEOUT_SCALE", "GOTOOLDIR", "GOROOT", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_FFLAGS", "CGO_LDFLAGS", "PKG_CONFIG", "LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "NIX_CONFIG", "NIX_USER_CONF_FILES", "NIX_REMOTE", "NIX_BUILD_HOOK", "NIX_CONF_DIR":
			if value != "" {
				return false
			}
		default:
			if strings.HasPrefix(name, "GO") && value != "" {
				return false
			}
		}
	}
	return true
}
func defaultGoConfigEligible(env map[string]string) bool {
	if value, exists := env["GOENV"]; exists {
		return value == "off"
	}
	base := env["XDG_CONFIG_HOME"]
	if base == "" {
		if !filepath.IsAbs(env["HOME"]) {
			return false
		}
		base = filepath.Join(env["HOME"], ".config")
	}
	if !filepath.IsAbs(base) {
		return false
	}
	// Pinned Go cfg.EnvFile uses os.UserConfigDir()/go/env on Linux. Stat only:
	// no nonempty/custom config body is opened, even to decide a fallback.
	path := filepath.Join(base, "go/env")
	info, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return safeParents(path, false)
	}
	return e == nil && info.Mode().IsRegular() && info.Size() == 0 && owned(info) && safeParents(path, false)
}
func workspaceEligible(root string, env map[string]string) bool {
	if env["GOWORK"] == "off" {
		return true
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		_, e := os.Lstat(filepath.Join(dir, "go.work"))
		if !os.IsNotExist(e) {
			return false
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return true
}
func safeParents(path string, requireOwned bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		// #nosec G703 -- Deliberate metadata-only inspection of canonical absolute ancestors enforces the ownership, mode and nonsymlink boundary before any local file body is opened. Initial root selection assumes trusted same-UID task parents; no caller-selected body is read here.
		s, e := os.Lstat(dir)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 || s.Mode().Perm()&0022 != 0 && s.Mode()&os.ModeSticky == 0 || requireOwned && !owned(s) {
			return false
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return true
}

type indexEntry struct{ Mode, OID, Name string }
type guardSnapshot struct {
	Root                                       string
	IndexSHA256, TreeSHA256, EnvironmentSHA256 string
}

func gitEnv(c config) []string {
	return []string{"PATH=" + filepath.Dir(c.Git), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0", "LANG=C", "LC_ALL=C"}
}
func gitArgs(root string, args ...string) []string {
	return append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "diff.external=", "-c", "core.attributesFile=/dev/null", "-c", "core.quotePath=false", "-C", root}, args...)
}
func smallGit(ctx context.Context, c config, root string, args ...string) []byte {
	cmd := pinnedGitCommand(ctx, c, root, args...)
	cmd.Env = gitEnv(c)
	var b bytes.Buffer
	cmd.Stdout = &limitedBuffer{Buffer: &b, Max: 16 << 20}
	cmd.Stderr = io.Discard
	must(cmd.Run())
	return b.Bytes()
}

type limitedBuffer struct {
	Buffer *bytes.Buffer
	Max    int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.Max-b.Buffer.Len() {
		return 0, errContract
	}
	return b.Buffer.Write(p)
}
func parseIndex(body []byte, expected manifest) []indexEntry {
	if len(body) > 16<<20 || len(body) == 0 || body[len(body)-1] != 0 {
		panic(errContract)
	}
	seen := map[string]bool{}
	out := []indexEntry{}
	for _, line := range bytes.Split(body[:len(body)-1], []byte{0}) {
		left, name, ok := strings.Cut(string(line), "\t")
		fields := strings.Fields(left)
		if !ok || len(fields) != 3 || fields[2] != "0" || name == "" || seen[name] {
			panic(errContract)
		}
		want, ok := expected[name]
		if !ok || (fields[0] != "100644" && fields[0] != "100755") || (fields[0] == "100755") != want.Executable {
			panic(errContract)
		}
		if len(fields[1]) != 40 && len(fields[1]) != 64 {
			panic(errContract)
		}
		if _, e := hex.DecodeString(fields[1]); e != nil {
			panic(errContract)
		}
		seen[name] = true
		out = append(out, indexEntry{fields[0], fields[1], name})
	}
	if len(out) != len(expected) {
		panic(errContract)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func verifyIndexBodies(ctx context.Context, c config, root string, index []indexEntry, expected manifest) {
	// Batch immutable index blob IDs; never ask Git to read/apply filters to an
	// untracked pathname. All names were checked against the expected map first.
	var input strings.Builder
	for _, entry := range index {
		input.WriteString(entry.OID)
		input.WriteByte('\n')
	}
	cmd := pinnedGitCommand(ctx, c, root, "cat-file", "--batch")
	cmd.Env = gitEnv(c)
	cmd.Stdin = strings.NewReader(input.String())
	cmd.Stderr = io.Discard
	stdout, e := cmd.StdoutPipe()
	must(e)
	must(cmd.Start())
	done := false
	defer func() {
		if !done {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}
	}()
	reader := bufio.NewReaderSize(stdout, 64<<10)
	for _, entry := range index {
		line, e := reader.ReadString('\n')
		must(e)
		if len(line) > 256 {
			panic(errContract)
		}
		fields := strings.Fields(line)
		want := expected[entry.Name]
		if len(fields) != 3 || fields[0] != entry.OID || fields[1] != "blob" {
			panic(errContract)
		}
		n, e := strconv.ParseInt(fields[2], 10, 64)
		must(e)
		if n != want.Bytes || n < 0 || n > 16<<20 {
			panic(errContract)
		}
		h := sha256.New()
		written, e := io.CopyN(h, reader, n)
		must(e)
		if written != n || hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
			panic(errContract)
		}
		end, e := reader.ReadByte()
		must(e)
		if end != '\n' {
			panic(errContract)
		}
	}
	if _, e = reader.ReadByte(); e != io.EOF {
		panic(errContract)
	}
	must(cmd.Wait())
	done = true
}
func verifyTree(root string, expected manifest, read func(string, int64) string) string {
	// Presence/name check occurs BEFORE Info/Open on an unknown file or subtree.
	dirs := map[string]bool{".": true}
	for name := range expected {
		for p := filepath.ToSlash(filepath.Dir(name)); p != "."; p = filepath.ToSlash(filepath.Dir(p)) {
			dirs[p] = true
		}
	}
	seen := manifest{}
	must(filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return errContract
		}
		name, e := filepath.Rel(root, path)
		if e != nil {
			return errContract
		}
		name = filepath.ToSlash(name)
		if name == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if !dirs[name] {
				return errContract
			}
			s, e := d.Info()
			if e != nil || !owned(s) || s.Mode().Perm()&0022 != 0 {
				return errContract
			}
			return nil
		}
		want, ok := expected[name]
		if !ok {
			return errContract
		}
		s, e := d.Info()
		if e != nil || !s.Mode().IsRegular() || !owned(s) || s.Mode().Perm()&0022 != 0 || s.Size() != want.Bytes || (s.Mode()&0111 != 0) != want.Executable {
			return errContract
		}
		got := fileIdentity{read(path, 16<<20), s.Size(), s.Mode()&0111 != 0}
		if got != want {
			return errContract
		}
		seen[name] = got
		return nil
	}))
	if !reflect.DeepEqual(seen, expected) {
		panic(errContract)
	}
	return canonical(seen)
}
func guard(ctx context.Context, c config, expected manifest, env []string, args []string) guardSnapshot {
	values := envMap(env)
	if !backendAvailable() || len(args) != 0 || !environmentEligible(c, values) || !defaultGoConfigEligible(values) {
		panic(errContract)
	}
	root, e := os.Getwd()
	must(e)
	if !filepath.IsAbs(root) || !safeParents(root, false) || !workspaceEligible(root, values) {
		panic(errContract)
	}
	top := smallGit(ctx, c, root, "rev-parse", "--show-toplevel")
	if string(top) != root+"\n" {
		panic(errContract)
	}
	info, e := os.Lstat(filepath.Join(root, ".git"))
	must(e)
	if (!info.IsDir() && !info.Mode().IsRegular()) || !owned(info) {
		panic(errContract)
	}
	index := parseIndex(smallGit(ctx, c, root, "ls-files", "--stage", "--full-name", "-z"), expected)
	tree := verifyTree(root, expected, hashFile)
	verifyIndexBodies(ctx, c, root, index, expected)
	return guardSnapshot{root, canonical(index), tree, canonical(values)}
}
func tryEligibility(fn func() guardSnapshot) (s guardSnapshot, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	s = fn()
	return s, true
}
func validateExport(path, root string) {
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || within(root, path) || !safeParents(path, false) {
		panic(errContract)
	}
	s, e := os.Lstat(path)
	if os.IsNotExist(e) {
		parent, e := os.Stat(filepath.Dir(path))
		must(e)
		if !owned(parent) {
			panic(errContract)
		}
		return
	}
	must(e)
	if !s.Mode().IsRegular() || !owned(s) || s.Mode().Perm()&0022 != 0 {
		panic(errContract)
	}
}
func exportProfile(path, root string, body []byte) {
	if path == "" {
		return
	}
	validateExport(path, root)
	f, e := os.CreateTemp(filepath.Dir(path), ".endlessfs-functional-profile-")
	must(e)
	tmp := f.Name()
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(tmp)
		}
	}()
	must(f.Chmod(0644))
	n, e := f.Write(body)
	must(e)
	if n != len(body) {
		panic(errContract)
	}
	must(f.Close())
	validateExport(path, root)
	must(os.Rename(tmp, path))
	committed = true
}
func (s guardSnapshot) Equal(other guardSnapshot) bool { return s == other }
