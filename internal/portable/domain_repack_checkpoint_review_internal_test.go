package portable

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// This deliberately extends TestUploadBatchLargeMetadataAdmission after its
// original admission/replay/provider assertions, reusing its real fixture.
// Synthetic opaque control values cannot establish a schema-011
// application-semantic checkpoint or raw-copy guarantee.
func reviewLargeMetadataCheckpointRawCopy(t *testing.T, f uploadBatchScaleFixture, requests []domain.CreateUploadRequest, capabilities []domain.UploadCapability) {
	t.Helper()
	ctx := context.Background()
	if len(requests) != 4_001 || len(capabilities) != len(requests) {
		t.Fatal("checkpoint proof did not receive the complete existing large-admission fixture")
	}
	reference := uploadDomainReference(f.scope.UserID())
	ids := make([]domain.UploadID, len(capabilities))
	for index, capability := range capabilities {
		ids[index] = capability.UploadID
	}
	// BatchID is intentionally absent: this public legacy path materializes
	// genuine terminal records, rather than relying on the compact batch overlay.
	// No capability expiry or receipt-retention clock is advanced for this proof.
	cancellation := domain.AbortUploadBatchRequest{UploadIDs: ids, IdempotencyKey: "closure-checkpoint-terminal-records"}
	f.fileLedger.Reset()
	sourceLedgers := [4]*providerbudget.Ledger{f.stateLedger, f.fileLedger, nil, nil}
	finishPhase := startIterationPhase(t, iterationTerminalization, sourceLedgers)
	if err := f.engine.Files().AbortUploadBatch(ctx, f.scope, cancellation); err != nil {
		t.Fatalf("materialize terminal application records: %v", err)
	}
	if events := f.fileLedger.Events(); len(events) != len(requests) {
		t.Fatalf("terminalization native calls=%d, want exactly %d aborts", len(events), len(requests))
	} else {
		for _, event := range events {
			if event.Kind != providerbudget.RequestUploadAbort || event.Failed {
				t.Fatal("terminalization did not abort each admitted native session exactly once")
			}
		}
	}
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationSourceIntentOracle, sourceLedgers)
	terminalProofs := closureCheckpointReviewTerminalIntents(t, f.engine, f.scope, requests, capabilities, nil)
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationCheckpoint, sourceLedgers)
	checkpoint, err := f.engine.CreateCheckpoint(ctx, "closure-real-multipack-copy")
	if err != nil {
		t.Fatalf("canonical application closure rejected terminal multi-pack authority: %v", err)
	}
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationFrozenAuthorityOracle, sourceLedgers)
	frozen, err := f.engine.stateDomainStore().loadHead(ctx, reference)
	if err != nil || !frozen.head.Frozen {
		t.Fatalf("checkpoint did not freeze owner authority: %v", err)
	}
	packIDs, laterPage := closureCheckpointReviewAuthorityPacks(t, f.engine, reference, frozen.head)
	if len(packIDs) < 2 || laterPage.root.Digest == "" {
		t.Fatalf("frozen authority needs multiple reachable packs including a non-root pack: packs=%d child=%t", len(packIDs), laterPage.root.Digest != "")
	}
	transactionID, fingerprint, err := normalizePortableUploadAbortBatch(f.scope.UserID(), cancellation)
	if err != nil {
		t.Fatal(err)
	}
	sourceView, err := newNamespaceStore(f.engine).loadView(ctx, f.scope.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	sourceOutcome := closureCheckpointReviewCancellationReceipt(t, f.engine, sourceView, transactionID, fingerprint, len(requests))

	stateBase, fileBase := objectmemory.New(), objectmemory.New()
	stateLedger, fileLedger := providerbudget.NewLedger(), providerbudget.NewLedger()
	stateCopy := budgettest.Wrap(providerbudget.RoleState, stateBase, stateLedger)
	fileCopy := budgettest.Wrap(providerbudget.RoleFile, fileBase, fileLedger)
	finishPhase(true)
	allLedgers := [4]*providerbudget.Ledger{f.stateLedger, f.fileLedger, stateLedger, fileLedger}
	finishPhase = startIterationPhase(t, iterationRawCopy, allLedgers)
	if _, err := stateCopy.Put(ctx, objectstore.MustKey("synthetic/native-version-shift"), []byte("shift"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	copyKey := func(key objectstore.Key, fileData bool) error {
		source, target := f.engine.backend, objectstore.Backend(stateCopy)
		if fileData {
			source, target = f.engine.fileBackend.(objectstore.Backend), fileCopy
		}
		object, err := source.Get(ctx, key)
		if err != nil {
			return err
		}
		_, err = target.Put(ctx, key, object.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		return err
	}
	inventoryPacks := make(map[string]bool, len(packIDs))
	if err := f.engine.visitCheckpointInventory(ctx, checkpoint, func(entry storageformat.CheckpointInventoryEntry) error {
		if _, reachable := packIDs[entry.Object.Key]; reachable {
			if entry.FileData || inventoryPacks[entry.Object.Key] {
				t.Fatal("reachable authority pack is repeated or assigned to file-data inventory")
			}
			inventoryPacks[entry.Object.Key] = true
		}
		return copyKey(objectstore.MustKey(entry.Object.Key), entry.FileData)
	}); err != nil {
		t.Fatal(err)
	}
	if len(inventoryPacks) != len(packIDs) {
		t.Fatalf("checkpoint omitted reachable authority packs: inventoried=%d reachable=%d", len(inventoryPacks), len(packIDs))
	}
	if err := copyKey(storageformat.CheckpointKey(checkpoint.CheckpointID), false); err != nil {
		t.Fatal(err)
	}
	for index := uint64(0); index < checkpoint.InventoryPageCount; index++ {
		if err := copyKey(storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, index), false); err != nil {
			t.Fatal(err)
		}
	}
	headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	sourceHead, err := f.engine.backend.Get(ctx, headKey)
	if err != nil {
		t.Fatal(err)
	}
	targetHead, err := stateCopy.Get(ctx, headKey)
	if err != nil || !bytes.Equal(sourceHead.Body, targetHead.Body) {
		t.Fatalf("raw copy changed canonical frozen head bytes: %v", err)
	}
	if sourceHead.Version == targetHead.Version {
		// A native version is an opaque backend-local CAS token. In the unlikely
		// event of equal counters after copying, an identical-body conditional
		// replacement makes the intended cross-backend inequality explicit.
		version, err := stateCopy.Put(ctx, headKey, targetHead.Body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: targetHead.Version})
		if err != nil {
			t.Fatal(err)
		}
		targetHead.Version = version
	}
	if sourceHead.Version == targetHead.Version {
		t.Fatal("raw copy did not shift native authority-head version")
	}
	finishPhase(true)
	writer := WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}}
	stateLedger.Reset()
	fileLedger.Reset()
	finishPhase = startIterationPhase(t, iterationSplitVerification, allLedgers)
	if err := VerifyCheckpointReadOnlyWithFileBackend(ctx, stateCopy, fileCopy, writer, checkpoint.CheckpointID); err != nil {
		t.Fatalf("read-only verification of split-backend raw copy: %v", err)
	}
	closureCheckpointReviewReadOnlyEvents(t, stateLedger.Events(), fileLedger.Events())
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationOpenCopy, allLedgers)
	copied, err := Open(ctx, Options{
		Backend: stateCopy, FileBackend: fileCopy, Clock: f.engine.clock,
		IDs: domain.NewIDGenerator(&deterministicScaleReader{state: 0x67584931}), Writer: writer,
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32),
	})
	if err != nil {
		t.Fatalf("open raw-copied canonical application authority: %v", err)
	}
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationWarmChildRemoval, allLedgers)

	// This is a child pack excluded from ALL three immediate root pack IDs.
	// Warm the exact authenticated child descriptor, remove durable authority,
	// and first prove an ordinary fresh request can still use its idle cache.
	missingKey := storageformat.DomainPagePackKey(reference.Kind, reference.ID, laterPage.root.PackID)
	store := copied.stateDomainStore()
	if _, err := newConsistencyDomainTreeSession(store, reference).readPage(ctx, laterPage); err != nil {
		t.Fatalf("warm non-root authority page: %v", err)
	}
	original, err := stateCopy.Get(ctx, missingKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateCopy.Delete(ctx, missingKey, objectstore.DeleteCondition{Version: original.Version}); err != nil {
		t.Fatal(err)
	}
	finishPhase(true)
	stateLedger.Reset()
	fileLedger.Reset()
	finishPhase = startIterationPhase(t, iterationWarmChildControl, allLedgers)
	if _, err := newConsistencyDomainTreeSession(store, reference).readPage(ctx, laterPage); err != nil {
		t.Fatalf("ordinary fresh request did not prove the deleted child pack was warm: %v", err)
	}
	if len(stateLedger.Events()) != 0 || len(fileLedger.Events()) != 0 {
		t.Fatal("warm child control issued provider work instead of using verified idle cache")
	}
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationMissingChildVerification, allLedgers)
	if err := copied.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err == nil {
		t.Fatal("warm idle cache hid missing non-root canonical checkpoint authority")
	}
	providerRead := false
	for _, event := range stateLedger.Events() {
		if event.Kind == providerbudget.RequestObjectGet && event.Target == missingKey.String() && event.Failed {
			providerRead = true
		}
	}
	if !providerRead {
		t.Fatal("checkpoint denial did not directly read the missing non-root pack")
	}
	closureCheckpointReviewReadOnlyEvents(t, stateLedger.Events(), fileLedger.Events())
	finishPhase(true)
	stateLedger.Reset()
	fileLedger.Reset()
	finishPhase = startIterationPhase(t, iterationIndependentMissingChildVerification, allLedgers)
	if err := VerifyCheckpointReadOnlyWithFileBackend(ctx, stateCopy, fileCopy, writer, checkpoint.CheckpointID); err == nil {
		t.Fatal("independent read-only verifier accepted the missing non-root pack")
	}
	closureCheckpointReviewReadOnlyEvents(t, stateLedger.Events(), fileLedger.Events())
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationRestoreClosedGate, allLedgers)
	restoredVersion, err := stateCopy.Put(ctx, missingKey, original.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err != nil {
		t.Fatal(err)
	}
	if restoredVersion == original.Version {
		t.Fatal("restoration did not change the native non-root pack version")
	}
	gate, err := copied.GateStatus(ctx)
	if err != nil || gate.Mode != storageformat.GateClosed || gate.CheckpointID != checkpoint.CheckpointID || gate.Epoch != checkpoint.GateEpoch {
		t.Fatalf("restored authority is not bound to its closed checkpoint gate: %v", err)
	}
	finishPhase(true)
	stateLedger.Reset()
	fileLedger.Reset()
	finishPhase = startIterationPhase(t, iterationOpenWrites, allLedgers)
	if err := copied.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatalf("reopen verified raw copy: %v", err)
	}
	restoredRead, openingPut := false, false
	for _, event := range stateLedger.Events() {
		if event.Kind == providerbudget.RequestObjectGet && event.Target == missingKey.String() && !event.Failed {
			restoredRead = true
		}
		if event.Kind == providerbudget.RequestObjectPut && event.Target == storageformat.WriteGateKey().String() {
			if !restoredRead {
				t.Fatal("reopen attempted to open the gate before reading restored non-root authority")
			}
			openingPut = openingPut || !event.Failed
		}
	}
	gate, err = copied.GateStatus(ctx)
	if !openingPut || err != nil || gate.Mode != storageformat.GateOpen || gate.CheckpointID != "" || gate.Epoch != checkpoint.GateEpoch+1 {
		t.Fatalf("verified restoration did not reopen the canonical gate: successful_put=%t err=%v", openingPut, err)
	}
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationCopiedIntentOracle, allLedgers)
	closureCheckpointReviewTerminalIntents(t, copied, f.scope, requests, capabilities, terminalProofs)
	finishPhase(true)
	finishPhase = startIterationPhase(t, iterationCopiedReceiptOracle, allLedgers)
	copiedView, err := newNamespaceStore(copied).loadView(ctx, f.scope.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	copiedOutcome := closureCheckpointReviewCancellationReceipt(t, copied, copiedView, transactionID, fingerprint, len(requests))
	if !reflect.DeepEqual(sourceOutcome, copiedOutcome) {
		t.Fatal("raw copy changed retained mutation identity, fingerprint, revision, deadline or result")
	}
	beforeReplay, err := stateCopy.Head(ctx, headKey)
	if err != nil {
		t.Fatal(err)
	}
	finishPhase(true)
	stateLedger.Reset()
	fileLedger.Reset()
	finishPhase = startIterationPhase(t, iterationCopiedReplayOracle, allLedgers)
	if err := copied.Files().AbortUploadBatch(ctx, f.scope, cancellation); err != nil {
		t.Fatalf("retained public cancellation replay after raw copy: %v", err)
	}
	changed := cancellation
	changed.UploadIDs = append([]domain.UploadID(nil), cancellation.UploadIDs...)
	changed.UploadIDs[0], changed.UploadIDs[1] = changed.UploadIDs[1], changed.UploadIDs[0]
	if err := copied.Files().AbortUploadBatch(ctx, f.scope, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("retained cancellation receipt lost ordered intent binding: %v", err)
	}
	if len(fileLedger.Events()) != 0 {
		t.Fatal("terminal receipt replay or changed-intent denial repeated native provider work")
	}
	closureCheckpointReviewReadOnlyEvents(t, stateLedger.Events(), nil)
	afterReplay, err := stateCopy.Head(ctx, headKey)
	if err != nil || afterReplay.Version != beforeReplay.Version {
		t.Fatalf("terminal replay or changed-intent denial republished owner authority: %v", err)
	}
	finishPhase(true)
	t.Logf("canonical raw-copy closure: intents=%d frozen_reachable_packs=%d inventoried_authority_packs=%d terminal_receipts=1 missing_nonroot_denials=2", len(requests), len(packIDs), len(inventoryPacks))
}

// Reachability follows authenticated full descriptors; it never infers a pack
// identity from its hashed provider key or counts merely-written garbage.
func closureCheckpointReviewAuthorityPacks(t *testing.T, engine *Engine, reference consistencyDomainRef, head storageformat.DomainHead) (map[string]string, domainPageRef) {
	t.Helper()
	ctx := context.Background()
	roots := []storageformat.DomainTreeRoot{head.Base, head.Outcomes, head.OutcomeExpiry}
	rootPacks := make(map[string]bool)
	pending := make([]domainPageRef, 0, len(roots))
	for _, root := range roots {
		if root.Digest != "" {
			pending = append(pending, domainPageRef{root: root})
			rootPacks[root.PackID] = true
		}
	}
	packIDs := make(map[string]string)
	visited := make(map[domainPageRef]bool)
	session := newConsistencyDomainTreeSession(engine.stateDomainStore(), reference)
	var nonRoot domainPageRef
	for len(pending) > 0 {
		ref := pending[0]
		pending = pending[1:]
		if visited[ref] {
			continue
		}
		visited[ref] = true
		page, err := session.readPage(ctx, ref)
		if err != nil {
			t.Fatalf("reachable authority descriptor is not durable and authenticated: %v", err)
		}
		if ref.root.PackID != "" {
			packIDs[storageformat.DomainPagePackKey(reference.Kind, reference.ID, ref.root.PackID).String()] = ref.root.PackID
			if !rootPacks[ref.root.PackID] && nonRoot.root.Digest == "" {
				nonRoot = ref
			}
		}
		for _, child := range page.Children {
			pending = append(pending, domainPageRef{
				root:     storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount},
				firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter,
			})
		}
	}
	keys := make([]string, 0, len(packIDs))
	for key := range packIDs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		object, err := engine.backend.Get(ctx, objectstore.MustKey(key))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storageformat.DecodeDomainPagePack(ctx, object.Body, reference.ID, reference.Kind, packIDs[key]); err != nil {
			t.Fatalf("reachable durable pack failed complete strict validation: %v", err)
		}
	}
	return packIDs, nonRoot
}

type closureCheckpointReviewIntentProof struct {
	valueDigest    string
	logicalVersion string
}

func closureCheckpointReviewTerminalIntents(t *testing.T, engine *Engine, scope domain.Scope, requests []domain.CreateUploadRequest, capabilities []domain.UploadCapability, expected []closureCheckpointReviewIntentProof) []closureCheckpointReviewIntentProof {
	t.Helper()
	ctx := context.Background()
	view, err := newNamespaceStore(engine).loadView(ctx, scope.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != len(requests) || expected != nil && len(expected) != len(requests) {
		t.Fatal("terminal intent oracle received an incomplete cohort")
	}
	proofs := make([]closureCheckpointReviewIntentProof, len(requests))
	seen := make(map[string]bool, len(requests))
	for index, request := range requests {
		normalized, fingerprint, err := normalizePortableUploadRequest(scope, request)
		if err != nil {
			t.Fatal(err)
		}
		record, value, err := engine.Files().portableUploadAtView(ctx, view, scope.UserID(), string(capabilities[index].UploadID))
		if err != nil {
			t.Fatalf("terminal record %d: %v", index, err)
		}
		raw, err := decodePortableUploadRecord(value.Data, scope.UserID(), record.UploadID)
		if err != nil || raw.State != storageformat.UploadAborted || raw.CleanupPending || raw.Completion != nil || !reflect.DeepEqual(raw, record) {
			t.Fatalf("terminal record %d relies on an overlay or has invalid raw state: %v", index, err)
		}
		byIntent, found, err := engine.Files().portableUploadByIdempotencyAtView(ctx, view, scope.UserID(), request.IdempotencyKey, fingerprint)
		if err != nil || !found || !reflect.DeepEqual(raw, byIntent) {
			t.Fatalf("terminal intent binding %d: found=%t err=%v", index, found, err)
		}
		capability := capabilities[index]
		if seen[raw.UploadID] || raw.UploadID != string(capability.UploadID) || raw.Area != areaName(scope.Area()) || raw.Batch == nil || raw.Batch.BatchID != capability.BatchID || raw.Batch.Index != uint64(index) || raw.Batch.Count != uint64(len(requests)) || capability.BatchIndex != uint64(index) || capability.BatchCount != uint64(len(requests)) || raw.RequestedPath != normalized.Path.String() || raw.ResolvedPath != normalized.Path.String() || raw.Size != normalized.Size || raw.MediaType != normalized.MediaType || raw.Conflict != normalized.Conflict || raw.ExpectedVersion != normalized.ExpectedVersion || raw.Resumable != normalized.Resumable || value.LogicalVersion == "" {
			t.Fatalf("terminal intent %d changed identity, ordered membership, logical version or request fields", index)
		}
		seen[raw.UploadID] = true
		proofs[index] = closureCheckpointReviewIntentProof{valueDigest: storageformat.Digest(value.Data), logicalVersion: value.LogicalVersion}
		if expected != nil && proofs[index] != expected[index] {
			t.Fatalf("raw copy changed terminal record %d bytes or portable logical version", index)
		}
	}
	return proofs
}

func closureCheckpointReviewCancellationReceipt(t *testing.T, engine *Engine, view *namespaceView, transactionID, fingerprint string, count int) consistencyDomainOutcome {
	t.Helper()
	ctx := context.Background()
	result, err := newNamespaceStore(engine).operationReplay(ctx, view, transactionID, fingerprint)
	if err != nil || result == nil || result.UploadBatch == nil || result.UploadBatch.TransactionID != transactionID || result.UploadBatch.ItemCount != uint64(count) || result.UploadBatch.State != "aborted" {
		t.Fatalf("retained public cancellation receipt is incomplete: %v", err)
	}
	outcome, found, err := engine.stateDomainStore().lookupOutcomeAtHeadWithSession(ctx, view.reference, view.head, transactionID, view.session)
	if err != nil || !found || outcome.MutationID != transactionID || outcome.Revision == 0 || outcome.RetainUntil.IsZero() || len(outcome.Result) == 0 {
		t.Fatalf("retained generic mutation authority is incomplete: found=%t err=%v", found, err)
	}
	return outcome
}

func closureCheckpointReviewReadOnlyEvents(t *testing.T, state, files []providerbudget.Event) {
	t.Helper()
	for _, events := range [][]providerbudget.Event{state, files} {
		for _, event := range events {
			switch event.Kind {
			case providerbudget.RequestObjectGet, providerbudget.RequestObjectHead, providerbudget.RequestObjectList, providerbudget.RequestObjectVerify:
			default:
				t.Fatalf("read-only checkpoint or replay performed forbidden provider work: %s", event.Kind)
			}
		}
	}
}
