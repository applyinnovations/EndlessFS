package memory

import (
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func FuzzRangeAndContentDisposition(f *testing.F) {
	for _, seed := range []struct {
		value    string
		filename string
		size     int64
	}{
		{"bytes=0-1", "safe.txt", 2},
		{"bytes=-1", "quote\".txt", 5},
		{"bytes=0-999", "line\r\nbreak.txt", 8},
		{"bytes=1-0", "日本語.txt", 8},
		{"bytes=0-1,3-4", "../escape", 8},
	} {
		f.Add(seed.value, seed.filename, seed.size)
	}
	f.Fuzz(func(t *testing.T, value, filename string, size int64) {
		start, end, partial, err := parseRange(value, size)
		if err == nil {
			if size == 0 && (start != 0 || end != -1 || partial) {
				t.Fatalf("empty range invariant failed: %d %d %v", start, end, partial)
			}
			if size > 0 && (start < 0 || end < start || end >= size) {
				t.Fatalf("range escaped object: size=%d start=%d end=%d", size, start, end)
			}
		}
		disposition := safeDisposition(domain.DispositionAttachment, filename)
		if strings.ContainsAny(disposition, "\r\n") {
			t.Fatalf("content disposition contains a line break: %q", disposition)
		}
	})
}
