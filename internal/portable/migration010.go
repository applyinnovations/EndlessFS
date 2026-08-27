package portable

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"sort"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const schema010ConservationSchema = 1

const (
	schema010DispositionRecover        = "recover"
	schema010DispositionAlreadyPresent = "already-present"
)

// schema010ConservationReceipt binds one schema-007 indexed-state authority
// item retained in a schema-009 bucket to the exact typed schema-010 value.
// The target bytes are metadata, never file-body bytes.
type schema010ConservationReceipt struct {
	SchemaVersion        int                                 `json:"schemaVersion"`
	SourceRootKey        string                              `json:"sourceRootKey"`
	SourceRootDigest     string                              `json:"sourceRootDigest"`
	SourceLogicalKey     string                              `json:"sourceLogicalKey"`
	SourceLogicalVersion string                              `json:"sourceLogicalVersion"`
	SourceVersionKey     string                              `json:"sourceVersionKey"`
	SourceVersionDigest  string                              `json:"sourceVersionDigest"`
	TargetDomainKind     storageformat.ConsistencyDomainKind `json:"targetDomainKind"`
	TargetDomainID       string                              `json:"targetDomainID"`
	TargetKey            string                              `json:"targetKey"`
	TargetRecordType     string                              `json:"targetRecordType"`
	TargetValue          []byte                              `json:"targetValue"`
	TargetValueDigest    string                              `json:"targetValueDigest"`
	Disposition          string                              `json:"disposition"`
}

type schema010ConservationRoot struct {
	Namespace         string `json:"namespace"`
	RootKey           string `json:"rootKey"`
	RootDigest        string `json:"rootDigest"`
	EntryCount        uint64 `json:"entryCount"`
	ReceiptCommitment string `json:"receiptCommitment"`
}

type schema010Conservation struct {
	SchemaVersion    int                             `json:"schemaVersion"`
	FreezeEpoch      uint64                          `json:"freezeEpoch"`
	SourceCatalog    storageformat.DomainCatalogHead `json:"sourceCatalog"`
	Roots            []schema010ConservationRoot     `json:"roots,omitempty"`
	SourceEntryCount uint64                          `json:"sourceEntryCount"`
	RecoveredCount   uint64                          `json:"recoveredCount"`
	PresentCount     uint64                          `json:"presentCount"`
	Commitment       string                          `json:"commitment"`
}

func (e *Engine) runStorageMigration009To010(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageStarted})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterDetection)); err != nil {
		return err
	}
	if complete, err := e.storageMigrationComplete(ctx, transition); err == nil && complete {
		return nil
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err := e.verifyMigrationWriterSet(ctx, transition); err != nil {
		return err
	}
	closed, err := e.closeStorageMigrationGate(ctx, transition, aggregateMigrationPlan{})
	if err != nil || !closed {
		return err
	}
	gate, active, err := e.readClosedStorageMigrationGate(ctx, transition)
	if err != nil || !active {
		return err
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageGateClosed})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterGateClosed)); err != nil {
		return err
	}
	conservation, err := e.stageSchema009IndexedState010(ctx, gate.Epoch)
	if err != nil {
		return domain.WrapError(domain.KindOf(err), "prove schema-009 indexed-state conservation", err)
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterDirectoryPrerequisites)); err != nil {
		return err
	}
	targetCatalog, err := e.installSchema010Receipts(ctx, conservation)
	if err != nil {
		return domain.WrapError(domain.KindOf(err), "install schema-010 recovered state", err)
	}
	if err := e.publishSchema010Catalog(ctx, conservation.SourceCatalog, targetCatalog, gate.Epoch); err != nil {
		return domain.WrapError(domain.KindOf(err), "publish schema-010 consistency-domain catalog", err)
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterDirectoryRoot)); err != nil {
		return err
	}
	if err := e.verifySchema010Conservation(ctx, conservation); err != nil {
		return domain.WrapError(domain.KindOf(err), "verify schema-010 state conservation", err)
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterDirectories)); err != nil {
		return err
	}
	if err := e.activateMigrationWriterSet(ctx, transition); err != nil {
		return err
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterWriterSet)); err != nil {
		return err
	}
	if err := e.activateMigrationSuperblock(ctx, transition, superblockObject, superblock); err != nil {
		return err
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterSuperblock)); err != nil {
		return err
	}
	if err := e.bindMigrationGateToTarget(ctx, transition); err != nil {
		return err
	}
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterGateBinding)); err != nil {
		return err
	}
	checkpoint, err := e.createCheckpointWhileClosed(ctx, transition.checkpointID)
	if err != nil {
		if complete, completeErr := e.storageMigrationComplete(ctx, transition); completeErr == nil && complete {
			return nil
		}
		return domain.WrapError(domain.KindOf(err), "create schema-010 migration checkpoint", err)
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageCheckpointCreated})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterCheckpoint)); err != nil {
		return err
	}
	if err := e.openWritesAfterCreatedCheckpoint(ctx, checkpoint); err != nil {
		if complete, completeErr := e.storageMigrationComplete(ctx, transition); completeErr == nil && complete {
			return nil
		}
		return err
	}
	if err := newDomainCatalog(e.backend, e.scheduler).unfreeze(ctx, checkpoint.GateEpoch); err != nil {
		return domain.WrapError(domain.KindOf(err), "unfreeze migrated schema-010 domains", err)
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageComplete})
	return nil
}

func validateSchema010Receipt(receipt schema010ConservationReceipt) (consistencyDomainRef, state.Key, []byte, error) {
	reference := consistencyDomainRef{Kind: receipt.TargetDomainKind, ID: receipt.TargetDomainID}
	logical, err := parseExistingStateKey(receipt.SourceLogicalKey)
	if err != nil || receipt.SchemaVersion != schema010ConservationSchema || receipt.SourceRootKey == "" || receipt.SourceRootDigest == "" || receipt.SourceLogicalVersion == "" || receipt.SourceVersionKey == "" || receipt.SourceVersionDigest == "" || receipt.TargetKey == "" || receipt.TargetRecordType == "" || receipt.TargetValueDigest == "" || validateConsistencyDomainRef(reference) != nil {
		return consistencyDomainRef{}, state.Key{}, nil, domain.NewError(domain.ErrorInvalid, "invalid schema-010 conservation receipt")
	}
	if receipt.Disposition != schema010DispositionRecover && receipt.Disposition != schema010DispositionAlreadyPresent || storageformat.Digest(receipt.TargetValue) != receipt.TargetValueDigest {
		return consistencyDomainRef{}, state.Key{}, nil, domain.NewError(domain.ErrorInvalid, "invalid schema-010 conservation receipt binding")
	}
	target, migratedReference, recordType, targetValue, err := migrateStateEntry009(logical, mustSchema010SourcePayload(receipt))
	if err != nil || migratedReference != reference || target.String() != receipt.TargetKey || recordType != receipt.TargetRecordType || !bytes.Equal(targetValue, receipt.TargetValue) {
		return consistencyDomainRef{}, state.Key{}, nil, domain.NewError(domain.ErrorInvalid, "schema-010 conservation receipt target mismatch")
	}
	body, err := storageformat.EncodeCanonical(receipt)
	return reference, target, body, err
}

// mustSchema010SourcePayload extracts the unchanged application payload from
// the typed target. Validation immediately compares a fresh migration result,
// so a malformed type or payload fails closed rather than being trusted.
func mustSchema010SourcePayload(receipt schema010ConservationReceipt) []byte {
	payload, err := storageformat.DecodeStateRecord009(receipt.TargetValue, receipt.TargetRecordType)
	if err != nil {
		return nil
	}
	return payload
}

func validateSchema010Conservation(value schema010Conservation) ([]byte, error) {
	if value.SchemaVersion != schema010ConservationSchema || value.FreezeEpoch == 0 || value.Commitment == "" || storageformat.ValidateDomainCatalogHead(value.SourceCatalog) != nil || value.SourceCatalog.FreezeEpoch != value.FreezeEpoch || value.SourceEntryCount != value.RecoveredCount+value.PresentCount {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid schema-010 conservation proof")
	}
	previous := ""
	var count uint64
	commitment := sha256.New()
	for _, root := range value.Roots {
		if root.Namespace == "" || root.RootKey == "" || root.RootDigest == "" || root.ReceiptCommitment == "" || root.RootKey <= previous || storageformat.StateIndexRootKey(root.Namespace).String() != root.RootKey {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid schema-010 conservation root proof")
		}
		previous = root.RootKey
		if ^uint64(0)-count < root.EntryCount {
			return nil, domain.NewError(domain.ErrorInvalid, "schema-010 conservation count overflows")
		}
		count += root.EntryCount
		writeSchema010Commitment(commitment, root.RootKey, root.RootDigest, root.ReceiptCommitment)
	}
	if count != value.SourceEntryCount || hex.EncodeToString(commitment.Sum(nil)) != value.Commitment {
		return nil, domain.NewError(domain.ErrorInvalid, "schema-010 conservation commitment mismatch")
	}
	return storageformat.EncodeCanonical(value)
}

func (e *Engine) readSchema010Conservation(ctx context.Context) (schema010Conservation, bool, error) {
	object, err := e.backend.Get(ctx, storageformat.Schema010MigrationConservationKey())
	if errors.Is(err, domain.ErrNotFound) {
		return schema010Conservation{}, false, nil
	}
	if err != nil {
		return schema010Conservation{}, false, err
	}
	var value schema010Conservation
	if decodeCanonicalValue(object.Body, &value) != nil {
		return schema010Conservation{}, false, domain.NewError(domain.ErrorInvalid, "invalid schema-010 conservation proof body")
	}
	body, err := validateSchema010Conservation(value)
	if err != nil || !bytes.Equal(body, object.Body) {
		return schema010Conservation{}, false, domain.NewError(domain.ErrorInvalid, "non-canonical schema-010 conservation proof")
	}
	return value, true, nil
}

// verifySchema010Authority is the ledger-enforced activation barrier. The
// writer set cannot advertise schema 010 until the durable source inventory,
// every source-to-target receipt, and every installed typed target have been
// independently re-read and verified under the closed gate.
func (e *Engine) verifySchema010Authority(ctx context.Context, transition storageMigration) error {
	if transition.id != storageMigration009To010 || transition.from != storageSchema009 || transition.to != storageSchema010 {
		return domain.NewError(domain.ErrorInvalid, "schema-010 authority verifier received another migration")
	}
	proof, found, err := e.readSchema010Conservation(ctx)
	if err != nil {
		return err
	}
	if !found {
		return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 conservation proof is missing")
	}
	return e.verifySchema010Conservation(ctx, proof)
}

func (e *Engine) stageSchema009IndexedState010(ctx context.Context, freezeEpoch uint64) (schema010Conservation, error) {
	if existing, found, err := e.readSchema010Conservation(ctx); err != nil || found {
		if found && existing.FreezeEpoch != freezeEpoch {
			return schema010Conservation{}, domain.NewError(domain.ErrorPreconditionFailed, "schema-010 conservation proof belongs to another gate epoch")
		}
		return existing, err
	}
	catalog := newDomainCatalog(e.backend, e.scheduler)
	sourceCatalog, err := catalog.load(ctx)
	if err != nil {
		return schema010Conservation{}, err
	}
	if !sourceCatalog.exists || sourceCatalog.head.FreezeEpoch != freezeEpoch {
		return schema010Conservation{}, domain.NewError(domain.ErrorPreconditionFailed, "schema-009 source catalog is not frozen")
	}
	proof := schema010Conservation{SchemaVersion: schema010ConservationSchema, FreezeEpoch: freezeEpoch, SourceCatalog: sourceCatalog.head}
	commitment := sha256.New()
	err = visitObjectPages(ctx, e.backend, storageformat.StateIndexRootPrefix(), func(info objectstore.ObjectInfo) error {
		keyValue := info.Key.String()
		if !strings.HasSuffix(keyValue, "/root.json") {
			if !strings.Contains(keyValue, "/nodes/") || !strings.HasSuffix(keyValue, ".json") {
				return domain.NewError(domain.ErrorInvalid, "unknown schema-007 state-index object")
			}
			return nil
		}
		object, err := e.backend.Get(ctx, info.Key)
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var root storageformat.StateIndexRoot
		if err := storageformat.DecodeEnvelope(object.Body, info.Key, stateIndexRootSchema, &envelope, &root); err != nil || root.SchemaVersion != 1 || storageformat.StateIndexRootKey(root.Namespace) != info.Key {
			return domain.NewError(domain.ErrorInvalid, "invalid schema-007 state-index root during recovery")
		}
		rootProof := schema010ConservationRoot{Namespace: root.Namespace, RootKey: info.Key.String(), RootDigest: storageformat.Digest(object.Body), EntryCount: root.EntryCount}
		rootCommitment := sha256.New()
		var observed uint64
		after := ""
		for {
			entries, err := e.collectStateIndexEntries(ctx, root, root.Namespace+"/", after, 256)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				receipt, err := e.schema010ReceiptForEntry(ctx, rootProof, entry, freezeEpoch)
				if err != nil {
					return err
				}
				_, _, body, err := validateSchema010Receipt(receipt)
				if err != nil {
					return err
				}
				receipt, body, err = e.writeSchema010Receipt(ctx, receipt, body)
				if err != nil {
					return err
				}
				writeSchema010Commitment(rootCommitment, receipt.SourceLogicalKey, receipt.SourceLogicalVersion, storageformat.Digest(body))
				observed++
				proof.SourceEntryCount++
				if receipt.Disposition == schema010DispositionRecover {
					proof.RecoveredCount++
				} else {
					proof.PresentCount++
				}
				after = entry.LogicalKey
			}
			if len(entries) < 256 {
				break
			}
		}
		if observed != root.EntryCount {
			return domain.NewError(domain.ErrorInvalid, "schema-007 state-index root count mismatch during recovery")
		}
		rootProof.ReceiptCommitment = hex.EncodeToString(rootCommitment.Sum(nil))
		proof.Roots = append(proof.Roots, rootProof)
		writeSchema010Commitment(commitment, rootProof.RootKey, rootProof.RootDigest, rootProof.ReceiptCommitment)
		return nil
	})
	if err != nil {
		return schema010Conservation{}, err
	}
	sort.Slice(proof.Roots, func(left, right int) bool { return proof.Roots[left].RootKey < proof.Roots[right].RootKey })
	// Recompute after sorting so a backend's page size or order cannot alter the
	// durable proof even though the backend contract already requires ordering.
	commitment = sha256.New()
	for _, root := range proof.Roots {
		writeSchema010Commitment(commitment, root.RootKey, root.RootDigest, root.ReceiptCommitment)
	}
	proof.Commitment = hex.EncodeToString(commitment.Sum(nil))
	body, err := validateSchema010Conservation(proof)
	if err != nil {
		return schema010Conservation{}, err
	}
	key := storageformat.Schema010MigrationConservationKey()
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return schema010Conservation{}, err
		}
		winner, getErr := e.backend.Get(ctx, key)
		if getErr != nil {
			return schema010Conservation{}, getErr
		}
		var stored schema010Conservation
		if decodeCanonicalValue(winner.Body, &stored) != nil {
			return schema010Conservation{}, domain.NewError(domain.ErrorInvalid, "schema-010 conservation proof winner is invalid")
		}
		if _, validationErr := validateSchema010Conservation(stored); validationErr != nil || !sameSchema010ConservationInventory(proof, stored) {
			return schema010Conservation{}, domain.NewError(domain.ErrorInvalid, "schema-010 conservation proof winner differs")
		}
		return stored, nil
	}
	return proof, nil
}

func sameSchema010ConservationInventory(left, right schema010Conservation) bool {
	if left.FreezeEpoch != right.FreezeEpoch || left.SourceEntryCount != right.SourceEntryCount || left.RecoveredCount != right.RecoveredCount || left.PresentCount != right.PresentCount || left.Commitment != right.Commitment {
		return false
	}
	leftRoots, leftErr := storageformat.EncodeCanonical(left.Roots)
	rightRoots, rightErr := storageformat.EncodeCanonical(right.Roots)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRoots, rightRoots)
}

func (e *Engine) schema010ReceiptForEntry(ctx context.Context, root schema010ConservationRoot, entry storageformat.StateIndexEntry, freezeEpoch uint64) (schema010ConservationReceipt, error) {
	logical, err := parseExistingStateKey(entry.LogicalKey)
	if err != nil || entry.LogicalVersion == "" || stateNamespace(logical) != root.Namespace {
		return schema010ConservationReceipt{}, domain.NewError(domain.ErrorInvalid, "invalid schema-007 state-index entry during recovery")
	}
	versionKey := storageformat.StateVersionKey(root.Namespace, logical.String(), entry.LogicalVersion)
	object, err := e.backend.Get(ctx, versionKey)
	if err != nil {
		return schema010ConservationReceipt{}, err
	}
	var envelope storageformat.Envelope
	var record storageformat.StateVersionRecord
	if err := storageformat.DecodeEnvelope(object.Body, versionKey, stateVersionSchema, &envelope, &record); err != nil || record.SchemaVersion != 1 || record.LogicalKey != logical.String() || record.LogicalVersion != entry.LogicalVersion {
		return schema010ConservationReceipt{}, domain.NewError(domain.ErrorInvalid, "invalid schema-007 state-version record during recovery")
	}
	target, reference, recordType, targetValue, err := migrateStateEntry009(logical, record.Data)
	if err != nil {
		return schema010ConservationReceipt{}, err
	}
	if err := e.prepareSchema010Target(ctx, reference, freezeEpoch); err != nil {
		return schema010ConservationReceipt{}, err
	}
	disposition := schema010DispositionRecover
	snapshot, err := e.stateDomainStore().loadHead(ctx, reference)
	if err != nil {
		return schema010ConservationReceipt{}, err
	}
	if snapshot.exists && snapshot.head.Registered {
		current, found, err := e.stateDomainStore().lookupAtHead(ctx, reference, snapshot.head, target.String())
		if err != nil {
			return schema010ConservationReceipt{}, err
		}
		if found {
			if current.LogicalVersion != entry.LogicalVersion || !bytes.Equal(current.Data, targetValue) {
				return schema010ConservationReceipt{}, domain.NewError(domain.ErrorConflict, "schema-010 recovery target conflicts with indexed authority")
			}
			disposition = schema010DispositionAlreadyPresent
		}
	}
	return schema010ConservationReceipt{
		SchemaVersion: schema010ConservationSchema,
		SourceRootKey: root.RootKey, SourceRootDigest: root.RootDigest,
		SourceLogicalKey: logical.String(), SourceLogicalVersion: entry.LogicalVersion,
		SourceVersionKey: versionKey.String(), SourceVersionDigest: storageformat.Digest(object.Body),
		TargetDomainKind: reference.Kind, TargetDomainID: reference.ID, TargetKey: target.String(),
		TargetRecordType: recordType, TargetValue: targetValue, TargetValueDigest: storageformat.Digest(targetValue),
		Disposition: disposition,
	}, nil
}

func (e *Engine) prepareSchema010Target(ctx context.Context, reference consistencyDomainRef, freezeEpoch uint64) error {
	store := e.stateDomainStore()
	for range 16 {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil || !snapshot.exists || !snapshot.head.Registered {
			return err
		}
		if !snapshot.head.Frozen || snapshot.head.FreezeEpoch != freezeEpoch {
			return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 recovery target is not frozen")
		}
		if len(snapshot.head.Deltas) == 0 {
			return nil
		}
		if err := store.unfreeze(ctx, reference, freezeEpoch); err != nil {
			if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
				continue
			}
			return err
		}
		if err := store.compact(ctx, reference); err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		if err := store.freeze(ctx, reference, freezeEpoch); err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "schema-010 recovery target remained contended")
}

func (e *Engine) writeSchema010Receipt(ctx context.Context, receipt schema010ConservationReceipt, body []byte) (schema010ConservationReceipt, []byte, error) {
	reference := consistencyDomainRef{Kind: receipt.TargetDomainKind, ID: receipt.TargetDomainID}
	key := storageformat.Schema010MigrationReceiptKey(schema008DomainIdentity(reference), receipt.TargetKey)
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		return receipt, body, nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return schema010ConservationReceipt{}, nil, err
	}
	winner, err := e.backend.Get(ctx, key)
	if err != nil {
		return schema010ConservationReceipt{}, nil, err
	}
	var stored schema010ConservationReceipt
	if decodeCanonicalValue(winner.Body, &stored) != nil {
		return schema010ConservationReceipt{}, nil, domain.NewError(domain.ErrorInvalid, "schema-010 conservation receipt winner is invalid")
	}
	// Another migrator may have installed this exact value after writing its
	// receipt but before this replica classified the target. Disposition is a
	// staging observation, not source or target authority; every other binding
	// must still match byte-for-byte.
	receipt.Disposition = stored.Disposition
	_, _, normalized, validationErr := validateSchema010Receipt(receipt)
	if validationErr != nil || !bytes.Equal(winner.Body, normalized) {
		return schema010ConservationReceipt{}, nil, domain.NewError(domain.ErrorInvalid, "schema-010 conservation receipt winner differs")
	}
	return stored, winner.Body, nil
}

func (e *Engine) installSchema010Receipts(ctx context.Context, proof schema010Conservation) (storageformat.DomainTreeRoot, error) {
	if _, err := validateSchema010Conservation(proof); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	catalog := newDomainCatalog(e.backend, e.scheduler)
	catalogSnapshot, err := catalog.load(ctx)
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	if catalogSnapshot.head.Root != proof.SourceCatalog.Root && catalogSnapshot.head.FreezeEpoch == proof.FreezeEpoch {
		// A prior attempt may already have published the deterministic target.
		if err := e.verifySchema010ReceiptTargets(ctx, proof); err == nil {
			return catalogSnapshot.head.Root, nil
		}
		return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorPreconditionFailed, "schema-010 source catalog changed during recovery")
	}
	request := objectstore.ListRequest{Prefix: storageformat.Schema010MigrationReceiptPrefix(), Limit: 1000}
	currentGroup := ""
	var currentReference consistencyDomainRef
	var recovered *schema008MigrationRuns
	catalogAdditions := newSchema008MigrationRuns(ctx, newDomainCatalogTreeSession(e.stateDomainStore()))
	finish := func() error {
		if recovered == nil {
			return nil
		}
		root, err := recovered.Finish()
		if err != nil {
			return err
		}
		_, sourceCatalogued, err := catalog.entryAt(ctx, proof.SourceCatalog, currentReference)
		if err != nil {
			return err
		}
		newDomain, err := e.installSchema010Domain(ctx, currentReference, root, proof.FreezeEpoch, sourceCatalogued)
		if err != nil {
			return err
		}
		if !newDomain {
			return nil
		}
		entry := storageformat.DomainCatalogEntry{DomainID: currentReference.ID, Kind: currentReference.Kind, HeadKey: storageformat.DomainHeadKey(currentReference.Kind, currentReference.ID).String()}
		body, err := storageformat.EncodeCanonical(entry)
		if err != nil {
			return err
		}
		return catalogAdditions.Add(storageformat.DomainEntry{Key: catalogEntryKey(currentReference), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-domain-catalog-entry-v1\x00"), body...))})
	}
	previous := ""
	var receipts uint64
	for {
		page, err := e.backend.List(ctx, request)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		for _, info := range page.Objects {
			keyValue := info.Key.String()
			if !strings.HasPrefix(keyValue, request.Prefix) || previous != "" && keyValue <= previous {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid schema-010 receipt listing")
			}
			previous = keyValue
			relative := strings.TrimPrefix(keyValue, request.Prefix)
			separator := strings.IndexByte(relative, '/')
			if separator <= 0 {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid schema-010 receipt key")
			}
			group := relative[:separator]
			if currentGroup != "" && group != currentGroup {
				if err := finish(); err != nil {
					return storageformat.DomainTreeRoot{}, err
				}
				recovered = nil
			}
			object, err := e.backend.Get(ctx, info.Key)
			if err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
			var receipt schema010ConservationReceipt
			if decodeCanonicalValue(object.Body, &receipt) != nil {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid schema-010 receipt body")
			}
			reference, _, body, err := validateSchema010Receipt(receipt)
			if err != nil || !bytes.Equal(body, object.Body) || storageformat.Schema010MigrationReceiptKey(schema008DomainIdentity(reference), receipt.TargetKey) != info.Key {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "schema-010 receipt key binding mismatch")
			}
			if recovered == nil {
				currentGroup, currentReference = group, reference
				recovered = newSchema008MigrationRuns(ctx, newConsistencyDomainTreeSession(e.stateDomainStore(), reference))
			} else if reference != currentReference {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "schema-010 receipt domain digest collision")
			}
			if receipt.Disposition == schema010DispositionRecover {
				if err := recovered.Add(storageformat.DomainEntry{Key: receipt.TargetKey, Value: receipt.TargetValue, LogicalVersion: receipt.SourceLogicalVersion}); err != nil {
					return storageformat.DomainTreeRoot{}, err
				}
			}
			receipts++
		}
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
	}
	if receipts != proof.SourceEntryCount {
		return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "schema-010 receipt count does not match conservation proof")
	}
	if err := finish(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	additions, err := catalogAdditions.Finish()
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	if additions.Digest == "" {
		return proof.SourceCatalog.Root, nil
	}
	return mergeNamespaceProjectionRuns(ctx, newDomainCatalogTreeSession(e.stateDomainStore()), []storageformat.DomainTreeRoot{proof.SourceCatalog.Root, additions})
}

func (e *Engine) installSchema010Domain(ctx context.Context, reference consistencyDomainRef, recovered storageformat.DomainTreeRoot, freezeEpoch uint64, sourceCatalogued bool) (bool, error) {
	store := e.stateDomainStore()
	for range 16 {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil {
			return false, err
		}
		if snapshot.exists && snapshot.head.Registered {
			if !snapshot.head.Frozen || snapshot.head.FreezeEpoch != freezeEpoch || len(snapshot.head.Deltas) != 0 {
				return false, domain.NewError(domain.ErrorPreconditionFailed, "schema-010 target domain is not compact and frozen")
			}
			if !sourceCatalogued {
				if snapshot.head.Base == recovered && snapshot.head.Outcomes.Digest == "" && snapshot.head.OutcomeExpiry.Digest == "" {
					return true, nil
				}
				return false, domain.NewError(domain.ErrorInvalid, "schema-010 target domain appeared outside the source catalog")
			}
			if recovered.Digest == "" {
				return false, nil
			}
			if complete, err := e.schema010DomainContainsRecoveredRoot(ctx, store, reference, snapshot.head, recovered); err != nil {
				return false, err
			} else if complete {
				return false, nil
			}
			merged, err := mergeNamespaceProjectionRuns(ctx, newConsistencyDomainTreeSession(store, reference), []storageformat.DomainTreeRoot{snapshot.head.Base, recovered})
			if err != nil {
				return false, err
			}
			if snapshot.head.Base == merged {
				return false, nil
			}
			next := snapshot.head
			next.Revision++
			next.BaseRevision = next.Revision
			next.Base = merged
			key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
			body, err := store.encodeHead(key, snapshot, next)
			if err != nil {
				return false, err
			}
			if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}); err == nil {
				return false, nil
			} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
				return false, err
			}
			continue
		}
		if sourceCatalogued {
			return false, domain.NewError(domain.ErrorPreconditionFailed, "schema-010 source catalog names a missing target domain")
		}
		if recovered.Digest == "" {
			return false, nil
		}
		next := storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: 1, BaseRevision: 1, Frozen: true, FreezeEpoch: freezeEpoch, Base: recovered}
		key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
		body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, 1, next)
		if err != nil {
			return false, err
		}
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if snapshot.exists {
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}
		}
		if _, err := e.backend.Put(ctx, key, body, condition); err == nil {
			return true, nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return false, err
		}
	}
	return false, domain.NewError(domain.ErrorUnavailable, "schema-010 target domain remained contended")
}

func (e *Engine) schema010DomainContainsRecoveredRoot(ctx context.Context, store *consistencyDomainStore, reference consistencyDomainRef, head storageformat.DomainHead, recovered storageformat.DomainTreeRoot) (bool, error) {
	iterator, err := newConsistencyDomainTreeIterator(ctx, newConsistencyDomainTreeSession(store, reference), recovered)
	if err != nil {
		return false, err
	}
	for {
		entry, found, err := iterator.Next()
		if err != nil || !found {
			return found == false, err
		}
		current, present, err := store.lookupAtHead(ctx, reference, head, entry.Key)
		if err != nil {
			return false, err
		}
		if !present || current.LogicalVersion != entry.LogicalVersion || !bytes.Equal(current.Data, entry.Value) {
			return false, nil
		}
	}
}

func (e *Engine) publishSchema010Catalog(ctx context.Context, source storageformat.DomainCatalogHead, target storageformat.DomainTreeRoot, freezeEpoch uint64) error {
	catalog := newDomainCatalog(e.backend, e.scheduler)
	for range 16 {
		snapshot, err := catalog.load(ctx)
		if err != nil {
			return err
		}
		if snapshot.head.Root == target && snapshot.head.FreezeEpoch == freezeEpoch {
			return nil
		}
		if snapshot.head.Root != source.Root || snapshot.head.FreezeEpoch != freezeEpoch {
			return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 catalog source changed")
		}
		next := snapshot.head
		next.Revision++
		next.Root = target
		if err := catalog.publish(ctx, snapshot, next); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "schema-010 catalog remained contended")
}

func (e *Engine) verifySchema010Conservation(ctx context.Context, proof schema010Conservation) error {
	if _, err := validateSchema010Conservation(proof); err != nil {
		return err
	}
	if err := e.verifySchema010ReceiptTargets(ctx, proof); err != nil {
		return err
	}
	var sourceCount uint64
	for _, rootProof := range proof.Roots {
		key, err := objectstore.ParseKey(rootProof.RootKey)
		if err != nil {
			return err
		}
		object, err := e.backend.Get(ctx, key)
		if err != nil {
			return err
		}
		if storageformat.Digest(object.Body) != rootProof.RootDigest {
			return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 recovery source changed before activation")
		}
		var envelope storageformat.Envelope
		var root storageformat.StateIndexRoot
		if err := storageformat.DecodeEnvelope(object.Body, key, stateIndexRootSchema, &envelope, &root); err != nil || root.Namespace != rootProof.Namespace || root.EntryCount != rootProof.EntryCount {
			return domain.NewError(domain.ErrorInvalid, "schema-010 recovery source root no longer matches proof")
		}
		rootCommitment := sha256.New()
		var observed uint64
		after := ""
		for {
			entries, err := e.collectStateIndexEntries(ctx, root, root.Namespace+"/", after, 256)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				receipt, err := e.schema010ReceiptForEntry(ctx, rootProof, entry, proof.FreezeEpoch)
				if err != nil {
					return err
				}
				reference := consistencyDomainRef{Kind: receipt.TargetDomainKind, ID: receipt.TargetDomainID}
				receiptKey := storageformat.Schema010MigrationReceiptKey(schema008DomainIdentity(reference), receipt.TargetKey)
				stored, err := e.backend.Get(ctx, receiptKey)
				if err != nil {
					return err
				}
				var storedReceipt schema010ConservationReceipt
				if decodeCanonicalValue(stored.Body, &storedReceipt) != nil {
					return domain.NewError(domain.ErrorInvalid, "invalid stored schema-010 conservation receipt")
				}
				// Installation turns a formerly missing value into a present one.
				// Preserve the durable staging disposition while independently
				// re-deriving every source and target binding.
				receipt.Disposition = storedReceipt.Disposition
				reference, _, expected, err := validateSchema010Receipt(receipt)
				if err != nil {
					return err
				}
				if !bytes.Equal(stored.Body, expected) {
					return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 conservation receipt is missing or changed")
				}
				writeSchema010Commitment(rootCommitment, receipt.SourceLogicalKey, receipt.SourceLogicalVersion, storageformat.Digest(expected))
				observed++
				after = entry.LogicalKey
			}
			if len(entries) < 256 {
				break
			}
		}
		if observed != rootProof.EntryCount || hex.EncodeToString(rootCommitment.Sum(nil)) != rootProof.ReceiptCommitment {
			return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 source-to-receipt conservation mismatch")
		}
		sourceCount += observed
	}
	if sourceCount != proof.SourceEntryCount {
		return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 verified source count mismatch")
	}
	return nil
}

func (e *Engine) verifySchema010ReceiptTargets(ctx context.Context, proof schema010Conservation) error {
	catalog := newDomainCatalog(e.backend, e.scheduler)
	snapshot, err := catalog.load(ctx)
	if err != nil {
		return err
	}
	if snapshot.head.FreezeEpoch != proof.FreezeEpoch {
		return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 target catalog is not frozen")
	}
	request := objectstore.ListRequest{Prefix: storageformat.Schema010MigrationReceiptPrefix(), Limit: 1000}
	var count uint64
	for {
		page, err := e.backend.List(ctx, request)
		if err != nil {
			return err
		}
		for _, info := range page.Objects {
			object, err := e.backend.Get(ctx, info.Key)
			if err != nil {
				return err
			}
			var receipt schema010ConservationReceipt
			if decodeCanonicalValue(object.Body, &receipt) != nil {
				return domain.NewError(domain.ErrorInvalid, "invalid schema-010 conservation receipt during verification")
			}
			reference, _, body, err := validateSchema010Receipt(receipt)
			if err != nil || !bytes.Equal(body, object.Body) || storageformat.Schema010MigrationReceiptKey(schema008DomainIdentity(reference), receipt.TargetKey) != info.Key {
				return domain.NewError(domain.ErrorInvalid, "misbound schema-010 conservation receipt during verification")
			}
			if _, found, err := catalog.entryAt(ctx, snapshot.head, reference); err != nil {
				return err
			} else if !found {
				return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 target domain is absent from catalog")
			}
			head, err := e.stateDomainStore().loadHead(ctx, reference)
			if err != nil {
				return err
			}
			if !head.exists || !head.head.Registered || !head.head.Frozen || head.head.FreezeEpoch != proof.FreezeEpoch {
				return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 target domain is not frozen")
			}
			value, found, err := e.stateDomainStore().lookupAtHead(ctx, reference, head.head, receipt.TargetKey)
			if err != nil {
				return err
			}
			if !found || value.LogicalVersion != receipt.SourceLogicalVersion || !bytes.Equal(value.Data, receipt.TargetValue) {
				return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 recovered target does not match source")
			}
			count++
		}
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
	}
	if count != proof.SourceEntryCount {
		return domain.NewError(domain.ErrorPreconditionFailed, "schema-010 target receipt count mismatch")
	}
	return nil
}

func writeSchema010Commitment(target hash.Hash, values ...string) {
	for _, value := range values {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = target.Write(length[:])
		_, _ = target.Write([]byte(value))
	}
}
