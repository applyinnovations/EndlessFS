package portable

import (
	"errors"
	"os"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestCheckpointVisitSetDeduplicatesAcrossMemoryAndMergedRuns(t *testing.T) {
	set, err := newCheckpointVisitSet()
	if err != nil {
		t.Fatal(err)
	}
	directory := set.directory
	t.Cleanup(func() { _ = set.Close() })
	set.limit = 2
	for index := 0; index < 33; index++ {
		value := domainTestKey(index)
		if seen, err := set.Seen(value); err != nil || seen {
			t.Fatalf("first Seen(%q) = %v, %v", value, seen, err)
		}
		if seen, err := set.Seen(value); err != nil || !seen {
			t.Fatalf("duplicate Seen(%q) = %v, %v", value, seen, err)
		}
	}
	nonempty := 0
	for _, path := range set.levels {
		if path != "" {
			nonempty++
		}
	}
	if nonempty > 6 {
		t.Fatalf("visited set retained %d immutable runs for 33 identities", nonempty)
	}
	for index := 0; index < 33; index++ {
		if seen, err := set.Seen(domainTestKey(index)); err != nil || !seen {
			t.Fatalf("reloaded Seen(%d) = %v, %v", index, seen, err)
		}
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint visited workspace still exists: %v", err)
	}
}

func TestCheckpointVisitSetFailsClosedOnCorruptRun(t *testing.T) {
	set, err := newCheckpointVisitSet()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	const name = "corrupt.visited"
	if err := set.root.WriteFile(name, []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	set.levels = []string{name}
	if _, err := set.Seen("new-page"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt visited run error = %v", err)
	}
}
