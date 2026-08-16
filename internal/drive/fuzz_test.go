package drive

import (
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
)

func FuzzShareSubtreeResolution(f *testing.F) {
	for _, seed := range []string{
		"/", "/nested/file.txt", "/../escape", "%2f..%2fescape", "/safe\\escape",
		"//absolute", "/.endlessfs/record", "/Cafe\u0301/file",
	} {
		f.Add(seed)
	}
	record := model.Share{RootPath: domain.MustParseUserPath("/shared/root"), Kind: domain.EntryDirectory}
	f.Fuzz(func(t *testing.T, relative string) {
		path, err := sharedPath(record, relative)
		if err == nil && path != record.RootPath && !path.IsDescendantOf(record.RootPath) {
			t.Fatalf("shared path escaped root: %q -> %q", relative, path.String())
		}
	})
}
