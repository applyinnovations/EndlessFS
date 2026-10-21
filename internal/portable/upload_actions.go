package portable

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/integrity"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func actionFailure(result *domain.UploadActionResult, err error, unknown bool) {
	result.ErrorKind, result.NextAction = domain.KindOf(err), "attention"
	result.Retryable = errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrRateLimited) || errors.Is(err, domain.ErrInternal)
	result.Outcome = "rejected"
	if result.Retryable {
		result.Outcome, result.NextAction = "retryable", "retry"
	}
	if unknown {
		result.Outcome = "unknown"
	}
}

func completionDestinationAtView(ctx context.Context, store *namespaceStore, view *namespaceView, scope domain.Scope, record storageformat.PortableUploadRecord) error {
	path, err := domain.ParseUserPath(record.ResolvedPath)
	if err != nil || path.IsRoot() {
		return domain.NewError(domain.ErrorInvalid, "stored upload destination is invalid")
	}
	trail, err := store.resolveTrail(ctx, view, scope.Area(), path.Parent())
	if err != nil {
		return err
	}
	parent := trail[len(trail)-1]
	existing, found, err := store.child(ctx, view, parent.entry, path.Name())
	if err != nil {
		return err
	}
	if record.TargetExisted {
		if !found || domain.Version(existing.Entry.LogicalVersion) != record.ExpectedVersion {
			return domain.NewError(domain.ErrorPreconditionFailed, "upload destination changed")
		}
	} else if found {
		return domain.NewError(domain.ErrorConflict, "upload destination appeared during transfer")
	}
	return nil
}

// UploadActions reports independent terminal outcomes while coalescing eligible
// writes through the existing atomic publication protocol. The authenticated
// upload record is each item's natural receipt: completion binds CRC32C and
// cancellation binds UploadID. Reordering or selecting a subset cannot allocate
// another file, replay a different checksum, or repeat a published transition.
// No durable format or existing full-membership admission contract changes.
func (s *FileStore) UploadActions(ctx context.Context, scope domain.Scope, requested []domain.UploadActionItem) ([]domain.UploadActionResult, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return nil, err
	}
	if len(requested) < 1 || len(requested) > domain.MaxUploadLifecycleItems {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid upload action page size")
	}
	seen := make(map[domain.UploadID]bool, len(requested))
	results := make([]domain.UploadActionResult, len(requested))
	for index, item := range requested {
		if item.UploadID == "" || len(item.UploadID) > 256 || seen[item.UploadID] {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid or repeated upload action ID")
		}
		seen[item.UploadID] = true
		results[index] = domain.UploadActionResult{Index: index, UploadID: item.UploadID, Action: item.Action}
		if item.Action != "complete" && item.Action != "cancel" || item.Action == "cancel" && item.CRC32C != "" {
			actionFailure(&results[index], domain.NewError(domain.ErrorInvalid, "invalid upload action"), false)
		} else if item.Action == "complete" {
			if _, ok := integrity.ParseCRC32C(item.CRC32C); !ok {
				actionFailure(&results[index], domain.NewError(domain.ErrorInvalid, "invalid completion checksum"), false)
			}
		}
	}
	ctx = providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "upload-actions", Subsystem: "recovery-metadata"})
	completionProofs := make(map[string]uploadCompletionProof)
	cancellationProofs := make(map[string]uploadCancellationProof)
	// One additional shared read can isolate an item changed by another replica.
	// Persistent contention returns explicit unknown/retryable results, never an
	// unbounded split-and-retry tree or one namespace transaction per item.
	for round := 0; round < 2; round++ {
		store := newNamespaceStore(s.engine)
		view, err := store.loadView(ctx, scope.UserID(), "")
		if err != nil {
			return nil, err
		}
		records := make([]portableUploadTransactionItem, len(requested))
		complete, cancel := make([]int, 0), make([]int, 0)
		destinations := make(map[string]bool)
		order := make([]int, len(requested))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(i, j int) bool { return requested[order[i]].UploadID < requested[order[j]].UploadID })
		for _, index := range order {
			if results[index].Outcome != "" {
				continue
			}
			request, result := requested[index], &results[index]
			record, value, readErr := s.portableUploadAtView(ctx, view, scope.UserID(), string(request.UploadID))
			if readErr == nil && record.Area != areaName(scope.Area()) {
				readErr = domain.NewError(domain.ErrorNotFound, "upload does not exist")
			}
			if readErr != nil {
				actionFailure(result, readErr, false)
				continue
			}
			status, _, statusErr := lifecycleRecordStatus(record, request.UploadID, !s.engine.clock.Now().Before(record.ExpiresAt))
			if statusErr != nil {
				actionFailure(result, statusErr, false)
				continue
			}
			result.Status = &status
			if record.State == storageformat.UploadCompleted {
				if request.Action == "complete" && (record.Completion == nil || record.Completion.CRC32C != request.CRC32C) {
					actionFailure(result, domain.NewError(domain.ErrorConflict, "completed checksum differs"), false)
					continue
				}
				entries, entryErr := completionEntriesFromRecords([]portableUploadTransactionItem{{record: record}})
				if entryErr != nil {
					actionFailure(result, entryErr, false)
					continue
				}
				result.Entry, result.Outcome, result.NextAction = &entries[0], "already_applied", "done"
				if request.Action == "cancel" {
					result.Outcome = "already_completed"
				}
				continue
			}
			if record.State == storageformat.UploadAborted {
				if request.Action == "cancel" {
					result.Outcome, result.NextAction = "already_applied", "done"
				} else {
					actionFailure(result, domain.NewError(domain.ErrorConflict, "upload was cancelled"), false)
					result.NextAction = "restart"
				}
				continue
			}
			if request.Action == "complete" {
				if status.State == domain.UploadStateExpired {
					actionFailure(result, domain.NewError(domain.ErrorConflict, "upload expired"), false)
					result.NextAction = "restart"
					continue
				}
				if destinations[record.ResolvedPath] {
					actionFailure(result, domain.NewError(domain.ErrorConflict, "completion repeats a destination"), false)
					continue
				}
				if err := completionDestinationAtView(ctx, store, view, scope, record); err != nil {
					actionFailure(result, err, false)
					continue
				}
				destinations[record.ResolvedPath] = true
				complete = append(complete, index)
			} else {
				cancel = append(cancel, index)
			}
			records[index] = portableUploadTransactionItem{uploadID: record.UploadID, record: record, value: value, crc32c: request.CRC32C}
		}
		if len(complete)+len(cancel) == 0 {
			return results, ctx.Err()
		}
		transfers, err := s.transferBackend()
		if err != nil {
			return nil, err
		}
		leases := make([][]byte, len(requested))
		groups := make(map[string][]int)
		for _, index := range cancel {
			record := records[index].record
			key := record.UploadID
			if record.Batch != nil {
				key = record.Batch.BatchID
			}
			groups[key] = append(groups[key], index)
		}
		for _, indexes := range groups {
			group := make([]portableUploadTransactionItem, len(indexes))
			for offset, index := range indexes {
				group[offset] = records[index]
			}
			loaded, loadErr := s.runtimeUploadLeasesForRange(ctx, group, 0, len(group))
			for offset, index := range indexes {
				if loadErr != nil {
					actionFailure(&results[index], loadErr, false)
				} else {
					leases[index] = loaded[offset]
				}
			}
		}
		indexes := append(append([]int(nil), complete...), cancel...)
		jobs := make(chan int)
		var wait sync.WaitGroup
		for worker := 0; worker < min(uploadLifecycleProviderConcurrency, len(indexes)); worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for index := range jobs {
					result, item := &results[index], &records[index]
					if result.Outcome != "" {
						continue
					}
					select {
					case s.engine.uploadLifecycleSlots <- struct{}{}:
					case <-ctx.Done():
						continue
					}
					traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "upload-actions", Subsystem: "upload-verification", ParallelGroup: "upload-action-page"})
					if requested[index].Action == "complete" {
						info, verifyErr := s.engine.fileBackend.Verify(traced, storageformat.BlobKey(item.record.OwnerID, item.record.BlobID), objectstore.ExpectedIntegrity{Size: item.record.Size, Checksum: objectstore.Checksum{Algorithm: objectstore.ChecksumCRC32C, Value: item.crc32c}})
						if verifyErr == nil && (info.Size != item.record.Size || !info.Fingerprint.Complete() || info.Fingerprint.CRC32C != item.crc32c) {
							verifyErr = domain.NewError(domain.ErrorPreconditionFailed, "uploaded integrity metadata differs")
						}
						if verifyErr != nil {
							actionFailure(result, verifyErr, false)
						} else {
							item.md5, item.verified = info.Fingerprint.MD5, true
						}
					} else {
						abortErr := transfers.AbortUpload(traced, leases[index])
						if errors.Is(abortErr, domain.ErrNotFound) {
							abortErr = nil
						}
						if abortErr != nil {
							actionFailure(result, abortErr, true)
						}
					}
					<-s.engine.uploadLifecycleSlots
				}
			}()
		}
		for _, index := range indexes {
			select {
			case jobs <- index:
			case <-ctx.Done():
				close(jobs)
				wait.Wait()
				return nil, ctx.Err()
			}
		}
		close(jobs)
		wait.Wait()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, action := range []string{"complete", "cancel"} {
			eligible := make([]int, 0)
			var intent strings.Builder
			for _, index := range order {
				if requested[index].Action == action && results[index].Outcome == "" {
					eligible = append(eligible, index)
					intent.WriteString(string(requested[index].UploadID))
					intent.WriteByte(0)
					intent.WriteString(requested[index].CRC32C)
					intent.WriteByte(0)
				}
			}
			if len(eligible) == 0 {
				continue
			}
			key := "lifecycle-" + action + ":" + storageformat.Digest([]byte(intent.String()))
			var applyErr error
			var entries []domain.Entry
			if action == "complete" {
				items := make([]domain.CompleteUploadBatchItem, len(eligible))
				for offset, index := range eligible {
					item := records[index]
					items[offset] = domain.CompleteUploadBatchItem{UploadID: requested[index].UploadID, CRC32C: item.crc32c}
					completionProofs[item.uploadID] = uploadCompletionProof{version: item.value.LogicalVersion, crc32c: item.crc32c, md5: item.md5}
				}
				published, err := s.completeUploadActionsProven(ctx, scope, domain.CompleteUploadBatchRequest{Items: items, IdempotencyKey: key}, completionProofs)
				applyErr, entries = err, published.Entries
			} else {
				ids := make([]domain.UploadID, len(eligible))
				for offset, index := range eligible {
					item := records[index]
					ids[offset] = requested[index].UploadID
					cancellationProofs[item.uploadID] = uploadCancellationProof{version: item.value.LogicalVersion, lease: leases[index]}
				}
				applyErr = s.abortUploadActionsProven(ctx, scope, domain.AbortUploadBatchRequest{UploadIDs: ids, IdempotencyKey: key}, cancellationProofs)
			}
			for offset, index := range eligible {
				result := &results[index]
				if applyErr != nil {
					if round == 0 && (errors.Is(applyErr, domain.ErrConflict) || errors.Is(applyErr, domain.ErrPreconditionFailed)) {
						continue
					}
					actionFailure(result, applyErr, true)
					if errors.Is(applyErr, domain.ErrConflict) || errors.Is(applyErr, domain.ErrPreconditionFailed) {
						result.ErrorKind, result.Retryable, result.NextAction = domain.ErrorUnavailable, true, "retry"
					}
					continue
				}
				result.Outcome, result.NextAction = "applied", "done"
				if action == "complete" {
					result.Entry = &entries[offset]
					result.Status.State, result.Status.DataComplete, result.Status.ConfirmedOffset = domain.UploadStateCompleted, true, records[index].record.Size
				} else {
					result.Status.State = domain.UploadStateAborted
				}
			}
		}
	}
	return results, ctx.Err()
}
