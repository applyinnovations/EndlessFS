package portable

import (
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestSchema008NamespaceBatchFingerprintBindsOrderAndIntent(t *testing.T) {
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	live, _ := domain.NewScope(owner, domain.AreaLive)
	specs := []namespaceBatchMoveSpec{
		{from: live, to: live, request: domain.CopyRequest{Source: domain.MustParseUserPath("/a"), Destination: domain.MustParseUserPath("/b")}},
		{from: live, to: live, request: domain.CopyRequest{Source: domain.MustParseUserPath("/c"), Destination: domain.MustParseUserPath("/d"), Conflict: domain.ConflictReplace}},
	}
	first, err := namespaceBatchFingerprint("copy", specs)
	if err != nil {
		t.Fatal(err)
	}
	reversed := append([]namespaceBatchMoveSpec(nil), specs...)
	reversed[0], reversed[1] = reversed[1], reversed[0]
	second, err := namespaceBatchFingerprint("copy", reversed)
	if err != nil || first == second {
		t.Fatalf("ordered fingerprints first=%q second=%q error=%v", first, second, err)
	}
	if err := rejectOverlappingNamespaceBatchPaths([]namespaceBatchMovePlan{{spec: specs[0], resolved: domain.MustParseUserPath("/parent")}, {spec: specs[1], resolved: domain.MustParseUserPath("/parent/child")}}, false); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("ancestor overlap error = %v", err)
	}
	if err := validatePortableIdempotencyKey(strings.Repeat("x", 129)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized idempotency key error = %v", err)
	}
}
