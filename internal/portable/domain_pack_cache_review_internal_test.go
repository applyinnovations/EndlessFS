package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type reviewCachedPack struct {
	reference consistencyDomainRef
	id        string
	key       objectstore.Key
	binding   consistencyDomainPackCacheKey
	root      domainPageRef
	body      []byte
	value     []byte
}

func reviewCanonicalCachedPack(t *testing.T, reference consistencyDomainRef, label string, value []byte) reviewCachedPack {
	t.Helper()
	id := storageformat.Digest([]byte(label))
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: []storageformat.DomainEntry{{Key: "item", LogicalVersion: "version", Value: append([]byte(nil), value...)}}}
	canonical, err := storageformat.EncodeCanonical(page)
	if err != nil {
		t.Fatal(err)
	}
	digest := storageformat.Digest(canonical)
	root, err := consistencyDomainPageDescriptor(page, digest, id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := storageformat.EncodeDomainPagePack(storageformat.DomainPagePack{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, PackID: id, Pages: []storageformat.DomainPackedPage{{Digest: digest, Page: page}}})
	if err != nil {
		t.Fatal(err)
	}
	key := storageformat.DomainPagePackKey(reference.Kind, reference.ID, id)
	return reviewCachedPack{reference: reference, id: id, key: key, binding: consistencyDomainPackCacheKey{key: key, domainID: reference.ID, kind: reference.Kind, packID: id}, root: root, body: body, value: append([]byte(nil), value...)}
}
func reviewCachedPackStore(backend objectstore.Backend, maximum int) *consistencyDomainStore {
	store := newConsistencyDomainStore(backend, nil)
	store.packReads = newConsistencyDomainPackReads(maximum)
	return store
}
func reviewReadCachedPack(ctx context.Context, store *consistencyDomainStore, pack reviewCachedPack) (storageformat.DomainPage, error) {
	session := newConsistencyDomainTreeSession(store, pack.reference)
	session.packKey = func(string) objectstore.Key { return pack.key }
	return session.readPage(ctx, pack.root)
}
func reviewAssertCachedPackValue(t *testing.T, page storageformat.DomainPage, pack reviewCachedPack, err error) {
	t.Helper()
	if err != nil || len(page.Entries) != 1 || !bytes.Equal(page.Entries[0].Value, pack.value) {
		t.Fatalf("verified immutable page differs: entries=%d err=%v", len(page.Entries), err)
	}
}

func TestDomainPackCacheReviewCopiesOnlyVerifiedWire(t *testing.T) {
	ctx := context.Background()
	pack := reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "copied-wire"}, "copied-wire", []byte("independent verified metadata"))
	shared := append([]byte(nil), pack.body...)
	var gets atomic.Int64
	backend := &admissionBackendProbe{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		gets.Add(1)
		return objectstore.Object{Key: pack.key, Body: shared}, nil
	}}
	store := reviewCachedPackStore(backend, len(pack.body))
	first, err := reviewReadCachedPack(ctx, store, pack)
	reviewAssertCachedPackValue(t, first, pack, err)
	// Neither a backend-owned buffer nor one request's decoded mutable slices
	// may be the source of a different request's immutable cache hit.
	shared[0] ^= 0xff
	first.Entries[0].Value[0] ^= 0xff
	second, err := reviewReadCachedPack(ctx, store, pack)
	reviewAssertCachedPackValue(t, second, pack, err)
	stats := store.packReads.snapshot()
	if gets.Load() != 1 || stats.Hits != 1 || stats.Entries != 1 || stats.Bytes != len(pack.body) {
		t.Fatalf("wire copy/hit accounting: gets=%d stats=%+v", gets.Load(), stats)
	}
	// Fully authenticated wire does not bypass the current descriptor's counts.
	bad := pack
	bad.root.root.EntryCount++
	if _, err := reviewReadCachedPack(ctx, store, bad); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cached descriptor bypassed: %v", err)
	}
	if gets.Load() != 1 {
		t.Fatal("descriptor denial unnecessarily fetched provider bytes")
	}
}

func TestDomainPackCacheReviewCorruptCachedWireFailsClosedThenRefetches(t *testing.T) {
	ctx := context.Background()
	pack := reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "cache-fault"}, "cache-fault", []byte("valid durable metadata"))
	var gets atomic.Int64
	backend := &admissionBackendProbe{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		gets.Add(1)
		return objectstore.Object{Key: pack.key, Body: append([]byte(nil), pack.body...)}, nil
	}}
	store := reviewCachedPackStore(backend, 1<<20)
	page, err := reviewReadCachedPack(ctx, store, pack)
	reviewAssertCachedPackValue(t, page, pack, err)
	store.packReads.mu.Lock()
	entry := store.packReads.cache[pack.binding].Value.(consistencyDomainPackCacheEntry)
	entry.body[0] ^= 0xff // Deliberate private cache-memory corruption.
	store.packReads.mu.Unlock()
	if _, err := reviewReadCachedPack(ctx, store, pack); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt cached wire did not fail closed: %v", err)
	}
	if gets.Load() != 1 || store.packReads.snapshot().Entries != 0 {
		t.Fatal("corrupt hit silently fell back or remained cached")
	}
	page, err = reviewReadCachedPack(ctx, store, pack)
	reviewAssertCachedPackValue(t, page, pack, err)
	if gets.Load() != 2 || store.packReads.snapshot().Entries != 1 {
		t.Fatal("healthy later request could not recover from evicted corrupt cache")
	}
}

func TestDomainPackCacheReviewLRUAccounting(t *testing.T) {
	// Pure LRU ownership tests intentionally use tiny opaque bodies; integration
	// tests above/below admit only complete strictly verified canonical packs.
	// Zero memos here exercise only LRU ownership; they are never decoded.
	ctx := context.Background()
	key := func(index int) consistencyDomainPackCacheKey {
		return consistencyDomainPackCacheKey{key: storageformat.DomainPagePackKey(storageformat.DomainNamespace, "lru-owner", storageformat.Digest([]byte(fmt.Sprint(index)))), domainID: "lru-owner", kind: storageformat.DomainNamespace, packID: storageformat.Digest([]byte(fmt.Sprint(index)))}
	}
	reads := newConsistencyDomainPackReads(4)
	reads.retainBody(ctx, key(1), []byte("aa"), storageformat.DomainPagePackWireProof{})
	reads.retainBody(ctx, key(2), []byte("bb"), storageformat.DomainPagePackWireProof{})
	if _, _, ok := reads.cachedBody(ctx, key(1)); !ok {
		t.Fatal("positive LRU hit absent")
	}
	reads.retainBody(ctx, key(3), []byte("cc"), storageformat.DomainPagePackWireProof{})
	if _, _, ok := reads.cachedBody(ctx, key(2)); ok {
		t.Fatal("least recently used body survived byte eviction")
	}
	for _, index := range []int{1, 3} {
		if _, _, ok := reads.cachedBody(ctx, key(index)); !ok {
			t.Fatal("recent cache body evicted")
		}
	}
	if stats := reads.snapshot(); stats.Bytes != 4 || stats.Entries != 2 || stats.Evictions != 1 {
		t.Fatalf("byte LRU accounting: %+v", stats)
	}
	reads.retainBody(ctx, key(1), []byte("a"), storageformat.DomainPagePackWireProof{})
	reads.retainBody(ctx, key(4), []byte("large"), storageformat.DomainPagePackWireProof{})
	if stats := reads.snapshot(); stats.Bytes != 3 || stats.Entries != 2 {
		t.Fatalf("replacement/oversize accounting: %+v", stats)
	}
	for _, maximum := range []int{0, -1} {
		disabled := newConsistencyDomainPackReads(maximum)
		disabled.retainBody(ctx, key(1), []byte("x"), storageformat.DomainPagePackWireProof{})
		if _, _, ok := disabled.cachedBody(ctx, key(1)); ok || disabled.snapshot().Entries != 0 {
			t.Fatal("disabled cache retained body")
		}
	}
	bounded := newConsistencyDomainPackReads(1024)
	for index := 0; index < metadataPackCacheMaximumEntries+3; index++ {
		bounded.retainBody(ctx, key(index), []byte("x"), storageformat.DomainPagePackWireProof{})
	}
	if stats := bounded.snapshot(); stats.Entries != metadataPackCacheMaximumEntries || stats.Bytes != metadataPackCacheMaximumEntries || stats.Evictions != 3 {
		t.Fatalf("entry LRU bound: %+v", stats)
	}
	if _, _, ok := bounded.cachedBody(ctx, key(0)); ok {
		t.Fatal("entry bound did not evict oldest")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	bounded.retainBody(canceled, key(999), []byte("x"), storageformat.DomainPagePackWireProof{})
	bounded.retainBody(withoutMetadataPackCache(ctx), key(998), []byte("x"), storageformat.DomainPagePackWireProof{})
	bounded.retainBody(ctx, key(997), nil, storageformat.DomainPagePackWireProof{})
	wireBound := newConsistencyDomainPackReads(storageformat.MaxDomainPagePackBytes + 1)
	wireBound.retainBody(ctx, key(1), make([]byte, storageformat.MaxDomainPagePackBytes+1), storageformat.DomainPagePackWireProof{})
	if wireBound.snapshot().Entries != 0 || bounded.snapshot().Entries != metadataPackCacheMaximumEntries {
		t.Fatal("bypass/cancellation/wire maximum changed cache ownership")
	}
}

func TestDomainPackCacheReviewProviderFailuresNeverBecomeIdleAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "unavailable", "malformed", "wrong-domain"} {
		t.Run(mode, func(t *testing.T) {
			pack := reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "provider-retry"}, "provider-retry", []byte("verified after retry"))
			var gets atomic.Int64
			backend := &admissionBackendProbe{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
				if gets.Add(1) == 1 {
					switch mode {
					case "missing":
						return objectstore.Object{}, domain.ErrNotFound
					case "unavailable":
						return objectstore.Object{}, domain.ErrUnavailable
					case "malformed":
						return objectstore.Object{Body: []byte("corrupt provider pack")}, nil
					case "wrong-domain":
						other := reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "foreign-domain"}, "provider-retry", pack.value)
						return objectstore.Object{Body: other.body}, nil
					}
				}
				return objectstore.Object{Key: pack.key, Body: pack.body}, nil
			}}
			store := reviewCachedPackStore(backend, 1<<20)
			if _, err := reviewReadCachedPack(context.Background(), store, pack); err == nil || store.packReads.snapshot().Entries != 0 {
				t.Fatalf("provider failure retained unverified authority: %v", err)
			}
			page, err := reviewReadCachedPack(context.Background(), store, pack)
			reviewAssertCachedPackValue(t, page, pack, err)
			if gets.Load() != 2 || store.packReads.snapshot().Entries != 1 {
				t.Fatal("provider failure prevented healthy retry")
			}
		})
	}
}

// Gate one cached decode before it acquires the process decode slot. This
// controls ordering without sleeps or holding a cache/provider mutex.
type reviewCacheDecodeGate struct {
	context.Context
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *reviewCacheDecodeGate) Done() <-chan struct{} {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.Context.Done()
}

func TestDomainPackCacheReviewCanceledHitPreservesVerifiedWire(t *testing.T) {
	pack := reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "canceled-cache-hit"}, "canceled-cache-hit", []byte("verified cache survives independent cancellation"))
	var gets atomic.Int64
	backend := &admissionBackendProbe{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		gets.Add(1)
		return objectstore.Object{Key: pack.key, Body: pack.body}, nil
	}}
	store := reviewCachedPackStore(backend, 1<<20)
	page, err := reviewReadCachedPack(context.Background(), store, pack)
	reviewAssertCachedPackValue(t, page, pack, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := &reviewCacheDecodeGate{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := reviewReadCachedPack(gate, store, pack); done <- err }()
	<-gate.entered
	waiting := make(chan struct{}, 1)
	healthy := make(chan error, 1)
	go func() {
		page, err := reviewReadCachedPack(&packReadWaitingContext{Context: context.Background(), ready: waiting}, store, pack)
		if err == nil && (len(page.Entries) != 1 || !bytes.Equal(page.Entries[0].Value, pack.value)) {
			err = errors.New("healthy waiter lost verified page")
		}
		healthy <- err
	}()
	<-waiting
	cancel()
	close(gate.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cached decode: %v", err)
	}
	if err := <-healthy; err != nil {
		t.Fatalf("healthy waiter inherited leader cancellation: %v", err)
	}
	if store.packReads.snapshot().Entries != 1 {
		t.Fatal("canceling one request evicted independently valid wire")
	}
	page, err = reviewReadCachedPack(context.Background(), store, pack)
	reviewAssertCachedPackValue(t, page, pack, err)
	if gets.Load() != 1 {
		t.Fatal("canceled cache hit caused unnecessary provider refetch")
	}
}

func TestDomainPackCacheReviewFreshVerificationCannotJoinCachedDecode(t *testing.T) {
	pack := reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "fresh-verification"}, "fresh-verification", []byte("valid cached original"))
	var gets atomic.Int64
	var corrupt atomic.Bool
	backend := &admissionBackendProbe{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		gets.Add(1)
		if corrupt.Load() {
			return objectstore.Object{Body: []byte("corrupt durable object")}, nil
		}
		return objectstore.Object{Key: pack.key, Body: pack.body}, nil
	}}
	store := reviewCachedPackStore(backend, 1<<20)
	page, err := reviewReadCachedPack(context.Background(), store, pack)
	reviewAssertCachedPackValue(t, page, pack, err)
	gate := &reviewCacheDecodeGate{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := reviewReadCachedPack(gate, store, pack); done <- err }()
	<-gate.entered
	corrupt.Store(true)
	freshCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, freshErr := reviewReadCachedPack(withoutMetadataPackCache(freshCtx), store, pack)
	cancel()
	close(gate.release)
	ordinaryErr := <-done
	if !errors.Is(freshErr, domain.ErrInvalid) || ordinaryErr != nil || gets.Load() != 2 || store.packReads.snapshot().Entries != 1 {
		t.Fatalf("fresh verification joined cached work or poisoned cache: fresh=%v ordinary=%v gets=%d cache=%+v", freshErr, ordinaryErr, gets.Load(), store.packReads.snapshot())
	}
}

func TestDomainPackCacheReviewScopeAndBackendIsolation(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "scope-owner"}
	pack := reviewCanonicalCachedPack(t, reference, "scope-shared-pack", []byte("metadata under exact owner and backend"))
	base := objectmemory.New()
	var gets atomic.Int64
	backend := &admissionBackendProbe{Backend: base, get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		gets.Add(1)
		return base.Get(ctx, key)
	}}
	store := reviewCachedPackStore(backend, 1<<20)
	packs := []reviewCachedPack{pack, reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "other-owner"}, "scope-shared-pack", pack.value), reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: reference.ID}, "scope-shared-pack", pack.value)}
	projection := pack
	projection.key = storageformat.ProjectionPagePackKey("projection-owner", storageformat.ProjectionDuplicates, pack.id)
	projection.binding.key = projection.key
	packs = append(packs, projection)
	for _, item := range packs {
		if _, err := base.Put(ctx, item.key, item.body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		page, err := reviewReadCachedPack(ctx, store, item)
		reviewAssertCachedPackValue(t, page, item, err)
	}
	if gets.Load() != 4 || store.packReads.snapshot().Entries != 4 {
		t.Fatalf("owner/kind/projection alias: gets=%d cache=%+v", gets.Load(), store.packReads.snapshot())
	}
	// Physical projection keys omit the expected projection-domain ID. A valid
	// cached body for another expected ID is not a cache or singleflight match.
	wrong := projection
	wrong.reference.ID = "another-projection-domain"
	if _, err := reviewReadCachedPack(ctx, store, wrong); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("projection domain binding bypassed: %v", err)
	}
	other := reviewCanonicalCachedPack(t, reference, "scope-shared-pack", []byte("different provider's independently valid metadata"))
	otherBackend := objectmemory.New()
	if _, err := otherBackend.Put(ctx, other.key, other.body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	otherStore := reviewCachedPackStore(otherBackend, 1<<20)
	page, err := reviewReadCachedPack(ctx, otherStore, other)
	reviewAssertCachedPackValue(t, page, other, err)
}

func TestDomainPackCacheReviewCheckpointBypassDetectsWarmDurableFaults(t *testing.T) {
	for _, mode := range []string{"corrupt", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x17cac001)
			ctx := context.Background()
			requests := scaleUploadRequestsCount("cached-checkpoint", 1)
			requests[0].Size = 4
			page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
			if err != nil || len(page) != 1 || page[0].Capability == nil {
				t.Fatalf("admission: %+v %v", page, err)
			}
			admissionWriteBytes(t, f, *page[0].Capability, []byte("four"), 0)
			result, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{{UploadID: page[0].UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor([]byte("four")).CRC32C}})
			if err != nil || result[0].Outcome != "applied" {
				t.Fatalf("actual completion: %+v %v", result, err)
			}
			checkpoint, err := f.engine.CreateCheckpoint(ctx, "warm-cache-integrity")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.engine.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err != nil {
				t.Fatalf("positive checkpoint: %v", err)
			}
			store := f.engine.stateDomainStore()
			reference := uploadDomainReference(f.scope.UserID())
			head, err := store.loadHead(ctx, reference)
			if err != nil {
				t.Fatal(err)
			}
			root := head.head.Base
			if root.PackID == "" {
				t.Fatal("fixture did not publish a canonical pack")
			}
			f.engine.packReads = newConsistencyDomainPackReads(16 << 20)
			store = f.engine.stateDomainStore()
			session := newConsistencyDomainTreeSession(store, reference)
			if _, err := session.readPage(ctx, domainPageRef{root: root}); err != nil {
				t.Fatal(err)
			}
			key := storageformat.DomainPagePackKey(reference.Kind, reference.ID, root.PackID)
			original, err := f.engine.backend.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "deleted" {
				err = f.engine.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: original.Version})
			} else {
				_, err = f.engine.backend.Put(ctx, key, []byte("synthetic corrupt durable pack"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: original.Version})
			}
			if err != nil {
				t.Fatal(err)
			}
			// Prove the cache is genuinely warm despite the durable fault, then require
			// checkpoint verification to consult provider authority and deny it.
			if _, err := newConsistencyDomainTreeSession(store, reference).readPage(ctx, domainPageRef{root: root}); err != nil {
				t.Fatalf("ordinary verified hit did not stay warm: %v", err)
			}
			f.stateLedger.Reset()
			f.fileLedger.Reset()
			if err := f.engine.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err == nil {
				t.Fatal("warm cache hid missing or corrupt checkpoint authority")
			}
			providerRead := false
			for _, event := range f.stateLedger.Events() {
				if event.Kind != providerbudget.RequestObjectGet && event.Kind != providerbudget.RequestObjectList {
					t.Fatalf("checkpoint verification mutated provider: %s", event.Kind)
				}
				if event.Kind == providerbudget.RequestObjectGet && event.Target == key.String() {
					providerRead = true
				}
			}
			if !providerRead {
				t.Fatal("checkpoint denial did not directly read the warm corrupt or missing pack")
			}
			condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
			if mode == "corrupt" {
				current, err := f.engine.backend.Head(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: current.Version}
			}
			if _, err := f.engine.backend.Put(ctx, key, original.Body, condition); err != nil {
				t.Fatal(err)
			}
			if err := f.engine.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err != nil {
				t.Fatalf("restored exact durable authority did not verify: %v", err)
			}
		})
	}
}

func TestDomainPackCacheReviewInflightProjectionBindingsAreIndependent(t *testing.T) {
	pack := reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "actual-projection-domain"}, "projection-shared-key", []byte("strictly bound projection"))
	pack.key = storageformat.ProjectionPagePackKey("same-owner", storageformat.ProjectionDuplicates, pack.id)
	var gets atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	backend := &admissionBackendProbe{Backend: objectmemory.New(), get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		if gets.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return objectstore.Object{}, ctx.Err()
			}
		}
		return objectstore.Object{Key: key, Body: pack.body}, nil
	}}
	store := reviewCachedPackStore(backend, 1<<20)
	wrong := pack
	wrong.reference.ID = "wrong-projection-domain"
	done := make(chan error, 1)
	go func() { _, err := reviewReadCachedPack(context.Background(), store, wrong); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	page, err := reviewReadCachedPack(ctx, store, pack)
	cancel()
	close(release)
	wrongErr := <-done
	reviewAssertCachedPackValue(t, page, pack, err)
	if !errors.Is(wrongErr, domain.ErrInvalid) || gets.Load() != 2 || store.packReads.snapshot().Entries != 1 {
		t.Fatalf("one projection poisoned another binding: wrong=%v gets=%d cache=%+v", wrongErr, gets.Load(), store.packReads.snapshot())
	}
}

func TestDomainPackCacheReviewEngineBackendLifetimesAreIndependent(t *testing.T) {
	a, b := newUploadBatchScaleFixture(t, 0x17cac011), newUploadBatchScaleFixture(t, 0x17cac012)
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: a.scope.UserID().String()}
	first := reviewCanonicalCachedPack(t, reference, "same-physical-pack-on-distinct-backends", []byte("backend A content"))
	second := reviewCanonicalCachedPack(t, reference, "same-physical-pack-on-distinct-backends", []byte("backend B content"))
	if first.key != second.key || first.root.root.Digest == second.root.root.Digest {
		t.Fatal("backend isolation fixture did not collide only in physical key")
	}
	ctx := context.Background()
	for _, fixture := range []struct {
		engine *Engine
		pack   reviewCachedPack
	}{{a.engine, first}, {b.engine, second}} {
		if _, err := fixture.engine.backend.Put(ctx, fixture.pack.key, fixture.pack.body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		page, err := reviewReadCachedPack(ctx, fixture.engine.stateDomainStore(), fixture.pack)
		reviewAssertCachedPackValue(t, page, fixture.pack, err)
	}
	if a.engine.packReads == b.engine.packReads || a.engine.packReads.snapshot().Entries != 1 || b.engine.packReads.snapshot().Entries != 1 {
		t.Fatal("independent engines share backend cache authority")
	}
}
