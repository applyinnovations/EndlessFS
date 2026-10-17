package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type reviewRepackFaultBackend struct {
	objectstore.Backend
	head     objectstore.Key
	packPuts int
	headPuts int
	before   func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) error
	after    func(context.Context, objectstore.Key, objectstore.NativeVersion, error) (objectstore.NativeVersion, error)
}

func (backend *reviewRepackFaultBackend) Put(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
	if strings.Contains(key.String(), "/packs/") {
		backend.packPuts++
	}
	if key == backend.head {
		backend.headPuts++
	}
	if backend.before != nil {
		if err := backend.before(ctx, key, body, condition); err != nil {
			return "", err
		}
	}
	version, err := backend.Backend.Put(ctx, key, body, condition)
	if backend.after != nil {
		return backend.after(ctx, key, version, err)
	}
	return version, err
}

func reviewRepackMutation(t *testing.T) consistencyDomainMutation {
	t.Helper()
	mutation := consistencyDomainMutation{ID: "independent-opaque-repack", Result: []byte("committed")}
	noise := &deterministicScaleReader{state: 0xa63927f1}
	for index := range 8 {
		value := make([]byte, 600_000)
		_, _ = noise.Read(value)
		mutation.Changes = append(mutation.Changes, consistencyDomainChange{Key: fmt.Sprintf("noise-%02d", index), Require: domainValueAbsent, Value: value})
	}
	return mutation
}

func reviewStageOpaqueChild(t *testing.T, store *consistencyDomainStore, reference consistencyDomainRef, mutation *consistencyDomainMutation) (consistencyDomainHeadSnapshot, *consistencyDomainTreeSession, storageformat.DomainTreeRoot) {
	t.Helper()
	snapshot, session := materializedDomainView(t, store, reference, mutation.ID)
	child, err := session.writePage(context.Background(), storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: []storageformat.DomainEntry{{Key: "nested-child", Value: []byte("synthetic-child-value"), LogicalVersion: "child-version"}}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := storageformat.EncodeCanonical(child.root)
	if err != nil {
		t.Fatal(err)
	}
	change := consistencyDomainChange{Key: "opaque-child-reference", Require: domainValueAbsent, Value: body}
	if len(mutation.Changes) > 0 && mutation.Changes[len(mutation.Changes)-1].Key == change.Key {
		mutation.Changes[len(mutation.Changes)-1] = change
	} else {
		mutation.Changes = append(mutation.Changes, change)
	}
	return snapshot, session, child.root
}

func reviewAssertRepackedMutation(t *testing.T, backend objectstore.Backend, reference consistencyDomainRef, mutation consistencyDomainMutation, child storageformat.DomainTreeRoot) {
	t.Helper()
	ctx := context.Background()
	reader := newConsistencyDomainStore(backend, nil, domain.NewFixedClock(time.Date(2073, 4, 5, 6, 7, 8, 0, time.UTC)))
	for _, change := range mutation.Changes {
		actual, err := reader.get(ctx, reference, change.Key)
		if err != nil || !bytes.Equal(actual.Data, change.Value) {
			t.Fatalf("published value changed for %s: err=%v", change.Key, err)
		}
	}
	embedded, err := reader.get(ctx, reference, "opaque-child-reference")
	if err != nil {
		t.Fatal(err)
	}
	var recovered storageformat.DomainTreeRoot
	if err := decodeCanonicalValue(embedded.Data, &recovered); err != nil || recovered != child {
		t.Fatalf("opaque descriptor changed: recovered=%+v child=%+v err=%v", recovered, child, err)
	}
	page, err := newConsistencyDomainTreeSession(reader, reference).readPage(ctx, domainPageRef{root: recovered})
	if err != nil || len(page.Entries) != 1 || string(page.Entries[0].Value) != "synthetic-child-value" {
		t.Fatalf("opaque child was not durably reachable after repack: page=%+v err=%v", page, err)
	}
	snapshot, err := reader.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	indexReader := newConsistencyDomainTreeSession(reader, reference)
	storedOutcome, found, err := indexReader.lookup(ctx, snapshot.head.Outcomes, mutation.ID)
	if err != nil || !found {
		t.Fatalf("repacked outcome root lost the mutation receipt: found=%t err=%v", found, err)
	}
	var receipt storageformat.DomainOutcome
	if err := decodeCanonicalValue(storedOutcome.Data, &receipt); err != nil || receipt.MutationID != mutation.ID || !bytes.Equal(receipt.Result, mutation.Result) {
		t.Fatalf("repacked mutation receipt changed: %+v %v", receipt, err)
	}
	expiry, found, err := indexReader.lookup(ctx, snapshot.head.OutcomeExpiry, consistencyDomainOutcomeExpiryKey(receipt.RetainUntil, mutation.ID))
	if err != nil || !found || string(expiry.Data) != mutation.ID {
		t.Fatalf("repacked expiry root lost its receipt binding: found=%t value=%q err=%v", found, expiry.Data, err)
	}
}

func TestMaterializedRepackReviewPreservesOpaqueChildrenAndPublicationBoundaries(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"success", "lost-prepared-put", "lost-root-put", "root-put-unavailable", "prepared-pack-collision", "lost-head-put", "concurrent-head", "freeze", "cancel-before-head", "corrupt-prepared-page"} {
		t.Run(mode, func(t *testing.T) {
			parallelCorrectiveHeavyFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := objectmemory.New()
			reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "independent-repack-owner"}
			backend := &reviewRepackFaultBackend{Backend: base, head: storageformat.DomainHeadKey(reference.Kind, reference.ID)}
			clock := domain.NewFixedClock(time.Date(2073, 4, 5, 6, 7, 8, 0, time.UTC))
			store := newConsistencyDomainStore(backend, nil, clock)
			if err := store.ensureRegistered(ctx, reference); err != nil {
				t.Fatal(err)
			}
			mutation := reviewRepackMutation(t)
			snapshot, session, child := reviewStageOpaqueChild(t, store, reference, &mutation)
			preparedKey := session.packKey(session.packID)
			backend.packPuts, backend.headPuts = 0, 0
			triggered := false
			backend.before = func(ctx context.Context, key objectstore.Key, _ []byte, _ objectstore.PutCondition) error {
				if !triggered && mode == "root-put-unavailable" && strings.Contains(key.String(), "/packs/") && key != preparedKey {
					triggered = true
					return domain.NewError(domain.ErrorUnavailable, "synthetic unavailable root write")
				}
				if !triggered && mode == "prepared-pack-collision" && key == preparedKey {
					triggered = true
					_, err := base.Put(ctx, key, []byte("synthetic conflicting immutable bytes"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
					return err
				}
				if key == backend.head {
					if _, err := newConsistencyDomainStore(base, nil, clock).get(context.Background(), reference, "noise-00"); !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("unpublished noise became visible before head CAS: %v", err)
					}
				}
				return nil
			}
			backend.after = func(_ context.Context, key objectstore.Key, version objectstore.NativeVersion, err error) (objectstore.NativeVersion, error) {
				if err == nil && !triggered && (mode == "lost-prepared-put" && key == preparedKey || mode == "lost-root-put" && strings.Contains(key.String(), "/packs/") && key != preparedKey || mode == "lost-head-put" && key == backend.head) {
					triggered = true
					return "", domain.NewError(domain.ErrorUnavailable, "synthetic lost durable write response")
				}
				return version, err
			}
			store.scheduler = SchedulerFunc(func(ctx context.Context, step string) error {
				if step != StepDomainBeforeHeadCommit || triggered {
					return nil
				}
				switch mode {
				case "concurrent-head":
					triggered = true
					_, err := newConsistencyDomainStore(base, nil, clock).mutate(ctx, reference, consistencyDomainMutation{ID: "concurrent-winner", Changes: []consistencyDomainChange{{Key: "winner", Require: domainValueAbsent, Value: []byte("winner")}}})
					return err
				case "freeze":
					triggered = true
					return newConsistencyDomainStore(base, nil, clock).freeze(ctx, reference, 7)
				case "cancel-before-head":
					triggered = true
					cancel()
				}
				return nil
			})
			if mode == "corrupt-prepared-page" {
				page := session.pendingPack[child.Digest]
				page.Entries = append([]storageformat.DomainEntry(nil), page.Entries...)
				page.Entries[0].Value = []byte("corrupt")
				session.pendingPack[child.Digest] = page
			}
			outcome, err := store.mutateMaterializedPrepared(ctx, reference, mutation, &snapshot, session)
			if mode == "success" || mode == "lost-head-put" {
				if err != nil || outcome.Replayed != (mode == "lost-head-put") {
					t.Fatalf("successful authoritative repack: outcome=%+v err=%v", outcome, err)
				}
				reviewAssertRepackedMutation(t, base, reference, mutation, child)
				fresh, freshSession := materializedDomainView(t, store, reference, mutation.ID)
				replayed, replayErr := store.mutateMaterializedPrepared(context.Background(), reference, mutation, &fresh, freshSession)
				if replayErr != nil || !replayed.Replayed || replayed.Revision != outcome.Revision {
					t.Fatalf("repacked durable receipt did not replay: %+v %v", replayed, replayErr)
				}
				if backend.packPuts < 3 {
					t.Fatalf("fixture did not exercise prepared plus multiple root packs: %d", backend.packPuts)
				}
				return
			}
			if err == nil {
				t.Fatal("fault did not deny publication")
			}
			if mode == "corrupt-prepared-page" {
				if !errors.Is(err, domain.ErrInvalid) || errors.Is(err, storageformat.ErrDomainPagePackTooLarge) || backend.packPuts != 0 || backend.headPuts != 0 {
					t.Fatalf("corruption triggered repack or publication: err=%v packs=%d heads=%d", err, backend.packPuts, backend.headPuts)
				}
				return
			}
			if !triggered {
				t.Fatalf("fault boundary not reached: %v", err)
			}
			if mode == "prepared-pack-collision" && (!errors.Is(err, errDomainPagePackIdentityCollision) || errors.Is(err, storageformat.ErrDomainPagePackTooLarge) || backend.headPuts != 0) {
				t.Fatalf("immutable collision was repacked or published: %v heads=%d", err, backend.headPuts)
			}
			if mode == "cancel-before-head" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
			if _, err := newConsistencyDomainStore(base, nil, clock).get(context.Background(), reference, "noise-00"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("failed repack published partial authority: %v", err)
			}
			if mode == "lost-prepared-put" || mode == "lost-root-put" || mode == "root-put-unavailable" {
				if backend.headPuts != 0 {
					t.Fatalf("head CAS attempted before pack flush completed: %d", backend.headPuts)
				}
				backend.after = nil
				backend.before = nil
				fresh, freshSession, freshChild := reviewStageOpaqueChild(t, store, reference, &mutation)
				retry, retryErr := store.mutateMaterializedPrepared(context.Background(), reference, mutation, &fresh, freshSession)
				if retryErr != nil || retry.Replayed {
					t.Fatalf("safe retry after lost pack write failed: %+v %v", retry, retryErr)
				}
				reviewAssertRepackedMutation(t, base, reference, mutation, freshChild)
			}
		})
	}
}

func TestBoundedDomainRepackReviewDeniesInvalidCanceledAndUnreadablePages(t *testing.T) {
	for _, mode := range []string{"canceled-page", "oversized-page", "invalid-page", "canceled-root", "missing-staged-root", "misbound-staged-root", "final-pack-unavailable", "prior-and-empty-roots"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := objectmemory.New()
			reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "repack-denial-owner"}
			backend := &reviewRepackFaultBackend{Backend: base, head: storageformat.DomainHeadKey(reference.Kind, reference.ID)}
			store := newConsistencyDomainStore(backend, nil, domain.NewFixedClock(time.Date(2073, 4, 5, 6, 7, 8, 0, time.UTC)))
			source := newConsistencyDomainTreeSession(store, reference)
			source.enablePackedWrites("synthetic-authenticated-head")
			if err := source.bindPackedMutation("synthetic-mutation"); err != nil {
				t.Fatal(err)
			}
			page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: []storageformat.DomainEntry{{Key: "selected", Value: []byte("synthetic-value"), LogicalVersion: "synthetic-version"}}}
			selected, err := source.writePage(ctx, page)
			if err != nil {
				t.Fatal(err)
			}
			roots := []storageformat.DomainTreeRoot{selected.root}
			switch mode {
			case "canceled-page", "canceled-root":
				cancel()
			case "oversized-page":
				page.Entries[0].Value = bytes.Repeat([]byte("x"), storageformat.MaxCanonicalBytes)
			case "invalid-page":
				page.SchemaVersion = 0
			case "missing-staged-root":
				delete(source.pendingPack, selected.root.Digest)
				delete(source.pages, selected.root.Digest)
			case "misbound-staged-root":
				roots[0].ByteCount++
			case "final-pack-unavailable":
				backend.before = func(_ context.Context, key objectstore.Key, _ []byte, _ objectstore.PutCondition) error {
					if strings.Contains(key.String(), "/packs/") {
						return domain.NewError(domain.ErrorUnavailable, "synthetic final pack interruption")
					}
					return nil
				}
			case "prior-and-empty-roots":
				roots[0].PackID = storageformat.Digest([]byte("prior-durable-pack"))
				roots = append(roots, storageformat.DomainTreeRoot{})
			}
			var actual []storageformat.DomainTreeRoot
			if mode == "canceled-page" || mode == "oversized-page" || mode == "invalid-page" {
				_, err = (&boundedDomainPacker{source: source, binding: "synthetic-root-set"}).write(ctx, page)
			} else {
				actual, err = repackStagedRoots(ctx, source, roots)
			}
			if mode == "prior-and-empty-roots" {
				if err != nil || len(actual) != 2 || actual[0] != roots[0] || actual[1] != roots[1] {
					t.Fatalf("unchanged durable/empty descriptors were rewritten: actual=%+v err=%v", actual, err)
				}
			} else if err == nil {
				t.Fatal("invalid, unreadable or canceled repacking returned a publishable root")
			}
			if (mode == "canceled-page" || mode == "canceled-root") && (!errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable)) {
				t.Fatalf("packing cancellation lost its cause/domain classification: %v", err)
			}
			if (mode == "oversized-page" || mode == "invalid-page" || mode == "misbound-staged-root") && (!errors.Is(err, domain.ErrInvalid) || errors.Is(err, storageformat.ErrDomainPagePackTooLarge)) {
				t.Fatalf("invalid canonical authority was classified as recoverable capacity: %v", err)
			}
			wantPuts := 0
			if mode == "final-pack-unavailable" {
				wantPuts = 1
				if !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("final pack interruption lost its classification: %v", err)
				}
			}
			if backend.packPuts != wantPuts || backend.headPuts != 0 || len(base.Export()) != 0 {
				t.Fatalf("denied repacking leaked durable writes or authority: packs=%d heads=%d objects=%d", backend.packPuts, backend.headPuts, len(base.Export()))
			}
		})
	}
}

func TestMaterializedRepackReviewOversizePreparedClosureFailsClosed(t *testing.T) {
	parallelCorrectiveHeavyFixture(t)
	ctx := context.Background()
	base := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "oversize-prepared-owner"}
	backend := &reviewRepackFaultBackend{Backend: base, head: storageformat.DomainHeadKey(reference.Kind, reference.ID)}
	store := newConsistencyDomainStore(backend, nil, domain.NewFixedClock(time.Date(2073, 4, 5, 6, 7, 8, 0, time.UTC)))
	if err := store.ensureRegistered(ctx, reference); err != nil {
		t.Fatal(err)
	}
	snapshot, session := materializedDomainView(t, store, reference, "oversize-prepared")
	noise := &deterministicScaleReader{state: 0xf89325}
	roots := make([]storageformat.DomainTreeRoot, 8)
	for index := range roots {
		value := make([]byte, 600_000)
		_, _ = noise.Read(value)
		child, err := session.writePage(ctx, storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Entries: []storageformat.DomainEntry{{Key: fmt.Sprintf("child-%02d", index), Value: value, LogicalVersion: "synthetic-version"}}})
		if err != nil {
			t.Fatal(err)
		}
		roots[index] = child.root
	}
	body, err := storageformat.EncodeCanonical(roots)
	if err != nil {
		t.Fatal(err)
	}
	backend.packPuts, backend.headPuts = 0, 0
	_, err = store.mutateMaterializedPrepared(ctx, reference, consistencyDomainMutation{ID: "oversize-prepared", Changes: []consistencyDomainChange{{Key: "opaque-children", Require: domainValueAbsent, Value: body}}}, &snapshot, session)
	if !errors.Is(err, storageformat.ErrDomainPagePackTooLarge) || backend.packPuts != 0 || backend.headPuts != 0 {
		t.Fatalf("oversized opaque dependency set was changed or published: err=%v packs=%d heads=%d", err, backend.packPuts, backend.headPuts)
	}
	if _, err := store.get(ctx, reference, "opaque-children"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("oversized prepared closure became visible: %v", err)
	}
}
