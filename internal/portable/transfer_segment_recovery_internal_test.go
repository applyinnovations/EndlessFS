package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func transferSegmentBatchItem(owner domain.UserID, batchID string, index, count uint64) portableUploadBatchItem {
	uploadID := fmt.Sprintf("upload-%05d", index)
	return portableUploadBatchItem{record: storageformat.PortableUploadRecord{
		SchemaVersion: 1,
		UploadID:      uploadID,
		OwnerID:       owner.String(),
		Area:          "live",
		RequestedPath: fmt.Sprintf("/upload-%05d.bin", index),
		ResolvedPath:  fmt.Sprintf("/upload-%05d.bin", index),
		BlobID:        uploadID,
		Size:          1,
		MediaType:     "application/octet-stream",
		Conflict:      domain.ConflictFail,
		Resumable:     true,
		State:         storageformat.UploadActive,
		CreatedAt:     time.Date(2070, 1, 2, 3, 4, 5, 0, time.UTC),
		ExpiresAt:     time.Date(2070, 1, 2, 4, 4, 5, 0, time.UTC),
		Batch:         &storageformat.PortableUploadBatchMember{BatchID: batchID, Index: index, Count: count},
	}}
}

func transferSegmentLeases(first, count uint64) []storageformat.PortableUploadLease {
	leasing := make([]storageformat.PortableUploadLease, count)
	for offset := range leasing {
		index := first + uint64(offset)
		leasing[offset] = storageformat.PortableUploadLease{Index: index, UploadID: fmt.Sprintf("upload-%05d", index), Lease: []byte(fmt.Sprintf("lease-%05d", index))}
	}
	return leasing
}

func putTransferSegment(t *testing.T, backend objectstore.Backend, value storageformat.PortableUploadLeaseSegment) {
	t.Helper()
	body, err := storageformat.EncodePortableUploadLeaseSegment(value)
	if err != nil {
		t.Fatal(err)
	}
	key := storageformat.UploadLeaseSegmentKey(value.BackendKind, value.BatchID, value.Segment)
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeUploadLeaseForRecordUsesTerminalAndCrashProgressSegments(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	batchID := storageformat.Digest([]byte("runtime-segment-batch"))
	newFixture := func(t *testing.T) (*objectmemory.Backend, *FileStore) {
		t.Helper()
		backend := objectmemory.New()
		return backend, openNamespaceTestEngine(t, backend).Files()
	}

	t.Run("single-upload-delegation", func(t *testing.T) {
		_, files := newFixture(t)
		if _, _, err := files.runtimeUploadLeaseForRecord(ctx, storageformat.PortableUploadRecord{UploadID: "missing"}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		backend, files := newFixture(t)
		leases := transferSegmentLeases(0, 2)
		putTransferSegment(t, backend, storageformat.PortableUploadLeaseSegment{
			SchemaVersion: 1, BackendKind: backend.BackendKind(), OwnerID: owner.String(), BatchID: batchID,
			Segment: 0, TotalCount: 2, FirstIndex: 0, Leases: leases,
		})
		record := transferSegmentBatchItem(owner, batchID, 1, 2).record
		lease, object, err := files.runtimeUploadLeaseForRecord(ctx, record)
		if err != nil || string(lease) != string(leases[1].Lease) || object.Key != storageformat.UploadLeaseSegmentKey(backend.BackendKind(), batchID, 0) {
			t.Fatalf("lease = %q, object=%+v, error=%v", lease, object, err)
		}
		lease[0] ^= 0xff
		second, _, secondErr := files.runtimeUploadLeaseForRecord(context.Background(), record)
		if secondErr != nil || string(second) != string(leases[1].Lease) {
			t.Fatalf("stored lease changed through returned slice: lease=%q error=%v", second, secondErr)
		}
	})

	t.Run("partial-crash-progress", func(t *testing.T) {
		backend, files := newFixture(t)
		putTransferSegment(t, backend, storageformat.PortableUploadLeaseSegment{
			SchemaVersion: 1, BackendKind: backend.BackendKind(), OwnerID: owner.String(), BatchID: batchID,
			Segment: 0, TotalCount: 1_500, FirstIndex: 0, Leases: transferSegmentLeases(0, storageformat.MaxUploadLeaseSegmentItems),
		})
		record := transferSegmentBatchItem(owner, batchID, 73, 1_500).record
		lease, object, err := files.runtimeUploadLeaseForRecord(ctx, record)
		if err != nil || string(lease) != "lease-00073" || object.Key != storageformat.UploadLeaseSegmentKey(backend.BackendKind(), batchID, 0) {
			t.Fatalf("fallback lease = %q, object=%+v, error=%v", lease, object, err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, files := newFixture(t)
		record := transferSegmentBatchItem(owner, batchID, 0, 1).record
		if _, _, err := files.runtimeUploadLeaseForRecord(ctx, record); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		backend, files := newFixture(t)
		key := storageformat.UploadLeaseSegmentKey(backend.BackendKind(), batchID, 0)
		if _, err := backend.Put(ctx, key, []byte("corrupt"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		record := transferSegmentBatchItem(owner, batchID, 0, 1).record
		if _, _, err := files.runtimeUploadLeaseForRecord(ctx, record); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("total-mismatch", func(t *testing.T) {
		backend, files := newFixture(t)
		putTransferSegment(t, backend, storageformat.PortableUploadLeaseSegment{
			SchemaVersion: 1, BackendKind: backend.BackendKind(), OwnerID: owner.String(), BatchID: batchID,
			Segment: 0, TotalCount: 2, FirstIndex: 0, Leases: transferSegmentLeases(0, 2),
		})
		record := transferSegmentBatchItem(owner, batchID, 0, 1).record
		if _, _, err := files.runtimeUploadLeaseForRecord(ctx, record); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("member-mismatch", func(t *testing.T) {
		backend, files := newFixture(t)
		putTransferSegment(t, backend, storageformat.PortableUploadLeaseSegment{
			SchemaVersion: 1, BackendKind: backend.BackendKind(), OwnerID: owner.String(), BatchID: batchID,
			Segment: 0, TotalCount: 2, FirstIndex: 0, Leases: transferSegmentLeases(0, 2),
		})
		record := transferSegmentBatchItem(owner, batchID, 0, 2).record
		record.UploadID = "different-upload"
		if _, _, err := files.runtimeUploadLeaseForRecord(ctx, record); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestUploadLeaseSegmentHelpersRejectNonContiguousOrMisboundAuthority(t *testing.T) {
	batchID := storageformat.Digest([]byte("helper-segment-batch"))
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	stored := storageformat.PortableUploadLeaseSegment{
		SchemaVersion: 1, BackendKind: "memory", OwnerID: owner.String(), BatchID: batchID,
		Segment: 0, TotalCount: 2, FirstIndex: 0, Leases: transferSegmentLeases(0, 2),
	}
	if merged, err := mergePortableUploadLeaseProgress(nil, stored, 0, 2); err != nil || len(merged) != 2 {
		t.Fatalf("terminal merge = %d, %v", len(merged), err)
	}
	partial := stored
	partial.TotalCount = 2_000
	partial.Leases = transferSegmentLeases(0, storageformat.MaxUploadLeaseSegmentItems)
	if merged, err := mergePortableUploadLeaseProgress(nil, partial, 0, partial.TotalCount); err != nil || len(merged) != storageformat.MaxUploadLeaseSegmentItems {
		t.Fatalf("partial merge = %d, %v", len(merged), err)
	}
	for name, run := range map[string]func() error{
		"total": func() error { _, err := mergePortableUploadLeaseProgress(nil, stored, 0, 3); return err },
		"prior": func() error {
			_, err := mergePortableUploadLeaseProgress(stored.Leases[:1], partial, 0, partial.TotalCount)
			return err
		},
		"first": func() error {
			candidate := partial
			candidate.FirstIndex = 1
			_, err := mergePortableUploadLeaseProgress(nil, candidate, 0, candidate.TotalCount)
			return err
		},
	} {
		t.Run("merge-"+name, func(t *testing.T) {
			if err := run(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	item := transferSegmentBatchItem(owner, batchID, 0, 2)
	for name, mutate := range map[string]func(*portableUploadBatchItem, *storageformat.PortableUploadLeaseSegment){
		"missing-batch": func(item *portableUploadBatchItem, _ *storageformat.PortableUploadLeaseSegment) {
			item.record.Batch = nil
		},
		"count": func(item *portableUploadBatchItem, _ *storageformat.PortableUploadLeaseSegment) {
			item.record.Batch.Count++
		},
		"before-first": func(item *portableUploadBatchItem, value *storageformat.PortableUploadLeaseSegment) {
			value.FirstIndex = 1
		},
		"cardinality": func(item *portableUploadBatchItem, value *storageformat.PortableUploadLeaseSegment) {
			value.Leases = nil
		},
		"member": func(item *portableUploadBatchItem, value *storageformat.PortableUploadLeaseSegment) {
			value.Leases[0].UploadID = "other"
		},
	} {
		t.Run("resume-"+name, func(t *testing.T) {
			candidateItem, candidateStored := item, stored
			batch := *item.record.Batch
			candidateItem.record.Batch = &batch
			candidateStored.Leases = append([]storageformat.PortableUploadLease(nil), stored.Leases...)
			mutate(&candidateItem, &candidateStored)
			if _, err := resumePortableUploadLeaseSegment(context.Background(), objectmemory.New(), []portableUploadBatchItem{candidateItem}, candidateStored); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	failure := domain.NewError(domain.ErrorUnavailable, "resume unavailable")
	backend := objectmemory.New()
	transfers := &transferFailureBackend{Backend: backend, transfers: backend, resumeErr: failure}
	if _, err := resumePortableUploadLeaseSegment(context.Background(), transfers, []portableUploadBatchItem{item}, stored); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("resume error = %v", err)
	}
	abortUploadHandles(context.Background(), &transferFailureBackend{Backend: backend, transfers: backend, abortErr: failure}, []objectstore.UploadHandle{{}, {Lease: []byte("sealed")}})
}

func TestEnsureUploadLeaseSegmentRejectsInvalidProgressBeforeProviderWork(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	files := engine.Files()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	batchID := storageformat.Digest([]byte("ensure-segment-batch"))
	valid := transferSegmentBatchItem(owner, batchID, 0, 1)
	for name, run := range map[string]func() error{
		"empty": func() error {
			_, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, nil, nil, true)
			return err
		},
		"missing-batch": func() error {
			item := valid
			item.record.Batch = nil
			_, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, true)
			return err
		},
		"progress": func() error {
			_, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{valid}, transferSegmentLeases(0, 1), true)
			return err
		},
		"member": func() error {
			item := valid
			item.record.OwnerID = "other-owner"
			_, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, true)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	failure := domain.NewError(domain.ErrorUnavailable, "begin unavailable")
	engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, beginErr: failure}
	if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{valid}, nil, true); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("begin failure = %v", err)
	}
}

func TestEnsureUploadLeaseSegmentReconcilesEveryProviderPublicationOutcome(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	batchID := storageformat.Digest([]byte("lease-segment-reconciliation"))
	item := transferSegmentBatchItem(owner, batchID, 0, 1)
	failure := domain.NewError(domain.ErrorUnavailable, "lease segment provider unavailable")
	validSegment := storageformat.PortableUploadLeaseSegment{
		SchemaVersion: 1, BackendKind: "memory", OwnerID: owner.String(), BatchID: batchID,
		Segment: 0, TotalCount: 1, FirstIndex: 0, Leases: transferSegmentLeases(0, 1),
	}
	segmentBody, err := storageformat.EncodePortableUploadLeaseSegment(validSegment)
	if err != nil {
		t.Fatal(err)
	}
	segmentKey := storageformat.UploadLeaseSegmentKey("memory", batchID, 0)
	providerHandle := objectstore.UploadHandle{
		Capability: objectstore.UploadCapability{Protocol: domain.UploadSingle, URL: "http://127.0.0.1/upload", Method: "PUT", ExpiresAt: item.record.ExpiresAt},
		Lease:      []byte("provider-lease"),
	}
	resumeValue := objectstore.UploadCapability{Protocol: domain.UploadSingle, URL: "http://127.0.0.1/resume", Method: "PUT", ExpiresAt: item.record.ExpiresAt}

	newFixture := func(t *testing.T, state objectstore.Backend, transfers *transferFailureBackend) *FileStore {
		t.Helper()
		bootstrap := state
		if hooked, ok := state.(*hookedBackend); ok {
			bootstrap = hooked.Backend
		}
		engine := openNamespaceTestEngine(t, bootstrap)
		engine.backend = state
		engine.fileBackend = transfers
		return engine.Files()
	}
	newTransfers := func(base objectstore.Backend) *transferFailureBackend {
		memory := objectmemory.New()
		return &transferFailureBackend{Backend: base, transfers: memory, beginHandle: &providerHandle, resumeValue: &resumeValue, abortOK: true}
	}

	t.Run("corrupt-existing", func(t *testing.T) {
		base := objectmemory.New()
		if _, err := base.Put(ctx, segmentKey, []byte("corrupt"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		files := newFixture(t, base, newTransfers(base))
		if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, false); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("existing-total-mismatch", func(t *testing.T) {
		base := objectmemory.New()
		mismatch := validSegment
		mismatch.TotalCount = 2
		mismatch.Leases = transferSegmentLeases(0, 2)
		body, err := storageformat.EncodePortableUploadLeaseSegment(mismatch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := base.Put(ctx, segmentKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		files := newFixture(t, base, newTransfers(base))
		if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, false); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("existing-resume-failure", func(t *testing.T) {
		base := objectmemory.New()
		if _, err := base.Put(ctx, segmentKey, segmentBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		transfers := newTransfers(base)
		transfers.resumeValue, transfers.resumeErr = nil, failure
		files := newFixture(t, base, transfers)
		if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, false); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("existing-read-failure", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{Backend: base, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == segmentKey {
				return objectstore.Object{}, failure
			}
			return base.Get(ctx, key)
		}}
		files := newFixture(t, hooks, newTransfers(hooks))
		if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, false); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("oversized-provider-lease", func(t *testing.T) {
		base := objectmemory.New()
		transfers := newTransfers(base)
		oversized := providerHandle
		oversized.Lease = make([]byte, storageformat.MaxSealedUploadLeaseBytes+1)
		transfers.beginHandle = &oversized
		files := newFixture(t, base, transfers)
		if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, true); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	for name, hooks := range map[string]*hookedBackend{
		"publication": {
			Backend: objectmemory.New(),
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", failure
			},
		},
		"winner-read": {
			Backend: objectmemory.New(),
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "winner")
			},
			get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{}, failure
			},
		},
	} {
		t.Run(name+"-failure", func(t *testing.T) {
			files := newFixture(t, hooks, newTransfers(hooks))
			if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, true); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("corrupt-winner", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{
			Backend: base,
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "winner")
			},
			get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{Key: key, Body: []byte("corrupt"), Version: "winner"}, nil
			},
		}
		files := newFixture(t, hooks, newTransfers(hooks))
		if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, true); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("different-winner", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{
			Backend: base,
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "winner")
			},
			get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{Key: key, Body: segmentBody, Version: "winner"}, nil
			},
		}
		files := newFixture(t, hooks, newTransfers(hooks))
		capabilities, leases, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, true)
		if err != nil || len(capabilities) != 1 || len(leases) != 1 || string(leases[0].Lease) != "lease-00000" {
			t.Fatalf("winner capabilities=%+v leases=%+v error=%v", capabilities, leases, err)
		}
	})

	t.Run("winner-resume-failure", func(t *testing.T) {
		base := objectmemory.New()
		hooks := &hookedBackend{
			Backend: base,
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "winner")
			},
			get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{Key: key, Body: segmentBody, Version: "winner"}, nil
			},
		}
		transfers := newTransfers(hooks)
		transfers.resumeValue, transfers.resumeErr = nil, failure
		files := newFixture(t, hooks, transfers)
		if _, _, err := files.ensurePortableUploadLeaseSegment(ctx, owner, batchID, 0, []portableUploadBatchItem{item}, nil, true); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestSchema011UploadTransactionNormalizationAndProgressDenialMatrix(t *testing.T) {
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	crc := integrity.CRC32C([]byte("body"))
	validCompletion := domain.CompleteUploadBatchRequest{
		Items:          []domain.CompleteUploadBatchItem{{UploadID: "upload-a", CRC32C: crc}},
		IdempotencyKey: "completion-operation-key",
	}
	transactionID, fingerprint, err := normalizePortableUploadCompletionBatch(owner, validCompletion)
	if err != nil || !storageformat.ValidDigest(transactionID) || !storageformat.ValidDigest(fingerprint) {
		t.Fatalf("valid completion normalization = %q, %q, %v", transactionID, fingerprint, err)
	}
	completionTooLarge := validCompletion
	completionTooLarge.Items = make([]domain.CompleteUploadBatchItem, storageformat.MaxPortableUploadBatchItems+1)
	for name, run := range map[string]func() error{
		"owner": func() error {
			_, _, err := normalizePortableUploadCompletionBatch(domain.UserID{}, validCompletion)
			return err
		},
		"empty": func() error {
			_, _, err := normalizePortableUploadCompletionBatch(owner, domain.CompleteUploadBatchRequest{})
			return err
		},
		"too-large": func() error {
			_, _, err := normalizePortableUploadCompletionBatch(owner, completionTooLarge)
			return err
		},
		"idempotency": func() error {
			request := validCompletion
			request.IdempotencyKey = "invalid\n"
			_, _, err := normalizePortableUploadCompletionBatch(owner, request)
			return err
		},
		"empty-upload": func() error {
			request := validCompletion
			request.Items = []domain.CompleteUploadBatchItem{{CRC32C: crc}}
			_, _, err := normalizePortableUploadCompletionBatch(owner, request)
			return err
		},
		"duplicate": func() error {
			request := validCompletion
			request.Items = append(request.Items, request.Items[0])
			_, _, err := normalizePortableUploadCompletionBatch(owner, request)
			return err
		},
		"crc": func() error {
			request := validCompletion
			request.Items[0].CRC32C = "invalid"
			_, _, err := normalizePortableUploadCompletionBatch(owner, request)
			return err
		},
	} {
		t.Run("complete-"+name, func(t *testing.T) {
			if err := run(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	validAbort := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{"upload-a"}, IdempotencyKey: "abort-operation-key"}
	abortTransactionID, abortFingerprint, err := normalizePortableUploadAbortBatch(owner, validAbort)
	if err != nil || !storageformat.ValidDigest(abortTransactionID) || !storageformat.ValidDigest(abortFingerprint) {
		t.Fatalf("valid abort normalization = %q, %q, %v", abortTransactionID, abortFingerprint, err)
	}
	validAbort.BatchID = storageformat.Digest([]byte("admitted-batch"))
	boundTransactionID, _, err := normalizePortableUploadAbortBatch(owner, validAbort)
	if err != nil || boundTransactionID == abortTransactionID {
		t.Fatalf("batch-bound abort transaction = %q, %v", boundTransactionID, err)
	}
	abortTooLarge := validAbort
	abortTooLarge.UploadIDs = make([]domain.UploadID, storageformat.MaxPortableUploadBatchItems+1)
	for name, run := range map[string]func() error{
		"owner": func() error { _, _, err := normalizePortableUploadAbortBatch(domain.UserID{}, validAbort); return err },
		"empty": func() error {
			_, _, err := normalizePortableUploadAbortBatch(owner, domain.AbortUploadBatchRequest{})
			return err
		},
		"too-large": func() error { _, _, err := normalizePortableUploadAbortBatch(owner, abortTooLarge); return err },
		"idempotency": func() error {
			request := validAbort
			request.IdempotencyKey = "invalid\n"
			_, _, err := normalizePortableUploadAbortBatch(owner, request)
			return err
		},
		"batch": func() error {
			request := validAbort
			request.BatchID = "invalid"
			_, _, err := normalizePortableUploadAbortBatch(owner, request)
			return err
		},
		"empty-upload": func() error {
			request := validAbort
			request.UploadIDs = []domain.UploadID{""}
			_, _, err := normalizePortableUploadAbortBatch(owner, request)
			return err
		},
		"duplicate": func() error {
			request := validAbort
			request.UploadIDs = []domain.UploadID{"same", "same"}
			request.BatchID = ""
			_, _, err := normalizePortableUploadAbortBatch(owner, request)
			return err
		},
	} {
		t.Run("abort-"+name, func(t *testing.T) {
			if err := run(); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	completionRequest := domain.CompleteUploadBatchRequest{Items: make([]domain.CompleteUploadBatchItem, storageformat.UploadTransactionSegmentItems+1)}
	completionProgress := storageformat.UploadTransactionSegment{Items: make([]storageformat.UploadTransactionSegmentItem, storageformat.UploadTransactionSegmentItems)}
	for index := range completionProgress.Items {
		uploadID := domain.UploadID(fmt.Sprintf("complete-%04d", index))
		completionRequest.Items[index] = domain.CompleteUploadBatchItem{UploadID: uploadID, CRC32C: crc}
		completionProgress.Items[index] = storageformat.UploadTransactionSegmentItem{Index: uint64(index), UploadID: string(uploadID), MD5: integrity.MD5([]byte{byte(index)}), CRC32C: crc}
	}
	completionRequest.Items[len(completionRequest.Items)-1] = domain.CompleteUploadBatchItem{UploadID: "complete-tail", CRC32C: crc}
	if err := validateCompletionProgress(completionProgress, completionRequest); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*storageformat.UploadTransactionSegment){
		"non-boundary": func(value *storageformat.UploadTransactionSegment) { value.Items = value.Items[:1] },
		"terminal": func(value *storageformat.UploadTransactionSegment) {
			value.Items = append(value.Items, storageformat.UploadTransactionSegmentItem{})
		},
		"index":  func(value *storageformat.UploadTransactionSegment) { value.Items[0].Index++ },
		"upload": func(value *storageformat.UploadTransactionSegment) { value.Items[0].UploadID = "other" },
		"crc": func(value *storageformat.UploadTransactionSegment) {
			value.Items[0].CRC32C = integrity.CRC32C([]byte("other"))
		},
	} {
		t.Run("completion-progress-"+name, func(t *testing.T) {
			candidate := completionProgress
			candidate.Items = append([]storageformat.UploadTransactionSegmentItem(nil), completionProgress.Items...)
			mutate(&candidate)
			if err := validateCompletionProgress(candidate, completionRequest); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	abortRequest := domain.AbortUploadBatchRequest{UploadIDs: make([]domain.UploadID, storageformat.UploadTransactionSegmentItems+1)}
	abortProgress := storageformat.UploadTransactionSegment{Items: make([]storageformat.UploadTransactionSegmentItem, storageformat.UploadTransactionSegmentItems)}
	for index := range abortProgress.Items {
		uploadID := domain.UploadID(fmt.Sprintf("abort-%04d", index))
		abortRequest.UploadIDs[index] = uploadID
		abortProgress.Items[index] = storageformat.UploadTransactionSegmentItem{Index: uint64(index), UploadID: string(uploadID)}
	}
	abortRequest.UploadIDs[len(abortRequest.UploadIDs)-1] = "abort-tail"
	if err := validateAbortProgress(abortProgress, abortRequest); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*storageformat.UploadTransactionSegment){
		"non-boundary": func(value *storageformat.UploadTransactionSegment) { value.Items = value.Items[:1] },
		"terminal": func(value *storageformat.UploadTransactionSegment) {
			value.Items = append(value.Items, storageformat.UploadTransactionSegmentItem{})
		},
		"index":  func(value *storageformat.UploadTransactionSegment) { value.Items[0].Index++ },
		"upload": func(value *storageformat.UploadTransactionSegment) { value.Items[0].UploadID = "other" },
		"md5":    func(value *storageformat.UploadTransactionSegment) { value.Items[0].MD5 = integrity.MD5(nil) },
		"crc":    func(value *storageformat.UploadTransactionSegment) { value.Items[0].CRC32C = crc },
	} {
		t.Run("abort-progress-"+name, func(t *testing.T) {
			candidate := abortProgress
			candidate.Items = append([]storageformat.UploadTransactionSegmentItem(nil), abortProgress.Items...)
			mutate(&candidate)
			if err := validateAbortProgress(candidate, abortRequest); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	prefix := completionProgress.Items[:2]
	if !uploadProgressPrefixEqual(completionProgress.Items, prefix) || uploadProgressPrefixEqual(prefix, completionProgress.Items) {
		t.Fatal("upload transaction prefix cardinality was not enforced")
	}
	changedPrefix := append([]storageformat.UploadTransactionSegmentItem(nil), prefix...)
	changedPrefix[0].UploadID = "different"
	if uploadProgressPrefixEqual(completionProgress.Items, changedPrefix) {
		t.Fatal("divergent upload transaction prefix was accepted")
	}
}

func TestSchema011UploadTransactionObjectsConvergeAndFailClosed(t *testing.T) {
	ctx := context.Background()
	backend := objectmemory.New()
	engine := openNamespaceTestEngine(t, backend)
	files := engine.Files()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	transactionID := storageformat.Digest([]byte("transaction-progress"))
	fingerprint := storageformat.Digest([]byte("transaction-fingerprint"))
	crc := integrity.CRC32C([]byte("body"))
	progress := storageformat.UploadTransactionSegment{
		SchemaVersion: 1, BackendKind: backend.BackendKind(), OwnerID: owner.String(), TransactionID: transactionID,
		RequestFingerprint: fingerprint, Kind: "complete", Segment: 1,
		Items: make([]storageformat.UploadTransactionSegmentItem, storageformat.UploadTransactionSegmentItems),
	}
	for index := range progress.Items {
		progress.Items[index] = storageformat.UploadTransactionSegmentItem{Index: uint64(index), UploadID: fmt.Sprintf("upload-%04d", index), MD5: integrity.MD5([]byte{byte(index)}), CRC32C: crc}
	}
	missing, err := files.absentUploadTransactionProgress(transactionID)
	if err != nil || missing.Key != storageformat.UploadTransactionProgressKey(backend.BackendKind(), transactionID) || missing.Version != "" {
		t.Fatalf("absent progress = %+v, %v", missing, err)
	}
	if _, _, err := files.readUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing progress error = %v", err)
	}
	written, object, err := files.publishUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete", progress, missing)
	if err != nil || len(written.Items) != len(progress.Items) || object.Version == "" {
		t.Fatalf("published progress = %d, %+v, %v", len(written.Items), object, err)
	}
	read, readObject, err := files.readUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete")
	if err != nil || len(read.Items) != len(progress.Items) || readObject.Version != object.Version {
		t.Fatalf("read progress = %d, %+v, %v", len(read.Items), readObject, err)
	}
	updated, updatedObject, err := files.publishUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete", progress, object)
	if err != nil || len(updated.Items) != len(progress.Items) || updatedObject.Version == object.Version {
		t.Fatalf("updated progress = %d, %+v, %v", len(updated.Items), updatedObject, err)
	}
	stale := object
	winner, winnerObject, err := files.publishUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete", progress, stale)
	if err != nil || len(winner.Items) != len(progress.Items) || winnerObject.Version != updatedObject.Version {
		t.Fatalf("lost-success winner = %d, %+v, %v", len(winner.Items), winnerObject, err)
	}
	divergent := progress
	divergent.Items = append([]storageformat.UploadTransactionSegmentItem(nil), progress.Items...)
	divergent.Items[0].UploadID = "divergent"
	if _, _, err := files.publishUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete", divergent, stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("divergent winner error = %v", err)
	}
	key := storageformat.UploadTransactionProgressKey(backend.BackendKind(), storageformat.Digest([]byte("corrupt-progress")))
	if _, err := backend.Put(ctx, key, []byte("corrupt"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.readUploadTransactionProgress(ctx, owner, storageformat.Digest([]byte("corrupt-progress")), fingerprint, "complete"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt progress error = %v", err)
	}

	failure := domain.NewError(domain.ErrorUnavailable, "transaction transport unavailable")
	engine.backend = &hookedBackend{Backend: backend,
		put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
			return "", failure
		},
		get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, domain.NewError(domain.ErrorNotFound, "missing winner")
		},
	}
	missing.Key = storageformat.UploadTransactionProgressKey(backend.BackendKind(), storageformat.Digest([]byte("failed-progress")))
	if _, _, err := files.publishUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete", progress, missing); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("missing winner error = %v", err)
	}
	engine.fileBackend = metadataOnlyBackend{Backend: backend}
	if _, err := files.absentUploadTransactionProgress(transactionID); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("missing transfer backend error = %v", err)
	}
}

func TestSchema011UploadVerificationLeaseAndAbortFailureMatrix(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	body := []byte("x")
	crc := integrity.CRC32C(body)
	md5 := integrity.MD5(body)

	t.Run("completion-entries", func(t *testing.T) {
		valid := portableUploadTransactionItem{record: storageformat.PortableUploadRecord{
			ResolvedPath: "/complete.bin", BlobID: "blob", Size: 1, MediaType: "application/octet-stream",
			Completion: &storageformat.PortableUploadCompletion{MD5: md5, CRC32C: crc, ModifiedAt: time.Date(2071, 2, 3, 4, 5, 6, 0, time.UTC)},
		}}
		entries, err := completionEntriesFromRecords([]portableUploadTransactionItem{valid})
		if err != nil || len(entries) != 1 || entries[0].Path.String() != "/complete.bin" {
			t.Fatalf("entries = %+v, %v", entries, err)
		}
		missing := valid
		missing.record.Completion = nil
		if _, err := completionEntriesFromRecords([]portableUploadTransactionItem{missing}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("missing completion error = %v", err)
		}
		root := valid
		root.record.ResolvedPath = "/"
		if _, err := completionEntriesFromRecords([]portableUploadTransactionItem{root}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("root completion error = %v", err)
		}
	})

	t.Run("verification", func(t *testing.T) {
		backend := objectmemory.New()
		key := storageformat.BlobKey(owner.String(), "verified-blob")
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		engine := openNamespaceTestEngine(t, backend)
		item := portableUploadTransactionItem{crc32c: crc, record: storageformat.PortableUploadRecord{OwnerID: owner.String(), BlobID: "verified-blob", Size: 1}}
		if err := engine.Files().verifyPortableUploadRange(ctx, []portableUploadTransactionItem{item}, 0, 1); err != nil {
			t.Fatal(err)
		}
		failure := domain.NewError(domain.ErrorUnavailable, "verify unavailable")
		engine.fileBackend = &hookedBackend{Backend: backend, verify: func(context.Context, objectstore.Key, objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
			return objectstore.ObjectInfo{}, failure
		}}
		if err := engine.Files().verifyPortableUploadRange(ctx, []portableUploadTransactionItem{item}, 0, 1); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("verify failure = %v", err)
		}
		engine.fileBackend = &hookedBackend{Backend: backend, verify: func(context.Context, objectstore.Key, objectstore.ExpectedIntegrity) (objectstore.ObjectInfo, error) {
			return objectstore.ObjectInfo{Size: 1, Fingerprint: objectstore.ContentFingerprint{CRC32C: crc}}, nil
		}}
		if err := engine.Files().verifyPortableUploadRange(ctx, []portableUploadTransactionItem{item}, 0, 1); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("incomplete fingerprint error = %v", err)
		}
	})

	t.Run("runtime-leases", func(t *testing.T) {
		newFiles := func(t *testing.T) (*objectmemory.Backend, *FileStore) {
			t.Helper()
			backend := objectmemory.New()
			return backend, openNamespaceTestEngine(t, backend).Files()
		}
		backend, files := newFiles(t)
		standalone := portableUploadTransactionItem{uploadID: "standalone", record: storageformat.PortableUploadRecord{UploadID: "standalone"}}
		if _, err := backend.Put(ctx, storageformat.LeaseKey(backend.BackendKind(), "standalone"), []byte("standalone-lease"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		leases, err := files.runtimeUploadLeasesForRange(ctx, []portableUploadTransactionItem{standalone}, 0, 1)
		if err != nil || len(leases) != 1 || string(leases[0]) != "standalone-lease" {
			t.Fatalf("standalone leases = %q, %v", leases, err)
		}

		batchID := storageformat.Digest([]byte("range-batch"))
		putTransferSegment(t, backend, storageformat.PortableUploadLeaseSegment{
			SchemaVersion: 1, BackendKind: backend.BackendKind(), OwnerID: owner.String(), BatchID: batchID,
			Segment: 0, TotalCount: 2, FirstIndex: 0, Leases: transferSegmentLeases(0, 2),
		})
		items := []portableUploadTransactionItem{{record: transferSegmentBatchItem(owner, batchID, 0, 2).record}, {record: transferSegmentBatchItem(owner, batchID, 1, 2).record}}
		leases, err = files.runtimeUploadLeasesForRange(ctx, items, 0, len(items))
		if err != nil || string(leases[0]) != "lease-00000" || string(leases[1]) != "lease-00001" {
			t.Fatalf("batch leases = %q, %v", leases, err)
		}

		partialBackend, partialFiles := newFiles(t)
		partialBatch := storageformat.Digest([]byte("range-partial"))
		putTransferSegment(t, partialBackend, storageformat.PortableUploadLeaseSegment{
			SchemaVersion: 1, BackendKind: partialBackend.BackendKind(), OwnerID: owner.String(), BatchID: partialBatch,
			Segment: 0, TotalCount: 1_500, FirstIndex: 0, Leases: transferSegmentLeases(0, storageformat.MaxUploadLeaseSegmentItems),
		})
		partial := portableUploadTransactionItem{record: transferSegmentBatchItem(owner, partialBatch, 4, 1_500).record}
		if leases, err := partialFiles.runtimeUploadLeasesForRange(ctx, []portableUploadTransactionItem{partial}, 0, 1); err != nil || string(leases[0]) != "lease-00004" {
			t.Fatalf("partial leases = %q, %v", leases, err)
		}

		for name, mutate := range map[string]func(*storageformat.PortableUploadLeaseSegment, *portableUploadTransactionItem){
			"total": func(segment *storageformat.PortableUploadLeaseSegment, _ *portableUploadTransactionItem) {
				segment.TotalCount++
			},
			"first": func(segment *storageformat.PortableUploadLeaseSegment, _ *portableUploadTransactionItem) {
				segment.FirstIndex = 1
			},
			"member": func(segment *storageformat.PortableUploadLeaseSegment, _ *portableUploadTransactionItem) {
				segment.Leases[0].UploadID = "other"
			},
		} {
			t.Run(name, func(t *testing.T) {
				candidateBackend, candidateFiles := newFiles(t)
				candidateBatch := storageformat.Digest([]byte("range-invalid-" + name))
				segment := storageformat.PortableUploadLeaseSegment{
					SchemaVersion: 1, BackendKind: candidateBackend.BackendKind(), OwnerID: owner.String(), BatchID: candidateBatch,
					Segment: 0, TotalCount: 1, FirstIndex: 0, Leases: transferSegmentLeases(0, 1),
				}
				item := portableUploadTransactionItem{record: transferSegmentBatchItem(owner, candidateBatch, 0, 1).record}
				mutate(&segment, &item)
				body, encodeErr := storageformat.EncodePortableUploadLeaseSegment(segment)
				if encodeErr != nil {
					// Persist a valid envelope and misbind the requesting record when the
					// canonical encoder correctly rejects the corrupt authority itself.
					segment = storageformat.PortableUploadLeaseSegment{SchemaVersion: 1, BackendKind: candidateBackend.BackendKind(), OwnerID: owner.String(), BatchID: candidateBatch, Segment: 0, TotalCount: 1, FirstIndex: 0, Leases: transferSegmentLeases(0, 1)}
					body, encodeErr = storageformat.EncodePortableUploadLeaseSegment(segment)
					if name == "total" {
						item.record.Batch.Count++
					} else if name == "first" {
						item.record.Batch.Index++
					} else {
						item.record.UploadID = "other"
					}
				}
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				key := storageformat.UploadLeaseSegmentKey(candidateBackend.BackendKind(), candidateBatch, 0)
				if _, err := candidateBackend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
					t.Fatal(err)
				}
				if _, err := candidateFiles.runtimeUploadLeasesForRange(ctx, []portableUploadTransactionItem{item}, 0, 1); !errors.Is(err, domain.ErrInvalid) && !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("error = %v", err)
				}
			})
		}
	})

	t.Run("abort-provider", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		leases := [][]byte{[]byte("first"), []byte("second")}
		engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, abortErr: domain.NewError(domain.ErrorNotFound, "already absent")}
		if err := engine.Files().abortPortableUploadRange(ctx, leases, 0, len(leases)); err != nil {
			t.Fatalf("not-found abort = %v", err)
		}
		failure := domain.NewError(domain.ErrorUnavailable, "abort unavailable")
		engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, abortErr: failure}
		if err := engine.Files().abortPortableUploadRange(ctx, leases, 0, len(leases)); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("abort failure = %v", err)
		}
		engine.fileBackend = metadataOnlyBackend{Backend: backend}
		if err := engine.Files().abortPortableUploadRange(ctx, leases, 0, len(leases)); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("transfer backend failure = %v", err)
		}
	})
}

type schema011TransferFixture struct {
	backend *objectmemory.Backend
	engine  *Engine
	scope   domain.Scope
	clock   *domain.FixedClock
}

func newSchema011TransferFixture(t *testing.T) schema011TransferFixture {
	t.Helper()
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2072, 3, 4, 5, 6, 7, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x6d}, 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<14)))
	return schema011TransferFixture{backend: backend, engine: engine, scope: namespaceTestScope(t, domain.AreaLive), clock: clock}
}

func (fixture schema011TransferFixture) createBatch(t *testing.T, paths ...string) []domain.UploadCapability {
	t.Helper()
	requests := make([]domain.CreateUploadRequest, len(paths))
	for index, path := range paths {
		requests[index] = domain.CreateUploadRequest{
			Path: domain.MustParseUserPath(path), Size: 1, MediaType: "application/octet-stream",
			IdempotencyKey: fmt.Sprintf("fixture-upload-%03d-%s", index, storageformat.Digest([]byte(path))[:16]),
		}
	}
	capabilities, err := fixture.engine.Files().CreateUploadBatch(context.Background(), fixture.scope, requests)
	if err != nil {
		t.Fatal(err)
	}
	return capabilities
}

func schema011CompletionRequest(capabilities []domain.UploadCapability, idempotency string) domain.CompleteUploadBatchRequest {
	items := make([]domain.CompleteUploadBatchItem, len(capabilities))
	for index, capability := range capabilities {
		items[index] = domain.CompleteUploadBatchItem{UploadID: capability.UploadID, CRC32C: integrity.CRC32C([]byte("x"))}
	}
	return domain.CompleteUploadBatchRequest{Items: items, IdempotencyKey: idempotency}
}

func useSchema011VerifiedMetadata(fixture schema011TransferFixture) {
	info := objectstore.ObjectInfo{Size: 1, Fingerprint: objectstore.ContentFingerprint{MD5: integrity.MD5([]byte("x")), CRC32C: integrity.CRC32C([]byte("x"))}}
	fixture.engine.fileBackend = &transferFailureBackend{Backend: fixture.backend, transfers: fixture.backend, verifyInfo: &info}
}

func schema011BatchPaths(count int, prefix string) []string {
	paths := make([]string, count)
	for index := range paths {
		paths[index] = fmt.Sprintf("/%s-%05d.bin", prefix, index)
	}
	return paths
}

func TestSchema011SegmentedUploadCompletionResumesAfterDurableProgress(t *testing.T) {
	fixture := newSchema011TransferFixture(t)
	capabilities := fixture.createBatch(t, schema011BatchPaths(storageformat.UploadTransactionSegmentItems+1, "segmented-completion")...)
	useSchema011VerifiedMetadata(fixture)
	failure := domain.NewError(domain.ErrorUnavailable, "completion worker stopped after durable progress")
	fixture.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepUploadBatchCompletionProgress {
			return failure
		}
		return nil
	})
	request := schema011CompletionRequest(capabilities, "segmented-completion-operation")
	if _, err := fixture.engine.Files().CompleteUploadBatch(context.Background(), fixture.scope, request); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted completion error = %v", err)
	}
	fixture.engine.scheduler = nil
	result, err := fixture.engine.Files().CompleteUploadBatch(context.Background(), fixture.scope, request)
	if err != nil || len(result.Entries) != len(capabilities) {
		t.Fatalf("resumed completion entries = %d, error = %v", len(result.Entries), err)
	}
	replayed, err := fixture.engine.Files().CompleteUploadBatch(context.Background(), fixture.scope, request)
	if err != nil || len(replayed.Entries) != len(capabilities) {
		t.Fatalf("replayed completion entries = %d, error = %v", len(replayed.Entries), err)
	}
}

func TestSchema011SegmentedUploadAbortResumesAfterDurableProgress(t *testing.T) {
	fixture := newSchema011TransferFixture(t)
	capabilities := fixture.createBatch(t, schema011BatchPaths(storageformat.UploadTransactionSegmentItems+1, "segmented-abort")...)
	failure := domain.NewError(domain.ErrorUnavailable, "abort worker stopped after durable progress")
	fixture.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
		if step == StepUploadBatchAbortProgress {
			return failure
		}
		return nil
	})
	uploadIDs := make([]domain.UploadID, len(capabilities))
	for index := range capabilities {
		uploadIDs[index] = capabilities[index].UploadID
	}
	request := domain.AbortUploadBatchRequest{
		UploadIDs: uploadIDs, BatchID: capabilities[0].BatchID, IdempotencyKey: "segmented-abort-operation",
	}
	if err := fixture.engine.Files().AbortUploadBatch(context.Background(), fixture.scope, request); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("interrupted abort error = %v", err)
	}
	fixture.engine.scheduler = nil
	if err := fixture.engine.Files().AbortUploadBatch(context.Background(), fixture.scope, request); err != nil {
		t.Fatalf("resumed abort error = %v", err)
	}
	if err := fixture.engine.Files().AbortUploadBatch(context.Background(), fixture.scope, request); err != nil {
		t.Fatalf("replayed abort error = %v", err)
	}
}

func TestSchema011UploadCompletionRejectsEveryAuthorityAndProviderGap(t *testing.T) {
	ctx := context.Background()
	invalid := newSchema011TransferFixture(t)
	if _, err := invalid.engine.Files().CompleteUploadBatch(ctx, domain.Scope{}, domain.CompleteUploadBatchRequest{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid scope error = %v", err)
	}
	if _, err := invalid.engine.Files().completeUploadBatch011(ctx, invalid.scope, domain.CompleteUploadBatchRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid request error = %v", err)
	}

	t.Run("missing", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		request := domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{{UploadID: "missing", CRC32C: integrity.CRC32C(nil)}}, IdempotencyKey: "missing-completion-operation"}
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("state-read", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/state-read.bin")
		failure := domain.NewError(domain.ErrorUnavailable, "state read unavailable")
		fixture.engine.backend = &hookedBackend{Backend: fixture.backend, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, failure
		}}
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "state-read-completion")); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("transfer-backend", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/transfer-backend.bin")
		fixture.engine.fileBackend = metadataOnlyBackend{Backend: fixture.backend}
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "transfer-backend-completion")); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("wrong-area", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/wrong-area.bin")
		trash, err := domain.NewScope(fixture.scope.UserID(), domain.AreaTrash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, trash, schema011CompletionRequest(capabilities, "wrong-area-completion")); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/expired.bin")
		fixture.clock.Advance(2 * time.Hour)
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "expired-completion")); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("verify-error", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/verify-error.bin")
		failure := domain.NewError(domain.ErrorUnavailable, "verification unavailable")
		fixture.engine.fileBackend = &transferFailureBackend{Backend: fixture.backend, transfers: fixture.backend, verifyErr: failure}
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "verify-error-completion")); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("verify-incomplete", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/verify-incomplete.bin")
		info := objectstore.ObjectInfo{Size: 1, Fingerprint: objectstore.ContentFingerprint{CRC32C: integrity.CRC32C([]byte("x"))}}
		fixture.engine.fileBackend = &transferFailureBackend{Backend: fixture.backend, transfers: fixture.backend, verifyInfo: &info}
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "verify-incomplete-completion")); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("destination-appeared", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/appeared.bin")
		if _, err := fixture.engine.Files().CreateDirectory(ctx, fixture.scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/appeared.bin")}); err != nil {
			t.Fatal(err)
		}
		useSchema011VerifiedMetadata(fixture)
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "appeared-completion")); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("destination-changed", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		path := domain.MustParseUserPath("/changed.bin")
		original, err := fixture.engine.Files().CreateDirectory(ctx, fixture.scope, domain.CreateDirectoryRequest{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		capabilities, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, []domain.CreateUploadRequest{{
			Path: path, Size: 1, MediaType: "application/octet-stream", Conflict: domain.ConflictReplace,
			ExpectedVersion: original.Version, IdempotencyKey: "changed-upload-item",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.engine.Files().CreateDirectory(ctx, fixture.scope, domain.CreateDirectoryRequest{Path: path, Conflict: domain.ConflictReplace, ExpectedVersion: original.Version}); err != nil {
			t.Fatal(err)
		}
		useSchema011VerifiedMetadata(fixture)
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "changed-completion")); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("duplicate-destination", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/duplicate-a.bin", "/duplicate-b.bin")
		store := newNamespaceStore(fixture.engine)
		view, err := store.loadView(ctx, fixture.scope.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		record, value, err := fixture.engine.Files().portableUploadAtView(ctx, view, fixture.scope.UserID(), string(capabilities[1].UploadID))
		if err != nil {
			t.Fatal(err)
		}
		record.ResolvedPath = "/duplicate-a.bin"
		body, err := storageformat.EncodeCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(fixture.scope.UserID()), consistencyDomainMutation{
			ID: "duplicate-upload-destination", Changes: []consistencyDomainChange{{Key: uploadRecordKey(string(capabilities[1].UploadID)), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}},
		}, view.headSnapshot, view.session); err != nil {
			t.Fatal(err)
		}
		useSchema011VerifiedMetadata(fixture)
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "duplicate-destination-completion")); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing-parent", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/original-parent.bin")
		store := newNamespaceStore(fixture.engine)
		view, err := store.loadView(ctx, fixture.scope.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		record, value, err := fixture.engine.Files().portableUploadAtView(ctx, view, fixture.scope.UserID(), string(capabilities[0].UploadID))
		if err != nil {
			t.Fatal(err)
		}
		record.ResolvedPath = "/missing-parent/file.bin"
		body, err := storageformat.EncodeCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(fixture.scope.UserID()), consistencyDomainMutation{
			ID: "missing-upload-parent", Changes: []consistencyDomainChange{{Key: uploadRecordKey(string(capabilities[0].UploadID)), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}},
		}, view.headSnapshot, view.session); err != nil {
			t.Fatal(err)
		}
		useSchema011VerifiedMetadata(fixture)
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "missing-parent-completion")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("replay-missing-record", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/replay-missing.bin")
		useSchema011VerifiedMetadata(fixture)
		request := schema011CompletionRequest(capabilities, "replay-missing-completion")
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, request); err != nil {
			t.Fatal(err)
		}
		store := newNamespaceStore(fixture.engine)
		view, err := store.loadView(ctx, fixture.scope.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		_, value, err := fixture.engine.Files().portableUploadAtView(ctx, view, fixture.scope.UserID(), string(capabilities[0].UploadID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(fixture.scope.UserID()), consistencyDomainMutation{
			ID: "delete-completed-upload-record", Changes: []consistencyDomainChange{{Key: uploadRecordKey(string(capabilities[0].UploadID)), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Delete: true}},
		}, view.headSnapshot, view.session); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("lost-response-after-publication", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/completion-lost-response.bin")
		useSchema011VerifiedMetadata(fixture)
		failure := domain.NewError(domain.ErrorUnavailable, "completion response lost")
		fixture.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
			if step == StepUploadBatchCompletionPublished {
				return failure
			}
			return nil
		})
		request := schema011CompletionRequest(capabilities, "completion-lost-response")
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("lost response error = %v", err)
		}
		fixture.engine.scheduler = nil
		result, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, request)
		if err != nil || len(result.Entries) != 1 {
			t.Fatalf("recovered completion = %+v, %v", result, err)
		}
	})
}

func TestSchema011UploadAbortRejectsEveryAuthorityAndProviderGap(t *testing.T) {
	ctx := context.Background()
	invalid := newSchema011TransferFixture(t)
	if err := invalid.engine.Files().AbortUploadBatch(ctx, domain.Scope{}, domain.AbortUploadBatchRequest{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid scope error = %v", err)
	}
	if err := invalid.engine.Files().abortUploadBatch011(ctx, invalid.scope, domain.AbortUploadBatchRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid request error = %v", err)
	}
	if _, err := invalid.engine.Files().CreateUploadBatch(ctx, domain.Scope{}, nil); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid admission scope error = %v", err)
	}

	t.Run("missing", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{"missing"}, IdempotencyKey: "missing-abort-operation"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("state-read", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/abort-state-read.bin")
		failure := domain.NewError(domain.ErrorUnavailable, "state read unavailable")
		fixture.engine.backend = &hookedBackend{Backend: fixture.backend, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, failure
		}}
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "abort-state-read"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("transfer-backend", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/abort-transfer-backend.bin")
		fixture.engine.fileBackend = metadataOnlyBackend{Backend: fixture.backend}
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "abort-transfer-backend"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("incomplete-batch", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/subset-a.bin", "/subset-b.bin")
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "subset-abort-operation"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing-lease-segment", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/missing-lease.bin")
		key := storageformat.UploadLeaseSegmentKey(fixture.backend.BackendKind(), capabilities[0].BatchID, 0)
		object, err := fixture.backend.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: object.Version}); err != nil {
			t.Fatal(err)
		}
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "missing-lease-abort"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("corrupt-lease-segment", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/corrupt-lease.bin")
		key := storageformat.UploadLeaseSegmentKey(fixture.backend.BackendKind(), capabilities[0].BatchID, 0)
		object, err := fixture.backend.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.backend.Put(ctx, key, []byte("corrupt"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			t.Fatal(err)
		}
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "corrupt-lease-abort"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("provider-failure", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/provider-abort.bin")
		failure := domain.NewError(domain.ErrorUnavailable, "provider abort unavailable")
		fixture.engine.fileBackend = &transferFailureBackend{Backend: fixture.backend, transfers: fixture.backend, abortErr: failure}
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "provider-failure-abort"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("terminal-legacy", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/terminal-abort.bin")
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, IdempotencyKey: "first-terminal-abort"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); err != nil {
			t.Fatal(err)
		}
		request.IdempotencyKey = "second-terminal-abort"
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("lost-response-after-publication", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/abort-lost-response.bin")
		failure := domain.NewError(domain.ErrorUnavailable, "abort response lost")
		fixture.engine.scheduler = SchedulerFunc(func(_ context.Context, step string) error {
			if step == StepUploadBatchAbortPublished {
				return failure
			}
			return nil
		})
		request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "abort-lost-response"}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("lost response error = %v", err)
		}
		fixture.engine.scheduler = nil
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); err != nil {
			t.Fatalf("recovered abort = %v", err)
		}
	})
}

func TestSchema011SegmentedTerminalCleanupIsIdempotentAndFailClosed(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	newRecord := func(engine *Engine, uploadID string, state storageformat.UploadState) storageformat.PortableUploadRecord {
		record := checkpointUploadRecord(engine, owner, uploadID, engine.clock.Now().Add(time.Hour))
		record.State, record.CleanupPending = state, true
		record.Batch = &storageformat.PortableUploadBatchMember{BatchID: storageformat.Digest([]byte("cleanup-" + uploadID)), Index: 0, Count: 1}
		return record
	}

	t.Run("completed-shared-segment", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		record := newRecord(engine, "completed-segmented", storageformat.UploadCompleted)
		seedCheckpointUploadRecord(t, engine, owner, record)
		if err := engine.Files().cleanupPortableUpload(ctx, owner, record.UploadID, nil); err != nil {
			t.Fatal(err)
		}
		stored, _, err := engine.Files().portableUpload(ctx, owner, record.UploadID)
		if err != nil || stored.CleanupPending {
			t.Fatalf("cleanup state = %+v, %v", stored, err)
		}
	})

	t.Run("aborted-missing-segment", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		record := newRecord(engine, "aborted-missing-segment", storageformat.UploadAborted)
		seedCheckpointUploadRecord(t, engine, owner, record)
		if err := engine.Files().cleanupPortableUpload(ctx, owner, record.UploadID, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("aborted-provider-not-found", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		record := newRecord(engine, "aborted-provider-absent", storageformat.UploadAborted)
		seedCheckpointUploadRecord(t, engine, owner, record)
		putTransferSegment(t, backend, storageformat.PortableUploadLeaseSegment{
			SchemaVersion: 1, BackendKind: backend.BackendKind(), OwnerID: owner.String(), BatchID: record.Batch.BatchID,
			Segment: 0, TotalCount: 1, FirstIndex: 0,
			Leases: []storageformat.PortableUploadLease{{Index: 0, UploadID: record.UploadID, Lease: []byte("sealed")}},
		})
		engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, abortErr: domain.NewError(domain.ErrorNotFound, "already absent")}
		if err := engine.Files().cleanupPortableUpload(ctx, owner, record.UploadID, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("aborted-provider-failure", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		record := newRecord(engine, "aborted-provider-failure", storageformat.UploadAborted)
		seedCheckpointUploadRecord(t, engine, owner, record)
		putTransferSegment(t, backend, storageformat.PortableUploadLeaseSegment{
			SchemaVersion: 1, BackendKind: backend.BackendKind(), OwnerID: owner.String(), BatchID: record.Batch.BatchID,
			Segment: 0, TotalCount: 1, FirstIndex: 0,
			Leases: []storageformat.PortableUploadLease{{Index: 0, UploadID: record.UploadID, Lease: []byte("sealed")}},
		})
		failure := domain.NewError(domain.ErrorUnavailable, "abort unavailable")
		engine.fileBackend = &transferFailureBackend{Backend: backend, transfers: backend, abortErr: failure}
		if err := engine.Files().cleanupPortableUpload(ctx, owner, record.UploadID, nil); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("non-terminal", func(t *testing.T) {
		backend := objectmemory.New()
		engine := openNamespaceTestEngine(t, backend)
		record := newRecord(engine, "active-cleanup", storageformat.UploadActive)
		seedCheckpointUploadRecord(t, engine, owner, record)
		if err := engine.Files().cleanupPortableUpload(ctx, owner, record.UploadID, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestPortableTransferMetadataBoundariesFailClosedWithoutObjectBodies(t *testing.T) {
	ctx := context.Background()
	owner := namespaceTestScope(t, domain.AreaLive).UserID()
	failure := domain.NewError(domain.ErrorUnavailable, "metadata authority unavailable")

	t.Run("lookup-and-progress-provider-failures", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		files := engine.Files()
		engine.backend = &hookedBackend{Backend: base, get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
			return objectstore.Object{}, failure
		}}
		if _, _, err := files.portableUpload(ctx, owner, "missing"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("portable upload lookup error = %v", err)
		}
		transactionID := storageformat.Digest([]byte("metadata-read-failure"))
		fingerprint := storageformat.Digest([]byte("metadata-read-fingerprint"))
		if _, _, err := files.readUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("progress read error = %v", err)
		}

		engine.backend = base
		engine.fileBackend = metadataOnlyBackend{Backend: base}
		if _, _, err := files.readUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete"); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("progress transfer backend error = %v", err)
		}
		batched := checkpointUploadRecord(engine, owner, "metadata-batch", engine.clock.Now().Add(time.Hour))
		batched.Batch = &storageformat.PortableUploadBatchMember{BatchID: storageformat.Digest([]byte("metadata-batch")), Index: 0, Count: 1}
		if _, _, err := files.runtimeUploadLeaseForRecord(ctx, batched); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("record lease transfer backend error = %v", err)
		}
		if _, err := files.runtimeUploadLeasesForRange(ctx, []portableUploadTransactionItem{{record: batched}}, 0, 1); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("range lease transfer backend error = %v", err)
		}
	})

	t.Run("progress-publication-denials", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		files := engine.Files()
		transactionID := storageformat.Digest([]byte("progress-publication"))
		fingerprint := storageformat.Digest([]byte("progress-publication-fingerprint"))
		key := storageformat.UploadTransactionProgressKey(base.BackendKind(), transactionID)
		if _, _, err := files.publishUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete", storageformat.UploadTransactionSegment{}, objectstore.Object{Key: key}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid progress publication error = %v", err)
		}
		progress := storageformat.UploadTransactionSegment{
			SchemaVersion: 1, BackendKind: base.BackendKind(), OwnerID: owner.String(), TransactionID: transactionID,
			RequestFingerprint: fingerprint, Kind: "complete", Segment: 1,
			Items: []storageformat.UploadTransactionSegmentItem{{Index: 0, UploadID: "upload", MD5: integrity.MD5(nil), CRC32C: integrity.CRC32C(nil)}},
		}
		engine.backend = &hookedBackend{
			Backend: base,
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "competing progress")
			},
			get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
				return objectstore.Object{}, failure
			},
		}
		if _, _, err := files.publishUploadTransactionProgress(ctx, owner, transactionID, fingerprint, "complete", progress, objectstore.Object{Key: key}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("winner read error = %v", err)
		}
	})

	t.Run("view-and-overlay-bindings", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		files := engine.Files()
		batchID := storageformat.Digest([]byte("overlay-binding"))
		if _, _, err := files.portableUploadBatchAbortAtView(ctx, nil, owner, batchID); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("nil abort view error = %v", err)
		}
		view, err := newNamespaceStore(engine).loadView(ctx, owner, "")
		if err != nil {
			t.Fatal(err)
		}
		view.uploadAborts = nil
		if _, found, err := files.portableUploadBatchAbortAtView(ctx, view, owner, batchID); err != nil || found || view.uploadAborts == nil {
			t.Fatalf("empty abort cache = found %t, error %v", found, err)
		}

		record := checkpointUploadRecord(engine, owner, "overlay-record", engine.clock.Now().Add(time.Hour))
		record.Batch = &storageformat.PortableUploadBatchMember{BatchID: batchID, Index: 0, Count: 1}
		seedCheckpointUploadRecord(t, engine, owner, record)
		view, err = newNamespaceStore(engine).loadView(ctx, owner, "")
		if err != nil {
			t.Fatal(err)
		}
		overlay := storageformat.PortableUploadBatchAbort{
			SchemaVersion: 1, OwnerID: owner.String(), BatchID: batchID, Count: 2, Aborted: []byte{1}, ModifiedAt: engine.clock.Now().UTC(),
		}
		body, err := storageformat.EncodeCanonical(overlay)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(owner), consistencyDomainMutation{
			ID: "misbound-overlay", Changes: []consistencyDomainChange{{Key: uploadBatchAbortKey(batchID), Require: domainValueAbsent, Value: body}},
		}, view.headSnapshot, view.session); err != nil {
			t.Fatal(err)
		}
		view, err = newNamespaceStore(engine).loadView(ctx, owner, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := files.portableUploadAtView(ctx, view, owner, record.UploadID); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("misbound overlay error = %v", err)
		}
	})

	t.Run("snapshot-applies-valid-batch-abort", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		files := engine.Files()
		batchID := storageformat.Digest([]byte("snapshot-overlay"))
		record := checkpointUploadRecord(engine, owner, "snapshot-record", engine.clock.Now().Add(time.Hour))
		record.Batch = &storageformat.PortableUploadBatchMember{BatchID: batchID, Index: 0, Count: 1}
		seedCheckpointUploadRecord(t, engine, owner, record)
		view, err := newNamespaceStore(engine).loadView(ctx, owner, "")
		if err != nil {
			t.Fatal(err)
		}
		overlay := storageformat.PortableUploadBatchAbort{
			SchemaVersion: 1, OwnerID: owner.String(), BatchID: batchID, Count: 1, Aborted: []byte{1}, ModifiedAt: engine.clock.Now().UTC(),
		}
		body, err := storageformat.EncodeCanonical(overlay)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(owner), consistencyDomainMutation{
			ID: "valid-overlay", Changes: []consistencyDomainChange{{Key: uploadBatchAbortKey(batchID), Require: domainValueAbsent, Value: body}},
		}, view.headSnapshot, view.session); err != nil {
			t.Fatal(err)
		}
		got, _, _, _, err := files.portableUploadSnapshot(ctx, owner, record.UploadID)
		if err != nil || got.State != storageformat.UploadAborted {
			t.Fatalf("overlay snapshot state = %s, error %v", got.State, err)
		}
	})

	t.Run("distinct-session-loser-is-revoked", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		intent := checkpointUploadRecord(engine, owner, "lease-race", engine.clock.Now().Add(time.Hour))
		leaseKey := storageformat.LeaseKey(base.BackendKind(), intent.UploadID)
		winner := objectstore.Object{Key: leaseKey, Body: []byte("winner-lease"), Version: "winner"}
		hooks := &hookedBackend{
			Backend: base,
			put: func(context.Context, objectstore.Key, []byte, objectstore.PutCondition) (objectstore.NativeVersion, error) {
				return "", domain.NewError(domain.ErrorConflict, "lease winner")
			},
			get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
				if key == leaseKey {
					return winner, nil
				}
				return base.Get(ctx, key)
			},
		}
		engine.backend = hooks
		begun := objectstore.UploadHandle{Capability: objectstore.UploadCapability{Protocol: domain.UploadSingle, URL: "http://127.0.0.1/loser", Method: "PUT", ExpiresAt: intent.ExpiresAt}, Lease: []byte("loser-lease")}
		resumed := objectstore.UploadCapability{Protocol: domain.UploadSingle, URL: "http://127.0.0.1/winner", Method: "PUT", ExpiresAt: intent.ExpiresAt}
		transfers := &transferFailureBackend{Backend: hooks, transfers: base, beginHandle: &begun, resumeValue: &resumed, abortOK: true}
		engine.fileBackend = transfers
		capability, object, err := engine.Files().ensurePortableUploadLease(ctx, intent, true)
		if abortCalls := transfers.abortCalls.Load(); err != nil || capability.URL != resumed.URL || string(object.Body) != "winner-lease" || abortCalls != 1 {
			t.Fatalf("reconciled lease = %+v, %q, aborts=%d, error=%v", capability, object.Body, abortCalls, err)
		}
	})

	t.Run("cleanup-propagates-segment-read-and-state-errors", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		files := engine.Files()
		batchID := storageformat.Digest([]byte("cleanup-read-failure"))
		record := checkpointUploadRecord(engine, owner, "cleanup-read-failure", engine.clock.Now().Add(time.Hour))
		record.State, record.CleanupPending = storageformat.UploadAborted, true
		record.Batch = &storageformat.PortableUploadBatchMember{BatchID: batchID, Index: 0, Count: 1}
		seedCheckpointUploadRecord(t, engine, owner, record)
		segmentKey := storageformat.UploadLeaseSegmentKey(base.BackendKind(), batchID, 0)
		engine.backend = &hookedBackend{Backend: base, get: func(_ context.Context, key objectstore.Key) (objectstore.Object, error) {
			if key == segmentKey {
				return objectstore.Object{}, failure
			}
			return base.Get(ctx, key)
		}}
		if err := files.cleanupPortableUpload(ctx, owner, record.UploadID, nil); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("cleanup segment read error = %v", err)
		}

		active := checkpointUploadRecord(engine, owner, "cleanup-active", engine.clock.Now().Add(time.Hour))
		active.CleanupPending = true
		engine.backend = base
		seedCheckpointUploadRecord(t, engine, owner, active)
		if err := files.cleanupPortableUpload(ctx, owner, active.UploadID, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("non-terminal cleanup error = %v", err)
		}
	})

	t.Run("missing-standalone-lease", func(t *testing.T) {
		base := objectmemory.New()
		files := openNamespaceTestEngine(t, base).Files()
		item := portableUploadTransactionItem{record: checkpointUploadRecord(files.engine, owner, "missing-standalone-lease", files.engine.clock.Now().Add(time.Hour))}
		if _, err := files.runtimeUploadLeasesForRange(ctx, []portableUploadTransactionItem{item}, 0, 1); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing standalone lease error = %v", err)
		}
	})

	t.Run("corrupt-transition-lock", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		record := checkpointUploadRecord(engine, owner, "corrupt-transition-lock", engine.clock.Now().Add(time.Hour))
		seedCheckpointUploadRecord(t, engine, owner, record)
		if _, err := engine.stateDomainStore().mutate(ctx, uploadDomainReference(owner), consistencyDomainMutation{
			ID: "corrupt-transition-lock", Changes: []consistencyDomainChange{{Key: transitionLockKey009, Require: domainValueAbsent, Value: []byte("invalid")}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := engine.Files().portableUploadSnapshot(ctx, owner, record.UploadID); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt transition lock error = %v", err)
		}
	})

	t.Run("corrupt-abort-overlay", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		batchID := storageformat.Digest([]byte("corrupt-snapshot-overlay"))
		record := checkpointUploadRecord(engine, owner, "corrupt-snapshot-overlay", engine.clock.Now().Add(time.Hour))
		record.Batch = &storageformat.PortableUploadBatchMember{BatchID: batchID, Index: 0, Count: 1}
		seedCheckpointUploadRecord(t, engine, owner, record)
		if _, err := engine.stateDomainStore().mutate(ctx, uploadDomainReference(owner), consistencyDomainMutation{
			ID: "corrupt-snapshot-overlay", Changes: []consistencyDomainChange{{Key: uploadBatchAbortKey(batchID), Require: domainValueAbsent, Value: []byte("invalid")}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := engine.Files().portableUploadSnapshot(ctx, owner, record.UploadID); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("corrupt abort overlay error = %v", err)
		}
	})

	t.Run("abort-overlay-page-read", func(t *testing.T) {
		base := objectmemory.New()
		engine := openNamespaceTestEngine(t, base)
		reference := uploadDomainReference(owner)
		missing := storageformat.DomainTreeRoot{Digest: storageformat.Digest([]byte("missing-abort-page")), Level: 0, EntryCount: 1}
		view := &namespaceView{
			reference: reference, head: storageformat.DomainHead{SchemaVersion: 1, Registered: true, Kind: reference.Kind, DomainID: reference.ID, Base: missing},
			session: newConsistencyDomainTreeSession(engine.stateDomainStore(), reference), uploadAborts: make(map[string]portableUploadAbortCache),
		}
		if _, _, err := engine.Files().portableUploadBatchAbortAtView(ctx, view, owner, storageformat.Digest([]byte("missing-abort-batch"))); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("abort overlay page error = %v", err)
		}
	})
}

func TestSchema011BatchPublicationAndReplayFailuresAreAtomic(t *testing.T) {
	ctx := context.Background()
	failure := domain.NewError(domain.ErrorUnavailable, "authoritative publication unavailable")

	t.Run("replace-existing-destination", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		path := domain.MustParseUserPath("/replace-success.bin")
		original, err := fixture.engine.Files().CreateDirectory(ctx, fixture.scope, domain.CreateDirectoryRequest{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		capabilities, err := fixture.engine.Files().CreateUploadBatch(ctx, fixture.scope, []domain.CreateUploadRequest{{
			Path: path, Size: 1, MediaType: "application/octet-stream", Conflict: domain.ConflictReplace,
			ExpectedVersion: original.Version, IdempotencyKey: "replace-success-item",
		}})
		if err != nil {
			t.Fatal(err)
		}
		useSchema011VerifiedMetadata(fixture)
		result, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, "replace-success-completion"))
		if err != nil || len(result.Entries) != 1 || result.Entries[0].Kind != domain.EntryFile {
			t.Fatalf("replace completion = %+v, %v", result, err)
		}
	})

	for _, test := range []struct {
		name  string
		abort bool
		batch bool
	}{
		{name: "complete"},
		{name: "abort-overlay", abort: true, batch: true},
		{name: "abort-legacy", abort: true},
	} {
		t.Run(test.name+"-head-publication", func(t *testing.T) {
			fixture := newSchema011TransferFixture(t)
			capabilities := fixture.createBatch(t, "/"+test.name+"-head.bin")
			headKey := storageformat.DomainHeadKey(storageformat.DomainNamespace, fixture.scope.UserID().String())
			fixture.engine.backend = &hookedBackend{
				Backend: fixture.backend,
				put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
					if key == headKey {
						return "", failure
					}
					return fixture.backend.Put(callCtx, key, body, condition)
				},
			}
			if test.abort {
				request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, IdempotencyKey: test.name + "-publication"}
				if test.batch {
					request.BatchID = capabilities[0].BatchID
				}
				if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("abort publication error = %v", err)
				}
				return
			}
			useSchema011VerifiedMetadata(fixture)
			if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, schema011CompletionRequest(capabilities, test.name+"-publication")); !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("completion publication error = %v", err)
			}
		})
	}

	t.Run("durable-progress-publication", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, schema011BatchPaths(storageformat.UploadTransactionSegmentItems+1, "progress-failure")...)
		useSchema011VerifiedMetadata(fixture)
		completion := schema011CompletionRequest(capabilities, "progress-publication-failure")
		completionID, _, err := normalizePortableUploadCompletionBatch(fixture.scope.UserID(), completion)
		if err != nil {
			t.Fatal(err)
		}
		completionKey := storageformat.UploadTransactionProgressKey(fixture.backend.BackendKind(), completionID)
		fixture.engine.backend = &hookedBackend{Backend: fixture.backend, put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == completionKey {
				return "", failure
			}
			return fixture.backend.Put(callCtx, key, body, condition)
		}}
		if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, completion); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("completion progress publication error = %v", err)
		}

		uploadIDs := make([]domain.UploadID, len(capabilities))
		for index := range capabilities {
			uploadIDs[index] = capabilities[index].UploadID
		}
		abort := domain.AbortUploadBatchRequest{UploadIDs: uploadIDs, BatchID: capabilities[0].BatchID, IdempotencyKey: "abort-progress-publication-failure"}
		abortID, _, err := normalizePortableUploadAbortBatch(fixture.scope.UserID(), abort)
		if err != nil {
			t.Fatal(err)
		}
		abortKey := storageformat.UploadTransactionProgressKey(fixture.backend.BackendKind(), abortID)
		fixture.engine.backend = &hookedBackend{Backend: fixture.backend, put: func(callCtx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if key == abortKey {
				return "", failure
			}
			return fixture.backend.Put(callCtx, key, body, condition)
		}}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, abort); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("abort progress publication error = %v", err)
		}
	})

	t.Run("replay-outcome-binding", func(t *testing.T) {
		for _, kind := range []string{"complete", "abort"} {
			t.Run(kind, func(t *testing.T) {
				fixture := newSchema011TransferFixture(t)
				capabilities := fixture.createBatch(t, "/replay-"+kind+".bin")
				var transactionID, fingerprint string
				if kind == "complete" {
					request := schema011CompletionRequest(capabilities, "replay-binding-complete")
					transactionID, fingerprint, _ = normalizePortableUploadCompletionBatch(fixture.scope.UserID(), request)
				} else {
					request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "replay-binding-abort"}
					transactionID, fingerprint, _ = normalizePortableUploadAbortBatch(fixture.scope.UserID(), request)
				}
				stateValue := "aborted"
				if kind == "abort" {
					stateValue = "completed"
				}
				result, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{
					SchemaVersion: 1, RequestFingerprint: fingerprint,
					UploadBatch: &storageformat.NamespaceUploadBatchResult{TransactionID: transactionID, ItemCount: 1, State: stateValue},
				})
				if err != nil {
					t.Fatal(err)
				}
				view, err := newNamespaceStore(fixture.engine).loadView(ctx, fixture.scope.UserID(), "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(fixture.scope.UserID()), consistencyDomainMutation{
					ID: transactionID, Changes: []consistencyDomainChange{{Key: "test/replay-" + kind, Require: domainValueAbsent, Value: []byte("bound")}}, Result: result,
				}, view.headSnapshot, view.session); err != nil {
					t.Fatal(err)
				}
				if kind == "complete" {
					request := schema011CompletionRequest(capabilities, "replay-binding-complete")
					if _, err := fixture.engine.Files().CompleteUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrInvalid) {
						t.Fatalf("completion replay binding error = %v", err)
					}
				} else {
					request := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{capabilities[0].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "replay-binding-abort"}
					if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrInvalid) {
						t.Fatalf("abort replay binding error = %v", err)
					}
				}
			})
		}
	})

	t.Run("duplicate-batch-member", func(t *testing.T) {
		fixture := newSchema011TransferFixture(t)
		capabilities := fixture.createBatch(t, "/duplicate-member-a.bin", "/duplicate-member-b.bin")
		view, err := newNamespaceStore(fixture.engine).loadView(ctx, fixture.scope.UserID(), "")
		if err != nil {
			t.Fatal(err)
		}
		record, value, err := fixture.engine.Files().portableUploadAtView(ctx, view, fixture.scope.UserID(), string(capabilities[1].UploadID))
		if err != nil {
			t.Fatal(err)
		}
		record.Batch.Index = 0
		body, err := storageformat.EncodeCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(fixture.scope.UserID()), consistencyDomainMutation{
			ID: "duplicate-batch-member", Changes: []consistencyDomainChange{{Key: uploadRecordKey(record.UploadID), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}},
		}, view.headSnapshot, view.session); err != nil {
			t.Fatal(err)
		}
		request := domain.AbortUploadBatchRequest{
			UploadIDs: []domain.UploadID{capabilities[0].UploadID, capabilities[1].UploadID}, BatchID: capabilities[0].BatchID, IdempotencyKey: "duplicate-batch-member-abort",
		}
		if err := fixture.engine.Files().AbortUploadBatch(ctx, fixture.scope, request); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("duplicate batch member error = %v", err)
		}
	})
}
