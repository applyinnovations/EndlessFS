package portable

import (
	"context"
	"errors"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Reuse the qualified native-I/O pool ceiling; control recovery pages share
// this engine-wide budget rather than multiplying it for every HTTP request.
const uploadLifecycleProviderConcurrency = uploadBatchProviderConcurrency

func lifecycleFailure(item *domain.UploadLifecycleItem, err error) {
	item.ErrorKind = domain.KindOf(err)
	item.Retryable = errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrRateLimited) || errors.Is(err, domain.ErrInternal)
	item.NextAction = "attention"
	if item.Retryable {
		item.NextAction = "retry"
	}
}

// lifecycleRecordStatus classifies terminal authority without provider work.
// Completed authority takes precedence over its former session deadline.
func lifecycleRecordStatus(record storageformat.PortableUploadRecord, uploadID domain.UploadID, expired bool) (domain.UploadStatus, bool, error) {
	path, err := domain.ParseUserPath(record.RequestedPath)
	if err != nil {
		return domain.UploadStatus{}, false, domain.NewError(domain.ErrorInvalid, "stored upload path is invalid")
	}
	status := domain.UploadStatus{UploadID: uploadID, Path: path, DeclaredSize: record.Size, ExpiresAt: record.ExpiresAt, State: domain.UploadStateActive, Protocol: domain.UploadSingle}
	if record.Resumable {
		status.Protocol = domain.UploadResumable
	}
	switch record.State {
	case storageformat.UploadCompleted:
		status.State, status.ConfirmedOffset, status.DataComplete = domain.UploadStateCompleted, record.Size, true
		return status, true, nil
	case storageformat.UploadAborted:
		status.State = domain.UploadStateAborted
		return status, true, nil
	}
	if expired {
		status.State = domain.UploadStateExpired
		return status, true, nil
	}
	return status, false, nil
}

// UploadLifecycle performs bounded recovery on one authenticated namespace
// view. Per-item absence/provider failure cannot poison an independent sibling.
// It does not create attempts, renew deadlines, or change batch membership.
func (s *FileStore) UploadLifecycle(ctx context.Context, scope domain.Scope, ids []domain.UploadID, resume bool) ([]domain.UploadLifecycleItem, error) {
	ctx = providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "upload-lifecycle", Subsystem: "recovery-metadata"})
	if err := validateFileRequest(ctx, scope); err != nil {
		return nil, err
	}
	if len(ids) < 1 || len(ids) > domain.MaxUploadLifecycleItems {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid upload recovery page size")
	}
	seen := make(map[domain.UploadID]bool, len(ids))
	for _, id := range ids {
		if id == "" || len(id) > 256 || seen[id] {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid or repeated upload recovery ID")
		}
		seen[id] = true
	}
	view, err := newNamespaceStore(s.engine).loadView(ctx, scope.UserID(), "")
	if err != nil {
		return nil, err
	}
	items := make([]domain.UploadLifecycleItem, len(ids))
	records := make([]storageformat.PortableUploadRecord, len(ids))
	pending := make([]int, 0, len(ids))
	for index, id := range ids {
		item := &items[index]
		item.Index, item.UploadID = index, id
		record, _, readErr := s.portableUploadAtView(ctx, view, scope.UserID(), string(id))
		if readErr == nil && record.Area != areaName(scope.Area()) {
			readErr = domain.NewError(domain.ErrorNotFound, "upload does not exist")
		}
		if readErr != nil {
			lifecycleFailure(item, readErr)
			continue
		}
		status, terminal, statusErr := lifecycleRecordStatus(record, id, !s.engine.clock.Now().Before(record.ExpiresAt))
		if statusErr != nil {
			lifecycleFailure(item, statusErr)
			continue
		}
		item.Status = &status
		if terminal {
			switch status.State {
			case domain.UploadStateCompleted:
				item.NextAction = "done"
			case domain.UploadStateExpired, domain.UploadStateAborted:
				item.NextAction = "restart"
			}
			continue
		}
		records[index] = record
		pending = append(pending, index)
	}
	if len(pending) == 0 {
		return items, ctx.Err()
	}
	transfers, err := s.transferBackend()
	if err != nil {
		return nil, err
	}
	// Group lease reads before concurrent provider work. Each existing segmented
	// admission is decoded once, then only requested leases remain live.
	leases := make([][]byte, len(ids))
	groups := make(map[string][]int)
	order := make([]string, 0)
	for _, index := range pending {
		record := records[index]
		key := record.UploadID
		if record.Batch != nil {
			key = record.Batch.BatchID
		}
		if _, found := groups[key]; !found {
			order = append(order, key)
		}
		groups[key] = append(groups[key], index)
	}
	for _, key := range order {
		indexes := groups[key]
		group := make([]portableUploadTransactionItem, len(indexes))
		for offset, index := range indexes {
			group[offset].record = records[index]
		}
		loaded, leaseErr := s.runtimeUploadLeasesForRange(ctx, group, 0, len(group))
		for offset, index := range indexes {
			if leaseErr != nil {
				lifecycleFailure(&items[index], leaseErr)
			} else {
				leases[index] = loaded[offset]
			}
		}
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < min(uploadLifecycleProviderConcurrency, len(pending)); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				item := &items[index]
				if item.ErrorKind != "" {
					continue
				}
				select {
				case s.engine.uploadLifecycleSlots <- struct{}{}:
				case <-ctx.Done():
					continue
				}
				if !s.engine.clock.Now().Before(records[index].ExpiresAt) {
					item.Status.State, item.NextAction = domain.UploadStateExpired, "restart"
					<-s.engine.uploadLifecycleSlots
					continue
				}
				traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "upload-lifecycle", Subsystem: "upload-session", ParallelGroup: "upload-lifecycle-page"})
				progress, progressErr := transfers.UploadProgress(traced, leases[index])
				if progressErr == nil && (progress.Offset < 0 || progress.Offset > records[index].Size || progress.Complete && progress.Offset != records[index].Size) {
					progressErr = domain.NewError(domain.ErrorInvalid, "upload provider progress is inconsistent")
				}
				if progressErr == nil {
					item.Status.ConfirmedOffset, item.Status.DataComplete = progress.Offset, progress.Complete
					item.NextAction = "resume"
					if progress.Complete {
						item.NextAction = "complete"
					} else if resume {
						capability, resumeErr := transfers.ResumeUpload(traced, leases[index])
						progressErr = resumeErr
						if resumeErr == nil && !capability.ExpiresAt.Equal(records[index].ExpiresAt) {
							progressErr = domain.NewError(domain.ErrorInvalid, "upload resume changed its session deadline")
						}
						if progressErr == nil {
							value := domainUploadCapability(records[index].UploadID, capability)
							if membership := records[index].Batch; membership != nil {
								value.BatchID, value.BatchIndex, value.BatchCount = membership.BatchID, membership.Index, membership.Count
							}
							item.Capability, item.NextAction = &value, "transfer"
						}
					}
				}
				<-s.engine.uploadLifecycleSlots
				if progressErr != nil {
					lifecycleFailure(item, progressErr)
				}
			}
		}()
	}
	for _, index := range pending {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
