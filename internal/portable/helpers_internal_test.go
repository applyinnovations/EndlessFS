package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
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

func withEntry(entry storageformat.DirectoryEntry, mutate func(*storageformat.DirectoryEntry)) storageformat.DirectoryEntry {
	mutate(&entry)
	return entry
}

var _ objectstore.Backend = objectmemory.New()

type metadataOnlyBackend struct{ objectstore.Backend }
