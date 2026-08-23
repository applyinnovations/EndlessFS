package portable

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type transferRecoveryHookBackend struct {
	*objectmemory.Backend
	getErr      error
	deleteErr   error
	abortErr    error
	progressErr error
}

func (backend *transferRecoveryHookBackend) Get(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
	if backend.getErr != nil && strings.Contains(key.String(), "/leases/") {
		return objectstore.Object{}, backend.getErr
	}
	return backend.Backend.Get(ctx, key)
}

func (backend *transferRecoveryHookBackend) Delete(ctx context.Context, key objectstore.Key, condition objectstore.DeleteCondition) error {
	if backend.deleteErr != nil && strings.Contains(key.String(), "/leases/") {
		return backend.deleteErr
	}
	return backend.Backend.Delete(ctx, key, condition)
}

func (backend *transferRecoveryHookBackend) AbortUpload(ctx context.Context, lease []byte) error {
	if backend.abortErr != nil {
		return backend.abortErr
	}
	return backend.Backend.AbortUpload(ctx, lease)
}

func (backend *transferRecoveryHookBackend) UploadProgress(ctx context.Context, lease []byte) (objectstore.UploadProgress, error) {
	if backend.progressErr != nil {
		return objectstore.UploadProgress{}, backend.progressErr
	}
	return backend.Backend.UploadProgress(ctx, lease)
}

func TestLazyDirectoryManifestValidationDeniesAmbiguousContentSources(t *testing.T) {
	validGroupID := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	validEntry := storageformat.DirectoryContentIndexEntry{GroupID: validGroupID, RelativePath: "/file.bin", Size: 1}
	validBase := &storageformat.DirectoryContentBase{Area: "live", DirectoryID: "directory", ManifestID: "manifest"}
	validSubtree := storageformat.DirectoryContentDelta{Area: "trash", DirectoryID: "directory", ManifestID: "manifest", Prefix: "/child"}
	validDirect := storageformat.DirectoryContentDelta{Entry: &validEntry}

	cases := map[string]storageformat.DirectoryManifest{
		"materialized-with-lazy-base": {
			SchemaVersion: 2, ContentBase: validBase,
		},
		"unsupported-schema": {
			SchemaVersion: 1,
		},
		"lazy-with-materialized-root": {
			SchemaVersion: 3, ContentIndexRootID: "root",
		},
		"lazy-with-invalid-sketch": {
			SchemaVersion: 3, ContentSketch: []string{"invalid"},
		},
		"empty-with-content-base": {
			SchemaVersion: 3, ContentBase: validBase,
		},
		"nonempty-without-source": {
			SchemaVersion: 3, RecursiveFileCount: 1,
		},
		"nonempty-with-invalid-base": {
			SchemaVersion: 3, RecursiveFileCount: 1, ContentBase: &storageformat.DirectoryContentBase{Area: "elsewhere"},
		},
		"too-many-deltas": {
			SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: make([]storageformat.DirectoryContentDelta, 257),
		},
		"delta-without-source": {
			SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: []storageformat.DirectoryContentDelta{{}},
		},
		"delta-with-two-sources": {
			SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: []storageformat.DirectoryContentDelta{{Entry: &validEntry, Area: "live"}},
		},
		"invalid-direct-entry": {
			SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: []storageformat.DirectoryContentDelta{{Entry: &storageformat.DirectoryContentIndexEntry{}}},
		},
		"root-subtree-prefix": {
			SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: []storageformat.DirectoryContentDelta{{Area: "live", DirectoryID: "directory", ManifestID: "manifest", Prefix: "/"}},
		},
		"invalid-subtree-area": {
			SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: []storageformat.DirectoryContentDelta{{Area: "elsewhere", DirectoryID: "directory", ManifestID: "manifest", Prefix: "/child"}},
		},
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateDirectoryManifestContent(manifest); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("validateDirectoryManifestContent() error = %v", err)
			}
		})
	}
	for name, manifest := range map[string]storageformat.DirectoryManifest{
		"empty":           {SchemaVersion: 3},
		"content-base":    {SchemaVersion: 3, RecursiveFileCount: 1, ContentBase: validBase},
		"direct-delta":    {SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: []storageformat.DirectoryContentDelta{validDirect}},
		"subtree-delta":   {SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: []storageformat.DirectoryContentDelta{validSubtree}},
		"removed-subtree": {SchemaVersion: 3, RecursiveFileCount: 1, ContentDeltas: []storageformat.DirectoryContentDelta{{Remove: true, Area: validSubtree.Area, DirectoryID: validSubtree.DirectoryID, ManifestID: validSubtree.ManifestID, Prefix: validSubtree.Prefix}}},
	} {
		t.Run("accept-"+name, func(t *testing.T) {
			if err := validateDirectoryManifestContent(manifest); err != nil {
				t.Fatalf("validateDirectoryManifestContent() error = %v", err)
			}
		})
	}
}

func TestPinnedDirectoryScopeAndLazyPreparationDenials(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 1, 2, 3, 4, 5, 0, time.UTC))
	backend := objectmemory.New()
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("pinned-directory", 1<<16)))
	user, _ := domain.ParseUserID("cGlubmVkLWRpcmVjdG9yeS11c2Vy")
	live, _ := domain.NewScope(user, domain.AreaLive)

	if _, err := directoryEntryStorageScope(domain.Scope{}, storageformat.DirectoryEntry{Kind: domain.EntryDirectory}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid inherited scope error = %v", err)
	}
	if _, err := directoryEntryStorageScope(live, storageformat.DirectoryEntry{Kind: domain.EntryFile}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("file storage scope error = %v", err)
	}
	if inherited, err := directoryEntryStorageScope(live, storageformat.DirectoryEntry{Kind: domain.EntryDirectory}); err != nil || inherited != live {
		t.Fatalf("inherited scope = %+v, %v", inherited, err)
	}
	if _, err := directoryEntryStorageScope(live, storageformat.DirectoryEntry{Kind: domain.EntryDirectory, StorageArea: "elsewhere"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid stored area error = %v", err)
	}
	if _, err := engine.Files().readDirectoryEntryMetadata(ctx, live, storageformat.DirectoryEntry{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid pinned entry error = %v", err)
	}

	accumulator, digest, err := directoryContentIdentity(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().prepareDirectoryWithLazyContent(live, "directory", -1, 0, 0, 1, storageformat.DirectoryIndexChild{}, nil, nil, nil, nil, nil, accumulator, digest, clock.Now(), "manifest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("negative aggregate error = %v", err)
	}
	if _, err := engine.Files().prepareDirectoryWithLazyContent(live, "directory", 0, 0, 0, 1, storageformat.DirectoryIndexChild{}, nil, nil, nil, nil, nil, "invalid", digest, clock.Now(), "manifest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid accumulator error = %v", err)
	}
	if _, err := engine.Files().prepareDirectoryWithLazyContent(live, "directory", 0, 0, 0, 1, storageformat.DirectoryIndexChild{}, nil, nil, nil, nil, nil, accumulator, "wrong", clock.Now(), "manifest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("content identity mismatch error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader(""))
	if _, err := engine.Files().prepareDirectoryWithLazyContent(live, "directory", 0, 0, 0, 1, storageformat.DirectoryIndexChild{}, nil, nil, nil, nil, nil, accumulator, digest, clock.Now(), ""); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("manifest ID generation error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader(strings.Repeat("restored-ids", 1<<16)))
	hugeBase := &storageformat.DirectoryContentBase{Area: "live", DirectoryID: strings.Repeat("x", storageformat.MaxCanonicalBytes), ManifestID: "manifest"}
	if _, err := engine.Files().prepareDirectoryWithLazyContent(live, "directory", 0, 0, 1, 1, storageformat.DirectoryIndexChild{}, nil, nil, hugeBase, nil, nil, accumulator, digest, clock.Now(), "manifest"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized manifest error = %v", err)
	}
	if _, err := engine.Files().prepareDirectoryWithLazyContent(live, "directory", 0, 0, 0, 0, storageformat.DirectoryIndexChild{}, nil, nil, nil, nil, nil, accumulator, digest, clock.Now(), "manifest-revision-zero"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid root revision error = %v", err)
	}

	prepared, err := engine.Files().prepareDirectory(ctx, live, "directory", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, prerequisite := range prepared.prerequisites {
		if _, err := backend.Put(ctx, objectstore.MustKey(prerequisite.Key), prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	entry := storageformat.DirectoryEntry{Kind: domain.EntryDirectory, DirectoryID: "directory", ManifestID: prepared.manifestID, ContentDigest: prepared.contentDigest, Size: 1}
	if _, err := engine.Files().readDirectoryEntryMetadata(ctx, live, entry); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("pinned aggregate mismatch error = %v", err)
	}

	operation := func(operationID string) storageformat.FileOperation {
		return storageformat.FileOperation{
			SchemaVersion: 1, OperationID: operationID, UserID: user.String(), Kind: operationDelete,
			State: storageformat.FileOperationRunning, Attempt: 1, Fence: 1, ReplicaAttemptID: "replica",
			ExpiresAt: clock.Now().Add(time.Minute), StartedAt: clock.Now(), UpdatedAt: clock.Now(),
			Roots: []storageformat.FileOperationRoot{{Key: "endlessfs/v1/test/root", PendingBody: []byte("pending"), FinalBody: []byte("final")}},
		}
	}
	putPendingRoot := func(t *testing.T, backend *objectmemory.Backend, operationID string, fence uint64, recursiveBytes int64) {
		t.Helper()
		key := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
		root := storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: storageformat.RootDirectoryID, RecursiveBytes: recursiveBytes, Pending: &storageformat.DirectoryTransition{
			OperationID: operationID, Fence: fence, PostManifestID: "post", PostContentAccumulator: accumulator, PostContentDigest: digest,
		}}
		if _, err := backend.Put(ctx, key, encodeInternalEnvelope(t, directoryRootSchema, key, 1, root), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("pending-operation-missing", func(t *testing.T) {
		candidate := objectmemory.New()
		candidateEngine := openInternalTestEngine(t, candidate, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<15)))
		putPendingRoot(t, candidate, "missing", 1, 0)
		if _, err := candidateEngine.Files().readDirectoryMetadata(ctx, live, storageformat.RootDirectoryID, false); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing pending operation error = %v", err)
		}
	})
	t.Run("pending-fence-invalid", func(t *testing.T) {
		candidate := objectmemory.New()
		candidateEngine := openInternalTestEngine(t, candidate, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<15)))
		stored := operation("operation")
		key := storageformat.OperationKey(user.String(), stored.OperationID)
		if _, err := candidate.Put(ctx, key, encodeInternalEnvelope(t, fileOperationSchema, key, 1, stored), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		putPendingRoot(t, candidate, stored.OperationID, 2, 0)
		if _, err := candidateEngine.Files().readDirectoryMetadata(ctx, live, storageformat.RootDirectoryID, false); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid pending fence error = %v", err)
		}
	})
}

func TestProviderMetadataMutationRecoveryDenials(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 2, 3, 4, 5, 6, 0, time.UTC))
	plain := objectmemory.New()
	plainEngine := openInternalTestEngine(t, metadataOnlyBackend{Backend: plain}, clock, strings.NewReader(strings.Repeat("metadata-only", 1<<16)))
	if err := plainEngine.ensureUploadCompletions(ctx, []string{"upload"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("metadata-only completion error = %v", err)
	}
	if err := plainEngine.ensureUploadAborts(ctx, []string{"upload"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("metadata-only abort error = %v", err)
	}

	newTransferEngine := func(t *testing.T) (*objectmemory.Backend, *Engine) {
		t.Helper()
		backend := objectmemory.New()
		server := httptest.NewServer(backend)
		t.Cleanup(server.Close)
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(strings.NewReader(strings.Repeat(t.Name(), 1<<15)))); err != nil {
			t.Fatal(err)
		}
		return backend, openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name()+"-engine", 1<<15)))
	}
	newHookedTransferEngine := func(t *testing.T) (*transferRecoveryHookBackend, *Engine) {
		t.Helper()
		backend := &transferRecoveryHookBackend{Backend: objectmemory.New()}
		server := httptest.NewServer(backend)
		t.Cleanup(server.Close)
		if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(strings.NewReader(strings.Repeat(t.Name(), 1<<15)))); err != nil {
			t.Fatal(err)
		}
		return backend, openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name()+"-engine", 1<<15)))
	}

	t.Run("order-and-missing-leases", func(t *testing.T) {
		_, engine := newTransferEngine(t)
		if err := engine.ensureUploadCompletions(ctx, []string{"same", "same"}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("completion order error = %v", err)
		}
		if err := engine.ensureUploadAborts(ctx, []string{"same", "same"}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("abort order error = %v", err)
		}
	})

	putLease := func(t *testing.T, backend *objectmemory.Backend, uploadID string, lease storageformat.TransferLease) {
		t.Helper()
		key := storageformat.LeaseKey(backend.BackendKind(), uploadID)
		body := encodeInternalEnvelope(t, transferLeaseSchema, key, 1, lease)
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("invalid-completion-lease", func(t *testing.T) {
		backend, engine := newTransferEngine(t)
		putLease(t, backend, "upload", storageformat.TransferLease{SchemaVersion: 1, BackendKind: "other", UploadID: "upload", Ciphertext: []byte("lease")})
		if err := engine.ensureUploadCompletions(ctx, []string{"upload"}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid completion lease error = %v", err)
		}
	})
	t.Run("malformed-completion-lease", func(t *testing.T) {
		backend, engine := newTransferEngine(t)
		key := storageformat.LeaseKey(backend.BackendKind(), "upload")
		if _, err := backend.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.ensureUploadCompletions(ctx, []string{"upload"}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed completion lease error = %v", err)
		}
	})
	t.Run("invalid-abort-lease", func(t *testing.T) {
		backend, engine := newTransferEngine(t)
		putLease(t, backend, "upload", storageformat.TransferLease{SchemaVersion: 1, BackendKind: backend.BackendKind(), UploadID: "different", Ciphertext: []byte("lease")})
		if err := engine.ensureUploadAborts(ctx, []string{"upload"}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid abort lease error = %v", err)
		}
	})
	t.Run("provider-has-not-completed", func(t *testing.T) {
		backend, engine := newTransferEngine(t)
		handle, err := backend.BeginUpload(ctx, objectstore.UploadRequest{UploadID: "upload", Key: objectstore.MustKey("endlessfs/v1/test/blob"), Size: 1, MediaType: "application/octet-stream", ExpiresAt: clock.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		putLease(t, backend, "upload", storageformat.TransferLease{SchemaVersion: 1, BackendKind: backend.BackendKind(), UploadID: "upload", Ciphertext: handle.Lease, ExpiresAt: clock.Now().Add(time.Minute)})
		if err := engine.ensureUploadCompletions(ctx, []string{"upload"}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("incomplete provider upload error = %v", err)
		}
	})
	t.Run("provider-abort-removes-lease", func(t *testing.T) {
		backend, engine := newTransferEngine(t)
		handle, err := backend.BeginUpload(ctx, objectstore.UploadRequest{UploadID: "upload", Key: objectstore.MustKey("endlessfs/v1/test/blob"), Size: 1, MediaType: "application/octet-stream", ExpiresAt: clock.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		putLease(t, backend, "upload", storageformat.TransferLease{SchemaVersion: 1, BackendKind: backend.BackendKind(), UploadID: "upload", Ciphertext: handle.Lease, ExpiresAt: clock.Now().Add(time.Minute)})
		if err := engine.ensureUploadAborts(ctx, []string{"upload"}); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Get(ctx, storageformat.LeaseKey(backend.BackendKind(), "upload")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("aborted lease remains: %v", err)
		}
	})
	t.Run("completion-lease-read-failure", func(t *testing.T) {
		backend, engine := newHookedTransferEngine(t)
		backend.getErr = domain.NewError(domain.ErrorUnavailable, "read denied")
		if err := engine.ensureUploadCompletions(ctx, []string{"upload"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("completion lease read error = %v", err)
		}
	})
	t.Run("abort-lease-read-failure", func(t *testing.T) {
		backend, engine := newHookedTransferEngine(t)
		backend.getErr = domain.NewError(domain.ErrorUnavailable, "read denied")
		if err := engine.ensureUploadAborts(ctx, []string{"upload"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("abort lease read error = %v", err)
		}
	})
	t.Run("malformed-abort-lease", func(t *testing.T) {
		backend, engine := newTransferEngine(t)
		key := storageformat.LeaseKey(backend.BackendKind(), "upload")
		if _, err := backend.Put(ctx, key, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
		if err := engine.ensureUploadAborts(ctx, []string{"upload"}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed abort lease error = %v", err)
		}
	})
	t.Run("completion-progress-failure", func(t *testing.T) {
		backend, engine := newHookedTransferEngine(t)
		putLease(t, backend.Backend, "upload", storageformat.TransferLease{SchemaVersion: 1, BackendKind: backend.BackendKind(), UploadID: "upload", Ciphertext: []byte("lease")})
		backend.progressErr = domain.NewError(domain.ErrorUnavailable, "progress denied")
		if err := engine.ensureUploadCompletions(ctx, []string{"upload"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("completion progress error = %v", err)
		}
	})
	t.Run("abort-provider-failure", func(t *testing.T) {
		backend, engine := newHookedTransferEngine(t)
		putLease(t, backend.Backend, "upload", storageformat.TransferLease{SchemaVersion: 1, BackendKind: backend.BackendKind(), UploadID: "upload", Ciphertext: []byte("lease")})
		backend.abortErr = domain.NewError(domain.ErrorUnavailable, "abort denied")
		if err := engine.ensureUploadAborts(ctx, []string{"upload"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("provider abort error = %v", err)
		}
	})
	t.Run("abort-lease-delete-failure", func(t *testing.T) {
		backend, engine := newHookedTransferEngine(t)
		handle, err := backend.BeginUpload(ctx, objectstore.UploadRequest{UploadID: "upload", Key: objectstore.MustKey("endlessfs/v1/test/blob"), Size: 1, MediaType: "application/octet-stream", ExpiresAt: clock.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		putLease(t, backend.Backend, "upload", storageformat.TransferLease{SchemaVersion: 1, BackendKind: backend.BackendKind(), UploadID: "upload", Ciphertext: handle.Lease})
		backend.deleteErr = domain.NewError(domain.ErrorUnavailable, "delete denied")
		if err := engine.ensureUploadAborts(ctx, []string{"upload"}); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("lease delete error = %v", err)
		}
	})

	destination := objectstore.MustKey("endlessfs/v1/test/destination")
	body := []byte("provider-attested")
	if _, err := plain.Put(ctx, destination, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	fingerprint := objectstore.FingerprintFor(body)
	if err := (&Engine{fileBackend: plain}).ensureMutationCopies(ctx, []storageformat.MutationCopy{{SourceKey: "endlessfs/v1/test/source", DestinationKey: destination.String(), Size: int64(len(body)), MD5: fingerprint.MD5, CRC32C: fingerprint.CRC32C}}); err != nil {
		t.Fatalf("existing provider-attested destination error = %v", err)
	}
	if fingerprint, present, err := mutationCopyFingerprint(storageformat.MutationCopy{SHA256: "legacy"}); err != nil || present || fingerprint != (objectstore.ContentFingerprint{}) {
		t.Fatalf("legacy SHA-only fingerprint = %+v, %t, %v", fingerprint, present, err)
	}
}

func TestFileOperationFailureAndFencingBranches(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 3, 4, 5, 6, 7, 0, time.UTC))
	user, _ := domain.ParseUserID("b3BlcmF0aW9uLWZhaWx1cmUtdXNlcg")
	makeOperation := func(operationID string, state storageformat.FileOperationState) (objectstore.Key, storageformat.FileOperation, []byte) {
		key := storageformat.OperationKey(user.String(), operationID)
		operation := storageformat.FileOperation{
			SchemaVersion: 1, OperationID: operationID, UserID: user.String(), Kind: operationDelete,
			State: state, Attempt: 1, Fence: 1, ReplicaAttemptID: "replica",
			ExpiresAt: clock.Now().Add(time.Minute), StartedAt: clock.Now(), UpdatedAt: clock.Now(),
			Roots: []storageformat.FileOperationRoot{{Key: "endlessfs/v1/test/root", PendingBody: []byte("pending"), FinalBody: []byte("final")}},
		}
		return key, operation, encodeInternalEnvelope(t, fileOperationSchema, key, 1, operation)
	}
	putOperation := func(t *testing.T, backend *objectmemory.Backend, key objectstore.Key, body []byte) {
		t.Helper()
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	newEngine := func(t *testing.T) (*objectmemory.Backend, *Engine) {
		t.Helper()
		backend := objectmemory.New()
		return backend, openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(t.Name(), 1<<16)))
	}

	t.Run("missing-operation", func(t *testing.T) {
		_, engine := newEngine(t)
		if err := engine.Files().failPreparingFileOperation(ctx, storageformat.OperationKey(user.String(), "missing"), "failed"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing operation error = %v", err)
		}
	})
	for _, state := range []storageformat.FileOperationState{storageformat.FileOperationFailed, storageformat.FileOperationRunning} {
		t.Run(string(state), func(t *testing.T) {
			backend, engine := newEngine(t)
			key, _, body := makeOperation("operation", state)
			putOperation(t, backend, key, body)
			err := engine.Files().failPreparingFileOperation(ctx, key, "failed")
			if state == storageformat.FileOperationFailed && err != nil {
				t.Fatal(err)
			}
			if state == storageformat.FileOperationRunning && !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("running operation error = %v", err)
			}
		})
	}
	t.Run("preparing-fails-durably", func(t *testing.T) {
		backend, engine := newEngine(t)
		key, _, body := makeOperation("operation", storageformat.FileOperationPreparing)
		putOperation(t, backend, key, body)
		if err := engine.Files().failPreparingFileOperation(ctx, key, "pinned input changed"); err != nil {
			t.Fatal(err)
		}
		operation, err := engine.Files().readFileOperation(ctx, user, "operation")
		if err != nil || operation.State != storageformat.FileOperationFailed || operation.ErrorKind != domain.ErrorPreconditionFailed {
			t.Fatalf("failed operation = %+v, %v", operation, err)
		}
	})
	t.Run("failure-record-too-large", func(t *testing.T) {
		backend, engine := newEngine(t)
		key, _, body := makeOperation("operation", storageformat.FileOperationPreparing)
		putOperation(t, backend, key, body)
		if err := engine.Files().failPreparingFileOperation(ctx, key, strings.Repeat("x", storageformat.MaxCanonicalBytes)); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("oversized failure error = %v", err)
		}
	})
	t.Run("failure-write-superseded", func(t *testing.T) {
		backend, engine := newEngine(t)
		key, _, body := makeOperation("operation", storageformat.FileOperationPreparing)
		putOperation(t, backend, key, body)
		engine.backend = &hookedBackend{Backend: backend, put: func(_ context.Context, target objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if target == key {
				return "", domain.NewError(domain.ErrorPreconditionFailed, "superseded")
			}
			return backend.Put(ctx, target, body, condition)
		}}
		if err := engine.Files().failPreparingFileOperation(ctx, key, "failed"); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("superseded failure error = %v", err)
		}
	})

	t.Run("legacy-execution-prerequisite-denied", func(t *testing.T) {
		backend, engine := newEngine(t)
		key, operation, _ := makeOperation("operation", storageformat.FileOperationRunning)
		operation.Prerequisites = []storageformat.MutationObject{{Key: "INVALID", Body: []byte("body")}}
		putOperation(t, backend, key, encodeInternalEnvelope(t, fileOperationSchema, key, 1, operation))
		if err := engine.Files().executeFileOperation(ctx, key); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid prerequisite error = %v", err)
		}
	})
	t.Run("legacy-execution-copy-denied", func(t *testing.T) {
		backend, engine := newEngine(t)
		key, operation, _ := makeOperation("operation", storageformat.FileOperationRunning)
		operation.Copies = []storageformat.MutationCopy{{SourceKey: "same", DestinationKey: "same", Size: 1}}
		putOperation(t, backend, key, encodeInternalEnvelope(t, fileOperationSchema, key, 1, operation))
		if err := engine.Files().executeFileOperation(ctx, key); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid copy error = %v", err)
		}
	})

	t.Run("step-page-encoding-denied", func(t *testing.T) {
		_, engine := newEngine(t)
		operation := storageformat.FileOperation{UserID: user.String(), OperationID: "operation", ReplicaAttemptID: "attempt", Roots: []storageformat.FileOperationRoot{{Key: "root", PendingBody: bytes.Repeat([]byte{'x'}, storageformat.MaxCanonicalBytes), FinalBody: []byte("final")}}}
		if err := engine.Files().persistFileOperationStepPages(ctx, &operation); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("oversized step page error = %v", err)
		}
	})
	t.Run("copy-step-page-write-denied", func(t *testing.T) {
		backend, engine := newEngine(t)
		operation := storageformat.FileOperation{UserID: user.String(), OperationID: "operation", ReplicaAttemptID: "attempt", Roots: []storageformat.FileOperationRoot{{Key: "root", PendingBody: []byte("pending"), FinalBody: []byte("final")}}, Copies: []storageformat.MutationCopy{{SourceKey: "source", DestinationKey: "destination", Size: 1}}}
		keyOperation := operation
		keyOperation.StepSetID = operation.ReplicaAttemptID
		failedKey := stagedFileOperationStepPageKey(keyOperation, 1)
		engine.backend = &hookedBackend{Backend: backend, put: func(_ context.Context, target objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
			if target == failedKey {
				return "", domain.NewError(domain.ErrorUnavailable, "write denied")
			}
			return backend.Put(ctx, target, body, condition)
		}}
		if err := engine.Files().persistFileOperationStepPages(ctx, &operation); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("copy step-page error = %v", err)
		}
	})

	t.Run("pin-prepared-directory-denials", func(t *testing.T) {
		userScope, _ := domain.NewScope(user, domain.AreaLive)
		child := directoryUpdate{scope: userScope, path: domain.MustParseUserPath("/child"), directoryID: "child"}
		if err := pinPreparedDirectoryInParent(nil, child, preparedDirectory{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("missing parent error = %v", err)
		}
		rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID).String()
		updates := map[string]directoryUpdate{rootKey: {path: domain.MustParseUserPath("/"), changes: map[string]directoryEntryMutation{}}}
		if err := pinPreparedDirectoryInParent(updates, child, preparedDirectory{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("missing parent change error = %v", err)
		}
		huge := storageformat.DirectoryEntry{Name: strings.Repeat("x", storageformat.MaxCanonicalBytes), Kind: domain.EntryDirectory, DirectoryID: "child"}
		updates[rootKey] = directoryUpdate{path: domain.MustParseUserPath("/"), changes: map[string]directoryEntryMutation{"child": {after: &huge}}}
		if err := pinPreparedDirectoryInParent(updates, child, preparedDirectory{}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("oversized pinned entry error = %v", err)
		}
	})

	t.Run("equal-depth-operation-sort", func(t *testing.T) {
		_, engine := newEngine(t)
		updates := map[string]directoryUpdate{
			"endlessfs/v1/test/a": {path: domain.MustParseUserPath("/a"), snapshot: directorySnapshot{pending: true}},
			"endlessfs/v1/test/b": {path: domain.MustParseUserPath("/b"), snapshot: directorySnapshot{pending: true}},
		}
		if _, _, err := engine.Files().buildFileOperation(ctx, user, "operation", "owner", operationDelete, updates, nil, nil); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("pending operation error = %v", err)
		}
	})
}
