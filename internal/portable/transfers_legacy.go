package portable

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// This file is a closed-gate compatibility boundary for schema-007 upload
// leases. Ordinary schema-008 transfer requests never call these helpers.
// They remain only so an upgrade can finish or safely drain predecessor work.
func (s *FileStore) recoverUploadLease(ctx context.Context, operationKey objectstore.Key) error {
	object, err := s.engine.backend.Get(ctx, operationKey)
	if err != nil {
		return err
	}
	var envelope storageformat.Envelope
	var record storageformat.UploadRecord
	if err := storageformat.DecodeEnvelope(object.Body, operationKey, uploadRecordSchema, &envelope, &record); err != nil {
		return err
	}
	if (record.SchemaVersion != 1 && record.SchemaVersion != 2) || record.State != storageformat.UploadActive || record.UploadID == "" || record.StagingKey == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid recoverable upload")
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return err
	}
	leaseKey := storageformat.LeaseKey(transfers.BackendKind(), record.UploadID)
	if record.BackendKind != transfers.BackendKind() || record.LeaseKey != leaseKey.String() {
		return domain.NewError(domain.ErrorPreconditionFailed, "recoverable upload backend does not match")
	}
	if _, err := s.engine.backend.Get(ctx, leaseKey); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if !s.engine.clock.Now().Before(record.ExpiresAt) {
		return nil
	}
	stagingKey, err := objectstore.ParseKey(record.StagingKey)
	if err != nil {
		return err
	}
	handle, err := transfers.BeginUpload(ctx, objectstore.UploadRequest{
		UploadID: record.UploadID, Key: stagingKey, Size: record.Size, MediaType: record.MediaType,
		Resumable: record.Resumable, ExpiresAt: record.ExpiresAt,
	})
	if err != nil {
		return err
	}
	body, err := storageformat.EncodeEnvelope(transferLeaseSchema, leaseKey, 1, storageformat.TransferLease{
		SchemaVersion: 1, BackendKind: transfers.BackendKind(), UploadID: record.UploadID,
		Ciphertext: append([]byte(nil), handle.Lease...), ExpiresAt: record.ExpiresAt,
	})
	if err != nil {
		return err
	}
	if _, err := s.engine.backend.Put(ctx, leaseKey, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return err
	}
	return nil
}

func (s *FileStore) readTransferLease(ctx context.Context, record storageformat.UploadRecord) (storageformat.TransferLease, objectstore.Object, error) {
	transfers, err := s.transferBackend()
	if err != nil {
		return storageformat.TransferLease{}, objectstore.Object{}, err
	}
	expected := storageformat.LeaseKey(transfers.BackendKind(), record.UploadID)
	if record.BackendKind != transfers.BackendKind() || record.LeaseKey != expected.String() {
		return storageformat.TransferLease{}, objectstore.Object{}, domain.NewError(domain.ErrorPreconditionFailed, "upload lease backend does not match")
	}
	object, err := s.engine.backend.Get(ctx, expected)
	if err != nil {
		return storageformat.TransferLease{}, objectstore.Object{}, err
	}
	var envelope storageformat.Envelope
	var lease storageformat.TransferLease
	if err := storageformat.DecodeEnvelope(object.Body, expected, transferLeaseSchema, &envelope, &lease); err != nil {
		return storageformat.TransferLease{}, objectstore.Object{}, err
	}
	if lease.SchemaVersion != 1 || lease.BackendKind != record.BackendKind || lease.UploadID != record.UploadID || lease.ExpiresAt != record.ExpiresAt || len(lease.Ciphertext) == 0 {
		return storageformat.TransferLease{}, objectstore.Object{}, domain.NewError(domain.ErrorInvalid, "invalid stored transfer lease")
	}
	return lease, object, nil
}
