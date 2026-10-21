package portable

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Public operations must not let one operation's already verified decoded pages
// conceal loss of the durable pack before its later independent inventory read.
// It exercises the public create, verify and reopen operations.
func TestCheckpointPublicMidOperationPackLossFailsClosed(t *testing.T) {
	for _, operation := range []string{"verify", "open", "create"} {
		for _, damage := range []string{"missing", "corrupt"} {
			t.Run(operation+"/"+damage, func(t *testing.T) {
				ctx := context.Background()
				f, reference := checkpointMidReadTypedFixture(t)
				checkpointID := "mid-read-" + operation + "-" + damage
				if operation == "create" {
					// Arming before CloseWrites would target capability-drain
					// traversal, rather than the closure under review. Close first,
					// then still invoke the public CreateCheckpoint API.
					if err := f.engine.CloseWrites(ctx, checkpointID); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.engine.CreateCheckpoint(ctx, checkpointID); err != nil {
						t.Fatalf("successful real typed checkpoint prerequisite: %v", err)
					}
					if err := f.engine.VerifyCheckpoint(ctx, checkpointID); err != nil {
						t.Fatalf("valid public verify prerequisite: %v", err)
					}
				}
				backend := f.engine.backend
				snapshot, err := f.engine.stateDomainStore().loadHead(ctx, reference)
				if err != nil || !snapshot.head.Frozen || snapshot.head.Base.PackID == "" {
					t.Fatalf("closed typed fixture geometry: frozen=%t packed=%t err=%v", snapshot.head.Frozen, snapshot.head.Base.PackID != "", err)
				}
				nonroot := checkpointMidReadNonrootBasePage(t, f.engine.stateDomainStore(), reference, snapshot.head)
				key := storageformat.DomainPagePackKey(reference.Kind, reference.ID, nonroot.root.PackID)
				original, err := backend.Get(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := storageformat.DecodeDomainPagePack(withoutMetadataPackCache(ctx), original.Body, reference.ID, reference.Kind, nonroot.root.PackID); err != nil {
					t.Fatalf("fault target is not exact authenticated original authority: %v", err)
				}
				before := checkpointMidReadAuthority(t, backend, reference)
				rootKey := storageformat.CheckpointKey(checkpointID)
				var checkpointBefore objectstore.Object
				if operation == "create" {
					if _, err := backend.Get(ctx, rootKey); !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("new checkpoint root already exists: %v", err)
					}
				} else {
					checkpointBefore, err = backend.Get(ctx, rootKey)
					if err != nil {
						t.Fatal(err)
					}
				}
				hook := &checkpointMidReadLossBackend{
					Backend: backend, key: key, reference: reference,
					packID: nonroot.root.PackID, original: original,
					damage: damage,
				}
				f.engine.backend = hook
				f.stateLedger.Reset()
				f.fileLedger.Reset()
				var callErr error
				switch operation {
				case "verify":
					callErr = f.engine.VerifyCheckpoint(ctx, checkpointID)
				case "open":
					callErr = f.engine.OpenWrites(ctx, checkpointID)
				case "create":
					_, callErr = f.engine.CreateCheckpoint(ctx, checkpointID)
				}
				f.engine.backend = backend
				// Restore even if any following assertion fails. The helper never
				// retries a product operation or mutates authority besides this
				// exact synthetic provider object and its exact restoration.
				defer hook.restore(t, ctx)
				observation := hook.snapshot()
				if observation.faultError != nil {
					t.Fatalf("synthetic provider fault did not execute: %v", observation.faultError)
				}
				if !observation.fired || observation.gets < 2 || observation.laterReads < 1 || !observation.reachabilityRead {
					t.Fatalf("public operation did not establish returned-good-then-independent-reachability-read geometry: fired=%t gets=%d later=%d reachability=%t", observation.fired, observation.gets, observation.laterReads, observation.reachabilityRead)
				}
				want := domain.ErrInvalid
				if damage == "missing" {
					want = domain.ErrNotFound
				}
				if !errors.Is(callErr, want) {
					t.Fatalf("public operation accepted or misclassified mid-operation durable %s: %v", damage, callErr)
				}
				checkpointMidReadNoPublication(t, f, key, damage)
				checkpointMidReadSameAuthority(t, backend, reference, before)
				if operation == "create" {
					if _, err := backend.Get(ctx, rootKey); !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("failed public create published a checkpoint root: %v", err)
					}
				} else {
					checkpointAfter, err := backend.Get(ctx, rootKey)
					if err != nil || checkpointAfter.Version != checkpointBefore.Version || !bytes.Equal(checkpointAfter.Body, checkpointBefore.Body) {
						t.Fatalf("failed public operation changed checkpoint authority: %v", err)
					}
				}
				gate, err := f.engine.GateStatus(ctx)
				if err != nil || gate.Mode != storageformat.GateClosed || gate.CheckpointID != checkpointID {
					t.Fatalf("damaged checkpoint escaped closed gate: closed=%t err=%v", gate.Mode == storageformat.GateClosed, err)
				}
				t.Logf("mid-operation public=%s fault=%s first_verified=true fresh_target_gets=%d later_reachability_get=%t no_publication=true file_calls=0", operation, damage, observation.laterReads, observation.reachabilityRead)

				hook.restore(t, ctx)
				if operation == "create" {
					if _, err := f.engine.CreateCheckpoint(ctx, checkpointID); err != nil {
						t.Fatalf("exact restoration did not permit public create: %v", err)
					}
				}
				if err := f.engine.VerifyCheckpoint(ctx, checkpointID); err != nil {
					t.Fatalf("exact restoration did not permit fresh public verification: %v", err)
				}
				if err := f.engine.OpenWrites(ctx, checkpointID); err != nil {
					t.Fatalf("exact restoration did not permit public reopen: %v", err)
				}
				gate, err = f.engine.GateStatus(ctx)
				if err != nil || gate.Mode != storageformat.GateOpen || gate.CheckpointID != "" {
					t.Fatalf("valid exact restoration did not open the gate: open=%t err=%v", gate.Mode == storageformat.GateOpen, err)
				}
			})
		}
	}
}

// Exclude every immediate root pack: namespace-root lookup independently reads
// the Base root before typed validation and would make a root-pack fault an
// insufficient control for the later, independent reachability proof.
func checkpointMidReadNonrootBasePage(t *testing.T, store *consistencyDomainStore, reference consistencyDomainRef, head storageformat.DomainHead) domainPageRef {
	t.Helper()
	roots := map[string]bool{head.Base.PackID: true, head.Outcomes.PackID: true, head.OutcomeExpiry.PackID: true}
	session := newConsistencyDomainTreeSession(store, reference)
	pending := []domainPageRef{{root: head.Base}}
	visited := make(map[domainPageRef]bool)
	for len(pending) != 0 {
		ref := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[ref] {
			continue
		}
		visited[ref] = true
		page, err := session.readPage(withoutMetadataPackCache(context.Background()), ref)
		if err != nil {
			t.Fatal(err)
		}
		if ref.root.PackID != "" && !roots[ref.root.PackID] {
			return ref
		}
		for _, child := range page.Children {
			pending = append(pending, domainPageRef{
				root:     storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount},
				firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter,
			})
		}
	}
	t.Fatal("typed fixture has no authenticated Base child pack outside all immediate root packs")
	return domainPageRef{}
}

func checkpointMidReadTypedFixture(t *testing.T) (uploadBatchScaleFixture, consistencyDomainRef) {
	t.Helper()
	const count = 257
	ctx := context.Background()
	f := newUploadBatchScaleFixture(t, 0x2593c621)
	requests := scaleUploadRequestsCount("mid-operation-checkpoint", count)
	capabilities, err := f.engine.Files().CreateUploadBatch(ctx, f.scope, requests)
	if err != nil || len(capabilities) != count {
		t.Fatalf("real public upload fixture: capabilities=%d err=%v", len(capabilities), err)
	}
	ids := make([]domain.UploadID, count)
	for index, capability := range capabilities {
		ids[index] = capability.UploadID
	}
	if err := f.engine.Files().AbortUploadBatch(ctx, f.scope, domain.AbortUploadBatchRequest{UploadIDs: ids, IdempotencyKey: "mid-operation-terminal"}); err != nil {
		t.Fatal(err)
	}
	reference := uploadDomainReference(f.scope.UserID())
	if err := f.engine.stateDomainStore().compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.engine.stateDomainStore().loadHead(ctx, reference)
	if err != nil || len(snapshot.head.Deltas) != 0 || snapshot.head.Base.EntryCount != 2*count || snapshot.head.Base.EntryCount <= 2*domainPageMaximumItems {
		t.Fatalf("typed pagination fixture: count=%d deltas=%d err=%v", snapshot.head.Base.EntryCount, len(snapshot.head.Deltas), err)
	}
	reader := newConsistencyDomainTreeSession(f.engine.stateDomainStore(), reference)
	values, err := reader.collect(withoutMetadataPackCache(ctx), snapshot.head.Base, "", "", int(snapshot.head.Base.EntryCount)+1)
	if err != nil || uint64(len(values)) != snapshot.head.Base.EntryCount {
		t.Fatalf("independent typed fixture inventory: count=%d err=%v", len(values), err)
	}
	uploads, idempotency := 0, 0
	for _, value := range values {
		switch {
		case strings.HasPrefix(value.Key, "upload/"):
			record, err := decodePortableUploadRecord(value.Value, f.scope.UserID(), strings.TrimPrefix(value.Key, "upload/"))
			if err != nil || record.State != storageformat.UploadAborted || record.CleanupPending {
				t.Fatal("fixture lacks exact terminal real upload authority")
			}
			uploads++
		case strings.HasPrefix(value.Key, "upload-idempotency/"):
			var record storageformat.PortableUploadIdempotency
			if decodeCanonicalValue(value.Value, &record) != nil || storageformat.ValidatePortableUploadIdempotency(record) != nil || record.OwnerID != reference.ID {
				t.Fatal("fixture has malformed or misbound real idempotency authority")
			}
			idempotency++
		}
	}
	if uploads != count || idempotency != count {
		t.Fatalf("real typed fixture pairs: uploads=%d idempotency=%d", uploads, idempotency)
	}
	return f, reference
}

type checkpointMidReadObservation struct {
	fired            bool
	gets, laterReads int
	reachabilityRead bool
	faultError       error
}

type checkpointMidReadLossBackend struct {
	objectstore.Backend
	mu          sync.Mutex
	key         objectstore.Key
	reference   consistencyDomainRef
	packID      string
	original    objectstore.Object
	damage      string
	observation checkpointMidReadObservation
	replacement objectstore.NativeVersion
	restored    bool
}

func (hook *checkpointMidReadLossBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	value, err := hook.Backend.Get(ctx, key)
	if key != hook.key {
		return value, err
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	hook.observation.gets++
	if hook.observation.fired {
		hook.observation.laterReads++
		hook.observation.reachabilityRead = hook.observation.reachabilityRead || checkpointMidReadReachabilityFrame()
		return value, err
	}
	if err != nil {
		hook.observation.faultError = err
		return value, err
	}
	if value.Version != hook.original.Version || !bytes.Equal(value.Body, hook.original.Body) {
		hook.observation.faultError = domain.NewError(domain.ErrorInvalid, "synthetic fault target changed before first read")
		return objectstore.Object{}, hook.observation.faultError
	}
	if _, verifyErr := storageformat.DecodeDomainPagePack(withoutMetadataPackCache(ctx), value.Body, hook.reference.ID, hook.reference.Kind, hook.packID); verifyErr != nil {
		hook.observation.faultError = verifyErr
		return objectstore.Object{}, verifyErr
	}
	hook.observation.fired = true
	if hook.damage == "missing" {
		hook.observation.faultError = hook.Backend.Delete(ctx, key, objectstore.DeleteCondition{Version: value.Version})
	} else {
		hook.replacement, hook.observation.faultError = hook.Backend.Put(ctx, key, []byte("synthetic-mid-operation-corrupt-pack"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: value.Version})
	}
	if hook.observation.faultError != nil {
		return objectstore.Object{}, hook.observation.faultError
	}
	// Return the exact valid bytes fetched before the provider lost durability.
	return value, nil
}

func (hook *checkpointMidReadLossBackend) snapshot() checkpointMidReadObservation {
	hook.mu.Lock()
	defer hook.mu.Unlock()
	return hook.observation
}

func (hook *checkpointMidReadLossBackend) restore(t *testing.T, ctx context.Context) {
	t.Helper()
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.restored || !hook.observation.fired || hook.observation.faultError != nil {
		return
	}
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if hook.damage == "corrupt" {
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: hook.replacement}
	}
	if _, err := hook.Backend.Put(ctx, hook.key, hook.original.Body, condition); err != nil {
		t.Fatalf("restore exact synthetic durable pack: %v", err)
	}
	hook.restored = true
}

// Closed boolean attribution only; no function names, paths, stack bytes or
// dynamic labels are printed. Public denial is the assertion. This observation
// distinguishes the old semantic-stage re-read from the selected implementation's
// later fresh reachability read without adding production instrumentation.
func checkpointMidReadReachabilityFrame() bool {
	var callers [48]uintptr
	count := runtime.Callers(2, callers[:])
	frames := runtime.CallersFrames(callers[:count])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".collectConsistencyDomainCheckpointReachability") {
			return true
		}
		if !more {
			return false
		}
	}
}

func checkpointMidReadAuthority(t *testing.T, backend objectstore.Backend, reference consistencyDomainRef) map[objectstore.Key]objectstore.Object {
	t.Helper()
	result := make(map[objectstore.Key]objectstore.Object)
	for _, key := range []objectstore.Key{storageformat.WriteGateKey(), storageformat.DomainCatalogHeadKey(), storageformat.DomainHeadKey(reference.Kind, reference.ID)} {
		value, err := backend.Get(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		result[key] = value
	}
	return result
}

func checkpointMidReadSameAuthority(t *testing.T, backend objectstore.Backend, reference consistencyDomainRef, expected map[objectstore.Key]objectstore.Object) {
	t.Helper()
	actual := checkpointMidReadAuthority(t, backend, reference)
	for key, before := range expected {
		after := actual[key]
		if before.Version != after.Version || !bytes.Equal(before.Body, after.Body) {
			t.Fatal("denied public operation changed gate, catalog or domain authority")
		}
	}
}

func checkpointMidReadNoPublication(t *testing.T, f uploadBatchScaleFixture, key objectstore.Key, damage string) {
	t.Helper()
	faultWrites := 0
	for _, event := range f.stateLedger.Events() {
		switch event.Kind {
		case providerbudget.RequestObjectPut, providerbudget.RequestObjectDelete, providerbudget.RequestObjectCopy:
			expected := providerbudget.RequestObjectPut
			if damage == "missing" {
				expected = providerbudget.RequestObjectDelete
			}
			if event.Kind != expected || event.Target != key.String() || event.Failed {
				t.Fatal("denied operation performed a write beyond the one injected synthetic fault")
			}
			faultWrites++
		}
	}
	if faultWrites != 1 {
		t.Fatalf("injected provider mutations=%d want=1", faultWrites)
	}
	if len(f.fileLedger.Events()) != 0 {
		t.Fatal("mid-operation checkpoint denial performed file-provider work")
	}
}
