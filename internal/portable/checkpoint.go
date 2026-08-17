package portable

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const checkpointSchema = "checkpoint-v1"

// VerifyCheckpointReadOnly validates an existing canonical bucket and closed
// checkpoint without initializing, repairing, or otherwise writing storage.
func VerifyCheckpointReadOnly(ctx context.Context, backend objectstore.Backend, writerConfiguration WriterConfiguration, checkpointID string) error {
	if backend == nil || checkpointID == "" {
		return domain.NewError(domain.ErrorInvalid, "backend and checkpoint ID are required")
	}
	writer, err := canonicalWriterConfiguration(writerConfiguration)
	if err != nil {
		return err
	}
	engine := &Engine{backend: backend, writer: writer}
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
	objects, err := e.authoritativeInventory(ctx)
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
		if err := e.VerifyCheckpoint(ctx, checkpointID); err != nil {
			return storageformat.Checkpoint{}, err
		}
		return e.readCheckpoint(ctx, checkpointID)
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
	if gate.Mode != storageformat.GateClosed || gate.Epoch != checkpoint.GateEpoch || gate.CheckpointID != checkpointID || checkpoint.WriterSetID != e.writer.WriterSetID {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint does not match closed write gate")
	}
	objects, err := e.authoritativeInventory(ctx)
	if err != nil {
		return err
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
	infos, err := e.listAll(ctx, "endlessfs/v1/")
	if err != nil {
		return nil, err
	}
	objects := make([]storageformat.CheckpointObject, 0, len(infos))
	for _, info := range infos {
		key := info.Key.String()
		if transientOrCheckpoint(key) {
			continue
		}
		object, getErr := e.backend.Get(ctx, info.Key)
		if getErr != nil {
			return nil, getErr
		}
		objects = append(objects, storageformat.CheckpointObject{Key: key, Size: int64(len(object.Body)), SHA256: storageformat.Digest(object.Body)})
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
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
