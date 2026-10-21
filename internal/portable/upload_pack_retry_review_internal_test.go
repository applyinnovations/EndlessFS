package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

// This is an ordinary, small pack, not the bounded overflow path. Losing the
// successful pack PUT must be recoverable even when the next attempt's clock
// differs; no head has yet made the first attempt's outcome authoritative.
func TestMaterializedPackRetryReviewAdvancingDefaultRetention(t *testing.T) {
	for _, advance := range []time.Duration{0, time.Millisecond} {
		t.Run(advance.String(), func(t *testing.T) {
			ctx := context.Background()
			clock := domain.NewFixedClock(time.Date(2074, 3, 4, 5, 6, 7, 0, time.UTC))
			base := objectmemory.New()
			backend := &admissionBackendProbe{Backend: base}
			store := newConsistencyDomainStore(backend, nil, clock)
			reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "ordinary-pack-retention-retry"}
			if err := store.ensureRegistered(ctx, reference); err != nil {
				t.Fatal(err)
			}
			mutation := consistencyDomainMutation{ID: "stable-logical-mutation", Result: []byte("committed"), Changes: []consistencyDomainChange{{Key: "stable-record", Require: domainValueAbsent, Value: []byte("stable-value")}}}
			initial, session := materializedDomainView(t, store, reference, mutation.ID)
			var firstKey objectstore.Key
			var firstBody []byte
			var attempts int
			backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
				isPack := strings.Contains(key.String(), "/packs/")
				if isPack {
					attempts++
					if attempts == 1 {
						firstKey, firstBody = key, bytes.Clone(body)
					} else {
						t.Logf("ordinary retry advance=%s samePackID=%t sameWire=%t", advance, key == firstKey, bytes.Equal(body, firstBody))
					}
				}
				version, err := base.Put(ctx, key, body, condition)
				if isPack && attempts == 1 && err == nil {
					return version, domain.ErrUnavailable
				}
				return version, err
			}
			if _, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &initial, session); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("lost durable pack response = %v", err)
			}
			if attempts != 1 || len(firstBody) == 0 {
				t.Fatal("did not lose an actual ordinary pack PUT")
			}
			head, err := store.loadHead(ctx, reference)
			if err != nil || head.object.Version != initial.object.Version || head.head.Revision != initial.head.Revision {
				t.Fatalf("pack response loss published a head: %+v, %v", head.head, err)
			}
			if _, err := store.get(ctx, reference, "stable-record"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("unpublished mutation became visible: %v", err)
			}
			clock.Advance(advance)
			reopened := newConsistencyDomainStore(backend, nil, clock)
			snapshot, retrySession := materializedDomainView(t, reopened, reference, mutation.ID)
			outcome, err := reopened.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, retrySession)
			if err != nil {
				t.Fatalf("ordinary retry could not recover durable pack PUT; identityCollision=%t: %v", errors.Is(err, errDomainPagePackIdentityCollision), err)
			}
			if outcome.Revision != initial.head.Revision+1 {
				t.Fatalf("retry revision = %d", outcome.Revision)
			}
			if value, err := reopened.get(ctx, reference, "stable-record"); err != nil || string(value.Data) != "stable-value" {
				t.Fatalf("reopened value = %+v, %v", value, err)
			}
			snapshot, retrySession = materializedDomainView(t, reopened, reference, mutation.ID)
			if replay, err := reopened.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, retrySession); err != nil || !replay.Replayed || replay.Revision != outcome.Revision {
				t.Fatalf("exact outcome replay = %+v, %v", replay, err)
			}
		})
	}
}

func TestUploadAdmissionRetryReviewActivationLostPackResponseWithAdvancingClock(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x1788a901)
	ctx := context.Background()
	clock := f.engine.clock.(*domain.FixedClock)
	requests := scaleUploadRequestsCount("activation-retention-retry", 1)
	requests[0].Size = 4
	transfer, _ := f.engine.Files().transferBackend()
	var starts atomic.Int64
	f.engine.fileBackend = &admissionProbe{FileControlBackend: f.engine.fileBackend, DirectTransferBackend: transfer, begin: func(ctx context.Context, request objectstore.UploadRequest) (objectstore.UploadHandle, error) {
		starts.Add(1)
		return transfer.BeginUpload(ctx, request)
	}}
	base := f.engine.backend
	backend := &admissionBackendProbe{Backend: base}
	f.engine.backend = backend
	var armed, lost bool
	var activationHead objectstore.Object
	var firstKey objectstore.Key
	var firstBody []byte
	var activationPackAttempts int
	f.engine.scheduler = SchedulerFunc(func(ctx context.Context, step string) error {
		if step == StepUploadAdmissionAfterLeases {
			var err error
			activationHead, err = base.Get(ctx, storageformat.DomainHeadKey(uploadDomainReference(f.scope.UserID()).Kind, uploadDomainReference(f.scope.UserID()).ID))
			if err != nil {
				return err
			}
			armed = true
		}
		return nil
	})
	backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
		activationPack := armed && strings.Contains(key.String(), "/packs/")
		if activationPack {
			activationPackAttempts++
			if activationPackAttempts == 1 {
				firstKey, firstBody = key, bytes.Clone(body)
			} else {
				t.Logf("activation retry samePackID=%t sameWire=%t", key == firstKey, bytes.Equal(body, firstBody))
			}
		}
		version, err := base.Put(ctx, key, body, condition)
		if activationPack && !lost && err == nil {
			current, readErr := base.Get(ctx, storageformat.DomainHeadKey(uploadDomainReference(f.scope.UserID()).Kind, uploadDomainReference(f.scope.UserID()).ID))
			if readErr != nil || current.Version != activationHead.Version || !bytes.Equal(current.Body, activationHead.Body) {
				t.Fatalf("activation pack became visible before head CAS: %v", readErr)
			}
			lost = true
			clock.Advance(time.Millisecond)
			return version, domain.ErrUnavailable
		}
		return version, err
	}
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if !lost || activationPackAttempts < 2 {
		t.Fatalf("actual durable activation-pack loss/retry not exercised: lost=%t attempts=%d page=%+v err=%v", lost, activationPackAttempts, page, err)
	}
	if err != nil || len(page) != 1 || page[0].Capability == nil || page[0].Outcome != "accepted" {
		t.Fatalf("public activation cannot recover advanced-clock pack response loss: page=%+v err=%v", page, err)
	}
	f.engine.scheduler = nil
	replica := newAdmissionReplica(t, f, 0x1788a902)
	replay, err := replica.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(replay) != 1 || replay[0].Capability == nil || replay[0].UploadID != page[0].UploadID || starts.Load() != 1 {
		t.Fatalf("independent activation replay recreated session: page=%+v starts=%d err=%v", replay, starts.Load(), err)
	}
	payload := []byte("four")
	admissionWriteBytes(t, f, *replay[0].Capability, payload, 0)
	completed, err := replica.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{{UploadID: replay[0].UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor(payload).CRC32C}})
	if err != nil || len(completed) != 1 || completed[0].Outcome != "applied" || completed[0].Status == nil || completed[0].Status.State != domain.UploadStateCompleted {
		t.Fatalf("recovered capability could not finish real bytes: %+v %v", completed, err)
	}
}

func TestMaterializedPackRetryReviewPinsExpiryCutoffAcrossLostPut(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-preparation", true: "fresh-preparation"}[fresh], func(t *testing.T) {
			ctx := context.Background()
			clock := domain.NewFixedClock(time.Date(2074, 3, 5, 0, 0, 0, 0, time.UTC))
			base := objectmemory.New()
			backend := &admissionBackendProbe{Backend: base}
			store := newConsistencyDomainStore(backend, nil, clock)
			reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "expiry-cutoff-retry"}
			if err := store.ensureRegistered(ctx, reference); err != nil {
				t.Fatal(err)
			}
			old := consistencyDomainMutation{ID: "about-to-expire", RetainUntil: clock.Now().Add(time.Millisecond), Changes: []consistencyDomainChange{{Key: "old", Require: domainValueAbsent, Value: []byte("old")}}}
			snapshot, session := materializedDomainView(t, store, reference, old.ID, old.RetainUntil)
			if _, err := store.mutateMaterializedPrepared(ctx, reference, old, &snapshot, session); err != nil {
				t.Fatal(err)
			}
			mutation := consistencyDomainMutation{ID: "cross-expiry", Changes: []consistencyDomainChange{{Key: "new", Require: domainValueAbsent, Value: []byte("new")}}}
			snapshot, session = materializedDomainView(t, store, reference, mutation.ID)
			var lost bool
			var firstKey objectstore.Key
			var firstBody []byte
			backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
				isPack := strings.Contains(key.String(), "/packs/")
				if isPack && lost {
					if fresh && key == firstKey || !fresh && (key != firstKey || !bytes.Equal(body, firstBody)) {
						t.Fatalf("cutoff crossed without coherent physical binding: fresh=%t samePack=%t sameWire=%t", fresh, key == firstKey, bytes.Equal(body, firstBody))
					}
				}
				version, err := base.Put(ctx, key, body, condition)
				if isPack && !lost && err == nil {
					firstKey, firstBody, lost = key, bytes.Clone(body), true
					clock.Advance(2 * time.Millisecond)
					return version, domain.ErrUnavailable
				}
				return version, err
			}
			if _, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session); !errors.Is(err, domain.ErrUnavailable) || !lost {
				t.Fatalf("lost pack boundary = %v lost=%t", err, lost)
			}
			if fresh {
				snapshot, session = materializedDomainView(t, store, reference, mutation.ID)
			}
			outcome, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session)
			if err != nil {
				t.Fatalf("cross-expiry retry = %v", err)
			}
			if !outcome.RetainUntil.Equal(session.preparedAt.Add(terminalOperationRetention)) {
				t.Fatalf("retention did not start at bound preparation: %s", outcome.RetainUntil)
			}
			head, err := store.loadHead(ctx, reference)
			if err != nil {
				t.Fatal(err)
			}
			reader := newConsistencyDomainTreeSession(store, reference)
			_, found, err := reader.lookup(ctx, head.head.Outcomes, old.ID)
			if err != nil || found == fresh {
				t.Fatalf("expiry pruning did not use captured cutoff: fresh=%t found=%t err=%v", fresh, found, err)
			}
			if _, found, err := store.lookupOutcomeAtHead(ctx, reference, head.head, old.ID); err != nil || found {
				t.Fatalf("retained physical page revived expired receipt: found=%t err=%v", found, err)
			}
		})
	}
}

func TestMaterializedPackRetryReviewExplicitDeadlineAndBindingDenials(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2074, 3, 6, 0, 0, 0, 0, time.UTC))
	base := objectmemory.New()
	store := newConsistencyDomainStore(base, nil, clock)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "explicit-retention-binding"}
	if err := store.ensureRegistered(ctx, reference); err != nil {
		t.Fatal(err)
	}
	mutation := consistencyDomainMutation{ID: "declared-deadline", RetainUntil: clock.Now().Add(2 * time.Hour), Changes: []consistencyDomainChange{{Key: "record", Require: domainValueAbsent, Value: []byte("value")}}}
	snapshot, session := materializedDomainView(t, store, reference, mutation.ID, mutation.RetainUntil)
	if err := session.bindPreparedMutation(session.preparationSeed, mutation.RetainUntil.In(time.FixedZone("offset", 3600))); err != nil {
		t.Fatalf("equal UTC deadline was not idempotent: %v", err)
	}
	if err := session.bindPreparedMutation(session.preparationSeed, time.Time{}); err != nil {
		t.Fatalf("repeated preparation changed its deadline: %v", err)
	}
	if err := session.bindPreparedMutation(session.preparationSeed, mutation.RetainUntil.Add(time.Second)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("late explicit deadline changed binding: %v", err)
	}
	if err := session.bindPreparedMutation("different-mutation", mutation.RetainUntil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("different mutation changed binding: %v", err)
	}
	clock.Advance(time.Minute)
	outcome, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session)
	if err != nil || !outcome.RetainUntil.Equal(mutation.RetainUntil) {
		t.Fatalf("explicit deadline changed during preparation: %+v %v", outcome, err)
	}
	for _, name := range []string{"unbound", "written", "logical-only", "expired", "empty", "late-override"} {
		t.Run(name, func(t *testing.T) {
			snapshot, err := store.loadHead(ctx, reference)
			if err != nil {
				t.Fatal(err)
			}
			session := newConsistencyDomainTreeSession(store, reference)
			session.enablePackedWrites("denial-source")
			switch name {
			case "unbound":
				_, err = store.mutateMaterializedPrepared(ctx, reference, consistencyDomainMutation{ID: "unbound", Changes: []consistencyDomainChange{{Key: "denied", Require: domainValueAbsent, Value: []byte("no")}}}, &snapshot, session)
			case "written":
				_, err = session.writePage(ctx, storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: []storageformat.DomainEntry{{Key: "prepared", Value: []byte("value"), LogicalVersion: "version"}}})
				if err != nil {
					t.Fatal(err)
				}
				err = session.bindPreparedMutation("too-late", time.Time{})
			case "logical-only":
				if err := session.bindPackedMutation("existing-logical-binding"); err != nil {
					t.Fatal(err)
				}
				err = session.bindPreparedMutation("too-late", time.Time{})
			case "expired":
				err = session.bindPreparedMutation("expired", clock.Now())
				if !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("expired explicit deadline = %v", err)
				}
				return
			case "empty":
				err = session.bindPreparedMutation("", time.Time{})
			case "late-override":
				if err := session.bindPreparedMutation("default", time.Time{}); err != nil {
					t.Fatal(err)
				}
				_, err = store.mutateMaterializedPrepared(ctx, reference, consistencyDomainMutation{ID: "override", RetainUntil: clock.Now().Add(time.Hour), Changes: []consistencyDomainChange{{Key: "denied", Require: domainValueAbsent, Value: []byte("no")}}}, &snapshot, session)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("inconsistent preparation binding = %v", err)
			}
			if _, err := store.get(ctx, reference, "denied"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("denied preparation published a value: %v", err)
			}
		})
	}
}

func TestMaterializedPackRetryReviewStalePreparationPreservesOpaqueReferences(t *testing.T) {
	for _, stage := range []string{"before-mutation", "before-head"} {
		t.Run(stage, func(t *testing.T) {
			writer := &planningDiagnosticWriter{records: make(chan map[string]any, 64)}
			observer := telemetry.New(nil, slog.New(slog.NewJSONHandler(writer, nil)))
			defer observer.Close()
			ctx := telemetry.Context(context.Background(), observer)
			clock := domain.NewFixedClock(time.Date(2074, 3, 7, 0, 0, 0, 0, time.UTC))
			base := objectmemory.New()
			backend := &reviewRepackFaultBackend{Backend: base}
			store := newConsistencyDomainStore(backend, nil, clock)
			reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "stale-preparation"}
			backend.head = storageformat.DomainHeadKey(reference.Kind, reference.ID)
			if err := store.ensureRegistered(ctx, reference); err != nil {
				t.Fatal(err)
			}
			stageChild := func() (consistencyDomainHeadSnapshot, *consistencyDomainTreeSession, consistencyDomainMutation, storageformat.DomainTreeRoot) {
				snapshot, session := materializedDomainView(t, store, reference, "opaque-owner")
				child, err := session.writePage(ctx, storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: []storageformat.DomainEntry{{Key: "child", Value: []byte("opaque-child-value"), LogicalVersion: "child-version"}}})
				if err != nil {
					t.Fatal(err)
				}
				value, err := storageformat.EncodeCanonical(child.root)
				if err != nil {
					t.Fatal(err)
				}
				return snapshot, session, consistencyDomainMutation{ID: "opaque-owner", Changes: []consistencyDomainChange{{Key: "opaque", Require: domainValueAbsent, Value: value}}}, child.root
			}
			snapshot, session, mutation, oldChild := stageChild()
			backend.packPuts, backend.headPuts = 0, 0
			if stage == "before-mutation" {
				clock.Advance(terminalOperationRetention)
			} else {
				store.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
					if step == StepDomainBeforeHeadCommit {
						clock.Advance(terminalOperationRetention)
					}
					return nil
				})
			}
			if _, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("expired preparation published: %v", err)
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
		diagnostic:
			for {
				select {
				case record := <-writer.records:
					if record["operation"] != "domain.commit" {
						continue
					}
					if record["stage"] != "snapshot_check" || record["reason"] != "snapshot_stale" || strings.Contains(fmt.Sprint(record), reference.ID) {
						t.Fatalf("stale preparation diagnostic = %v", record)
					}
					break diagnostic
				case <-deadline.C:
					t.Fatal("stale preparation diagnostic missing")
				}
			}
			wantPacks := map[string]int{"before-mutation": 0, "before-head": 1}[stage]
			if backend.headPuts != 0 || backend.packPuts != wantPacks {
				t.Fatalf("stale publication boundary head=%d pack=%d", backend.headPuts, backend.packPuts)
			}
			if _, err := store.get(ctx, reference, "opaque"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("stale opaque reference became authoritative: %v", err)
			}
			store.scheduler = nil
			snapshot, session, mutation, child := stageChild()
			if child.PackID == oldChild.PackID {
				t.Fatal("rebuilt preparation reused expired physical identity")
			}
			if _, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session); err != nil {
				t.Fatalf("fresh rebuild failed: %v", err)
			}
			reopened := newConsistencyDomainStore(base, nil, clock)
			value, err := reopened.get(ctx, reference, "opaque")
			var actual storageformat.DomainTreeRoot
			if err != nil || decodeCanonicalValue(value.Data, &actual) != nil || actual != child {
				t.Fatalf("opaque child descriptor changed: %+v %v", actual, err)
			}
			reader := newConsistencyDomainTreeSession(reopened, reference)
			got, found, err := reader.lookup(ctx, actual, "child")
			if err != nil || !found || string(got.Data) != "opaque-child-value" {
				t.Fatalf("independent reader lost exact child: found=%t value=%q err=%v", found, got.Data, err)
			}
		})
	}
}

func TestMaterializedPackRetryReviewCompactionCrossesExpiryAfterLostPut(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2074, 3, 8, 0, 0, 0, 0, time.UTC))
	base := objectmemory.New()
	backend := &admissionBackendProbe{Backend: base}
	store := newConsistencyDomainStore(backend, nil, clock)
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "compaction-expiry-retry"}
	for _, mutation := range []consistencyDomainMutation{
		{ID: "expires-during-retry", RetainUntil: clock.Now().Add(time.Millisecond), Changes: []consistencyDomainChange{{Key: "alpha", Require: domainValueAbsent, Value: []byte("one")}}},
		{ID: "remains-retained", RetainUntil: clock.Now().Add(time.Hour), Changes: []consistencyDomainChange{{Key: "beta", Require: domainValueAbsent, Value: []byte("two")}}},
	} {
		if _, err := store.mutate(ctx, reference, mutation); err != nil {
			t.Fatal(err)
		}
	}
	initial, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	var lost bool
	var firstKey objectstore.Key
	var firstBody []byte
	backend.put = func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
		isPack := strings.Contains(key.String(), "/packs/")
		if isPack && lost {
			t.Logf("compaction retry samePackID=%t sameWire=%t", key == firstKey, bytes.Equal(body, firstBody))
		}
		version, err := base.Put(ctx, key, body, condition)
		if isPack && !lost && err == nil {
			firstKey, firstBody, lost = key, bytes.Clone(body), true
			clock.Advance(2 * time.Millisecond)
			return version, domain.ErrUnavailable
		}
		return version, err
	}
	if err := store.compactSnapshot(ctx, reference, initial); !errors.Is(err, domain.ErrUnavailable) || !lost {
		t.Fatalf("lost actual compaction pack PUT = %v lost=%t", err, lost)
	}
	current, err := store.loadHead(ctx, reference)
	if err != nil || current.object.Version != initial.object.Version {
		t.Fatalf("lost pack response changed compaction head: %v", err)
	}
	if err := store.compact(ctx, reference); err != nil {
		t.Fatalf("fresh compaction retry failed; identityCollision=%t: %v", errors.Is(err, errDomainPagePackIdentityCollision), err)
	}
	current, err = store.loadHead(ctx, reference)
	if err != nil || current.head.Revision != initial.head.Revision || len(current.head.Deltas) != 0 {
		t.Fatalf("compaction changed logical revision or failed folding: %+v %v", current.head, err)
	}
	for key, want := range map[string]string{"alpha": "one", "beta": "two"} {
		if value, err := store.get(ctx, reference, key); err != nil || string(value.Data) != want {
			t.Fatalf("compaction lost %s: %+v %v", key, value, err)
		}
	}
	if _, found, err := store.lookupOutcomeAtHead(ctx, reference, current.head, "expires-during-retry"); err != nil || found {
		t.Fatalf("compaction revived expired outcome: %t %v", found, err)
	}
	if _, found, err := store.lookupOutcomeAtHead(ctx, reference, current.head, "remains-retained"); err != nil || !found {
		t.Fatalf("compaction lost live outcome: %t %v", found, err)
	}
}
