package portable

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func replaceObjectBody(t *testing.T, backend *objectmemory.Backend, key objectstore.Key, body []byte) {
	t.Helper()
	current, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: current.Version}); err != nil {
		t.Fatal(err)
	}
}

func TestConsistencyDomainHeadCorruptionFailsClosed(t *testing.T) {
	ctx := context.Background()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "corruption-owner"}
	seed := objectmemory.New()
	store := newConsistencyDomainStore(seed, nil)
	if _, err := store.mutate(ctx, reference, consistencyDomainMutation{ID: "seed", Changes: []consistencyDomainChange{{Key: "record", Require: domainValueAbsent, Value: []byte("authority")}}}); err != nil {
		t.Fatal(err)
	}
	headKey := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	headObject, err := seed.Get(ctx, headKey)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var head storageformat.DomainHead
	if err := storageformat.DecodeEnvelope(headObject.Body, headKey, domainHeadSchema, &envelope, &head); err != nil {
		t.Fatal(err)
	}
	encode := func(value storageformat.DomainHead) []byte {
		body, err := storageformat.EncodeEnvelope(domainHeadSchema, headKey, envelope.Revision, value)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	for _, test := range []struct {
		name string
		body func() []byte
	}{
		{name: "malformed", body: func() []byte { return []byte("{") }},
		{name: "non-canonical", body: func() []byte { return append(append([]byte(nil), headObject.Body...), '\n') }},
		{name: "key-binding", body: func() []byte { changed := head; changed.DomainID = "another-owner"; return encode(changed) }},
		{name: "revision-gap", body: func() []byte { changed := head; changed.Deltas[0].Revision++; return encode(changed) }},
		{name: "logical-version", body: func() []byte {
			changed := head
			changed.Deltas[0].Changes[0].LogicalVersion = ""
			return encode(changed)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := objectmemory.New()
			if err := backend.Import(seed.Export()); err != nil {
				t.Fatal(err)
			}
			replaceObjectBody(t, backend, headKey, test.body())
			if _, err := newConsistencyDomainStore(backend, nil).get(ctx, reference, "record"); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("corrupt head read error = %v; want invalid", err)
			}
		})
	}
}

func TestNamespaceAndOutcomePageCorruptionFailsClosed(t *testing.T) {
	t.Run("namespace-page", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		store := newNamespaceStore(engine)
		live := namespaceTestScope(t, domain.AreaLive)
		entry := seedNamespaceBatchFiles(t, store, live, 1)[0]
		var pageKey objectstore.Key
		for key := range backend.Export() {
			if strings.Contains(key, "/domains/namespace/") && strings.Contains(key, "/pages/") {
				pageKey = objectstore.MustKey(key)
				break
			}
		}
		if pageKey.String() == "" {
			t.Fatal("namespace fixture has no immutable page")
		}
		object, err := backend.Get(context.Background(), pageKey)
		if err != nil {
			t.Fatal(err)
		}
		var page storageformat.DomainPage
		if err := decodeCanonicalValue(object.Body, &page); err != nil {
			t.Fatal(err)
		}
		page.Entries[0].Value = append(page.Entries[0].Value, byte('x'))
		body, err := storageformat.EncodeCanonical(page)
		if err != nil {
			t.Fatal(err)
		}
		replaceObjectBody(t, backend, pageKey, body)
		if _, err := store.stat(context.Background(), live, entry.Path); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt namespace page error = %v; want invalid", err)
		}
	})

	t.Run("missing-namespace-page", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		store := newNamespaceStore(engine)
		live := namespaceTestScope(t, domain.AreaLive)
		entry := seedNamespaceBatchFiles(t, store, live, 1)[0]
		for key := range backend.Export() {
			if !strings.Contains(key, "/domains/namespace/") || !strings.Contains(key, "/pages/") {
				continue
			}
			parsed := objectstore.MustKey(key)
			object, err := backend.Get(context.Background(), parsed)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Delete(context.Background(), parsed, objectstore.DeleteCondition{Version: object.Version}); err != nil {
				t.Fatal(err)
			}
			break
		}
		if _, err := store.stat(context.Background(), live, entry.Path); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing namespace page error = %v; want not found", err)
		}
	})

	t.Run("outcome-page", func(t *testing.T) {
		backend := objectmemory.New()
		store := newConsistencyDomainStore(backend, nil)
		reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "outcome-corruption"}
		mutation := consistencyDomainMutation{ID: "retained-outcome", Changes: []consistencyDomainChange{{Key: "value", Require: domainValueAbsent, Value: []byte("one")}}}
		if _, err := store.mutate(context.Background(), reference, mutation); err != nil {
			t.Fatal(err)
		}
		if err := store.compact(context.Background(), reference); err != nil {
			t.Fatal(err)
		}
		head, err := store.loadHead(context.Background(), reference)
		if err != nil || head.head.Outcomes.Digest == "" {
			t.Fatalf("compacted outcome head = %+v, %v", head.head, err)
		}
		key := storageformat.DomainPageKey(reference.Kind, reference.ID, head.head.Outcomes.Digest)
		replaceObjectBody(t, backend, key, []byte("{}"))
		if _, err := store.mutate(context.Background(), reference, mutation); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt outcome replay error = %v; want invalid", err)
		}
	})
}
