package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Seed the exact predecessor's nonempty materialized intent receipt. The new
// implementation must replay its authenticated records without interpreting
// that old phase result as a new nil-result mutation.
func reviewSeedOldAdmissionIntent(t *testing.T, f uploadBatchScaleFixture, request domain.CreateUploadRequest) storageformat.PortableUploadRecord {
	t.Helper()
	ctx := context.Background()
	request, fingerprint, err := normalizePortableUploadRequest(f.scope, request)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.engine.ids.OpaqueID()
	if err != nil {
		t.Fatal(err)
	}
	now := f.engine.clock.Now().UTC()
	record := storageformat.PortableUploadRecord{SchemaVersion: 1, UploadID: id, OwnerID: f.scope.UserID().String(), Area: areaName(f.scope.Area()), RequestedPath: request.Path.String(), ResolvedPath: request.Path.String(), BlobID: id, Size: request.Size, MediaType: request.MediaType, Conflict: request.Conflict, ExpectedVersion: request.ExpectedVersion, Resumable: request.Resumable, State: storageformat.UploadInitializing, CreatedAt: now, ExpiresAt: now.Add(f.engine.uploadLifetime(request.Resumable))}
	recordBody, err := storageformat.EncodeCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	receiptBody, err := storageformat.EncodeCanonical(storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: f.scope.UserID().String(), KeyDigest: storageformat.Digest([]byte(request.IdempotencyKey)), Fingerprint: fingerprint, UploadID: id})
	if err != nil {
		t.Fatal(err)
	}
	phaseFingerprint := namespaceRequestFingerprint("upload-admission", request.IdempotencyKey+"\x00"+fingerprint)
	phaseID := "upload-admission:" + phaseFingerprint
	resultBody, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: phaseFingerprint, Upload: &storageformat.NamespaceUploadMutationResult{UploadID: id, State: "initializing"}})
	if err != nil {
		t.Fatal(err)
	}
	view, err := newNamespaceStore(f.engine).loadView(ctx, f.scope.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := view.bindMutation(phaseID, phaseFingerprint); err != nil {
		t.Fatal(err)
	}
	_, err = f.engine.stateDomainStore().mutateMaterializedPrepared(ctx, uploadDomainReference(f.scope.UserID()), consistencyDomainMutation{ID: phaseID, Result: resultBody, Changes: []consistencyDomainChange{{Key: uploadRecordKey(id), Require: domainValueAbsent, Value: recordBody}, {Key: uploadIdempotencyKey(request.IdempotencyKey), Require: domainValueAbsent, Value: receiptBody}}}, view.headSnapshot, view.session)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestUploadPhaseReceiptReviewPredecessorResultsReplayFromRecords(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x1788a911)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("old-phase-result", 1)
	requests[0].Size = 4
	record := reviewSeedOldAdmissionIntent(t, f, requests[0])
	transfer, _ := f.engine.Files().transferBackend()
	var starts atomic.Int64
	f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: transfer, begin: func(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
		starts.Add(1)
		return transfer.BeginUpload(ctx, request)
	}}
	f.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepUploadAdmissionAfterLeases {
			return domain.ErrUnavailable
		}
		return nil
	})
	interrupted, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(interrupted) != 1 || interrupted[0].UploadID != domain.UploadID(record.UploadID) || interrupted[0].Capability != nil || interrupted[0].Outcome != "unknown" || starts.Load() != 1 {
		t.Fatalf("old initializing result replay: %+v starts=%d err=%v", interrupted, starts.Load(), err)
	}
	f.engine.scheduler = nil
	view, err := newNamespaceStore(f.engine).loadView(ctx, f.scope.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	record, value, err := f.engine.Files().portableUploadAtView(ctx, view, f.scope.UserID(), record.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	record.State = storageformat.UploadActive
	body, err := storageformat.EncodeCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := namespaceRequestFingerprint("upload-admission-activate", record.UploadID+"\x00"+value.LogicalVersion)
	id := "upload-admission-activate:" + fingerprint
	oldResult, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Upload: &storageformat.NamespaceUploadMutationResult{UploadID: record.UploadID, State: "active"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := view.bindMutation(id, fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.stateDomainStore().mutateMaterializedPrepared(ctx, uploadDomainReference(f.scope.UserID()), consistencyDomainMutation{ID: id, Result: oldResult, Changes: []consistencyDomainChange{{Key: uploadRecordKey(record.UploadID), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}}}, view.headSnapshot, view.session); err != nil {
		t.Fatal(err)
	}
	replica := newAdmissionReplica(t, f, 0x1788a912)
	page, err := replica.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(page) != 1 || page[0].Capability == nil || page[0].UploadID != domain.UploadID(record.UploadID) || starts.Load() != 1 {
		t.Fatalf("old active result replay: %+v starts=%d err=%v", page, starts.Load(), err)
	}
	payload := []byte("four")
	admissionWriteBytes(t, f, *page[0].Capability, payload, 0)
	action := domain.UploadActionItem{UploadID: page[0].UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor(payload).CRC32C}
	completed, err := replica.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{action})
	if err != nil || len(completed) != 1 || completed[0].Status == nil || completed[0].Status.State != domain.UploadStateCompleted {
		t.Fatalf("old active capability could not complete actual bytes: %+v %v", completed, err)
	}
	replay, err := replica.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || replay[0].Outcome != "completed" || replay[0].Capability != nil || starts.Load() != 1 {
		t.Fatalf("completed replay recreated old session: %+v %v", replay, err)
	}
	changed := requests[0]
	changed.Size++
	conflict, err := replica.Files().AdmitUploads(ctx, f.scope, []domain.CreateUploadRequest{changed})
	if err != nil || conflict[0].ErrorKind != domain.ErrorConflict || conflict[0].Capability != nil || starts.Load() != 1 {
		t.Fatalf("phase result removal weakened intent binding: %+v %v", conflict, err)
	}
	action.CRC32C = objectstore.FingerprintFor([]byte("evil")).CRC32C
	forged, err := replica.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{action})
	if err != nil || forged[0].ErrorKind != domain.ErrorConflict || forged[0].Outcome != "rejected" {
		t.Fatalf("phase result removal weakened completion CRC binding: %+v %v", forged, err)
	}
}

func TestUploadPhaseReceiptReviewEmptyResultsRecoverLostHeadResponses(t *testing.T) {
	for _, phase := range []string{"intent", "activation"} {
		t.Run(phase, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x1788a921)
			ctx := context.Background()
			clock := f.engine.clock.(*domain.FixedClock)
			transfer, _ := f.engine.Files().transferBackend()
			var starts atomic.Int64
			f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: transfer, begin: func(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
				starts.Add(1)
				return transfer.BeginUpload(ctx, request)
			}}
			base := f.engine.backend
			backend := &admissionBackendProbe{Backend: base}
			f.engine.backend = backend
			armed, lost := phase == "intent", false
			f.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
				if phase == "activation" && step == StepUploadAdmissionAfterLeases {
					armed = true
				}
				return nil
			})
			reference := uploadDomainReference(f.scope.UserID())
			headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
			backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
				version, err := base.Put(ctx, key, body, condition)
				if err == nil && key == headKey && armed && !lost {
					lost = true
					clock.Advance(time.Millisecond)
					return version, domain.ErrUnavailable
				}
				return version, err
			}
			requests := scaleUploadRequestsCount("lost-phase-head", 1)
			page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
			if err != nil || len(page) != 1 || page[0].Capability == nil || !lost || starts.Load() != 1 {
				t.Fatalf("nil-result lost head response: %+v lost=%t starts=%d err=%v", page, lost, starts.Load(), err)
			}
			store := f.engine.stateDomainStore()
			head, err := store.loadHead(ctx, reference)
			if err != nil {
				t.Fatal(err)
			}
			session := newConsistencyDomainTreeSession(store, reference)
			outcomes, err := session.collect(ctx, head.head.Outcomes, "", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			var phases int
			for _, entry := range outcomes {
				phases++
				var outcome storageformat.DomainOutcome
				if err := decodeCanonicalValue(entry.Value, &outcome); err != nil || len(outcome.Result) != 0 || outcome.Fingerprint == "" || outcome.Revision == 0 || !clock.Now().Before(outcome.RetainUntil) {
					t.Fatalf("empty result removed durable replay commitment: %+v %v", outcome, err)
				}
			}
			if phases != 2 {
				t.Fatalf("retained phase commitments = %d", phases)
			}
			f.engine.scheduler = nil
			replica := newAdmissionReplica(t, f, 0x1788a922)
			replay, err := replica.Files().AdmitUploads(ctx, f.scope, requests)
			if err != nil || replay[0].Capability == nil || replay[0].UploadID != page[0].UploadID || starts.Load() != 1 {
				t.Fatalf("independent nil-result replay changed session: %+v %v", replay, err)
			}
			if _, err := store.get(ctx, reference, uploadRecordKey(string(page[0].UploadID))); errors.Is(err, domain.ErrNotFound) {
				t.Fatal("nil result was mistaken for missing upload state")
			}
		})
	}
}

func TestUploadPhaseReceiptReviewCheckpointPreservesCompactEmptyReceipts(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x1788a931)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("checkpoint-compact-phase", 1)
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(page) != 1 || page[0].Capability == nil {
		t.Fatalf("checkpoint setup: %+v %v", page, err)
	}
	uploadEmptyCapabilities(t, f.server.Client(), []domain.UploadCapability{*page[0].Capability})
	if results, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{{UploadID: page[0].UploadID, Action: "complete", CRC32C: "AAAAAA"}}); err != nil || len(results) != 1 || results[0].Outcome != "applied" {
		t.Fatalf("checkpoint completion: %+v %v", results, err)
	}
	checkpoint, err := f.engine.CreateCheckpoint(ctx, "compact-phase-receipts")
	if err != nil {
		t.Fatalf("checkpoint rejected empty opaque phase receipts: %v", err)
	}
	if err := f.engine.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	replica := newAdmissionReplica(t, f, 0x1788a932)
	replay, err := replica.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(replay) != 1 || replay[0].UploadID != page[0].UploadID || replay[0].Outcome != "completed" || replay[0].Capability != nil {
		t.Fatalf("checkpoint lost record-based phase replay: %+v %v", replay, err)
	}
	seen := make(map[string]bool)
	for _, phase := range []string{"ui", "ua"} {
		id := uploadPhaseMutationID(phase, "same-complete-intent")
		encoded := id
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(encoded) != 43 || len(decoded) != 32 || seen[id] {
			t.Fatalf("phase identity lost full digest or context separation: phase=%s size=%d err=%v", phase, len(decoded), err)
		}
		seen[id] = true
	}
}

func TestUploadPhaseReceiptReviewLegacyRecoveryEconomicsEvidence(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x93948046)
	ctx := context.Background()
	caps, err := f.engine.Files().CreateUploadBatch(ctx, f.scope, scaleUploadRequestsCount("lifecycle-economics", 100))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]domain.UploadID, len(caps))
	for index, cap := range caps {
		ids[index] = cap.UploadID
		if _, err := f.engine.Files().UploadStatus(ctx, f.scope, cap.UploadID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.Files().ResumeUpload(ctx, f.scope, cap.UploadID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.Files().UploadStatus(ctx, f.scope, cap.UploadID); err != nil {
			t.Fatal(err)
		}
	}
	f.stateLedger.Reset()
	if _, err := f.engine.Files().UploadLifecycle(ctx, f.scope, ids, true); err != nil {
		t.Fatal(err)
	}
	for _, event := range f.stateLedger.Events() {
		t.Logf("synthetic legacy recovery metadata kind=%s responseBytes=%d target=%s", event.Kind, event.ResponseBytes, event.Target)
	}
	store := f.engine.stateDomainStore()
	reference := uploadDomainReference(f.scope.UserID())
	head, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	object, err := f.engine.backend.Get(ctx, storageformat.DomainPagePackKey(reference.Kind, reference.ID, head.head.Base.PackID))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := storageformat.DecodeDomainPagePack(ctx, object.Body, reference.ID, reference.Kind, head.head.Base.PackID)
	if err != nil {
		t.Fatal(err)
	}
	for _, packed := range pack.Pages {
		pageBody, err := storageformat.EncodeCanonical(packed.Page)
		if err != nil {
			t.Fatal(err)
		}
		families := make(map[string]int)
		var values int
		for _, entry := range packed.Page.Entries {
			family := "outcome-or-expiry"
			if strings.HasPrefix(entry.Key, "upload/") {
				family = "upload"
			} else if strings.HasPrefix(entry.Key, "upload-idempotency/") {
				family = "idempotency"
			}
			families[family]++
			values += len(entry.Value)
		}
		t.Logf("synthetic basepack census packPages=%d level=%d entries=%d children=%d canonicalBytes=%d valueBytes=%d families=%v", len(pack.Pages), packed.Page.Level, len(packed.Page.Entries), len(packed.Page.Children), len(pageBody), values, families)
	}
	reader := newConsistencyDomainTreeSession(store, reference)
	entries, err := reader.collect(ctx, head.head.Outcomes, "", "", 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		var outcome storageformat.DomainOutcome
		if err := decodeCanonicalValue(entry.Value, &outcome); err != nil {
			t.Fatal(err)
		}
		t.Logf("synthetic retained phase idBytes=%d resultBytes=%d deltaCount=%d", len(entry.Key), len(outcome.Result), len(head.head.Deltas))
	}
}
