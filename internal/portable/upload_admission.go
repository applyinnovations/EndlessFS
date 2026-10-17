package portable

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Admission phases are separate modeled waves: a lease write depends on its
// native begin, and resume depends on progress. Legacy traces are unchanged.
func admissionProviderContext(ctx context.Context, phase string) context.Context {
	trace := providerbudget.TraceFromContext(ctx)
	if trace.Operation != "upload-admission" {
		return ctx
	}
	trace.Subsystem = "upload-session"
	if phase == "lease-read" || phase == "lease-write" {
		trace.Subsystem = "admission-leases"
	}
	trace.ParallelGroup = "upload-admission-" + phase
	return providerbudget.WithTrace(ctx, trace)
}

func admissionFailure(result *domain.UploadAdmissionResult, err error, unknown bool) {
	result.Capability = nil
	result.ErrorKind = domain.KindOf(err)
	result.Retryable = errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrRateLimited) || errors.Is(err, domain.ErrInternal)
	result.Outcome, result.NextAction = "rejected", "attention"
	if result.Retryable {
		result.Outcome, result.NextAction = "retryable", "retry"
	}
	if unknown && result.Retryable {
		result.Outcome = "unknown"
	}
}

func admissionTerminal(result *domain.UploadAdmissionResult, record storageformat.PortableUploadRecord, nowExpired bool) bool {
	status, terminal, err := lifecycleRecordStatus(record, domain.UploadID(record.UploadID), nowExpired)
	if err != nil {
		admissionFailure(result, err, false)
		return true
	}
	result.UploadID, result.Status = domain.UploadID(record.UploadID), &status
	if terminal {
		result.Capability = nil
		result.Outcome = string(status.State)
		result.NextAction = "restart"
		if status.State == domain.UploadStateCompleted {
			result.NextAction = "done"
		}
	}
	return terminal
}

type admissionRecord struct {
	record storageformat.PortableUploadRecord
	fresh  bool
}

// AdmitUploads uses existing single-upload receipts and transient leases, but
// coalesces canonical intent and activation publications for the page. A native
// failure never revokes an accepted sibling. Existing exact-membership batches
// are read-only replay inputs; their admission contract is not reinterpreted.
func (s *FileStore) AdmitUploads(ctx context.Context, scope domain.Scope, requested []domain.CreateUploadRequest) ([]domain.UploadAdmissionResult, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return nil, err
	}
	if len(requested) < 1 || len(requested) > domain.MaxUploadLifecycleItems {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid admission page size")
	}
	seen := make(map[string]bool, len(requested))
	for _, request := range requested {
		if len(request.IdempotencyKey) < 16 || validatePortableIdempotencyKey(request.IdempotencyKey) != nil || seen[request.IdempotencyKey] {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid or repeated admission item key")
		}
		seen[request.IdempotencyKey] = true
	}
	ctx = providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "upload-admission", Subsystem: "admission-metadata"})
	results := make([]domain.UploadAdmissionResult, len(requested))
	requests := make([]domain.CreateUploadRequest, len(requested))
	fingerprints := make([]string, len(requested))
	for i, request := range requested {
		results[i].Index, results[i].ID = i, request.IdempotencyKey
		var err error
		requests[i], fingerprints[i], err = normalizePortableUploadRequest(scope, request)
		if err != nil {
			admissionFailure(&results[i], err, false)
		}
	}
	valid := false
	for _, result := range results {
		if result.Outcome == "" {
			valid = true
			break
		}
	}
	if !valid {
		return results, nil
	}
	records, err := s.admissionIntents(ctx, scope, requests, fingerprints, results)
	if err != nil {
		for i := range results {
			if results[i].Outcome == "" {
				admissionFailure(&results[i], err, true)
			}
		}
		return results, nil
	}
	pending := make([]int, 0, len(requested))
	for i, item := range records {
		if results[i].Outcome != "" || item.record.UploadID == "" {
			continue
		}
		if !admissionTerminal(&results[i], item.record, !s.engine.clock.Now().Before(item.record.ExpiresAt)) {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		return results, nil
	}
	if err := s.engine.step(ctx, StepUploadAdmissionAfterIntents); err != nil {
		for i := range results {
			if results[i].Outcome == "" {
				admissionFailure(&results[i], err, true)
			}
		}
		return results, nil
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < min(uploadLifecycleProviderConcurrency, len(pending)); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				select {
				case s.engine.uploadLifecycleSlots <- struct{}{}:
				case <-ctx.Done():
					admissionFailure(&results[i], domain.ErrUnavailable, true)
					continue
				}
				func() {
					defer func() { <-s.engine.uploadLifecycleSlots }()
					record := records[i].record
					if admissionTerminal(&results[i], record, !s.engine.clock.Now().Before(record.ExpiresAt)) {
						return
					}
					traced := admissionProviderContext(ctx, "begin")
					var capability objectstore.UploadCapability
					var err error
					if record.State == storageformat.UploadInitializing && record.Batch == nil {
						capability, _, err = s.ensurePortableUploadLease(traced, record, records[i].fresh)
					} else {
						var lease []byte
						lease, _, err = s.runtimeUploadLeaseForRecord(admissionProviderContext(ctx, "lease-read"), record)
						if err == nil {
							var backend objectstore.DirectTransferBackend
							backend, err = s.transferBackend()
							if err == nil {
								var progress objectstore.UploadProgress
								progress, err = backend.UploadProgress(admissionProviderContext(ctx, "progress"), lease)
								if err == nil && (progress.Offset < 0 || progress.Offset > record.Size || progress.Complete && progress.Offset != record.Size) {
									err = domain.NewError(domain.ErrorInvalid, "upload provider progress is inconsistent")
								}
								if err == nil {
									results[i].Status.ConfirmedOffset, results[i].Status.DataComplete = progress.Offset, progress.Complete
									if !progress.Complete {
										capability, err = backend.ResumeUpload(admissionProviderContext(ctx, "resume"), lease)
									}
								}
							}
						}
					}
					if err == nil && !results[i].Status.DataComplete && !capability.ExpiresAt.Equal(record.ExpiresAt) {
						err = domain.NewError(domain.ErrorInvalid, "admission changed upload deadline")
					}
					if err != nil {
						admissionFailure(&results[i], err, true)
						return
					}
					if !results[i].Status.DataComplete {
						value := domainUploadCapability(record.UploadID, capability)
						if membership := record.Batch; membership != nil {
							value.BatchID, value.BatchIndex, value.BatchCount = membership.BatchID, membership.Index, membership.Count
						}
						results[i].Capability = &value
					}
				}()
			}
		}()
	}
	for _, i := range pending {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.engine.step(ctx, StepUploadAdmissionAfterLeases); err != nil {
		for _, i := range pending {
			if results[i].Outcome == "" {
				admissionFailure(&results[i], err, true)
			}
		}
		return results, nil
	}
	if err := s.activateAdmissionPage(ctx, scope, records, results); err != nil {
		for _, i := range pending {
			if results[i].Outcome == "" {
				admissionFailure(&results[i], err, true)
			}
		}
		return results, nil
	}
	if err := s.engine.step(ctx, StepUploadAdmissionAfterActivation); err != nil {
		for _, i := range pending {
			if results[i].Outcome == "accepted" || results[i].Outcome == "already_accepted" {
				admissionFailure(&results[i], err, true)
			}
		}
	}
	return results, nil
}

func (s *FileStore) admissionIntents(ctx context.Context, scope domain.Scope, requests []domain.CreateUploadRequest, fingerprints []string, results []domain.UploadAdmissionResult) ([]admissionRecord, error) {
	records := make([]admissionRecord, len(requests))
	for attempt := 0; attempt < 8; attempt++ {
		store := newNamespaceStore(s.engine)
		view, err := store.loadView(ctx, scope.UserID(), "")
		if err != nil {
			return nil, err
		}
		if view.headSnapshot == nil {
			return nil, domain.NewError(domain.ErrorInvalid, "admission snapshot is missing")
		}
		if !view.headSnapshot.exists || !view.headSnapshot.head.Registered {
			if err := s.engine.stateDomainStore().ensureRegistered(ctx, uploadDomainReference(scope.UserID()), *view.headSnapshot); err != nil {
				return nil, err
			}
			continue
		}
		changes := make([]consistencyDomainChange, 0, len(requests)*2)
		bindings := make([]string, 0, len(requests))
		firstID := ""
		destinations := make(map[string]bool)
		for i, request := range requests {
			if results[i].Outcome != "" {
				continue
			}
			record, found, err := s.portableUploadByIdempotencyAtView(ctx, view, scope.UserID(), request.IdempotencyKey, fingerprints[i])
			if err != nil {
				admissionFailure(&results[i], err, false)
				continue
			}
			if found {
				records[i] = admissionRecord{record: record}
				continue
			}
			resolved, targetExisted, err := store.prepareDestinationAtView(ctx, view, scope, request.Path, request.Conflict, request.ExpectedVersion)
			if err != nil {
				admissionFailure(&results[i], err, false)
				continue
			}
			if destinations[resolved.String()] {
				admissionFailure(&results[i], domain.NewError(domain.ErrorConflict, "admission repeats a destination"), false)
				continue
			}
			destinations[resolved.String()] = true
			id, err := s.engine.ids.OpaqueID()
			if err != nil {
				return nil, err
			}
			now := s.engine.clock.Now().UTC()
			record = storageformat.PortableUploadRecord{SchemaVersion: 1, UploadID: id, OwnerID: scope.UserID().String(), Area: areaName(scope.Area()), RequestedPath: request.Path.String(), ResolvedPath: resolved.String(), BlobID: id, Size: request.Size, MediaType: request.MediaType, Conflict: request.Conflict, ExpectedVersion: request.ExpectedVersion, TargetExisted: targetExisted, Resumable: request.Resumable, State: storageformat.UploadInitializing, CreatedAt: now, ExpiresAt: now.Add(s.engine.uploadLifetime(request.Resumable))}
			recordBody, err := storageformat.EncodeCanonical(record)
			if err != nil {
				return nil, err
			}
			receipt := storageformat.PortableUploadIdempotency{SchemaVersion: 1, OwnerID: scope.UserID().String(), KeyDigest: storageformat.Digest([]byte(request.IdempotencyKey)), Fingerprint: fingerprints[i], UploadID: id}
			receiptBody, err := storageformat.EncodeCanonical(receipt)
			if err != nil {
				return nil, err
			}
			changes = append(changes, consistencyDomainChange{Key: uploadRecordKey(id), Require: domainValueAbsent, Value: recordBody}, consistencyDomainChange{Key: uploadIdempotencyKey(request.IdempotencyKey), Require: domainValueAbsent, Value: receiptBody})
			records[i] = admissionRecord{record: record, fresh: true}
			bindings = append(bindings, request.IdempotencyKey+"\x00"+fingerprints[i])
			if firstID == "" {
				firstID = id
			}
		}
		if len(changes) == 0 {
			return records, nil
		}
		sort.Strings(bindings)
		fingerprint := namespaceRequestFingerprint("upload-admission", strings.Join(bindings, "\x00"))
		body, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Upload: &storageformat.NamespaceUploadMutationResult{UploadID: firstID, State: "initializing"}})
		if err != nil {
			return nil, err
		}
		id := "upload-admission:" + fingerprint
		if err := view.bindMutation(id, fingerprint); err != nil {
			return nil, err
		}
		_, err = s.engine.stateDomainStore().mutateMaterializedPrepared(ctx, uploadDomainReference(scope.UserID()), consistencyDomainMutation{ID: id, Changes: changes, Result: body}, view.headSnapshot, view.session)
		if err == nil {
			return records, nil
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) && !errors.Is(err, domain.ErrUnavailable) {
			return nil, err
		}
	}
	return nil, domain.NewError(domain.ErrorUnavailable, "admission intents remained contended")
}

func (s *FileStore) activateAdmissionPage(ctx context.Context, scope domain.Scope, records []admissionRecord, results []domain.UploadAdmissionResult) error {
	pending := false
	for _, result := range results {
		if result.Outcome == "" {
			pending = true
			break
		}
	}
	if !pending {
		return nil
	}
	for attempt := 0; attempt < 8; attempt++ {
		store := newNamespaceStore(s.engine)
		view, err := store.loadView(ctx, scope.UserID(), "")
		if err != nil {
			return err
		}
		changes := make([]consistencyDomainChange, 0, len(records))
		bindings := make([]string, 0, len(records))
		firstID := ""
		ready := make([]int, 0, len(records))
		for i, item := range records {
			if item.record.UploadID == "" || results[i].Outcome != "" {
				continue
			}
			record, value, err := s.portableUploadAtView(ctx, view, scope.UserID(), item.record.UploadID)
			if err != nil {
				admissionFailure(&results[i], err, true)
				continue
			}
			confirmed, complete := results[i].Status.ConfirmedOffset, results[i].Status.DataComplete
			if admissionTerminal(&results[i], record, !s.engine.clock.Now().Before(record.ExpiresAt)) {
				continue
			}
			results[i].Status.ConfirmedOffset, results[i].Status.DataComplete = confirmed, complete
			ready = append(ready, i)
			if record.State != storageformat.UploadInitializing || record.Batch != nil {
				continue
			}
			record.State = storageformat.UploadActive
			body, err := storageformat.EncodeCanonical(record)
			if err != nil {
				return err
			}
			changes = append(changes, consistencyDomainChange{Key: uploadRecordKey(record.UploadID), Require: domainValuePresent, ExpectedVersion: value.LogicalVersion, Value: body})
			bindings = append(bindings, record.UploadID+"\x00"+string(value.LogicalVersion))
			if firstID == "" {
				firstID = record.UploadID
			}
		}
		if len(changes) > 0 {
			sort.Strings(bindings)
			fingerprint := namespaceRequestFingerprint("upload-admission-activate", strings.Join(bindings, "\x00"))
			body, err := storageformat.EncodeCanonical(storageformat.NamespaceMutationResult{SchemaVersion: 1, RequestFingerprint: fingerprint, Upload: &storageformat.NamespaceUploadMutationResult{UploadID: firstID, State: "active"}})
			if err != nil {
				return err
			}
			id := "upload-admission-activate:" + fingerprint
			if err := view.bindMutation(id, fingerprint); err != nil {
				return err
			}
			_, err = s.engine.stateDomainStore().mutateMaterializedPrepared(ctx, uploadDomainReference(scope.UserID()), consistencyDomainMutation{ID: id, Changes: changes, Result: body}, view.headSnapshot, view.session)
			if err != nil {
				if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrUnavailable) {
					continue
				}
				return err
			}
		}
		for _, i := range ready {
			results[i].Outcome, results[i].NextAction = "accepted", "transfer"
			if !records[i].fresh {
				results[i].Outcome = "already_accepted"
			}
			if results[i].Status.DataComplete {
				results[i].NextAction = "complete"
			}
		}
		return nil
	}
	return domain.NewError(domain.ErrorUnavailable, "admission activation remained contended")
}
