package portable

import (
	"context"
	"errors"
	"math"
	"reflect"
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
	schema008 := writeGateSchemaAtLeast(gate.WriterFeatures, storageSchema008, e.writer.RequiredFeatures)
	if schema008 {
		if err := e.validateSchema008CheckpointClosure(ctx, gate.Epoch); err != nil {
			return storageformat.Checkpoint{}, err
		}
	}
	summary, err := e.prepareCheckpointInventoryMetadata(ctx, checkpointID, gate.Epoch, schema008)
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
func (e *Engine) prepareCheckpointInventoryMetadata(ctx context.Context, checkpointID string, gateEpoch uint64, schema008 bool) (checkpointInventorySummary, error) {
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
	if err := e.walkCheckpointMetadata(ctx, schema008, func(info objectstore.ObjectInfo, fileData bool) error {
		metadataBackend := objectstore.MetadataBackend(e.backend)
		if fileData {
			metadataBackend = e.fileBackend
		}
		object, _, err := streamCheckpointObject(ctx, metadataBackend, info)
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

func (e *Engine) checkpointRoleIncludes(key string, fileData, schema008 bool) (bool, error) {
	if transientOrCheckpoint(key) {
		return false, nil
	}
	isFileData := isFileDataKey(key)
	if e.separateFileBackend && fileData != isFileData {
		return false, domain.NewError(domain.ErrorPreconditionFailed, "canonical object is stored in the wrong backend")
	}
	if fileData {
		return isFileData, nil
	}
	if schema008 {
		if isFileData {
			return false, nil
		}
		// Schema-007 metadata and all rebuildable projections are
		// intentionally absent from schema-008 portability checkpoints.
		// They are neither mutation authority nor needed to reconstruct it.
		return isSchema008AuthorityStateKey(key), nil
	}
	return !isFileData, nil
}

func isSchema008AuthorityStateKey(key string) bool {
	if key == storageformat.SuperblockKey().String() || key == storageformat.WriterSetKey().String() || key == storageformat.WriteGateKey().String() || key == storageformat.DomainCatalogHeadKey().String() {
		return true
	}
	if strings.HasPrefix(key, storageformat.TransitionPrefix()+"plans/") || strings.HasPrefix(key, storageformat.TransitionPrefix()+"decisions/") {
		return strings.HasSuffix(key, ".json")
	}
	segments := strings.Split(key, "/")
	if len(segments) == 6 && segments[0] == "endlessfs" && segments[1] == "v1" && segments[2] == "domains" && segments[3] == "catalog" && segments[4] == "pages" && strings.HasSuffix(segments[5], ".json") {
		return true
	}
	if len(segments) != 6 && len(segments) != 7 || segments[0] != "endlessfs" || segments[1] != "v1" || segments[2] != "domains" {
		return false
	}
	switch storageformat.ConsistencyDomainKind(segments[3]) {
	case storageformat.DomainNamespace, storageformat.DomainOwnerControl, storageformat.DomainAdmin, storageformat.DomainCapability, storageformat.DomainShare, storageformat.DomainIdentity, storageformat.DomainOwnerJobs:
	default:
		return false
	}
	if len(segments) == 6 {
		return segments[4] != "" && segments[5] == "head.json"
	}
	return segments[4] != "" && segments[5] == "pages" && strings.HasSuffix(segments[6], ".json")
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
	previousDigest := storageformat.Digest([]byte("endlessfs-checkpoint-inventory-v2\x00" + checkpoint.CheckpointID + "\x00" + strconv.FormatUint(checkpoint.GateEpoch, 10)))
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
	schema008 := writeGateSchemaAtLeast(gate.WriterFeatures, storageSchema008, e.writer.RequiredFeatures)
	if schema008 {
		if err := e.validateSchema008CheckpointClosure(ctx, gate.Epoch); err != nil {
			return err
		}
	}
	metadata := newCheckpointMetadataStream(e, schema008)
	var exact *schema008CheckpointMetadataStream
	if schema008 {
		var err error
		exact, err = newSchema008CheckpointMetadataStream(ctx, e)
		if err != nil {
			return err
		}
		defer exact.Close()
	}
	nextMetadata := func() (objectstore.ObjectInfo, bool, bool, error) {
		if exact != nil {
			return exact.next(ctx)
		}
		return metadata.next(ctx)
	}
	if err := e.visitCheckpointInventory(ctx, checkpoint, func(entry storageformat.CheckpointInventoryEntry) error {
		info, fileData, found, err := nextMetadata()
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
	if _, _, found, err := nextMetadata(); err != nil || found {
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
	schema008   bool
}

type checkpointMetadataRole uint8

const (
	checkpointMetadataAll checkpointMetadataRole = iota
	checkpointMetadataState
	checkpointMetadataFile
)

func newCheckpointMetadataIterator(engine *Engine, backend objectstore.MetadataBackend, role checkpointMetadataRole, schema008 bool) *checkpointMetadataIterator {
	return &checkpointMetadataIterator{
		engine: engine, backend: backend, role: role,
		request:   objectstore.ListRequest{Prefix: "endlessfs/v1/", Limit: 1000},
		schema008: schema008,
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
			if iterator.schema008 || iterator.role != checkpointMetadataAll {
				fileData := iterator.role == checkpointMetadataFile || iterator.role == checkpointMetadataAll && isFileDataKey(key)
				included, err = iterator.engine.checkpointRoleIncludes(key, fileData, iterator.schema008)
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

func newCheckpointMetadataStream(engine *Engine, schema008 bool) *checkpointMetadataStream {
	stream := &checkpointMetadataStream{engine: engine}
	if !engine.separateFileBackend {
		stream.single = newCheckpointMetadataIterator(engine, engine.backend, checkpointMetadataAll, schema008)
		return stream
	}
	stream.state = newCheckpointMetadataIterator(engine, engine.backend, checkpointMetadataState, schema008)
	stream.file = newCheckpointMetadataIterator(engine, engine.fileBackend, checkpointMetadataFile, schema008)
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

func (e *Engine) walkCheckpointMetadata(ctx context.Context, schema008 bool, visit func(objectstore.ObjectInfo, bool) error) error {
	if schema008 {
		stream, err := newSchema008CheckpointMetadataStream(ctx, e)
		if err != nil {
			return err
		}
		defer stream.Close()
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
	stream := newCheckpointMetadataStream(e, schema008)
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
