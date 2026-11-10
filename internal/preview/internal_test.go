package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/provider"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestContentBindingAndArtifactValidationFailures(t *testing.T) {
	binding := internalBinding(t)
	if !binding.Valid() {
		t.Fatal("valid binding was rejected")
	}
	tooShort := binding
	tooShort.RecipeID = ""
	if tooShort.Valid() {
		t.Fatal("empty recipe was accepted")
	}
	badCharacter := binding
	badCharacter.RecipeID = "image_v1"
	if badCharacter.Valid() {
		t.Fatal("invalid recipe character was accepted")
	}
	artifact := internalArtifact("generation", binding.Variant)
	if !artifact.ValidFor(binding) {
		t.Fatal("valid artifact was rejected")
	}
	invalidShape := artifact
	invalidShape.Width = 0
	if invalidShape.ValidFor(binding) {
		t.Fatal("invalid artifact dimensions were accepted")
	}
	invalidDigest := artifact
	invalidDigest.SHA256 = "invalid"
	if invalidDigest.ValidFor(binding) {
		t.Fatal("invalid artifact digest was accepted")
	}
	shortDigest := artifact
	shortDigest.SHA256 = base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size-1))
	if shortDigest.ValidFor(binding) {
		t.Fatal("short artifact digest was accepted")
	}
	mismatchedDigest := artifact
	mismatchedDigest.SHA256 = base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))
	if mismatchedDigest.ValidFor(binding) {
		t.Fatal("mismatched artifact digest was accepted")
	}
	invalidCRC32C := artifact
	invalidCRC32C.CRC32C = "invalid"
	if invalidCRC32C.ValidFor(binding) {
		t.Fatal("invalid artifact CRC32C was accepted")
	}
	mismatchedCRC32C := artifact
	mismatchedCRC32C.CRC32C = ChecksumCRC32C([]byte("different"))
	if mismatchedCRC32C.ValidFor(binding) {
		t.Fatal("mismatched artifact CRC32C was accepted")
	}
	invalidWebP := artifact
	invalidWebP.Bytes = append([]byte(nil), artifact.Bytes...)
	invalidWebP.Bytes[51] ^= 0xff
	sum := sha256.Sum256(invalidWebP.Bytes)
	invalidWebP.SHA256 = base64.RawURLEncoding.EncodeToString(sum[:])
	invalidWebP.CRC32C = ChecksumCRC32C(invalidWebP.Bytes)
	if invalidWebP.ValidFor(binding) {
		t.Fatal("corrupt WebP was accepted")
	}
}

func TestGenerateDurableRunningFailureAndReplayTransitions(t *testing.T) {
	binding := internalBinding(t)
	entry := domain.Entry{
		Path: domain.MustParseUserPath("/source.png"), Name: "source.png", Kind: domain.EntryFile,
		Size: binding.SourceSize, MediaType: binding.MediaType, Version: "source-version",
		ContentID: binding.ContentID, ContentVersion: binding.ContentVersion, ContentModifiedAt: time.Now(),
	}
	request := GenerateRequest{Path: entry.Path, Version: entry.Version, Variant: binding.Variant, IdempotencyKey: "preview-durable-running-0001"}
	newService := func(source *scriptedStorage, store *scriptedStore) *Service {
		return &Service{
			options: Options{Automatic: true, Resolutions: []int{binding.Variant}, OperationTimeout: time.Second, OperationRetention: time.Hour, HardMaxSourceBytes: 1024},
			source:  source, store: store, generators: []Generator{scriptedGenerator{}},
			client: internalResponseClient(http.StatusOK, io.NopCloser(bytes.NewReader([]byte("source")))),
			ids:    domain.NewIDGenerator(bytes.NewReader(make([]byte, 4096))), clock: domain.SystemClock{}, state: state.NewMemoryStore(),
			global: make(chan struct{}, 1), perUser: make(map[string]*userLimit), inflight: make(map[string]*generationCall),
		}
	}
	runningService := newService(&scriptedStorage{stat: entry}, &scriptedStore{latestErr: domain.ErrNotFound, claimErr: domain.ErrConflict})
	operation, err := runningService.Generate(context.Background(), binding.Owner, request)
	if err != nil || operation.State != domain.OperationRunning || operation.Result == nil || operation.Result.State != StateGenerating {
		t.Fatalf("running operation = %+v, %v", operation, err)
	}

	failedService := newService(&scriptedStorage{statErr: domain.ErrNotFound}, &scriptedStore{latestErr: domain.ErrNotFound})
	request.IdempotencyKey = "preview-durable-failed-0001"
	operation, err = failedService.Generate(context.Background(), binding.Owner, request)
	if !errors.Is(err, domain.ErrNotFound) || operation.State != domain.OperationFailed {
		t.Fatalf("failed operation = %+v, %v", operation, err)
	}
	if replayed, replayErr := replayedOperation(Operation{State: domain.OperationFailed, ErrorKind: domain.ErrorInvalid}); !errors.Is(replayErr, domain.ErrInvalid) || replayed.State != domain.OperationFailed {
		t.Fatalf("failed replay = %+v, %v", replayed, replayErr)
	}
}

func TestServicePreservesSafePreviewStoreStartupFailureCategory(t *testing.T) {
	store := &scriptedStore{validateErr: domain.NewError(domain.ErrorUnavailable, "preview store validation failed: capability origin")}
	_, err := NewService(
		Options{ApplicationState: state.NewMemoryStore()},
		&scriptedStorage{}, store, nil, http.DefaultClient,
		domain.NewIDGenerator(bytes.NewReader(make([]byte, 64))), domain.SystemClock{},
	)
	if !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "capability origin") {
		t.Fatalf("startup validation error = %v", err)
	}
}

func TestStoreValidationCategoryRejectsUnclassifiedAndUnknownDetails(t *testing.T) {
	for _, err := range []error{
		errors.New("provider detail"),
		domain.NewError(domain.ErrorUnavailable, "preview store validation failed: bucket secret"),
	} {
		if category := StoreValidationCategory(err); category != "" {
			t.Fatalf("unsafe startup category = %q", category)
		}
	}
}

func TestDurableOperationStateRejectsCorruptionAndInvalidIndexes(t *testing.T) {
	binding := internalBinding(t)
	store := state.NewMemoryStore()
	service := &Service{
		state: store, ids: domain.NewIDGenerator(bytes.NewReader(make([]byte, 128))),
		clock:   domain.NewFixedClock(time.Date(2044, 2, 3, 4, 5, 6, 0, time.UTC)),
		options: Options{OperationTimeout: time.Minute, OperationRetention: time.Hour},
	}
	idempotencyKey := "preview-corrupt-state-0001"
	digest := sha256.Sum256([]byte(binding.Owner.String() + "\x00" + idempotencyKey))
	key := state.MustKey(state.NamespaceIdempotency, "preview", binding.Owner.String(), base64.RawURLEncoding.EncodeToString(digest[:]))
	if _, err := store.Create(context.Background(), key, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.claimOperation(context.Background(), binding.Owner, idempotencyKey, "fingerprint"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt durable idempotency error = %v", err)
	}
	expires := service.clock.Now().Add(time.Hour)
	entry := operationIndexEntry{OperationID: "operation", IdempotencyDigest: "digest", ExpiresAt: expires}
	if validOperationIndex(operationIndexRecord{SchemaVersion: 1, Entries: []operationIndexEntry{entry, entry}}) {
		t.Fatal("duplicate durable operation index was accepted")
	}
	if validOperationIndex(operationIndexRecord{SchemaVersion: 2}) {
		t.Fatal("unknown durable operation index schema was accepted")
	}
	if validOperationIndex(operationIndexRecord{SchemaVersion: 1, Entries: []operationIndexEntry{{}}}) {
		t.Fatal("invalid durable operation index entry was accepted")
	}

	operationID := domain.OperationID("semantic-operation")
	digestValue := sha256.Sum256([]byte("semantic-idempotency"))
	base := operationRecord{
		SchemaVersion: 1, OwnerID: binding.Owner.String(), Fingerprint: "fingerprint", IdempotencyDigest: base64.RawURLEncoding.EncodeToString(digestValue[:]),
		LeaseEpoch: 1, LeaseExpiresAt: service.clock.Now().Add(time.Minute), ExpiresAt: service.clock.Now().Add(time.Hour),
		Operation: Operation{ID: operationID, State: domain.OperationRunning, StartedAt: service.clock.Now(), UpdatedAt: service.clock.Now()},
	}
	if !validOperationRecord(base, binding.Owner, operationID) {
		t.Fatal("valid running operation record was rejected")
	}
	tests := []struct {
		name   string
		mutate func(*operationRecord)
	}{
		{name: "noncanonical digest", mutate: func(record *operationRecord) { record.IdempotencyDigest = "digest" }},
		{name: "unknown state", mutate: func(record *operationRecord) { record.Operation.State = "unknown" }},
		{name: "running without lease", mutate: func(record *operationRecord) { record.LeaseExpiresAt = time.Time{} }},
		{name: "running lease before update", mutate: func(record *operationRecord) { record.LeaseExpiresAt = record.Operation.UpdatedAt }},
		{name: "running result without generation", mutate: func(record *operationRecord) {
			record.Operation.Result = &ItemResult{Path: domain.MustParseUserPath("/source.png"), Version: "version", Variant: 256, State: StateGenerating}
		}},
		{name: "terminal with lease", mutate: func(record *operationRecord) {
			record.Operation.State = domain.OperationFailed
			record.Operation.ErrorKind = domain.ErrorInvalid
		}},
		{name: "unknown failure kind", mutate: func(record *operationRecord) {
			record.Operation.State = domain.OperationFailed
			record.Operation.ErrorKind = "unknown"
			record.LeaseExpiresAt = time.Time{}
		}},
		{name: "updated after expiry", mutate: func(record *operationRecord) { record.Operation.UpdatedAt = record.ExpiresAt }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate)
			if validOperationRecord(candidate, binding.Owner, operationID) {
				t.Fatalf("corrupt operation record was accepted: %+v", candidate)
			}
		})
	}
}

func TestDurableOperationIndexCleansExpiredRecordsAndBoundsContention(t *testing.T) {
	owner := internalBinding(t).Owner
	clock := domain.NewFixedClock(time.Date(2045, 3, 4, 5, 6, 7, 0, time.UTC))
	store := state.NewMemoryStore()
	service := &Service{state: store, clock: clock}
	firstID, secondID := operationIDsInSameShard()
	firstDigestBytes := sha256.Sum256([]byte("expired-digest"))
	firstDigest := base64.RawURLEncoding.EncodeToString(firstDigestBytes[:])
	first := operationIndexEntry{OperationID: firstID, IdempotencyDigest: firstDigest, ExpiresAt: clock.Now().Add(time.Minute)}
	if err := service.registerOperation(context.Background(), owner, first); err != nil {
		t.Fatal(err)
	}
	operationKey := previewOperationKey(owner, firstID)
	operationBody, err := state.EncodeJSON(operationRecord{
		SchemaVersion: 1, OwnerID: owner.String(), Fingerprint: "fingerprint", IdempotencyDigest: firstDigest,
		LeaseEpoch: 1, LeaseExpiresAt: clock.Now().Add(30 * time.Second), ExpiresAt: first.ExpiresAt,
		Operation: Operation{ID: firstID, State: domain.OperationRunning, StartedAt: clock.Now(), UpdatedAt: clock.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), operationKey, operationBody); err != nil {
		t.Fatal(err)
	}
	idempotencyKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), first.IdempotencyDigest)
	idempotencyBody, err := state.EncodeJSON(idempotencyRecord{SchemaVersion: 1, Fingerprint: "fingerprint", OperationID: firstID, ExpiresAt: first.ExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), idempotencyKey, idempotencyBody); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	second := operationIndexEntry{OperationID: secondID, IdempotencyDigest: "new-digest", ExpiresAt: clock.Now().Add(time.Hour)}
	if err := service.registerOperation(context.Background(), owner, second); err != nil {
		t.Fatal(err)
	}
	if err := service.registerOperation(context.Background(), owner, second); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("operation identity collision error = %v", err)
	}
	if err := (&Service{state: &getFailureStore{AtomicStore: store, err: domain.ErrUnavailable}, clock: clock}).registerOperation(context.Background(), owner, second); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("operation index read error = %v", err)
	}
	third := operationIndexEntry{OperationID: operationIDForShard(operationShard(second.OperationID), "store-error"), IdempotencyDigest: "store-error", ExpiresAt: clock.Now().Add(time.Hour)}
	if err := (&Service{state: &compareAndSwapErrorStore{AtomicStore: store, err: domain.ErrUnavailable}, clock: clock}).registerOperation(context.Background(), owner, third); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("operation index write error = %v", err)
	}
	if _, err := store.Get(context.Background(), operationKey); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired operation cleanup error = %v", err)
	}
	if _, err := store.Get(context.Background(), idempotencyKey); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired idempotency cleanup error = %v", err)
	}

	corruptStore := state.NewMemoryStore()
	corruptService := &Service{state: corruptStore, clock: clock}
	shard := operationShard(secondID)
	indexKey := state.MustKey(state.NamespaceOperations, "preview-index", owner.String(), shard)
	if _, err := corruptStore.Create(context.Background(), indexKey, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := corruptService.registerOperation(context.Background(), owner, second); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt operation index error = %v", err)
	}

	capacityStore := state.NewMemoryStore()
	entries := make([]operationIndexEntry, maxOperationsPerShard)
	for index := range entries {
		entries[index] = operationIndexEntry{OperationID: domain.OperationID(fmt.Sprintf("operation-%03d", index)), IdempotencyDigest: fmt.Sprintf("digest-%03d", index), ExpiresAt: clock.Now().Add(time.Hour)}
	}
	capacityBody, err := state.EncodeJSON(operationIndexRecord{SchemaVersion: 1, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capacityStore.Create(context.Background(), indexKey, capacityBody); err != nil {
		t.Fatal(err)
	}
	capacityService := &Service{state: capacityStore, clock: clock}
	if err := capacityService.registerOperation(context.Background(), owner, second); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("operation index capacity error = %v", err)
	}

	contentionStore := &compareAndSwapFailureStore{AtomicStore: store}
	contentionService := &Service{state: contentionStore, clock: clock}
	thirdID := operationIDForShard(shard, "contention")
	if err := contentionService.registerOperation(context.Background(), owner, operationIndexEntry{OperationID: thirdID, IdempotencyDigest: "contention", ExpiresAt: clock.Now().Add(time.Hour)}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("operation index contention error = %v", err)
	}
}

func TestExpiredPreviewBindingCleanupRejectsEveryPartialRelationship(t *testing.T) {
	ctx := context.Background()
	owner := internalBinding(t).Owner
	now := time.Date(2048, 5, 6, 7, 8, 9, 0, time.UTC)
	operationID := domain.OperationID("expired-cleanup-operation")
	digest := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, sha256.Size))
	expiresAt := now.Add(-time.Second)
	idempotency := idempotencyRecord{SchemaVersion: 1, Fingerprint: "fingerprint", OperationID: operationID, ExpiresAt: expiresAt}
	idempotencyKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), digest)
	operation := operationRecord{
		SchemaVersion: 1, OwnerID: owner.String(), Fingerprint: idempotency.Fingerprint, IdempotencyDigest: digest,
		LeaseEpoch: 1, LeaseExpiresAt: now.Add(-time.Minute), ExpiresAt: expiresAt,
		Operation: Operation{ID: operationID, State: domain.OperationRunning, StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-2 * time.Minute)},
	}
	indexEntry := operationIndexEntry{OperationID: operationID, IdempotencyDigest: digest, ExpiresAt: expiresAt}
	indexSnapshot := operationIndexSnapshot{record: operationIndexRecord{SchemaVersion: 1, Entries: []operationIndexEntry{indexEntry}}}

	if err := (&Service{state: state.NewMemoryStore()}).cleanupExpiredOperationIndex(ctx, owner, operationIndexSnapshot{record: operationIndexRecord{SchemaVersion: 1, Entries: []operationIndexEntry{{OperationID: "future", IdempotencyDigest: "future", ExpiresAt: now.Add(time.Hour)}}}}, now); err != nil {
		t.Fatalf("future-only cleanup error = %v", err)
	}
	if err := (&Service{state: state.NewMemoryStore()}).cleanupExpiredOperationIndex(ctx, owner, indexSnapshot, now); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing idempotency cleanup error = %v", err)
	}
	if err := (&Service{state: &getFailureStore{AtomicStore: state.NewMemoryStore(), err: domain.ErrUnavailable}}).cleanupExpiredOperationIndex(ctx, owner, indexSnapshot, now); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("idempotency read cleanup error = %v", err)
	}
	corruptIdempotencyStore := state.NewMemoryStore()
	if _, err := corruptIdempotencyStore.Create(ctx, idempotencyKey, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := (&Service{state: corruptIdempotencyStore}).cleanupExpiredOperationIndex(ctx, owner, indexSnapshot, now); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt idempotency cleanup error = %v", err)
	}

	if err := (&Service{state: state.NewMemoryStore()}).cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, "v1", idempotency); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing operation cleanup error = %v", err)
	}
	if err := (&Service{state: &getFailureStore{AtomicStore: state.NewMemoryStore(), err: domain.ErrUnavailable}}).cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, "v1", idempotency); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("operation read cleanup error = %v", err)
	}
	corruptOperationStore := state.NewMemoryStore()
	if _, err := corruptOperationStore.Create(ctx, previewOperationKey(owner, operationID), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := (&Service{state: corruptOperationStore}).cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, "v1", idempotency); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt operation cleanup error = %v", err)
	}

	newOperationStore := func(t *testing.T) *state.MemoryStore {
		t.Helper()
		store := state.NewMemoryStore()
		seedOperationRecord(t, store, owner, operation)
		return store
	}
	wrongKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x52}, sha256.Size)))
	if err := (&Service{state: newOperationStore(t)}).cleanupExpiredPreviewBinding(ctx, owner, wrongKey, "v1", idempotency); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("wrong idempotency key cleanup error = %v", err)
	}
	if err := (&Service{state: newOperationStore(t)}).cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, "v1", idempotency); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing operation index cleanup error = %v", err)
	}

	seedIndex := func(t *testing.T, store *state.MemoryStore, entries []operationIndexEntry) {
		t.Helper()
		body, err := state.EncodeJSON(operationIndexRecord{SchemaVersion: 1, Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		indexKey := state.MustKey(state.NamespaceOperations, "preview-index", owner.String(), operationShard(operationID))
		if _, err := store.Create(ctx, indexKey, body); err != nil {
			t.Fatal(err)
		}
	}
	mismatchedIndexStore := newOperationStore(t)
	mismatched := indexEntry
	mismatched.IdempotencyDigest = "different"
	seedIndex(t, mismatchedIndexStore, []operationIndexEntry{mismatched})
	if err := (&Service{state: mismatchedIndexStore}).cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, "v1", idempotency); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("mismatched index cleanup error = %v", err)
	}

	missingEntryStore := newOperationStore(t)
	otherID := operationIDForShard(operationShard(operationID), "other-cleanup-operation")
	seedIndex(t, missingEntryStore, []operationIndexEntry{{OperationID: otherID, IdempotencyDigest: "other", ExpiresAt: expiresAt}})
	if err := (&Service{state: missingEntryStore}).cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, "v1", idempotency); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing index entry cleanup error = %v", err)
	}

	successStore := newOperationStore(t)
	idempotencyBody, err := state.EncodeJSON(idempotency)
	if err != nil {
		t.Fatal(err)
	}
	idempotencyVersion, err := successStore.Create(ctx, idempotencyKey, idempotencyBody)
	if err != nil {
		t.Fatal(err)
	}
	seedIndex(t, successStore, []operationIndexEntry{indexEntry, {OperationID: otherID, IdempotencyDigest: "other", ExpiresAt: now.Add(time.Hour)}})
	if err := (&Service{state: successStore}).cleanupExpiredPreviewBinding(ctx, owner, idempotencyKey, idempotencyVersion, idempotency); err != nil {
		t.Fatalf("valid cleanup error = %v", err)
	}
}

func TestHydrateOperationReauthorizesExactGenerationAndRejectsDrift(t *testing.T) {
	binding := internalBinding(t)
	entry := domain.Entry{
		Path: domain.MustParseUserPath("/source.png"), Kind: domain.EntryFile, Version: "source-version",
		Size: binding.SourceSize, MediaType: binding.MediaType, ContentID: binding.ContentID, ContentVersion: binding.ContentVersion,
	}
	metadata := internalArtifact("generation", binding.Variant).Metadata()
	base := Operation{ID: "operation", State: domain.OperationSucceeded, Result: &ItemResult{
		Path: entry.Path, Version: entry.Version, Variant: binding.Variant, State: StateReady, Artifact: &metadata,
	}}
	service := &Service{
		options: Options{Resolutions: []int{binding.Variant}}, source: &scriptedStorage{stat: entry},
		store: &scriptedStore{}, generators: []Generator{scriptedGenerator{}},
	}
	hydrated, err := service.hydrateOperation(context.Background(), binding.Owner, base)
	if err != nil || hydrated.Result.Capability == nil {
		t.Fatalf("hydrated operation = %+v, %v", hydrated, err)
	}
	if unchanged, err := service.hydrateOperation(context.Background(), binding.Owner, Operation{State: domain.OperationRunning}); err != nil || unchanged.State != domain.OperationRunning {
		t.Fatalf("nonterminal hydration = %+v, %v", unchanged, err)
	}

	tests := []struct {
		name    string
		mutate  func(*Service, *Operation)
		wantErr error
	}{
		{name: "invalid item", mutate: func(_ *Service, operation *Operation) { operation.Result.Path = domain.MustParseUserPath("/") }, wantErr: domain.ErrInvalid},
		{name: "stat failure", mutate: func(service *Service, _ *Operation) { service.source = &scriptedStorage{statErr: domain.ErrNotFound} }, wantErr: domain.ErrNotFound},
		{name: "version drift", mutate: func(service *Service, _ *Operation) {
			changed := entry
			changed.Version = "changed"
			service.source = &scriptedStorage{stat: changed}
		}, wantErr: domain.ErrPreconditionFailed},
		{name: "format removed", mutate: func(service *Service, _ *Operation) { service.generators = nil }, wantErr: domain.ErrPreconditionFailed},
		{name: "metadata corruption", mutate: func(_ *Service, operation *Operation) { operation.Result.Artifact.Width = 0 }, wantErr: domain.ErrInvalid},
		{name: "capability unavailable", mutate: func(service *Service, _ *Operation) {
			service.store = &scriptedStore{capabilityErr: domain.ErrUnavailable}
		}, wantErr: domain.ErrUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateService := &Service{
				options: service.options, source: service.source, store: service.store,
				generators: append([]Generator(nil), service.generators...),
			}
			candidate := base
			result := *base.Result
			artifact := *base.Result.Artifact
			result.Artifact = &artifact
			candidate.Result = &result
			test.mutate(candidateService, &candidate)
			if _, err := candidateService.hydrateOperation(context.Background(), binding.Owner, candidate); !errors.Is(err, test.wantErr) {
				t.Fatalf("hydrateOperation() error = %v, want %v", err, test.wantErr)
			}
		})
	}
	unsafe := operationRecord{SchemaVersion: 1, OwnerID: binding.Owner.String(), Fingerprint: "fingerprint", IdempotencyDigest: "digest", LeaseEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), Operation: hydrated}
	unsafe.Operation.StartedAt, unsafe.Operation.UpdatedAt = time.Now(), time.Now()
	if validOperationRecord(unsafe, binding.Owner, hydrated.ID) {
		t.Fatal("persisted bearer capability was accepted")
	}
}

func TestDurableOperationClaimLeaseAndStateFailureBoundaries(t *testing.T) {
	binding := internalBinding(t)
	owner := binding.Owner
	clock := domain.NewFixedClock(time.Date(2047, 4, 5, 6, 7, 8, 0, time.UTC))
	newService := func(store state.AtomicStore) *Service {
		return &Service{
			state: store, clock: clock, ids: domain.NewIDGenerator(bytes.NewReader(make([]byte, 4096))),
			options: Options{OperationTimeout: time.Minute, OperationRetention: time.Hour},
		}
	}
	store := state.NewMemoryStore()
	service := newService(store)
	idempotencyKey := "preview-durable-lease-0001"
	fingerprint := "fingerprint"
	operationID := domain.OperationID("durable-operation")
	digestBytes := sha256.Sum256([]byte(owner.String() + "\x00" + idempotencyKey))
	digest := base64.RawURLEncoding.EncodeToString(digestBytes[:])
	record := operationRecord{
		SchemaVersion: 1, OwnerID: owner.String(), Fingerprint: fingerprint, IdempotencyDigest: digest, LeaseEpoch: 1,
		LeaseExpiresAt: clock.Now().Add(time.Minute), ExpiresAt: clock.Now().Add(time.Hour),
		Operation: Operation{ID: operationID, State: domain.OperationRunning, StartedAt: clock.Now(), UpdatedAt: clock.Now()},
	}
	seedDurableOperation(t, store, owner, idempotencyKey, record)
	claim, err := service.claimOperation(context.Background(), owner, idempotencyKey, fingerprint)
	if err != nil || claim.claimed || claim.record.Operation.ID != operationID {
		t.Fatalf("unexpired durable claim = %+v, %v", claim, err)
	}
	if _, err := service.claimOperation(context.Background(), owner, idempotencyKey, "different"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("durable fingerprint conflict error = %v", err)
	}
	mismatchedStore := state.NewMemoryStore()
	seedDurableOperation(t, mismatchedStore, owner, idempotencyKey, record)
	mismatchedKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), digest)
	mismatchedValue, err := mismatchedStore.Get(context.Background(), mismatchedKey)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedBody, err := state.EncodeJSON(idempotencyRecord{
		SchemaVersion: 1, Fingerprint: fingerprint, OperationID: operationID, ExpiresAt: record.ExpiresAt.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatchedStore.CompareAndSwap(context.Background(), mismatchedKey, mismatchedValue.Version, mismatchedBody); err != nil {
		t.Fatal(err)
	}
	if _, err := newService(mismatchedStore).claimOperation(context.Background(), owner, idempotencyKey, fingerprint); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cross-record mismatch error = %v", err)
	}

	clock.Advance(2 * time.Minute)
	preconditionService := newService(&compareAndSwapFailureStore{AtomicStore: store})
	claim, err = preconditionService.claimOperation(context.Background(), owner, idempotencyKey, fingerprint)
	if err != nil || claim.claimed {
		t.Fatalf("lost durable takeover race = %+v, %v", claim, err)
	}
	unavailableService := newService(&compareAndSwapErrorStore{AtomicStore: store, err: domain.ErrUnavailable})
	if _, err := unavailableService.claimOperation(context.Background(), owner, idempotencyKey, fingerprint); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("durable takeover store error = %v", err)
	}

	expiredStore := state.NewMemoryStore()
	expiredService := newService(expiredStore)
	expiredKey := "preview-expired-idempotency-0001"
	expiredDigestBytes := sha256.Sum256([]byte(owner.String() + "\x00" + expiredKey))
	expiredDigest := base64.RawURLEncoding.EncodeToString(expiredDigestBytes[:])
	expiredStateKey := state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), expiredDigest)
	expiredBody, err := state.EncodeJSON(idempotencyRecord{SchemaVersion: 1, Fingerprint: fingerprint, OperationID: "expired-operation", ExpiresAt: clock.Now().Add(-time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expiredStore.Create(context.Background(), expiredStateKey, expiredBody); err != nil {
		t.Fatal(err)
	}
	expiredBindingRecord := operationRecord{
		SchemaVersion: 1, OwnerID: owner.String(), Fingerprint: fingerprint, IdempotencyDigest: expiredDigest, LeaseEpoch: 1,
		LeaseExpiresAt: clock.Now().Add(-2 * time.Second), ExpiresAt: clock.Now().Add(-time.Second),
		Operation: Operation{ID: "expired-operation", State: domain.OperationRunning, StartedAt: clock.Now().Add(-time.Hour), UpdatedAt: clock.Now().Add(-time.Minute)},
	}
	seedOperationRecord(t, expiredStore, owner, expiredBindingRecord)
	indexKey := state.MustKey(state.NamespaceOperations, "preview-index", owner.String(), operationShard("expired-operation"))
	indexBody, err := state.EncodeJSON(operationIndexRecord{SchemaVersion: 1, Entries: []operationIndexEntry{{OperationID: "expired-operation", IdempotencyDigest: expiredDigest, ExpiresAt: expiredBindingRecord.ExpiresAt}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expiredStore.Create(context.Background(), indexKey, indexBody); err != nil {
		t.Fatal(err)
	}
	claim, err = expiredService.claimOperation(context.Background(), owner, expiredKey, fingerprint)
	if err != nil || !claim.claimed || claim.record.Operation.ID == "expired-operation" {
		t.Fatalf("expired idempotency replacement = %+v, %v", claim, err)
	}

	corruptStore := state.NewMemoryStore()
	corruptKey := previewOperationKey(owner, "corrupt-operation")
	if _, err := corruptStore.Create(context.Background(), corruptKey, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newService(corruptStore).readOperation(context.Background(), owner, "corrupt-operation"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt operation read error = %v", err)
	}
	if _, _, err := newService(&getFailureStore{AtomicStore: corruptStore, err: domain.ErrUnavailable}).readOperation(context.Background(), owner, "corrupt-operation"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unavailable operation read error = %v", err)
	}
	if _, err := newService(&getFailureStore{AtomicStore: corruptStore, err: domain.ErrUnavailable}).claimOperation(context.Background(), owner, "preview-state-error-0001", fingerprint); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("unavailable idempotency read error = %v", err)
	}

	operationValue, err := store.Get(context.Background(), previewOperationKey(owner, operationID))
	if err != nil {
		t.Fatal(err)
	}
	finishClaim := operationClaim{record: record, operationKey: previewOperationKey(owner, operationID), operationVersion: operationValue.Version, claimed: true}
	if _, err := newService(&compareAndSwapErrorStore{AtomicStore: store, err: domain.ErrUnavailable}).finishOperation(context.Background(), finishClaim, record.Operation); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("finish operation store error = %v", err)
	}
	invalidOwnerClaim := finishClaim
	invalidOwnerClaim.record.OwnerID = "invalid"
	if _, err := newService(&compareAndSwapFailureStore{AtomicStore: store}).finishOperation(context.Background(), invalidOwnerClaim, record.Operation); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("finish operation owner error = %v", err)
	}
	readFailureStore := &getFailureStore{AtomicStore: &compareAndSwapFailureStore{AtomicStore: store}, err: domain.ErrUnavailable}
	if _, err := newService(readFailureStore).finishOperation(context.Background(), finishClaim, record.Operation); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("finish operation concurrent read error = %v", err)
	}

	for failAt := 1; failAt <= 1; failAt++ {
		failureStore := &nthCreateFailureStore{AtomicStore: state.NewMemoryStore(), failAt: failAt}
		failureService := newService(failureStore)
		if _, err := failureService.claimOperation(context.Background(), owner, fmt.Sprintf("preview-create-failure-%04d", failAt), fingerprint); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("atomic claim failure %d error = %v", failAt, err)
		}
		for _, prefix := range []state.Prefix{
			state.MustPrefix(state.NamespaceOperations, "preview", owner.String()),
			state.MustPrefix(state.NamespaceOperations, "preview-index", owner.String()),
			state.MustPrefix(state.NamespaceIdempotency, "preview", owner.String()),
		} {
			page, listErr := failureStore.List(context.Background(), prefix, state.PageRequest{})
			if listErr != nil || len(page.Items) != 0 {
				t.Fatalf("failed atomic preview claim left records under %q: %+v, %v", prefix.String(), page, listErr)
			}
		}
	}
	idFailureService := newService(state.NewMemoryStore())
	idFailureService.ids = domain.NewIDGenerator(bytes.NewReader(nil))
	if _, err := idFailureService.claimOperation(context.Background(), owner, "preview-id-failure-0001", fingerprint); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("operation ID failure error = %v", err)
	}

	expiredRecord := record
	expiredRecord.Operation.ID = "expired-visible-operation"
	expiredRecord.ExpiresAt = clock.Now().Add(-time.Second)
	expiredRecord.LeaseExpiresAt = time.Time{}
	expiredRecord.Operation.State = domain.OperationFailed
	expiredRecord.Operation.ErrorKind = domain.ErrorInvalid
	seedOperationRecord(t, store, owner, expiredRecord)
	if _, err := service.Operation(context.Background(), owner, expiredRecord.Operation.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired Operation() error = %v", err)
	}
}

type scriptedStore struct {
	latest         Artifact
	latestErr      error
	validateErr    error
	capabilityErr  error
	claimErr       error
	commitErr      error
	recordReadyErr error
}

func (s *scriptedStore) Validate(context.Context) error { return s.validateErr }
func (*scriptedStore) Check(context.Context) error      { return nil }
func (s *scriptedStore) Claim(context.Context, Binding, string, time.Time) (GenerationClaim, error) {
	return GenerationClaim{ID: "claim", Epoch: 1, ExpiresAt: time.Now().Add(time.Hour)}, s.claimErr
}
func (*scriptedStore) Release(context.Context, Binding, GenerationClaim) error { return nil }
func (s *scriptedStore) Commit(context.Context, Binding, GenerationClaim, Artifact) error {
	return s.commitErr
}
func (s *scriptedStore) Latest(context.Context, Binding) (ArtifactMetadata, error) {
	return s.latest.Metadata(), s.latestErr
}
func (*scriptedStore) ResolveReady(_ context.Context, selections []ReadySelection) ([]*ArtifactMetadata, error) {
	return make([]*ArtifactMetadata, len(selections)), nil
}
func (s *scriptedStore) RecordReady(context.Context, ReadySelection, ArtifactMetadata) error {
	return s.recordReadyErr
}
func (s *scriptedStore) Read(context.Context, Binding, string) (Artifact, error) {
	return s.latest, s.latestErr
}
func (s *scriptedStore) CreateDownload(context.Context, Binding, string) (domain.DownloadCapability, error) {
	return domain.DownloadCapability{URL: "http://127.0.0.1:1234/preview", Method: http.MethodGet}, s.capabilityErr
}
func (s *scriptedStore) CreateKnownDownload(context.Context, Binding, ArtifactMetadata) (domain.DownloadCapability, error) {
	return domain.DownloadCapability{URL: "http://127.0.0.1:1234/preview", Method: http.MethodGet}, s.capabilityErr
}
func (*scriptedStore) Ready() bool        { return true }
func (*scriptedStore) DataOrigin() string { return "http://127.0.0.1:1234" }

type scriptedStorage struct {
	provider.Storage
	download    domain.DownloadCapability
	downloadErr error
	stat        domain.Entry
	statErr     error
}

func (s *scriptedStorage) Stat(context.Context, domain.Scope, domain.UserPath) (domain.Entry, error) {
	return s.stat, s.statErr
}

func (s *scriptedStorage) CreateDownload(context.Context, domain.Scope, domain.CreateDownloadRequest) (domain.DownloadCapability, error) {
	return s.download, s.downloadErr
}

type scriptedGenerator struct {
	generated GeneratedArtifact
	err       error
}

func (scriptedGenerator) Capability() string             { return "image" }
func (scriptedGenerator) RecipeID() string               { return "image-webp-q80-v1" }
func (scriptedGenerator) Supports(string) bool           { return true }
func (scriptedGenerator) SelfTest(context.Context) error { return nil }
func (g scriptedGenerator) Generate(context.Context, GenerationRequest) (GeneratedArtifact, error) {
	return g.generated, g.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func internalClient(function roundTripFunc) *http.Client { return &http.Client{Transport: function} }

func internalResponseClient(status int, body io.ReadCloser) *http.Client {
	return internalClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
	})
}

func internalBinding(t *testing.T) Binding {
	t.Helper()
	owner, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x62}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return Binding{Owner: owner, ContentID: "content", ContentVersion: "version", MediaType: "image/png", SourceSize: 6, RecipeID: "image-webp-q80-v1", Variant: 256}
}

func internalArtifact(generationID string, variant int) Artifact {
	data := OnePixelWebP()
	sum := sha256.Sum256(data)
	return Artifact{GenerationID: generationID, Variant: variant, Width: 1, Height: 1, ContentType: ContentTypeWebP, Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(sum[:]), CRC32C: ChecksumCRC32C(data), Bytes: data}
}

func operationIDsInSameShard() (domain.OperationID, domain.OperationID) {
	first := domain.OperationID("same-shard-first")
	return first, operationIDForShard(operationShard(first), "same-shard-second")
}

func operationIDForShard(shard, prefix string) domain.OperationID {
	for index := 0; ; index++ {
		candidate := domain.OperationID(fmt.Sprintf("%s-%d", prefix, index))
		if operationShard(candidate) == shard {
			return candidate
		}
	}
}

func operationShard(operationID domain.OperationID) string {
	digest := sha256.Sum256([]byte(operationID))
	return strconv.Itoa(int(digest[0]) % operationIndexShards)
}

type compareAndSwapFailureStore struct{ state.AtomicStore }

func (*compareAndSwapFailureStore) CompareAndSwap(context.Context, state.Key, state.Version, []byte) (state.Version, error) {
	return "", domain.NewError(domain.ErrorPreconditionFailed, "injected contention")
}

func (*compareAndSwapFailureStore) Mutate(context.Context, state.Mutation) (state.MutationOutcome, error) {
	return state.MutationOutcome{}, domain.NewError(domain.ErrorPreconditionFailed, "injected contention")
}

type compareAndSwapErrorStore struct {
	state.AtomicStore
	err error
}

func (s *compareAndSwapErrorStore) CompareAndSwap(context.Context, state.Key, state.Version, []byte) (state.Version, error) {
	return "", s.err
}

func (s *compareAndSwapErrorStore) Mutate(context.Context, state.Mutation) (state.MutationOutcome, error) {
	return state.MutationOutcome{}, s.err
}

type getFailureStore struct {
	state.AtomicStore
	err error
}

type nthCreateFailureStore struct {
	state.AtomicStore
	failAt  int
	creates int
	mutates int
}

func (s *nthCreateFailureStore) Mutate(ctx context.Context, mutation state.Mutation) (state.MutationOutcome, error) {
	s.mutates++
	if s.mutates == s.failAt {
		return state.MutationOutcome{}, domain.NewError(domain.ErrorUnavailable, "injected atomic mutation failure")
	}
	return s.AtomicStore.Mutate(ctx, mutation)
}

func (s *nthCreateFailureStore) Create(ctx context.Context, key state.Key, body []byte) (state.Version, error) {
	s.creates++
	if s.creates == s.failAt {
		return "", domain.NewError(domain.ErrorUnavailable, "injected create failure")
	}
	return s.AtomicStore.Create(ctx, key, body)
}

func (s *getFailureStore) Get(context.Context, state.Key) (state.Value, error) {
	return state.Value{}, s.err
}

func seedDurableOperation(t *testing.T, store state.Store, owner domain.UserID, idempotencyKey string, record operationRecord) {
	t.Helper()
	seedOperationRecord(t, store, owner, record)
	digestBytes := sha256.Sum256([]byte(owner.String() + "\x00" + idempotencyKey))
	digest := base64.RawURLEncoding.EncodeToString(digestBytes[:])
	body, err := state.EncodeJSON(idempotencyRecord{SchemaVersion: 1, Fingerprint: record.Fingerprint, OperationID: record.Operation.ID, ExpiresAt: record.ExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), state.MustKey(state.NamespaceIdempotency, "preview", owner.String(), digest), body); err != nil {
		t.Fatal(err)
	}
}

func seedOperationRecord(t *testing.T, store state.Store, owner domain.UserID, record operationRecord) {
	t.Helper()
	body, err := state.EncodeJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), previewOperationKey(owner, record.Operation.ID), body); err != nil {
		t.Fatal(err)
	}
}
