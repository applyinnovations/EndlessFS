package storageformat

import (
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
)

func schema008TestTime() time.Time {
	return time.Date(2061, 2, 3, 4, 5, 6, 0, time.UTC)
}

func schema008TestFile() NamespaceEntry {
	return NamespaceEntry{
		SchemaVersion: 1,
		NodeID:        "file-node",
		Entry: DirectoryEntry{
			Name: "photo.jpg", Kind: domain.EntryFile, BlobID: "blob-1", Size: 42,
			MediaType: "image/jpeg", ModifiedAt: schema008TestTime(), LogicalVersion: "file-version",
		},
	}
}

func schema008TestDirectory() NamespaceEntry {
	digest := Digest([]byte("children"))
	return NamespaceEntry{
		SchemaVersion: 1,
		NodeID:        "directory-node",
		Entry: DirectoryEntry{
			Name: "photos", Kind: domain.EntryDirectory, DirectoryID: "directory-node",
			Size: 42, FileCount: 1, ContentDigest: digest, ModifiedAt: schema008TestTime(),
			LogicalVersion: "directory-version",
		},
		Children: DomainTreeRoot{Digest: digest, EntryCount: 1}, EntryCount: 1,
		ContentAccumulator: "accumulator",
	}
}

func requireSchema008Invalid(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("error = %v, want invalid", err)
	}
}

func TestSchema008NamespaceRecordsAcceptValidFormsAndDenyEveryAuthorityMismatch(t *testing.T) {
	file := schema008TestFile()
	directory := schema008TestDirectory()
	trashed := directory
	trashed.Trash = &NamespaceTrashMetadata{OriginalPath: "/photos", OriginalVersion: "old-version", TrashedAt: schema008TestTime()}
	for name, entry := range map[string]NamespaceEntry{"file": file, "directory": directory, "trash": trashed} {
		t.Run("valid-"+name, func(t *testing.T) {
			if err := ValidateNamespaceEntry(entry); err != nil {
				t.Fatal(err)
			}
		})
	}

	invalid := map[string]NamespaceEntry{
		"schema":            func() NamespaceEntry { value := file; value.SchemaVersion = 0; return value }(),
		"node":              func() NamespaceEntry { value := file; value.NodeID = ""; return value }(),
		"occurrence-digest": func() NamespaceEntry { value := directory; value.OccurrenceContextID = "invalid"; return value }(),
		"logical-version":   func() NamespaceEntry { value := file; value.Entry.LogicalVersion = ""; return value }(),
		"negative-size":     func() NamespaceEntry { value := file; value.Entry.Size = -1; return value }(),
		"negative-count":    func() NamespaceEntry { value := directory; value.Entry.FileCount = -1; return value }(),
		"invalid-tree":      func() NamespaceEntry { value := file; value.Children.EntryCount = 1; return value }(),
		"trash-root":        func() NamespaceEntry { value := trashed; value.Trash.OriginalPath = "/"; return value }(),
		"trash-version":     func() NamespaceEntry { value := trashed; value.Trash.OriginalVersion = ""; return value }(),
		"trash-time":        func() NamespaceEntry { value := trashed; value.Trash.TrashedAt = time.Time{}; return value }(),
		"file-occurrence-context": func() NamespaceEntry {
			value := file
			value.OccurrenceContextID = Digest([]byte("context"))
			return value
		}(),
		"file-name":                func() NamespaceEntry { value := file; value.Entry.Name = ""; return value }(),
		"file-blob":                func() NamespaceEntry { value := file; value.Entry.BlobID = ""; return value }(),
		"file-directory":           func() NamespaceEntry { value := file; value.Entry.DirectoryID = "directory"; return value }(),
		"file-recursive-count":     func() NamespaceEntry { value := file; value.Entry.FileCount = 1; return value }(),
		"file-entry-count":         func() NamespaceEntry { value := file; value.EntryCount = 1; return value }(),
		"file-accumulator":         func() NamespaceEntry { value := file; value.ContentAccumulator = "unexpected"; return value }(),
		"file-children":            func() NamespaceEntry { value := file; value.Children = directory.Children; return value }(),
		"directory-blob":           func() NamespaceEntry { value := directory; value.Entry.BlobID = "blob"; return value }(),
		"directory-identity":       func() NamespaceEntry { value := directory; value.Entry.DirectoryID = "other"; return value }(),
		"directory-count-binding":  func() NamespaceEntry { value := directory; value.EntryCount = 2; return value }(),
		"directory-accumulator":    func() NamespaceEntry { value := directory; value.ContentAccumulator = ""; return value }(),
		"directory-content-digest": func() NamespaceEntry { value := directory; value.Entry.ContentDigest = ""; return value }(),
		"kind":                     func() NamespaceEntry { value := file; value.Entry.Kind = "unknown"; return value }(),
	}
	for name, entry := range invalid {
		t.Run("invalid-"+name, func(t *testing.T) { requireSchema008Invalid(t, ValidateNamespaceEntry(entry)) })
	}
}

func TestSchema008DomainPageRejectsMissingAuthorityBinding(t *testing.T) {
	requireSchema008Invalid(t, ValidateDomainPage(DomainPage{}, Digest([]byte("expected-page"))))
}

func TestSchema008MutationResultAndBatchItemValidationMatrix(t *testing.T) {
	fingerprint := Digest([]byte("request"))
	results := map[string]NamespaceMutationResult{
		"operation": {SchemaVersion: 1, RequestFingerprint: fingerprint, Operation: &domain.Operation{ID: "operation", State: domain.OperationSucceeded}},
		"entry":     {SchemaVersion: 1, RequestFingerprint: fingerprint, Entry: &DirectoryEntry{Name: "entry"}},
		"batch": {SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &NamespaceBatch{
			Operation: domain.Operation{ID: "batch", State: domain.OperationSucceeded}, Items: DomainTreeRoot{Digest: Digest([]byte("items")), EntryCount: 2}, ItemCount: 2,
		}},
		"upload-created": {SchemaVersion: 1, RequestFingerprint: fingerprint, Upload: &NamespaceUploadMutationResult{UploadID: "upload", State: "created"}},
		"upload-aborted": {SchemaVersion: 1, RequestFingerprint: fingerprint, Upload: &NamespaceUploadMutationResult{UploadID: "upload", State: "aborted"}},
	}
	for name, result := range results {
		t.Run("valid-"+name, func(t *testing.T) {
			if err := ValidateNamespaceMutationResult(result); err != nil {
				t.Fatal(err)
			}
		})
	}

	invalidResults := map[string]NamespaceMutationResult{
		"schema":          {RequestFingerprint: fingerprint, Entry: &DirectoryEntry{}},
		"fingerprint":     {SchemaVersion: 1, RequestFingerprint: "invalid", Entry: &DirectoryEntry{}},
		"no-kind":         {SchemaVersion: 1, RequestFingerprint: fingerprint},
		"multiple-kinds":  {SchemaVersion: 1, RequestFingerprint: fingerprint, Entry: &DirectoryEntry{}, Operation: &domain.Operation{}},
		"batch-operation": {SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &NamespaceBatch{ItemCount: 1, Items: DomainTreeRoot{Digest: Digest([]byte("items")), EntryCount: 1}}},
		"batch-state":     {SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &NamespaceBatch{Operation: domain.Operation{ID: "batch", State: domain.OperationRunning}, ItemCount: 1, Items: DomainTreeRoot{Digest: Digest([]byte("items")), EntryCount: 1}}},
		"batch-empty":     {SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &NamespaceBatch{Operation: domain.Operation{ID: "batch", State: domain.OperationSucceeded}}},
		"batch-count":     {SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &NamespaceBatch{Operation: domain.Operation{ID: "batch", State: domain.OperationSucceeded}, ItemCount: 2, Items: DomainTreeRoot{Digest: Digest([]byte("items")), EntryCount: 1}}},
		"batch-too-large": {SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &NamespaceBatch{Operation: domain.Operation{ID: "batch", State: domain.OperationSucceeded}, ItemCount: MaxNamespaceBatchItems + 1, Items: DomainTreeRoot{Digest: Digest([]byte("items")), EntryCount: MaxNamespaceBatchItems + 1}}},
		"batch-tree":      {SchemaVersion: 1, RequestFingerprint: fingerprint, Batch: &NamespaceBatch{Operation: domain.Operation{ID: "batch", State: domain.OperationSucceeded}, ItemCount: 1, Items: DomainTreeRoot{EntryCount: 1}}},
		"upload-id":       {SchemaVersion: 1, RequestFingerprint: fingerprint, Upload: &NamespaceUploadMutationResult{State: "created"}},
		"upload-state":    {SchemaVersion: 1, RequestFingerprint: fingerprint, Upload: &NamespaceUploadMutationResult{UploadID: "upload", State: "complete"}},
	}
	for name, result := range invalidResults {
		t.Run("invalid-"+name, func(t *testing.T) { requireSchema008Invalid(t, ValidateNamespaceMutationResult(result)) })
	}

	validItem := NamespaceBatchItem{Index: 1, Source: "/from", Destination: "/to", OperationID: "item", State: domain.OperationSucceeded}
	if err := ValidateNamespaceBatchItem(validItem); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*NamespaceBatchItem){
		"source-empty":        func(item *NamespaceBatchItem) { item.Source = "" },
		"operation-empty":     func(item *NamespaceBatchItem) { item.OperationID = "" },
		"state":               func(item *NamespaceBatchItem) { item.State = domain.OperationFailed },
		"source-invalid":      func(item *NamespaceBatchItem) { item.Source = "relative" },
		"destination-invalid": func(item *NamespaceBatchItem) { item.Destination = "relative" },
	} {
		t.Run("invalid-item-"+name, func(t *testing.T) {
			item := validItem
			mutate(&item)
			requireSchema008Invalid(t, ValidateNamespaceBatchItem(item))
		})
	}
}

func TestSchema008UploadAndDuplicateProjectionValidationMatrix(t *testing.T) {
	now := schema008TestTime()
	upload := PortableUploadRecord{
		SchemaVersion: 1, UploadID: "upload", OwnerID: "owner", Area: "live", RequestedPath: "/requested", ResolvedPath: "/resolved",
		BlobID: "blob", Size: 42, MediaType: "image/jpeg", Conflict: domain.ConflictFail, State: UploadActive, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := ValidatePortableUploadRecord(upload); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PortableUploadRecord){
		"schema": func(value *PortableUploadRecord) { value.SchemaVersion = 0 }, "id": func(value *PortableUploadRecord) { value.UploadID = "" },
		"owner": func(value *PortableUploadRecord) { value.OwnerID = "" }, "area": func(value *PortableUploadRecord) { value.Area = "archive" },
		"requested": func(value *PortableUploadRecord) { value.RequestedPath = "" }, "resolved": func(value *PortableUploadRecord) { value.ResolvedPath = "" },
		"blob": func(value *PortableUploadRecord) { value.BlobID = "" }, "size": func(value *PortableUploadRecord) { value.Size = -1 },
		"media": func(value *PortableUploadRecord) { value.MediaType = "" }, "conflict": func(value *PortableUploadRecord) { value.Conflict = "invalid" },
		"state": func(value *PortableUploadRecord) { value.State = "invalid" }, "created": func(value *PortableUploadRecord) { value.CreatedAt = time.Time{} },
		"expiry": func(value *PortableUploadRecord) { value.ExpiresAt = value.CreatedAt },
	} {
		t.Run("invalid-upload-"+name, func(t *testing.T) {
			value := upload
			mutate(&value)
			requireSchema008Invalid(t, ValidatePortableUploadRecord(value))
		})
	}
	for _, state := range []UploadState{UploadInitializing, UploadCompleted, UploadAborted} {
		value := upload
		value.State = state
		if err := ValidatePortableUploadRecord(value); err != nil {
			t.Fatalf("terminal upload %q = %v", state, err)
		}
	}
	cleanup := upload
	cleanup.State, cleanup.CleanupPending = UploadCompleted, true
	if err := ValidatePortableUploadRecord(cleanup); err != nil {
		t.Fatalf("cleanup-pending upload: %v", err)
	}
	cleanup.State = UploadActive
	requireSchema008Invalid(t, ValidatePortableUploadRecord(cleanup))

	batched := upload
	batched.Batch = &PortableUploadBatchMember{BatchID: Digest([]byte("batch")), Index: 0, Count: 1}
	if err := ValidatePortableUploadRecord(batched); err != nil {
		t.Fatalf("valid batch binding: %v", err)
	}
	for name, mutate := range map[string]func(*PortableUploadBatchMember){
		"batch": func(value *PortableUploadBatchMember) { value.BatchID = "invalid" },
		"zero":  func(value *PortableUploadBatchMember) { value.Count = 0 },
		"large": func(value *PortableUploadBatchMember) { value.Count = MaxPortableUploadBatchItems + 1 },
		"index": func(value *PortableUploadBatchMember) { value.Index = value.Count },
	} {
		t.Run("invalid-batch-"+name, func(t *testing.T) {
			value := batched
			member := *batched.Batch
			mutate(&member)
			value.Batch = &member
			requireSchema008Invalid(t, ValidatePortableUploadRecord(value))
		})
	}

	completed := upload
	completed.State = UploadCompleted
	completed.Completion = &PortableUploadCompletion{
		MD5: integrity.MD5([]byte("body")), CRC32C: integrity.CRC32C([]byte("body")), ModifiedAt: now,
	}
	if err := ValidatePortableUploadRecord(completed); err != nil {
		t.Fatalf("valid completion: %v", err)
	}
	for name, mutate := range map[string]func(*PortableUploadRecord){
		"state": func(value *PortableUploadRecord) { value.State = UploadActive },
		"time":  func(value *PortableUploadRecord) { value.Completion.ModifiedAt = time.Time{} },
		"md5":   func(value *PortableUploadRecord) { value.Completion.MD5 = "invalid" },
		"crc":   func(value *PortableUploadRecord) { value.Completion.CRC32C = "invalid" },
	} {
		t.Run("invalid-completion-"+name, func(t *testing.T) {
			value := completed
			completion := *completed.Completion
			value.Completion = &completion
			mutate(&value)
			requireSchema008Invalid(t, ValidatePortableUploadRecord(value))
		})
	}

	idempotency := PortableUploadIdempotency{SchemaVersion: 1, OwnerID: "owner", KeyDigest: Digest([]byte("key")), Fingerprint: Digest([]byte("fingerprint")), UploadID: "upload"}
	if err := ValidatePortableUploadIdempotency(idempotency); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PortableUploadIdempotency){
		"schema": func(value *PortableUploadIdempotency) { value.SchemaVersion = 0 }, "owner": func(value *PortableUploadIdempotency) { value.OwnerID = "" },
		"key": func(value *PortableUploadIdempotency) { value.KeyDigest = "invalid" }, "fingerprint": func(value *PortableUploadIdempotency) { value.Fingerprint = "invalid" },
		"upload": func(value *PortableUploadIdempotency) { value.UploadID = "" },
	} {
		t.Run("invalid-idempotency-"+name, func(t *testing.T) {
			value := idempotency
			mutate(&value)
			requireSchema008Invalid(t, ValidatePortableUploadIdempotency(value))
		})
	}

	group := Digest([]byte("group"))
	summary := DuplicateProjectionSummary{SchemaVersion: 1, GroupID: group, Kind: domain.DuplicateFile, OccurrenceCount: 2, Size: 42, FileCount: 1}
	if err := ValidateDuplicateProjectionSummary(summary); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DuplicateProjectionSummary){
		"schema": func(value *DuplicateProjectionSummary) { value.SchemaVersion = 0 }, "group": func(value *DuplicateProjectionSummary) { value.GroupID = "invalid" },
		"kind": func(value *DuplicateProjectionSummary) { value.Kind = "invalid" }, "occurrences": func(value *DuplicateProjectionSummary) { value.OccurrenceCount = 0 },
		"size": func(value *DuplicateProjectionSummary) { value.Size = -1 }, "count": func(value *DuplicateProjectionSummary) { value.FileCount = 2 },
		"file-container": func(value *DuplicateProjectionSummary) { value.ContainedBy = group },
	} {
		t.Run("invalid-summary-"+name, func(t *testing.T) {
			value := summary
			mutate(&value)
			requireSchema008Invalid(t, ValidateDuplicateProjectionSummary(value))
		})
	}
	directorySummary := summary
	directorySummary.Kind, directorySummary.FileCount, directorySummary.ContainedBy = domain.DuplicateDirectory, 3, group
	if err := ValidateDuplicateProjectionSummary(directorySummary); err != nil {
		t.Fatal(err)
	}
	directorySummary.ContainedBy = "invalid"
	requireSchema008Invalid(t, ValidateDuplicateProjectionSummary(directorySummary))

	preference := DuplicateDirectoryPreference{SchemaVersion: 1, PairID: group, LeftIdentity: "a", RightIdentity: "b", Ignored: true, Revision: 1}
	if err := ValidateDuplicateDirectoryPreference(preference); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DuplicateDirectoryPreference){
		"schema": func(value *DuplicateDirectoryPreference) { value.SchemaVersion = 0 }, "pair": func(value *DuplicateDirectoryPreference) { value.PairID = "invalid" },
		"left": func(value *DuplicateDirectoryPreference) { value.LeftIdentity = "" }, "right": func(value *DuplicateDirectoryPreference) { value.RightIdentity = "" },
		"order": func(value *DuplicateDirectoryPreference) { value.LeftIdentity = value.RightIdentity }, "revision": func(value *DuplicateDirectoryPreference) { value.Revision = 0 },
	} {
		t.Run("invalid-preference-"+name, func(t *testing.T) {
			value := preference
			mutate(&value)
			requireSchema008Invalid(t, ValidateDuplicateDirectoryPreference(value))
		})
	}

	occurrence := DuplicateProjectionOccurrence{SchemaVersion: 1, BlobID: "blob", Occurrence: DuplicateOccurrence{GroupID: group, Kind: domain.DuplicateFile, Area: "live", Path: "/photo.jpg", Version: "version", Size: 42, FileCount: 1}}
	if err := ValidateDuplicateProjectionOccurrence(occurrence); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DuplicateProjectionOccurrence){
		"schema": func(value *DuplicateProjectionOccurrence) { value.SchemaVersion = 0 }, "kind": func(value *DuplicateProjectionOccurrence) { value.Occurrence.Kind = "invalid" },
		"group": func(value *DuplicateProjectionOccurrence) { value.Occurrence.GroupID = "invalid" }, "area": func(value *DuplicateProjectionOccurrence) { value.Occurrence.Area = "" },
		"path": func(value *DuplicateProjectionOccurrence) { value.Occurrence.Path = "" }, "version": func(value *DuplicateProjectionOccurrence) { value.Occurrence.Version = "" },
		"size": func(value *DuplicateProjectionOccurrence) { value.Occurrence.Size = -1 }, "count": func(value *DuplicateProjectionOccurrence) { value.Occurrence.FileCount = 2 },
		"blob": func(value *DuplicateProjectionOccurrence) { value.BlobID = "" },
	} {
		t.Run("invalid-occurrence-"+name, func(t *testing.T) {
			value := occurrence
			mutate(&value)
			requireSchema008Invalid(t, ValidateDuplicateProjectionOccurrence(value))
		})
	}
	directoryOccurrence := occurrence
	directoryOccurrence.BlobID = ""
	directoryOccurrence.Occurrence.Kind = domain.DuplicateDirectory
	if err := ValidateDuplicateProjectionOccurrence(directoryOccurrence); err != nil {
		t.Fatal(err)
	}
	directoryOccurrence.BlobID = "unexpected"
	requireSchema008Invalid(t, ValidateDuplicateProjectionOccurrence(directoryOccurrence))
}
