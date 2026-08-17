package portable

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestPortableDirectoryEntryValidationMatrix(t *testing.T) {
	now := time.Date(2043, 1, 2, 3, 4, 5, 0, time.UTC)
	validDirectory := storageformat.DirectoryEntry{Name: "dir", NameDigest: storageformat.NameDigest("dir"), Kind: domain.EntryDirectory, DirectoryID: "directory", ModifiedAt: now}
	validDirectory.LogicalVersion, _ = directoryEntryVersion(validDirectory)
	validFile := storageformat.DirectoryEntry{Name: "file", NameDigest: storageformat.NameDigest("file"), Kind: domain.EntryFile, BlobID: "blob", Size: 3, MediaType: "text/plain", ModifiedAt: now}
	validFile.LogicalVersion, _ = directoryEntryVersion(validFile)
	legacyFile := storageformat.DirectoryEntry{Name: "legacy", NameDigest: storageformat.NameDigest("legacy"), Kind: domain.EntryFile, BlobID: "legacy-blob", Size: 3, MediaType: "text/plain", ModifiedAt: now}
	legacyFile.LogicalVersion, _ = directoryEntryVersion(legacyFile)
	valid := replaceDirectoryEntry([]storageformat.DirectoryEntry{validDirectory}, nil, validFile)
	if err := validateDirectoryEntries(valid); err != nil {
		t.Fatalf("valid entries error = %v", err)
	}
	if err := validateDirectoryEntries([]storageformat.DirectoryEntry{legacyFile}); err != nil {
		t.Fatalf("pre-v1.1 canonical entry error = %v", err)
	}
	legacyIdentity := domainEntry(domain.MustParseUserPath("/legacy"), legacyFile).PreviewContentIdentity()
	legacyFile.Name = "renamed"
	legacyFile.NameDigest = storageformat.NameDigest(legacyFile.Name)
	legacyFile.LogicalVersion, _ = directoryEntryVersion(legacyFile)
	if renamed := domainEntry(domain.MustParseUserPath("/renamed"), legacyFile).PreviewContentIdentity(); renamed != legacyIdentity || renamed.ContentID == "" || renamed.ContentVersion == "" || renamed.ContentModifiedAt.IsZero() {
		t.Fatalf("derived legacy identity changed on rename: before=%+v after=%+v", legacyIdentity, renamed)
	}

	invalid := []storageformat.DirectoryEntry{
		{},
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.NameDigest = "wrong" }),
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.LogicalVersion = "" }),
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.Size = -1 }),
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.ModifiedAt = time.Time{} }),
		withEntry(validDirectory, func(entry *storageformat.DirectoryEntry) { entry.DirectoryID = "" }),
		withEntry(validDirectory, func(entry *storageformat.DirectoryEntry) { entry.BlobID = "blob" }),
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.BlobID = "" }),
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.DirectoryID = "directory" }),
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.MediaType = "" }),
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.Kind = "link" }),
		withEntry(validFile, func(entry *storageformat.DirectoryEntry) { entry.LogicalVersion = "wrong" }),
	}
	for index, entry := range invalid {
		if entry.Name != "" && entry.NameDigest == storageformat.NameDigest(entry.Name) && !entry.ModifiedAt.IsZero() && entry.Size >= 0 && entry.LogicalVersion != "wrong" && entry.LogicalVersion != "" {
			entry.LogicalVersion, _ = directoryEntryVersion(entry)
		}
		if err := validateDirectoryEntries([]storageformat.DirectoryEntry{entry}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid entry %d error = %v", index, err)
		}
	}
	if err := validateDirectoryEntries([]storageformat.DirectoryEntry{validFile, validDirectory}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unsorted entries error = %v", err)
	}
}

func TestPortableDirectoryAndCursorHelpers(t *testing.T) {
	now := time.Date(2043, 1, 2, 3, 4, 5, 0, time.UTC)
	entry := storageformat.DirectoryEntry{Name: "report.txt", NameDigest: storageformat.NameDigest("report.txt"), Kind: domain.EntryFile, BlobID: "blob", Size: 5, MediaType: "text/plain", ModifiedAt: now}
	entry.LogicalVersion, _ = directoryEntryVersion(entry)
	path := domain.MustParseUserPath("/report.txt")

	if _, _, err := resolveDirectoryDestination(path, domain.ConflictFail, "", []storageformat.DirectoryEntry{entry}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("fail conflict error = %v", err)
	}
	if _, _, err := resolveDirectoryDestination(path, domain.ConflictReplace, "stale", []storageformat.DirectoryEntry{entry}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("replace conflict error = %v", err)
	}
	resolved, existing, err := resolveDirectoryDestination(path, domain.ConflictReplace, domain.Version(entry.LogicalVersion), []storageformat.DirectoryEntry{entry})
	if err != nil || resolved != path || existing == nil {
		t.Fatalf("replace resolution = %v, %+v, %v", resolved, existing, err)
	}
	rename, _, err := resolveDirectoryDestination(path, domain.ConflictRename, "", []storageformat.DirectoryEntry{entry})
	if err != nil || rename.String() != "/report (1).txt" {
		t.Fatalf("rename resolution = %v, %v", rename, err)
	}
	if _, _, err := resolveDirectoryDestination(path, "unknown", "", []storageformat.DirectoryEntry{entry}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid conflict error = %v", err)
	}
	missing := domain.MustParseUserPath("/missing")
	if resolved, existing, err := resolveDirectoryDestination(missing, domain.ConflictFail, "", []storageformat.DirectoryEntry{entry}); err != nil || resolved != missing || existing != nil {
		t.Fatalf("missing resolution = %v, %+v, %v", resolved, existing, err)
	}

	longName := strings.Repeat("é", 120) + ".txt"
	longPath := domain.MustParseUserPath("/" + longName)
	longEntries := []storageformat.DirectoryEntry{{Name: longName}}
	longRename, err := availableDirectoryName(longPath, longEntries)
	if err != nil || len(longRename.Name()) > 255 || !strings.HasSuffix(longRename.Name(), " (1).txt") {
		t.Fatalf("long rename = %q, %v", longRename.String(), err)
	}

	cursor := listCursor{SchemaVersion: 1, UserID: "user", Area: "live", DirectoryPath: "/", DirectoryID: "root", ManifestID: "manifest", PageSize: 2, Sort: domain.SortName, Index: 1}
	encoded, err := encodeListCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeListCursor(encoded)
	if err != nil || decoded != cursor {
		t.Fatalf("cursor round trip = %+v, %v", decoded, err)
	}
	for _, invalid := range []string{"%", base64.RawURLEncoding.EncodeToString([]byte(`{"schemaVersion":1} `)), base64.RawURLEncoding.EncodeToString([]byte(`{"schemaVersion":2}`))} {
		if _, err := decodeListCursor(invalid); err == nil {
			t.Fatalf("decodeListCursor(%q) unexpectedly succeeded", invalid)
		}
	}
	if size, err := normalizeFilePageSize(0); err != nil || size != 200 {
		t.Fatalf("default page size = %d, %v", size, err)
	}
	for _, size := range []int{-1, 1001} {
		if _, err := normalizeFilePageSize(size); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid page size %d error = %v", size, err)
		}
	}
	if size, err := normalizeFilePageSize(1000); err != nil || size != 1000 {
		t.Fatalf("page size = %d, %v", size, err)
	}
}

func TestPortableSortingAndOperationHelpers(t *testing.T) {
	now := time.Date(2043, 1, 2, 3, 4, 5, 0, time.UTC)
	entries := []domain.Entry{
		{Path: domain.MustParseUserPath("/b"), Name: "b", Kind: domain.EntryFile, Size: 1, ModifiedAt: now.Add(time.Minute)},
		{Path: domain.MustParseUserPath("/a"), Name: "a", Kind: domain.EntryDirectory, Size: 2, ModifiedAt: now},
	}
	for _, field := range []domain.SortField{domain.SortName, domain.SortModified, domain.SortSize, domain.SortKind} {
		copyEntries := append([]domain.Entry(nil), entries...)
		sortDomainEntries(copyEntries, field, false)
		sortDomainEntries(copyEntries, field, true)
	}
	if !validSort(domain.SortName) || !validSort(domain.SortModified) || !validSort(domain.SortSize) || !validSort(domain.SortKind) || validSort("unknown") {
		t.Fatal("sort field validation mismatch")
	}
	if areaName(domain.AreaLive) != "live" || areaName(domain.AreaTrash) != "trash" {
		t.Fatal("area name mismatch")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateFileRequest(canceled, domain.Scope{}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("canceled validation error = %v", err)
	}
	if err := validateFileRequest(context.Background(), domain.Scope{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("scope validation error = %v", err)
	}

	objects, err := normalizeMutationObjects([]storageformat.MutationObject{{Key: "b", Body: []byte("2")}, {Key: "a", Body: []byte("1")}, {Key: "a", Body: []byte("1")}})
	if err != nil || len(objects) != 2 || objects[0].Key != "a" {
		t.Fatalf("normalized objects = %+v, %v", objects, err)
	}
	for _, invalid := range [][]storageformat.MutationObject{{{Key: "", Body: []byte("x")}}, {{Key: "a"}}, {{Key: "a", Body: []byte("x")}, {Key: "a", Body: []byte("y")}}} {
		if _, err := normalizeMutationObjects(invalid); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid prerequisites error = %v", err)
		}
	}
	for _, key := range []string{"ok", "", strings.Repeat("x", 128)} {
		if err := validatePortableIdempotencyKey(key); err != nil {
			t.Fatalf("valid idempotency key error = %v", err)
		}
	}
	for _, key := range []string{strings.Repeat("x", 129), "line\nbreak", string([]byte{0xff})} {
		if err := validatePortableIdempotencyKey(key); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid idempotency key error = %v", err)
		}
	}

	for state, expected := range map[storageformat.FileOperationState]domain.OperationState{
		storageformat.FileOperationRunning:   domain.OperationRunning,
		storageformat.FileOperationCommitted: domain.OperationRunning,
		storageformat.FileOperationSucceeded: domain.OperationSucceeded,
		storageformat.FileOperationFailed:    domain.OperationFailed,
	} {
		operation := domainFileOperation(storageformat.FileOperation{OperationID: "op", State: state, ErrorKind: domain.ErrorConflict, Error: "message", StartedAt: now, UpdatedAt: now})
		if operation.State != expected || operation.ID != "op" {
			t.Fatalf("operation mapping = %+v", operation)
		}
	}
	if retained := removeDirectoryEntry([]storageformat.DirectoryEntry{{Name: "a"}, {Name: "b"}}, "a"); len(retained) != 1 || retained[0].Name != "b" {
		t.Fatalf("removeDirectoryEntry = %+v", retained)
	}
	for _, key := range []string{"endlessfs/v1/admissions/a", "endlessfs/v1/staging/a", "endlessfs/v1/leases/a", "endlessfs/v1/checkpoints/a"} {
		if !transientOrCheckpoint(key) {
			t.Fatalf("%q should be transient", key)
		}
	}
	if transientOrCheckpoint("endlessfs/v1/users/a") {
		t.Fatal("authoritative object treated as transient")
	}
}

func TestPortableOpenAndCheckpointInputMatrix(t *testing.T) {
	base := Options{
		Backend: objectmemory.New(), Clock: domain.NewFixedClock(time.Now().UTC()), IDs: domain.NewIDGenerator(strings.NewReader(strings.Repeat("x", 4096))),
		Writer:   WriterConfiguration{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: []byte("0123456789abcdef0123456789abcdef"),
	}
	for name, mutate := range map[string]func(*Options){
		"backend":      func(options *Options) { options.Backend = nil },
		"clock":        func(options *Options) { options.Clock = nil },
		"ids":          func(options *Options) { options.IDs = nil },
		"lease":        func(options *Options) { options.LeaseTTL = 0 },
		"cursor-key":   func(options *Options) { options.CursorKey = []byte("short") },
		"upload-ttl":   func(options *Options) { options.UploadTTL = -time.Second },
		"download-ttl": func(options *Options) { options.DownloadTTL = 11 * time.Minute },
		"cursor-ttl":   func(options *Options) { options.CursorTTL = 2 * time.Hour },
	} {
		t.Run(name, func(t *testing.T) {
			options := base
			mutate(&options)
			if _, err := Open(context.Background(), options); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("Open() error = %v", err)
			}
		})
	}
	for _, writer := range []WriterConfiguration{
		{},
		{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{""}},
		{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key", "key"}},
		{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}, RequiredFeatures: []string{""}},
		{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}, RequiredFeatures: []string{"feature", "feature"}},
	} {
		if _, err := canonicalWriterConfiguration(writer); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid writer error = %v", err)
		}
	}
	if err := VerifyCheckpointReadOnly(context.Background(), nil, base.Writer, "checkpoint"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("nil backend verification error = %v", err)
	}
	if err := VerifyCheckpointReadOnly(context.Background(), base.Backend, base.Writer, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty checkpoint verification error = %v", err)
	}
	engine := &Engine{backend: base.Backend}
	if _, err := engine.readCheckpoint(context.Background(), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty checkpoint read error = %v", err)
	}
	if engine.step(context.Background(), "noop") != nil {
		t.Fatal("nil scheduler failed")
	}
	want := errors.New("step failure")
	engine.scheduler = SchedulerFunc(func(context.Context, string) error { return want })
	if !errors.Is(engine.step(context.Background(), "test"), want) {
		t.Fatal("scheduler error was not returned")
	}
}

func TestUploadRecoveryMarksDurableRecordAborted(t *testing.T) {
	backend := objectmemory.New()
	files := &FileStore{engine: &Engine{backend: backend}}
	key := storageformat.OperationKey("UVFRUVFRUVFRUVFRUVFRUQ", "upload")
	record := storageformat.UploadRecord{SchemaVersion: 1, UploadID: "upload", State: storageformat.UploadActive}
	body, err := storageformat.EncodeEnvelope(uploadRecordSchema, key, 1, record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	files.markUploadAborted(context.Background(), key)
	object, err := backend.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var envelope storageformat.Envelope
	var stored storageformat.UploadRecord
	if err := storageformat.DecodeEnvelope(object.Body, key, uploadRecordSchema, &envelope, &stored); err != nil || stored.State != storageformat.UploadAborted || envelope.Revision != 2 {
		t.Fatalf("stored upload = %+v, %+v, %v", envelope, stored, err)
	}

	files.markUploadAborted(context.Background(), storageformat.OperationKey("UVFRUVFRUVFRUVFRUVFRUQ", "missing"))
	malformedKey := storageformat.OperationKey("UVFRUVFRUVFRUVFRUVFRUQ", "malformed")
	if _, err := backend.Put(context.Background(), malformedKey, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	files.markUploadAborted(context.Background(), malformedKey)
	files.markUploadAborted(context.Background(), key)
}

func TestCreateUploadRequiresDirectTransferBackend(t *testing.T) {
	backend := metadataOnlyBackend{Backend: objectmemory.New()}
	clock := domain.NewFixedClock(time.Date(2043, 2, 3, 4, 5, 6, 0, time.UTC))
	engine, err := Open(context.Background(), Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(strings.NewReader(strings.Repeat("z", 4096))),
		Writer:   WriterConfiguration{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	user, _ := domain.ParseUserID("UVFRUVFRUVFRUVFRUVFRUQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	if _, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: domain.MustParseUserPath("/file.txt"), Size: 1, MediaType: "text/plain"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("CreateUpload() error = %v", err)
	}
}

func TestPortableStateCorruptionAndCursorMatrixFailsClosed(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2045, 1, 2, 3, 4, 5, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("r", 1<<20)))
	key := state.MustKey(state.NamespaceAccounts, "corruption")
	version, err := engine.Create(context.Background(), key, []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	objectKey := canonicalStateKey(key)
	original, err := backend.Get(context.Background(), objectKey)
	if err != nil {
		t.Fatal(err)
	}
	replaceInternalObject(t, backend, objectKey, original.Version, []byte("not-json"))
	if _, err := engine.Get(context.Background(), key); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Get(corrupt) error = %v", err)
	}
	if _, err := engine.CompareAndSwap(context.Background(), key, version, []byte("updated")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CompareAndSwap(corrupt) error = %v", err)
	}
	if err := engine.Delete(context.Background(), key, version); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Delete(corrupt) error = %v", err)
	}
	if _, err := engine.List(context.Background(), state.MustPrefix(state.NamespaceAccounts), state.PageRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List(corrupt) error = %v", err)
	}

	backend = objectmemory.New()
	engine = openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("s", 1<<20)))
	key = state.MustKey(state.NamespaceAccounts, "collision")
	objectKey = canonicalStateKey(key)
	body, err := storageformat.EncodeEnvelope(stateRecordSchema, objectKey, 1, storageformat.StateRecord{SchemaVersion: 1, LogicalKey: state.MustKey(state.NamespaceAccounts, "other").String(), Data: []byte("value")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), objectKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Get(context.Background(), key); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Get(collision) error = %v", err)
	}
	if _, err := engine.List(context.Background(), state.MustPrefix(state.NamespaceAccounts), state.PageRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List(collision) error = %v", err)
	}

	validRecord := storageformat.StateVersionRecord{SchemaVersion: 1, LogicalKey: key.String(), LogicalVersion: "version", Data: []byte("snapshot")}
	validSnapshotKey := storageformat.StateVersionKey("accounts", key.String(), validRecord.LogicalVersion)
	validSnapshotBody, err := storageformat.EncodeEnvelope(stateVersionSchema, validSnapshotKey, 1, validRecord)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), validSnapshotKey, validSnapshotBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	baseCursor := stateListCursor{SchemaVersion: 1, Prefix: "accounts/", Limit: 1, Index: 0, GateEpoch: 1, GateVersion: "gate", ExpiresAt: clock.Now().Add(time.Minute), Snapshots: []string{validSnapshotKey.String()}}
	for name, cursor := range map[string]stateListCursor{
		"invalid-key": withStateCursor(baseCursor, func(value *stateListCursor) { value.Snapshots[0] = "INVALID" }),
		"missing": withStateCursor(baseCursor, func(value *stateListCursor) {
			value.Snapshots[0] = storageformat.StateVersionKey("accounts", "accounts/bWlzc2luZw", "version").String()
		}),
		"bad-envelope": withStateCursor(baseCursor, func(value *stateListCursor) {
			value.Snapshots[0] = storageformat.StateVersionKey("accounts", "accounts/YmFk", "version").String()
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "bad-envelope" {
				badKey := objectstore.MustKey(cursor.Snapshots[0])
				if _, err := backend.Put(context.Background(), badKey, []byte("not-json"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := engine.stateCursorPage(context.Background(), cursor); !errors.Is(err, domain.ErrInvalid) && !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("stateCursorPage() error = %v", err)
			}
		})
	}

	invalidRecordKey := storageformat.StateVersionKey("accounts", key.String(), "invalid")
	invalidRecordBody, err := storageformat.EncodeEnvelope(stateVersionSchema, invalidRecordKey, 1, storageformat.StateVersionRecord{SchemaVersion: 2, LogicalKey: key.String(), LogicalVersion: "invalid", Data: []byte("snapshot")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(context.Background(), invalidRecordKey, invalidRecordBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	invalidCursor := baseCursor
	invalidCursor.Snapshots = []string{invalidRecordKey.String()}
	if _, err := engine.stateCursorPage(context.Background(), invalidCursor); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid snapshot error = %v", err)
	}

	encoded, err := engine.encodeStateListCursor(baseCursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.decodeStateListCursor(encoded); err != nil {
		t.Fatal(err)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := engine.decodeStateListCursor(base64.RawURLEncoding.EncodeToString(sealed)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("tampered cursor error = %v", err)
	}
	if _, err := engine.decodeStateListCursor("%"); err == nil {
		t.Fatal("invalid base64 cursor succeeded")
	}
	invalidCursorBody, err := storageformat.EncodeCanonical(withStateCursor(baseCursor, func(value *stateListCursor) { value.SchemaVersion = 2 }))
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{0x33}, engine.cursorAEAD.NonceSize())
	invalidCursorSealed := engine.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, invalidCursorBody, []byte("endlessfs-state-cursor-v1"))
	if _, err := engine.decodeStateListCursor(base64.RawURLEncoding.EncodeToString(invalidCursorSealed)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid cursor schema error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader("short"))
	if _, err := engine.encodeStateListCursor(baseCursor); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("cursor randomness error = %v", err)
	}
	if err := validateStateMutation(key, bytes.Repeat([]byte("x"), state.MaxRecordBytes+1)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized state mutation error = %v", err)
	}
	if _, err := parseExistingStateKey("accounts/%"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid stored key error = %v", err)
	}
}

func TestPortableDirectoryManifestCorruptionMatrixFailsClosed(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2045, 2, 3, 4, 5, 6, 0, time.UTC))
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("d", 1<<20)))
	user, _ := domain.ParseUserID("ZGRkZGRkZGRkZGRkZGRkZA")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/child")}); err != nil {
		t.Fatal(err)
	}
	fixture := backend.Export()
	rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
	var rootEnvelope storageformat.Envelope
	var root storageformat.DirectoryRoot
	if err := storageformat.DecodeEnvelope(fixture[rootKey.String()], rootKey, directoryRootSchema, &rootEnvelope, &root); err != nil {
		t.Fatal(err)
	}
	manifestKey := storageformat.DirectoryManifestKey(user.String(), "live", storageformat.RootDirectoryID, root.ManifestID)
	var manifestEnvelope storageformat.Envelope
	var manifest storageformat.DirectoryManifest
	if err := storageformat.DecodeEnvelope(fixture[manifestKey.String()], manifestKey, directoryManifestSchema, &manifestEnvelope, &manifest); err != nil {
		t.Fatal(err)
	}
	pageKey := storageformat.DirectoryPageKey(user.String(), "live", storageformat.RootDirectoryID, manifest.PageIDs[0])
	var pageEnvelope storageformat.Envelope
	var page storageformat.DirectoryPage
	if err := storageformat.DecodeEnvelope(fixture[pageKey.String()], pageKey, directoryPageSchema, &pageEnvelope, &page); err != nil {
		t.Fatal(err)
	}

	corruptions := map[string]func(map[string][]byte){
		"root-envelope": func(objects map[string][]byte) { objects[rootKey.String()] = []byte("not-json") },
		"root-fields": func(objects map[string][]byte) {
			invalid := root
			invalid.DirectoryID = "other"
			objects[rootKey.String()] = encodeInternalEnvelope(t, directoryRootSchema, rootKey, rootEnvelope.Revision, invalid)
		},
		"pending-fields": func(objects map[string][]byte) {
			invalid := root
			invalid.Pending = &storageformat.DirectoryTransition{OperationID: "", Fence: 0, PreManifestID: root.ManifestID, PostManifestID: "post"}
			objects[rootKey.String()] = encodeInternalEnvelope(t, directoryRootSchema, rootKey, rootEnvelope.Revision, invalid)
		},
		"pending-operation": func(objects map[string][]byte) {
			invalid := root
			invalid.Pending = &storageformat.DirectoryTransition{OperationID: "missing", Fence: 1, PreManifestID: root.ManifestID, PostManifestID: "post"}
			objects[rootKey.String()] = encodeInternalEnvelope(t, directoryRootSchema, rootKey, rootEnvelope.Revision, invalid)
		},
		"manifest-envelope": func(objects map[string][]byte) { objects[manifestKey.String()] = []byte("not-json") },
		"manifest-fields": func(objects map[string][]byte) {
			invalid := manifest
			invalid.EntryCount = -1
			objects[manifestKey.String()] = encodeInternalEnvelope(t, directoryManifestSchema, manifestKey, manifestEnvelope.Revision, invalid)
		},
		"missing-page":  func(objects map[string][]byte) { delete(objects, pageKey.String()) },
		"page-envelope": func(objects map[string][]byte) { objects[pageKey.String()] = []byte("not-json") },
		"page-fields": func(objects map[string][]byte) {
			invalid := page
			invalid.DirectoryID = "other"
			objects[pageKey.String()] = encodeInternalEnvelope(t, directoryPageSchema, pageKey, pageEnvelope.Revision, invalid)
		},
		"entry-count": func(objects map[string][]byte) {
			invalid := manifest
			invalid.EntryCount++
			objects[manifestKey.String()] = encodeInternalEnvelope(t, directoryManifestSchema, manifestKey, manifestEnvelope.Revision, invalid)
		},
		"entry-value": func(objects map[string][]byte) {
			invalid := page
			invalid.Entries = append([]storageformat.DirectoryEntry(nil), page.Entries...)
			invalid.Entries[0].LogicalVersion = "wrong"
			objects[pageKey.String()] = encodeInternalEnvelope(t, directoryPageSchema, pageKey, pageEnvelope.Revision, invalid)
		},
		"entry-name": func(objects map[string][]byte) {
			invalid := page
			invalid.Entries = append([]storageformat.DirectoryEntry(nil), page.Entries...)
			invalid.Entries[0].Name = "/"
			invalid.Entries[0].NameDigest = storageformat.NameDigest("/")
			invalid.Entries[0].LogicalVersion, _ = directoryEntryVersion(invalid.Entries[0])
			objects[pageKey.String()] = encodeInternalEnvelope(t, directoryPageSchema, pageKey, pageEnvelope.Revision, invalid)
		},
	}
	for name, corrupt := range corruptions {
		t.Run(name, func(t *testing.T) {
			objects := cloneInternalObjects(fixture)
			corrupt(objects)
			candidateBackend := objectmemory.New()
			if err := candidateBackend.Import(objects); err != nil {
				t.Fatal(err)
			}
			candidate := openInternalTestEngine(t, candidateBackend, clock, strings.NewReader(strings.Repeat(name, 1<<16)))
			_, err := candidate.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/")})
			if err == nil {
				t.Fatal("corrupted directory unexpectedly listed")
			}
			if !errors.Is(err, domain.ErrInvalid) && !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("corruption error = %v", err)
			}
		})
	}

	missingManifestCursor, err := encodeListCursor(listCursor{SchemaVersion: 1, UserID: user.String(), Area: "live", DirectoryPath: "/", DirectoryID: storageformat.RootDirectoryID, ManifestID: "missing", PageSize: 200, Sort: domain.SortName, Index: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), Cursor: missingManifestCursor}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing cursor manifest error = %v", err)
	}
	offsetCursor, err := encodeListCursor(listCursor{SchemaVersion: 1, UserID: user.String(), Area: "live", DirectoryPath: "/", DirectoryID: storageformat.RootDirectoryID, ManifestID: root.ManifestID, PageSize: 200, Sort: domain.SortName, Index: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), Cursor: offsetCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("cursor offset error = %v", err)
	}
	endCursor, err := encodeListCursor(listCursor{SchemaVersion: 1, UserID: user.String(), Area: "live", DirectoryPath: "/", DirectoryID: storageformat.RootDirectoryID, ManifestID: root.ManifestID, PageSize: 200, Sort: domain.SortName, Index: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page, err := engine.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/"), Cursor: endCursor}); err != nil || len(page.Entries) != 0 {
		t.Fatalf("terminal cursor page = %+v, %v", page, err)
	}
	if _, err := engine.Files().List(context.Background(), domain.Scope{}, domain.ListRequest{Directory: domain.MustParseUserPath("/")}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("List(invalid scope) error = %v", err)
	}
	if _, err := engine.Files().Stat(context.Background(), domain.Scope{}, domain.MustParseUserPath("/")); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("Stat(invalid scope) error = %v", err)
	}

	if _, err := engine.Get(context.Background(), state.Key{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Get(invalid key) error = %v", err)
	}
	if _, err := engine.CompareAndSwap(context.Background(), state.Key{}, "version", nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CompareAndSwap(invalid key) error = %v", err)
	}
	if err := engine.Delete(context.Background(), state.Key{}, "version"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Delete(invalid key) error = %v", err)
	}
	if _, err := engine.List(context.Background(), state.MustPrefix(state.NamespaceAccounts), state.PageRequest{Limit: 1001}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("List(invalid limit) error = %v", err)
	}

	for name, random := range map[string]string{"child-id": "", "parent-manifest-id": strings.Repeat("x", 16), "parent-page-id": strings.Repeat("x", 32), "child-manifest-id": strings.Repeat("x", 48)} {
		t.Run(name, func(t *testing.T) {
			candidateBackend := objectmemory.New()
			candidate := openInternalTestEngine(t, candidateBackend, clock, strings.NewReader(strings.Repeat("i", 4096)))
			candidate.ids = domain.NewIDGenerator(strings.NewReader(random))
			if _, err := candidate.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: domain.MustParseUserPath("/failure")}); !errors.Is(err, domain.ErrInternal) {
				t.Fatalf("CreateDirectory() randomness error = %v", err)
			}
		})
	}

	fileBackend := objectmemory.New()
	fileEngine := openInternalTestEngine(t, fileBackend, clock, strings.NewReader(strings.Repeat("f", 1<<20)))
	fileEntry := storageformat.DirectoryEntry{Name: "file", NameDigest: storageformat.NameDigest("file"), Kind: domain.EntryFile, BlobID: "blob", Size: 1, MediaType: "text/plain", ModifiedAt: clock.Now()}
	fileEntry.LogicalVersion, _ = directoryEntryVersion(fileEntry)
	prepared, err := fileEngine.Files().prepareDirectory(context.Background(), scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{fileEntry}, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, prerequisite := range prepared.prerequisites {
		if _, err := fileBackend.Put(context.Background(), objectstore.MustKey(prerequisite.Key), prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fileBackend.Put(context.Background(), storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID), prepared.rootBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fileEngine.Files().Stat(context.Background(), scope, domain.MustParseUserPath("/file/child")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Stat(under file) error = %v", err)
	}
	if _, err := fileEngine.Files().List(context.Background(), scope, domain.ListRequest{Directory: domain.MustParseUserPath("/file")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("List(file as directory) error = %v", err)
	}
	if _, err := fileEngine.Files().prepareDirectory(context.Background(), scope, "invalid", []storageformat.DirectoryEntry{{}}, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("prepareDirectory(invalid entry) error = %v", err)
	}
	largeEntries := make([]storageformat.DirectoryEntry, 0, 2)
	for _, name := range []string{"large-a", "large-b"} {
		entry := storageformat.DirectoryEntry{Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile, BlobID: name, Size: 1, MediaType: strings.Repeat("x", storageformat.MaxCanonicalBytes/2+1024), ModifiedAt: clock.Now()}
		entry.LogicalVersion, err = directoryEntryVersion(entry)
		if err != nil {
			t.Fatal(err)
		}
		largeEntries = replaceDirectoryEntry(largeEntries, nil, entry)
	}
	if _, err := fileEngine.Files().prepareDirectory(context.Background(), scope, "large", largeEntries, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("prepareDirectory(oversized page) error = %v", err)
	}
	tooLargeEntry := fileEntry
	tooLargeEntry.MediaType = strings.Repeat("x", storageformat.MaxCanonicalBytes+1)
	if _, err := directoryEntryVersion(tooLargeEntry); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("directoryEntryVersion(oversized) error = %v", err)
	}

	entries := make([]storageformat.DirectoryEntry, 0, 10_000)
	for index := 1; index <= 10_000; index++ {
		entries = append(entries, storageformat.DirectoryEntry{Name: fmt.Sprintf("name (%d).txt", index)})
	}
	if _, err := availableDirectoryName(domain.MustParseUserPath("/name.txt"), entries); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("exhausted rename error = %v", err)
	}
	existing := storageformat.DirectoryEntry{Name: "old", NameDigest: "old"}
	if replaced := replaceDirectoryEntry([]storageformat.DirectoryEntry{existing}, &existing, storageformat.DirectoryEntry{Name: "new", NameDigest: "new"}); len(replaced) != 1 || replaced[0].Name != "new" {
		t.Fatalf("replacement = %+v", replaced)
	}
	same := []domain.Entry{
		{Path: domain.MustParseUserPath("/b"), Name: "same", Kind: domain.EntryFile, Size: 1, ModifiedAt: clock.Now()},
		{Path: domain.MustParseUserPath("/a"), Name: "same", Kind: domain.EntryFile, Size: 1, ModifiedAt: clock.Now()},
	}
	for _, field := range []domain.SortField{domain.SortModified, domain.SortSize, domain.SortKind, domain.SortName} {
		values := append([]domain.Entry(nil), same...)
		sortDomainEntries(values, field, true)
	}
	longPath := domain.MustParseUserPath("/" + strings.Repeat("x", 250) + ".txt")
	if renamed, err := availableDirectoryName(longPath, nil); err != nil || len(renamed.Name()) > 255 {
		t.Fatalf("truncated rename = %q, %v", renamed.String(), err)
	}
	equalDigest := "same"
	_ = replaceDirectoryEntry([]storageformat.DirectoryEntry{{Name: "b", NameDigest: equalDigest}}, nil, storageformat.DirectoryEntry{Name: "a", NameDigest: equalDigest})
	if _, err := decodeListCursor(base64.RawURLEncoding.EncodeToString([]byte(`{"schemaVersion":2}`))); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid list cursor schema error = %v", err)
	}
	if err := decodeCanonicalValue([]byte("not-json"), &listCursor{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid canonical value error = %v", err)
	}
}

func TestPortableTransferDurableRecordAndCapabilityMatrix(t *testing.T) {
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	clock := domain.NewFixedClock(time.Date(2045, 3, 4, 5, 6, 7, 0, time.UTC))
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(strings.NewReader(strings.Repeat("p", 1<<20)))); err != nil {
		t.Fatal(err)
	}
	engine := openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat("u", 1<<20)))
	user, _ := domain.ParseUserID("ZWVlZWVlZWVlZWVlZWVlZQ")
	scope, _ := domain.NewScope(user, domain.AreaLive)
	request := domain.CreateUploadRequest{Path: domain.MustParseUserPath("/file.bin"), Size: 4, MediaType: "application/octet-stream", Resumable: true, IdempotencyKey: "durable-upload"}
	capability, err := engine.Files().CreateUpload(context.Background(), scope, request)
	if err != nil {
		t.Fatal(err)
	}
	uploadID := string(capability.UploadID)
	operationKey := storageformat.OperationKey(user.String(), uploadID)
	leaseKey := storageformat.LeaseKey(backend.BackendKind(), uploadID)
	operationObject, err := backend.Get(context.Background(), operationKey)
	if err != nil {
		t.Fatal(err)
	}
	var operationEnvelope storageformat.Envelope
	var record storageformat.UploadRecord
	if err := storageformat.DecodeEnvelope(operationObject.Body, operationKey, uploadRecordSchema, &operationEnvelope, &record); err != nil {
		t.Fatal(err)
	}
	leaseObject, err := backend.Get(context.Background(), leaseKey)
	if err != nil {
		t.Fatal(err)
	}
	var leaseEnvelope storageformat.Envelope
	var lease storageformat.TransferLease
	if err := storageformat.DecodeEnvelope(leaseObject.Body, leaseKey, transferLeaseSchema, &leaseEnvelope, &lease); err != nil {
		t.Fatal(err)
	}
	fixture := backend.Export()

	if _, err := engine.Files().CreateUpload(context.Background(), domain.Scope{}, request); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("CreateUpload(invalid scope) error = %v", err)
	}
	invalid := request
	invalid.MediaType = "bad media type"
	if _, err := engine.Files().CreateUpload(context.Background(), scope, invalid); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CreateUpload(invalid media type) error = %v", err)
	}
	invalid = request
	invalid.Conflict = "unknown"
	if _, err := engine.Files().CreateUpload(context.Background(), scope, invalid); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CreateUpload(invalid conflict) error = %v", err)
	}
	invalid = request
	invalid.IdempotencyKey = strings.Repeat("x", 129)
	if _, err := engine.Files().CreateUpload(context.Background(), scope, invalid); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CreateUpload(invalid idempotency key) error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader("short"))
	withoutIdempotency := request
	withoutIdempotency.IdempotencyKey = ""
	if _, err := engine.Files().CreateUpload(context.Background(), scope, withoutIdempotency); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("CreateUpload(randomness failure) error = %v", err)
	}
	engine.ids = domain.NewIDGenerator(strings.NewReader(strings.Repeat("v", 1<<20)))
	missingParent := request
	missingParent.Path = domain.MustParseUserPath("/missing/file.bin")
	missingParent.IdempotencyKey = ""
	if _, err := engine.Files().CreateUpload(context.Background(), scope, missingParent); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CreateUpload(missing parent) error = %v", err)
	}

	for name, mutate := range map[string]func(*storageformat.UploadRecord){
		"schema": func(value *storageformat.UploadRecord) { value.SchemaVersion = 2 },
		"upload": func(value *storageformat.UploadRecord) { value.UploadID = "other" },
		"state":  func(value *storageformat.UploadRecord) { value.State = "unknown" },
	} {
		t.Run("record-"+name, func(t *testing.T) {
			objects := cloneInternalObjects(fixture)
			invalidRecord := record
			mutate(&invalidRecord)
			objects[operationKey.String()] = encodeInternalEnvelope(t, uploadRecordSchema, operationKey, operationEnvelope.Revision, invalidRecord)
			candidateBackend, candidate := openInternalTransferFixture(t, objects, clock, name)
			_ = candidateBackend
			if _, _, _, err := candidate.Files().readUploadRecord(context.Background(), user, uploadID); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("readUploadRecord() error = %v", err)
			}
		})
	}

	objects := cloneInternalObjects(fixture)
	objects[operationKey.String()] = []byte("not-json")
	_, candidate := openInternalTransferFixture(t, objects, clock, "bad-record")
	if _, _, _, err := candidate.Files().readUploadRecord(context.Background(), user, uploadID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed upload record error = %v", err)
	}

	for name, mutate := range map[string]func(*storageformat.TransferLease, *storageformat.UploadRecord){
		"backend-binding": func(_ *storageformat.TransferLease, value *storageformat.UploadRecord) { value.BackendKind = "other" },
		"lease-schema":    func(value *storageformat.TransferLease, _ *storageformat.UploadRecord) { value.SchemaVersion = 2 },
		"lease-upload":    func(value *storageformat.TransferLease, _ *storageformat.UploadRecord) { value.UploadID = "other" },
		"lease-expiry": func(value *storageformat.TransferLease, _ *storageformat.UploadRecord) {
			value.ExpiresAt = value.ExpiresAt.Add(time.Second)
		},
		"lease-empty": func(value *storageformat.TransferLease, _ *storageformat.UploadRecord) { value.Ciphertext = nil },
	} {
		t.Run(name, func(t *testing.T) {
			objects := cloneInternalObjects(fixture)
			invalidLease, invalidRecord := lease, record
			mutate(&invalidLease, &invalidRecord)
			objects[operationKey.String()] = encodeInternalEnvelope(t, uploadRecordSchema, operationKey, operationEnvelope.Revision, invalidRecord)
			if name != "backend-binding" {
				objects[leaseKey.String()] = encodeInternalEnvelope(t, transferLeaseSchema, leaseKey, leaseEnvelope.Revision, invalidLease)
			}
			_, candidate := openInternalTransferFixture(t, objects, clock, name)
			_, _, err := candidate.Files().readTransferLease(context.Background(), invalidRecord)
			if err == nil {
				t.Fatal("corrupt transfer lease unexpectedly succeeded")
			}
		})
	}
	objects = cloneInternalObjects(fixture)
	objects[leaseKey.String()] = []byte("not-json")
	_, candidate = openInternalTransferFixture(t, objects, clock, "bad-lease")
	if _, _, err := candidate.Files().readTransferLease(context.Background(), record); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("malformed transfer lease error = %v", err)
	}

	for name, mutate := range map[string]func(map[string][]byte, *storageformat.UploadRecord){
		"missing-record": func(values map[string][]byte, _ *storageformat.UploadRecord) { delete(values, operationKey.String()) },
		"malformed-record": func(values map[string][]byte, _ *storageformat.UploadRecord) {
			values[operationKey.String()] = []byte("not-json")
		},
		"invalid-record":   func(values map[string][]byte, value *storageformat.UploadRecord) { value.State = "unknown" },
		"backend-mismatch": func(values map[string][]byte, value *storageformat.UploadRecord) { value.BackendKind = "other" },
		"existing-lease":   func(values map[string][]byte, _ *storageformat.UploadRecord) {},
		"expired": func(values map[string][]byte, value *storageformat.UploadRecord) {
			delete(values, leaseKey.String())
			value.ExpiresAt = clock.Now().Add(-time.Second)
		},
		"bad-staging": func(values map[string][]byte, value *storageformat.UploadRecord) {
			delete(values, leaseKey.String())
			value.StagingKey = "INVALID"
		},
		"begin-failure": func(values map[string][]byte, value *storageformat.UploadRecord) {
			delete(values, leaseKey.String())
			value.MediaType = ""
		},
	} {
		t.Run("recover-"+name, func(t *testing.T) {
			values := cloneInternalObjects(fixture)
			candidateRecord := record
			mutate(values, &candidateRecord)
			if _, found := values[operationKey.String()]; found && name != "malformed-record" {
				values[operationKey.String()] = encodeInternalEnvelope(t, uploadRecordSchema, operationKey, operationEnvelope.Revision, candidateRecord)
			}
			_, candidate := openInternalTransferFixture(t, values, clock, "recover-"+name)
			err := candidate.Files().recoverUploadLease(context.Background(), operationKey)
			if name == "existing-lease" || name == "expired" {
				if err != nil {
					t.Fatalf("recoverUploadLease() error = %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid recovery unexpectedly succeeded")
			}
		})
	}
	values := cloneInternalObjects(fixture)
	delete(values, leaseKey.String())
	rebuiltBackend, rebuilt := openInternalTransferFixture(t, values, clock, "recover-success")
	if err := rebuilt.Files().recoverUploadLease(context.Background(), operationKey); err != nil {
		t.Fatalf("recoverUploadLease(missing lease) error = %v", err)
	}
	if _, err := rebuiltBackend.Get(context.Background(), leaseKey); err != nil {
		t.Fatalf("recoverUploadLease did not persist lease: %v", err)
	}

	fingerprint := storageformat.Digest([]byte(fmt.Sprintf("upload\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%t", "live", request.Path.String(), request.Size, request.MediaType, domain.ConflictFail, request.ExpectedVersion, request.Resumable)))
	if _, found, err := engine.Files().lookupIdempotentUpload(context.Background(), user, request.IdempotencyKey, "different"); found || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed idempotent lookup = found %v, error %v", found, err)
	}
	if replayed, found, err := engine.Files().lookupIdempotentUpload(context.Background(), user, request.IdempotencyKey, fingerprint); err != nil || !found || replayed.UploadID != capability.UploadID {
		t.Fatalf("valid idempotent lookup = %+v, found %v, error %v", replayed, found, err)
	}
	idempotencyKey := storageformat.IdempotencyKey(user.String(), request.IdempotencyKey)
	idempotencyObject, err := backend.Get(context.Background(), idempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	var idempotencyEnvelope storageformat.Envelope
	var idempotency storageformat.IdempotencyRecord
	if err := storageformat.DecodeEnvelope(idempotencyObject.Body, idempotencyKey, idempotencySchema, &idempotencyEnvelope, &idempotency); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string][]byte, *storageformat.IdempotencyRecord, *storageformat.UploadRecord){
		"malformed": func(values map[string][]byte, _ *storageformat.IdempotencyRecord, _ *storageformat.UploadRecord) {
			values[idempotencyKey.String()] = []byte("not-json")
		},
		"binding": func(_ map[string][]byte, value *storageformat.IdempotencyRecord, _ *storageformat.UploadRecord) {
			value.Kind = "other"
		},
		"record-missing": func(values map[string][]byte, _ *storageformat.IdempotencyRecord, _ *storageformat.UploadRecord) {
			delete(values, operationKey.String())
		},
		"record-inactive": func(_ map[string][]byte, _ *storageformat.IdempotencyRecord, value *storageformat.UploadRecord) {
			value.State = storageformat.UploadAborted
		},
		"lease-missing": func(values map[string][]byte, _ *storageformat.IdempotencyRecord, _ *storageformat.UploadRecord) {
			delete(values, leaseKey.String())
		},
	} {
		t.Run("idempotency-"+name, func(t *testing.T) {
			values := cloneInternalObjects(fixture)
			candidateID, candidateRecord := idempotency, record
			mutate(values, &candidateID, &candidateRecord)
			if name != "malformed" {
				values[idempotencyKey.String()] = encodeInternalEnvelope(t, idempotencySchema, idempotencyKey, idempotencyEnvelope.Revision, candidateID)
			}
			if _, found := values[operationKey.String()]; found {
				values[operationKey.String()] = encodeInternalEnvelope(t, uploadRecordSchema, operationKey, operationEnvelope.Revision, candidateRecord)
			}
			_, candidate := openInternalTransferFixture(t, values, clock, "idempotency-"+name)
			if _, found, err := candidate.Files().lookupIdempotentUpload(context.Background(), user, request.IdempotencyKey, fingerprint); err == nil {
				t.Fatalf("lookupIdempotentUpload() = found %v, error %v", found, err)
			}
		})
	}
	_, imported := openInternalTransferFixture(t, fixture, clock, "resume-missing-session")
	if _, found, err := imported.Files().lookupIdempotentUpload(context.Background(), user, request.IdempotencyKey, fingerprint); !found || !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("imported idempotent lookup = found %v, error %v", found, err)
	}

	metadataEngine, err := Open(context.Background(), Options{
		Backend: metadataOnlyBackend{Backend: backend}, Clock: clock, IDs: domain.NewIDGenerator(strings.NewReader(strings.Repeat("m", 1<<20))),
		Writer:   WriterConfiguration{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x44}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := metadataEngine.Files().UploadStatus(context.Background(), scope, capability.UploadID); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("metadata-only UploadStatus() error = %v", err)
	}
	if _, err := metadataEngine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("metadata-only CompleteUpload() error = %v", err)
	}
	if err := metadataEngine.Files().recoverUploadLease(context.Background(), operationKey); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("metadata-only recoverUploadLease() error = %v", err)
	}
	if _, _, err := metadataEngine.Files().readTransferLease(context.Background(), record); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("metadata-only readTransferLease() error = %v", err)
	}

	if _, err := engine.Files().UploadStatus(context.Background(), scope, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("UploadStatus(empty) error = %v", err)
	}
	if _, err := engine.Files().UploadStatus(context.Background(), domain.Scope{}, capability.UploadID); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("UploadStatus(invalid scope) error = %v", err)
	}
	if err := engine.Files().AbortUpload(context.Background(), scope, ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("AbortUpload(empty) error = %v", err)
	}
	if err := engine.Files().AbortUpload(context.Background(), domain.Scope{}, capability.UploadID); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("AbortUpload(invalid scope) error = %v", err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), domain.Scope{}, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("CompleteUpload(invalid scope) error = %v", err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: "bad media type"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CompleteUpload(invalid media) error = %v", err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size + 1, MediaType: request.MediaType}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("CompleteUpload(mismatched constraints) error = %v", err)
	}

	currentOperationVersion := operationObject.Version
	for name, mutate := range map[string]func(*storageformat.UploadRecord){
		"status-area":            func(value *storageformat.UploadRecord) { value.Area = "trash" },
		"status-path":            func(value *storageformat.UploadRecord) { value.RequestedPath = "INVALID" },
		"completion-destination": func(value *storageformat.UploadRecord) { value.ResolvedPath = "INVALID" },
		"completion-state":       func(value *storageformat.UploadRecord) { value.State = storageformat.UploadCompleted },
	} {
		mutated := record
		mutate(&mutated)
		currentOperationVersion = replaceInternalObject(t, backend, operationKey, currentOperationVersion, encodeInternalEnvelope(t, uploadRecordSchema, operationKey, operationEnvelope.Revision, mutated))
		switch name {
		case "status-area":
			if _, err := engine.Files().UploadStatus(context.Background(), scope, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("UploadStatus(wrong area) error = %v", err)
			}
			if err := engine.Files().AbortUpload(context.Background(), scope, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("AbortUpload(wrong area) error = %v", err)
			}
			if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("CompleteUpload(wrong area) error = %v", err)
			}
		case "status-path":
			if _, err := engine.Files().UploadStatus(context.Background(), scope, capability.UploadID); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("UploadStatus(invalid stored path) error = %v", err)
			}
		case "completion-destination":
			if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("CompleteUpload(invalid destination) error = %v", err)
			}
		case "completion-state":
			if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("CompleteUpload(changed completed destination) error = %v", err)
			}
		}
		currentOperationVersion = replaceInternalObject(t, backend, operationKey, currentOperationVersion, operationObject.Body)
	}

	values = cloneInternalObjects(fixture)
	delete(values, leaseKey.String())
	_, candidate = openInternalTransferFixture(t, values, clock, "status-missing-lease")
	if _, err := candidate.Files().UploadStatus(context.Background(), scope, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UploadStatus(missing lease) error = %v", err)
	}
	_, candidate = openInternalTransferFixture(t, fixture, clock, "status-missing-session")
	if _, err := candidate.Files().UploadStatus(context.Background(), scope, capability.UploadID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UploadStatus(missing session) error = %v", err)
	}
	expiredValues := cloneInternalObjects(fixture)
	expiredRecord := record
	expiredRecord.ExpiresAt = clock.Now().Add(-time.Second)
	expiredValues[operationKey.String()] = encodeInternalEnvelope(t, uploadRecordSchema, operationKey, operationEnvelope.Revision, expiredRecord)
	_, candidate = openInternalTransferFixture(t, expiredValues, clock, "expired-completion")
	if _, err := candidate.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("CompleteUpload(expired) error = %v", err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("incomplete CompleteUpload() error = %v", err)
	}
	if err := backend.SimulateUploadOffset(context.Background(), uploadID, request.Size); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType, ChecksumSHA256: "required"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("checksum CompleteUpload() error = %v", err)
	}
	invalidRecord := record
	invalidRecord.StagingKey = "INVALID"
	updatedVersion := replaceInternalObject(t, backend, operationKey, currentOperationVersion, encodeInternalEnvelope(t, uploadRecordSchema, operationKey, operationEnvelope.Revision, invalidRecord))
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid staging CompleteUpload() error = %v", err)
	}
	replaceInternalObject(t, backend, operationKey, updatedVersion, operationObject.Body)

	if _, err := engine.Files().CreateDirectory(context.Background(), scope, domain.CreateDirectoryRequest{Path: request.Path}); err != nil {
		t.Fatal(err)
	}
	conflictingUpload := request
	conflictingUpload.IdempotencyKey = "another-upload"
	if _, err := engine.Files().CreateUpload(context.Background(), scope, conflictingUpload); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CreateUpload(existing destination) error = %v", err)
	}
	if _, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{UploadID: capability.UploadID, Path: request.Path, Size: request.Size, MediaType: request.MediaType}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("appeared destination CompleteUpload() error = %v", err)
	}
	if _, err := engine.Files().CreateDownload(context.Background(), domain.Scope{}, domain.CreateDownloadRequest{Path: request.Path}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("CreateDownload(invalid scope) error = %v", err)
	}
	if _, err := engine.Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: request.Path, Disposition: "unknown"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CreateDownload(invalid disposition) error = %v", err)
	}
	if _, err := engine.Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: request.Path, Version: "version"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CreateDownload(directory) error = %v", err)
	}

	downloadBackend := objectmemory.New()
	downloadServer := httptest.NewServer(downloadBackend)
	t.Cleanup(downloadServer.Close)
	if err := downloadBackend.ConfigureDataPlane(downloadServer.URL, clock, domain.NewIDGenerator(strings.NewReader(strings.Repeat("w", 1<<20)))); err != nil {
		t.Fatal(err)
	}
	downloadEngine := openInternalTestEngine(t, downloadBackend, clock, strings.NewReader(strings.Repeat("x", 1<<20)))
	downloadEntry := storageformat.DirectoryEntry{Name: "missing.bin", NameDigest: storageformat.NameDigest("missing.bin"), Kind: domain.EntryFile, BlobID: "missing-blob", Size: 4, MediaType: "application/octet-stream", ModifiedAt: clock.Now()}
	downloadEntry.LogicalVersion, _ = directoryEntryVersion(downloadEntry)
	prepared, err := downloadEngine.Files().prepareDirectory(context.Background(), scope, storageformat.RootDirectoryID, []storageformat.DirectoryEntry{downloadEntry}, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, prerequisite := range prepared.prerequisites {
		if _, err := downloadBackend.Put(context.Background(), objectstore.MustKey(prerequisite.Key), prerequisite.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			t.Fatal(err)
		}
	}
	rootKey := storageformat.DirectoryRootKey(user.String(), "live", storageformat.RootDirectoryID)
	if _, err := downloadBackend.Put(context.Background(), rootKey, prepared.rootBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	downloadPath := domain.MustParseUserPath("/missing.bin")
	if _, err := downloadEngine.Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: downloadPath, Version: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("CreateDownload(stale version) error = %v", err)
	}
	if _, err := downloadEngine.Files().CreateDownload(context.Background(), scope, domain.CreateDownloadRequest{Path: downloadPath, Version: domain.Version(downloadEntry.LogicalVersion)}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CreateDownload(missing blob) error = %v", err)
	}
}

func openInternalTransferFixture(t *testing.T, objects map[string][]byte, clock *domain.FixedClock, seed string) (*objectmemory.Backend, *Engine) {
	t.Helper()
	backend := objectmemory.New()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	if err := backend.ConfigureDataPlane(server.URL, clock, domain.NewIDGenerator(strings.NewReader(strings.Repeat(seed, 1<<16)))); err != nil {
		t.Fatal(err)
	}
	if err := backend.Import(objects); err != nil {
		t.Fatal(err)
	}
	return backend, openInternalTestEngine(t, backend, clock, strings.NewReader(strings.Repeat(seed, 1<<16)))
}

func openInternalTestEngine(t *testing.T, backend objectstore.Backend, clock *domain.FixedClock, random *strings.Reader) *Engine {
	t.Helper()
	engine, err := Open(context.Background(), Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(random),
		Writer:   WriterConfiguration{WriterSetID: "writer", ConfigurationDigest: "digest", KeyringIdentifiers: []string{"key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x44}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func replaceInternalObject(t *testing.T, backend *objectmemory.Backend, key objectstore.Key, version objectstore.NativeVersion, body []byte) objectstore.NativeVersion {
	t.Helper()
	updated, err := backend.Put(context.Background(), key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func withStateCursor(cursor stateListCursor, mutate func(*stateListCursor)) stateListCursor {
	cursor.Snapshots = append([]string(nil), cursor.Snapshots...)
	mutate(&cursor)
	return cursor
}

func cloneInternalObjects(objects map[string][]byte) map[string][]byte {
	clone := make(map[string][]byte, len(objects))
	for key, body := range objects {
		clone[key] = append([]byte(nil), body...)
	}
	return clone
}

func encodeInternalEnvelope(t *testing.T, schema string, key objectstore.Key, revision uint64, value any) []byte {
	t.Helper()
	body, err := storageformat.EncodeEnvelope(schema, key, revision, value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func withEntry(entry storageformat.DirectoryEntry, mutate func(*storageformat.DirectoryEntry)) storageformat.DirectoryEntry {
	mutate(&entry)
	return entry
}

var _ objectstore.Backend = objectmemory.New()

type metadataOnlyBackend struct{ objectstore.Backend }
