package portable

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// This exercises the actual typed checkpoint semantic validator. Its one-GET
// requirement is conditional on the asserted durable one-pack geometry, not a
// guessed request budget for arbitrary namespace shapes.
func TestCheckpointTypedPaginationReusesOneOperationSession(t *testing.T) {
	const uploads = 257
	f := newUploadBatchScaleFixture(t, 0x1270b431)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("checkpoint-session", uploads)
	capabilities, err := f.engine.Files().CreateUploadBatch(ctx, f.scope, requests)
	if err != nil || len(capabilities) != uploads {
		t.Fatalf("real upload fixture: capabilities=%d err=%v", len(capabilities), err)
	}
	ids := make([]domain.UploadID, len(capabilities))
	for index, capability := range capabilities {
		ids[index] = capability.UploadID
	}
	// Omit BatchID to retain genuine individual terminal upload values and
	// their idempotency edges, as required by the existing 4,001-item proof.
	if err := f.engine.Files().AbortUploadBatch(ctx, f.scope, domain.AbortUploadBatchRequest{
		UploadIDs: ids, IdempotencyKey: "checkpoint-session-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	reference := uploadDomainReference(f.scope.UserID())
	store := f.engine.stateDomainStore()
	if err := store.compact(ctx, reference); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.head.Deltas) != 0 || snapshot.head.Base.EntryCount != 2*uploads || snapshot.head.Base.EntryCount <= 2*domainPageMaximumItems {
		t.Fatalf("fixture must materialize all 514 values across more than two list pages without deltas: values=%d deltas=%d", snapshot.head.Base.EntryCount, len(snapshot.head.Deltas))
	}
	maintenance := withoutMetadataPackCache(ctx)
	geometry := newConsistencyDomainTreeSession(store, reference)
	values, err := geometry.collect(maintenance, snapshot.head.Base, "", "", int(snapshot.head.Base.EntryCount)+1)
	if err != nil || uint64(len(values)) != snapshot.head.Base.EntryCount {
		t.Fatalf("independent durable fixture inventory: count=%d err=%v", len(values), err)
	}
	uploadValues, idempotencyValues := 0, 0
	for _, value := range values {
		switch {
		case strings.HasPrefix(value.Key, "upload/"):
			if _, err := decodePortableUploadRecord(value.Value, f.scope.UserID(), strings.TrimPrefix(value.Key, "upload/")); err != nil {
				t.Fatal(err)
			}
			uploadValues++
		case strings.HasPrefix(value.Key, "upload-idempotency/"):
			var record storageformat.PortableUploadIdempotency
			if err := decodeCanonicalValue(value.Value, &record); err != nil || storageformat.ValidatePortableUploadIdempotency(record) != nil {
				t.Fatal("real admission fixture has invalid upload idempotency")
			}
			idempotencyValues++
		}
	}
	if uploadValues != uploads || idempotencyValues != uploads {
		t.Fatalf("fixture must contain all real typed upload/idempotency pairs: upload=%d idempotency=%d", uploadValues, idempotencyValues)
	}
	// Ordinary compaction structurally shares old packs. Materialize the exact
	// independently read public-operation values/versions into one fresh pack
	// for this conditional pagination control. The supplied immutable head is
	// synthetic; the 4,001-item test separately proves real published heads.
	materialized := newConsistencyDomainTreeSession(store, reference)
	materialized.enablePackedWrites("checkpoint-pagination-single-pack")
	snapshot.head.Base, err = materialized.buildTree(maintenance, values)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialized.flushPack(maintenance); err != nil {
		t.Fatal(err)
	}
	geometry = newConsistencyDomainTreeSession(store, reference)
	packID := checkpointSessionReviewSinglePack(t, maintenance, geometry, snapshot.head.Base)
	key := storageformat.DomainPagePackKey(reference.Kind, reference.ID, packID)

	validate := func() error {
		return f.engine.validateKnownControlDomainValuesForSchema(withoutMetadataPackCache(context.Background()), reference, snapshot.head, true)
	}
	for operation := 0; operation < 2; operation++ {
		f.stateLedger.Reset()
		f.fileLedger.Reset()
		if err := validate(); err != nil {
			t.Fatalf("typed maintenance operation %d: %v", operation, err)
		}
		checkpointSessionReviewOneGet(t, f, key, false)
	}
	// A new maintenance operation must not inherit an older operation's decoded
	// maps or the ordinary idle cache's durability assumption.
	for _, damage := range []string{"missing", "corrupt"} {
		t.Run(damage, func(t *testing.T) {
			warm := newConsistencyDomainTreeSession(store, reference)
			if _, err := warm.readPage(ctx, domainPageRef{root: snapshot.head.Base}); err != nil {
				t.Fatal(err)
			}
			original, err := f.engine.backend.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			var replaceVersion objectstore.NativeVersion
			if damage == "missing" {
				if err := f.engine.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: original.Version}); err != nil {
					t.Fatal(err)
				}
			} else {
				replaceVersion, err = f.engine.backend.Put(ctx, key, []byte("synthetic-corrupt-pack"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: original.Version})
				if err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
				if damage == "corrupt" {
					condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: replaceVersion}
				}
				if _, err := f.engine.backend.Put(ctx, key, original.Body, condition); err != nil {
					t.Errorf("restore exact durable authority: %v", err)
				}
			}()
			f.stateLedger.Reset()
			f.fileLedger.Reset()
			err = validate()
			want := domain.ErrInvalid
			if damage == "missing" {
				want = domain.ErrNotFound
			}
			if !errors.Is(err, want) {
				t.Fatalf("fresh maintenance failed to reject %s durable pack: %v", damage, err)
			}
			checkpointSessionReviewOneGet(t, f, key, damage == "missing")
		})
	}
	f.stateLedger.Reset()
	f.fileLedger.Reset()
	if err := validate(); err != nil {
		t.Fatalf("fresh maintenance rejected exact restored pack: %v", err)
	}
	checkpointSessionReviewOneGet(t, f, key, false)
}

// Follow authenticated descriptors and reject literal pages or multiple packs;
// otherwise the regression's exact one-GET expectation would not be justified.
func checkpointSessionReviewSinglePack(t *testing.T, ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot) string {
	t.Helper()
	pending := []domainPageRef{{root: root}}
	visited := make(map[domainPageRef]bool)
	packID := ""
	for len(pending) > 0 {
		ref := pending[0]
		pending = pending[1:]
		if visited[ref] {
			continue
		}
		visited[ref] = true
		if ref.root.PackID == "" || packID != "" && packID != ref.root.PackID {
			t.Fatal("fixture base is not entirely contained in one authenticated pack")
		}
		packID = ref.root.PackID
		page, err := session.readPage(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		for _, child := range page.Children {
			pending = append(pending, domainPageRef{
				root:     storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount},
				firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter,
			})
		}
	}
	if len(visited) < 4 || packID == "" {
		t.Fatalf("fixture did not establish a multi-page one-pack tree: pages=%d", len(visited))
	}
	return packID
}

func checkpointSessionReviewOneGet(t *testing.T, f uploadBatchScaleFixture, key objectstore.Key, failed bool) {
	t.Helper()
	events := f.stateLedger.Events()
	if len(events) != 1 {
		// Error rather than Fatal preserves subsequent freshness/durability proofs
		// when the intended unmodified baseline repeats the same pack GET.
		t.Errorf("one-pack typed pagination issued %d state calls; want exactly one GET for one maintenance operation", len(events))
	}
	for _, event := range events {
		if event.Kind != providerbudget.RequestObjectGet || event.Target != key.String() || event.Failed != failed {
			t.Fatal("typed pagination performed unexpected state work or wrong failure classification")
		}
	}
	if len(f.fileLedger.Events()) != 0 {
		t.Fatal("typed metadata pagination performed file-provider work")
	}
}

// Return copies must remain detached even when a caller reuses a session.
func checkpointSessionReviewDetachedValues(t *testing.T, got []storageformat.DomainEntry, expected []storageformat.DomainEntry) {
	t.Helper()
	if len(got) != len(expected) {
		t.Fatalf("pagination count=%d want=%d", len(got), len(expected))
	}
	for index := range expected {
		if got[index].Key != expected[index].Key || got[index].LogicalVersion != expected[index].LogicalVersion || !bytes.Equal(got[index].Value, expected[index].Value) {
			t.Fatalf("pagination entry %d differs from literal expected key/version/value", index)
		}
	}
}
