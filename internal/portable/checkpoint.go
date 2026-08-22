package portable

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
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
	checkpointSchema               = "checkpoint-v1"
	checkpointSchemaV2             = "checkpoint-v2"
	checkpointWorkSchema           = "checkpoint-work-v1"
	checkpointInventoryPageSchema  = "checkpoint-inventory-page-v1"
	checkpointInventoryPageEntries = 512
)

// VerifyCheckpointReadOnly validates an existing canonical bucket and closed
// checkpoint without initializing, repairing, or otherwise writing storage.
func VerifyCheckpointReadOnly(ctx context.Context, backend objectstore.Backend, writerConfiguration WriterConfiguration, checkpointID string) error {
	return VerifyCheckpointReadOnlyWithFileBackend(ctx, backend, nil, writerConfiguration, checkpointID)
}

// VerifyCheckpointReadOnlyWithFileBackend validates a checkpoint whose
// canonical metadata and file bytes may be stored on distinct backends. A nil
// file backend selects the state backend and preserves the one-bucket layout.
func VerifyCheckpointReadOnlyWithFileBackend(ctx context.Context, backend, fileBackend objectstore.Backend, writerConfiguration WriterConfiguration, checkpointID string) error {
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
	summary, err := e.prepareCheckpointInventory(ctx, checkpointID, gate.Epoch)
	if err != nil {
		return storageformat.Checkpoint{}, err
	}
	checkpoint := storageformat.Checkpoint{
		SchemaVersion: 2, CheckpointID: checkpointID, BucketID: superblock.BucketID,
		WriterSetID: e.writer.WriterSetID, GateEpoch: gate.Epoch,
		KeyFormatVersion: storageformat.KeyFormatVersion, WriterProtocolVersion: storageformat.WriterProtocolVersion,
		CreatedAt: e.clock.Now().UTC(), InventoryPageCount: summary.pageCount,
		StateObjectCount: summary.stateObjects, FileObjectCount: summary.fileObjects,
		InventoryDigest: summary.inventoryDigest,
	}
	key := storageformat.CheckpointKey(checkpointID)
	body, err := storageformat.EncodeEnvelope(checkpointSchemaV2, key, 1, checkpoint)
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
		if existing.SchemaVersion == 1 {
			if err := e.VerifyCheckpoint(ctx, checkpointID); err != nil {
				return storageformat.Checkpoint{}, err
			}
			return existing, nil
		}
		if err := e.verifyCheckpointV2Summary(existing, gate, summary); err != nil {
			return storageformat.Checkpoint{}, err
		}
		return existing, nil
	}
	return checkpoint, nil
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
	if checkpoint.SchemaVersion == 2 {
		return e.verifyCheckpointV2(ctx, checkpoint, gate)
	}
	objects, inventoryErr := e.authoritativeInventory(ctx)
	if inventoryErr != nil {
		return inventoryErr
	}
	return e.verifyCheckpointInventory(checkpoint, gate, objects)
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
	if schema != checkpointSchema && schema != checkpointSchemaV2 {
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
	if checkpoint.SchemaVersion != 2 || schema != checkpointSchemaV2 || len(checkpoint.Objects) != 0 || checkpoint.InventoryPageCount == 0 || checkpoint.StateObjectCount > math.MaxUint64-checkpoint.FileObjectCount || checkpoint.StateObjectCount+checkpoint.FileObjectCount == 0 {
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

func (e *Engine) prepareCheckpointInventory(ctx context.Context, checkpointID string, gateEpoch uint64) (checkpointInventorySummary, error) {
	stateObjects, err := e.checkpointInventoryRole(ctx, e.backend, false, checkpointID, gateEpoch)
	if err != nil {
		return checkpointInventorySummary{}, err
	}
	fileBackend := e.backend
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

func (e *Engine) checkpointInventoryRole(ctx context.Context, backend objectstore.Backend, fileData bool, checkpointID string, gateEpoch uint64) (uint64, error) {
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

func (e *Engine) checkpointRoleTotals(ctx context.Context, backend objectstore.Backend, fileData bool) (uint64, int64, error) {
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

func walkObjectInfos(ctx context.Context, backend objectstore.Backend, prefix string, visit func(objectstore.ObjectInfo) error) error {
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

func streamCheckpointObject(ctx context.Context, backend objectstore.Backend, expected objectstore.ObjectInfo) (storageformat.CheckpointObject, string, error) {
	stream, err := backend.Open(ctx, expected.Key)
	if err != nil {
		return storageformat.CheckpointObject{}, "", err
	}
	sha256Hash := sha256.New()
	crc32cHash := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	written, readErr := io.CopyBuffer(io.MultiWriter(sha256Hash, crc32cHash), stream.Body, make([]byte, 128<<10))
	closeErr := stream.Body.Close()
	if readErr != nil {
		return storageformat.CheckpointObject{}, "", readErr
	}
	if closeErr != nil {
		return storageformat.CheckpointObject{}, "", closeErr
	}
	if stream.Key != expected.Key || stream.Size != expected.Size || written != expected.Size || (expected.Version != "" && stream.Version != expected.Version) {
		return storageformat.CheckpointObject{}, "", domain.NewError(domain.ErrorPreconditionFailed, "authoritative object changed during checkpoint")
	}
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32cHash.Sum32())
	return storageformat.CheckpointObject{Key: expected.Key.String(), Size: written, SHA256: base64.RawURLEncoding.EncodeToString(sha256Hash.Sum(nil))}, base64.RawURLEncoding.EncodeToString(checksum[:]), nil
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
	previousDigest := checkpointInventorySeed(checkpoint.CheckpointID, checkpoint.GateEpoch)
	previousWorkKey := ""
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
		if err := storageformat.DecodeEnvelope(object.Body, key, checkpointInventoryPageSchema, &envelope, &page); err != nil {
			return err
		}
		if page.SchemaVersion != 1 || page.CheckpointID != checkpoint.CheckpointID || page.GateEpoch != checkpoint.GateEpoch || page.Index != index || page.PreviousDigest != previousDigest || len(page.Entries) == 0 || len(page.Entries) > checkpointInventoryPageEntries {
			return domain.NewError(domain.ErrorPreconditionFailed, "invalid checkpoint inventory page")
		}
		for _, entry := range page.Entries {
			parsed, parseErr := objectstore.ParseKey(entry.Object.Key)
			workKey := ""
			if parseErr == nil {
				workKey = storageformat.CheckpointWorkKey(checkpoint.CheckpointID, parsed.String()).String()
			}
			if parseErr != nil || transientOrCheckpoint(entry.Object.Key) || entry.FileData != isFileDataKey(entry.Object.Key) || entry.Object.Size < 0 || entry.Object.SHA256 == "" || (previousWorkKey != "" && workKey <= previousWorkKey) {
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

func (e *Engine) verifyCheckpointV2(ctx context.Context, checkpoint storageformat.Checkpoint, gate storageformat.WriteGate) error {
	if gate.Mode != storageformat.GateClosed || gate.Epoch != checkpoint.GateEpoch || gate.CheckpointID != checkpoint.CheckpointID || checkpoint.WriterSetID != e.writer.WriterSetID {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint does not match closed write gate")
	}
	if err := e.visitCheckpointInventory(ctx, checkpoint, func(entry storageformat.CheckpointInventoryEntry) error {
		backend := e.backend
		if entry.FileData && e.separateFileBackend {
			backend = e.fileBackend
		}
		key := objectstore.MustKey(entry.Object.Key)
		actual, _, err := streamCheckpointObject(ctx, backend, objectstore.ObjectInfo{Key: key, Size: entry.Object.Size})
		if err != nil {
			return err
		}
		if actual.SHA256 != entry.Object.SHA256 {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory mismatch")
		}
		return nil
	}); err != nil {
		return err
	}
	stateObjects, _, err := e.checkpointRoleTotals(ctx, e.backend, false)
	if err != nil {
		return err
	}
	fileBackend := e.backend
	if e.separateFileBackend {
		fileBackend = e.fileBackend
	}
	fileObjects, _, err := e.checkpointRoleTotals(ctx, fileBackend, true)
	if err != nil {
		return err
	}
	if stateObjects != checkpoint.StateObjectCount || fileObjects != checkpoint.FileObjectCount {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory mismatch")
	}
	return nil
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

func (e *Engine) authoritativeInventoryFrom(ctx context.Context, backend objectstore.Backend, fileData bool) ([]storageformat.CheckpointObject, error) {
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
	} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
