package portable

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func capabilityBoundaryNoPublication(t *testing.T, f uploadBatchScaleFixture, want error, apply func() error) {
	t.Helper()
	f.stateLedger.Reset()
	f.fileLedger.Reset()
	if err := apply(); !errors.Is(err, want) {
		t.Fatalf("capability boundary result=%v, want %v", err, want)
	}
	for _, event := range f.stateLedger.Events() {
		if event.Kind != providerbudget.RequestObjectGet {
			t.Fatalf("denied or replayed terminal request performed state mutation: %s", event.Kind)
		}
	}
	if len(f.fileLedger.Events()) != 0 {
		t.Fatalf("denied or replayed terminal request performed %d native operations", len(f.fileLedger.Events()))
	}
}

func TestUploadCapabilityBoundaryReviewCompletedReceiptRevalidation(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x179ac101)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("capability-terminal-boundary", 2)
	for i := range requests {
		requests[i].Size = 4
	}
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(page) != 2 || page[0].Capability == nil || page[1].Capability == nil {
		t.Fatalf("admission: %+v %v", page, err)
	}
	payload := []byte("four")
	crc := objectstore.FingerprintFor(payload).CRC32C
	for _, item := range page {
		admissionWriteBytes(t, f, *item.Capability, payload, 0)
	}
	first := domain.CompleteUploadBatchItem{UploadID: page[0].UploadID, CRC32C: crc}
	second := domain.CompleteUploadBatchItem{UploadID: page[1].UploadID, CRC32C: crc}
	// The public predecessor receipt remains a valid natural-record replay source.
	original, err := f.engine.Files().CompleteUploadBatch(ctx, f.scope, domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{first}, IdempotencyKey: "capability-boundary-original-completion"})
	if err != nil || len(original.Entries) != 1 || original.Entries[0].Size != 4 {
		t.Fatalf("positive publication: %+v %v", original, err)
	}
	replayRequest := domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{first}, IdempotencyKey: "capability-boundary-independent-replay"}
	capabilityBoundaryNoPublication(t, f, nil, func() error {
		replay, err := f.engine.Files().completeUploadActionsProven(ctx, f.scope, replayRequest, nil)
		if err == nil && !reflect.DeepEqual(replay, original) {
			t.Fatalf("terminal record changed exact entry: %+v want %+v", replay, original)
		}
		return err
	})
	forged := replayRequest
	forged.Items = []domain.CompleteUploadBatchItem{{UploadID: first.UploadID, CRC32C: objectstore.FingerprintFor([]byte("evil")).CRC32C}}
	capabilityBoundaryNoPublication(t, f, domain.ErrConflict, func() error {
		_, err := f.engine.Files().completeUploadActionsProven(ctx, f.scope, forged, nil)
		return err
	})
	// A lower-level publication must fail closed when another replica changed only
	// one member after the outer action page validated its records/proofs.
	mixed := domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{first, second}, IdempotencyKey: "capability-boundary-mixed-completion"}
	capabilityBoundaryNoPublication(t, f, domain.ErrConflict, func() error {
		_, err := f.engine.Files().completeUploadActionsProven(ctx, f.scope, mixed, nil)
		return err
	})
	trash, _ := domain.NewScope(f.scope.UserID(), domain.AreaTrash)
	capabilityBoundaryNoPublication(t, f, domain.ErrNotFound, func() error {
		_, err := f.engine.Files().completeUploadActionsProven(ctx, trash, replayRequest, nil)
		return err
	})
	capabilityBoundaryNoPublication(t, f, domain.ErrConflict, func() error {
		_, err := f.engine.Files().CompleteUploadBatch(ctx, trash, domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{second}, IdempotencyKey: "capability-boundary-legacy-wrong-area"})
		return err
	})
	capabilityBoundaryNoPublication(t, f, domain.ErrInvalid, func() error {
		_, err := f.engine.Files().CompleteUploadBatch(ctx, f.scope, domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{second, second}, IdempotencyKey: "capability-boundary-duplicate-member"})
		return err
	})
	// Independent retry keeps the completed sibling's exact result and actually
	// publishes the still-active sibling rather than rejecting the whole page.
	result, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{{UploadID: first.UploadID, Action: "complete", CRC32C: crc}, {UploadID: second.UploadID, Action: "complete", CRC32C: crc}})
	if err != nil || len(result) != 2 || result[0].Outcome != "already_applied" || result[1].Outcome != "applied" || result[0].Entry == nil || !reflect.DeepEqual(*result[0].Entry, original.Entries[0]) {
		t.Fatalf("selective positive completion: %+v %v", result, err)
	}
	for _, request := range requests {
		entry, err := f.engine.Files().Stat(ctx, f.scope, request.Path)
		if err != nil || entry.Size != 4 {
			t.Fatalf("actual transferred file absent: %+v %v", entry, err)
		}
	}
}

func TestUploadCapabilityBoundaryReviewAbortedReceiptRevalidation(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x179ac111)
	ctx := context.Background()
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, scaleUploadRequestsCount("capability-cancel-boundary", 2))
	if err != nil || len(page) != 2 || page[0].Capability == nil || page[1].Capability == nil {
		t.Fatalf("admission: %+v %v", page, err)
	}
	first := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{page[0].UploadID}, IdempotencyKey: "capability-boundary-original-cancel"}
	if err := f.engine.Files().AbortUploadBatch(ctx, f.scope, first); err != nil {
		t.Fatal(err)
	}
	capabilityBoundaryNoPublication(t, f, nil, func() error { return f.engine.Files().abortUploadActionsProven(ctx, f.scope, first, nil) })
	mixed := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{page[0].UploadID, page[1].UploadID}, IdempotencyKey: "capability-boundary-mixed-cancel"}
	capabilityBoundaryNoPublication(t, f, domain.ErrConflict, func() error { return f.engine.Files().abortUploadActionsProven(ctx, f.scope, mixed, nil) })
	trash, _ := domain.NewScope(f.scope.UserID(), domain.AreaTrash)
	capabilityBoundaryNoPublication(t, f, domain.ErrNotFound, func() error { return f.engine.Files().abortUploadActionsProven(ctx, trash, first, nil) })
	capabilityBoundaryNoPublication(t, f, domain.ErrConflict, func() error {
		return f.engine.Files().AbortUploadBatch(ctx, trash, domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{page[1].UploadID}, IdempotencyKey: "capability-boundary-legacy-cancel-area"})
	})
	result, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{{UploadID: page[0].UploadID, Action: "cancel"}, {UploadID: page[1].UploadID, Action: "cancel"}})
	if err != nil || len(result) != 2 || result[0].Outcome != "already_applied" || result[1].Outcome != "applied" {
		t.Fatalf("selective positive cancellation: %+v %v", result, err)
	}
	statuses, err := f.engine.Files().UploadLifecycle(ctx, f.scope, []domain.UploadID{page[0].UploadID, page[1].UploadID}, false)
	if err != nil || len(statuses) != 2 || statuses[0].Status.State != domain.UploadStateAborted || statuses[1].Status.State != domain.UploadStateAborted {
		t.Fatalf("terminal cancel records: %+v %v", statuses, err)
	}
}

func TestUploadCapabilityBoundaryReviewReplicaCompletesDuringVerification(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x179ac121)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("capability-replica-boundary", 2)
	for i := range requests {
		requests[i].Size = 4
	}
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(page) != 2 || page[0].Capability == nil || page[1].Capability == nil {
		t.Fatalf("admission: %+v %v", page, err)
	}
	payload := []byte("four")
	crc := objectstore.FingerprintFor(payload).CRC32C
	for _, item := range page {
		admissionWriteBytes(t, f, *item.Capability, payload, 0)
	}
	replica := newAdmissionReplica(t, f, 0x179ac122)
	base := f.engine.fileBackend
	probe := &lifecycleProbe{FileControlBackend: base, DirectTransferBackend: base.(objectstore.DirectTransferBackend)}
	var once sync.Once
	var published domain.CompleteUploadBatchResult
	var publicationErr error
	// Actual native verification happens before this synchronous second-replica
	// head publication. No sleeps, background work, or fabricated proof versions.
	probe.verify = func(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
		info, err := base.Verify(ctx, key, expected)
		if err != nil {
			return info, err
		}
		once.Do(func() {
			published, publicationErr = replica.Files().CompleteUploadBatch(ctx, f.scope, domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{{UploadID: page[0].UploadID, CRC32C: crc}}, IdempotencyKey: "capability-boundary-competing-replica"})
		})
		if publicationErr != nil {
			return objectstore.ObjectInfo{}, publicationErr
		}
		return info, nil
	}
	f.engine.fileBackend = probe
	f.stateLedger.Reset()
	f.fileLedger.Reset()
	result, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{{UploadID: page[0].UploadID, Action: "complete", CRC32C: crc}, {UploadID: page[1].UploadID, Action: "complete", CRC32C: crc}})
	if err != nil || publicationErr != nil || len(published.Entries) != 1 || len(result) != 2 || result[0].Outcome != "already_applied" || result[1].Outcome != "applied" || result[0].Entry == nil || !reflect.DeepEqual(*result[0].Entry, published.Entries[0]) {
		t.Fatalf("replica boundary poisoned sibling: %+v publication=%+v err=%v competing=%v", result, published, err, publicationErr)
	}
	headKey := storageformat.DomainHeadKey(uploadDomainReference(f.scope.UserID()).Kind, uploadDomainReference(f.scope.UserID()).ID).String()
	heads, verifies := 0, 0
	verifiedKeys := make(map[string]int)
	for _, event := range f.stateLedger.Events() {
		if event.Kind == providerbudget.RequestObjectPut && event.Target == headKey && !event.Failed {
			heads++
		}
	}
	for _, event := range f.fileLedger.Events() {
		if event.Kind == providerbudget.RequestObjectVerify {
			verifies++
			verifiedKeys[event.Target]++
		} else {
			t.Fatalf("completion recreated or resumed native authority: %s", event.Kind)
		}
	}
	// Two original verifications, one competing-replica verification, and one
	// surviving-item revalidation in the single bounded outer retry round.
	if heads != 2 || verifies != 4 || len(verifiedKeys) != 2 {
		t.Fatalf("replica recovery repeated publication or exceeded its bounded integrity work: heads=%d want2 verifies=%d want4 objects=%d want2", heads, verifies, len(verifiedKeys))
	}
	for _, count := range verifiedKeys {
		if count != 2 {
			t.Fatalf("terminal or surviving item verification count=%d, want2", count)
		}
	}
	for i, request := range requests {
		entry, err := f.engine.Files().Stat(ctx, f.scope, request.Path)
		if err != nil || result[i].Entry == nil || !reflect.DeepEqual(entry, *result[i].Entry) || entry.Size != 4 {
			t.Fatalf("replica publication lost exact transferred entry: %+v %v", entry, err)
		}
	}
}
