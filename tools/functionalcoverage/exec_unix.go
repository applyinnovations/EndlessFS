//go:build linux || darwin

package main

import "syscall"

func execLegacy(path string, args, env []string) int {
	if !legacyPath(path) {
		panic(errContract)
	}
	requirePinnedExecutable(path)
	// #nosec G204 G702 -- Only the immutable Nix-store original fresh coverage program is accepted; argv is passed directly to exec, preserving legacy CLI behavior without constructing a shell command. This branch is reachable only before producer realization.
	must(syscall.Exec(path, append([]string{path}, args...), env))
	return 1
}
