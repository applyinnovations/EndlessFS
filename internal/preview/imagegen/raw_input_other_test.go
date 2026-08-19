//go:build !linux

package imagegen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAnonymousRawInputRejectsUnavailableTemporaryDirectory(t *testing.T) {
	notDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", notDirectory)
	if input, _, err := anonymousRawInput([]byte("raw")); err == nil {
		_ = input.Close()
		t.Fatal("anonymous RAW input accepted an unavailable temporary directory")
	}
}
