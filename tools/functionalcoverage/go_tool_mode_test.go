package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func modeFixture(c config) map[string]commandResult {
	start := time.Date(2026, time.October, 19, 0, 0, 0, 0, time.UTC)
	ready := cleanupObservation{Adequate: true, NaturalZero: true, FinalZero: true, InitialScanComplete: true, FinalScanComplete: true}
	return map[string]commandResult{
		"go-tool-mode-off": {Argv: append([]string{c.Go}, goToolSetupArgv()...), StartedUTC: start, FinishedUTC: start.Add(time.Second), Cleanup: ready},
		"go-environment":   {Argv: append([]string{c.Go}, goEnvironmentArgv()...), StartedUTC: start.Add(time.Second), FinishedUTC: start.Add(2 * time.Second), Cleanup: ready},
	}
}
func TestFunctionalCoverageGoToolModePolicyAndIdentity(t *testing.T) {
	if goToolSetupPolicy != "private-go-home-telemetry-off-before-first-go-command-v1" || !reflect.DeepEqual(goToolSetupArgv(), []string{"telemetry", "off"}) {
		t.Fatal("private Go-tool mode policy changed")
	}
	c := config{Go: "/pinned/bin/go"}
	base := inputIdentity{GoToolSetupPolicy: goToolSetupPolicy, GoToolSetupArgv: append([]string{c.Go}, goToolSetupArgv()...)}
	for _, kind := range []string{"policy", "executable", "mode"} {
		body, e := json.Marshal(base)
		if e != nil {
			t.Fatal(e)
		}
		var changed inputIdentity
		decodeJSON(body, &changed)
		switch kind {
		case "policy":
			changed.GoToolSetupPolicy = "different"
		case "executable":
			changed.GoToolSetupArgv[0] = "foreign"
		case "mode":
			changed.GoToolSetupArgv[2] = "local"
		}
		if canonical(base) == canonical(changed) {
			t.Fatal("Go-tool setup input omitted from identity")
		}
	}
	for _, entry := range environment(c, "/build/private") {
		name, _, _ := strings.Cut(entry, "=")
		if name == "GOTELEMETRY" || name == "GO_TELEMETRY_CHILD" {
			t.Fatal("mode replaced by unsupported telemetry environment hack")
		}
	}
}
func TestFunctionalCoverageGoToolModeCommandReceiptDenials(t *testing.T) {
	c := config{Go: "/pinned/bin/go"}
	requireGoToolModeCommands(c, modeFixture(c))
	for _, kind := range []string{"missing-mode", "wrong-executable", "wrong-mode", "extra-argument", "exit", "left-child", "capped", "io-failed", "missing-observation", "query-omits-mode", "observation-before-off", "later-go-before-off"} {
		t.Run(kind, func(t *testing.T) {
			commands := modeFixture(c)
			mode := commands["go-tool-mode-off"]
			observed := commands["go-environment"]
			switch kind {
			case "wrong-executable":
				mode.Argv[0] = "foreign"
			case "wrong-mode":
				mode.Argv[2] = "local"
			case "extra-argument":
				mode.Argv = append(mode.Argv, "extra")
			case "exit":
				mode.ExitCode = 1
			case "left-child":
				mode.Cleanup.NaturalZero = false
			case "capped":
				mode.Capped = true
			case "io-failed":
				mode.IOFailed = true
			case "query-omits-mode":
				observed.Argv = observed.Argv[:len(observed.Argv)-1]
			case "observation-before-off":
				observed.StartedUTC = mode.StartedUTC
			case "later-go-before-off":
				commands["inventory"] = commandResult{StartedUTC: mode.StartedUTC}
			}
			commands["go-tool-mode-off"], commands["go-environment"] = mode, observed
			if kind == "missing-mode" {
				delete(commands, "go-tool-mode-off")
			}
			if kind == "missing-observation" {
				delete(commands, "go-environment")
			}
			expectContract(t, func() { requireGoToolModeCommands(c, commands) })
		})
	}
}
func TestFunctionalCoverageObservedGoToolModeMustActuallyBeOff(t *testing.T) {
	c := config{GoRoot: "/go/share/go", GoVersion: "1.26.9", Native: "/native/bin/cc"}
	work := "/build/private"
	values := map[string]string{"GOROOT": c.GoRoot, "GOVERSION": "go1.26.9", "GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1", "CGO_ENABLED": "0", "CC": c.Native, "CXX": "/native/bin/c++", "GOENV": "", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "-mod=vendor", "GOCACHE": filepath.Join(work, "go-cache"), "GOTELEMETRY": "off"}
	if !effectiveEnvironmentValid(c, work, values) {
		t.Fatal("observed private mode off rejected")
	}
	for _, mode := range []string{"local", "on", "", "OFF"} {
		values["GOTELEMETRY"] = mode
		if effectiveEnvironmentValid(c, work, values) {
			t.Fatal("unverified telemetry mode admitted")
		}
	}
	delete(values, "GOTELEMETRY")
	if effectiveEnvironmentValid(c, work, values) {
		t.Fatal("missing mode observation admitted")
	}
}
