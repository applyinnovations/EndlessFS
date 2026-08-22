package portable

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	checkpointSchema                = "checkpoint-v1"
	checkpointSchemaV2              = "checkpoint-v2"
	checkpointSchemaV3              = "checkpoint-v3"
	checkpointWorkSchema            = "checkpoint-work-v1"
	checkpointInventoryPageSchema   = "checkpoint-inventory-page-v1"
	checkpointInventoryPageSchemaV2 = "checkpoint-inventory-page-v2"
	checkpointInventoryPageEntries  = 512
)

// VerifyCheckpointReadOnly validates an existing canonical bucket and closed
// checkpoint without initializing, repairing, or otherwise writing storage.
func VerifyCheckpointReadOnly(ctx context.Context, backend objectstore.Backend, writerConfiguration WriterConfiguration, checkpointID string) error {
	return VerifyCheckpointReadOnlyWithFileBackend(ctx, backend, nil, writerConfiguration, checkpointID)
}

// VerifyCheckpointReadOnlyWithFileBackend validates a checkpoint whose
// canonical metadata and file bytes may be stored on distinct backends. A nil
// file backend selects the state backend and preserves the one-bucket layout.
func VerifyCheckpointReadOnlyWithFileBackend(ctx context.Context, backend objectstore.Backend, fileBackend objectstore.FileControlBackend, writerConfiguration WriterConfiguration, checkpointID string) error {
	if backend == nil || checkpointID == "" {
		return domain.NewError(domain.ErrorInvalid, "backend and checkpoint ID are required")
	}
	separateFileBackend := fileBackend != nil
	if fileBackend == nil {
		fileBackend = backend
	}
	writer, err := canonicalWriterConfiguration(writerConfiguration)
	if err != nil {
		return err
	}
	engine := &Engine{backend: backend, fileBackend: fileBackend, separateFileBackend: separateFileBackend, writer: writer}
	superblockObject, err := backend.Get(ctx, storageformat.SuperblockKey())
	if err != nil {
		return err
	}
	var superblock storageformat.Superblock
	if err := state.DecodeJSONWithLimit(superblockObject.Body, &superblock, storageformat.MaxCanonicalBytes); err != nil {
		return err
	}
	if superblock.SchemaVersion != 1 || superblock.FormatID != storageformat.FormatID || superblock.BucketID == "" || superblock.CanonicalEncoder != storageformat.CanonicalEncoder || superblock.KeyFormatVersion != storageformat.KeyFormatVersion || superblock.WriterProtocolVersion != storageformat.WriterProtocolVersion || superblock.CreatedAt.IsZero() || !reflect.DeepEqual(superblock.RequiredFeatures, writer.RequiredFeatures) {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable superblock")
	}
	writerObject, err := backend.Get(ctx, storageformat.WriterSetKey())
	if err != nil {
		return err
	}
	var writerEnvelope storageformat.Envelope
	var storedWriter storageformat.WriterSet
	if err := storageformat.DecodeEnvelope(writerObject.Body, storageformat.WriterSetKey(), writerSetSchema, &writerEnvelope, &storedWriter); err != nil {
		return err
	}
	if !reflect.DeepEqual(storedWriter, writer) {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set")
	}
	return engine.VerifyCheckpoint(ctx, checkpointID)
}

func (e *Engine) CreateCheckpoint(ctx context.Context, checkpointID string) (storageformat.Checkpoint, error) {
	if err := e.CloseWrites(ctx, checkpointID); err != nil {
		return storageformat.Checkpoint{}, err
	}
	return e.createCheckpointWhileClosed(ctx, checkpointID)
}

func (e *Engine) createCheckpointWhileClosed(ctx context.Context, checkpointID string) (storageformat.Checkpoint, error) {
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return storageformat.Checkpoint{}, err
	}
	if gate.Mode != storageformat.GateClosed || gate.CheckpointID != checkpointID {
		return storageformat.Checkpoint{}, domain.NewError(domain.ErrorPreconditionFailed, "write gate is not checkpoint-closed")
	}
	superblockObject, err := e.backend.Get(ctx, storageformat.SuperblockKey())
	if err != nil {
		return storageformat.Checkpoint{}, err
	}
	var superblock storageformat.Superblock
	if err := state.DecodeJSONWithLimit(superblockObject.Body, &superblock, storageformat.MaxCanonicalBytes); err != nil {
		return storageformat.Checkpoint{}, err
	}
	if existing, readErr := e.readCheckpoint(ctx, checkpointID); readErr == nil {
		if existing.SchemaVersion < 3 {
			if err := e.retireLegacyCheckpoint(ctx, checkpointID); err != nil {
				return storageformat.Checkpoint{}, err
			}
		}
	} else if !errors.Is(readErr, domain.ErrNotFound) {
		return storageformat.Checkpoint{}, readErr
	}
	summary, err := e.prepareCheckpointInventoryMetadata(ctx, checkpointID, gate.Epoch)
	if err != nil {
		return storageformat.Checkpoint{}, err
	}
	checkpoint := storageformat.Checkpoint{
		SchemaVersion: 3, CheckpointID: checkpointID, BucketID: superblock.BucketID,
		WriterSetID: e.writer.WriterSetID, GateEpoch: gate.Epoch,
		KeyFormatVersion: storageformat.KeyFormatVersion, WriterProtocolVersion: storageformat.WriterProtocolVersion,
		CreatedAt: e.clock.Now().UTC(), InventoryPageCount: summary.pageCount,
		StateObjectCount: summary.stateObjects, FileObjectCount: summary.fileObjects,
		InventoryDigest: summary.inventoryDigest,
	}
	key := storageformat.CheckpointKey(checkpointID)
	body, err := storageformat.EncodeEnvelope(checkpointSchemaV3, key, 1, checkpoint)
	if err != nil {
		return storageformat.Checkpoint{}, err
	}
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return storageformat.Checkpoint{}, err
		}
		existing, readErr := e.readCheckpoint(ctx, checkpointID)
		if readErr != nil {
			return storageformat.Checkpoint{}, readErr
		}
		if err := e.verifyCheckpointV2Summary(existing, gate, summary); err != nil {
			return storageformat.Checkpoint{}, err
		}
		return existing, nil
	}
	return checkpoint, nil
}

// retireLegacyCheckpoint removes only non-authoritative checkpoint artifacts
// while the canonical gate remains closed. Artifacts are deleted before the
// root, so a crash cannot expose a missing root as a completed retirement and
// a concurrent v3 builder cannot begin until the legacy root is gone.
func (e *Engine) retireLegacyCheckpoint(ctx context.Context, checkpointID string) error {
	for _, prefix := range []string{
		storageformat.CheckpointWorkPrefix(checkpointID),
		storageformat.CheckpointInventoryPagePrefix(checkpointID),
	} {
		if err := walkObjectInfos(ctx, e.backend, prefix, func(info objectstore.ObjectInfo) error {
			err := e.backend.Delete(ctx, info.Key, objectstore.DeleteCondition{Version: info.Version})
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrPreconditionFailed) {
				return nil
			}
			return err
		}); err != nil {
			return err
		}
	}
	key := storageformat.CheckpointKey(checkpointID)
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var envelope storageformat.Envelope
	if err := state.DecodeJSONWithLimit(object.Body, &envelope, storageformat.MaxCanonicalBytes); err != nil {
		return err
	}
	if envelope.Schema == checkpointSchemaV3 {
		return nil
	}
	if envelope.Schema != checkpointSchema && envelope.Schema != checkpointSchemaV2 {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible checkpoint")
	}
	err = e.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: object.Version})
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrPreconditionFailed) {
		return nil
	}
	return err
}

func (e *Engine) VerifyCheckpoint(ctx context.Context, checkpointID string) error {
	checkpoint, err := e.readCheckpoint(ctx, checkpointID)
	if err != nil {
		return err
	}
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if checkpoint.SchemaVersion == 3 {
		return e.verifyCheckpointV3(ctx, checkpoint, gate)
	}
	return domain.NewError(domain.ErrorPreconditionFailed, "legacy SHA-only checkpoint requires replacement with metadata checkpoint v3 while writes remain closed")
}

func (e *Engine) verifyCheckpointInventory(checkpoint storageformat.Checkpoint, gate storageformat.WriteGate, objects []storageformat.CheckpointObject) error {
	if gate.Mode != storageformat.GateClosed || gate.Epoch != checkpoint.GateEpoch || gate.CheckpointID != checkpoint.CheckpointID || checkpoint.WriterSetID != e.writer.WriterSetID {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint does not match closed write gate")
	}
	encoded, err := storageformat.EncodeCanonical(objects)
	if err != nil {
		return err
	}
	if storageformat.Digest(encoded) != checkpoint.InventoryDigest || !reflect.DeepEqual(objects, checkpoint.Objects) {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory mismatch")
	}
	return nil
}

func (e *Engine) openWritesAfterCreatedCheckpoint(ctx context.Context, checkpoint storageformat.Checkpoint) error {
	stored, err := e.readCheckpoint(ctx, checkpoint.CheckpointID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, checkpoint) {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint changed before write gate opened")
	}
	return e.openClosedWriteGate(ctx, checkpoint.CheckpointID)
}

func (e *Engine) readCheckpoint(ctx context.Context, checkpointID string) (storageformat.Checkpoint, error) {
	if checkpointID == "" {
		return storageformat.Checkpoint{}, domain.NewError(domain.ErrorInvalid, "checkpoint ID is required")
	}
	key := storageformat.CheckpointKey(checkpointID)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return storageformat.Checkpoint{}, err
	}
	var envelope storageformat.Envelope
	var checkpoint storageformat.Checkpoint
	if err := state.DecodeJSONWithLimit(object.Body, &envelope, storageformat.MaxCanonicalBytes); err != nil {
		return storageformat.Checkpoint{}, err
	}
	schema := envelope.Schema
	if schema != checkpointSchema && schema != checkpointSchemaV2 && schema != checkpointSchemaV3 {
		return storageformat.Checkpoint{}, domain.NewError(domain.ErrorPreconditionFailed, "incompatible checkpoint")
	}
	if err := storageformat.DecodeEnvelope(object.Body, key, schema, &envelope, &checkpoint); err != nil {
		return storageformat.Checkpoint{}, err
	}
	if checkpoint.CheckpointID != checkpointID || checkpoint.KeyFormatVersion != storageformat.KeyFormatVersion || checkpoint.WriterProtocolVersion != storageformat.WriterProtocolVersion || checkpoint.InventoryDigest == "" {
		return storageformat.Checkpoint{}, domain.NewError(domain.ErrorPreconditionFailed, "incompatible checkpoint")
	}
	if checkpoint.SchemaVersion == 1 {
		if schema != checkpointSchema || len(checkpoint.Objects) == 0 || checkpoint.InventoryPageCount != 0 || checkpoint.StateObjectCount != 0 || checkpoint.FileObjectCount != 0 {
			return storageformat.Checkpoint{}, domain.NewError(domain.ErrorPreconditionFailed, "incompatible checkpoint")
		}
		return checkpoint, nil
	}
	if (checkpoint.SchemaVersion != 2 || schema != checkpointSchemaV2) && (checkpoint.SchemaVersion != 3 || schema != checkpointSchemaV3) || len(checkpoint.Objects) != 0 || checkpoint.InventoryPageCount == 0 || checkpoint.StateObjectCount > math.MaxUint64-checkpoint.FileObjectCount || checkpoint.StateObjectCount+checkpoint.FileObjectCount == 0 {
		return storageformat.Checkpoint{}, domain.NewError(domain.ErrorPreconditionFailed, "incompatible checkpoint")
	}
	return checkpoint, nil
}

type checkpointInventorySummary struct {
	pageCount       uint64
	stateObjects    uint64
	fileObjects     uint64
	inventoryDigest string
}

// prepareCheckpointInventoryMetadata builds checkpoint-v3 directly from
// provider metadata. It deliberately performs no object-body reads and writes
// no object-per-item journal. Immutable pages make a restart idempotent.
func (e *Engine) prepareCheckpointInventoryMetadata(ctx context.Context, checkpointID string, gateEpoch uint64) (checkpointInventorySummary, error) {
	previousDigest := checkpointInventorySeedV3(checkpointID, gateEpoch)
	pageIndex := uint64(0)
	entries := make([]storageformat.CheckpointInventoryEntry, 0, checkpointInventoryPageEntries)
	var stateObjects uint64
	var fileObjects uint64
	flush := func() error {
		if len(entries) == 0 {
			return nil
		}
		page := storageformat.CheckpointInventoryPage{
			SchemaVersion: 2, CheckpointID: checkpointID, GateEpoch: gateEpoch,
			Index: pageIndex, PreviousDigest: previousDigest,
			Entries: append([]storageformat.CheckpointInventoryEntry(nil), entries...),
		}
		key := storageformat.CheckpointInventoryPageKey(checkpointID, pageIndex)
		body, err := storageformat.EncodeEnvelope(checkpointInventoryPageSchemaV2, key, 1, page)
		if err != nil {
			return err
		}
		if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			if !errors.Is(err, domain.ErrConflict) {
				return err
			}
			existing, readErr := e.backend.Get(ctx, key)
			if readErr != nil || !reflect.DeepEqual(existing.Body, body) {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory page conflict")
			}
		}
		previousDigest = storageformat.Digest(body)
		if pageIndex == math.MaxUint64 {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory page count overflow")
		}
		pageIndex++
		entries = entries[:0]
		return nil
	}
	migrationID := ""
	if migration, found := migrationForCheckpoint(checkpointID); found {
		migrationID = migration.id.String()
	}
	completed := [2]int{}
	completedBytes := [2]int64{}
	report := func(fileData, final bool) {
		index := 0
		role := "state"
		if fileData {
			index = 1
			role = "file"
		}
		if !final && completed[index]%64 != 0 {
			return
		}
		totalObjects := 0
		totalBytes := int64(0)
		if final {
			totalObjects = completed[index]
			totalBytes = completedBytes[index]
		}
		e.observeMigration(MigrationProgress{
			MigrationID: migrationID, Stage: MigrationStageCheckpointInventory, Role: role,
			CompletedObjects: completed[index], TotalObjects: totalObjects,
			CompletedBytes: completedBytes[index], TotalBytes: totalBytes,
		})
	}
	if err := e.walkCheckpointMetadata(ctx, func(info objectstore.ObjectInfo, fileData bool) error {
		object, err := checkpointObjectFromInfo(info)
		if err != nil {
			return err
		}
		index := 0
		if fileData {
			index = 1
			if fileObjects == math.MaxUint64 {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint object count overflow")
			}
			fileObjects++
		} else {
			if stateObjects == math.MaxUint64 {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint object count overflow")
			}
			stateObjects++
		}
		entries = append(entries, storageformat.CheckpointInventoryEntry{FileData: fileData, Object: object})
		completed[index]++
		completedBytes[index] = saturatingInventoryBytes(completedBytes[index], object.Size)
		report(fileData, false)
		if len(entries) == checkpointInventoryPageEntries {
			return flush()
		}
		return nil
	}); err != nil {
		return checkpointInventorySummary{}, err
	}
	report(false, true)
	report(true, true)
	if err := flush(); err != nil {
		return checkpointInventorySummary{}, err
	}
	if pageIndex == 0 || stateObjects > math.MaxUint64-fileObjects {
		return checkpointInventorySummary{}, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory is empty or overflowing")
	}
	if err := e.validateCheckpointPageSet(ctx, checkpointID, pageIndex); err != nil {
		return checkpointInventorySummary{}, err
	}
	return checkpointInventorySummary{
		pageCount: pageIndex, stateObjects: stateObjects, fileObjects: fileObjects, inventoryDigest: previousDigest,
	}, nil
}

func checkpointObjectFromInfo(info objectstore.ObjectInfo) (storageformat.CheckpointObject, error) {
	if !info.Key.Valid() || info.Size < 0 || !info.Fingerprint.Complete() {
		return storageformat.CheckpointObject{}, domain.NewError(domain.ErrorPreconditionFailed, "provider did not attest complete MD5 and CRC32C object metadata")
	}
	return storageformat.CheckpointObject{
		Key: info.Key.String(), Size: info.Size, MD5: info.Fingerprint.MD5, CRC32C: info.Fingerprint.CRC32C,
	}, nil
}

func checkpointInventorySeedV3(checkpointID string, gateEpoch uint64) string {
	return storageformat.Digest([]byte("endlessfs-checkpoint-inventory-v3\x00" + checkpointID + "\x00" + strconv.FormatUint(gateEpoch, 10)))
}

func (e *Engine) prepareCheckpointInventory(ctx context.Context, checkpointID string, gateEpoch uint64) (checkpointInventorySummary, error) {
	stateObjects, err := e.checkpointInventoryRole(ctx, e.backend, false, checkpointID, gateEpoch)
	if err != nil {
		return checkpointInventorySummary{}, err
	}
	var fileBackend objectstore.MetadataBackend = e.backend
	if e.separateFileBackend {
		fileBackend = e.fileBackend
	}
	fileObjects, err := e.checkpointInventoryRole(ctx, fileBackend, true, checkpointID, gateEpoch)
	if err != nil {
		return checkpointInventorySummary{}, err
	}
	if stateObjects > math.MaxUint64-fileObjects {
		return checkpointInventorySummary{}, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint object count overflow")
	}
	return e.buildCheckpointInventoryPages(ctx, checkpointID, gateEpoch, stateObjects, fileObjects)
}

func (e *Engine) checkpointInventoryRole(ctx context.Context, backend objectstore.MetadataBackend, fileData bool, checkpointID string, gateEpoch uint64) (uint64, error) {
	totalObjects, totalBytes, err := e.checkpointRoleTotals(ctx, backend, fileData)
	if err != nil {
		return 0, err
	}
	var completedObjects uint64
	var resumedObjects uint64
	var completedBytes int64
	migrationID := ""
	if migration, found := migrationForCheckpoint(checkpointID); found {
		migrationID = migration.id.String()
	}
	reportProgress := func() {
		if completedObjects == totalObjects || completedObjects%64 == 0 {
			role := "state"
			if fileData {
				role = "file"
			}
			e.observeMigration(MigrationProgress{
				MigrationID: migrationID, Stage: MigrationStageCheckpointInventory, Role: role,
				CompletedObjects: boundedProgressCount(completedObjects), TotalObjects: boundedProgressCount(totalObjects),
				CompletedBytes: completedBytes, TotalBytes: totalBytes, ResumedObjects: boundedProgressCount(resumedObjects),
			})
		}
	}
	err = walkObjectInfos(ctx, backend, "endlessfs/v1/", func(info objectstore.ObjectInfo) error {
		included, includeErr := e.checkpointRoleIncludes(info.Key.String(), fileData)
		if includeErr != nil || !included {
			return includeErr
		}
		entry, found, readErr := e.readCheckpointWorkEntry(ctx, checkpointID, gateEpoch, info.Key.String())
		if readErr != nil {
			return readErr
		}
		if found {
			if entry.fileData != fileData || entry.object.Size != info.Size {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint work does not match authoritative object")
			}
			if _, verifyErr := backend.Verify(ctx, info.Key, objectstore.ExpectedIntegrity{Size: entry.object.Size, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: entry.crc32c}}); verifyErr != nil {
				return verifyErr
			}
			resumedObjects++
			completedObjects++
			completedBytes = saturatingInventoryBytes(completedBytes, entry.object.Size)
			reportProgress()
			return nil
		}
		checkpointObject, crc32c, streamErr := streamCheckpointObject(ctx, backend, info)
		if streamErr != nil {
			return streamErr
		}
		progress := storageformat.CheckpointWork{SchemaVersion: 1, CheckpointID: checkpointID, GateEpoch: gateEpoch, FileData: fileData, Object: checkpointObject, CRC32C: crc32c}
		if writeErr := e.writeCheckpointWork(ctx, progress); writeErr != nil {
			return writeErr
		}
		completedObjects++
		completedBytes = saturatingInventoryBytes(completedBytes, checkpointObject.Size)
		reportProgress()
		return nil
	})
	if err != nil {
		return 0, err
	}
	if totalObjects == 0 {
		reportProgress()
	}
	if completedObjects != totalObjects {
		return 0, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory count changed")
	}
	return completedObjects, nil
}

func (e *Engine) checkpointRoleTotals(ctx context.Context, backend objectstore.MetadataBackend, fileData bool) (uint64, int64, error) {
	var objects uint64
	var totalBytes int64
	err := walkObjectInfos(ctx, backend, "endlessfs/v1/", func(info objectstore.ObjectInfo) error {
		included, err := e.checkpointRoleIncludes(info.Key.String(), fileData)
		if err != nil || !included {
			return err
		}
		if info.Size < 0 || objects == math.MaxUint64 {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory size is invalid")
		}
		objects++
		totalBytes = saturatingInventoryBytes(totalBytes, info.Size)
		return nil
	})
	return objects, totalBytes, err
}

func (e *Engine) checkpointRoleIncludes(key string, fileData bool) (bool, error) {
	if transientOrCheckpoint(key) {
		return false, nil
	}
	isFileData := isFileDataKey(key)
	if e.separateFileBackend && fileData != isFileData {
		return false, domain.NewError(domain.ErrorPreconditionFailed, "canonical object is stored in the wrong backend")
	}
	return fileData == isFileData, nil
}

func walkObjectInfos(ctx context.Context, backend objectstore.MetadataBackend, prefix string, visit func(objectstore.ObjectInfo) error) error {
	request := objectstore.ListRequest{Prefix: prefix, Limit: 1000}
	previousKey := ""
	for {
		page, err := backend.List(ctx, request)
		if err != nil {
			return err
		}
		for _, info := range page.Objects {
			key := info.Key.String()
			if key == "" || !strings.HasPrefix(key, prefix) || (previousKey != "" && key <= previousKey) {
				return domain.NewError(domain.ErrorPreconditionFailed, "object listing is not in canonical key order")
			}
			if err := visit(info); err != nil {
				return err
			}
			previousKey = key
		}
		if page.NextCursor == "" {
			return nil
		}
		request.Cursor = page.NextCursor
	}
}

func (e *Engine) readCheckpointWorkEntry(ctx context.Context, checkpointID string, gateEpoch uint64, objectKey string) (checkpointWorkEntry, bool, error) {
	key := storageformat.CheckpointWorkKey(checkpointID, objectKey)
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return checkpointWorkEntry{}, false, nil
	}
	if err != nil {
		return checkpointWorkEntry{}, false, err
	}
	entry, err := e.decodeCheckpointWork(object, checkpointID, gateEpoch)
	return entry, err == nil, err
}

func (e *Engine) decodeCheckpointWork(object objectstore.Object, checkpointID string, gateEpoch uint64) (checkpointWorkEntry, error) {
	var envelope storageformat.Envelope
	var progress storageformat.CheckpointWork
	if err := storageformat.DecodeEnvelope(object.Body, object.Key, checkpointWorkSchema, &envelope, &progress); err != nil {
		return checkpointWorkEntry{}, err
	}
	parsedKey, keyErr := objectstore.ParseKey(progress.Object.Key)
	integrity := objectstore.ExpectedIntegrity{Size: progress.Object.Size, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: progress.CRC32C}}
	if keyErr != nil || transientOrCheckpoint(progress.Object.Key) || progress.FileData != isFileDataKey(progress.Object.Key) || progress.SchemaVersion != 1 || progress.CheckpointID != checkpointID || progress.GateEpoch != gateEpoch || progress.Object.Size < 0 || progress.Object.SHA256 == "" || integrity.Validate() != nil || storageformat.CheckpointWorkKey(checkpointID, parsedKey.String()) != object.Key {
		return checkpointWorkEntry{}, domain.NewError(domain.ErrorPreconditionFailed, "invalid checkpoint work record")
	}
	proof, proofErr := e.checkpointWorkProof(progress)
	if proofErr != nil || !hmac.Equal([]byte(progress.Proof), []byte(proof)) {
		return checkpointWorkEntry{}, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint work authentication failed")
	}
	return checkpointWorkEntry{fileData: progress.FileData, object: progress.Object, crc32c: progress.CRC32C}, nil
}

func (e *Engine) writeCheckpointWork(ctx context.Context, progress storageformat.CheckpointWork) error {
	proof, err := e.checkpointWorkProof(progress)
	if err != nil {
		return err
	}
	progress.Proof = proof
	key := storageformat.CheckpointWorkKey(progress.CheckpointID, progress.Object.Key)
	body, err := storageformat.EncodeEnvelope(checkpointWorkSchema, key, 1, progress)
	if err != nil {
		return err
	}
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return err
		}
		existing, readErr := e.backend.Get(ctx, key)
		if readErr != nil || !reflect.DeepEqual(existing.Body, body) {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint work record conflict")
		}
	}
	return nil
}

func (e *Engine) buildCheckpointInventoryPages(ctx context.Context, checkpointID string, gateEpoch, stateObjects, fileObjects uint64) (checkpointInventorySummary, error) {
	previousDigest := checkpointInventorySeed(checkpointID, gateEpoch)
	pageIndex := uint64(0)
	entries := make([]storageformat.CheckpointInventoryEntry, 0, checkpointInventoryPageEntries)
	var seenState uint64
	var seenFile uint64
	flush := func() error {
		if len(entries) == 0 {
			return nil
		}
		page := storageformat.CheckpointInventoryPage{SchemaVersion: 1, CheckpointID: checkpointID, GateEpoch: gateEpoch, Index: pageIndex, PreviousDigest: previousDigest, Entries: entries}
		key := storageformat.CheckpointInventoryPageKey(checkpointID, pageIndex)
		body, err := storageformat.EncodeEnvelope(checkpointInventoryPageSchema, key, 1, page)
		if err != nil {
			return err
		}
		if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			if !errors.Is(err, domain.ErrConflict) {
				return err
			}
			existing, readErr := e.backend.Get(ctx, key)
			if readErr != nil || !reflect.DeepEqual(existing.Body, body) {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory page conflict")
			}
		}
		previousDigest = storageformat.Digest(body)
		if pageIndex == math.MaxUint64 {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory page count overflow")
		}
		pageIndex++
		entries = make([]storageformat.CheckpointInventoryEntry, 0, checkpointInventoryPageEntries)
		return nil
	}
	err := walkObjectInfos(ctx, e.backend, storageformat.CheckpointWorkPrefix(checkpointID), func(info objectstore.ObjectInfo) error {
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		entry, err := e.decodeCheckpointWork(object, checkpointID, gateEpoch)
		if err != nil {
			return err
		}
		if entry.fileData {
			if seenFile == math.MaxUint64 {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint object count overflow")
			}
			seenFile++
		} else {
			if seenState == math.MaxUint64 {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint object count overflow")
			}
			seenState++
		}
		entries = append(entries, storageformat.CheckpointInventoryEntry{FileData: entry.fileData, Object: entry.object})
		if len(entries) == checkpointInventoryPageEntries {
			return flush()
		}
		return nil
	})
	if err != nil {
		return checkpointInventorySummary{}, err
	}
	if err := flush(); err != nil {
		return checkpointInventorySummary{}, err
	}
	if seenState != stateObjects || seenFile != fileObjects || pageIndex == 0 {
		return checkpointInventorySummary{}, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint work references a missing authoritative object")
	}
	if err := e.validateCheckpointPageSet(ctx, checkpointID, pageIndex); err != nil {
		return checkpointInventorySummary{}, err
	}
	return checkpointInventorySummary{pageCount: pageIndex, stateObjects: stateObjects, fileObjects: fileObjects, inventoryDigest: previousDigest}, nil
}

func (e *Engine) validateCheckpointPageSet(ctx context.Context, checkpointID string, pageCount uint64) error {
	var index uint64
	err := walkObjectInfos(ctx, e.backend, storageformat.CheckpointInventoryPagePrefix(checkpointID), func(info objectstore.ObjectInfo) error {
		if index >= pageCount || info.Key != storageformat.CheckpointInventoryPageKey(checkpointID, index) {
			return domain.NewError(domain.ErrorPreconditionFailed, "unexpected checkpoint inventory page")
		}
		index++
		return nil
	})
	if err != nil {
		return err
	}
	if index != pageCount {
		return domain.NewError(domain.ErrorPreconditionFailed, "missing checkpoint inventory page")
	}
	return nil
}

func checkpointInventorySeed(checkpointID string, gateEpoch uint64) string {
	return storageformat.Digest([]byte("endlessfs-checkpoint-inventory-v2\x00" + checkpointID + "\x00" + strconv.FormatUint(gateEpoch, 10)))
}

func boundedProgressCount(value uint64) int {
	maximum := uint64(^uint(0) >> 1)
	if value > maximum {
		return int(maximum)
	}
	return int(value)
}

func saturatingInventoryBytes(current, add int64) int64 {
	if add < 0 || current > math.MaxInt64-add {
		return math.MaxInt64
	}
	return current + add
}

func streamCheckpointObject(ctx context.Context, backend objectstore.MetadataBackend, expected objectstore.ObjectInfo) (storageformat.CheckpointObject, string, error) {
	current := expected
	if !current.Fingerprint.Complete() {
		var err error
		current, err = backend.Head(ctx, expected.Key)
		if err != nil {
			return storageformat.CheckpointObject{}, "", err
		}
	}
	if current.Key != expected.Key || current.Size != expected.Size || (expected.Version != "" && current.Version != expected.Version) {
		return storageformat.CheckpointObject{}, "", domain.NewError(domain.ErrorPreconditionFailed, "authoritative object changed during checkpoint")
	}
	object, err := checkpointObjectFromInfo(current)
	if err != nil {
		return storageformat.CheckpointObject{}, "", err
	}
	return object, object.CRC32C, nil
}

// VisitCheckpointObjects streams a checkpoint's provider-independent object
// inventory without materializing the complete file set in process memory.
func (e *Engine) VisitCheckpointObjects(ctx context.Context, checkpointID string, visit func(storageformat.CheckpointObject) error) error {
	if visit == nil {
		return domain.NewError(domain.ErrorInvalid, "checkpoint visitor is required")
	}
	checkpoint, err := e.readCheckpoint(ctx, checkpointID)
	if err != nil {
		return err
	}
	return e.visitCheckpointInventory(ctx, checkpoint, func(entry storageformat.CheckpointInventoryEntry) error {
		return visit(entry.Object)
	})
}

func (e *Engine) visitCheckpointInventory(ctx context.Context, checkpoint storageformat.Checkpoint, visit func(storageformat.CheckpointInventoryEntry) error) error {
	if checkpoint.SchemaVersion == 1 {
		for _, object := range checkpoint.Objects {
			if visit != nil {
				if err := visit(storageformat.CheckpointInventoryEntry{FileData: isFileDataKey(object.Key), Object: object}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	pageSchema := checkpointInventoryPageSchema
	pageVersion := 1
	previousDigest := checkpointInventorySeed(checkpoint.CheckpointID, checkpoint.GateEpoch)
	if checkpoint.SchemaVersion == 3 {
		pageSchema = checkpointInventoryPageSchemaV2
		pageVersion = 2
		previousDigest = checkpointInventorySeedV3(checkpoint.CheckpointID, checkpoint.GateEpoch)
	}
	previousWorkKey := ""
	previousInventoryKey := ""
	var stateObjects uint64
	var fileObjects uint64
	for index := uint64(0); index < checkpoint.InventoryPageCount; index++ {
		key := storageformat.CheckpointInventoryPageKey(checkpoint.CheckpointID, index)
		object, err := e.backend.Get(ctx, key)
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var page storageformat.CheckpointInventoryPage
		if err := storageformat.DecodeEnvelope(object.Body, key, pageSchema, &envelope, &page); err != nil {
			return err
		}
		if page.SchemaVersion != pageVersion || page.CheckpointID != checkpoint.CheckpointID || page.GateEpoch != checkpoint.GateEpoch || page.Index != index || page.PreviousDigest != previousDigest || len(page.Entries) == 0 || len(page.Entries) > checkpointInventoryPageEntries {
			return domain.NewError(domain.ErrorPreconditionFailed, "invalid checkpoint inventory page")
		}
		for _, entry := range page.Entries {
			parsed, parseErr := objectstore.ParseKey(entry.Object.Key)
			workKey := ""
			if parseErr == nil {
				workKey = storageformat.CheckpointWorkKey(checkpoint.CheckpointID, parsed.String()).String()
			}
			validDigest := entry.Object.SHA256 != ""
			if checkpoint.SchemaVersion == 3 {
				validDigest = objectstore.ContentFingerprint{MD5: entry.Object.MD5, CRC32C: entry.Object.CRC32C}.Complete() && entry.Object.SHA256 == ""
				inventoryKey := entry.Object.Key
				if previousInventoryKey != "" && inventoryKey <= previousInventoryKey {
					validDigest = false
				}
				previousInventoryKey = inventoryKey
			}
			if parseErr != nil || transientOrCheckpoint(entry.Object.Key) || entry.FileData != isFileDataKey(entry.Object.Key) || entry.Object.Size < 0 || !validDigest || (checkpoint.SchemaVersion < 3 && previousWorkKey != "" && workKey <= previousWorkKey) {
				return domain.NewError(domain.ErrorPreconditionFailed, "invalid checkpoint inventory entry")
			}
			previousWorkKey = workKey
			if entry.FileData {
				if fileObjects == math.MaxUint64 {
					return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint object count overflow")
				}
				fileObjects++
			} else {
				if stateObjects == math.MaxUint64 {
					return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint object count overflow")
				}
				stateObjects++
			}
			if visit != nil {
				if err := visit(entry); err != nil {
					return err
				}
			}
		}
		previousDigest = storageformat.Digest(object.Body)
	}
	if previousDigest != checkpoint.InventoryDigest || stateObjects != checkpoint.StateObjectCount || fileObjects != checkpoint.FileObjectCount {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory mismatch")
	}
	return e.validateCheckpointPageSet(ctx, checkpoint.CheckpointID, checkpoint.InventoryPageCount)
}

func (e *Engine) verifyCheckpointV2Summary(checkpoint storageformat.Checkpoint, gate storageformat.WriteGate, summary checkpointInventorySummary) error {
	if gate.Mode != storageformat.GateClosed || gate.Epoch != checkpoint.GateEpoch || gate.CheckpointID != checkpoint.CheckpointID || checkpoint.WriterSetID != e.writer.WriterSetID || checkpoint.InventoryPageCount != summary.pageCount || checkpoint.StateObjectCount != summary.stateObjects || checkpoint.FileObjectCount != summary.fileObjects || checkpoint.InventoryDigest != summary.inventoryDigest {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint does not match closed write gate")
	}
	return nil
}

func (e *Engine) verifyCheckpointV3(ctx context.Context, checkpoint storageformat.Checkpoint, gate storageformat.WriteGate) error {
	if gate.Mode != storageformat.GateClosed || gate.Epoch != checkpoint.GateEpoch || gate.CheckpointID != checkpoint.CheckpointID || checkpoint.WriterSetID != e.writer.WriterSetID {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint does not match closed write gate")
	}
	metadata := newCheckpointMetadataStream(e)
	if err := e.visitCheckpointInventory(ctx, checkpoint, func(entry storageformat.CheckpointInventoryEntry) error {
		info, fileData, found, err := metadata.next(ctx)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory is longer than authoritative metadata")
		}
		if entry.FileData != fileData {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory backend role mismatch")
		}
		if entry.Object.Key != info.Key.String() {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory ordering mismatch")
		}
		if !checkpointObjectMatchesInfo(entry.Object, info) {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory metadata mismatch")
		}
		return nil
	}); err != nil {
		return err
	}
	if _, _, found, err := metadata.next(ctx); err != nil || found {
		if err != nil {
			return err
		}
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory has extra objects")
	}
	return nil
}

func checkpointObjectMatchesInfo(expected storageformat.CheckpointObject, actual objectstore.ObjectInfo) bool {
	return expected.Key == actual.Key.String() && expected.Size == actual.Size && expected.MD5 == actual.Fingerprint.MD5 && expected.CRC32C == actual.Fingerprint.CRC32C && actual.Fingerprint.Complete()
}

type checkpointMetadataIterator struct {
	engine      *Engine
	backend     objectstore.MetadataBackend
	role        checkpointMetadataRole
	request     objectstore.ListRequest
	page        objectstore.ListPage
	index       int
	done        bool
	previousKey string
}

type checkpointMetadataRole uint8

const (
	checkpointMetadataAll checkpointMetadataRole = iota
	checkpointMetadataState
	checkpointMetadataFile
)

func newCheckpointMetadataIterator(engine *Engine, backend objectstore.MetadataBackend, role checkpointMetadataRole) *checkpointMetadataIterator {
	return &checkpointMetadataIterator{
		engine: engine, backend: backend, role: role,
		request: objectstore.ListRequest{Prefix: "endlessfs/v1/", Limit: 1000},
	}
}

func (iterator *checkpointMetadataIterator) next(ctx context.Context) (objectstore.ObjectInfo, bool, error) {
	for {
		if iterator.index < len(iterator.page.Objects) {
			info := iterator.page.Objects[iterator.index]
			iterator.index++
			key := info.Key.String()
			if key == "" || !strings.HasPrefix(key, iterator.request.Prefix) || (iterator.previousKey != "" && key <= iterator.previousKey) {
				return objectstore.ObjectInfo{}, false, domain.NewError(domain.ErrorPreconditionFailed, "object listing is not in canonical key order")
			}
			iterator.previousKey = key
			included := !transientOrCheckpoint(key)
			var err error
			if iterator.role != checkpointMetadataAll {
				included, err = iterator.engine.checkpointRoleIncludes(key, iterator.role == checkpointMetadataFile)
			}
			if err != nil {
				return objectstore.ObjectInfo{}, false, err
			}
			if included {
				return info, true, nil
			}
			continue
		}
		if iterator.done {
			return objectstore.ObjectInfo{}, false, nil
		}
		page, err := iterator.backend.List(ctx, iterator.request)
		if err != nil {
			return objectstore.ObjectInfo{}, false, err
		}
		iterator.page = page
		iterator.index = 0
		if page.NextCursor == "" {
			iterator.done = true
		} else {
			iterator.request.Cursor = page.NextCursor
		}
	}
}

type checkpointMetadataStream struct {
	engine *Engine
	single *checkpointMetadataIterator
	state  *checkpointMetadataIterator
	file   *checkpointMetadataIterator

	stateInfo  objectstore.ObjectInfo
	stateFound bool
	stateReady bool
	fileInfo   objectstore.ObjectInfo
	fileFound  bool
	fileReady  bool
}

func newCheckpointMetadataStream(engine *Engine) *checkpointMetadataStream {
	stream := &checkpointMetadataStream{engine: engine}
	if !engine.separateFileBackend {
		stream.single = newCheckpointMetadataIterator(engine, engine.backend, checkpointMetadataAll)
		return stream
	}
	stream.state = newCheckpointMetadataIterator(engine, engine.backend, checkpointMetadataState)
	stream.file = newCheckpointMetadataIterator(engine, engine.fileBackend, checkpointMetadataFile)
	return stream
}

func (stream *checkpointMetadataStream) next(ctx context.Context) (objectstore.ObjectInfo, bool, bool, error) {
	if stream.single != nil {
		info, found, err := stream.single.next(ctx)
		return info, found && isFileDataKey(info.Key.String()), found, err
	}
	if !stream.stateReady {
		info, found, err := stream.state.next(ctx)
		if err != nil {
			return objectstore.ObjectInfo{}, false, false, err
		}
		stream.stateInfo, stream.stateFound, stream.stateReady = info, found, true
	}
	if !stream.fileReady {
		info, found, err := stream.file.next(ctx)
		if err != nil {
			return objectstore.ObjectInfo{}, false, false, err
		}
		stream.fileInfo, stream.fileFound, stream.fileReady = info, found, true
	}
	if !stream.stateFound && !stream.fileFound {
		return objectstore.ObjectInfo{}, false, false, nil
	}
	if stream.stateFound && (!stream.fileFound || stream.stateInfo.Key.String() < stream.fileInfo.Key.String()) {
		info := stream.stateInfo
		stream.stateReady = false
		return info, false, true, nil
	}
	if stream.fileFound && (!stream.stateFound || stream.fileInfo.Key.String() < stream.stateInfo.Key.String()) {
		info := stream.fileInfo
		stream.fileReady = false
		return info, true, true, nil
	}
	return objectstore.ObjectInfo{}, false, false, domain.NewError(domain.ErrorPreconditionFailed, "canonical object is duplicated across backend roles")
}

func (e *Engine) walkCheckpointMetadata(ctx context.Context, visit func(objectstore.ObjectInfo, bool) error) error {
	stream := newCheckpointMetadataStream(e)
	for {
		info, fileData, found, err := stream.next(ctx)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := visit(info, fileData); err != nil {
			return err
		}
	}
}

func (e *Engine) authoritativeInventory(ctx context.Context) ([]storageformat.CheckpointObject, error) {
	objects, err := e.authoritativeInventoryFrom(ctx, e.backend, false)
	if err != nil {
		return nil, err
	}
	if e.separateFileBackend {
		fileObjects, fileErr := e.authoritativeInventoryFrom(ctx, e.fileBackend, true)
		if fileErr != nil {
			return nil, fileErr
		}
		objects = append(objects, fileObjects...)
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

type checkpointWorkEntry struct {
	fileData bool
	object   storageformat.CheckpointObject
	crc32c   string
}

func (e *Engine) checkpointWorkProof(progress storageformat.CheckpointWork) (string, error) {
	if len(e.checkpointWorkKey) != 32 {
		return "", domain.NewError(domain.ErrorInvalid, "checkpoint work key is unavailable")
	}
	progress.Proof = ""
	body, err := storageformat.EncodeCanonical(progress)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, e.checkpointWorkKey)
	_, _ = mac.Write([]byte("endlessfs-checkpoint-work-v1"))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (e *Engine) authoritativeInventoryFrom(ctx context.Context, backend objectstore.MetadataBackend, fileData bool) ([]storageformat.CheckpointObject, error) {
	infos, err := listAllFrom(ctx, backend, "endlessfs/v1/")
	if err != nil {
		return nil, err
	}
	objects := make([]storageformat.CheckpointObject, 0, len(infos))
	for _, info := range infos {
		key := info.Key.String()
		if transientOrCheckpoint(key) {
			continue
		}
		if e.separateFileBackend && fileData != isFileDataKey(key) {
			return nil, domain.NewError(domain.ErrorPreconditionFailed, "canonical object is stored in the wrong backend")
		}
		entry, _, streamErr := streamCheckpointObject(ctx, backend, info)
		if streamErr != nil {
			return nil, streamErr
		}
		objects = append(objects, entry)
	}
	return objects, nil
}

func isFileDataKey(key string) bool {
	segments := strings.Split(key, "/")
	return len(segments) == 6 && segments[0] == "endlessfs" && segments[1] == "v1" && segments[2] == "fs" && segments[3] != "" && segments[4] == "blobs" && segments[5] != ""
}

func transientOrCheckpoint(key string) bool {
	for _, prefix := range []string{
		"endlessfs/v1/admissions/",
		"endlessfs/v1/staging/",
		"endlessfs/v1/leases/",
		"endlessfs/v1/checkpoints/",
		"endlessfs/v1/maintenance/",
	} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
