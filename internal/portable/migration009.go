package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const schema009MigrationStageSchema = 1

type schema009MigrationStage struct {
	SchemaVersion  int                                 `json:"schemaVersion"`
	SourceIdentity string                              `json:"sourceIdentity"`
	DomainKind     storageformat.ConsistencyDomainKind `json:"domainKind"`
	DomainID       string                              `json:"domainID"`
	Tree           string                              `json:"tree"`
	Key            string                              `json:"key"`
	Value          []byte                              `json:"value"`
	LogicalVersion string                              `json:"logicalVersion"`
}

type schema009MigrationStagingComplete struct {
	SchemaVersion int                             `json:"schemaVersion"`
	FreezeEpoch   uint64                          `json:"freezeEpoch"`
	SourceCatalog storageformat.DomainCatalogHead `json:"sourceCatalog"`
}

func validateSchema009MigrationStage(stage schema009MigrationStage) (consistencyDomainRef, []byte, error) {
	reference := consistencyDomainRef{Kind: stage.DomainKind, ID: stage.DomainID}
	if stage.SchemaVersion != schema009MigrationStageSchema || stage.SourceIdentity == "" || stage.Key == "" || stage.LogicalVersion == "" || stage.Tree != "base" && stage.Tree != "outcomes" && stage.Tree != "outcome-expiry" || validateConsistencyDomainRef(reference) != nil {
		return consistencyDomainRef{}, nil, domain.NewError(domain.ErrorInvalid, "invalid schema-009 migration stage")
	}
	body, err := storageformat.EncodeCanonical(stage)
	if err != nil {
		return consistencyDomainRef{}, nil, err
	}
	return reference, body, nil
}

func schema009StringField(payload []byte, name string) (string, error) {
	var fields map[string]json.RawMessage
	if err := state.DecodeJSONWithLimit(payload, &fields, state.MaxRecordBytes); err != nil {
		return "", err
	}
	raw, found := fields[name]
	if !found {
		return "", domain.NewError(domain.ErrorInvalid, "schema-008 state record is missing migration binding")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", domain.NewError(domain.ErrorInvalid, "schema-008 state record has invalid migration binding")
	}
	return value, nil
}

func schema009OptionalStringField(payload []byte, name string) (string, bool, error) {
	var fields map[string]json.RawMessage
	if err := state.DecodeJSONWithLimit(payload, &fields, state.MaxRecordBytes); err != nil {
		return "", false, err
	}
	raw, found := fields[name]
	if !found || string(raw) == "null" {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", false, domain.NewError(domain.ErrorInvalid, "schema-008 state record has invalid optional migration binding")
	}
	return value, true, nil
}

func stateRecordType009(namespace state.Namespace, parts []string) (string, error) {
	switch namespace {
	case state.NamespaceUsers:
		return storageformat.StateRecordProfile, nil
	case state.NamespaceAccounts:
		return storageformat.StateRecordAccount, nil
	case state.NamespaceCredentials:
		if len(parts) == 2 && parts[1] == "index" {
			return storageformat.StateRecordCredentialIndex, nil
		}
		return storageformat.StateRecordCredential, nil
	case state.NamespaceCeremonies:
		return storageformat.StateRecordCeremony, nil
	case state.NamespaceSessions:
		return storageformat.StateRecordSession, nil
	case state.NamespaceInvites:
		return storageformat.StateRecordInvite, nil
	case state.NamespaceRecoveries:
		return storageformat.StateRecordRecovery, nil
	case state.NamespaceShares:
		return storageformat.StateRecordShare, nil
	case state.NamespaceTrash:
		return storageformat.StateRecordTrash, nil
	case state.NamespaceUploads:
		return storageformat.StateRecordUpload, nil
	case state.NamespaceBootstrap:
		if len(parts) == 1 && parts[0] == "first-account" {
			return storageformat.StateRecordFirstAccount, nil
		}
		return storageformat.StateRecordBootstrap, nil
	case state.NamespaceRoles:
		return storageformat.StateRecordAdminRoles, nil
	case state.NamespacePreferences:
		return storageformat.StateRecordThemePreference, nil
	case state.NamespaceIdempotency:
		if len(parts) > 0 && parts[0] == "preview" {
			return storageformat.StateRecordPreviewIdempotency, nil
		}
		return storageformat.StateRecordIdempotency, nil
	case state.NamespaceOperations:
		if len(parts) > 0 {
			switch parts[0] {
			case "preview":
				return storageformat.StateRecordPreviewOperation, nil
			case "preview-index":
				return storageformat.StateRecordPreviewIndex, nil
			case "batch":
				return storageformat.StateRecordBatchOperation, nil
			case "identity":
				if len(parts) > 3 && parts[2] == "authentication" {
					return storageformat.StateRecordAuthenticationOperation, nil
				}
				return storageformat.StateRecordRegistrationOperation, nil
			case "admin":
				return storageformat.StateRecordMutationOutcome, nil
			}
		}
	}
	return "", domain.NewError(domain.ErrorInvalid, "state key has no schema-009 record type")
}

// migrateStateEntry009 is frozen into the 008 -> 009 edge. It relocates old
// capability-sharded records using owner bindings from their canonical bodies,
// then wraps the unchanged application bytes in the typed schema-009 envelope.
func migrateStateEntry009(key state.Key, payload []byte) (state.Key, consistencyDomainRef, string, []byte, error) {
	namespace, parts, err := decodedStatePath(key.String(), false)
	if err != nil {
		return state.Key{}, consistencyDomainRef{}, "", nil, err
	}
	targetParts := append([]string(nil), parts...)
	switch namespace {
	case state.NamespaceCredentials:
		switch {
		case len(parts) == 2 && parts[0] == "user-index":
			targetParts = []string{parts[1], "index"}
		case len(parts) == 1:
			owner, fieldErr := schema009StringField(payload, "userID")
			if fieldErr != nil {
				return state.Key{}, consistencyDomainRef{}, "", nil, fieldErr
			}
			targetParts = []string{owner, parts[0]}
		}
	case state.NamespaceSessions:
		if len(parts) == 1 {
			owner, fieldErr := schema009StringField(payload, "userID")
			if fieldErr != nil {
				return state.Key{}, consistencyDomainRef{}, "", nil, fieldErr
			}
			targetParts = []string{owner, parts[0]}
		}
	case state.NamespaceCeremonies:
		if len(parts) == 1 {
			owner, found, fieldErr := schema009OptionalStringField(payload, "userID")
			if fieldErr != nil {
				return state.Key{}, consistencyDomainRef{}, "", nil, fieldErr
			}
			if found {
				targetParts = []string{"owner", owner, parts[0]}
			} else {
				targetParts = []string{"capability", parts[0]}
			}
		}
	case state.NamespaceRecoveries:
		if len(parts) == 1 {
			owner, fieldErr := schema009StringField(payload, "targetUserID")
			if fieldErr != nil {
				return state.Key{}, consistencyDomainRef{}, "", nil, fieldErr
			}
			targetParts = []string{owner, parts[0]}
		}
	case state.NamespaceShares:
		if len(parts) == 1 {
			owner, ownerErr := schema009StringField(payload, "ownerUserID")
			shareID, shareErr := schema009StringField(payload, "shareID")
			if ownerErr != nil || shareErr != nil {
				return state.Key{}, consistencyDomainRef{}, "", nil, domain.NewError(domain.ErrorInvalid, "schema-008 share migration binding is invalid")
			}
			targetParts = []string{owner, shareID}
		}
	case state.NamespaceIdempotency:
		if len(parts) == 2 && parts[0] != "preview" && parts[0] != "drive" && parts[0] != "identity" {
			targetParts = []string{"identity", parts[0], parts[1]}
		}
	case state.NamespaceOperations:
		if len(parts) == 2 && parts[0] == "registration" {
			owner, fieldErr := schema009StringField(payload, "userID")
			if fieldErr != nil {
				return state.Key{}, consistencyDomainRef{}, "", nil, fieldErr
			}
			targetParts = []string{"identity", owner, parts[1]}
		}
	}
	target, err := state.NewKey(namespace, targetParts...)
	if err != nil {
		return state.Key{}, consistencyDomainRef{}, "", nil, err
	}
	reference, err := stateDomainReferenceForKey009(target)
	if err != nil {
		return state.Key{}, consistencyDomainRef{}, "", nil, err
	}
	recordType, err := stateRecordType009(namespace, targetParts)
	if err != nil {
		return state.Key{}, consistencyDomainRef{}, "", nil, err
	}
	body, err := storageformat.EncodeStateRecord009(recordType, payload)
	if err != nil {
		return state.Key{}, consistencyDomainRef{}, "", nil, err
	}
	return target, reference, recordType, body, nil
}

func (e *Engine) runStorageMigration008To009(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
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
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageGateClosed})
	if err := e.step(ctx, MigrationStepName(string(transition.id), StepMigrationAfterGateClosed)); err != nil {
		return err
	}
	staging, err := e.stageSchema008Domains009(ctx, gate.Epoch)
	if err != nil {
		return domain.WrapError(domain.KindOf(err), "stage schema-008 consistency domains", err)
	}
	targetRoot, err := e.installSchema009StagedDomains(ctx, gate.Epoch)
	if err != nil {
		return domain.WrapError(domain.KindOf(err), "install schema-009 consistency domains", err)
	}
	if err := e.retireSchema008DomainHeads009(ctx, staging.SourceCatalog, targetRoot, gate.Epoch); err != nil {
		return domain.WrapError(domain.KindOf(err), "retire schema-008 consistency domains", err)
	}
	if err := e.publishSchema009Catalog(ctx, targetRoot, gate.Epoch); err != nil {
		return domain.WrapError(domain.KindOf(err), "publish schema-009 consistency-domain catalog", err)
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
		return domain.WrapError(domain.KindOf(err), "create schema-009 migration checkpoint", err)
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
		return domain.WrapError(domain.KindOf(err), "unfreeze migrated schema-009 domains", err)
	}
	e.observeMigration(MigrationProgress{MigrationID: transition.id.String(), Stage: MigrationStageComplete})
	return nil
}

func (e *Engine) readSchema009StagingComplete(ctx context.Context) (schema009MigrationStagingComplete, bool, error) {
	object, err := e.backend.Get(ctx, storageformat.Schema009MigrationStageCompleteKey())
	if errors.Is(err, domain.ErrNotFound) {
		return schema009MigrationStagingComplete{}, false, nil
	}
	if err != nil {
		return schema009MigrationStagingComplete{}, false, err
	}
	var marker schema009MigrationStagingComplete
	if decodeCanonicalValue(object.Body, &marker) != nil || marker.SchemaVersion != 1 || marker.FreezeEpoch == 0 || storageformat.ValidateDomainCatalogHead(marker.SourceCatalog) != nil || marker.SourceCatalog.FreezeEpoch != marker.FreezeEpoch {
		return schema009MigrationStagingComplete{}, false, domain.NewError(domain.ErrorInvalid, "invalid schema-009 migration staging marker")
	}
	return marker, true, nil
}

func (e *Engine) stageSchema008Domains009(ctx context.Context, freezeEpoch uint64) (schema009MigrationStagingComplete, error) {
	if marker, found, err := e.readSchema009StagingComplete(ctx); err != nil || found {
		return marker, err
	}
	catalog := newDomainCatalog(e.backend, e.scheduler)
	entries, err := catalog.freeze(ctx, freezeEpoch)
	if err != nil {
		return schema009MigrationStagingComplete{}, err
	}
	for _, entry := range entries {
		reference := consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}
		for {
			source, loadErr := catalog.store.loadHead(ctx, reference)
			if loadErr != nil {
				return schema009MigrationStagingComplete{}, loadErr
			}
			if !source.exists || !source.head.Registered {
				return schema009MigrationStagingComplete{}, domain.NewError(domain.ErrorPreconditionFailed, "schema-008 catalog names a missing domain")
			}
			if source.head.Frozen {
				if source.head.FreezeEpoch != freezeEpoch {
					return schema009MigrationStagingComplete{}, domain.NewError(domain.ErrorPreconditionFailed, "schema-008 migration source froze at another epoch")
				}
				if len(source.head.Deltas) != 0 {
					// The ordinary schema-008 closure freezes without compaction.
					// With the write gate closed and catalog still frozen, migration
					// can safely reopen only this head, compact it, and refreeze it.
					if unfreezeErr := catalog.store.unfreeze(ctx, reference, freezeEpoch); unfreezeErr != nil {
						return schema009MigrationStagingComplete{}, unfreezeErr
					}
					continue
				}
				break
			}
			if len(source.head.Deltas) != 0 {
				if compactErr := catalog.store.compactSnapshot(ctx, reference, source); compactErr != nil && !errors.Is(compactErr, domain.ErrConflict) && !errors.Is(compactErr, domain.ErrPreconditionFailed) {
					return schema009MigrationStagingComplete{}, compactErr
				}
				continue
			}
			if freezeErr := catalog.store.freeze(ctx, reference, freezeEpoch); freezeErr != nil {
				return schema009MigrationStagingComplete{}, freezeErr
			}
		}
	}
	snapshot, err := catalog.load(ctx)
	if err != nil {
		return schema009MigrationStagingComplete{}, err
	}
	if snapshot.head.FreezeEpoch != freezeEpoch {
		return schema009MigrationStagingComplete{}, domain.NewError(domain.ErrorPreconditionFailed, "schema-008 source catalog is not frozen")
	}
	if err := catalog.visitEntries(ctx, snapshot.head, func(entry storageformat.DomainCatalogEntry) error {
		return e.stageSchema008Domain009(ctx, consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}, freezeEpoch)
	}); err != nil {
		return schema009MigrationStagingComplete{}, err
	}
	marker := schema009MigrationStagingComplete{SchemaVersion: 1, FreezeEpoch: freezeEpoch, SourceCatalog: snapshot.head}
	body, err := storageformat.EncodeCanonical(marker)
	if err != nil {
		return schema009MigrationStagingComplete{}, err
	}
	key := storageformat.Schema009MigrationStageCompleteKey()
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return schema009MigrationStagingComplete{}, err
		}
		winner, getErr := e.backend.Get(ctx, key)
		if getErr != nil || !bytes.Equal(winner.Body, body) {
			return schema009MigrationStagingComplete{}, domain.NewError(domain.ErrorInvalid, "schema-009 staging marker winner differs")
		}
	}
	return marker, nil
}

func (e *Engine) stageSchema008Domain009(ctx context.Context, reference consistencyDomainRef, freezeEpoch uint64) error {
	snapshot, err := e.stateDomainStore().loadHead(ctx, reference)
	if err != nil {
		return err
	}
	if !snapshot.exists || !snapshot.head.Registered || !snapshot.head.Frozen || snapshot.head.FreezeEpoch != freezeEpoch {
		return domain.NewError(domain.ErrorPreconditionFailed, "schema-008 migration source domain is not frozen")
	}
	session := newConsistencyDomainTreeSession(e.stateDomainStore(), reference)
	stageTree := func(tree string, root storageformat.DomainTreeRoot, preserveOnly bool) error {
		iterator, err := newConsistencyDomainTreeIterator(ctx, session, root)
		if err != nil {
			return err
		}
		for {
			entry, found, err := iterator.Next()
			if err != nil || !found {
				return err
			}
			targetReference, targetKey, targetValue := reference, entry.Key, entry.Value
			if tree == "base" {
				if logical, parseErr := parseExistingStateKey(entry.Key); parseErr == nil {
					target, migratedReference, _, body, migrationErr := migrateStateEntry009(logical, entry.Value)
					if migrationErr != nil {
						return migrationErr
					}
					targetReference, targetKey, targetValue = migratedReference, target.String(), body
				} else if reference.Kind != storageformat.DomainNamespace {
					return domain.NewError(domain.ErrorInvalid, "schema-008 state domain contains a non-state key")
				}
			} else if !preserveOnly {
				continue
			}
			sourceIdentity := storageformat.Digest([]byte(catalogEntryKey(reference) + "\x00" + tree + "\x00" + entry.Key))
			stage := schema009MigrationStage{SchemaVersion: 1, SourceIdentity: sourceIdentity, DomainKind: targetReference.Kind, DomainID: targetReference.ID, Tree: tree, Key: targetKey, Value: targetValue, LogicalVersion: entry.LogicalVersion}
			if err := e.writeSchema009MigrationStage(ctx, stage); err != nil {
				return err
			}
		}
	}
	if err := stageTree("base", snapshot.head.Base, reference.Kind == storageformat.DomainNamespace); err != nil {
		return err
	}
	if reference.Kind == storageformat.DomainNamespace {
		if err := stageTree("outcomes", snapshot.head.Outcomes, true); err != nil {
			return err
		}
		if err := stageTree("outcome-expiry", snapshot.head.OutcomeExpiry, true); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) writeSchema009MigrationStage(ctx context.Context, stage schema009MigrationStage) error {
	reference, body, err := validateSchema009MigrationStage(stage)
	if err != nil {
		return err
	}
	key := storageformat.Schema009MigrationStageKey(schema008DomainIdentity(reference), stage.SourceIdentity)
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	winner, err := e.backend.Get(ctx, key)
	if err != nil || !bytes.Equal(winner.Body, body) {
		return domain.NewError(domain.ErrorInvalid, "schema-009 migration stage winner differs")
	}
	return nil
}

func (e *Engine) installSchema009StagedDomains(ctx context.Context, freezeEpoch uint64) (storageformat.DomainTreeRoot, error) {
	prefix := storageformat.Schema009MigrationStagePrefix()
	request := objectstore.ListRequest{Prefix: prefix, Limit: 1000}
	currentGroup := ""
	var currentRef consistencyDomainRef
	type domainRuns struct{ base, outcomes, expiry *schema008MigrationRuns }
	var runs *domainRuns
	catalogRuns := newSchema008MigrationRuns(ctx, newDomainCatalogTreeSession(e.stateDomainStore()))
	finish := func() error {
		if runs == nil {
			return nil
		}
		base, err := runs.base.Finish()
		if err != nil {
			return err
		}
		outcomes, err := runs.outcomes.Finish()
		if err != nil {
			return err
		}
		expiry, err := runs.expiry.Finish()
		if err != nil {
			return err
		}
		if err := e.installSchema009Domain(ctx, currentRef, base, outcomes, expiry, freezeEpoch); err != nil {
			return err
		}
		entry := storageformat.DomainCatalogEntry{DomainID: currentRef.ID, Kind: currentRef.Kind, HeadKey: storageformat.DomainHeadKey(currentRef.Kind, currentRef.ID).String()}
		body, err := storageformat.EncodeCanonical(entry)
		if err != nil {
			return err
		}
		return catalogRuns.Add(storageformat.DomainEntry{Key: catalogEntryKey(currentRef), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-domain-catalog-entry-v1\x00"), body...))})
	}
	previous := ""
	for {
		page, err := e.backend.List(ctx, request)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		for _, info := range page.Objects {
			key := info.Key.String()
			if !strings.HasPrefix(key, prefix) || previous != "" && key <= previous {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid schema-009 migration stage listing")
			}
			previous = key
			relative := strings.TrimPrefix(key, prefix)
			separator := strings.IndexByte(relative, '/')
			if separator <= 0 {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid schema-009 migration stage key")
			}
			group := relative[:separator]
			if currentGroup != "" && group != currentGroup {
				if err := finish(); err != nil {
					return storageformat.DomainTreeRoot{}, err
				}
				runs = nil
			}
			object, err := e.backend.Get(ctx, info.Key)
			if err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
			var stage schema009MigrationStage
			if decodeCanonicalValue(object.Body, &stage) != nil {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid schema-009 migration stage body")
			}
			reference, _, err := validateSchema009MigrationStage(stage)
			if err != nil || storageformat.Schema009MigrationStageKey(schema008DomainIdentity(reference), stage.SourceIdentity) != info.Key {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "schema-009 migration stage key binding mismatch")
			}
			if currentGroup == "" || group != currentGroup {
				currentGroup, currentRef = group, reference
				runs = &domainRuns{
					base:     newSchema008MigrationRuns(ctx, newConsistencyDomainTreeSession(e.stateDomainStore(), reference)),
					outcomes: newSchema008MigrationRuns(ctx, newConsistencyDomainTreeSession(e.stateDomainStore(), reference)),
					expiry:   newSchema008MigrationRuns(ctx, newConsistencyDomainTreeSession(e.stateDomainStore(), reference)),
				}
			} else if reference != currentRef {
				return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "schema-009 migration domain digest collision")
			}
			target := runs.base
			if stage.Tree == "outcomes" {
				target = runs.outcomes
			} else if stage.Tree == "outcome-expiry" {
				target = runs.expiry
			}
			if err := target.Add(storageformat.DomainEntry{Key: stage.Key, Value: stage.Value, LogicalVersion: stage.LogicalVersion}); err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
		}
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
	}
	if err := finish(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	return catalogRuns.Finish()
}

func (e *Engine) installSchema009Domain(ctx context.Context, reference consistencyDomainRef, base, outcomes, expiry storageformat.DomainTreeRoot, freezeEpoch uint64) error {
	store := e.stateDomainStore()
	key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
	for range 16 {
		snapshot, err := store.loadHead(ctx, reference)
		if err != nil {
			return err
		}
		if snapshot.exists && snapshot.head.Registered && snapshot.head.Base == base && snapshot.head.Outcomes == outcomes && snapshot.head.OutcomeExpiry == expiry && snapshot.head.Frozen && snapshot.head.FreezeEpoch == freezeEpoch && len(snapshot.head.Deltas) == 0 {
			return nil
		}
		nextRevision := uint64(1)
		if snapshot.exists && snapshot.head.Registered {
			if !snapshot.head.Frozen || snapshot.head.FreezeEpoch != freezeEpoch {
				return domain.NewError(domain.ErrorPreconditionFailed, "schema-009 target domain is not frozen")
			}
			nextRevision = snapshot.head.Revision + 1
		}
		next := storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind, Registered: true, Revision: nextRevision, BaseRevision: nextRevision, Frozen: true, FreezeEpoch: freezeEpoch, Base: base, Outcomes: outcomes, OutcomeExpiry: expiry}
		body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, snapshot.envelope.Revision+1, next)
		if err != nil {
			return err
		}
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if snapshot.exists {
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}
		}
		if _, err := e.backend.Put(ctx, key, body, condition); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "schema-009 target domain remained contended")
}

func (e *Engine) retireSchema008DomainHeads009(ctx context.Context, source storageformat.DomainCatalogHead, target storageformat.DomainTreeRoot, freezeEpoch uint64) error {
	catalogSession := newDomainCatalogTreeSession(e.stateDomainStore())
	return newDomainCatalog(e.backend, e.scheduler).visitEntries(ctx, source, func(entry storageformat.DomainCatalogEntry) error {
		reference := consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}
		if _, found, err := catalogSession.lookup(ctx, target, catalogEntryKey(reference)); err != nil || found {
			return err
		}
		store := e.stateDomainStore()
		for range 16 {
			snapshot, err := store.loadHead(ctx, reference)
			if err != nil {
				return err
			}
			if !snapshot.exists || !snapshot.head.Registered {
				return nil
			}
			if !snapshot.head.Frozen || snapshot.head.FreezeEpoch != freezeEpoch {
				return domain.NewError(domain.ErrorPreconditionFailed, "schema-008 retired domain is not frozen")
			}
			next := storageformat.DomainHead{SchemaVersion: 1, DomainID: reference.ID, Kind: reference.Kind}
			key := storageformat.DomainHeadKey(reference.Kind, reference.ID)
			body, err := storageformat.EncodeEnvelope(domainHeadSchema, key, snapshot.envelope.Revision+1, next)
			if err != nil {
				return err
			}
			if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}); err == nil {
				return nil
			} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
				return err
			}
		}
		return domain.NewError(domain.ErrorUnavailable, "schema-008 retired domain remained contended")
	})
}

func (e *Engine) publishSchema009Catalog(ctx context.Context, root storageformat.DomainTreeRoot, freezeEpoch uint64) error {
	catalog := newDomainCatalog(e.backend, e.scheduler)
	for range 16 {
		snapshot, err := catalog.load(ctx)
		if err != nil {
			return err
		}
		if snapshot.head.Root == root && snapshot.head.FreezeEpoch == freezeEpoch {
			return nil
		}
		if snapshot.head.FreezeEpoch != freezeEpoch {
			return domain.NewError(domain.ErrorPreconditionFailed, "schema-009 catalog source is not frozen")
		}
		next := snapshot.head
		next.Revision++
		next.Root = root
		if err := catalog.publish(ctx, snapshot, next); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "schema-009 catalog remained contended")
}
