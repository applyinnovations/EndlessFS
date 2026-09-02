package portable

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash"
	"math"
	"reflect"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	checkpointGarbagePlanSchema       = "checkpoint-garbage-plan-v1"
	checkpointGarbagePlanPageSchema   = "checkpoint-garbage-plan-page-v1"
	checkpointGarbagePlanPageEntries  = 128
	checkpointGarbagePlanSchemaNumber = 1
)

type checkpointGarbagePlanBuilder struct {
	engine          *Engine
	checkpointID    string
	gateEpoch       uint64
	inventoryDigest string
	digest          hash.Hash
	rootEntries     []storageformat.GarbageCollectionEntry
	pageEntries     []storageformat.GarbageCollectionEntry
	pageCount       uint64
	entryCount      uint64
	previous        string
	versions        map[string]objectstore.ObjectInfo
}

// prepareCheckpointGarbagePlan performs the sweep discovery while the
// checkpoint gate is already closed and its exact reachability set is still
// available locally. The durable plan contains only portable roles and keys;
// native generations remain process-local and are discarded after the sweep.
func (e *Engine) prepareCheckpointGarbagePlan(ctx context.Context, checkpointID string, gateEpoch uint64, inventoryDigest string, reachable *checkpointVisitSet) (map[string]objectstore.ObjectInfo, error) {
	if checkpointID == "" || inventoryDigest == "" || reachable == nil {
		return nil, domain.NewError(domain.ErrorInvalid, "checkpoint garbage plan requires a frozen inventory")
	}
	key := storageformat.GarbageCollectionPlanKey(checkpointID)
	if object, err := e.backend.Get(ctx, key); err == nil {
		plan, decodeErr := decodeCheckpointGarbagePlan(object, checkpointID, gateEpoch, inventoryDigest)
		if decodeErr != nil {
			return nil, decodeErr
		}
		checkpoint := storageformat.Checkpoint{SchemaVersion: 3, CheckpointID: checkpointID, GateEpoch: gateEpoch, InventoryDigest: inventoryDigest}
		if err := e.validateCheckpointGarbagePlan(ctx, checkpoint, plan); err != nil {
			return nil, err
		}
		return nil, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	builder := &checkpointGarbagePlanBuilder{
		engine: e, checkpointID: checkpointID, gateEpoch: gateEpoch, inventoryDigest: inventoryDigest,
		digest: checkpointGarbageEntriesDigest(), rootEntries: make([]storageformat.GarbageCollectionEntry, 0, checkpointGarbagePlanPageEntries),
		pageEntries: make([]storageformat.GarbageCollectionEntry, 0, checkpointGarbagePlanPageEntries),
		pageCount:   1, versions: make(map[string]objectstore.ObjectInfo, checkpointGarbagePlanPageEntries),
	}
	collect := func(backend objectstore.MetadataBackend, role string, eligible func(objectstore.Key) bool) error {
		return walkObjectInfos(ctx, backend, "endlessfs/v1/", func(info objectstore.ObjectInfo) error {
			if !eligible(info.Key) {
				return nil
			}
			seen, err := reachable.Seen(role + "\x00" + info.Key.String())
			if err != nil || seen {
				return err
			}
			return builder.add(ctx, storageformat.GarbageCollectionEntry{Role: role, Key: info.Key.String()}, info)
		})
	}
	stateEligible := func(key objectstore.Key) bool {
		return checkpointDomainGarbageEligible(key) || strings.HasPrefix(key.String(), storageformat.TransitionPrefix()) && checkpointJSONGarbageEligible(key) || strings.HasPrefix(key.String(), storageformat.ProjectionPrefix()) && checkpointJSONGarbageEligible(key) || strings.HasPrefix(key.String(), storageformat.LeasePrefix()) && checkpointJSONGarbageEligible(key)
	}
	if err := collect(e.backend, garbageCollectionStateRole, stateEligible); err != nil {
		return nil, err
	}
	if err := collect(e.fileBackend, garbageCollectionFileRole, checkpointBlobGarbageEligible); err != nil {
		return nil, err
	}
	if err := builder.flushPage(ctx); err != nil {
		return nil, err
	}
	plan := storageformat.GarbageCollectionPlan{
		SchemaVersion: checkpointGarbagePlanSchemaNumber, CheckpointID: checkpointID, GateEpoch: gateEpoch,
		InventoryDigest: inventoryDigest, PageCount: builder.pageCount, EntryCount: builder.entryCount,
		EntriesDigest: base64.RawURLEncoding.EncodeToString(builder.digest.Sum(nil)), Entries: builder.rootEntries,
	}
	body, err := storageformat.EncodeEnvelope(checkpointGarbagePlanSchema, key, 1, plan)
	if err != nil {
		return nil, err
	}
	if _, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return nil, err
		}
		existing, getErr := e.backend.Get(ctx, key)
		if getErr != nil || !reflect.DeepEqual(existing.Body, body) {
			return nil, domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan conflict")
		}
	}
	return builder.versions, nil
}

func (builder *checkpointGarbagePlanBuilder) add(ctx context.Context, entry storageformat.GarbageCollectionEntry, info objectstore.ObjectInfo) error {
	if err := validateCheckpointGarbageEntry(entry); err != nil {
		return err
	}
	identity := checkpointGarbageEntryIdentity(entry)
	if builder.previous != "" && identity <= builder.previous {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan is not ordered")
	}
	builder.previous = identity
	if err := writeCheckpointGarbageDigestEntry(builder.digest, entry); err != nil {
		return err
	}
	if builder.entryCount == math.MaxUint64 {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan count overflow")
	}
	builder.entryCount++
	if len(builder.rootEntries) < checkpointGarbagePlanPageEntries {
		builder.rootEntries = append(builder.rootEntries, entry)
	} else {
		builder.pageEntries = append(builder.pageEntries, entry)
		if len(builder.pageEntries) == checkpointGarbagePlanPageEntries {
			if err := builder.flushPage(ctx); err != nil {
				return err
			}
		}
	}
	if builder.versions != nil {
		if builder.entryCount > checkpointGarbagePlanPageEntries {
			builder.versions = nil
		} else {
			builder.versions[identity] = info
		}
	}
	return nil
}

func (builder *checkpointGarbagePlanBuilder) flushPage(ctx context.Context) error {
	if len(builder.pageEntries) == 0 {
		return nil
	}
	page := storageformat.GarbageCollectionPlanPage{
		SchemaVersion: checkpointGarbagePlanSchemaNumber, CheckpointID: builder.checkpointID, GateEpoch: builder.gateEpoch,
		InventoryDigest: builder.inventoryDigest, Index: builder.pageCount,
		Entries: append([]storageformat.GarbageCollectionEntry(nil), builder.pageEntries...),
	}
	key := storageformat.GarbageCollectionPlanPageKey(builder.checkpointID, page.Index)
	body, err := storageformat.EncodeEnvelope(checkpointGarbagePlanPageSchema, key, 1, page)
	if err != nil {
		return err
	}
	if _, err := builder.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
		existing, getErr := builder.engine.backend.Get(ctx, key)
		if getErr != nil || !reflect.DeepEqual(existing.Body, body) {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan page conflict")
		}
	}
	if builder.pageCount == math.MaxUint64 {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan page count overflow")
	}
	builder.pageCount++
	builder.pageEntries = builder.pageEntries[:0]
	return nil
}

func checkpointGarbageEntriesDigest() hash.Hash {
	digest := sha256.New()
	_, _ = digest.Write([]byte("endlessfs-checkpoint-garbage-plan-v1\x00"))
	return digest
}

func writeCheckpointGarbageDigestEntry(digest hash.Hash, entry storageformat.GarbageCollectionEntry) error {
	body, err := storageformat.EncodeCanonical(entry)
	if err != nil {
		return err
	}
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(body)
	return nil
}

func checkpointGarbageEntryIdentity(entry storageformat.GarbageCollectionEntry) string {
	order := "1"
	if entry.Role == garbageCollectionStateRole {
		order = "0"
	}
	return order + "\x00" + entry.Key
}

func validateCheckpointGarbageEntry(entry storageformat.GarbageCollectionEntry) error {
	key, err := objectstore.ParseKey(entry.Key)
	if err != nil {
		return err
	}
	switch entry.Role {
	case garbageCollectionStateRole:
		if !(checkpointDomainGarbageEligible(key) || strings.HasPrefix(entry.Key, storageformat.TransitionPrefix()) && checkpointJSONGarbageEligible(key) || strings.HasPrefix(entry.Key, storageformat.ProjectionPrefix()) && checkpointJSONGarbageEligible(key) || strings.HasPrefix(entry.Key, storageformat.LeasePrefix()) && checkpointJSONGarbageEligible(key)) {
			return domain.NewError(domain.ErrorInvalid, "checkpoint garbage plan contains an ineligible state object")
		}
	case garbageCollectionFileRole:
		if !checkpointBlobGarbageEligible(key) {
			return domain.NewError(domain.ErrorInvalid, "checkpoint garbage plan contains an ineligible file object")
		}
	default:
		return domain.NewError(domain.ErrorInvalid, "checkpoint garbage plan contains an unknown backend role")
	}
	return nil
}

func decodeCheckpointGarbagePlan(object objectstore.Object, checkpointID string, gateEpoch uint64, inventoryDigest string) (storageformat.GarbageCollectionPlan, error) {
	key := storageformat.GarbageCollectionPlanKey(checkpointID)
	if object.Key != key {
		return storageformat.GarbageCollectionPlan{}, domain.NewError(domain.ErrorInvalid, "checkpoint garbage plan key mismatch")
	}
	var envelope storageformat.Envelope
	var plan storageformat.GarbageCollectionPlan
	if err := storageformat.DecodeEnvelope(object.Body, key, checkpointGarbagePlanSchema, &envelope, &plan); err != nil {
		return storageformat.GarbageCollectionPlan{}, err
	}
	if plan.SchemaVersion != checkpointGarbagePlanSchemaNumber || plan.CheckpointID != checkpointID || plan.GateEpoch != gateEpoch || plan.InventoryDigest != inventoryDigest || plan.PageCount == 0 || plan.EntryCount < uint64(len(plan.Entries)) || plan.EntriesDigest == "" || len(plan.Entries) > checkpointGarbagePlanPageEntries {
		return storageformat.GarbageCollectionPlan{}, domain.NewError(domain.ErrorInvalid, "invalid checkpoint garbage plan")
	}
	return plan, nil
}

func (e *Engine) readCheckpointGarbagePlan(ctx context.Context, checkpoint storageformat.Checkpoint) (storageformat.GarbageCollectionPlan, bool, error) {
	key := storageformat.GarbageCollectionPlanKey(checkpoint.CheckpointID)
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return storageformat.GarbageCollectionPlan{}, false, nil
	}
	if err != nil {
		return storageformat.GarbageCollectionPlan{}, false, err
	}
	plan, err := decodeCheckpointGarbagePlan(object, checkpoint.CheckpointID, checkpoint.GateEpoch, checkpoint.InventoryDigest)
	return plan, true, err
}

// validateCheckpointGarbagePlan authenticates the complete immutable plan
// before the first destructive request. Later pages are streamed again during
// deletion so validation memory remains bounded by one 128-entry page.
func (e *Engine) validateCheckpointGarbagePlan(ctx context.Context, checkpoint storageformat.Checkpoint, plan storageformat.GarbageCollectionPlan) error {
	digest := checkpointGarbageEntriesDigest()
	previous := ""
	count := uint64(0)
	for pageIndex := uint64(0); pageIndex < plan.PageCount; pageIndex++ {
		entries, err := e.checkpointGarbagePlanEntries(ctx, checkpoint, plan, pageIndex)
		if err != nil {
			return err
		}
		if pageIndex+1 < plan.PageCount && len(entries) != checkpointGarbagePlanPageEntries {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan page cardinality mismatch")
		}
		for _, entry := range entries {
			if count >= plan.EntryCount {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan page cardinality mismatch")
			}
			if err := validateCheckpointGarbageEntry(entry); err != nil {
				return err
			}
			identity := checkpointGarbageEntryIdentity(entry)
			if previous != "" && identity <= previous {
				return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan is not ordered")
			}
			previous = identity
			if err := writeCheckpointGarbageDigestEntry(digest, entry); err != nil {
				return err
			}
			count++
		}
	}
	if count != plan.EntryCount || base64.RawURLEncoding.EncodeToString(digest.Sum(nil)) != plan.EntriesDigest {
		return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage plan digest mismatch")
	}
	return nil
}

func (e *Engine) checkpointGarbagePlanEntries(ctx context.Context, checkpoint storageformat.Checkpoint, plan storageformat.GarbageCollectionPlan, index uint64) ([]storageformat.GarbageCollectionEntry, error) {
	if index == 0 {
		return plan.Entries, nil
	}
	key := storageformat.GarbageCollectionPlanPageKey(checkpoint.CheckpointID, index)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	var envelope storageformat.Envelope
	var page storageformat.GarbageCollectionPlanPage
	if err := storageformat.DecodeEnvelope(object.Body, key, checkpointGarbagePlanPageSchema, &envelope, &page); err != nil {
		return nil, err
	}
	if page.SchemaVersion != checkpointGarbagePlanSchemaNumber || page.CheckpointID != checkpoint.CheckpointID || page.GateEpoch != checkpoint.GateEpoch || page.InventoryDigest != checkpoint.InventoryDigest || page.Index != index || len(page.Entries) == 0 || len(page.Entries) > checkpointGarbagePlanPageEntries {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid checkpoint garbage plan page")
	}
	return page.Entries, nil
}

func (e *Engine) runCheckpointGarbagePlan(ctx context.Context, checkpoint storageformat.Checkpoint, plan storageformat.GarbageCollectionPlan, fastVersions map[string]objectstore.ObjectInfo) error {
	if plan.PageCount > uint64(math.MaxInt) {
		return domain.NewError(domain.ErrorInvalid, "checkpoint garbage plan has too many pages")
	}
	if err := e.validateCheckpointGarbagePlan(ctx, checkpoint, plan); err != nil {
		return err
	}
	var session checkpointGarbageCollectionSession
	fresh := fastVersions != nil
	if !fresh {
		var err error
		session, err = e.readCheckpointGarbageSession(ctx, checkpoint, int(plan.PageCount))
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if err == nil && session.value.Phase == checkpointGarbageCollectionComplete {
			return nil
		}
	}
	start := 0
	if session.value.Phase == checkpointGarbageCollectionSweeping {
		start = session.value.SweepIndex
	}
	for pageIndex := start; pageIndex < int(plan.PageCount); pageIndex++ {
		entries, err := e.checkpointGarbagePlanEntries(ctx, checkpoint, plan, uint64(pageIndex))
		if err != nil {
			return err
		}
		versions := fastVersions
		if versions == nil {
			versions, err = e.reacquireCheckpointGarbageVersions(ctx, entries)
			if err != nil {
				return err
			}
		}
		if len(entries) > 0 {
			if err := e.verifyCheckpointGarbageGate(ctx, checkpoint); err != nil {
				return err
			}
			if err := e.deleteCheckpointGarbageEntries(ctx, entries, versions); err != nil {
				return err
			}
		}
		phase := checkpointGarbageCollectionSweeping
		if pageIndex+1 == int(plan.PageCount) {
			phase = checkpointGarbageCollectionComplete
		}
		session, err = e.publishCheckpointGarbagePlanSession(ctx, checkpoint, session, int(plan.PageCount), pageIndex+1, phase)
		if err != nil {
			return err
		}
		if fresh && pageIndex == 0 {
			if err := e.step(ctx, StepCheckpointGarbageAfterSession); err != nil {
				return err
			}
		}
		if err := e.step(ctx, StepCheckpointGarbageAfterPage); err != nil {
			return err
		}
	}
	return e.step(ctx, StepCheckpointGarbageAfterComplete)
}

func (e *Engine) readCheckpointGarbageSession(ctx context.Context, checkpoint storageformat.Checkpoint, pageCount int) (checkpointGarbageCollectionSession, error) {
	key := storageformat.GarbageCollectionSessionKey(checkpoint.CheckpointID)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return checkpointGarbageCollectionSession{}, err
	}
	var envelope storageformat.Envelope
	var value storageformat.GarbageCollectionSession
	if err := storageformat.DecodeEnvelope(object.Body, key, checkpointGarbageCollectionSchema, &envelope, &value); err != nil || validateCheckpointGarbageSession(value, checkpoint, pageCount) != nil {
		return checkpointGarbageCollectionSession{}, domain.NewError(domain.ErrorInvalid, "invalid checkpoint garbage-collection session")
	}
	return checkpointGarbageCollectionSession{object: object, envelope: envelope, value: value}, nil
}

func (e *Engine) publishCheckpointGarbagePlanSession(ctx context.Context, checkpoint storageformat.Checkpoint, session checkpointGarbageCollectionSession, pageCount, nextPage int, phase string) (checkpointGarbageCollectionSession, error) {
	value := storageformat.GarbageCollectionSession{
		SchemaVersion: 2, CheckpointID: checkpoint.CheckpointID, GateEpoch: checkpoint.GateEpoch,
		GateVersion: checkpoint.InventoryDigest, Phase: phase, SweepIndex: nextPage, UpdatedAt: e.clock.Now().UTC(),
	}
	key := storageformat.GarbageCollectionSessionKey(checkpoint.CheckpointID)
	if session.object.Key.Valid() {
		session.value = value
		return e.updateCheckpointGarbageSession(ctx, session)
	}
	body, err := storageformat.EncodeEnvelope(checkpointGarbageCollectionSchema, key, 1, value)
	if err != nil {
		return checkpointGarbageCollectionSession{}, err
	}
	version, err := e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if err == nil {
		return checkpointGarbageCollectionSession{object: objectstore.Object{Key: key, Body: body, Version: version, Size: int64(len(body))}, envelope: storageformat.Envelope{Revision: 1}, value: value}, nil
	}
	if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
		return checkpointGarbageCollectionSession{}, err
	}
	winner, readErr := e.readCheckpointGarbageSession(ctx, checkpoint, pageCount)
	if readErr != nil {
		return checkpointGarbageCollectionSession{}, readErr
	}
	if winner.value.SweepIndex < nextPage || winner.value.Phase != phase && winner.value.Phase != checkpointGarbageCollectionComplete {
		return checkpointGarbageCollectionSession{}, errCheckpointGarbageContended
	}
	return winner, nil
}

func (e *Engine) reacquireCheckpointGarbageVersions(ctx context.Context, entries []storageformat.GarbageCollectionEntry) (map[string]objectstore.ObjectInfo, error) {
	wanted := make(map[string]storageformat.GarbageCollectionEntry, len(entries))
	for _, entry := range entries {
		wanted[checkpointGarbageEntryIdentity(entry)] = entry
	}
	versions := make(map[string]objectstore.ObjectInfo, len(entries))
	collect := func(backend objectstore.MetadataBackend, role string) error {
		return walkObjectInfos(ctx, backend, "endlessfs/v1/", func(info objectstore.ObjectInfo) error {
			identity := checkpointGarbageEntryIdentity(storageformat.GarbageCollectionEntry{Role: role, Key: info.Key.String()})
			if _, found := wanted[identity]; found {
				versions[identity] = info
			}
			return nil
		})
	}
	if err := collect(e.backend, garbageCollectionStateRole); err != nil {
		return nil, err
	}
	if err := collect(e.fileBackend, garbageCollectionFileRole); err != nil {
		return nil, err
	}
	return versions, nil
}

func (e *Engine) deleteCheckpointGarbageEntries(ctx context.Context, entries []storageformat.GarbageCollectionEntry, versions map[string]objectstore.ObjectInfo) error {
	errs := make([]error, len(entries))
	var wait sync.WaitGroup
	for index, entry := range entries {
		info, found := versions[checkpointGarbageEntryIdentity(entry)]
		if !found {
			continue
		}
		key, err := objectstore.ParseKey(entry.Key)
		if err != nil || info.Key != key || info.Version == "" {
			return domain.NewError(domain.ErrorPreconditionFailed, "checkpoint garbage listing metadata mismatch")
		}
		backend := e.fileBackend
		if entry.Role == garbageCollectionStateRole {
			backend = e.backend
		}
		index, backend, info := index, backend, info
		wait.Add(1)
		go func() {
			defer wait.Done()
			traced := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "checkpoint-garbage", Subsystem: "garbage-collection", ParallelGroup: "checkpoint-garbage-page"})
			if err := backend.Delete(traced, info.Key, objectstore.DeleteCondition{Version: info.Version}); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrPreconditionFailed) {
				errs[index] = err
			}
		}()
	}
	wait.Wait()
	return errors.Join(errs...)
}
