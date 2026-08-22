package portable

import (
	"errors"
	"math"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestDuplicateDirectoryComparisonRejectsByteOverflow(t *testing.T) {
	left := map[string][]domain.DuplicateOccurrence{
		"group": {
			{GroupID: "group", Size: math.MaxInt64},
			{GroupID: "group", Size: math.MaxInt64},
		},
	}
	right := map[string][]domain.DuplicateOccurrence{
		"group": {
			{GroupID: "group", Size: math.MaxInt64},
			{GroupID: "group", Size: math.MaxInt64},
		},
	}
	_, err := compareDirectoryInventories(domain.DuplicateOccurrence{}, domain.DuplicateOccurrence{}, left, right)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("compareDirectoryInventories() error = %v; want invalid overflow", err)
	}
}

func TestDuplicateDirectoryComparisonRejectsZeroByteIdentitySizeMismatch(t *testing.T) {
	left := map[string][]domain.DuplicateOccurrence{"group": {{GroupID: "group", Size: 0}}}
	right := map[string][]domain.DuplicateOccurrence{"group": {{GroupID: "group", Size: 1}}}
	_, err := compareDirectoryInventories(domain.DuplicateOccurrence{}, domain.DuplicateOccurrence{}, left, right)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("compareDirectoryInventories() error = %v; want invalid size mismatch", err)
	}
}
