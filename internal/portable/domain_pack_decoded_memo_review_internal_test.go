package portable

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

// This exercises all three separately reviewed loadPack decoder call sites.
// It warms a genuine strict snapshot on one backend, then requires a separate
// backend's fresh actual GET to deny missing/corrupt/unavailable authority.
func TestDomainPackDecodedMemoReviewFreshBackendPresenceMaintenanceGETAndPureReuse(t *testing.T) {
	pack := reviewCanonicalCachedPack(t, consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: "decoded-memo-cross-backend-review"}, "decoded-memo-cross-backend-review", []byte("backend-independent canonical result"))
	warmObserver := telemetry.New(nil, nil)
	ctx := telemetry.Context(context.Background(), warmObserver)
	var primaryMode atomic.Int32
	var primaryGets atomic.Int64
	primary := &admissionBackendProbe{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
		primaryGets.Add(1)
		if primaryMode.Load() == 1 {
			return objectstore.Object{}, domain.ErrNotFound
		}
		body := bytes.Clone(pack.body)
		if primaryMode.Load() == 2 {
			body[0] ^= 0xff
		}
		return objectstore.Object{Key: pack.key, Body: body}, nil
	}}
	store := reviewCachedPackStore(primary, 1<<20)
	page, err := reviewReadCachedPack(ctx, store, pack)
	reviewAssertCachedPackValue(t, page, pack, err)
	if primaryGets.Load() != 1 || store.packReads.snapshot().Entries != 1 {
		t.Fatal("strict provider warmup was not exercised")
	}
	var warmMetrics bytes.Buffer
	warmObserver.WritePrometheus(&warmMetrics)
	if !strings.Contains(warmMetrics.String(), `endlessfs_operations_total{operation="metadata.pack.materialize",role="state",result="success"} 1`) {
		t.Fatal("the proposed ordinary decoded-memo integration did not actually execute")
	}
	for _, mode := range []string{"missing", "corrupt", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			var gets atomic.Int64
			other := &admissionBackendProbe{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
				if gets.Add(1) == 1 {
					switch mode {
					case "missing":
						return objectstore.Object{}, domain.ErrNotFound
					case "unavailable":
						return objectstore.Object{}, domain.ErrUnavailable
					case "corrupt":
						body := bytes.Clone(pack.body)
						body[0] ^= 0xff
						return objectstore.Object{Key: pack.key, Body: body}, nil
					}
				}
				return objectstore.Object{Key: pack.key, Body: bytes.Clone(pack.body)}, nil
			}}
			otherStore := reviewCachedPackStore(other, 1<<20)
			_, err := reviewReadCachedPack(ctx, otherStore, pack)
			want := domain.ErrInvalid
			if mode == "missing" {
				want = domain.ErrNotFound
			}
			if mode == "unavailable" {
				want = domain.ErrUnavailable
			}
			if !errors.Is(err, want) || gets.Load() != 1 || otherStore.packReads.snapshot().Entries != 0 {
				t.Fatal("globally warm pure result bypassed a fresh backend failure or silently retried")
			}
			page, err := reviewReadCachedPack(ctx, otherStore, pack)
			reviewAssertCachedPackValue(t, page, pack, err)
			if gets.Load() != 2 || otherStore.packReads.snapshot().Entries != 1 {
				t.Fatal("independent later valid provider read did not recover")
			}
		})
	}
	for _, mode := range []int32{1, 2} {
		primaryMode.Store(mode)
		before := primaryGets.Load()
		_, err := reviewReadCachedPack(withoutMetadataPackCache(ctx), store, pack)
		want := domain.ErrNotFound
		if mode == 2 {
			want = domain.ErrInvalid
		}
		if !errors.Is(err, want) || primaryGets.Load() != before+1 {
			t.Fatal("maintenance bypass treated either cache as provider presence")
		}
	}
	primaryMode.Store(0)
	observer := telemetry.New(nil, nil)
	observed := withoutMetadataPackCache(telemetry.Context(ctx, observer))
	verificationStore := reviewCachedPackStore(primary, 1<<20)
	beforeGets := primaryGets.Load()
	beforeCache := verificationStore.packReads.snapshot()
	if beforeCache.Entries != 0 || beforeCache.Bytes != 0 {
		t.Fatal("fresh verification store unexpectedly contains idle wire")
	}
	// Distinct operation sessions must each fetch actual provider bytes. This
	// models the independent closure/reachability read boundary, not a full
	// checkpoint proof; the unchanged real checkpoint/raw-copy suites do that.
	for operation := range 2 {
		page, err = reviewReadCachedPack(observed, verificationStore, pack)
		reviewAssertCachedPackValue(t, page, pack, err)
		if primaryGets.Load() != beforeGets+int64(operation)+1 {
			t.Fatal("later fresh verification reused provider-presence state")
		}
	}
	afterCache := verificationStore.packReads.snapshot()
	if afterCache.Entries != beforeCache.Entries || afterCache.Bytes != beforeCache.Bytes {
		t.Fatal("bypassed maintenance read admitted idle wire/proof")
	}
	var metrics bytes.Buffer
	observer.WritePrometheus(&metrics)
	if !strings.Contains(metrics.String(), `endlessfs_operations_total{operation="metadata.pack.materialize",role="state",result="success"} 2`) || strings.Contains(metrics.String(), `endlessfs_operations_total{operation="metadata.pack.decode",role="state",result=`) {
		t.Fatal("valid globally warm fresh-GET reads did not reuse only the exact pure decoder result")
	}
}
