package portable

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type portableUploadTransactionItem struct {
	uploadID string
	crc32c   string
	record   storageformat.PortableUploadRecord
	value    consistencyDomainValue
	md5      string
}

func normalizePortableUploadCompletionBatch(owner domain.UserID, request domain.CompleteUploadBatchRequest) (string, string, error) {
	if !owner.Valid() || len(request.Items) < 1 || len(request.Items) > storageformat.MaxPortableUploadBatchItems || validatePortableIdempotencyKey(request.IdempotencyKey) != nil {
		return "", "", domain.NewError(domain.ErrorInvalid, "invalid upload completion batch")
	}
	seen := make(map[domain.UploadID]struct{}, len(request.Items))
	var intent strings.Builder
	intent.WriteString("endlessfs-upload-completion-batch-v1\x00")
	intent.WriteString(owner.String())
	intent.WriteByte(0)
	for _, item := range request.Items {
		if item.UploadID == "" {
			return "", "", domain.NewError(domain.ErrorInvalid, "upload completion ID is required")
		}
		if _, duplicate := seen[item.UploadID]; duplicate {
			return "", "", domain.NewError(domain.ErrorInvalid, "upload completion batch repeats an upload")
		}
		seen[item.UploadID] = struct{}{}
		if _, ok := integrity.ParseCRC32C(item.CRC32C); !ok {
			return "", "", domain.NewError(domain.ErrorInvalid, "upload completion CRC32C is invalid")
		}
		intent.WriteString(storageformat.Digest([]byte(string(item.UploadID) + "\x00" + item.CRC32C)))
		intent.WriteByte(0)
	}
	fingerprint := storageformat.Digest([]byte(intent.String()))
	transactionID := storageformat.Digest([]byte("endlessfs-upload-completion-transaction-v1\x00" + owner.String() + "\x00" + request.IdempotencyKey))
	return transactionID, fingerprint, nil
}

func (s *FileStore) readUploadTransactionProgress(ctx context.Context, owner domain.UserID, transactionID, fingerprint, kind string) (storageformat.UploadTransactionSegment, objectstore.Object, error) {
	transfers, err := s.transferBackend()
	if err != nil {
		return storageformat.UploadTransactionSegment{}, objectstore.Object{}, err
	}
	key := storageformat.UploadTransactionProgressKey(transfers.BackendKind(), transactionID)
	traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "upload-" + kind + "-batch", Subsystem: "transaction-progress"})
	object, err := s.engine.backend.Get(traced, key)
	if errors.Is(err, domain.ErrNotFound) {
		return storageformat.UploadTransactionSegment{}, objectstore.Object{Key: key}, err
	}
	if err != nil {
		return storageformat.UploadTransactionSegment{}, objectstore.Object{}, err
	}
	var progress storageformat.UploadTransactionSegment
	for segment := uint64(1); segment <= storageformat.MaxPortableUploadBatchItems/storageformat.UploadTransactionSegmentItems; segment++ {
		progress, err = storageformat.DecodeUploadTransactionSegment(object.Body, transfers.BackendKind(), owner.String(), transactionID, fingerprint, kind, segment)
		if err == nil {
			return progress, object, nil
		}
	}
	return storageformat.UploadTransactionSegment{}, objectstore.Object{}, err
}

func (s *FileStore) absentUploadTransactionProgress(transactionID string) (objectstore.Object, error) {
	transfers, err := s.transferBackend()
	if err != nil {
		return objectstore.Object{}, err
	}
	return objectstore.Object{Key: storageformat.UploadTransactionProgressKey(transfers.BackendKind(), transactionID)}, nil
}

func (s *FileStore) publishUploadTransactionProgress(ctx context.Context, owner domain.UserID, transactionID, fingerprint, kind string, progress storageformat.UploadTransactionSegment, previous objectstore.Object) (storageformat.UploadTransactionSegment, objectstore.Object, error) {
	body, err := storageformat.EncodeUploadTransactionSegment(progress)
	if err != nil {
		return storageformat.UploadTransactionSegment{}, objectstore.Object{}, err
	}
	condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
	if previous.Version != "" {
		condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: previous.Version}
	}
	traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "upload-" + kind + "-batch", Subsystem: "transaction-progress"})
	version, putErr := s.engine.backend.Put(traced, previous.Key, body, condition)
	if putErr == nil {
		return progress, objectstore.Object{Key: previous.Key, Body: body, Version: version}, nil
	}
	winner, winnerObject, getErr := s.readUploadTransactionProgress(ctx, owner, transactionID, fingerprint, kind)
	if getErr != nil {
		if errors.Is(getErr, domain.ErrNotFound) {
			return storageformat.UploadTransactionSegment{}, objectstore.Object{}, putErr
		}
		return storageformat.UploadTransactionSegment{}, objectstore.Object{}, getErr
	}
	if len(winner.Items) < len(progress.Items) || !uploadProgressPrefixEqual(winner.Items, progress.Items) {
		return storageformat.UploadTransactionSegment{}, objectstore.Object{}, domain.NewError(domain.ErrorConflict, "upload transaction progress diverged")
	}
	return winner, winnerObject, nil
}

func uploadProgressPrefixEqual(left, right []storageformat.UploadTransactionSegmentItem) bool {
	if len(left) < len(right) {
		return false
	}
	for index := range right {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateCompletionProgress(progress storageformat.UploadTransactionSegment, request domain.CompleteUploadBatchRequest) error {
	if len(progress.Items) > len(request.Items) || len(progress.Items)%storageformat.UploadTransactionSegmentItems != 0 || len(progress.Items) == len(request.Items) {
		return domain.NewError(domain.ErrorInvalid, "invalid upload completion progress boundary")
	}
	for index, item := range progress.Items {
		if item.Index != uint64(index) || item.UploadID != string(request.Items[index].UploadID) || item.CRC32C != request.Items[index].CRC32C {
			return domain.NewError(domain.ErrorInvalid, "upload completion progress does not match request")
		}
	}
	return nil
}

func (s *FileStore) verifyPortableUploadRange(ctx context.Context, items []portableUploadTransactionItem, first, last int) error {
	errorsByIndex := make([]error, last-first)
	jobs := make(chan int)
	workers := uploadBatchProviderConcurrency
	if workers > last-first {
		workers = last - first
	}
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			for index := range jobs {
				item := &items[index]
				traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "complete-upload-batch", Subsystem: "object-verification", ParallelGroup: "upload-verification-batch"})
				info, err := s.engine.fileBackend.Verify(traced, storageformat.BlobKey(item.record.OwnerID, item.record.BlobID), objectstore.ExpectedIntegrity{Size: item.record.Size, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: item.crc32c}})
				if err == nil && (info.Size != item.record.Size || !info.Fingerprint.Complete() || info.Fingerprint.CRC32C != item.crc32c) {
					err = domain.NewError(domain.ErrorPreconditionFailed, "uploaded object has invalid provider integrity metadata")
				}
				if err == nil {
					item.md5 = info.Fingerprint.MD5
				}
				errorsByIndex[index-first] = err
			}
		}()
	}
	for index := first; index < last; index++ {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	for _, err := range errorsByIndex {
		if err != nil {
			return err
		}
	}
	return nil
}

func completionEntriesFromRecords(items []portableUploadTransactionItem) ([]domain.Entry, error) {
	entries := make([]domain.Entry, len(items))
	for index, item := range items {
		if item.record.Completion == nil {
			return nil, domain.NewError(domain.ErrorInvalid, "completed upload is missing its portable result")
		}
		path, err := domain.ParseUserPath(item.record.ResolvedPath)
		if err != nil || path.IsRoot() {
			return nil, domain.NewError(domain.ErrorInvalid, "completed upload path is invalid")
		}
		entry := storageformat.DirectoryEntry{
			Name: path.Name(), NameDigest: storageformat.NameDigest(path.Name()), Kind: domain.EntryFile,
			BlobID: item.record.BlobID, Size: item.record.Size, MediaType: item.record.MediaType,
			MD5: item.record.Completion.MD5, CRC32C: item.record.Completion.CRC32C, ModifiedAt: item.record.Completion.ModifiedAt,
		}
		entry.LogicalVersion, err = directoryEntryVersion(entry)
		if err != nil {
			return nil, err
		}
		entries[index] = domainEntry(path, entry)
	}
	return entries, nil
}

func (s *FileStore) completeUploadBatch011(ctx context.Context, scope domain.Scope, request domain.CompleteUploadBatchRequest) (domain.CompleteUploadBatchResult, error) {
	transactionID, fingerprint, err := normalizePortableUploadCompletionBatch(scope.UserID(), request)
	if err != nil {
		return domain.CompleteUploadBatchResult{}, err
	}
	store := newNamespaceStore(s.engine)
	items := make([]portableUploadTransactionItem, len(request.Items))
	load := func() (*namespaceView, bool, error) {
		view, loadErr := store.loadView(ctx, scope.UserID(), "")
		if loadErr != nil {
			return nil, false, loadErr
		}
		if replay, replayErr := store.operationReplay(ctx, view, transactionID, fingerprint); replayErr != nil {
			return nil, false, replayErr
		} else if replay != nil {
			if replay.UploadBatch == nil || replay.UploadBatch.TransactionID != transactionID || replay.UploadBatch.ItemCount != uint64(len(items)) || replay.UploadBatch.State != "completed" {
				return nil, false, domain.NewError(domain.ErrorInvalid, "invalid upload completion batch outcome")
			}
			for index, requested := range request.Items {
				record, value, recordErr := s.portableUploadAtView(ctx, view, scope.UserID(), string(requested.UploadID))
				if recordErr != nil {
					return nil, false, recordErr
				}
				if record.Completion == nil {
					return nil, false, domain.NewError(domain.ErrorInvalid, "completed upload is missing its portable result")
				}
				items[index] = portableUploadTransactionItem{uploadID: string(requested.UploadID), crc32c: requested.CRC32C, record: record, value: value, md5: record.Completion.MD5}
			}
			return view, true, nil
		}
		for index, requested := range request.Items {
			record, value, recordErr := s.portableUploadAtView(ctx, view, scope.UserID(), string(requested.UploadID))
			if recordErr != nil {
				return nil, false, recordErr
			}
			if record.Area != areaName(scope.Area()) || record.State != storageformat.UploadActive && record.State != storageformat.UploadInitializing || !s.engine.clock.Now().Before(record.ExpiresAt) {
				return nil, false, domain.NewError(domain.ErrorConflict, "upload completion contains an inactive upload")
			}
			items[index] = portableUploadTransactionItem{uploadID: string(requested.UploadID), crc32c: requested.CRC32C, record: record, value: value}
		}
		return view, false, nil
	}
	view, replayed, err := load()
	if err != nil {
		return domain.CompleteUploadBatchResult{}, err
	}
	if replayed {
		entries, resultErr := completionEntriesFromRecords(items)
		return domain.CompleteUploadBatchResult{Entries: entries}, resultErr
	}
	if err := view.bindMutation(transactionID, fingerprint); err != nil {
		return domain.CompleteUploadBatchResult{}, err
	}

	// The overwhelmingly common path has no recovery object. Avoid a guaranteed
	// not-found read: the first deterministic create-only progress publication
	// detects an interrupted predecessor, reads and validates the winner, and
	// resumes from its sealed boundary.
	progressObject, err := s.absentUploadTransactionProgress(transactionID)
	if err != nil {
		return domain.CompleteUploadBatchResult{}, err
	}
	var progress storageformat.UploadTransactionSegment
	completed := 0
	for completed < len(items) {
		last := completed + storageformat.UploadTransactionSegmentItems
		if last > len(items) {
			last = len(items)
		}
		if err := s.verifyPortableUploadRange(ctx, items, completed, last); err != nil {
			return domain.CompleteUploadBatchResult{}, err
		}
		completed = last
		if completed == len(items) {
			break
		}
		transfers, transferErr := s.transferBackend()
		if transferErr != nil {
			return domain.CompleteUploadBatchResult{}, transferErr
		}
		progress = storageformat.UploadTransactionSegment{
			SchemaVersion: 1, BackendKind: transfers.BackendKind(), OwnerID: scope.UserID().String(), TransactionID: transactionID,
			RequestFingerprint: fingerprint, Kind: "complete", Segment: uint64((completed + storageformat.UploadTransactionSegmentItems - 1) / storageformat.UploadTransactionSegmentItems),
			Items: make([]storageformat.UploadTransactionSegmentItem, completed),
		}
		for index := 0; index < completed; index++ {
			progress.Items[index] = storageformat.UploadTransactionSegmentItem{Index: uint64(index), UploadID: items[index].uploadID, MD5: items[index].md5, CRC32C: items[index].crc32c}
		}
		progress, progressObject, err = s.publishUploadTransactionProgress(ctx, scope.UserID(), transactionID, fingerprint, "complete", progress, progressObject)
		if err != nil {
			return domain.CompleteUploadBatchResult{}, err
		}
		if err := validateCompletionProgress(progress, request); err != nil {
			return domain.CompleteUploadBatchResult{}, err
		}
		if len(progress.Items) > completed {
			completed = len(progress.Items)
			for index := 0; index < completed; index++ {
				items[index].md5 = progress.Items[index].MD5
			}
		}
		if err := s.engine.step(ctx, StepUploadBatchCompletionProgress); err != nil {
			return domain.CompleteUploadBatchResult{}, err
		}
	}
	if err := s.engine.step(ctx, StepUploadBatchCompletionVerified); err != nil {
		return domain.CompleteUploadBatchResult{}, err
	}

	modifiedAt := s.engine.clock.Now().UTC()
	for {
		if view == nil {
			view, replayed, err = load()
			if err != nil {
				return domain.CompleteUploadBatchResult{}, err
			}
			if replayed {
				entries, resultErr := completionEntriesFromRecords(items)
				return domain.CompleteUploadBatchResult{Entries: entries}, resultErr
			}
		}
		if err := view.bindMutation(transactionID, fingerprint); err != nil {
			return domain.CompleteUploadBatchResult{}, err
		}
		frames := make(map[string]namespaceFrame)
		grouped := make(map[string][]namespaceDirectoryEdit)
		additional := make([]consistencyDomainChange, 0, len(items))
		seenDestinations := make(map[string]struct{}, len(items))
		for index := range items {
			item := &items[index]
			path, parseErr := domain.ParseUserPath(item.record.ResolvedPath)
			if parseErr != nil || path.IsRoot() {
				return domain.CompleteUploadBatchResult{}, domain.NewError(domain.ErrorInvalid, "stored upload path is invalid")
			}
			if _, duplicate := seenDestinations[path.String()]; duplicate {
				return domain.CompleteUploadBatchResult{}, domain.NewError(domain.ErrorInvalid, "upload completion repeats a destination")
			}
			seenDestinations[path.String()] = struct{}{}
			trail, trailErr := store.resolveTrail(ctx, view, scope.Area(), path.Parent())
			if trailErr != nil {
				return domain.CompleteUploadBatchResult{}, trailErr
			}
			for key, frame := range mergeNamespaceFrames(trail) {
				frames[key] = frame
			}
			parent := trail[len(trail)-1]
			existing, found, childErr := store.child(ctx, view, parent.entry, path.Name())
			if childErr != nil {
				return domain.CompleteUploadBatchResult{}, childErr
			}
			if item.record.TargetExisted {
				if !found || domain.Version(existing.Entry.LogicalVersion) != item.record.ExpectedVersion {
					return domain.CompleteUploadBatchResult{}, domain.NewError(domain.ErrorPreconditionFailed, "upload destination changed")
				}
			} else if found {
				return domain.CompleteUploadBatchResult{}, domain.NewError(domain.ErrorConflict, "upload destination appeared during transfer")
			}
			entry := storageformat.DirectoryEntry{
				Name: path.Name(), NameDigest: storageformat.NameDigest(path.Name()), Kind: domain.EntryFile,
				BlobID: item.record.BlobID, Size: item.record.Size, MediaType: item.record.MediaType,
				MD5: item.md5, CRC32C: item.crc32c, ModifiedAt: modifiedAt,
			}
			entry.LogicalVersion, err = directoryEntryVersion(entry)
			if err != nil {
				return domain.CompleteUploadBatchResult{}, err
			}
			placed := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: storageformat.Digest([]byte(fmt.Sprintf("endlessfs-upload-completion-file-v1\x00%s\x00%016x", transactionID, index))), Entry: entry}
			edit := namespaceDirectoryEdit{after: &placed}
			if found {
				existingCopy := existing
				edit.before = &existingCopy
			}
			grouped[parent.key] = append(grouped[parent.key], edit)
			item.record.State, item.record.CleanupPending = storageformat.UploadCompleted, false
			item.record.Completion = &storageformat.PortableUploadCompletion{MD5: item.md5, CRC32C: item.crc32c, ModifiedAt: modifiedAt}
			body, encodeErr := storageformat.EncodeCanonical(item.record)
			if encodeErr != nil {
				return domain.CompleteUploadBatchResult{}, encodeErr
			}
			additional = append(additional, consistencyDomainChange{Key: uploadRecordKey(item.uploadID), Require: domainValuePresent, ExpectedVersion: item.value.LogicalVersion, Value: body})
		}
		changes := make(map[string]storageformat.NamespaceEntry, len(grouped))
		keys := make([]string, 0, len(grouped))
		for key := range grouped {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			updated, applyErr := store.applyDirectoryEdits(ctx, view, frames[key].entry, grouped[key], modifiedAt)
			if applyErr != nil {
				return domain.CompleteUploadBatchResult{}, applyErr
			}
			changes[key] = updated
		}
		if err := store.propagate(ctx, view, frames, changes, modifiedAt); err != nil {
			return domain.CompleteUploadBatchResult{}, err
		}
		_, err = store.commitMaterializedWithAdditionalChanges(ctx, view, transactionID, fingerprint, changes, additional, storageformat.NamespaceMutationResult{UploadBatch: &storageformat.NamespaceUploadBatchResult{TransactionID: transactionID, ItemCount: uint64(len(items)), State: "completed"}})
		if err == nil {
			if err := s.engine.step(ctx, StepUploadBatchCompletionPublished); err != nil {
				return domain.CompleteUploadBatchResult{}, err
			}
			entries, resultErr := completionEntriesFromRecords(items)
			return domain.CompleteUploadBatchResult{Entries: entries}, resultErr
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return domain.CompleteUploadBatchResult{}, err
		}
		view = nil
	}
}

func normalizePortableUploadAbortBatch(owner domain.UserID, request domain.AbortUploadBatchRequest) (string, string, error) {
	if !owner.Valid() || len(request.UploadIDs) < 1 || len(request.UploadIDs) > storageformat.MaxPortableUploadBatchItems || validatePortableIdempotencyKey(request.IdempotencyKey) != nil || request.BatchID != "" && !storageformat.ValidDigest(request.BatchID) {
		return "", "", domain.NewError(domain.ErrorInvalid, "invalid upload cancellation batch")
	}
	seen := make(map[domain.UploadID]struct{}, len(request.UploadIDs))
	var intent strings.Builder
	intent.WriteString("endlessfs-upload-abort-batch-v1\x00")
	intent.WriteString(owner.String())
	intent.WriteByte(0)
	intent.WriteString(request.BatchID)
	intent.WriteByte(0)
	for _, uploadID := range request.UploadIDs {
		if uploadID == "" {
			return "", "", domain.NewError(domain.ErrorInvalid, "upload cancellation ID is required")
		}
		if _, duplicate := seen[uploadID]; duplicate {
			return "", "", domain.NewError(domain.ErrorInvalid, "upload cancellation batch repeats an upload")
		}
		seen[uploadID] = struct{}{}
		intent.WriteString(storageformat.Digest([]byte(uploadID)))
		intent.WriteByte(0)
	}
	fingerprint := storageformat.Digest([]byte(intent.String()))
	transactionBinding := request.IdempotencyKey
	if request.BatchID != "" {
		// One canonical cancellation transaction per admitted batch makes the
		// compact overlay safely replayable without retaining 10,000 outcomes.
		transactionBinding = "batch:" + request.BatchID
	}
	transactionID := storageformat.Digest([]byte("endlessfs-upload-abort-transaction-v1\x00" + owner.String() + "\x00" + transactionBinding))
	return transactionID, fingerprint, nil
}

func validateAbortProgress(progress storageformat.UploadTransactionSegment, request domain.AbortUploadBatchRequest) error {
	if len(progress.Items) > len(request.UploadIDs) || len(progress.Items)%storageformat.UploadTransactionSegmentItems != 0 || len(progress.Items) == len(request.UploadIDs) {
		return domain.NewError(domain.ErrorInvalid, "invalid upload cancellation progress boundary")
	}
	for index, item := range progress.Items {
		if item.Index != uint64(index) || item.UploadID != string(request.UploadIDs[index]) || item.MD5 != "" || item.CRC32C != "" {
			return domain.NewError(domain.ErrorInvalid, "upload cancellation progress does not match request")
		}
	}
	return nil
}

func (s *FileStore) runtimeUploadLeasesForRange(ctx context.Context, items []portableUploadTransactionItem, first, last int) ([][]byte, error) {
	transfers, err := s.transferBackend()
	if err != nil {
		return nil, err
	}
	leases := make([][]byte, last-first)
	type segmentBinding struct {
		batchID string
		segment uint64
		count   uint64
	}
	segments := make(map[segmentBinding]storageformat.PortableUploadLeaseSegment)
	for index := first; index < last; index++ {
		record := items[index].record
		if record.Batch == nil {
			lease, _, leaseErr := s.runtimeUploadLease(ctx, record.UploadID)
			if leaseErr != nil {
				return nil, leaseErr
			}
			leases[index-first] = lease
			continue
		}
		binding := segmentBinding{batchID: record.Batch.BatchID, segment: (record.Batch.Count - 1) / storageformat.MaxUploadLeaseSegmentItems, count: record.Batch.Count}
		segment, found := segments[binding]
		if !found {
			key := storageformat.UploadLeaseSegmentKey(transfers.BackendKind(), binding.batchID, binding.segment)
			object, getErr := s.engine.backend.Get(ctx, key)
			if errors.Is(getErr, domain.ErrNotFound) && binding.segment != record.Batch.Index/storageformat.MaxUploadLeaseSegmentItems {
				binding.segment = record.Batch.Index / storageformat.MaxUploadLeaseSegmentItems
				key = storageformat.UploadLeaseSegmentKey(transfers.BackendKind(), binding.batchID, binding.segment)
				object, getErr = s.engine.backend.Get(ctx, key)
			}
			if getErr != nil {
				return nil, getErr
			}
			segment, getErr = storageformat.DecodePortableUploadLeaseSegment(object.Body, transfers.BackendKind(), record.OwnerID, binding.batchID, binding.segment)
			if getErr != nil {
				return nil, getErr
			}
			if segment.TotalCount != record.Batch.Count {
				return nil, domain.NewError(domain.ErrorInvalid, "upload cancellation lease total is misbound")
			}
			segments[binding] = segment
		}
		if record.Batch.Index < segment.FirstIndex {
			return nil, domain.NewError(domain.ErrorInvalid, "upload cancellation lease is missing or misbound")
		}
		offset := record.Batch.Index - segment.FirstIndex
		if offset >= uint64(len(segment.Leases)) || segment.Leases[offset].Index != record.Batch.Index || segment.Leases[offset].UploadID != record.UploadID {
			return nil, domain.NewError(domain.ErrorInvalid, "upload cancellation lease is missing or misbound")
		}
		leases[index-first] = append([]byte(nil), segment.Leases[offset].Lease...)
	}
	return leases, nil
}

func (s *FileStore) abortPortableUploadRange(ctx context.Context, leases [][]byte, first, last int) error {
	transfers, err := s.transferBackend()
	if err != nil {
		return err
	}
	errorsByIndex := make([]error, last-first)
	jobs := make(chan int)
	workers := uploadBatchProviderConcurrency
	if workers > last-first {
		workers = last - first
	}
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			for offset := range jobs {
				traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "abort-upload-batch", Subsystem: "upload-session", ParallelGroup: "upload-abort-batch"})
				err := transfers.AbortUpload(traced, leases[first+offset])
				if errors.Is(err, domain.ErrNotFound) {
					err = nil
				}
				errorsByIndex[offset] = err
			}
		}()
	}
	for offset := 0; offset < last-first; offset++ {
		jobs <- offset
	}
	close(jobs)
	wait.Wait()
	for _, err := range errorsByIndex {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *FileStore) abortUploadBatch011(ctx context.Context, scope domain.Scope, request domain.AbortUploadBatchRequest) error {
	transactionID, fingerprint, err := normalizePortableUploadAbortBatch(scope.UserID(), request)
	if err != nil {
		return err
	}
	store := newNamespaceStore(s.engine)
	items := make([]portableUploadTransactionItem, len(request.UploadIDs))
	load := func() (*namespaceView, bool, error) {
		view, loadErr := store.loadView(ctx, scope.UserID(), "")
		if loadErr != nil {
			return nil, false, loadErr
		}
		if replay, replayErr := store.operationReplay(ctx, view, transactionID, fingerprint); replayErr != nil {
			return nil, false, replayErr
		} else if replay != nil {
			if replay.UploadBatch == nil || replay.UploadBatch.TransactionID != transactionID || replay.UploadBatch.ItemCount != uint64(len(items)) || replay.UploadBatch.State != "aborted" {
				return nil, false, domain.NewError(domain.ErrorInvalid, "invalid upload cancellation batch outcome")
			}
			return view, true, nil
		}
		for index, uploadID := range request.UploadIDs {
			record, value, recordErr := s.portableUploadAtView(ctx, view, scope.UserID(), string(uploadID))
			if recordErr != nil {
				return nil, false, recordErr
			}
			if record.Area != areaName(scope.Area()) || record.State == storageformat.UploadCompleted || record.State == storageformat.UploadAborted {
				return nil, false, domain.NewError(domain.ErrorConflict, "upload cancellation contains a terminal upload")
			}
			if request.BatchID != "" && (record.Batch == nil || record.Batch.BatchID != request.BatchID || record.Batch.Count != uint64(len(request.UploadIDs))) {
				return nil, false, domain.NewError(domain.ErrorConflict, "upload cancellation does not match one complete admitted batch")
			}
			items[index] = portableUploadTransactionItem{uploadID: string(uploadID), record: record, value: value}
		}
		return view, false, nil
	}
	view, replayed, err := load()
	if err != nil || replayed {
		return err
	}
	// As in completion, do not charge every fresh cancellation for a speculative
	// recovery read. A create-only progress publication recovers an interrupted
	// winner on conflict and verifies the exact request binding before resuming.
	progressObject, err := s.absentUploadTransactionProgress(transactionID)
	if err != nil {
		return err
	}
	var progress storageformat.UploadTransactionSegment
	completed := 0
	leases, err := s.runtimeUploadLeasesForRange(ctx, items, 0, len(items))
	if err != nil {
		return err
	}
	for completed < len(items) {
		last := completed + storageformat.UploadTransactionSegmentItems
		if last > len(items) {
			last = len(items)
		}
		if err := s.abortPortableUploadRange(ctx, leases, completed, last); err != nil {
			return err
		}
		completed = last
		if completed == len(items) {
			break
		}
		transfers, transferErr := s.transferBackend()
		if transferErr != nil {
			return transferErr
		}
		progress = storageformat.UploadTransactionSegment{
			SchemaVersion: 1, BackendKind: transfers.BackendKind(), OwnerID: scope.UserID().String(), TransactionID: transactionID,
			RequestFingerprint: fingerprint, Kind: "abort", Segment: uint64((completed + storageformat.UploadTransactionSegmentItems - 1) / storageformat.UploadTransactionSegmentItems),
			Items: make([]storageformat.UploadTransactionSegmentItem, completed),
		}
		for index := 0; index < completed; index++ {
			progress.Items[index] = storageformat.UploadTransactionSegmentItem{Index: uint64(index), UploadID: items[index].uploadID}
		}
		progress, progressObject, err = s.publishUploadTransactionProgress(ctx, scope.UserID(), transactionID, fingerprint, "abort", progress, progressObject)
		if err != nil {
			return err
		}
		if err := validateAbortProgress(progress, request); err != nil {
			return err
		}
		if len(progress.Items) > completed {
			completed = len(progress.Items)
		}
		if err := s.engine.step(ctx, StepUploadBatchAbortProgress); err != nil {
			return err
		}
	}
	if err := s.engine.step(ctx, StepUploadBatchAbortApplied); err != nil {
		return err
	}
	for {
		if view == nil {
			view, replayed, err = load()
			if err != nil || replayed {
				return err
			}
		}
		if err := view.bindMutation(transactionID, fingerprint); err != nil {
			return err
		}
		if request.BatchID != "" {
			cached := view.uploadAborts[request.BatchID]
			bitmap := make([]byte, (len(items)+7)/8)
			seenMembers := make([]bool, len(items))
			if cached.found {
				if cached.record.Count != uint64(len(items)) {
					return domain.NewError(domain.ErrorInvalid, "upload cancellation overlay count is misbound")
				}
				copy(bitmap, cached.record.Aborted)
			}
			for index := range items {
				member := items[index].record.Batch
				if member == nil || member.BatchID != request.BatchID || member.Count != uint64(len(items)) || member.Index >= uint64(len(items)) {
					return domain.NewError(domain.ErrorInvalid, "upload cancellation member is misbound")
				}
				if seenMembers[member.Index] {
					return domain.NewError(domain.ErrorInvalid, "upload cancellation repeats a batch member")
				}
				seenMembers[member.Index] = true
				bitmap[member.Index/8] |= 1 << (member.Index % 8)
			}
			for _, found := range seenMembers {
				if !found {
					return domain.NewError(domain.ErrorInvalid, "upload cancellation omits a batch member")
				}
			}
			overlay := storageformat.PortableUploadBatchAbort{
				SchemaVersion: 1, OwnerID: scope.UserID().String(), BatchID: request.BatchID,
				Count: uint64(len(items)), Aborted: bitmap, ModifiedAt: s.engine.clock.Now().UTC(),
			}
			body, encodeErr := storageformat.EncodeCanonical(overlay)
			if encodeErr != nil || storageformat.ValidatePortableUploadBatchAbort(overlay) != nil {
				if encodeErr != nil {
					return encodeErr
				}
				return domain.NewError(domain.ErrorInvalid, "invalid upload cancellation overlay")
			}
			requirement := domainValueAbsent
			expectedVersion := ""
			if cached.found {
				requirement, expectedVersion = domainValuePresent, cached.value.LogicalVersion
			}
			resultBody, encodeErr := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{
				SchemaVersion: 1, RequestFingerprint: fingerprint,
				UploadBatch: &storageformat.NamespaceUploadBatchResult{TransactionID: transactionID, ItemCount: uint64(len(items)), State: "aborted"},
			})
			if encodeErr != nil {
				return encodeErr
			}
			_, err = s.engine.stateDomainStore().mutatePrepared(ctx, uploadDomainReference(scope.UserID()), consistencyDomainMutation{
				ID:      transactionID,
				Changes: []consistencyDomainChange{{Key: uploadBatchAbortKey(request.BatchID), Require: requirement, ExpectedVersion: expectedVersion, Value: body}},
				Result:  resultBody,
			}, view.headSnapshot, view.session)
			if err == nil {
				return s.engine.step(ctx, StepUploadBatchAbortPublished)
			}
			if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
				return err
			}
			view = nil
			continue
		}

		changes := make([]consistencyDomainChange, 0, len(items))
		for index := range items {
			item := &items[index]
			item.record.State, item.record.CleanupPending, item.record.Completion = storageformat.UploadAborted, false, nil
			body, encodeErr := storageformat.EncodeCanonical(item.record)
			if encodeErr != nil {
				return encodeErr
			}
			changes = append(changes, consistencyDomainChange{Key: uploadRecordKey(item.uploadID), Require: domainValuePresent, ExpectedVersion: item.value.LogicalVersion, Value: body})
		}
		resultBody, encodeErr := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{
			SchemaVersion: 1, RequestFingerprint: fingerprint,
			UploadBatch: &storageformat.NamespaceUploadBatchResult{TransactionID: transactionID, ItemCount: uint64(len(items)), State: "aborted"},
		})
		if encodeErr != nil {
			return encodeErr
		}
		_, err = s.engine.stateDomainStore().mutateMaterializedPrepared(ctx, uploadDomainReference(scope.UserID()), consistencyDomainMutation{ID: transactionID, Changes: changes, Result: resultBody}, view.headSnapshot, view.session)
		if err == nil {
			return s.engine.step(ctx, StepUploadBatchAbortPublished)
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		view = nil
	}
}
