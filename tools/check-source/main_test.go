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

	violations, err := check(root)
	if err != nil {
		t.Fatalf("check() error = %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("check() violations = %v", violations)
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
