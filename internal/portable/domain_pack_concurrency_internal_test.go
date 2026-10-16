package portable

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Signal when a request is blocked either in its provider read or waiting for
// another request's immutable read. No sleeps or scheduler timing assumptions.
type packReadWaitingContext struct {
	context.Context
	once  sync.Once
	ready chan<- struct{}
}

func (ctx *packReadWaitingContext) Done() <-chan struct{} {
	ctx.once.Do(func() { ctx.ready <- struct{}{} })
	return ctx.Context.Done()
}

func TestMetadataPackConcurrentRequestsShareReadAndDecode(t *testing.T) {
	const requests = 50 // Incident cohort per pod.
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "synthetic-upload-owner"}
	packID := storageformat.Digest([]byte("synthetic-upload-pack"))
	pack := storageformat.DomainPagePack{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, PackID: packID}
	value := bytes.Repeat([]byte(`{"record":"retained-control-metadata"}`), 512)
	for pageIndex := range 8 {
		page := storageformat.DomainPage{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind}
		for entryIndex := range 16 {
			page.Entries = append(page.Entries, storageformat.DomainEntry{Key: fmt.Sprintf("upload/%03d-%03d", pageIndex, entryIndex), Value: value, LogicalVersion: "v1"})
		}
		body, err := storageformat.EncodeCanonical(page)
		if err != nil {
			t.Fatal(err)
		}
		pack.Pages = append(pack.Pages, storageformat.DomainPackedPage{Digest: storageformat.Digest(body), Page: page})
	}
	digest := pack.Pages[0].Digest
	wire, err := storageformat.EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, requests)
	release := make(chan struct{})
	var reads atomic.Int32
	backend := &hookedBackend{Backend: objectmemory.New(), get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
		reads.Add(1)
		select {
		case <-ctx.Done():
			return objectstore.Object{}, ctx.Err()
		case <-release:
			return objectstore.Object{Key: key, Body: wire}, nil
		}
	}}
	engine := openNamespaceTestEngine(t, objectmemory.New())
	engine.backend = backend
	results := make(chan error, requests)
	for range requests {
		go func() {
			ctx := &packReadWaitingContext{Context: context.Background(), ready: ready}
			// Each upload status constructs its own namespace store/session.
			session := newConsistencyDomainTreeSession(newNamespaceStore(engine).domain, reference)
			pages, err := session.loadPack(ctx, packID)
			if err == nil && !bytes.Equal(pages[digest].Entries[0].Value, value) {
				err = fmt.Errorf("wrong immutable page")
			}
			results <- err
		}()
	}
	for range requests {
		<-ready
	}
	close(release)
	for range requests {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("concurrent metadata requests read/decode the same immutable pack %d times, want 1", got)
	}
	t.Logf("%d concurrent requests, %d wire bytes, %d immutable pages: %d read/decode", requests, len(wire), len(pack.Pages), reads.Load())
}
