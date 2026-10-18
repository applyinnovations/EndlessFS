package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestUploadBatchLargeMetadataAdmission(t *testing.T) {
	parallelCorrectiveHeavyFixture(t)
	fixture := newUploadBatchScaleFixture(t, 0x67584930)
	ctx := context.Background()
	parent := ""
	for range 12 {
		parent += "/" + strings.Repeat("a", 250)
		if _, err := fixture.engine.Files().CreateDirectory(ctx, fixture.scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath(parent)}); err != nil {
			t.Fatal(err)
		}
	}
	requests := scaleUploadRequestsCount("large-metadata", 4_001)
	for index := range requests {
		requests[index].Path = domain.MustParseUserPath(parent + requests[index].Path.String())
	}
	// The browser sends the common directory once in defaults. A small control
	// request can therefore expand beyond the pack bound without file bytes.
	items := make([]map[string]any, len(requests))
	for index, request := range requests {
		items[index] = map[string]any{"name": request.Path.Name(), "size": request.Size}
	}
	wire, err := json.Marshal(map[string]any{"defaults": map[string]any{"path": parent, "mediaType": "application/octet-stream", "conflict": "rename", "resumable": true}, "uploads": items})
	if err != nil || len(wire) >= 1<<20 {
		t.Fatalf("compact control request = %d bytes, %v", len(wire), err)
	}
	fixture.stateLedger.Reset()
	fixture.fileLedger.Reset()
	capabilities, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil || len(capabilities) != len(requests) {
		t.Fatalf("CreateUploadBatch() = %d capabilities, %v; want %d", len(capabilities), err, len(requests))
	}
	headKey := storageformat.DomainHeadKey(storageformat.DomainNamespace, fixture.scope.UserID().String()).String()
	admissionState, admissionFiles := fixture.stateLedger.Events(), fixture.fileLedger.Events()
	reference := uploadDomainReference(fixture.scope.UserID())
	store := fixture.engine.stateDomainStore()
	head, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	// Provider key components hash pack IDs. Recover the expected IDs from
	// authenticated head/page descriptors rather than trusting stored headers.
	packIDs := make(map[string]string)
	visited := make(map[storageformat.DomainTreeRoot]bool)
	session := newConsistencyDomainTreeSession(store, reference)
	pending := []storageformat.DomainTreeRoot{head.head.Base, head.head.Outcomes, head.head.OutcomeExpiry}
	for len(pending) > 0 {
		root := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if root.Digest == "" || visited[root] {
			continue
		}
		visited[root] = true
		if root.PackID != "" {
			packIDs[storageformat.DomainPagePackKey(reference.Kind, reference.ID, root.PackID).String()] = root.PackID
		}
		page, err := session.readPage(ctx, domainPageRef{root: root})
		if err != nil {
			t.Fatalf("published page descriptor is not durable: %v", err)
		}
		for _, child := range page.Children {
			pending = append(pending, storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount})
		}
	}
	headPuts, pagePuts, packPuts, packedPages := 0, 0, 0, 0
	var packWireBytes, packExpandedBytes int
	for _, event := range admissionState {
		if event.Kind == providerbudget.RequestObjectPut && !event.Failed {
			if event.Target == headKey {
				headPuts++
			}
			if strings.Contains(event.Target, "/pages/") {
				pagePuts++
			}
			if strings.Contains(event.Target, "/packs/") {
				if headPuts != 0 {
					t.Fatal("metadata pack was written after the visibility head")
				}
				key, err := objectstore.ParseKey(event.Target)
				if err != nil {
					t.Fatal(err)
				}
				object, err := fixture.engine.backend.Get(ctx, key)
				if err != nil {
					t.Fatalf("published pack is not durable: %v", err)
				}
				packID, found := packIDs[event.Target]
				if !found {
					t.Fatal("admission wrote a pack unreachable from the published roots")
				}
				pack, err := storageformat.DecodeDomainPagePack(ctx, object.Body, reference.ID, reference.Kind, packID)
				if err != nil {
					t.Fatalf("durable pack failed canonical/key-binding validation: %v", err)
				}
				var expanded bytes.Buffer
				encoder := json.NewEncoder(&expanded)
				encoder.SetEscapeHTML(false)
				err = encoder.Encode(pack)
				expandedSize := len(bytes.TrimSuffix(expanded.Bytes(), []byte{'\n'}))
				if err != nil || len(object.Body) > storageformat.MaxDomainPagePackBytes || expandedSize > storageformat.MaxExpandedDomainPagePackBytes {
					t.Fatalf("unbounded durable pack: wire=%d expanded=%d err=%v", len(object.Body), expandedSize, err)
				}
				packPuts++
				packedPages += len(pack.Pages)
				packWireBytes += len(object.Body)
				packExpandedBytes += expandedSize
			}
		}
	}
	if headPuts != 1 || pagePuts != 0 || packPuts < 2 || packedPages < 2 || len(admissionFiles) != len(requests) {
		t.Fatalf("admission: head puts=%d raw page puts=%d pack puts=%d packed pages=%d provider starts=%d", headPuts, pagePuts, packPuts, packedPages, len(admissionFiles))
	}
	for _, event := range admissionFiles {
		if event.Kind != providerbudget.RequestUploadBegin || event.Failed {
			t.Fatalf("admission did not create exactly one successful provider session per item: %+v", event)
		}
	}
	t.Logf("compact request=%d bytes; admission state calls=%d file calls=%d packs=%d packed pages=%d pack wire=%d expanded=%d head publications=%d", len(wire), len(admissionState), len(admissionFiles), packPuts, packedPages, packWireBytes, packExpandedBytes, headPuts)
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for role, events := range map[string][]providerbudget.Event{"state": admissionState, "file": admissionFiles} {
		totals, err := model.Estimate(events)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("large metadata admission %s economics: %+v", role, totals)
	}
	// Reopen without the admission engine's page/pack caches. Authenticate every
	// record through its persisted intent key at the independently loaded head.
	reader, err := Open(ctx, Options{
		Backend: fixture.engine.backend, FileBackend: fixture.engine.fileBackend, Clock: fixture.engine.clock,
		IDs:      domain.NewIDGenerator(&deterministicScaleReader{state: 0x98764321}),
		Writer:   WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := newNamespaceStore(reader).loadView(ctx, fixture.scope.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	seenIDs := make(map[string]bool, len(requests))
	for index, request := range requests {
		normalized, fingerprint, err := normalizePortableUploadRequest(fixture.scope, request)
		if err != nil {
			t.Fatal(err)
		}
		record, found, err := reader.Files().portableUploadByIdempotencyAtView(ctx, view, fixture.scope.UserID(), request.IdempotencyKey, fingerprint)
		if err != nil || !found || record.Batch == nil {
			t.Fatalf("independent intent lookup %d: found=%t batch=%+v err=%v", index, found, record.Batch, err)
		}
		capability := capabilities[index]
		if seenIDs[record.UploadID] || record.UploadID != string(capability.UploadID) || record.Batch.BatchID != capability.BatchID || record.Batch.Index != uint64(index) || record.Batch.Count != uint64(len(requests)) || capability.BatchIndex != uint64(index) || capability.BatchCount != uint64(len(requests)) || record.State != storageformat.UploadInitializing || record.RequestedPath != normalized.Path.String() || record.ResolvedPath != normalized.Path.String() || record.Size != normalized.Size || record.MediaType != normalized.MediaType || record.Conflict != normalized.Conflict || record.ExpectedVersion != normalized.ExpectedVersion || record.Resumable != normalized.Resumable {
			t.Fatalf("independent persisted admission %d changed identity, membership, state or intent", index)
		}
		seenIDs[record.UploadID] = true
	}
	fixture.fileLedger.Reset()
	replayed, err := reader.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil || len(replayed) != len(capabilities) {
		t.Fatalf("replay = %d capabilities, %v", len(replayed), err)
	}
	for index := range capabilities {
		if replayed[index].UploadID != capabilities[index].UploadID || replayed[index].BatchID != capabilities[index].BatchID {
			t.Fatalf("replay changed item %d identity", index)
		}
	}
	for _, event := range fixture.fileLedger.Events() {
		if event.Kind == providerbudget.RequestUploadBegin {
			t.Fatal("replay created a new provider session")
		}
	}
	requests[0].Size++
	if _, err := reader.Files().CreateUploadBatch(ctx, fixture.scope, requests); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed intent under the same keys = %v", err)
	}
	// Restore the admitted input after the negative probe; reuse this exact
	// already-verified fixture for canonical multi-pack checkpoint portability.
	requests[0].Size--
	reviewLargeMetadataCheckpointRawCopy(t, fixture, requests, capabilities)
}
