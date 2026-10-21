package main

import (
	"context"
	"path/filepath"
	"reflect"
)

// This is a Go-tool user-configuration mode in a fresh owned HOME, not an
// environment override or a change to EndlessFS application observability.
const goToolSetupPolicy = "private-go-home-telemetry-off-before-first-go-command-v1"

func goToolSetupArgv() []string { return []string{"telemetry", "off"} }
func goEnvironmentArgv() []string {
	return []string{"env", "-json", "GOROOT", "GOVERSION", "GOOS", "GOARCH", "GOAMD64", "CGO_ENABLED", "CC", "CXX", "GOENV", "GOWORK", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GOFLAGS", "GOCACHE", "GOTELEMETRY"}
}
func initializeGoToolMode(ctx context.Context, c config, work, source string, env []string, commands map[string]commandResult) {
	if ctx.Err() != nil {
		panic(errContract)
	}
	commands["go-tool-mode-off"] = runCommand(ctx, c, work, "go-tool-mode-off", c.Go, goToolSetupArgv(), env, source, 1<<20)
	if !commandPassed(commands["go-tool-mode-off"]) {
		panic(errContract)
	}
	commands["go-environment"] = runCommand(ctx, c, work, "go-environment", c.Go, goEnvironmentArgv(), env, source, 1<<20)
	requireGoToolModeCommands(c, commands)
	var values map[string]string
	readJSON(filepath.Join(work, "private-go-environment.stdout"), 1<<20, &values)
	if !effectiveEnvironmentValid(c, work, values) {
		panic(errContract)
	}
}
func requireGoToolModeCommands(c config, commands map[string]commandResult) {
	mode, ok := commands["go-tool-mode-off"]
	if !ok || !commandPassed(mode) || !reflect.DeepEqual(mode.Argv, append([]string{c.Go}, goToolSetupArgv()...)) || mode.StartedUTC.IsZero() || mode.FinishedUTC.Before(mode.StartedUTC) {
		panic(errContract)
	}
	observed, ok := commands["go-environment"]
	if !ok || !commandPassed(observed) || !reflect.DeepEqual(observed.Argv, append([]string{c.Go}, goEnvironmentArgv()...)) || observed.StartedUTC.Before(mode.FinishedUTC) || observed.FinishedUTC.Before(observed.StartedUTC) {
		panic(errContract)
	}
	for _, name := range []string{"seed-install", "inventory", "coverage", "policy"} {
		if next, exists := commands[name]; exists && next.StartedUTC.Before(observed.FinishedUTC) {
			panic(errContract)
		}
	}
}
