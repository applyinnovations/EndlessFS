package storageformat

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func TestConsistencyDomainKeysRemainCanonicalAndBounded(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "catalog head", key: DomainCatalogHeadKey().String()},
		{name: "catalog page", key: DomainCatalogPageKey(strings.Repeat("a", 43)).String()},
		{name: "namespace head", key: DomainHeadKey(DomainNamespace, strings.Repeat("u", 128)).String()},
		{name: "control page", key: DomainPageKey(DomainOwnerControl, strings.Repeat("u", 128), strings.Repeat("d", 43)).String()},
		{name: "claim", key: DomainClaimKey(DomainNamespace, strings.Repeat("u", 128), strings.Repeat("i", 128)).String()},
		{name: "projection head", key: ProjectionHeadKey(strings.Repeat("u", 128), ProjectionDuplicates).String()},
		{name: "projection page", key: ProjectionPageKey(strings.Repeat("u", 128), ProjectionAdminUsers, strings.Repeat("d", 43)).String()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.key) > 240 {
				t.Fatalf("key length=%d, want <= 240: %s", len(test.key), test.key)
			}
			if strings.HasPrefix(test.key, "/") || strings.HasSuffix(test.key, "/") {
				t.Fatalf("non-canonical key %q", test.key)
			}
			for _, character := range test.key {
				if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '/' || character == '-' || character == '.' {
					continue
				}
				t.Fatalf("key %q contains forbidden character %q", test.key, character)
			}
		})
	}
}

func TestConsistencyDomainRecordsValidateBoundsAndIntegrity(t *testing.T) {
	leaf := DomainPage{
		SchemaVersion: 1,
		DomainID:      "owner-a",
		Kind:          DomainNamespace,
		Level:         0,
		Entries: []DomainEntry{
			{Key: "alpha", Value: []byte(`{"value":1}`), LogicalVersion: "version-a"},
			{Key: "beta", Value: []byte(`{"value":2}`), LogicalVersion: "version-b"},
		},
	}
	leafBody, err := EncodeCanonical(leaf)
	if err != nil {
		t.Fatal(err)
	}
	leafDigest := Digest(leafBody)
	if err := ValidateDomainPage(leaf, leafDigest); err != nil {
		t.Fatalf("ValidateDomainPage(valid leaf) = %v", err)
	}

	head := DomainHead{
		SchemaVersion: 1,
		DomainID:      "owner-a",
		Kind:          DomainNamespace,
		Revision:      2,
		BaseRevision:  1,
		Base: DomainTreeRoot{
			Digest: leafDigest, Level: 0, EntryCount: 2,
		},
		Deltas: []DomainDelta{{
			MutationID: "mutation-a", Fingerprint: Digest([]byte("fingerprint-a")), Revision: 2,
			Changes: []DomainChange{{Key: "gamma", Value: []byte(`{"value":3}`), LogicalVersion: "version-c"}},
			Result:  []byte(`{"committed":true}`),
		}},
	}
	if err := ValidateDomainHead(head); err != nil {
		t.Fatalf("ValidateDomainHead(valid) = %v", err)
	}

	badOrder := leaf
	badOrder.Entries = append([]DomainEntry(nil), leaf.Entries...)
	badOrder.Entries[0], badOrder.Entries[1] = badOrder.Entries[1], badOrder.Entries[0]
	if err := ValidateDomainPage(badOrder, Digest(leafBody)); err == nil {
		t.Fatal("out-of-order/corrupt domain page was accepted")
	}

	oversized := head
	oversized.Deltas = []DomainDelta{{MutationID: "large", Fingerprint: Digest([]byte("large")), Revision: 2, Changes: []DomainChange{{Key: "large", Value: bytes.Repeat([]byte("x"), MaxCanonicalBytes), LogicalVersion: "version-large"}}}}
	if err := ValidateDomainHead(oversized); err == nil {
		t.Fatal("oversized domain delta window was accepted")
	}
}

func TestConsistencyDomainCanonicalBodiesContainNoProviderNativeFields(t *testing.T) {
	records := []any{
		DomainHead{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainNamespace, Revision: 1, BaseRevision: 1, Base: DomainTreeRoot{Digest: "digest"}},
		DomainClaim{SchemaVersion: 1, DomainID: "owner-a", MutationID: "mutation", Fingerprint: "fingerprint", State: DomainClaimPrepared},
		DomainCatalogHead{SchemaVersion: 1, Revision: 1, Root: DomainTreeRoot{Digest: "digest"}},
		ProjectionHead{SchemaVersion: 1, OwnerID: "owner-a", Kind: ProjectionDuplicates, SourceRevision: 1, Root: DomainTreeRoot{Digest: "digest"}},
	}
	for _, record := range records {
		body, err := EncodeCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
		lower := bytes.ToLower(body)
		for _, forbidden := range []string{"generation", "etag", "versionid", "bucket", "container", "signedurl", "uploadsession", "multipart", "blockid"} {
			if bytes.Contains(lower, []byte(forbidden)) {
				t.Fatalf("canonical domain body contains provider-native field %q: %s", forbidden, body)
			}
		}
	}
}

func TestConsistencyDomainAuxiliaryRecordsValidateAndDenyCorruption(t *testing.T) {
	leaf := DomainPage{
		SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Level: 0,
		Entries: []DomainEntry{{Key: "alpha", Value: []byte("a"), LogicalVersion: "version-a"}},
	}
	leafBody, err := EncodeCanonical(leaf)
	if err != nil {
		t.Fatal(err)
	}
	leafDigest := Digest(leafBody)
	branch := DomainPage{
		SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Level: 1,
		Children: []DomainPageChild{{FirstKey: "alpha", LastKey: "alpha", Digest: leafDigest, Level: 0, EntryCount: 1, ByteCount: 1}},
	}
	branchBody, err := EncodeCanonical(branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDomainPage(branch, Digest(branchBody)); err != nil {
		t.Fatalf("ValidateDomainPage(valid branch) = %v", err)
	}

	compacted := DomainHead{
		SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl,
		Revision: 3, BaseRevision: 3, Frozen: true, FreezeEpoch: 7,
		Base: DomainTreeRoot{Digest: leafDigest, Level: 0, EntryCount: 1, ByteCount: 1},
	}
	if err := ValidateDomainHead(compacted); err != nil {
		t.Fatalf("ValidateDomainHead(valid compacted head) = %v", err)
	}

	prepared := DomainClaim{SchemaVersion: 1, DomainID: "owner-a", MutationID: "mutation-a", Fingerprint: Digest([]byte("intent")), State: DomainClaimPrepared}
	committed := prepared
	committed.State, committed.Revision, committed.Result = DomainClaimCommitted, 3, []byte("result")
	failed := prepared
	failed.State = DomainClaimFailed
	for _, claim := range []DomainClaim{prepared, committed, failed} {
		if err := ValidateDomainClaim(claim); err != nil {
			t.Fatalf("ValidateDomainClaim(%+v) = %v", claim, err)
		}
	}

	catalogPage := DomainCatalogPage{SchemaVersion: 1, Entries: []DomainCatalogEntry{
		{DomainID: "global", Kind: DomainAdmin, HeadKey: DomainHeadKey(DomainAdmin, "global").String()},
		{DomainID: "owner-a", Kind: DomainNamespace, HeadKey: DomainHeadKey(DomainNamespace, "owner-a").String()},
	}}
	catalogBody, err := EncodeCanonical(catalogPage)
	if err != nil {
		t.Fatal(err)
	}
	catalogDigest := Digest(catalogBody)
	if err := ValidateDomainCatalogPage(catalogPage, catalogDigest); err != nil {
		t.Fatalf("ValidateDomainCatalogPage(valid) = %v", err)
	}
	catalogHead := DomainCatalogHead{SchemaVersion: 1, Revision: 1, Root: DomainTreeRoot{Digest: catalogDigest, EntryCount: 2}}
	if err := ValidateDomainCatalogHead(catalogHead); err != nil {
		t.Fatalf("ValidateDomainCatalogHead(valid) = %v", err)
	}
	for _, kind := range []ProjectionKind{ProjectionDuplicates, ProjectionAdminUsers, ProjectionAccounting, ProjectionSearch} {
		head := ProjectionHead{SchemaVersion: 1, OwnerID: "owner-a", Kind: kind, SourceRevision: 3, Root: DomainTreeRoot{Digest: leafDigest, EntryCount: 1}}
		if err := ValidateProjectionHead(head); err != nil {
			t.Fatalf("ValidateProjectionHead(%q) = %v", kind, err)
		}
	}

	invalidHeads := []DomainHead{
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Revision: 1, BaseRevision: 1, Frozen: true},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Revision: 1, BaseRevision: 2},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Revision: 2, BaseRevision: 0, Deltas: []DomainDelta{{MutationID: "gap", Fingerprint: Digest([]byte("gap")), Revision: 2, Changes: []DomainChange{{Key: "key", Value: []byte("value"), LogicalVersion: "version"}}}}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Revision: 2, BaseRevision: 1, Deltas: []DomainDelta{{MutationID: "bad-change", Fingerprint: Digest([]byte("change")), Revision: 2, Changes: []DomainChange{{Key: "key", Delete: true, Value: []byte("unexpected")}}}}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Revision: 3, BaseRevision: 1, Deltas: []DomainDelta{{MutationID: "short", Fingerprint: Digest([]byte("short")), Revision: 2, Changes: []DomainChange{{Key: "key", Value: []byte("value"), LogicalVersion: "version"}}}}},
	}
	for index, head := range invalidHeads {
		if err := ValidateDomainHead(head); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid head %d error = %v", index, err)
		}
	}

	badLeaf := leaf
	badLeaf.Children = []DomainPageChild{{FirstKey: "x", LastKey: "x", Digest: leafDigest, EntryCount: 1}}
	badBranch := branch
	badBranch.Entries = []DomainEntry{{Key: "x", LogicalVersion: "v"}}
	badChild := branch
	badChild.Children = []DomainPageChild{{FirstKey: "z", LastKey: "a", Digest: "not-a-digest", EntryCount: 0}}
	for index, test := range []struct {
		page   DomainPage
		digest string
	}{{page: badLeaf, digest: leafDigest}, {page: badBranch, digest: Digest(branchBody)}, {page: badChild, digest: Digest(branchBody)}, {page: leaf, digest: Digest([]byte("wrong"))}} {
		if err := ValidateDomainPage(test.page, test.digest); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid page %d error = %v", index, err)
		}
	}

	badClaim := prepared
	badClaim.Result = []byte("result-before-commit")
	if err := ValidateDomainClaim(badClaim); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("prepared claim with result error = %v", err)
	}
	badCatalog := catalogPage
	badCatalog.Entries = append([]DomainCatalogEntry(nil), catalogPage.Entries...)
	badCatalog.Entries[0].HeadKey = DomainHeadKey(DomainAdmin, "other").String()
	if err := ValidateDomainCatalogPage(badCatalog, catalogDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("misbound catalog entry error = %v", err)
	}
	badCatalog = catalogPage
	badCatalog.Entries = []DomainCatalogEntry{catalogPage.Entries[1], catalogPage.Entries[0]}
	if err := ValidateDomainCatalogPage(badCatalog, catalogDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unordered catalog error = %v", err)
	}
	if err := ValidateDomainCatalogHead(DomainCatalogHead{SchemaVersion: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid catalog head error = %v", err)
	}
	if err := ValidateDomainCatalogHead(DomainCatalogHead{SchemaVersion: 1, Revision: 1, Root: DomainTreeRoot{Digest: "not-a-digest", EntryCount: 1}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("catalog head with invalid root error = %v", err)
	}
	if err := ValidateDomainCatalogPage(DomainCatalogPage{SchemaVersion: 1}, catalogDigest); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty catalog page error = %v", err)
	}
	if err := ValidateDomainCatalogPage(catalogPage, Digest([]byte("wrong-catalog"))); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("catalog page digest mismatch error = %v", err)
	}
	if err := ValidateProjectionHead(ProjectionHead{SchemaVersion: 1, OwnerID: "owner-a", Kind: "unknown", SourceRevision: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid projection head error = %v", err)
	}
	if err := ValidateProjectionHead(ProjectionHead{SchemaVersion: 1, OwnerID: "owner-a", Kind: ProjectionSearch, SourceRevision: 1, Root: DomainTreeRoot{Digest: "not-a-digest", EntryCount: 1}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("projection head with invalid root error = %v", err)
	}
}

func TestConsistencyDomainKeyHelpersRejectUnknownKindsAndInvalidText(t *testing.T) {
	if DomainPrefix() != "endlessfs/v1/domains/" || ProjectionPrefix() != "endlessfs/v1/projections/" {
		t.Fatalf("unexpected domain prefixes %q %q", DomainPrefix(), ProjectionPrefix())
	}
	for _, call := range []func(){
		func() { _ = DomainHeadKey("unknown", "owner") },
		func() { _ = ProjectionHeadKey("owner", "unknown") },
		func() { _ = DomainHeadKey(DomainNamespace, "") },
		func() { _ = DomainClaimKey(DomainNamespace, "owner", "bad\x00id") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid consistency-domain key did not panic")
				}
			}()
			call()
		}()
	}
}
