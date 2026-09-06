package state

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type memoryRecord struct {
	data    []byte
	version Version
}

type listSnapshot struct {
	prefix Prefix
	limit  int
	items  []Item
	index  int
}

type memoryMutationOutcome struct {
	fingerprint string
	outcome     MutationOutcome
}

// MemoryStore is a deterministic, concurrency-safe Store implementation.
type MemoryStore struct {
	mu        sync.RWMutex
	records   map[string]memoryRecord
	snapshots map[string]*listSnapshot
	outcomes  map[string]memoryMutationOutcome
	versions  uint64
	cursors   uint64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		records:   make(map[string]memoryRecord),
		snapshots: make(map[string]*listSnapshot),
		outcomes:  make(map[string]memoryMutationOutcome),
	}
}

// Mutate applies all changes while holding the store's single deterministic
// commit lock. Preconditions are checked in full before any record is changed.
func (s *MemoryStore) Mutate(ctx context.Context, mutation Mutation) (MutationOutcome, error) {
	return s.applyMutation(ctx, mutation)
}

// Transact has the same single-lock implementation in memory because the
// deterministic backend has one in-process consistency domain. Object-store
// implementations provide the durable multi-domain decision protocol.
func (s *MemoryStore) Transact(ctx context.Context, mutation Mutation) (MutationOutcome, error) {
	return s.applyMutation(ctx, mutation)
}

func (s *MemoryStore) applyMutation(ctx context.Context, mutation Mutation) (MutationOutcome, error) {
	if err := contextError(ctx); err != nil {
		return MutationOutcome{}, err
	}
	normalized, fingerprint, err := NormalizeMutation(mutation)
	if err != nil {
		return MutationOutcome{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if recorded, found := s.outcomes[normalized.ID]; found {
		if recorded.fingerprint != fingerprint {
			return MutationOutcome{}, domain.NewError(domain.ErrorConflict, "atomic state idempotency key was reused")
		}
		outcome := cloneMutationOutcome(recorded.outcome)
		outcome.Replayed = true
		return outcome, nil
	}
	for _, change := range normalized.Changes {
		record, found := s.records[change.Key.value]
		switch change.Requirement {
		case RequirementAbsent:
			if found {
				return MutationOutcome{}, domain.NewError(domain.ErrorConflict, "state record already exists")
			}
		case RequirementPresent:
			if !found {
				return MutationOutcome{}, domain.NewError(domain.ErrorNotFound, "state record not found")
			}
			if change.ExpectedVersion != "" && record.version != change.ExpectedVersion {
				return MutationOutcome{}, domain.NewError(domain.ErrorPreconditionFailed, "stale state version")
			}
		}
	}
	outcome := MutationOutcome{ID: normalized.ID, Result: append([]byte(nil), normalized.Result...), Changes: make([]ChangeResult, 0, len(normalized.Changes))}
	for _, change := range normalized.Changes {
		if change.Delete {
			delete(s.records, change.Key.value)
			outcome.Changes = append(outcome.Changes, ChangeResult{Key: change.Key})
			continue
		}
		version := s.nextVersion()
		s.records[change.Key.value] = memoryRecord{data: append([]byte(nil), change.Data...), version: version}
		outcome.Changes = append(outcome.Changes, ChangeResult{Key: change.Key, Version: version})
	}
	s.outcomes[normalized.ID] = memoryMutationOutcome{fingerprint: fingerprint, outcome: cloneMutationOutcome(outcome)}
	return outcome, nil
}

func (s *MemoryStore) Get(ctx context.Context, key Key) (Value, error) {
	if err := contextError(ctx); err != nil {
		return Value{}, err
	}
	if err := validateKey(key); err != nil {
		return Value{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, found := s.records[key.value]
	if !found {
		return Value{}, domain.NewError(domain.ErrorNotFound, "state record not found")
	}
	return Value{Data: append([]byte(nil), record.data...), Version: record.version}, nil
}

func (s *MemoryStore) List(ctx context.Context, prefix Prefix, request PageRequest) (Page, error) {
	if err := contextError(ctx); err != nil {
		return Page{}, err
	}
	if err := validatePrefix(prefix); err != nil {
		return Page{}, err
	}
	limit, err := normalizePageLimit(request.Limit)
	if err != nil {
		return Page{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if request.Cursor != "" {
		snapshot, found := s.snapshots[request.Cursor]
		if !found || snapshot.prefix != prefix || snapshot.limit != limit {
			return Page{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope state cursor")
		}
		return s.snapshotPage(request.Cursor, snapshot), nil
	}

	keys := make([]string, 0)
	for key := range s.records {
		if strings.HasPrefix(key, prefix.value) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	items := make([]Item, 0, len(keys))
	for _, key := range keys {
		record := s.records[key]
		items = append(items, Item{
			Key:   Key{value: key},
			Value: Value{Data: append([]byte(nil), record.data...), Version: record.version},
		})
	}
	if len(items) <= limit {
		return Page{Items: items}, nil
	}
	s.cursors++
	cursor := "c" + string(versionString(s.cursors))
	snapshot := &listSnapshot{prefix: prefix, limit: limit, items: items}
	s.snapshots[cursor] = snapshot
	return s.snapshotPage(cursor, snapshot), nil
}

func (s *MemoryStore) snapshotPage(cursor string, snapshot *listSnapshot) Page {
	end := min(snapshot.index+snapshot.limit, len(snapshot.items))
	items := append([]Item(nil), snapshot.items[snapshot.index:end]...)
	snapshot.index = end
	if end == len(snapshot.items) {
		delete(s.snapshots, cursor)
		cursor = ""
	}
	return Page{Items: items, NextCursor: cursor}
}

func (s *MemoryStore) Create(ctx context.Context, key Key, data []byte) (Version, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if err := validateKey(key); err != nil {
		return "", err
	}
	if err := validateRecordData(data); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[key.value]; exists {
		return "", domain.NewError(domain.ErrorConflict, "state record already exists")
	}
	version := s.nextVersion()
	s.records[key.value] = memoryRecord{data: append([]byte(nil), data...), version: version}
	return version, nil
}

func (s *MemoryStore) CompareAndSwap(ctx context.Context, key Key, current Version, data []byte) (Version, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if err := validateKey(key); err != nil {
		return "", err
	}
	if err := validateRecordData(data); err != nil {
		return "", err
	}
	if current == "" {
		return "", domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[key.value]
	if !exists {
		return "", domain.NewError(domain.ErrorNotFound, "state record not found")
	}
	if record.version != current {
		return "", domain.NewError(domain.ErrorPreconditionFailed, "stale state version")
	}
	version := s.nextVersion()
	s.records[key.value] = memoryRecord{data: append([]byte(nil), data...), version: version}
	return version, nil
}

func (s *MemoryStore) Delete(ctx context.Context, key Key, current Version) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return err
	}
	if current == "" {
		return domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[key.value]
	if !exists {
		return domain.NewError(domain.ErrorNotFound, "state record not found")
	}
	if record.version != current {
		return domain.NewError(domain.ErrorPreconditionFailed, "stale state version")
	}
	delete(s.records, key.value)
	return nil
}

func (s *MemoryStore) nextVersion() Version {
	s.versions++
	return versionString(s.versions)
}

func validateRecordData(data []byte) error {
	if len(data) > MaxRecordBytes {
		return domain.NewError(domain.ErrorInvalid, "invalid state record size")
	}
	return nil
}
