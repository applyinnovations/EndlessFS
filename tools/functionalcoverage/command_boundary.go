package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

func requirePinnedExecutable(path string) {
	if !storePath(path) {
		panic(errContract)
	}
	f, e := openEvidenceFile(path, os.O_RDONLY, 0)
	must(e)
	info, e := f.Stat()
	closeErr := f.Close()
	must(e)
	must(closeErr)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		panic(errContract)
	}
}

// A fresh private HOME has no user nix.conf. Declare only the two features
// required by this pinned CLI; offline policy and ordinary resource settings stay.
func realizeArgv(c config) []string {
	return []string{"build", "--extra-experimental-features", "nix-command flakes", "--offline", "--no-write-lock-file", "--no-link", "--json", c.ProducerInstallable}
}

func commandShape(c config, work, name, exe string, args []string) bool {
	type shape struct {
		exe  string
		args []string
	}
	allowed := map[string]shape{
		"go-tool-mode-off": {c.Go, goToolSetupArgv()}, "go-environment": {c.Go, goEnvironmentArgv()},
		"inventory": {c.Go, []string{"list", "-json", "./..."}}, "coverage": {c.Go, coverageArgv(filepath.Join(work, "coverage.out"))},
		"seed-install": {filepath.Join(c.Seed, "bin/cache-seed"), []string{"install", filepath.Join(c.Seed, "cache"), filepath.Join(work, "go-cache")}},
		"policy":       {c.Gawk, []string{"-f", c.Policy, filepath.Join(work, "coverage.out")}},
		"realize":      {c.Nix, realizeArgv(c)},
	}
	if name == "consume-policy" {
		return exe == c.Gawk && len(args) == 3 && args[0] == "-f" && args[1] == c.Policy && storePath(args[2]) && filepath.Base(args[2]) == "coverage.out"
	}
	want, ok := allowed[name]
	return ok && want.exe == exe && reflect.DeepEqual(want.args, args)
}
func canonicalCommand(ctx context.Context, c config, work, name, exe string, args, env []string, dir string) *exec.Cmd {
	if ctx.Err() != nil || !filepath.IsAbs(work) || filepath.Clean(work) != work || !within(work, dir) || !commandShape(c, work, name, exe, args) || !reflect.DeepEqual(env, environment(c, work)) {
		panic(errContract)
	}
	requirePinnedExecutable(exe)
	// #nosec G204 G702 -- The executable is an immutable validated Nix-store regular executable; commandShape checks the complete closed role-specific argv and the canonical environment is exact. No shell or caller-supplied command string is constructed.
	return exec.CommandContext(ctx, exe, args...)
}
func gitShape(args []string) bool {
	return reflect.DeepEqual(args, []string{"rev-parse", "--show-toplevel"}) || reflect.DeepEqual(args, []string{"ls-files", "--stage", "--full-name", "-z"}) || reflect.DeepEqual(args, []string{"cat-file", "--batch"})
}
func pinnedGitCommand(ctx context.Context, c config, root string, args ...string) *exec.Cmd {
	if ctx.Err() != nil || !filepath.IsAbs(root) || filepath.Clean(root) != root || !safeParents(root, false) || !gitShape(args) || !strings.HasSuffix(c.Git, "/bin/git") {
		panic(errContract)
	}
	info, e := os.Lstat(root)
	must(e)
	if !info.IsDir() || !owned(info) || info.Mode().Perm()&0022 != 0 {
		panic(errContract)
	}
	requirePinnedExecutable(c.Git)
	// #nosec G204 G702 -- Pinned immutable Git executable, exactly one of three fixed metadata-only argv shapes, validated root, closed gitArgs options and later closed gitEnv. No hooks, shell, filters or pathname-based blob reads are selected.
	return exec.CommandContext(ctx, c.Git, gitArgs(root, args...)...)
}
func legacyPath(path string) bool {
	return storePath(path) && filepath.Base(path) == "endlessfs-test-coverage-fresh"
}
