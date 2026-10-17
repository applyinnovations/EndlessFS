package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type admissionProbe struct {
	objectstore.FileControlBackend
	objectstore.DirectTransferBackend
	begin func(context.Context, objectstore.UploadRequest) (objectstore.UploadHandle, error)
}

func (p *admissionProbe) BeginUpload(ctx context.Context, r objectstore.UploadRequest) (objectstore.UploadHandle, error) {
	return p.begin(ctx, r)
}

func TestUploadAdmissionProviderFailurePreservesAcceptedSiblings(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x17881001)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("independent-provider", 3)
	requests[1].Size = 1
	base, _ := f.engine.Files().transferBackend()
	var mu sync.Mutex
	begins := make(map[string]int)
	var fail atomic.Bool
	fail.Store(true)
	f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: base, begin: func(ctx context.Context, r objectstore.UploadRequest) (objectstore.UploadHandle, error) {
		mu.Lock()
		begins[r.UploadID]++
		mu.Unlock()
		if r.Size == 1 && fail.Load() {
			return objectstore.UploadHandle{}, domain.ErrUnavailable
		}
		return base.BeginUpload(ctx, r)
	}}
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || page[0].Capability == nil || page[1].Outcome != "unknown" || !page[1].Retryable || page[2].Capability == nil {
		t.Fatalf("mixed provider: %+v %v", page, err)
	}
	accepted := []domain.UploadID{page[0].UploadID, page[2].UploadID}
	failedID := page[1].UploadID
	fail.Store(false)
	retry, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests[1:2])
	if err != nil || retry[0].Capability == nil || retry[0].UploadID != failedID {
		t.Fatalf("selective retry: %+v %v", retry, err)
	}
	replay, err := f.engine.Files().AdmitUploads(ctx, f.scope, []domain.CreateUploadRequest{requests[2], requests[0]})
	if err != nil || replay[0].UploadID != accepted[1] || replay[1].UploadID != accepted[0] {
		t.Fatalf("accepted replay: %+v %v", replay, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if begins[string(accepted[0])] != 1 || begins[string(accepted[1])] != 1 || begins[string(failedID)] != 2 {
		t.Fatalf("accepted siblings recreated: %+v", begins)
	}
}

func TestUploadAdmissionRecoversDurableBoundaries(t *testing.T) {
	for _, step := range []string{StepUploadAdmissionAfterIntents, StepUploadAdmissionAfterLeases, StepUploadAdmissionAfterActivation} {
		t.Run(step, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x17881002)
			ctx := context.Background()
			requests := scaleUploadRequestsCount("admission-boundary", 3)
			base, _ := f.engine.Files().transferBackend()
			var begins atomic.Int64
			f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: base, begin: func(ctx context.Context, r objectstore.UploadRequest) (objectstore.UploadHandle, error) {
				begins.Add(1)
				return base.BeginUpload(ctx, r)
			}}
			f.engine.scheduler = SchedulerFunc(func(_ context.Context, current string) error {
				if current == step {
					return domain.ErrUnavailable
				}
				return nil
			})
			first, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
			if err != nil || len(first) != 3 {
				t.Fatalf("interrupted: %+v %v", first, err)
			}
			for _, item := range first {
				if item.Capability != nil || item.Outcome != "unknown" {
					t.Fatalf("exposed interrupted capability: %+v", item)
				}
			}
			f.engine.scheduler = nil
			recovered := newAdmissionReplica(t, f, 0x17881999)
			replay, err := recovered.Files().AdmitUploads(ctx, f.scope, []domain.CreateUploadRequest{requests[2], requests[0], requests[1]})
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range replay {
				if item.Capability == nil {
					t.Fatalf("recovery: %+v", replay)
				}
			}
			if begins.Load() != 3 {
				t.Fatalf("recreated durable sessions at %s: %d", step, begins.Load())
			}
		})
	}
}

func TestUploadAdmissionSharedPublicationComparison(t *testing.T) {
	model, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"legacy-cohort", "independent-page"} {
		t.Run(mode, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x17881003)
			requests := scaleUploadRequestsCount("admission-economics", 100)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			if mode == "legacy-cohort" {
				_, err = f.engine.Files().CreateUploadBatch(context.Background(), f.scope, requests)
			} else {
				var items []domain.UploadAdmissionResult
				items, err = f.engine.Files().AdmitUploads(context.Background(), f.scope, requests)
				for _, item := range items {
					if item.Capability == nil {
						t.Fatalf("admission: %+v", item)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			runtime.ReadMemStats(&after)
			t.Logf("admission-memory mode=%s allocatedBytes=%d retainedHeapDeltaBytes=%d", mode, after.TotalAlloc-before.TotalAlloc, int64(after.HeapAlloc)-int64(before.HeapAlloc))
			events := append(f.stateLedger.Events(), f.fileLedger.Events()...)
			totals, err := model.Estimate(events)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(totals)
			kinds := make(map[string]int)
			for _, event := range f.stateLedger.Events() {
				kinds[string(event.Kind)+":"+event.Subsystem]++
			}
			t.Logf("admission-comparison mode=%s state=%d native=%d kinds=%v totals=%s", mode, len(f.stateLedger.Events()), len(f.fileLedger.Events()), kinds, body)
		})
	}
}

// Lost lease-write responses must not abort the exact lease that committed.
// The wrapper executes the real durable write before losing its reply.
type admissionLostLeaseWrite struct {
	objectstore.Backend
	lost       atomic.Bool
	failRead   bool
	unreadable atomic.Bool
}

func (b *admissionLostLeaseWrite) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	version, err := b.Backend.Put(ctx, key, body, condition)
	if err == nil && condition.Mode == objectstore.PutCreateOnly && !b.lost.Load() {
		// Canonical writes use different roles; lease keys contain the leases family.
		if objectstoreKeyIsUploadLease(key) && b.lost.CompareAndSwap(false, true) {
			return version, domain.ErrUnavailable
		}
	}
	return version, err
}
func (b *admissionLostLeaseWrite) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if b.failRead && b.lost.Load() && objectstoreKeyIsUploadLease(key) && b.unreadable.CompareAndSwap(false, true) {
		return objectstore.Object{}, domain.ErrUnavailable
	}
	return b.Backend.Get(ctx, key)
}

func objectstoreKeyIsUploadLease(key objectstore.Key) bool {
	return strings.HasPrefix(key.String(), storageformat.LeasePrefix())
}

func TestUploadAdmissionLostLeaseWriteResponsePreservesCommittedSession(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x17881004)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("admission-lost-lease", 1)
	base, _ := f.engine.Files().transferBackend()
	var begins atomic.Int64
	f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: base, begin: func(ctx context.Context, r objectstore.UploadRequest) (objectstore.UploadHandle, error) {
		begins.Add(1)
		return base.BeginUpload(ctx, r)
	}}
	backend := &admissionLostLeaseWrite{Backend: f.engine.backend}
	f.engine.backend = backend
	first, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(first) != 1 || !backend.lost.Load() {
		t.Fatalf("lost write not exercised: %+v %v", first, err)
	}
	replay, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(replay) != 1 || replay[0].Capability == nil || begins.Load() != 1 {
		t.Fatalf("durable native session revoked or recreated: first=%+v replay=%+v begins=%d err=%v", first, replay, begins.Load(), err)
	}
}

func admissionWriteBytes(t *testing.T, f uploadBatchScaleFixture, cap domain.UploadCapability, body []byte, offset int64) {
	t.Helper()
	request, err := http.NewRequest(cap.Method, cap.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range cap.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Upload-Offset", fmt.Sprint(offset))
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("synthetic data=%d", response.StatusCode)
	}
}

func TestUploadAdmissionConfirmedProgressTerminalPrecedenceAndBounds(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x17881005)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("admission-progress", 3)
	for i := range requests {
		requests[i].Size = 3
	}
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	first := *page[0].Capability
	admissionWriteBytes(t, f, first, []byte("a"), 0)
	replay, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests[:1])
	if err != nil || replay[0].Status.ConfirmedOffset != 1 || replay[0].Capability == nil || replay[0].UploadID != first.UploadID {
		t.Fatalf("confirmed progress replay: %+v %v", replay, err)
	}
	admissionWriteBytes(t, f, first, []byte("bc"), 1)
	replay, err = f.engine.Files().AdmitUploads(ctx, f.scope, requests[:1])
	if err != nil || !replay[0].Status.DataComplete || replay[0].Capability != nil || replay[0].NextAction != "complete" {
		t.Fatalf("complete data replay: %+v %v", replay, err)
	}
	outcomes, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{{UploadID: first.UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor([]byte("abc")).CRC32C}, {UploadID: page[1].UploadID, Action: "cancel"}})
	if err != nil || outcomes[0].Outcome != "applied" || outcomes[1].Outcome != "applied" {
		t.Fatalf("terminal setup: %+v %v", outcomes, err)
	}
	f.engine.clock.(*domain.FixedClock).Advance(25 * time.Hour)
	f.fileLedger.Reset()
	replay, err = f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || replay[0].Outcome != "completed" || replay[1].Outcome != "aborted" || replay[2].Outcome != "expired" || len(f.fileLedger.Events()) != 0 {
		t.Fatalf("terminal/expiry precedence: %+v %v native=%d", replay, err, len(f.fileLedger.Events()))
	}
	for _, bad := range [][]domain.CreateUploadRequest{nil, make([]domain.CreateUploadRequest, 101), {requests[0], requests[0]}, {{IdempotencyKey: ""}}, {{IdempotencyKey: "short"}}, {{IdempotencyKey: strings.Repeat("x", 129)}}, {{IdempotencyKey: "invalid\nitem-key-0000"}}} {
		if _, err := f.engine.Files().AdmitUploads(ctx, f.scope, bad); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid envelope=%v", err)
		}
	}
	if _, err := f.engine.Files().AdmitUploads(ctx, domain.Scope{}, requests); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid scope=%v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.engine.Files().AdmitUploads(cancelled, f.scope, requests); err == nil {
		t.Fatal("cancelled context accepted")
	}
	invalid := domain.UploadAdmissionResult{}
	if !admissionTerminal(&invalid, storageformat.PortableUploadRecord{RequestedPath: "invalid"}, false) || invalid.ErrorKind != domain.ErrorInvalid {
		t.Fatalf("malformed stored destination: %+v", invalid)
	}
}

func TestUploadAdmissionInvalidItemsAndProviderAttestations(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x17881006)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("admission-invalid", 5)
	requests[0].Path = domain.MustParseUserPath("/")
	requests[1].Size = -1
	requests[2].MediaType = "invalid"
	requests[3].Conflict = domain.ConflictMode("invalid")
	f.stateLedger.Reset()
	f.fileLedger.Reset()
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests[:4])
	if err != nil || len(f.stateLedger.Events()) != 0 || len(f.fileLedger.Events()) != 0 {
		t.Fatalf("invalid metadata did provider work: %+v %v", page, err)
	}
	for _, item := range page {
		if item.ErrorKind != domain.ErrorInvalid {
			t.Fatalf("invalid metadata: %+v", item)
		}
	}
	duplicate := scaleUploadRequestsCount("admission-duplicate-path", 2)
	duplicate[1].Path = duplicate[0].Path
	page, err = f.engine.Files().AdmitUploads(ctx, f.scope, duplicate)
	if err != nil || page[0].Capability == nil || page[1].ErrorKind != domain.ErrorConflict {
		t.Fatalf("duplicate destination poisoned valid item: %+v %v", page, err)
	}
	base, _ := f.engine.Files().transferBackend()
	control := f.engine.fileBackend
	f.engine.fileBackend = &lifecycleProbe{FileControlBackend: control, DirectTransferBackend: base, progress: func(context.Context, []byte) (objectstore.UploadProgress, error) {
		return objectstore.UploadProgress{Offset: 1}, nil
	}}
	page, err = f.engine.Files().AdmitUploads(ctx, f.scope, duplicate[:1])
	if err != nil || page[0].ErrorKind != domain.ErrorInvalid {
		t.Fatalf("invalid native progress: %+v %v", page, err)
	}
	f.engine.fileBackend = &lifecycleProbe{FileControlBackend: control, DirectTransferBackend: base, resume: func(ctx context.Context, lease []byte) (objectstore.UploadCapability, error) {
		cap, err := base.ResumeUpload(ctx, lease)
		cap.ExpiresAt = cap.ExpiresAt.Add(time.Hour)
		return cap, err
	}}
	page, err = f.engine.Files().AdmitUploads(ctx, f.scope, duplicate[:1])
	if err != nil || page[0].ErrorKind != domain.ErrorInvalid {
		t.Fatalf("renewed native deadline: %+v %v", page, err)
	}
	f.engine.fileBackend = lifecycleControlOnly{FileControlBackend: control}
	page, err = f.engine.Files().AdmitUploads(ctx, f.scope, requests[4:])
	if err != nil || page[0].Capability != nil || page[0].ErrorKind == "" {
		t.Fatalf("unsupported native control: %+v %v", page, err)
	}
}

func TestUploadAdmissionFailsClosedBeforeNativeWork(t *testing.T) {
	for _, mode := range []string{"entropy", "registration", "intent-publication"} {
		t.Run(mode, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x17884901)
			scope := f.scope
			if mode == "entropy" {
				f.engine.ids = domain.NewIDGenerator(bytes.NewReader(nil))
			} else {
				if mode == "registration" {
					owner, err := domain.ParseUserID("YWRtaXNzaW9uLXVucmVnaXN0ZXJlZC1vd25lcg")
					if err != nil {
						t.Fatal(err)
					}
					scope, err = domain.NewScope(owner, domain.AreaLive)
					if err != nil {
						t.Fatal(err)
					}
				}
				base := f.engine.backend
				f.engine.backend = &admissionBackendProbe{Backend: base, put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
					return "", domain.ErrUnauthorized
				}}
			}
			page, err := f.engine.Files().AdmitUploads(context.Background(), scope, scaleUploadRequestsCount("admission-early-failure", 2))
			if err != nil || len(page) != 2 || len(f.fileLedger.Events()) != 0 {
				t.Fatalf("early failure contacted native provider: %+v %v", page, err)
			}
			for _, item := range page {
				if item.Capability != nil || item.ErrorKind == "" {
					t.Fatalf("early failure exposed capability: %+v", item)
				}
			}
		})
	}
}

func TestUploadAdmissionLegacyReplayAndWaitingCancellation(t *testing.T) {
	t.Run("legacy member recovery retains binding", func(t *testing.T) {
		f := newUploadBatchScaleFixture(t, 0x17884902)
		requests := scaleUploadRequestsCount("admission-legacy-replay", 2)
		caps, err := f.engine.Files().CreateUploadBatch(context.Background(), f.scope, requests)
		if err != nil {
			t.Fatal(err)
		}
		f.fileLedger.Reset()
		page, err := f.engine.Files().AdmitUploads(context.Background(), f.scope, requests[1:])
		if err != nil || page[0].Capability == nil || page[0].UploadID != caps[1].UploadID || page[0].Capability.BatchID != caps[1].BatchID || page[0].Capability.BatchIndex != 1 || page[0].Capability.BatchCount != 2 {
			t.Fatalf("legacy binding changed: %+v %v", page, err)
		}
		for _, event := range f.fileLedger.Events() {
			if string(event.Kind) == "upload_begin" {
				t.Fatal("legacy replay recreated native session")
			}
		}
	})
	t.Run("cancel while all native slots are occupied", func(t *testing.T) {
		f := newUploadBatchScaleFixture(t, 0x17884903)
		for range cap(f.engine.uploadLifecycleSlots) {
			f.engine.uploadLifecycleSlots <- struct{}{}
		}
		defer func() {
			for len(f.engine.uploadLifecycleSlots) > 0 {
				<-f.engine.uploadLifecycleSlots
			}
		}()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
			if step == StepUploadAdmissionAfterIntents {
				cancel()
			}
			return nil
		})
		page, err := f.engine.Files().AdmitUploads(ctx, f.scope, scaleUploadRequestsCount("admission-wait-cancel", 3))
		if err == nil || page != nil || len(f.fileLedger.Events()) != 0 {
			t.Fatalf("cancelled queued work escaped: %+v %v", page, err)
		}
	})
}

func TestUploadAdmissionPublicationFailuresWithholdCapabilities(t *testing.T) {
	for _, mode := range []string{"intent-contention", "activation-read", "activation-denied"} {
		t.Run(mode, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x17884904)
			base := f.engine.backend
			var afterLeases atomic.Bool
			f.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
				if step == StepUploadAdmissionAfterLeases {
					afterLeases.Store(true)
				}
				return nil
			})
			f.engine.backend = &admissionBackendProbe{Backend: base, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
				if afterLeases.Load() && mode == "activation-read" && strings.HasSuffix(key.String(), "/head.json") {
					return objectstore.Object{}, domain.ErrUnavailable
				}
				return base.Get(ctx, key)
			}, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
				if strings.HasSuffix(key.String(), "/head.json") {
					if mode == "intent-contention" {
						return "", domain.ErrPreconditionFailed
					}
					if mode == "activation-denied" && afterLeases.Load() {
						return "", domain.ErrUnauthorized
					}
				}
				return base.Put(ctx, key, body, condition)
			}}
			requests := scaleUploadRequestsCount("admission-publication-denial", 2)
			page, err := f.engine.Files().AdmitUploads(context.Background(), f.scope, requests)
			if err != nil || len(page) != 2 {
				t.Fatalf("failure envelope: %+v %v", page, err)
			}
			for _, item := range page {
				if item.Capability != nil || item.ErrorKind == "" {
					t.Fatalf("publication failure exposed capability: %+v", item)
				}
			}
			if mode == "intent-contention" && len(f.fileLedger.Events()) != 0 {
				t.Fatal("contended intent started native work")
			}
			f.engine.backend = base
			f.engine.scheduler = nil
			f.fileLedger.Reset()
			replay, err := newAdmissionReplica(t, f, 0x17884905).Files().AdmitUploads(context.Background(), f.scope, requests)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range replay {
				if item.Capability == nil {
					t.Fatalf("publication retry did not recover: %+v", item)
				}
			}
			if mode != "intent-contention" {
				for _, event := range f.fileLedger.Events() {
					if string(event.Kind) == "upload_begin" {
						t.Fatal("activation retry recreated durable session")
					}
				}
			}
		})
	}
}

func TestUploadAdmissionLostLeaseWriteAndReadResponsesDoNotRevokeSession(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x17881007)
	requests := scaleUploadRequestsCount("admission-uncertain-lease", 1)
	ctx := context.Background()
	base, _ := f.engine.Files().transferBackend()
	var begins atomic.Int64
	f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: base, begin: func(ctx context.Context, r objectstore.UploadRequest) (objectstore.UploadHandle, error) {
		begins.Add(1)
		return base.BeginUpload(ctx, r)
	}}
	f.engine.backend = &admissionLostLeaseWrite{Backend: f.engine.backend, failRead: true}
	first, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || first[0].Outcome != "unknown" || first[0].Capability != nil {
		t.Fatalf("uncertain durable write: %+v %v", first, err)
	}
	replay, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || replay[0].Capability == nil || begins.Load() != 1 {
		t.Fatalf("uncertain native winner revoked: %+v %v begins=%d", replay, err, begins.Load())
	}
}

func TestUploadAdmissionCancellationAndQueuedExpiryJoinProviderWork(t *testing.T) {
	t.Run("two pages share engine budget and cancellation joins", func(t *testing.T) {
		f := newUploadBatchScaleFixture(t, 0x17881008)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		base, _ := f.engine.Files().transferBackend()
		var active, peak atomic.Int64
		entered := make(chan struct{}, 100)
		f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: base, begin: func(ctx context.Context, _ objectstore.UploadRequest) (objectstore.UploadHandle, error) {
			current := active.Add(1)
			for p := peak.Load(); current > p && !peak.CompareAndSwap(p, current); p = peak.Load() {
			}
			entered <- struct{}{}
			<-ctx.Done()
			active.Add(-1)
			return objectstore.UploadHandle{}, ctx.Err()
		}}
		done := make(chan struct{}, 2)
		for _, name := range []string{"admission-budget-a", "admission-budget-b"} {
			go func(name string) {
				defer func() { done <- struct{}{} }()
				_, _ = f.engine.Files().AdmitUploads(ctx, f.scope, scaleUploadRequestsCount(name, 100))
			}(name)
		}
		for i := 0; i < 100; i++ {
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider budget did not fill")
			}
		}
		cancel()
		for i := 0; i < 2; i++ {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("admission returned with detached work")
			}
		}
		if peak.Load() != 100 || active.Load() != 0 {
			t.Fatalf("unbounded/detached native work: peak=%d active=%d", peak.Load(), active.Load())
		}
	})
	t.Run("expiry after waiting cannot start provider session", func(t *testing.T) {
		f := newUploadBatchScaleFixture(t, 0x17881009)
		ctx := context.Background()
		base, _ := f.engine.Files().transferBackend()
		var begins atomic.Int64
		ready := make(chan struct{})
		var once sync.Once
		f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: base, begin: func(ctx context.Context, r objectstore.UploadRequest) (objectstore.UploadHandle, error) {
			begins.Add(1)
			return base.BeginUpload(ctx, r)
		}}
		f.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
			if step == StepUploadAdmissionAfterIntents {
				once.Do(func() { close(ready) })
			}
			return nil
		})
		for i := 0; i < 100; i++ {
			f.engine.uploadLifecycleSlots <- struct{}{}
		}
		done := make(chan []domain.UploadAdmissionResult, 1)
		go func() {
			page, _ := f.engine.Files().AdmitUploads(ctx, f.scope, scaleUploadRequestsCount("admission-queued-expiry", 1))
			done <- page
		}()
		<-ready
		f.engine.clock.(*domain.FixedClock).Advance(25 * time.Hour)
		for i := 0; i < 100; i++ {
			<-f.engine.uploadLifecycleSlots
		}
		page := <-done
		if len(page) != 1 || page[0].Outcome != "expired" || page[0].Capability != nil || begins.Load() != 0 {
			t.Fatalf("queued expiry renewed authority: %+v begins=%d", page, begins.Load())
		}
	})
}

func TestUploadAdmissionTwoThroughEightReplicasConverge(t *testing.T) {
	for replicas := 2; replicas <= 8; replicas++ {
		t.Run(fmt.Sprint(replicas), func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x17882000+uint64(replicas))
			ctx := context.Background()
			requests := scaleUploadRequestsCount("admission-replicas", 3)
			engines := make([]*Engine, replicas)
			for i := range engines {
				engine, err := Open(ctx, Options{Backend: f.engine.backend, FileBackend: f.engine.fileBackend, Clock: f.engine.clock, IDs: domain.NewIDGenerator(&deterministicScaleReader{state: uint64(900 + i)}), Writer: WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}}, LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32)})
				if err != nil {
					t.Fatal(err)
				}
				engines[i] = engine
			}
			start := make(chan struct{})
			pages := make(chan []domain.UploadAdmissionResult, replicas)
			errs := make(chan error, replicas)
			for _, engine := range engines {
				go func(engine *Engine) {
					<-start
					page, err := engine.Files().AdmitUploads(ctx, f.scope, requests)
					pages <- page
					errs <- err
				}(engine)
			}
			close(start)
			var expected []domain.UploadID
			for i := 0; i < replicas; i++ {
				page := <-pages
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
				if len(page) != 3 {
					t.Fatalf("page=%+v", page)
				}
				if expected == nil {
					expected = []domain.UploadID{page[0].UploadID, page[1].UploadID, page[2].UploadID}
				}
				for j, item := range page {
					if item.Capability == nil || item.UploadID != expected[j] {
						t.Fatalf("replicas diverged: %+v expected=%v", page, expected)
					}
				}
			}
			f.fileLedger.Reset()
			page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests[1:2])
			if err != nil || page[0].UploadID != expected[1] {
				t.Fatalf("settled replay: %+v %v", page, err)
			}
			for _, event := range f.fileLedger.Events() {
				if string(event.Kind) == "upload_begin" {
					t.Fatal("accepted replica receipt recreated a session")
				}
			}
		})
	}
}

func newAdmissionReplica(t *testing.T, f uploadBatchScaleFixture, seed uint64) *Engine {
	t.Helper()
	engine, err := Open(context.Background(), Options{Backend: f.engine.backend, FileBackend: f.engine.fileBackend, Clock: f.engine.clock, IDs: domain.NewIDGenerator(&deterministicScaleReader{state: seed}), Writer: WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}}, LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

type admissionBackendProbe struct {
	objectstore.Backend
	get func(context.Context, objectstore.Key) (objectstore.Object, error)
	put func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error)
}

func (b *admissionBackendProbe) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if b.get != nil {
		return b.get(ctx, key)
	}
	return b.Backend.Get(ctx, key)
}
func (b *admissionBackendProbe) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if b.put != nil {
		return b.put(ctx, key, body, condition)
	}
	return b.Backend.Put(ctx, key, body, condition)
}

func TestUploadAdmissionUncertainMetadataAndDefiniteLeaseFailures(t *testing.T) {
	t.Run("unavailable initial shared view preserves invalid-item outcome", func(t *testing.T) {
		f := newUploadBatchScaleFixture(t, 0x17883001)
		base := f.engine.backend
		var blocked atomic.Bool
		blocked.Store(true)
		f.engine.backend = &admissionBackendProbe{Backend: base, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
			if blocked.Load() {
				return objectstore.Object{}, domain.ErrUnavailable
			}
			return base.Get(ctx, key)
		}}
		requests := scaleUploadRequestsCount("admission-read-failure", 2)
		requests[1].Size = -1
		page, err := f.engine.Files().AdmitUploads(context.Background(), f.scope, requests)
		if err != nil || page[0].Outcome != "unknown" || page[0].Capability != nil || page[1].ErrorKind != domain.ErrorInvalid || len(f.fileLedger.Events()) != 0 {
			t.Fatalf("shared read failure: %+v %v", page, err)
		}
		blocked.Store(false)
		page, err = f.engine.Files().AdmitUploads(context.Background(), f.scope, requests[:1])
		if err != nil || page[0].Capability == nil {
			t.Fatalf("shared read retry: %+v %v", page, err)
		}
	})
	t.Run("definite rejected lease write can retry same intent", func(t *testing.T) {
		f := newUploadBatchScaleFixture(t, 0x17883002)
		base := f.engine.backend
		var rejected atomic.Bool
		f.engine.backend = &admissionBackendProbe{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if objectstoreKeyIsUploadLease(key) && rejected.CompareAndSwap(false, true) {
				return "", domain.ErrUnavailable
			}
			return base.Put(ctx, key, body, condition)
		}}
		requests := scaleUploadRequestsCount("admission-lease-rejected", 1)
		first, err := f.engine.Files().AdmitUploads(context.Background(), f.scope, requests)
		if err != nil || first[0].Outcome != "unknown" || first[0].Capability != nil {
			t.Fatalf("rejected lease write: %+v %v", first, err)
		}
		replay, err := newAdmissionReplica(t, f, 0x17883999).Files().AdmitUploads(context.Background(), f.scope, requests)
		if err != nil || replay[0].Capability == nil || replay[0].UploadID != first[0].UploadID {
			t.Fatalf("unaccepted intent retry: %+v %v", replay, err)
		}
	})
	t.Run("activation contention preserves durable leases", func(t *testing.T) {
		f := newUploadBatchScaleFixture(t, 0x17883003)
		base := f.engine.backend
		var blocked atomic.Bool
		f.engine.backend = &admissionBackendProbe{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if blocked.Load() && strings.HasSuffix(key.String(), "/head.json") {
				return "", domain.ErrPreconditionFailed
			}
			return base.Put(ctx, key, body, condition)
		}}
		f.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
			if step == StepUploadAdmissionAfterLeases {
				blocked.Store(true)
			}
			return nil
		})
		requests := scaleUploadRequestsCount("admission-activation-contention", 3)
		page, err := f.engine.Files().AdmitUploads(context.Background(), f.scope, requests)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page {
			if item.Outcome != "unknown" || item.Capability != nil {
				t.Fatalf("contended activation exposed lease: %+v", item)
			}
		}
		blocked.Store(false)
		f.engine.scheduler = nil
		f.fileLedger.Reset()
		replay, err := newAdmissionReplica(t, f, 0x17883998).Files().AdmitUploads(context.Background(), f.scope, requests)
		if err != nil {
			t.Fatal(err)
		}
		for i, item := range replay {
			if item.Capability == nil || item.UploadID != page[i].UploadID {
				t.Fatalf("activation replay: %+v", replay)
			}
		}
		for _, event := range f.fileLedger.Events() {
			if string(event.Kind) == "upload_begin" {
				t.Fatal("durable lease recreated after contended activation")
			}
		}
	})
	t.Run("cancellation between native work and activation wins", func(t *testing.T) {
		f := newUploadBatchScaleFixture(t, 0x17883004)
		ctx := context.Background()
		requests := scaleUploadRequestsCount("admission-cancel-boundary", 2)
		var once sync.Once
		f.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
			if step == StepUploadAdmissionAfterLeases {
				once.Do(func() {
					record, found, err := f.engine.Files().portableUploadByIdempotency(ctx, f.scope.UserID(), requests[0].IdempotencyKey, namespaceRequestFingerprint("upload", "live", requests[0].Path.String(), "0", "application/octet-stream", "fail", "", "true"))
					if err != nil || !found {
						t.Fatalf("intent=%+v %v", record, err)
					}
					outcome, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{{UploadID: domain.UploadID(record.UploadID), Action: "cancel"}})
					if err != nil || outcome[0].Outcome != "applied" {
						t.Fatalf("cancel=%+v %v", outcome, err)
					}
				})
			}
			return nil
		})
		page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
		if err != nil || page[0].Outcome != "aborted" || page[0].Capability != nil || page[1].Capability == nil {
			t.Fatalf("cancelled item resurrected: %+v %v", page, err)
		}
	})
}
