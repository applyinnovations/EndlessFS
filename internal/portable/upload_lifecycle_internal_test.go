package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
)

type lifecycleStorage interface {
	UploadLifecycle(context.Context, domain.Scope, []domain.UploadID, bool) ([]domain.UploadLifecycleItem, error)
}

func TestUploadActionsReplacementPreservesTargetVersion(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x17578050)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("replacement-lifecycle", 2)
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	uploadEmptyCapabilities(t, fixture.server.Client(), caps)
	items := make([]domain.CompleteUploadBatchItem, 2)
	for index, cap := range caps {
		items[index] = domain.CompleteUploadBatchItem{UploadID: cap.UploadID, CRC32C: "AAAAAA"}
	}
	initial, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, domain.CompleteUploadBatchRequest{Items: items, IdempotencyKey: "replacement-lifecycle-initial"})
	if err != nil {
		t.Fatal(err)
	}
	for index := range requests {
		requests[index].Conflict = domain.ConflictReplace
		requests[index].ExpectedVersion = initial.Entries[index].Version
		requests[index].IdempotencyKey += "-replace"
	}
	replacements, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	uploadEmptyCapabilities(t, fixture.server.Client(), replacements)
	if _, err := fixture.engine.Files().Delete(ctx, fixture.scope, domain.DeleteRequest{Path: requests[1].Path, ExpectedVersion: initial.Entries[1].Version, IdempotencyKey: "replacement-lifecycle-remove-target"}); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, []domain.UploadActionItem{{UploadID: replacements[0].UploadID, Action: "complete", CRC32C: "AAAAAA"}, {UploadID: replacements[1].UploadID, Action: "complete", CRC32C: "AAAAAA"}})
	if err != nil || result[0].Outcome != "applied" || result[1].ErrorKind != domain.ErrorPreconditionFailed {
		t.Fatalf("changed replacement target poisoned sibling or bypassed version: %+v %v", result, err)
	}
	if _, err := fixture.engine.Files().Stat(ctx, fixture.scope, requests[1].Path); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removed target resurrected: %v", err)
	}
}

type lifecycleControlOnly struct{ objectstore.FileControlBackend }

type lifecycleObservedClock struct {
	domain.Clock
	once     sync.Once
	observed chan struct{}
}

func (clock *lifecycleObservedClock) Now() time.Time {
	value := clock.Clock.Now()
	clock.once.Do(func() { close(clock.observed) })
	return value
}

func TestUploadLifecycleRechecksExpiryAfterWaitingForProviderBudget(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x17578052)
	ctx := context.Background()
	capability, err := fixture.engine.Files().CreateUpload(ctx, fixture.scope, scaleUploadRequestsCount("queued-expiry-lifecycle", 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	clock := fixture.engine.clock.(*domain.FixedClock)
	observed := make(chan struct{})
	fixture.engine.clock = &lifecycleObservedClock{Clock: clock, observed: observed}
	for range cap(fixture.engine.uploadLifecycleSlots) {
		fixture.engine.uploadLifecycleSlots <- struct{}{}
	}
	type outcome struct {
		items []domain.UploadLifecycleItem
		err   error
	}
	done := make(chan outcome, 1)
	fixture.fileLedger.Reset()
	go func() {
		items, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, []domain.UploadID{capability.UploadID}, true)
		done <- outcome{items, err}
	}()
	<-observed
	clock.Advance(capability.ExpiresAt.Sub(clock.Now()))
	for range cap(fixture.engine.uploadLifecycleSlots) {
		<-fixture.engine.uploadLifecycleSlots
	}
	result := <-done
	if result.err != nil || result.items[0].Status.State != domain.UploadStateExpired || result.items[0].Capability != nil || len(fixture.fileLedger.Events()) != 0 {
		t.Fatalf("queued expiry performed native work or renewed authority: %+v %v", result.items, result.err)
	}
}

func TestUploadActionsCancellationJoinsNativeWork(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x17578053)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequestsCount("cancelled-actions-lifecycle", 100))
	if err != nil {
		t.Fatal(err)
	}
	base := fixture.engine.fileBackend
	entered := make(chan struct{}, 100)
	var active atomic.Int64
	probe := &lifecycleProbe{FileControlBackend: base, DirectTransferBackend: base.(objectstore.DirectTransferBackend)}
	probe.abort = func(ctx context.Context, _ []byte) error {
		active.Add(1)
		defer active.Add(-1)
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	fixture.engine.fileBackend = probe
	items := make([]domain.UploadActionItem, len(caps))
	for index, cap := range caps {
		items[index] = domain.UploadActionItem{UploadID: cap.UploadID, Action: "cancel"}
	}
	done := make(chan error, 1)
	go func() { _, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, items); done <- err }()
	for range caps {
		<-entered
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled actions: %v", err)
	}
	if active.Load() != 0 {
		t.Fatalf("action request returned with %d detached native operations", active.Load())
	}
	fixture.engine.fileBackend = base
	status, err := fixture.engine.Files().UploadLifecycle(context.Background(), fixture.scope, []domain.UploadID{caps[0].UploadID}, false)
	if err != nil || status[0].Status.State != domain.UploadStateActive {
		t.Fatalf("cancelled request falsely committed cancellation: %+v %v", status, err)
	}
}

func TestUploadActionsProviderDenialAndLostCancellationRecovery(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x17578051)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("provider-lifecycle", 2)
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	uploadEmptyCapabilities(t, fixture.server.Client(), caps[:1])
	base := fixture.engine.fileBackend
	fixture.engine.fileBackend = &lifecycleControlOnly{FileControlBackend: base}
	if _, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, []domain.UploadID{caps[0].UploadID}, true); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("missing native recovery interface did not fail closed: %v", err)
	}
	if _, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, []domain.UploadActionItem{{UploadID: caps[0].UploadID, Action: "complete", CRC32C: "AAAAAA"}}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("missing native action interface did not fail closed: %v", err)
	}
	probe := &lifecycleProbe{FileControlBackend: base, DirectTransferBackend: base.(objectstore.DirectTransferBackend)}
	probe.verify = func(context.Context, objectstore.Key, objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
		return objectstore.ObjectInfo{Size: 1, Fingerprint: objectstore.FingerprintFor(nil)}, nil
	}
	probe.abort = func(ctx context.Context, lease []byte) error {
		if err := probe.DirectTransferBackend.AbortUpload(ctx, lease); err != nil {
			return err
		}
		return domain.ErrUnavailable
	}
	fixture.engine.fileBackend = probe
	items := []domain.UploadActionItem{{UploadID: caps[0].UploadID, Action: "complete", CRC32C: "AAAAAA"}, {UploadID: caps[1].UploadID, Action: "cancel"}}
	result, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, items)
	if err != nil || result[0].ErrorKind != domain.ErrorPreconditionFailed || result[1].Outcome != "unknown" || !result[1].Retryable {
		t.Fatalf("invalid attestation/lost cancellation falsely succeeded: %+v %v", result, err)
	}
	if _, err := fixture.engine.Files().Stat(ctx, fixture.scope, requests[0].Path); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("invalid provider metadata published a file: %v", err)
	}
	probe.verify = nil
	probe.abort = func(context.Context, []byte) error { return domain.ErrNotFound }
	recovered, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, items)
	if err != nil || recovered[0].Outcome != "applied" || recovered[1].Outcome != "applied" {
		t.Fatalf("independent verification/cancellation recovery: %+v %v", recovered, err)
	}
	fixture.engine.fileBackend = base
	replay, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, items)
	if err != nil || replay[0].Outcome != "already_applied" || replay[1].Outcome != "already_applied" {
		t.Fatalf("terminal receipts lost after provider recovery: %+v %v", replay, err)
	}
}

func TestUploadLifecycleDeadlineDataCompleteAndTerminalPrecedence(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x17578048)
	ctx := context.Background()
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequestsCount("deadline-lifecycle", 2))
	if err != nil {
		t.Fatal(err)
	}
	uploadEmptyCapabilities(t, fixture.server.Client(), caps[:1])
	fixture.fileLedger.Reset()
	ready, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, []domain.UploadID{caps[0].UploadID}, true)
	if err != nil || !ready[0].Status.DataComplete || ready[0].NextAction != "complete" || ready[0].Capability != nil {
		t.Fatalf("data-complete session resumed: %+v %v", ready, err)
	}
	for _, event := range fixture.fileLedger.Events() {
		if event.Kind == providerbudget.RequestUploadResume {
			t.Fatalf("data-complete session reissued capability: %+v", event)
		}
	}
	completed, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, []domain.UploadActionItem{{UploadID: caps[0].UploadID, Action: "complete", CRC32C: "AAAAAA"}})
	if err != nil || completed[0].Outcome != "applied" {
		t.Fatalf("finalization: %+v %v", completed, err)
	}
	clock := fixture.engine.clock.(*domain.FixedClock)
	clock.Advance(caps[1].ExpiresAt.Sub(clock.Now()) - time.Nanosecond)
	before, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, []domain.UploadID{caps[1].UploadID}, true)
	if err != nil || before[0].Capability == nil {
		t.Fatalf("deadline rounded early: %+v %v", before, err)
	}
	clock.Advance(time.Nanosecond)
	fixture.fileLedger.Reset()
	boundary, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, []domain.UploadID{caps[0].UploadID, caps[1].UploadID}, true)
	if err != nil || boundary[0].Status.State != domain.UploadStateCompleted || boundary[1].Status.State != domain.UploadStateExpired || len(fixture.fileLedger.Events()) != 0 {
		t.Fatalf("exact expiry did native work or changed completed receipt: %+v %v", boundary, err)
	}
	rejected, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, []domain.UploadActionItem{{UploadID: caps[1].UploadID, Action: "complete", CRC32C: "AAAAAA"}, {UploadID: caps[0].UploadID, Action: "cancel"}})
	if err != nil || rejected[0].NextAction != "restart" || rejected[1].Outcome != "already_completed" {
		t.Fatalf("terminal action precedence: %+v %v", rejected, err)
	}
}

func TestUploadActionsInputAndDestinationIsolation(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x17578049)
	ctx := context.Background()
	for _, items := range [][]domain.UploadActionItem{nil, make([]domain.UploadActionItem, 101), {{UploadID: ""}}, {{UploadID: domain.UploadID(strings.Repeat("x", 257))}}, {{UploadID: "same"}, {UploadID: "same"}}} {
		if _, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, items); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid envelope: %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := fixture.engine.Files().UploadActions(cancelled, fixture.scope, []domain.UploadActionItem{{UploadID: "missing", Action: "cancel"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled action admitted: %v", err)
	}
	if _, err := fixture.engine.Files().UploadLifecycle(cancelled, fixture.scope, []domain.UploadID{"missing"}, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled recovery admitted: %v", err)
	}
	requested := scaleUploadRequestsCount("destination-lifecycle", 4)
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requested)
	if err != nil {
		t.Fatal(err)
	}
	uploadEmptyCapabilities(t, fixture.server.Client(), caps)
	if _, err := fixture.engine.Files().CreateDirectory(ctx, fixture.scope, domain.CreateDirectoryRequest{Path: requested[0].Path}); err != nil {
		t.Fatal(err)
	}
	results, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, []domain.UploadActionItem{{UploadID: caps[0].UploadID, Action: "complete", CRC32C: "AAAAAA"}, {UploadID: caps[1].UploadID, Action: "complete", CRC32C: "bad"}, {UploadID: caps[2].UploadID, Action: "cancel", CRC32C: "AAAAAA"}, {UploadID: caps[3].UploadID, Action: "cancel"}})
	if err != nil || results[0].ErrorKind != domain.ErrorConflict || results[1].ErrorKind != domain.ErrorInvalid || results[2].ErrorKind != domain.ErrorInvalid || results[3].Outcome != "applied" {
		t.Fatalf("poisoned destination/actions blocked valid sibling: %+v %v", results, err)
	}
	aborted, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, []domain.UploadActionItem{{UploadID: caps[3].UploadID, Action: "complete", CRC32C: "AAAAAA"}})
	if err != nil || aborted[0].NextAction != "restart" {
		t.Fatalf("aborted upload completed: %+v %v", aborted, err)
	}
	duplicate := scaleUploadRequestsCount("duplicate-destination-lifecycle", 1)[0]
	first, err := fixture.engine.Files().CreateUpload(ctx, fixture.scope, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	duplicate.IdempotencyKey += "-second"
	second, err := fixture.engine.Files().CreateUpload(ctx, fixture.scope, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	uploadEmptyCapabilities(t, fixture.server.Client(), []domain.UploadCapability{first, second})
	results, err = fixture.engine.Files().UploadActions(ctx, fixture.scope, []domain.UploadActionItem{{UploadID: first.UploadID, Action: "complete", CRC32C: "AAAAAA"}, {UploadID: second.UploadID, Action: "complete", CRC32C: "AAAAAA"}})
	if err != nil {
		t.Fatal(err)
	}
	applied, rejected := 0, 0
	for _, item := range results {
		if item.Outcome == "applied" {
			applied++
		}
		if item.ErrorKind == domain.ErrorConflict {
			rejected++
		}
	}
	if applied != 1 || rejected != 1 {
		t.Fatalf("conflicting destinations lost deterministic single publication: %+v", results)
	}
}

func TestUploadActionsRecoverAfterTerminalBoundaries(t *testing.T) {
	for _, scenario := range []struct{ action, step string }{{"complete", StepUploadBatchCompletionVerified}, {"complete", StepUploadBatchCompletionPublished}, {"cancel", StepUploadBatchAbortApplied}, {"cancel", StepUploadBatchAbortPublished}} {
		t.Run(scenario.step, func(t *testing.T) {
			fixture := newUploadBatchScaleFixture(t, 0x17578046)
			ctx := context.Background()
			caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequestsCount("fault-actions", 3))
			if err != nil {
				t.Fatal(err)
			}
			uploadEmptyCapabilities(t, fixture.server.Client(), caps)
			requested := make([]domain.UploadActionItem, len(caps))
			for i, cap := range caps {
				requested[i] = domain.UploadActionItem{UploadID: cap.UploadID, Action: scenario.action}
				if scenario.action == "complete" {
					requested[i].CRC32C = "AAAAAA"
				}
			}
			var fired atomic.Bool
			fixture.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
				if step == scenario.step && fired.CompareAndSwap(false, true) {
					return domain.ErrUnavailable
				}
				return nil
			})
			result, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, requested)
			if err != nil {
				t.Fatal(err)
			}
			if !fired.Load() {
				t.Fatal("fault boundary not exercised")
			}
			for _, item := range result {
				if item.Outcome != "unknown" || !item.Retryable {
					t.Fatalf("uncertain terminal effects misreported: %+v", item)
				}
			}
			fixture.engine.scheduler = nil
			replay, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, []domain.UploadActionItem{requested[2], requested[0]})
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range replay {
				if item.Outcome != "applied" && item.Outcome != "already_applied" {
					t.Fatalf("selective fault recovery failed: %+v", item)
				}
			}
			final, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, requested)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range final {
				if item.Outcome != "applied" && item.Outcome != "already_applied" {
					t.Fatalf("remaining intent lost: %+v", item)
				}
			}
		})
	}
}

func TestUploadActionsTwoThroughEightReplicasConverge(t *testing.T) {
	for replicas := 2; replicas <= 8; replicas++ {
		t.Run(fmt.Sprint(replicas), func(t *testing.T) {
			fixture := newUploadBatchScaleFixture(t, 0x17578047)
			ctx := context.Background()
			engines := []*Engine{fixture.engine}
			for index := 1; index < replicas; index++ {
				engine, err := Open(ctx, Options{Backend: fixture.engine.backend, FileBackend: fixture.engine.fileBackend, Clock: fixture.engine.clock, IDs: domain.NewIDGenerator(&deterministicScaleReader{state: uint64(index + 1000)}), Writer: WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}}, LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32)})
				if err != nil {
					t.Fatal(err)
				}
				engines = append(engines, engine)
			}
			requests := scaleUploadRequestsCount("replica-actions", 4)
			caps, err := engines[0].Files().CreateUploadBatch(ctx, fixture.scope, requests)
			if err != nil {
				t.Fatal(err)
			}
			uploadEmptyCapabilities(t, fixture.server.Client(), caps[:2])
			items := make([]domain.UploadActionItem, 4)
			for index, cap := range caps {
				items[index] = domain.UploadActionItem{UploadID: cap.UploadID, Action: "cancel"}
				if index < 2 {
					items[index].Action = "complete"
					items[index].CRC32C = "AAAAAA"
				}
			}
			start := make(chan struct{})
			failures := make(chan error, replicas)
			var joined sync.WaitGroup
			for _, engine := range engines {
				joined.Add(1)
				go func(engine *Engine) {
					defer joined.Done()
					<-start
					_, err := engine.Files().UploadActions(ctx, fixture.scope, items)
					failures <- err
				}(engine)
			}
			close(start)
			joined.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, engine := range engines {
				result, err := engine.Files().UploadActions(ctx, fixture.scope, items)
				if err != nil {
					t.Fatal(err)
				}
				for index, item := range result {
					if item.Outcome != "already_applied" {
						t.Fatalf("replicas failed to converge: %+v", item)
					}
					if index < 2 {
						entry, err := engine.Files().Stat(ctx, fixture.scope, requests[index].Path)
						if err != nil || item.Entry == nil || entry.Version != item.Entry.Version {
							t.Fatalf("duplicate or inconsistent file: %+v %v", item, err)
						}
					}
				}
			}
		})
	}
}

type uploadActionStorage interface {
	UploadActions(context.Context, domain.Scope, []domain.UploadActionItem) ([]domain.UploadActionResult, error)
}

func TestUploadActionsPartialSuccessSelectiveReplayAndChecksumBinding(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x17478046)
	ctx := context.Background()
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequestsCount("lifecycle-actions", 5))
	if err != nil {
		t.Fatal(err)
	}
	uploadEmptyCapabilities(t, fixture.server.Client(), caps[:4])
	if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{{UploadID: caps[0].UploadID, CRC32C: "AAAAAA"}}, IdempotencyKey: "already-completed-action"}); err != nil {
		t.Fatal(err)
	}
	storage, ok := any(fixture.engine.Files()).(uploadActionStorage)
	if !ok {
		t.Fatal("storage lacks independently reported bulk terminal actions")
	}
	items := []domain.UploadActionItem{{UploadID: caps[0].UploadID, Action: "complete", CRC32C: "AAAAAA"}, {UploadID: caps[1].UploadID, Action: "complete", CRC32C: "AAAAAA"}, {UploadID: caps[2].UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor([]byte("different")).CRC32C}, {UploadID: caps[3].UploadID, Action: "cancel"}, {UploadID: caps[4].UploadID, Action: "complete", CRC32C: "AAAAAA"}, {UploadID: "missing", Action: "cancel"}}
	results, err := storage.UploadActions(ctx, fixture.scope, items)
	if err != nil || len(results) != len(items) {
		t.Fatalf("actions: %+v %v", results, err)
	}
	if results[0].Outcome != "already_applied" || results[0].Entry == nil || results[1].Outcome != "applied" || results[1].Entry == nil || results[3].Outcome != "applied" || results[3].Status == nil || results[3].Status.State != domain.UploadStateAborted {
		t.Fatalf("valid siblings poisoned: %+v", results)
	}
	for _, index := range []int{2, 4, 5} {
		if results[index].Outcome != "rejected" || results[index].ErrorKind == "" || results[index].Entry != nil {
			t.Fatalf("invalid item %d falsely applied: %+v", index, results[index])
		}
	}
	fixture.fileLedger.Reset()
	replay, err := storage.UploadActions(ctx, fixture.scope, []domain.UploadActionItem{items[3], items[1]})
	if err != nil || replay[0].Outcome != "already_applied" || replay[1].Outcome != "already_applied" || len(fixture.fileLedger.Events()) != 0 {
		t.Fatalf("selective/reordered replay repeated provider effects: %+v %v events=%+v", replay, err, fixture.fileLedger.Events())
	}
	changed := items[1]
	changed.CRC32C = items[2].CRC32C
	replay, err = storage.UploadActions(ctx, fixture.scope, []domain.UploadActionItem{changed})
	if err != nil || replay[0].Outcome != "rejected" || replay[0].ErrorKind != domain.ErrorConflict {
		t.Fatalf("changed completed intent accepted: %+v %v", replay, err)
	}
}

type lifecycleProbe struct {
	objectstore.FileControlBackend
	objectstore.DirectTransferBackend
	progress func(context.Context, []byte) (objectstore.UploadProgress, error)
	resume   func(context.Context, []byte) (objectstore.UploadCapability, error)
	verify   func(context.Context, objectstore.Key, objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error)
	abort    func(context.Context, []byte) error
}

func (p *lifecycleProbe) Verify(ctx context.Context, key objectstore.Key, expected objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
	if p.verify != nil {
		return p.verify(ctx, key, expected)
	}
	return p.FileControlBackend.Verify(ctx, key, expected)
}
func (p *lifecycleProbe) AbortUpload(ctx context.Context, lease []byte) error {
	if p.abort != nil {
		return p.abort(ctx, lease)
	}
	return p.DirectTransferBackend.AbortUpload(ctx, lease)
}

func (p *lifecycleProbe) UploadProgress(ctx context.Context, lease []byte) (objectstore.UploadProgress, error) {
	if p.progress != nil {
		return p.progress(ctx, lease)
	}
	return p.DirectTransferBackend.UploadProgress(ctx, lease)
}
func (p *lifecycleProbe) ResumeUpload(ctx context.Context, lease []byte) (objectstore.UploadCapability, error) {
	if p.resume != nil {
		return p.resume(ctx, lease)
	}
	return p.DirectTransferBackend.ResumeUpload(ctx, lease)
}

func TestUploadLifecycleProcessBudgetAndCancellation(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x12478046)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequestsCount("bounded-lifecycle", 100))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]domain.UploadID, len(caps))
	for i, c := range caps {
		ids[i] = c.UploadID
	}
	base := fixture.engine.fileBackend
	var active, peak atomic.Int64
	entered := make(chan struct{}, domain.MaxUploadLifecycleItems)
	fixture.engine.fileBackend = &lifecycleProbe{FileControlBackend: base, DirectTransferBackend: base.(objectstore.DirectTransferBackend), progress: func(ctx context.Context, _ []byte) (objectstore.UploadProgress, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- struct{}{}
		<-ctx.Done()
		return objectstore.UploadProgress{}, ctx.Err()
	}}
	done := make(chan error, 2)
	for page := 0; page < 2; page++ {
		go func() { _, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, ids, true); done <- err }()
	}
	for count := 0; count < uploadLifecycleProviderConcurrency; count++ {
		<-entered
	}
	cancel()
	for page := 0; page < 2; page++ {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	}
	if peak.Load() > uploadLifecycleProviderConcurrency || active.Load() != 0 {
		t.Fatalf("detached/unbounded provider work: peak=%d active=%d", peak.Load(), active.Load())
	}
}

func TestUploadLifecycleBoundsAndProviderFailures(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x77348046)
	ctx := context.Background()
	cap, err := fixture.engine.Files().CreateUpload(ctx, fixture.scope, scaleUploadRequestsCount("lifecycle-denials", 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]domain.UploadID{nil, {""}, {"a", "a"}, {domain.UploadID(string(make([]byte, 257)))}, make([]domain.UploadID, 101)} {
		if _, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, ids, true); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid page: %v", err)
		}
	}
	base := fixture.engine.fileBackend
	probe := &lifecycleProbe{FileControlBackend: base, DirectTransferBackend: base.(objectstore.DirectTransferBackend)}
	fixture.engine.fileBackend = probe
	for _, failure := range []error{domain.ErrUnavailable, domain.ErrRateLimited, domain.ErrInvalid} {
		probe.progress = func(context.Context, []byte) (objectstore.UploadProgress, error) {
			return objectstore.UploadProgress{}, failure
		}
		items, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, []domain.UploadID{cap.UploadID, "missing"}, true)
		if err != nil || items[0].ErrorKind != domain.KindOf(failure) || items[0].Retryable == (failure == domain.ErrInvalid) || items[1].ErrorKind != domain.ErrorNotFound {
			t.Fatalf("mixed failures: %+v %v", items, err)
		}
	}
	probe.progress = func(context.Context, []byte) (objectstore.UploadProgress, error) {
		return objectstore.UploadProgress{Offset: 1}, nil
	}
	items, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, []domain.UploadID{cap.UploadID}, true)
	if err != nil || items[0].ErrorKind != domain.ErrorInvalid {
		t.Fatalf("invalid provider offset: %+v %v", items, err)
	}
	probe.progress = nil
	probe.resume = func(context.Context, []byte) (objectstore.UploadCapability, error) {
		return objectstore.UploadCapability{ExpiresAt: cap.ExpiresAt.Add(time.Hour)}, nil
	}
	items, err = fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, []domain.UploadID{cap.UploadID}, true)
	if err != nil || items[0].ErrorKind != domain.ErrorInvalid || items[0].Capability != nil {
		t.Fatalf("renewed provider deadline: %+v %v", items, err)
	}
}

func TestProviderBudgetUploadLifecycleSharedPageComparison(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x93948046)
	ctx := context.Background()
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequestsCount("lifecycle-economics", 100))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]domain.UploadID, len(caps))
	for i, c := range caps {
		ids[i] = c.UploadID
	}
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"single-status-resume-status", "shared-page"} {
		fixture.stateLedger.Reset()
		fixture.fileLedger.Reset()
		if mode == "shared-page" {
			_, err = fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, ids, true)
		} else {
			for _, id := range ids {
				if _, err = fixture.engine.Files().UploadStatus(ctx, fixture.scope, id); err != nil {
					t.Fatal(err)
				}
				if _, err = fixture.engine.Files().ResumeUpload(ctx, fixture.scope, id); err != nil {
					t.Fatal(err)
				}
				if _, err = fixture.engine.Files().UploadStatus(ctx, fixture.scope, id); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		events := append(fixture.stateLedger.Events(), fixture.fileLedger.Events()...)
		totals, err := model.Estimate(events)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(totals)
		t.Logf("lifecycle-economics mode=%s state=%d file=%d totals=%s", mode, len(fixture.stateLedger.Events()), len(fixture.fileLedger.Events()), body)
		if mode == "shared-page" {
			checkTransferScaleBudget(t, "file-upload-lifecycle-resume-100-schema-012", fixture.stateLedger.Events(), fixture.fileLedger.Events())
			for _, event := range fixture.fileLedger.Events() {
				if event.Kind == providerbudget.RequestObjectGet || event.Kind == providerbudget.RequestObjectOpen || event.Kind == providerbudget.RequestDataDownload {
					t.Fatalf("control plane read file bodies: %+v", event)
				}
			}
		}
	}
}

func TestProviderBudgetUploadLifecyclePagesAndActions(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"status", "expired", "actions", "replay"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newUploadBatchScaleFixture(t, 0x94448046)
			caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, scaleUploadRequestsCount("lifecycle-budget", 100))
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]domain.UploadID, 100)
			actions := make([]domain.UploadActionItem, 100)
			for i, c := range caps {
				ids[i] = c.UploadID
				actions[i] = domain.UploadActionItem{UploadID: c.UploadID, Action: "cancel"}
				if i < 50 {
					actions[i].Action = "complete"
					actions[i].CRC32C = "AAAAAA"
				}
			}
			name := "file-upload-lifecycle-status-100-schema-012"
			if mode == "expired" {
				fixture.engine.clock.(*domain.FixedClock).Advance(24 * time.Hour)
				name = "file-upload-lifecycle-expired-100-schema-012"
			}
			if mode == "actions" || mode == "replay" {
				uploadEmptyCapabilities(t, fixture.server.Client(), caps[:50])
				name = "file-upload-actions-mixed-100-schema-012"
			}
			if mode == "replay" {
				if _, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, actions); err != nil {
					t.Fatal(err)
				}
				name = "file-upload-actions-replay-100-schema-012"
			}
			fixture.stateLedger.Reset()
			fixture.fileLedger.Reset()
			if mode == "actions" || mode == "replay" {
				result, err := fixture.engine.Files().UploadActions(ctx, fixture.scope, actions)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range result {
					if item.Outcome != "applied" && item.Outcome != "already_applied" {
						t.Fatalf("action not applied: %+v", item)
					}
				}
			} else {
				if _, err := fixture.engine.Files().UploadLifecycle(ctx, fixture.scope, ids, false); err != nil {
					t.Fatal(err)
				}
			}
			model, err := gcs.RegionalStandardFlatEconomics()
			if err != nil {
				t.Fatal(err)
			}
			events := append(fixture.stateLedger.Events(), fixture.fileLedger.Events()...)
			budget, err := providerbudget.Calibrate(name, model, []providerbudget.Role{providerbudget.RoleState, providerbudget.RoleFile}, events)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(budget)
			t.Logf("LIFECYCLE_CALIBRATION=%s", body)
			if directory := os.Getenv("ENDLESSFS_LIFECYCLE_BUDGET_EVIDENCE"); directory != "" {
				if err := os.WriteFile(filepath.Join(directory, mode+".json"), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			checkTransferScaleBudget(t, name, fixture.stateLedger.Events(), fixture.fileLedger.Events())
		})
	}
}

func TestUploadLifecycleMixedExpiryRecoveryAndSelectiveRetry(t *testing.T) {
	fixture := newUploadBatchScaleFixture(t, 0x91348046)
	ctx := context.Background()
	fixture.engine.uploadTTL = 5 * time.Minute
	fixture.engine.uploadSessionTTL = 24 * time.Hour
	requests := scaleUploadRequestsCount("lifecycle", 2)
	caps, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	storage, ok := any(fixture.engine.Files()).(lifecycleStorage)
	if !ok {
		t.Fatal("storage lacks bounded, per-item upload recovery")
	}
	ids := []domain.UploadID{caps[0].UploadID, "missing", caps[1].UploadID}
	items, err := storage.UploadLifecycle(ctx, fixture.scope, ids, true)
	if err != nil || len(items) != len(ids) {
		t.Fatalf("mixed recovery: %+v, %v", items, err)
	}
	for _, index := range []int{0, 2} {
		item := items[index]
		if item.Index != index || item.UploadID != ids[index] || item.Status == nil || item.Status.State != domain.UploadStateActive || item.Capability == nil || item.Capability.UploadID != ids[index] || item.ErrorKind != "" {
			t.Fatalf("active sibling poisoned by missing item: %+v", item)
		}
	}
	if items[1].ErrorKind != domain.ErrorNotFound || items[1].Status != nil || items[1].Capability != nil || items[1].Retryable {
		t.Fatalf("missing result disclosed state: %+v", items[1])
	}
	fixture.engine.clock.(*domain.FixedClock).Advance(24 * time.Hour)
	items, err = storage.UploadLifecycle(ctx, fixture.scope, ids, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 2} {
		item := items[index]
		if item.Status == nil || item.Status.State != domain.UploadStateExpired || item.Capability != nil || item.Retryable || item.NextAction != "restart" || !item.Status.ExpiresAt.Equal(caps[0].ExpiresAt) {
			t.Fatalf("expired authority renewed or hidden: %+v", item)
		}
	}
	// Selecting one terminal item for a new attempt must not renew its sibling.
	requests[0].IdempotencyKey += "-new-attempt"
	fresh, err := fixture.engine.Files().CreateUpload(ctx, fixture.scope, requests[0])
	if err != nil || fresh.UploadID == caps[0].UploadID {
		t.Fatalf("explicit fresh attempt: %+v, %v", fresh, err)
	}
	items, err = storage.UploadLifecycle(ctx, fixture.scope, []domain.UploadID{fresh.UploadID, caps[1].UploadID}, true)
	if err != nil || items[0].Capability == nil || items[1].Status.State != domain.UploadStateExpired {
		t.Fatalf("selective recovery changed sibling: %+v, %v", items, err)
	}
}
