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
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const checkpointSchema = "checkpoint-v1"
const checkpointWorkSchema = "checkpoint-work-v1"

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
	objects, err := e.authoritativeInventoryResumable(ctx, checkpointID, gate.Epoch)
	if err != nil {
		return storageformat.Checkpoint{}, err
	}
	inventoryBody, err := storageformat.EncodeCanonical(objects)
	if err != nil {
		return storageformat.Checkpoint{}, err
	}
	checkpoint := storageformat.Checkpoint{
		SchemaVersion: 1, CheckpointID: checkpointID, BucketID: superblock.BucketID,
		WriterSetID: e.writer.WriterSetID, GateEpoch: gate.Epoch,
		KeyFormatVersion: storageformat.KeyFormatVersion, WriterProtocolVersion: storageformat.WriterProtocolVersion,
		CreatedAt: e.clock.Now().UTC(), Objects: objects, InventoryDigest: storageformat.Digest(inventoryBody),
	}
	key := storageformat.CheckpointKey(checkpointID)
	body, err := storageformat.EncodeEnvelope(checkpointSchema, key, 1, checkpoint)
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
		if err := e.verifyCheckpointInventory(existing, gate, objects); err != nil {
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
	objects, err := e.authoritativeInventory(ctx)
	if err != nil {
		return err
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
	if err := storageformat.DecodeEnvelope(object.Body, key, checkpointSchema, &envelope, &checkpoint); err != nil {
		return storageformat.Checkpoint{}, err
	}
	if checkpoint.SchemaVersion != 1 || checkpoint.CheckpointID != checkpointID || checkpoint.KeyFormatVersion != storageformat.KeyFormatVersion || checkpoint.WriterProtocolVersion != storageformat.WriterProtocolVersion {
		return storageformat.Checkpoint{}, domain.NewError(domain.ErrorPreconditionFailed, "incompatible checkpoint")
	}
	return checkpoint, nil
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

func (e *Engine) authoritativeInventoryResumable(ctx context.Context, checkpointID string, gateEpoch uint64) ([]storageformat.CheckpointObject, error) {
	work, err := e.readCheckpointWork(ctx, checkpointID, gateEpoch)
	if err != nil {
		return nil, err
	}
	objects, seen, err := e.authoritativeInventoryFromWork(ctx, e.backend, false, checkpointID, gateEpoch, work)
	if err != nil {
		return nil, err
	}
	if e.separateFileBackend {
		fileObjects, fileSeen, fileErr := e.authoritativeInventoryFromWork(ctx, e.fileBackend, true, checkpointID, gateEpoch, work)
		if fileErr != nil {
			return nil, fileErr
		}
		objects = append(objects, fileObjects...)
		for key := range fileSeen {
			seen[key] = struct{}{}
		}
	}
	for key := range work {
		if _, found := seen[key]; !found {
			return nil, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint work references a missing authoritative object")
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

func (e *Engine) readCheckpointWork(ctx context.Context, checkpointID string, gateEpoch uint64) (map[string]checkpointWorkEntry, error) {
	infos, err := listAllFrom(ctx, e.backend, storageformat.CheckpointWorkPrefix(checkpointID))
	if err != nil {
		return nil, err
	}
	work := make(map[string]checkpointWorkEntry, len(infos))
	for _, info := range infos {
		object, getErr := e.backend.Get(ctx, info.Key)
		if getErr != nil {
			return nil, getErr
		}
		var envelope storageformat.Envelope
		var progress storageformat.CheckpointWork
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, checkpointWorkSchema, &envelope, &progress); err != nil {
			return nil, err
		}
		integrity := objectstore.ExpectedIntegrity{Size: progress.Object.Size, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: progress.CRC32C}}
		if progress.SchemaVersion != 1 || progress.CheckpointID != checkpointID || progress.GateEpoch != gateEpoch || progress.Object.Key == "" || progress.Object.Size < 0 || progress.Object.SHA256 == "" || integrity.Validate() != nil || storageformat.CheckpointWorkKey(checkpointID, progress.Object.Key) != info.Key {
			return nil, domain.NewError(domain.ErrorPreconditionFailed, "invalid checkpoint work record")
		}
		proof, proofErr := e.checkpointWorkProof(progress)
		if proofErr != nil || !hmac.Equal([]byte(progress.Proof), []byte(proof)) {
			return nil, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint work authentication failed")
		}
		if _, duplicate := work[progress.Object.Key]; duplicate {
			return nil, domain.NewError(domain.ErrorPreconditionFailed, "duplicate checkpoint work record")
		}
		work[progress.Object.Key] = checkpointWorkEntry{fileData: progress.FileData, object: progress.Object, crc32c: progress.CRC32C}
	}
	return work, nil
}

func (e *Engine) authoritativeInventoryFromWork(ctx context.Context, backend objectstore.Backend, fileData bool, checkpointID string, gateEpoch uint64, work map[string]checkpointWorkEntry) ([]storageformat.CheckpointObject, map[string]struct{}, error) {
	infos, err := listAllFrom(ctx, backend, "endlessfs/v1/")
	if err != nil {
		return nil, nil, err
	}
	objects := make([]storageformat.CheckpointObject, 0, len(infos))
	seen := make(map[string]struct{}, len(infos))
	totalObjects := 0
	var totalBytes int64
	for _, info := range infos {
		key := info.Key.String()
		if transientOrCheckpoint(key) {
			continue
		}
		if !e.separateFileBackend || fileData == isFileDataKey(key) {
			if info.Size < 0 || totalBytes > math.MaxInt64-info.Size {
				return nil, nil, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint inventory size is invalid")
			}
			totalObjects++
			totalBytes += info.Size
		}
	}
	completedObjects := 0
	resumedObjects := 0
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
				CompletedObjects: completedObjects, TotalObjects: totalObjects,
				CompletedBytes: completedBytes, TotalBytes: totalBytes, ResumedObjects: resumedObjects,
			})
		}
	}
	for _, info := range infos {
		key := info.Key.String()
		if transientOrCheckpoint(key) {
			continue
		}
		if e.separateFileBackend && fileData != isFileDataKey(key) {
			return nil, nil, domain.NewError(domain.ErrorPreconditionFailed, "canonical object is stored in the wrong backend")
		}
		seen[key] = struct{}{}
		if progress, found := work[key]; found {
			if progress.fileData != fileData || progress.object.Size != info.Size {
				return nil, nil, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint work does not match authoritative object")
			}
			if _, verifyErr := backend.Verify(ctx, info.Key, objectstore.ExpectedIntegrity{Size: progress.object.Size, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: progress.crc32c}}); verifyErr != nil {
				return nil, nil, verifyErr
			}
			objects = append(objects, progress.object)
			completedObjects++
			resumedObjects++
			completedBytes += progress.object.Size
			reportProgress()
			continue
		}
		object, getErr := backend.Get(ctx, info.Key)
		if getErr != nil {
			return nil, nil, getErr
		}
		if int64(len(object.Body)) != info.Size {
			return nil, nil, domain.NewError(domain.ErrorPreconditionFailed, "authoritative object size changed during checkpoint")
		}
		entry := storageformat.CheckpointObject{Key: key, Size: int64(len(object.Body)), SHA256: storageformat.Digest(object.Body)}
		integrity := objectstore.IntegrityFor(object.Body)
		progress := storageformat.CheckpointWork{SchemaVersion: 1, CheckpointID: checkpointID, GateEpoch: gateEpoch, FileData: fileData, Object: entry, CRC32C: integrity.Checksum.Value}
		proof, proofErr := e.checkpointWorkProof(progress)
		if proofErr != nil {
			return nil, nil, proofErr
		}
		progress.Proof = proof
		progressKey := storageformat.CheckpointWorkKey(checkpointID, key)
		body, encodeErr := storageformat.EncodeEnvelope(checkpointWorkSchema, progressKey, 1, progress)
		if encodeErr != nil {
			return nil, nil, encodeErr
		}
		if _, putErr := e.backend.Put(ctx, progressKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); putErr != nil {
			if !errors.Is(putErr, domain.ErrConflict) {
				return nil, nil, putErr
			}
			existing, readErr := e.backend.Get(ctx, progressKey)
			if readErr != nil || !reflect.DeepEqual(existing.Body, body) {
				return nil, nil, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint work record conflict")
			}
		}
		objects = append(objects, entry)
		completedObjects++
		completedBytes += entry.Size
		reportProgress()
	}
	if totalObjects == 0 {
		reportProgress()
	}
	return objects, seen, nil
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
		object, getErr := backend.Get(ctx, info.Key)
		if getErr != nil {
			return nil, getErr
		}
		objects = append(objects, storageformat.CheckpointObject{Key: key, Size: int64(len(object.Body)), SHA256: storageformat.Digest(object.Body)})
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
