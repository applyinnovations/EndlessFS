package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckAcceptsGoAndApplicationAssets(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "main.go")
	writeFixture(t, root, "internal/web/static/app.js")
	writeFixture(t, root, "internal/web/static/app.css")

	violations, err := check(root)
	if err != nil {
		t.Fatalf("check() error = %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("check() violations = %v", violations)
	}
}

func TestCheckRejectsForbiddenToolingAndThemeCode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "package.json")
	writeFixture(t, root, "script.py")
	writeFixture(t, root, "themes/example/unsafe.css")

	violations, err := check(root)
	if err != nil {
		t.Fatalf("check() error = %v", err)
	}
	joined := strings.Join(violations, "\n")
	for _, want := range []string{"package.json", "script.py", "theme bundles are data-only"} {
		if !strings.Contains(joined, want) {
			t.Errorf("violations %q do not contain %q", joined, want)
		}
	}
}

func TestCheckSkipsBuildAndVersionControlDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, ".git/generated.py")
	writeFixture(t, root, "result-build/package.json")
	writeFixture(t, root, "vendor/example/dependency/Makefile")
	writeFixture(t, root, "vendor/example/dependency/generator.py")

	violations, err := check(root)
	if err != nil {
		t.Fatalf("check() error = %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("check() violations = %v", violations)
	}
}

func TestCheckRejectsForbiddenIdentityConceptsAndMissingRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "internal", "model", "identity.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("email oauth password"), 0o600); err != nil {
		t.Fatal(err)
	}
	violations, err := check(root)
	if err != nil || len(violations) != 3 {
		t.Fatalf("identity violations = %+v, %v", violations, err)
	}
	if _, err := check(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing source root was accepted")
	}
}

func TestCheckRejectsWallClockFuzzSmokeBudgets(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "flake.nix")
	content := `
fuzztime="''${ENDLESSFS_FUZZTIME:-2s}"
go test ./internal/logging -fuzz '^FuzzStructuredLogRedaction$' -fuzztime 1s
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	violations, err := check(root)
	if err != nil {
		t.Fatalf("check() error = %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "fuzz smoke budgets must use an iteration count") {
		t.Fatalf("violations %q do not reject wall-clock fuzzing", joined)
	}
}

func TestCheckRejectsBrowserRuntimeInsideFlakeChecks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "flake.nix")
	content := `
checks = forAllSystems (system: {
  e2e = pkgs.runCommand "e2e" { } ''
    export ENDLESSFS_RUN_E2E=1
    go test ./internal/e2e
  '';
});
devShells = forAllSystems (system: { });
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	violations, err := check(root)
	if err != nil {
		t.Fatalf("check() error = %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "browser runtime gate must run outside the Nix build sandbox") {
		t.Fatalf("violations %q do not reject sandboxed browser execution", joined)
	}
}

func writeFixture(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
}
