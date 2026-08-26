package storageformat

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

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
		{name: "snapshot", key: DomainSnapshotKey(DomainOwnerControl, strings.Repeat("u", 128), strings.Repeat("d", 43)).String()},
		{name: "projection head", key: ProjectionHeadKey(strings.Repeat("u", 128), ProjectionDuplicates).String()},
		{name: "projection page", key: ProjectionPageKey(strings.Repeat("u", 128), ProjectionAdminUsers, strings.Repeat("d", 43)).String()},
		{name: "operation preparation page", key: FileOperationPreparationPageKey("owner-a", "operation-a", "run-set-a", 1, 2, 3).String()},
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

func TestSchema008PrefixHelpersStayInsideCanonicalNamespaces(t *testing.T) {
	prefixes := []string{
		StateQuerySnapshotPrefix(),
		Schema008MigrationStageDomainPrefix("namespace:owner-a"),
		Schema008MigrationSourceMarkerPrefix(),
		Schema008MigrationSubtreePrefix(),
		DuplicateOccurrenceGroupPrefix("owner-a", "file", "group-a"),
		DuplicateSummaryPrefix("owner-a", "file"),
		DuplicateSimilarityPostingPrefix("owner-a", 7, "sketch-a"),
		GarbageCollectionMarkPrefix("checkpoint-a"),
	}
	for _, prefix := range prefixes {
		if !strings.HasPrefix(prefix, root) || !strings.HasSuffix(prefix, "/") {
			t.Fatalf("non-canonical prefix %q", prefix)
		}
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
		Registered:    true,
		Revision:      2,
		BaseRevision:  1,
		Base: DomainTreeRoot{
			Digest: leafDigest, Level: 0, EntryCount: 2,
		},
		Deltas: []DomainDelta{{
			MutationID: "mutation-a", Fingerprint: Digest([]byte("fingerprint-a")), Revision: 2,
			RetainUntil: time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC),
			Changes:     []DomainChange{{Key: "gamma", Value: []byte(`{"value":3}`), LogicalVersion: "version-c"}},
			Result:      []byte(`{"committed":true}`),
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
		DomainHead{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainNamespace, Registered: true, Revision: 1, BaseRevision: 1, Base: DomainTreeRoot{Digest: "digest"}},
		DomainOutcome{MutationID: "mutation", Fingerprint: "fingerprint", Revision: 1},
		DomainCatalogHead{SchemaVersion: 1, Revision: 1, Root: DomainTreeRoot{Digest: "digest"}},
		ProjectionHead{SchemaVersion: 1, OwnerID: "owner-a", ProjectionID: "projection-a", Kind: ProjectionDuplicates, SourceDomainID: "owner-a", SourceRevision: 1, Root: DomainTreeRoot{Digest: "digest"}},
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
		Registered: true, Revision: 3, BaseRevision: 3, Frozen: true, FreezeEpoch: 7,
		Base: DomainTreeRoot{Digest: leafDigest, Level: 0, EntryCount: 1, ByteCount: 1},
	}
	if err := ValidateDomainHead(compacted); err != nil {
		t.Fatalf("ValidateDomainHead(valid compacted head) = %v", err)
	}

	outcome := DomainOutcome{MutationID: "mutation-a", Fingerprint: Digest([]byte("intent")), Revision: 3, RetainUntil: time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC), Result: []byte("result")}
	if err := ValidateDomainOutcome(outcome); err != nil {
		t.Fatalf("ValidateDomainOutcome(%+v) = %v", outcome, err)
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
	for _, kind := range []ProjectionKind{ProjectionDuplicates, ProjectionAdminUsers, ProjectionAccounting, ProjectionSearch, ProjectionModified, ProjectionSize, ProjectionEntryKind} {
		head := ProjectionHead{SchemaVersion: 1, OwnerID: "owner-a", ProjectionID: "projection-a", Kind: kind, SourceDomainID: "owner-a", SourceRevision: 3, Root: DomainTreeRoot{Digest: leafDigest, EntryCount: 1}}
		if err := ValidateProjectionHead(head); err != nil {
			t.Fatalf("ValidateProjectionHead(%q) = %v", kind, err)
		}
	}

	invalidHeads := []DomainHead{
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 1, BaseRevision: 2},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 2, BaseRevision: 0, Deltas: []DomainDelta{{MutationID: "gap", Fingerprint: Digest([]byte("gap")), Revision: 2, Changes: []DomainChange{{Key: "key", Value: []byte("value"), LogicalVersion: "version"}}}}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 2, BaseRevision: 1, Deltas: []DomainDelta{{MutationID: "bad-change", Fingerprint: Digest([]byte("change")), Revision: 2, Changes: []DomainChange{{Key: "key", Delete: true, Value: []byte("unexpected")}}}}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 3, BaseRevision: 1, Deltas: []DomainDelta{{MutationID: "short", Fingerprint: Digest([]byte("short")), Revision: 2, Changes: []DomainChange{{Key: "key", Value: []byte("value"), LogicalVersion: "version"}}}}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 1, BaseRevision: 1, Outcomes: DomainTreeRoot{Digest: "invalid", EntryCount: 1}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 1, BaseRevision: 1, OutcomeExpiry: DomainTreeRoot{Digest: "invalid", EntryCount: 1}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 0, Base: DomainTreeRoot{Digest: leafDigest, EntryCount: 1}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 2, BaseRevision: 1, Deltas: []DomainDelta{{MutationID: "empty-change-version", Fingerprint: Digest([]byte("empty-change-version")), Revision: 2, RetainUntil: time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC), Changes: []DomainChange{{Key: "key"}}}}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 2, BaseRevision: 1, Deltas: []DomainDelta{{MutationID: "delete-version", Fingerprint: Digest([]byte("delete-version")), Revision: 2, RetainUntil: time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC), Changes: []DomainChange{{Key: "key", Delete: true, LogicalVersion: "unexpected"}}}}},
		{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 2, BaseRevision: 1, Deltas: []DomainDelta{{MutationID: "unordered", Fingerprint: Digest([]byte("unordered")), Revision: 2, RetainUntil: time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC), Changes: []DomainChange{{Key: "z", Value: []byte("z"), LogicalVersion: "v-z"}, {Key: "a", Value: []byte("a"), LogicalVersion: "v-a"}}}}},
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

	badOutcome := outcome
	badOutcome.Revision = 0
	if err := ValidateDomainOutcome(badOutcome); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid outcome error = %v", err)
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
	if err := ValidateProjectionHead(ProjectionHead{SchemaVersion: 1, OwnerID: "owner-a", ProjectionID: "projection-a", Kind: "unknown", SourceDomainID: "owner-a", SourceRevision: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid projection head error = %v", err)
	}
	if err := ValidateProjectionHead(ProjectionHead{SchemaVersion: 1, OwnerID: "owner-a", ProjectionID: "projection-a", Kind: ProjectionSearch, SourceDomainID: "owner-a", SourceRevision: 1, Root: DomainTreeRoot{Digest: "not-a-digest", EntryCount: 1}}); !errors.Is(err, domain.ErrInvalid) {
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
		func() { _ = DomainPageKey(DomainNamespace, "owner", "bad\x00id") },
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

func TestSchema007IdempotencyDigestReconstructsAndAuthenticatesLegacyKey(t *testing.T) {
	userID, original := "owner-a", "client-idempotency-key"
	digest := Digest([]byte(original))
	key, err := Schema007IdempotencyKeyFromDigest(userID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if key != IdempotencyKey(userID, original) {
		t.Fatalf("reconstructed key = %s, want %s", key, IdempotencyKey(userID, original))
	}
	for _, invalid := range []string{"", "not-base64", Digest([]byte("wrong"))[:20]} {
		if _, err := Schema007IdempotencyKeyFromDigest(userID, invalid); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("digest %q error = %v", invalid, err)
		}
	}
}

func TestConsistencyDomainInitialAndSnapshotAuthorityValidationMatrix(t *testing.T) {
	now := time.Date(2062, 3, 4, 5, 6, 7, 0, time.UTC)
	initial := DomainHead{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl}
	if err := ValidateInitialDomainHead(initial); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DomainHead){
		"registered":    func(value *DomainHead) { value.Registered = true },
		"schema":        func(value *DomainHead) { value.SchemaVersion = 0 },
		"domain":        func(value *DomainHead) { value.DomainID = "" },
		"kind":          func(value *DomainHead) { value.Kind = "unknown" },
		"revision":      func(value *DomainHead) { value.Revision = 1 },
		"base-revision": func(value *DomainHead) { value.BaseRevision = 1 },
		"frozen":        func(value *DomainHead) { value.Frozen = true },
		"freeze-epoch":  func(value *DomainHead) { value.FreezeEpoch = 1 },
		"base":          func(value *DomainHead) { value.Base.Digest = Digest([]byte("base")) },
		"outcomes":      func(value *DomainHead) { value.Outcomes.Digest = Digest([]byte("outcomes")) },
		"expiry":        func(value *DomainHead) { value.OutcomeExpiry.Digest = Digest([]byte("expiry")) },
		"delta": func(value *DomainHead) {
			value.Deltas = []DomainDelta{{MutationID: "mutation"}}
		},
	} {
		t.Run("invalid-initial-"+name, func(t *testing.T) {
			value := initial
			mutate(&value)
			if !errors.Is(ValidateInitialDomainHead(value), domain.ErrInvalid) {
				t.Fatalf("initial head %q was accepted", name)
			}
		})
	}

	current := DomainHead{SchemaVersion: 1, DomainID: "owner-a", Kind: DomainOwnerControl, Registered: true, Revision: 1, BaseRevision: 1}
	snapshot := DomainSnapshot{SchemaVersion: 1, DomainID: current.DomainID, Kind: current.Kind, Head: current, ExpiresAt: now}
	if err := ValidateDomainSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DomainSnapshot){
		"schema":         func(value *DomainSnapshot) { value.SchemaVersion = 0 },
		"expiry":         func(value *DomainSnapshot) { value.ExpiresAt = time.Time{} },
		"domain-binding": func(value *DomainSnapshot) { value.DomainID = "other" },
		"kind-binding":   func(value *DomainSnapshot) { value.Kind = DomainNamespace },
		"head":           func(value *DomainSnapshot) { value.Head.Registered = false },
	} {
		t.Run("invalid-snapshot-"+name, func(t *testing.T) {
			value := snapshot
			mutate(&value)
			if !errors.Is(ValidateDomainSnapshot(value), domain.ErrInvalid) {
				t.Fatalf("domain snapshot %q was accepted", name)
			}
		})
	}

	query := StateQuerySnapshot{SchemaVersion: 1, Prefix: "accounts/", DomainID: "query", Root: DomainTreeRoot{Digest: Digest([]byte("query")), EntryCount: 1}, ExpiresAt: now}
	if err := ValidateStateQuerySnapshot(query); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*StateQuerySnapshot){
		"schema": func(value *StateQuerySnapshot) { value.SchemaVersion = 0 },
		"prefix": func(value *StateQuerySnapshot) { value.Prefix = "" },
		"domain": func(value *StateQuerySnapshot) { value.DomainID = "" },
		"expiry": func(value *StateQuerySnapshot) { value.ExpiresAt = time.Time{} },
		"root":   func(value *StateQuerySnapshot) { value.Root.Digest = "invalid" },
	} {
		t.Run("invalid-query-"+name, func(t *testing.T) {
			value := query
			mutate(&value)
			if !errors.Is(ValidateStateQuerySnapshot(value), domain.ErrInvalid) {
				t.Fatalf("state query snapshot %q was accepted", name)
			}
		})
	}
}

func TestConsistencyDomainRevisionEncodingAndRootBindingDenials(t *testing.T) {
	now := time.Date(2064, 5, 6, 7, 8, 9, 0, time.UTC)
	delta := DomainDelta{
		MutationID: "mutation", Fingerprint: Digest([]byte("fingerprint")), Revision: 2, RetainUntil: now.Add(time.Hour),
		Changes: []DomainChange{{Key: "key", Value: []byte("value"), LogicalVersion: "version"}},
	}
	if err := ValidateDomainHead(DomainHead{SchemaVersion: 1, DomainID: "owner", Kind: DomainOwnerControl, Registered: true, Revision: 3, BaseRevision: 1, Deltas: []DomainDelta{delta}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("delta-window revision mismatch error = %v", err)
	}
	if err := ValidateDomainHead(DomainHead{SchemaVersion: 1, DomainID: "owner", Kind: DomainOwnerControl, Registered: true, Revision: 2, BaseRevision: 1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("base revision mismatch error = %v", err)
	}

	oversizedPage := DomainPage{SchemaVersion: 1, DomainID: "owner", Kind: DomainOwnerControl, Entries: []DomainEntry{{Key: "key", Value: bytes.Repeat([]byte("x"), MaxCanonicalBytes), LogicalVersion: "version"}}}
	if err := ValidateDomainPage(oversizedPage, Digest([]byte("expected"))); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized domain page error = %v", err)
	}
	oversizedCatalog := DomainCatalogPage{SchemaVersion: 1, Entries: []DomainCatalogEntry{{DomainID: strings.Repeat("x", MaxCanonicalBytes), Kind: DomainAdmin, HeadKey: DomainHeadKey(DomainAdmin, strings.Repeat("x", MaxCanonicalBytes)).String()}}}
	if err := ValidateDomainCatalogPage(oversizedCatalog, Digest([]byte("expected"))); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized catalog page error = %v", err)
	}
	projection := ProjectionHead{SchemaVersion: 1, OwnerID: "owner", ProjectionID: "projection", Kind: ProjectionSearch, SourceDomainID: "source", SourceRevision: 1, SourceRoot: DomainTreeRoot{EntryCount: 1}}
	if err := ValidateProjectionHead(projection); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid projection source root error = %v", err)
	}

	if _, err := EncodeEnvelope("test-v1", SuperblockKey(), 1, make(chan int)); err == nil {
		t.Fatal("unsupported envelope payload encoded")
	}
}
