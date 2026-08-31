package portable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type uploadPlanningHeadBarrierBackend struct {
	objectstore.Backend
	key      objectstore.Key
	target   int
	mu       sync.Mutex
	arrivals int
	release  chan struct{}
}

func (backend *uploadPlanningHeadBarrierBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if key == backend.key {
		backend.mu.Lock()
		backend.arrivals++
		arrival := backend.arrivals
		if arrival == backend.target {
			close(backend.release)
		}
		release := backend.release
		backend.mu.Unlock()
		if arrival <= backend.target {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-release:
			}
		}
	}
	return backend.Backend.Put(ctx, key, body, condition)
}

func TestUploadPlanningUsesSizeBeforeExactProviderFingerprint(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 2)

	sizes, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{
		{ID: "same-size", Path: domain.MustParseUserPath("/new.bin"), Size: 1},
		{ID: "unique-size", Path: domain.MustParseUserPath("/unique.bin"), Size: 91},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sizes.Token == "" || len(sizes.Items) != 2 || !sizes.Items[0].FingerprintRequired || sizes.Items[1].FingerprintRequired {
		t.Fatalf("PlanUploadSizes() = %+v", sizes)
	}

	exact, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: sizes.Token, Items: []domain.UploadFingerprintPlanItem{
		{ID: "same-size", Path: domain.MustParseUserPath("/new.bin"), Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.Items) != 1 || exact.Items[0].Action != domain.UploadPlanReuse || exact.Items[0].SourcePath == nil || *exact.Items[0].SourcePath != seeded[0].Path || exact.Items[0].SourceVersion != seeded[0].Version {
		t.Fatalf("PlanUploadFingerprints() = %+v; source=%+v", exact, seeded[0])
	}
	different := objectstore.FingerprintFor([]byte("different content"))
	upload, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: sizes.Token, Items: []domain.UploadFingerprintPlanItem{
		{ID: "different", Path: domain.MustParseUserPath("/different.bin"), Size: 1, MD5: different.MD5, CRC32C: different.CRC32C},
	}})
	if err != nil || len(upload.Items) != 1 || upload.Items[0].Action != domain.UploadPlanUpload || upload.Items[0].SourcePath != nil {
		t.Fatalf("unmatched fingerprint plan = %+v, %v", upload, err)
	}
	encoded, err := json.Marshal(exact)
	if err != nil || strings.Contains(string(encoded), "md5") || strings.Contains(string(encoded), "crc32c") || strings.Contains(string(encoded), testProviderMD5) {
		t.Fatalf("fingerprint response exposed lookup inputs: %s, %v", encoded, err)
	}
}

func TestUploadPlanningValidatesBoundsIDsPathsFingerprintsAndExpiry(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 1)
	files := engine.Files()

	tooManySizes := make([]domain.UploadSizePlanItem, maximumUploadPlanItems+1)
	tooManyFingerprints := make([]domain.UploadFingerprintPlanItem, maximumUploadPlanItems+1)
	for index := range tooManySizes {
		id := fmt.Sprintf("item-%04d", index)
		path := domain.MustParseUserPath(fmt.Sprintf("/item-%04d.bin", index))
		tooManySizes[index] = domain.UploadSizePlanItem{ID: id, Path: path, Size: 1}
		tooManyFingerprints[index] = domain.UploadFingerprintPlanItem{ID: id, Path: path, Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}
	}
	for name, request := range map[string]domain.UploadSizePlanRequest{
		"empty":         {},
		"too many":      {Items: tooManySizes},
		"duplicate ID":  {Items: []domain.UploadSizePlanItem{{ID: "same", Path: domain.MustParseUserPath("/a"), Size: 1}, {ID: "same", Path: domain.MustParseUserPath("/b"), Size: 2}}},
		"unsafe ID":     {Items: []domain.UploadSizePlanItem{{ID: "line\nbreak", Path: domain.MustParseUserPath("/a"), Size: 1}}},
		"root path":     {Items: []domain.UploadSizePlanItem{{ID: "root", Path: domain.MustParseUserPath("/"), Size: 1}}},
		"negative size": {Items: []domain.UploadSizePlanItem{{ID: "negative", Path: domain.MustParseUserPath("/a"), Size: -1}}},
	} {
		t.Run("size "+name, func(t *testing.T) {
			if _, err := files.PlanUploadSizes(ctx, live.UserID(), request); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("PlanUploadSizes() error = %v", err)
			}
		})
	}
	valid, err := files.PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "valid", Path: domain.MustParseUserPath("/incoming"), Size: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]domain.UploadFingerprintPlanRequest{
		"empty":            {Token: valid.Token},
		"missing token":    {Items: tooManyFingerprints[:1]},
		"too many":         {Token: valid.Token, Items: tooManyFingerprints},
		"duplicate ID":     {Token: valid.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "same", Path: domain.MustParseUserPath("/a"), Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}, {ID: "same", Path: domain.MustParseUserPath("/b"), Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}}},
		"invalid checksum": {Token: valid.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "bad", Path: domain.MustParseUserPath("/a"), Size: 1, MD5: testProviderMD5, CRC32C: "bad"}}},
		"corrupt token":    {Token: "not-an-upload-plan", Items: tooManyFingerprints[:1]},
	} {
		t.Run("fingerprint "+name, func(t *testing.T) {
			if _, err := files.PlanUploadFingerprints(ctx, live.UserID(), request); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("PlanUploadFingerprints() error = %v", err)
			}
		})
	}
	engine.clock.(*domain.FixedClock).Advance(time.Hour)
	if _, err := files.PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: valid.Token, Items: tooManyFingerprints[:1]}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("expired upload plan token error = %v", err)
	}
}

func TestUploadPlanningSupportsAnEmptyNamespaceAndFailsClosedForCorruptDerivedState(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	owner, err := domain.ParseUserID("ZW1wdHktdXBsb2FkLXBsYW5uZXI")
	if err != nil {
		t.Fatal(err)
	}
	files := engine.Files()
	empty, err := files.PlanUploadSizes(ctx, owner, domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "empty", Path: domain.MustParseUserPath("/new.bin"), Size: 9}}})
	if err != nil || len(empty.Items) != 1 || empty.Items[0].FingerprintRequired || empty.Token == "" {
		t.Fatalf("empty namespace size plan = %+v, %v", empty, err)
	}
	fingerprint := objectstore.FingerprintFor([]byte("123456789"))
	exact, err := files.PlanUploadFingerprints(ctx, owner, domain.UploadFingerprintPlanRequest{Token: empty.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "empty", Path: domain.MustParseUserPath("/new.bin"), Size: 9, MD5: fingerprint.MD5, CRC32C: fingerprint.CRC32C}}})
	if err != nil || len(exact.Items) != 1 || exact.Items[0].Action != domain.UploadPlanUpload {
		t.Fatalf("empty namespace exact plan = %+v, %v", exact, err)
	}

	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 1)
	valid, err := files.PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "corrupt", Path: domain.MustParseUserPath("/incoming.bin"), Size: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	groupID, err := duplicateFileGroupID(storageformat.DirectoryEntry{Kind: domain.EntryFile, Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C})
	if err != nil {
		t.Fatal(err)
	}
	projectionID := uploadPlanningProjectionID008(live.UserID())
	session := duplicateProjectionSession008(engine, live.UserID(), projectionID)
	corruptRoot, err := session.buildTree(ctx, []storageformat.DomainEntry{{
		Key: "source/" + groupID + "/corrupt", Value: []byte("{"), LogicalVersion: storageformat.Digest([]byte("corrupt upload planning occurrence")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var validToken uploadPlanProjectionToken008
	if err := files.decodeDuplicateCursor(valid.Token, &validToken); err != nil {
		t.Fatal(err)
	}
	validToken.Root = corruptRoot
	corruptToken, err := files.encodeDuplicateCursor(validToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: corruptToken, Items: []domain.UploadFingerprintPlanItem{{ID: "corrupt", Path: domain.MustParseUserPath("/incoming.bin"), Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt upload planning occurrence error = %v", err)
	}

	headKey := storageformat.ScopedProjectionHeadKey(live.UserID().String(), storageformat.ProjectionDuplicates, projectionID)
	headObject, err := backend.Get(ctx, headKey)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var head storageformat.ProjectionHead
	if err := storageformat.DecodeEnvelope(headObject.Body, headKey, duplicateProjectionHeadSchema, &envelope, &head); err != nil {
		t.Fatal(err)
	}
	head.SourceRevision += 100
	headBody, err := storageformat.EncodeEnvelope(duplicateProjectionHeadSchema, headKey, envelope.Revision+1, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, headKey, headBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: headObject.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := files.PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "future", Path: domain.MustParseUserPath("/future.bin"), Size: 1}}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("future upload projection source error = %v", err)
	}
}

func TestUploadPlanningTreatsCollectedPinnedPagesAsAReplanConflict(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	live := namespaceTestScope(t, domain.AreaLive)
	seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 1)
	files := engine.Files()
	plan, err := files.PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "collected", Path: domain.MustParseUserPath("/incoming.bin"), Size: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	var token uploadPlanProjectionToken008
	if err := files.decodeDuplicateCursor(plan.Token, &token); err != nil {
		t.Fatal(err)
	}
	session := duplicateProjectionSession008(engine, live.UserID(), token.ProjectionID)
	pageKey := session.pageKey(token.Root.Digest)
	page, err := backend.Get(ctx, pageKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(ctx, pageKey, objectstore.DeleteCondition{Version: page.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := files.PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: plan.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "collected", Path: domain.MustParseUserPath("/incoming.bin"), Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}}}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("collected upload planning page error = %v", err)
	}
}

func TestUploadPlanningDetectsExactTargetAndRejectsCrossOwnerToken(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 1)
	sizes, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "target", Path: seeded[0].Path, Size: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: sizes.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "target", Path: seeded[0].Path, Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}}})
	if err != nil || len(exact.Items) != 1 || exact.Items[0].Action != domain.UploadPlanSkip || exact.Items[0].TargetVersion != seeded[0].Version {
		t.Fatalf("exact target = %+v, %v", exact, err)
	}
	otherUser, err := domain.ParseUserID("b3RoZXItcGxhbi11c2VyLTAwMQ")
	if err != nil {
		t.Fatal(err)
	}
	other, err := domain.NewScope(otherUser, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().PlanUploadFingerprints(ctx, other.UserID(), domain.UploadFingerprintPlanRequest{Token: sizes.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "target", Path: seeded[0].Path, Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cross-owner token error = %v", err)
	}
	if _, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: sizes.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "target", Path: seeded[0].Path, Size: 1, MD5: "not-provider-metadata", CRC32C: testProviderCRC32C}}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid local lookup hint error = %v", err)
	}
}

func TestUploadPlanningRejectsStaleSnapshotsAndIncrementallyRemovesTrashedFiles(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFiles(t, newNamespaceStore(engine), live, 1)

	stale, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "stale", Path: domain.MustParseUserPath("/new.bin"), Size: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: seeded[0].Path, ExpectedVersion: seeded[0].Version, TrashID: "upload-plan-trash", IdempotencyKey: "upload-plan-trash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: stale.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "stale", Path: domain.MustParseUserPath("/new.bin"), Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}}}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale upload planning snapshot error = %v", err)
	}

	updated, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "after-trash", Path: domain.MustParseUserPath("/new.bin"), Size: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Items) != 1 || updated.Items[0].FingerprintRequired {
		t.Fatalf("incremental projection retained trashed source: %+v", updated)
	}
}

func TestUploadPlanningIncrementallyRemovesAndRestoresNestedSubtrees(t *testing.T) {
	ctx := context.Background()
	engine := openNamespaceTestEngine(t, objectmemory.New())
	live := namespaceTestScope(t, domain.AreaLive)
	directory, err := engine.Files().CreateDirectory(ctx, live, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/folder"), Conflict: domain.ConflictFail})
	if err != nil {
		t.Fatal(err)
	}
	publishNamespaceTestFile(t, newNamespaceStore(engine), live, "/folder/file.bin", 7, "nested-upload-plan")
	directory, err = engine.Files().Stat(ctx, live, directory.Path)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "nested", Path: domain.MustParseUserPath("/incoming.bin"), Size: 7}}}
	before, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), request)
	if err != nil || len(before.Items) != 1 || !before.Items[0].FingerprintRequired {
		t.Fatalf("nested projection before Trash = %+v, %v", before, err)
	}
	if _, err := engine.Files().MoveToTrash(ctx, live.UserID(), domain.TrashRequest{Path: directory.Path, ExpectedVersion: directory.Version, TrashID: "nested-upload-plan-trash", IdempotencyKey: "nested-upload-plan-trash"}); err != nil {
		t.Fatal(err)
	}
	afterTrash, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), request)
	if err != nil || len(afterTrash.Items) != 1 || afterTrash.Items[0].FingerprintRequired {
		t.Fatalf("nested projection after Trash = %+v, %v", afterTrash, err)
	}
	if _, err := engine.Files().RestoreFromTrash(ctx, live.UserID(), "nested-upload-plan-trash", domain.ConflictFail, "nested-upload-plan-restore"); err != nil {
		t.Fatal(err)
	}
	afterRestore, err := engine.Files().PlanUploadSizes(ctx, live.UserID(), request)
	if err != nil || len(afterRestore.Items) != 1 || !afterRestore.Items[0].FingerprintRequired {
		t.Fatalf("nested projection after restore = %+v, %v", afterRestore, err)
	}
}

func TestUploadPlanningConcurrentColdBuildersConvergeAcrossEightReplicas(t *testing.T) {
	ctx := context.Background()
	base := objectmemory.New()
	seedEngine := openNamespaceTestEngine(t, base)
	live := namespaceTestScope(t, domain.AreaLive)
	seeded := seedNamespaceBatchFiles(t, newNamespaceStore(seedEngine), live, 256)
	seedVersions := make(map[domain.UserPath]domain.Version, len(seeded))
	for _, entry := range seeded {
		seedVersions[entry.Path] = entry.Version
	}
	barrier := &uploadPlanningHeadBarrierBackend{
		Backend: base,
		key:     storageformat.ScopedProjectionHeadKey(live.UserID().String(), storageformat.ProjectionDuplicates, uploadPlanningProjectionID008(live.UserID())),
		target:  8, release: make(chan struct{}),
	}
	replicas := make([]*Engine, barrier.target)
	for index := range replicas {
		replicas[index] = openNamespaceTestEngine(t, barrier)
	}
	type result struct {
		plan domain.UploadSizePlan
		err  error
	}
	results := make(chan result, len(replicas))
	for _, replica := range replicas {
		go func(replica *Engine) {
			plan, err := replica.Files().PlanUploadSizes(ctx, live.UserID(), domain.UploadSizePlanRequest{Items: []domain.UploadSizePlanItem{{ID: "concurrent", Path: domain.MustParseUserPath("/incoming.bin"), Size: 1}}})
			results <- result{plan: plan, err: err}
		}(replica)
	}
	for index, replica := range replicas {
		outcome := <-results
		if outcome.err != nil || len(outcome.plan.Items) != 1 || !outcome.plan.Items[0].FingerprintRequired {
			t.Fatalf("replica size plan = %+v, %v", outcome.plan, outcome.err)
		}
		exact, err := replica.Files().PlanUploadFingerprints(ctx, live.UserID(), domain.UploadFingerprintPlanRequest{Token: outcome.plan.Token, Items: []domain.UploadFingerprintPlanItem{{ID: "concurrent", Path: domain.MustParseUserPath("/incoming.bin"), Size: 1, MD5: testProviderMD5, CRC32C: testProviderCRC32C}}})
		if err != nil || len(exact.Items) != 1 || exact.Items[0].Action != domain.UploadPlanReuse || exact.Items[0].SourcePath == nil || seedVersions[*exact.Items[0].SourcePath] != exact.Items[0].SourceVersion {
			t.Fatalf("replica %d exact plan = %+v, %v", index, exact, err)
		}
	}
	barrier.mu.Lock()
	arrivals := barrier.arrivals
	barrier.mu.Unlock()
	if arrivals < barrier.target {
		t.Fatalf("projection head contenders = %d, want at least %d", arrivals, barrier.target)
	}
}
