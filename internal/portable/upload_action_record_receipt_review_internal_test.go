package portable

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func reviewActionLegacyKey(action string, items []domain.UploadActionItem) string {
	ordered := append([]domain.UploadActionItem(nil), items...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].UploadID < ordered[j].UploadID })
	var intent strings.Builder
	for _, item := range ordered {
		intent.WriteString(string(item.UploadID))
		intent.WriteByte(0)
		intent.WriteString(item.CRC32C)
		intent.WriteByte(0)
	}
	return "lifecycle-" + action + ":" + storageformat.Digest([]byte(intent.String()))
}

func reviewLatestActionOutcome(t *testing.T, f uploadBatchScaleFixture) storageformat.DomainOutcome {
	t.Helper()
	ctx := context.Background()
	store := f.engine.stateDomainStore()
	reference := uploadDomainReference(f.scope.UserID())
	head, err := store.loadHead(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := newConsistencyDomainTreeSession(store, reference).collect(ctx, head.head.Outcomes, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		var outcome storageformat.DomainOutcome
		if err := decodeCanonicalValue(entry.Value, &outcome); err != nil {
			t.Fatal(err)
		}
		if outcome.Revision == head.head.Revision {
			return outcome
		}
	}
	t.Fatal("published action has no retained outcome")
	return storageformat.DomainOutcome{}
}

// This public-API regression also runs against the pre-change source snapshot:
// successful actual-byte actions previously retained an unused generic result
// in the same transaction namespace a public legacy caller may select.
func TestUploadActionReceiptReviewNaturalRecordAuthority(t *testing.T) {
	for _, action := range []string{"complete", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x1789b101)
			ctx := context.Background()
			requests := scaleUploadRequestsCount("record-receipt", 1)
			requests[0].Size = 4
			page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
			if err != nil || len(page) != 1 || page[0].Capability == nil {
				t.Fatalf("admission: %+v %v", page, err)
			}
			item := domain.UploadActionItem{UploadID: page[0].UploadID, Action: action}
			if action == "complete" {
				payload := []byte("four")
				admissionWriteBytes(t, f, *page[0].Capability, payload, 0)
				item.CRC32C = objectstore.FingerprintFor(payload).CRC32C
			}
			result, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{item})
			if err != nil || len(result) != 1 || result[0].Outcome != "applied" {
				t.Fatalf("actual terminal action: %+v %v", result, err)
			}
			outcome := reviewLatestActionOutcome(t, f)
			decoded, decodeErr := base64.RawURLEncoding.DecodeString(outcome.Fingerprint)
			if len(outcome.Result) != 0 || decodeErr != nil || len(decoded) != 32 || outcome.Revision == 0 || !f.engine.clock.Now().Before(outcome.RetainUntil) {
				t.Fatalf("natural records must retain full mutation binding without duplicate result: resultBytes=%d fingerprintBytes=%d revision=%d err=%v", len(outcome.Result), len(decoded), outcome.Revision, decodeErr)
			}
			key := reviewActionLegacyKey(action, []domain.UploadActionItem{item})
			var legacyID string
			if action == "complete" {
				legacyID, _, err = normalizePortableUploadCompletionBatch(f.scope.UserID(), domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{{UploadID: item.UploadID, CRC32C: item.CRC32C}}, IdempotencyKey: key})
				if result[0].Entry == nil || result[0].Entry.Size != 4 || result[0].Entry.Path != requests[0].Path || result[0].Entry.Version == "" {
					t.Fatalf("completion lost exact portable entry: %+v", result)
				}
			} else {
				legacyID, _, err = normalizePortableUploadAbortBatch(f.scope.UserID(), domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{item.UploadID}, IdempotencyKey: key})
			}
			if err != nil || outcome.MutationID == legacyID || !storageformat.ValidDigest(outcome.MutationID) {
				t.Fatalf("private outcome aliases a public legacy transaction: err=%v", err)
			}
			f.fileLedger.Reset()
			replica := newAdmissionReplica(t, f, 0x1789b102)
			replay, err := replica.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{item})
			if err != nil || replay[0].Outcome != "already_applied" || !reflect.DeepEqual(result[0].Entry, replay[0].Entry) || len(f.fileLedger.Events()) != 0 {
				t.Fatalf("record replay repeated provider work or changed exact entry: %+v %v", replay, err)
			}
		})
	}
}

func TestUploadActionReceiptReviewPrivateCommitDenials(t *testing.T) {
	for _, test := range []struct {
		name, fingerprint string
		materialized      bool
		allowed           bool
	}{{"positive-control", storageformat.Digest([]byte("valid")), true, true}, {"delta", storageformat.Digest([]byte("valid")), false, false}, {"empty-fingerprint", "", true, false}, {"malformed-fingerprint", "invalid", true, false}} {
		t.Run(test.name, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x1789b111)
			ctx := context.Background()
			page, err := f.engine.Files().AdmitUploads(ctx, f.scope, scaleUploadRequestsCount("otherwise-valid-denial", 1))
			if err != nil || len(page) != 1 || page[0].Capability == nil {
				t.Fatalf("setup: %+v %v", page, err)
			}
			store := newNamespaceStore(f.engine)
			view, err := store.loadView(ctx, f.scope.UserID(), "")
			if err != nil {
				t.Fatal(err)
			}
			record, value, err := f.engine.Files().portableUploadAtView(ctx, view, f.scope.UserID(), string(page[0].UploadID))
			if err != nil {
				t.Fatal(err)
			}
			record.State, record.CleanupPending, record.Completion = storageformat.UploadAborted, false, nil
			body, err := storageformat.EncodeCanonical(record)
			if err != nil {
				t.Fatal(err)
			}
			id := "private-denial"
			if err := view.bindMutation(id, storageformat.Digest([]byte("valid"))); err != nil {
				t.Fatal(err)
			}
			root := view.roots[domain.AreaLive]
			root.Entry.LogicalVersion, err = directoryEntryVersion(root.Entry)
			if err != nil {
				t.Fatal(err)
			}
			changes := map[string]storageformat.NamespaceEntry{namespaceFrameKey(domain.AreaLive, namespaceRootPath()): root}
			additional := []consistencyDomainChange{{Key: uploadRecordKey(record.UploadID), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body}}
			f.stateLedger.Reset()
			f.fileLedger.Reset()
			_, err = store.commitWithAdditionalChangesResultMode(ctx, view, id, test.fingerprint, changes, additional, storageformat.NamespaceMutationResult{}, test.materialized, false)
			if test.allowed {
				if err != nil || len(f.stateLedger.Events()) == 0 {
					t.Fatalf("otherwise-valid positive control did not publish: %v events=%d", err, len(f.stateLedger.Events()))
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalid) || len(f.stateLedger.Events()) != 0 || len(f.fileLedger.Events()) != 0 {
				t.Fatalf("unsupported record-only publication performed work: %v state=%d files=%d", err, len(f.stateLedger.Events()), len(f.fileLedger.Events()))
			}
		})
	}
	f := newUploadBatchScaleFixture(t, 0x1789b112)
	f.stateLedger.Reset()
	err := f.engine.Files().abortUploadActionsProven(context.Background(), f.scope, domain.AbortUploadBatchRequest{BatchID: storageformat.Digest([]byte("legacy-cohort")), UploadIDs: []domain.UploadID{"opaque-upload"}, IdempotencyKey: "private-overlay-denial"}, nil)
	if !errors.Is(err, domain.ErrInvalid) || len(f.stateLedger.Events()) != 0 {
		t.Fatalf("private cancellation accepted a cohort overlay: %v", err)
	}
}

func TestUploadActionReceiptReviewLostHeadAndTerminalBindings(t *testing.T) {
	for _, action := range []string{"complete", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newUploadBatchScaleFixture(t, 0x1789b121)
			ctx := context.Background()
			requests := scaleUploadRequestsCount("lost-record-head", 2)
			for i := range requests {
				requests[i].Size = 4
			}
			page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
			if err != nil || len(page) != 2 || page[0].Capability == nil || page[1].Capability == nil {
				t.Fatalf("setup: %+v %v", page, err)
			}
			items := make([]domain.UploadActionItem, 2)
			for i := range items {
				items[i] = domain.UploadActionItem{UploadID: page[i].UploadID, Action: action}
				if action == "complete" {
					admissionWriteBytes(t, f, *page[i].Capability, []byte("four"), 0)
					items[i].CRC32C = objectstore.FingerprintFor([]byte("four")).CRC32C
				}
			}
			base := f.engine.backend
			headKey := storageformat.DomainHeadKey(uploadDomainReference(f.scope.UserID()).Kind, uploadDomainReference(f.scope.UserID()).ID)
			lost, headWrites := false, 0
			f.engine.backend = &admissionBackendProbe{Backend: base, put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
				version, err := base.Put(ctx, key, body, condition)
				if err == nil && key == headKey {
					headWrites++
					if !lost {
						lost = true
						return version, domain.ErrUnavailable
					}
				}
				return version, err
			}}
			first, err := f.engine.Files().UploadActions(ctx, f.scope, items)
			if err != nil || len(first) != 2 || !lost || headWrites != 1 {
				t.Fatalf("lost durable head not exercised atomically: %+v err=%v writes=%d", first, err, headWrites)
			}
			f.fileLedger.Reset()
			replica := newAdmissionReplica(t, f, 0x1789b122)
			f.engine.clock.(*domain.FixedClock).Advance(48 * time.Hour)
			reordered := []domain.UploadActionItem{items[1], items[0]}
			replay, err := replica.Files().UploadActions(ctx, f.scope, reordered)
			if err != nil || len(replay) != 2 || replay[0].Outcome != "already_applied" || replay[1].Outcome != "already_applied" || len(f.fileLedger.Events()) != 0 || headWrites != 1 {
				t.Fatalf("expired/reordered replay repeated native publication: %+v %v writes=%d", replay, err, headWrites)
			}
			if action == "complete" {
				stat, err := replica.Files().Stat(ctx, f.scope, requests[1].Path)
				if err != nil || replay[0].Entry == nil || !reflect.DeepEqual(*replay[0].Entry, stat) {
					t.Fatalf("record entry differs from authoritative publication: %+v %+v %v", replay[0].Entry, stat, err)
				}
				forged := items[0]
				forged.CRC32C = objectstore.FingerprintFor([]byte("evil")).CRC32C
				denied, err := replica.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{forged, items[1]})
				if err != nil || denied[0].ErrorKind != domain.ErrorConflict || denied[1].Outcome != "already_applied" || headWrites != 1 {
					t.Fatalf("forged CRC poisoned completed sibling: %+v %v", denied, err)
				}
			}
			trash, _ := domain.NewScope(f.scope.UserID(), domain.AreaTrash)
			denied, err := replica.Files().UploadActions(ctx, trash, []domain.UploadActionItem{items[0]})
			if err != nil || denied[0].ErrorKind != domain.ErrorNotFound || headWrites != 1 {
				t.Fatalf("wrong scope read terminal authority: %+v %v", denied, err)
			}
		})
	}
}

// Old independent actions used exactly these public legacy methods and markers.
// Both orderings exercise the public API with the same deterministic key text.
func TestUploadActionReceiptReviewLegacyMarkersAndPublicKeyIsolation(t *testing.T) {
	for _, action := range []string{"complete", "cancel"} {
		for _, legacyFirst := range []bool{true, false} {
			name := action + "/private-first"
			if legacyFirst {
				name = action + "/legacy-first"
			}
			t.Run(name, func(t *testing.T) {
				f := newUploadBatchScaleFixture(t, 0x1789b131)
				ctx := context.Background()
				requests := scaleUploadRequestsCount("old-action-marker", 1)
				requests[0].Size = 4
				page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
				if err != nil || page[0].Capability == nil {
					t.Fatalf("setup: %+v %v", page, err)
				}
				item := domain.UploadActionItem{UploadID: page[0].UploadID, Action: action}
				if action == "complete" {
					admissionWriteBytes(t, f, *page[0].Capability, []byte("four"), 0)
					item.CRC32C = objectstore.FingerprintFor([]byte("four")).CRC32C
				}
				key := reviewActionLegacyKey(action, []domain.UploadActionItem{item})
				completion := domain.CompleteUploadBatchRequest{Items: []domain.CompleteUploadBatchItem{{UploadID: item.UploadID, CRC32C: item.CRC32C}}, IdempotencyKey: key}
				cancellation := domain.AbortUploadBatchRequest{UploadIDs: []domain.UploadID{item.UploadID}, IdempotencyKey: key}
				legacy := func() ([]domain.Entry, error) {
					if action == "complete" {
						result, err := f.engine.Files().CompleteUploadBatch(ctx, f.scope, completion)
						return result.Entries, err
					}
					return nil, f.engine.Files().AbortUploadBatch(ctx, f.scope, cancellation)
				}
				var original []domain.Entry
				if legacyFirst {
					original, err = legacy()
					if err != nil {
						t.Fatal(err)
					}
					outcome := reviewLatestActionOutcome(t, f)
					var marker storageformat.NamespaceMutationResult
					if err := decodeCanonicalValue(outcome.Result, &marker); err != nil || marker.UploadBatch == nil || marker.UploadBatch.TransactionID != outcome.MutationID || marker.UploadBatch.ItemCount != 1 || marker.UploadBatch.State != map[string]string{"complete": "completed", "cancel": "aborted"}[action] {
						t.Fatalf("legacy exact recognized marker changed: %+v %v", marker, err)
					}
					f.fileLedger.Reset()
					replay, err := legacy()
					if err != nil || !reflect.DeepEqual(original, replay) || len(f.fileLedger.Events()) != 0 {
						t.Fatalf("legacy marker replay changed entry or repeated work: %+v %v", replay, err)
					}
				}
				f.fileLedger.Reset()
				private, err := f.engine.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{item})
				expected := "applied"
				if legacyFirst {
					expected = "already_applied"
				}
				if err != nil || private[0].Outcome != expected {
					t.Fatalf("old/new record replay: %+v %v", private, err)
				}
				if legacyFirst && action == "complete" && (private[0].Entry == nil || !reflect.DeepEqual(*private[0].Entry, original[0])) {
					t.Fatal("old marker record replay changed exact entry")
				}
				f.fileLedger.Reset()
				if !legacyFirst {
					if _, err := legacy(); !errors.Is(err, domain.ErrConflict) {
						t.Fatalf("public key consumed private nil receipt instead of normal terminal conflict: %v", err)
					}
				} else if action == "complete" {
					completion.Items[0].CRC32C = objectstore.FingerprintFor([]byte("evil")).CRC32C
					if _, err := legacy(); !errors.Is(err, domain.ErrConflict) {
						t.Fatalf("legacy recognized marker lost changed intent binding: %v", err)
					}
				} else {
					cancellation.UploadIDs = []domain.UploadID{"changed-upload"}
					if _, err := legacy(); !errors.Is(err, domain.ErrConflict) {
						t.Fatalf("legacy cancellation marker lost changed intent binding: %v", err)
					}
				}
				if len(f.fileLedger.Events()) != 0 {
					t.Fatal("terminal receipt replays performed native work")
				}
			})
		}
	}
}

func TestUploadActionReceiptReviewCheckpointRawCopyPreservesExactTerminalRecords(t *testing.T) {
	f := newUploadBatchScaleFixture(t, 0x1789b141)
	ctx := context.Background()
	requests := scaleUploadRequestsCount("copy-terminal-receipts", 2)
	for i := range requests {
		requests[i].Size = 4
	}
	page, err := f.engine.Files().AdmitUploads(ctx, f.scope, requests)
	if err != nil || len(page) != 2 || page[0].Capability == nil || page[1].Capability == nil {
		t.Fatalf("setup: %+v %v", page, err)
	}
	admissionWriteBytes(t, f, *page[0].Capability, []byte("four"), 0)
	items := []domain.UploadActionItem{{UploadID: page[0].UploadID, Action: "complete", CRC32C: objectstore.FingerprintFor([]byte("four")).CRC32C}, {UploadID: page[1].UploadID, Action: "cancel"}}
	original, err := f.engine.Files().UploadActions(ctx, f.scope, items)
	if err != nil || original[0].Outcome != "applied" || original[1].Outcome != "applied" || original[0].Entry == nil {
		t.Fatalf("terminal setup: %+v %v", original, err)
	}
	checkpoint, err := f.engine.CreateCheckpoint(ctx, "record-only-actions")
	if err != nil {
		t.Fatalf("nil generic results are valid materialized checkpoint authority: %v", err)
	}
	if err := f.engine.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	stateCopy, fileCopy := objectmemory.New(), objectmemory.New()
	// Shift native versions before copying. No portable receipt may depend on them.
	if _, err := stateCopy.Put(ctx, objectstore.MustKey("synthetic/native-version-shift"), []byte("shift"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	copyKey := func(key objectstore.Key, fileData bool) error {
		source, target := f.engine.backend, objectstore.Backend(stateCopy)
		if fileData {
			source, target = f.engine.fileBackend.(objectstore.Backend), fileCopy
		}
		object, err := source.Get(ctx, key)
		if err != nil {
			return err
		}
		_, err = target.Put(ctx, key, object.Body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		return err
	}
	if err := f.engine.visitCheckpointInventory(ctx, checkpoint, func(entry storageformat.CheckpointInventoryEntry) error {
		return copyKey(objectstore.MustKey(entry.Object.Key), entry.FileData)
	}); err != nil {
		t.Fatal(err)
	}
	if err := copyKey(storageformat.CheckpointKey(checkpoint.CheckpointID), false); err != nil {
		t.Fatal(err)
	}
	for index := uint64(0); index < checkpoint.InventoryPageCount; index++ {
		if err := copyKey(storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, index), false); err != nil {
			t.Fatal(err)
		}
	}
	headKey := storageformat.DomainHeadKey(uploadDomainReference(f.scope.UserID()).Kind, uploadDomainReference(f.scope.UserID()).ID)
	sourceHead, err := f.engine.backend.Head(ctx, headKey)
	if err != nil {
		t.Fatal(err)
	}
	targetHead, err := stateCopy.Head(ctx, headKey)
	if err != nil || sourceHead.Version == targetHead.Version {
		t.Fatalf("raw copy did not shift native head versions: %v", err)
	}
	copied, err := Open(ctx, Options{Backend: stateCopy, FileBackend: fileCopy, Clock: f.engine.clock, IDs: domain.NewIDGenerator(&deterministicScaleReader{state: 0x1789b142}), Writer: WriterConfiguration{WriterSetID: "upload-scale", ConfigurationDigest: "upload-scale-v1", KeyringIdentifiers: []string{"key"}}, LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x64}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if err := copied.VerifyCheckpoint(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := copied.OpenWrites(ctx, checkpoint.CheckpointID); err != nil {
		t.Fatal(err)
	}
	replay, err := copied.Files().UploadActions(ctx, f.scope, []domain.UploadActionItem{items[1], items[0]})
	if err != nil || replay[0].Outcome != "already_applied" || replay[1].Outcome != "already_applied" || !reflect.DeepEqual(original[0].Entry, replay[1].Entry) {
		t.Fatalf("raw copy lost exact terminal entry or cancellation: %+v %v", replay, err)
	}
	stat, err := copied.Files().Stat(ctx, f.scope, requests[0].Path)
	if err != nil || !reflect.DeepEqual(stat, *original[0].Entry) {
		t.Fatalf("raw copied visible file differs from record authority: %+v %v", stat, err)
	}
	view, err := newNamespaceStore(copied).loadView(ctx, f.scope.UserID(), "")
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := copied.Files().portableUploadAtView(ctx, view, f.scope.UserID(), string(items[0].UploadID))
	if err != nil {
		t.Fatal(err)
	}
	info, err := fileCopy.Verify(ctx, storageformat.BlobKey(f.scope.UserID().String(), record.BlobID), objectstore.ExpectedIntegrity{Size: 4, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: items[0].CRC32C}})
	if err != nil || info.Size != 4 {
		t.Fatalf("raw copy lost actual transferred bytes: %+v %v", info, err)
	}
}
