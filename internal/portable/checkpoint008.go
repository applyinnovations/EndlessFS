package portable

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// drainExpiredSchema008Uploads proves that no live provider-native upload
// capability can mutate bytes outside the frozen canonical snapshot. An
// unexpired capability makes checkpoint closure retryable; expired leases are
// aborted and removed before the checkpoint inventory is constructed.
func (e *Engine) drainExpiredSchema008Uploads(ctx context.Context) error {
	catalogSnapshot, found, err := e.readDomainCatalogIfPresent(ctx)
	if err != nil || !found {
		return err
	}
	files := &FileStore{engine: e}
	return newDomainCatalog(e.backend, e.scheduler).visitEntries(ctx, catalogSnapshot.head, func(entry storageformat.DomainCatalogEntry) error {
		if entry.Kind != storageformat.DomainNamespace {
			return nil
		}
		reference := consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}
		snapshot, err := e.stateDomainStore().loadHead(ctx, reference)
		if err != nil {
			return err
		}
		after := ""
		for {
			values, err := e.stateDomainStore().listAtHead(ctx, reference, snapshot.head, "upload/", after, domainPageMaximumItems)
			if err != nil {
				return err
			}
			for _, value := range values {
				var record storageformat.PortableUploadRecord
				if err := decodeCanonicalValue(value.Value, &record); err != nil || storageformat.ValidatePortableUploadRecord(record) != nil || record.OwnerID != reference.ID || uploadRecordKey(record.UploadID) != value.Key {
					return domain.NewError(domain.ErrorInvalid, "invalid portable upload while closing checkpoint")
				}
				if record.CleanupPending {
					owner, parseErr := domain.ParseUserID(record.OwnerID)
					if parseErr != nil {
						return domain.NewError(domain.ErrorInvalid, "invalid upload cleanup owner")
					}
					if err := files.cleanupPortableUpload(ctx, owner, record.UploadID, nil); err != nil {
						return err
					}
					continue
				}
				if record.State != storageformat.UploadActive && record.State != storageformat.UploadInitializing {
					continue
				}
				if e.clock.Now().UTC().Before(record.ExpiresAt) {
					return domain.NewError(domain.ErrorUnavailable, "active upload capability prevents checkpoint closure")
				}
				transfers, err := files.transferBackend()
				if err != nil {
					return err
				}
				leaseKey := storageformat.LeaseKey(transfers.BackendKind(), record.UploadID)
				lease, getErr := e.backend.Get(ctx, leaseKey)
				if errors.Is(getErr, domain.ErrNotFound) {
					continue
				}
				if getErr != nil {
					return getErr
				}
				if err := transfers.AbortUpload(ctx, lease.Body); err != nil && !errors.Is(err, domain.ErrNotFound) {
					return err
				}
				if err := e.backend.Delete(ctx, leaseKey, objectstore.DeleteCondition{Version: lease.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
					return err
				}
			}
			if len(values) < domainPageMaximumItems {
				return nil
			}
			after = values[len(values)-1].Key
		}
	})
}

// validateSchema008CheckpointClosure authenticates all schema-008 authority
// while the catalog and every registered domain are frozen. The inventory
// builder that follows remains metadata-only; this validation never opens a
// file body and checks referenced blobs solely through provider metadata.
func (e *Engine) validateSchema008CheckpointClosure(ctx context.Context, freezeEpoch uint64) error {
	return e.validateConsistencyDomainCheckpointClosure(ctx, freezeEpoch, false)
}

// validateSchema009CheckpointClosure additionally enforces the schema-009
// invariant-aligned route and typed value binding for every application state
// entry. The schema-008 wrapper remains separate because migration must
// authenticate predecessor authority using its historical routing law.
func (e *Engine) validateSchema009CheckpointClosure(ctx context.Context, freezeEpoch uint64) error {
	return e.validateConsistencyDomainCheckpointClosure(ctx, freezeEpoch, true)
}

func (e *Engine) validateConsistencyDomainCheckpointClosure(ctx context.Context, freezeEpoch uint64, schema009 bool) error {
	catalogSnapshot, found, err := e.readDomainCatalogIfPresent(ctx)
	if err != nil {
		return err
	}
	if !found || catalogSnapshot.head.FreezeEpoch != freezeEpoch {
		return domain.NewError(domain.ErrorPreconditionFailed, "consistency-domain catalog is not checkpoint-frozen")
	}
	catalog := newDomainCatalog(e.backend, e.scheduler)
	if err := catalog.visitEntries(ctx, catalogSnapshot.head, func(entry storageformat.DomainCatalogEntry) error {
		reference := consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}
		snapshot, err := e.stateDomainStore().loadHead(ctx, reference)
		if err != nil {
			return err
		}
		if !snapshot.exists || !snapshot.head.Registered || !snapshot.head.Frozen || snapshot.head.FreezeEpoch != freezeEpoch {
			return domain.NewError(domain.ErrorPreconditionFailed, "registered consistency domain is not checkpoint-frozen")
		}
		if err := e.validateConsistencyDomainClosureForSchema(ctx, reference, snapshot.head, schema009); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	// Every authoritative page is reached through the catalog, a frozen domain
	// head, an authority tree, or a namespace/result root above. Unreachable
	// content-addressed pages are collectable garbage: checkpoint liveness and
	// portability must never depend on decoding them.
	return nil
}

func (e *Engine) validateConsistencyDomainClosure(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead) error {
	return e.validateConsistencyDomainClosureForSchema(ctx, reference, head, false)
}

func (e *Engine) validateConsistencyDomainClosureForSchema(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, schema009 bool) error {
	session := newConsistencyDomainTreeSession(e.stateDomainStore(), reference)
	rootNames := []string{"base", "outcomes", "outcome-expiry"}
	for rootIndex, root := range []storageformat.DomainTreeRoot{head.Base, head.Outcomes, head.OutcomeExpiry} {
		iterator, err := newConsistencyDomainTreeIterator(ctx, session, root)
		if err != nil {
			return domain.WrapError(domain.KindOf(err), fmt.Sprintf("open consistency-domain %s authority tree", rootNames[rootIndex]), err)
		}
		for {
			entry, found, err := iterator.Next()
			if err != nil {
				return domain.WrapError(domain.KindOf(err), fmt.Sprintf("walk consistency-domain %s authority tree", rootNames[rootIndex]), err)
			}
			if !found {
				break
			}
			if entry.Key == "" || entry.LogicalVersion == "" {
				return domain.NewError(domain.ErrorInvalid, "invalid consistency-domain closure entry")
			}
		}
	}
	if reference.Kind != storageformat.DomainNamespace {
		return e.validateKnownControlDomainValuesForSchema(ctx, reference, head, schema009)
	}
	owner, err := domain.ParseUserID(reference.ID)
	if err != nil {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace owner domain")
	}
	for _, area := range []domain.Area{domain.AreaLive, domain.AreaTrash} {
		value, found, err := e.stateDomainStore().lookupAtHead(ctx, reference, head, namespaceRootKey(area))
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		root, err := decodeNamespaceEntry(value.Data)
		if err != nil {
			return err
		}
		if root.Entry.Name != "" {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace checkpoint root")
		}
		if err := e.validateNamespaceEntryClosure(ctx, session, owner, namespaceRootPath(), root); err != nil {
			return err
		}
	}
	for _, delta := range head.Deltas {
		if schema009 && schema009NamespaceDeltaHasOpaqueResult(delta) {
			continue
		}
		if err := e.validateNamespaceMutationResultClosure(ctx, session, delta.Result); err != nil {
			return err
		}
	}
	if err := e.validateNamespaceOutcomeClosureForSchema(ctx, session, head.Outcomes, schema009); err != nil {
		return err
	}
	return e.validateKnownControlDomainValuesForSchema(ctx, reference, head, schema009)
}

func (e *Engine) validateKnownControlDomainValues(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead) error {
	return e.validateKnownControlDomainValuesForSchema(ctx, reference, head, false)
}

func (e *Engine) validateKnownControlDomainValuesForSchema(ctx context.Context, reference consistencyDomainRef, head storageformat.DomainHead, schema009 bool) error {
	session := newConsistencyDomainTreeSession(e.stateDomainStore(), reference)
	var namespaceOwner domain.UserID
	if reference.Kind == storageformat.DomainNamespace {
		var err error
		namespaceOwner, err = domain.ParseUserID(reference.ID)
		if err != nil {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace owner domain")
		}
	}
	after := ""
	for {
		values, err := e.stateDomainStore().listAtHead(ctx, reference, head, "", after, domainPageMaximumItems)
		if err != nil {
			return err
		}
		for _, value := range values {
			if reference.Kind == storageformat.DomainNamespace {
				switch {
				case value.Key == namespaceRootKey(domain.AreaLive), value.Key == namespaceRootKey(domain.AreaTrash):
					// The recursive namespace validator above owns these values.
				case strings.HasPrefix(value.Key, "upload/"):
					uploadID := strings.TrimPrefix(value.Key, "upload/")
					if _, err := decodePortableUploadRecord(value.Value, namespaceOwner, uploadID); err != nil {
						return domain.NewError(domain.ErrorInvalid, "invalid portable upload in checkpoint closure")
					}
				case strings.HasPrefix(value.Key, "upload-idempotency/"):
					var record storageformat.PortableUploadIdempotency
					if err := decodeCanonicalValue(value.Value, &record); err != nil || storageformat.ValidatePortableUploadIdempotency(record) != nil || record.OwnerID != reference.ID || value.Key != "upload-idempotency/"+record.KeyDigest {
						return domain.NewError(domain.ErrorInvalid, "invalid portable upload idempotency in checkpoint closure")
					}
					upload, found, err := e.stateDomainStore().lookupAtHeadWithSession(ctx, reference, head, uploadRecordKey(record.UploadID), session)
					if err != nil {
						return err
					}
					if !found {
						return domain.NewError(domain.ErrorInvalid, "portable upload idempotency target is missing")
					}
					if _, err := decodePortableUploadRecord(upload.Data, namespaceOwner, record.UploadID); err != nil {
						return domain.NewError(domain.ErrorInvalid, "portable upload idempotency target is invalid")
					}
				default:
					if !schema009 {
						return domain.NewError(domain.ErrorInvalid, "unknown namespace authority value")
					}
					key, err := parseExistingStateKey(value.Key)
					if err != nil {
						return domain.NewError(domain.ErrorInvalid, "unknown namespace authority value")
					}
					routed, routeErr := stateDomainReferenceForKey009(key)
					if routeErr != nil || routed != reference {
						return domain.NewError(domain.ErrorInvalid, "state authority value is stored in the wrong consistency domain")
					}
					if _, err := decodeStateValue009(key, value.Value); err != nil {
						return domain.NewError(domain.ErrorInvalid, "invalid typed namespace state authority")
					}
				}
				continue
			}
			if strings.HasPrefix(value.Key, "duplicates/ignore/group/") {
				groupID := strings.TrimPrefix(value.Key, "duplicates/ignore/group/")
				var record domain.DuplicateIgnore
				if reference.Kind != storageformat.DomainOwnerControl || !strings.HasPrefix(reference.ID, "owner:") || decodeCanonicalValue(value.Value, &record) != nil || validateDuplicateGroupID(groupID) != nil || record.GroupID != groupID || record.Revision == 0 {
					return domain.NewError(domain.ErrorInvalid, "invalid duplicate ignore authority in checkpoint closure")
				}
				continue
			}
			if strings.HasPrefix(value.Key, "duplicates/ignore/pair/") {
				pairID := strings.TrimPrefix(value.Key, "duplicates/ignore/pair/")
				var record storageformat.DuplicateDirectoryPreference
				if reference.Kind != storageformat.DomainOwnerControl || !strings.HasPrefix(reference.ID, "owner:") || decodeCanonicalValue(value.Value, &record) != nil || storageformat.ValidateDuplicateDirectoryPreference(record) != nil || record.PairID != pairID {
					return domain.NewError(domain.ErrorInvalid, "invalid duplicate directory preference in checkpoint closure")
				}
				continue
			}
			key, err := parseExistingStateKey(value.Key)
			if err != nil {
				return domain.NewError(domain.ErrorInvalid, "unknown consistency-domain authority value")
			}
			routed, err := stateDomainReferenceForKey008(key)
			if schema009 {
				routed, err = stateDomainReferenceForKey009(key)
				if err == nil {
					_, err = decodeStateValue009(key, value.Value)
				}
			}
			if err != nil || routed != reference {
				return domain.NewError(domain.ErrorInvalid, "state authority value is stored in the wrong consistency domain")
			}
		}
		if len(values) < domainPageMaximumItems {
			return nil
		}
		after = values[len(values)-1].Key
	}
}

func (e *Engine) validateNamespaceEntryClosure(ctx context.Context, session *consistencyDomainTreeSession, owner domain.UserID, path domain.UserPath, directory storageformat.NamespaceEntry) error {
	if directory.Entry.Kind != domain.EntryDirectory {
		return domain.NewError(domain.ErrorInvalid, "namespace closure root is not a directory")
	}
	iterator, err := newConsistencyDomainTreeIterator(ctx, session, directory.Children)
	if err != nil {
		return err
	}
	var count uint64
	for {
		value, found, err := iterator.Next()
		if err != nil {
			return err
		}
		if !found {
			break
		}
		entry, err := decodeNamespaceEntry(value.Value)
		if err != nil || entry.Entry.Name != value.Key || entry.Entry.LogicalVersion != value.LogicalVersion {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace checkpoint child binding")
		}
		childPath, err := path.Join(entry.Entry.Name)
		if err != nil {
			return domain.NewError(domain.ErrorInvalid, "namespace checkpoint path exceeds canonical bounds")
		}
		count++
		if entry.Entry.Kind == domain.EntryDirectory {
			if err := e.validateNamespaceEntryClosure(ctx, session, owner, childPath, entry); err != nil {
				return err
			}
			continue
		}
		info, err := e.fileBackend.Head(ctx, storageformat.BlobKey(owner.String(), entry.Entry.BlobID))
		if err != nil {
			return domain.WrapError(domain.KindOf(err), "validate namespace file-blob metadata", err)
		}
		if info.Size != entry.Entry.Size || info.Fingerprint.MD5 != entry.Entry.MD5 || info.Fingerprint.CRC32C != entry.Entry.CRC32C || !info.Fingerprint.Complete() {
			return domain.NewError(domain.ErrorPreconditionFailed, "namespace blob metadata does not match checkpoint authority")
		}
	}
	if count != directory.EntryCount {
		return domain.NewError(domain.ErrorInvalid, "namespace checkpoint directory count mismatch")
	}
	return nil
}

func (e *Engine) validateNamespaceOutcomeClosure(ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot) error {
	return e.validateNamespaceOutcomeClosureForSchema(ctx, session, root, false)
}

func (e *Engine) validateNamespaceOutcomeClosureForSchema(ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot, schema009 bool) error {
	iterator, err := newConsistencyDomainTreeIterator(ctx, session, root)
	if err != nil {
		return err
	}
	for {
		value, found, err := iterator.Next()
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		var outcome storageformat.DomainOutcome
		if err := decodeCanonicalValue(value.Value, &outcome); err != nil || storageformat.ValidateDomainOutcome(outcome) != nil || outcome.MutationID != value.Key {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace outcome closure")
		}
		if schema009 && !isRecognizedNamespaceMutationResult(outcome.Result) {
			// StateStore results are intentionally opaque to the portable engine.
			// Schema 008 never routed generic state into the namespace domain;
			// schema 009 does, so only a positively recognized namespace result may
			// name additional immutable batch pages.
			continue
		}
		if err := e.validateNamespaceMutationResultClosure(ctx, session, outcome.Result); err != nil {
			return err
		}
	}
}

func schema009NamespaceDeltaHasOpaqueResult(delta storageformat.DomainDelta) bool {
	for _, change := range delta.Changes {
		if change.Key == transitionLockKey009 {
			return true
		}
		if _, err := parseExistingStateKey(change.Key); err == nil {
			return true
		}
	}
	return false
}

func isRecognizedNamespaceMutationResult(body []byte) bool {
	var result storageformat.NamespaceMutationResult
	return decodeCanonicalValue(body, &result) == nil && validateNamespaceMutationResult(result) == nil
}

func (e *Engine) validateNamespaceMutationResultClosure(ctx context.Context, session *consistencyDomainTreeSession, body []byte) error {
	var result storageformat.NamespaceMutationResult
	if err := decodeCanonicalValue(body, &result); err != nil || validateNamespaceMutationResult(result) != nil {
		return domain.NewError(domain.ErrorInvalid, "invalid namespace outcome result closure")
	}
	if result.Batch == nil {
		return nil
	}
	items, err := newConsistencyDomainTreeIterator(ctx, session, result.Batch.Items)
	if err != nil {
		return err
	}
	var count uint64
	for {
		item, found, err := items.Next()
		if err != nil {
			return err
		}
		if !found {
			break
		}
		var stored storageformat.NamespaceBatchItem
		if err := decodeCanonicalValue(item.Value, &stored); err != nil || storageformat.ValidateNamespaceBatchItem(stored) != nil {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace batch outcome closure")
		}
		count++
	}
	if count != result.Batch.ItemCount {
		return domain.NewError(domain.ErrorInvalid, "namespace batch outcome count mismatch")
	}
	return nil
}

// Compile-time proof that checkpoint validation has no file-body interface.
var _ objectstore.MetadataBackend = (objectstore.FileControlBackend)(nil)
