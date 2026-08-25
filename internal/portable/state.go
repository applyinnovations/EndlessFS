package portable

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type stateListCursor struct {
	SchemaVersion int       `json:"schemaVersion"`
	Prefix        string    `json:"prefix"`
	Limit         int       `json:"limit"`
	Namespace     string    `json:"namespace"`
	Revision      uint64    `json:"revision"`
	Snapshot      string    `json:"snapshot"`
	Composite     bool      `json:"composite,omitempty"`
	After         string    `json:"after"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type stateRoute struct {
	reference consistencyDomainRef
	exact     bool
}

func decodedStatePath(value string, prefix bool) (state.Namespace, []string, error) {
	if prefix {
		value = strings.TrimSuffix(value, "/")
	}
	parts := strings.Split(value, "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", nil, domain.NewError(domain.ErrorInvalid, "invalid state path")
	}
	decoded := make([]string, 0, len(parts)-1)
	for _, encoded := range parts[1:] {
		part, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(part) == 0 || base64.RawURLEncoding.EncodeToString(part) != encoded {
			return "", nil, domain.NewError(domain.ErrorInvalid, "invalid state path")
		}
		decoded = append(decoded, string(part))
	}
	return state.Namespace(parts[0]), decoded, nil
}

func stateCapabilityReference(namespace state.Namespace, discriminator string, kind storageformat.ConsistencyDomainKind) consistencyDomainRef {
	digest := storageformat.Digest([]byte("endlessfs-state-capability-shard-v1\x00" + string(namespace) + "\x00" + discriminator))
	return consistencyDomainRef{Kind: kind, ID: "state:" + string(namespace) + ":" + digest[:2]}
}

func stateRouteForPath(namespace state.Namespace, parts []string) stateRoute {
	switch namespace {
	case state.NamespaceBootstrap, state.NamespaceRoles:
		return stateRoute{reference: consistencyDomainRef{Kind: storageformat.DomainAdmin, ID: "administration"}, exact: true}
	case state.NamespaceUsers, state.NamespaceAccounts, state.NamespacePreferences, state.NamespaceTrash, state.NamespaceUploads:
		if len(parts) > 0 {
			return stateRoute{reference: consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:" + parts[0]}, exact: true}
		}
	case state.NamespaceCredentials:
		if len(parts) > 1 && parts[0] == "user-index" {
			return stateRoute{reference: consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:" + parts[1]}, exact: true}
		}
		if len(parts) > 0 {
			return stateRoute{reference: stateCapabilityReference(namespace, parts[0], storageformat.DomainCapability), exact: true}
		}
	case state.NamespaceSessions, state.NamespaceCeremonies, state.NamespaceInvites, state.NamespaceRecoveries:
		if len(parts) > 0 {
			return stateRoute{reference: stateCapabilityReference(namespace, parts[0], storageformat.DomainCapability), exact: true}
		}
	case state.NamespaceShares:
		if len(parts) > 0 {
			return stateRoute{reference: stateCapabilityReference(namespace, parts[0], storageformat.DomainShare), exact: true}
		}
	case state.NamespaceIdempotency:
		if len(parts) > 0 {
			ownerIndex := 0
			if (parts[0] == "preview" || parts[0] == "drive") && len(parts) > 1 {
				ownerIndex = 1
			}
			return stateRoute{reference: consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:" + parts[ownerIndex]}, exact: true}
		}
	case state.NamespaceOperations:
		if len(parts) > 1 && (parts[0] == "preview" || parts[0] == "preview-index" || parts[0] == "batch") {
			return stateRoute{reference: consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "owner:" + parts[1]}, exact: true}
		}
		if len(parts) > 0 {
			return stateRoute{reference: stateCapabilityReference(namespace, strings.Join(parts, "\x00"), storageformat.DomainCapability), exact: true}
		}
	}
	return stateRoute{}
}

func stateDomainReferenceForKey(key state.Key) (consistencyDomainRef, error) {
	if !key.Valid() {
		return consistencyDomainRef{}, domain.NewError(domain.ErrorInvalid, "invalid state key")
	}
	namespace, parts, err := decodedStatePath(key.String(), false)
	if err != nil {
		return consistencyDomainRef{}, err
	}
	route := stateRouteForPath(namespace, parts)
	if !route.exact {
		return consistencyDomainRef{}, domain.NewError(domain.ErrorInvalid, "state key has no consistency-domain route")
	}
	return route.reference, nil
}

func stateDomainReferenceForPrefix(prefix state.Prefix) (consistencyDomainRef, bool, error) {
	if !prefix.Valid() {
		return consistencyDomainRef{}, false, domain.NewError(domain.ErrorInvalid, "invalid state prefix")
	}
	namespace, parts, err := decodedStatePath(prefix.String(), true)
	if err != nil {
		return consistencyDomainRef{}, false, err
	}
	route := stateRouteForPath(namespace, parts)
	return route.reference, route.exact, nil
}

func (e *Engine) stateDomainStore() *consistencyDomainStore {
	return newConsistencyDomainStore(e.backend, e.scheduler, e.clock)
}

func (e *Engine) Get(ctx context.Context, key state.Key) (state.Value, error) {
	if err := validateStateKey(key); err != nil {
		return state.Value{}, err
	}
	reference, err := stateDomainReferenceForKey(key)
	if err != nil {
		return state.Value{}, err
	}
	value, err := e.stateDomainStore().get(ctx, reference, key.String())
	if err != nil {
		return state.Value{}, err
	}
	return state.Value{Data: append([]byte(nil), value.Data...), Version: state.Version(value.LogicalVersion)}, nil
}

func (e *Engine) List(ctx context.Context, prefix state.Prefix, request state.PageRequest) (state.Page, error) {
	if !prefix.Valid() {
		return state.Page{}, domain.NewError(domain.ErrorInvalid, "invalid state prefix")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return state.Page{}, domain.NewError(domain.ErrorInvalid, "page limit must be between 1 and 1000")
	}
	namespace := strings.SplitN(prefix.String(), "/", 2)[0]
	after := ""
	wantRevision := uint64(0)
	snapshotDigest := ""
	compositeSnapshot := false
	expiresAt := e.clock.Now().UTC().Add(e.cursorTTL)
	if request.Cursor != "" {
		cursor, err := e.decodeStateListCursor(request.Cursor)
		if err != nil || cursor.Prefix != prefix.String() || cursor.Namespace != namespace || cursor.Limit != limit || cursor.After == "" || !e.clock.Now().Before(cursor.ExpiresAt) {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope state cursor")
		}
		after, wantRevision, snapshotDigest, compositeSnapshot, expiresAt = cursor.After, cursor.Revision, cursor.Snapshot, cursor.Composite, cursor.ExpiresAt
	}
	reference, exact, err := stateDomainReferenceForPrefix(prefix)
	if err != nil {
		return state.Page{}, err
	}
	if !exact {
		if request.Cursor != "" && !compositeSnapshot {
			return state.Page{}, domain.NewError(domain.ErrorInvalid, "state cursor snapshot kind changed")
		}
		return e.listStateAcrossDomains(ctx, prefix, request, limit, after, snapshotDigest, expiresAt)
	}
	if compositeSnapshot {
		return state.Page{}, domain.NewError(domain.ErrorInvalid, "state cursor snapshot kind changed")
	}
	var entries []storageformat.DomainEntry
	var revision uint64
	if snapshotDigest == "" {
		entries, revision, snapshotDigest, err = e.stateDomainStore().list(ctx, reference, prefix.String(), after, limit+1, expiresAt)
	} else {
		entries, revision, err = e.stateDomainStore().listSnapshot(ctx, reference, snapshotDigest, prefix.String(), after, limit+1)
	}
	if err != nil {
		return state.Page{}, err
	}
	if wantRevision != 0 && wantRevision != revision {
		return state.Page{}, domain.NewError(domain.ErrorInvalid, "state cursor snapshot changed")
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	page := state.Page{Items: make([]state.Item, 0, len(entries))}
	for _, entry := range entries {
		logical, err := parseExistingStateKey(entry.Key)
		if err != nil {
			return state.Page{}, err
		}
		page.Items = append(page.Items, state.Item{Key: logical, Value: state.Value{Data: append([]byte(nil), entry.Value...), Version: state.Version(entry.LogicalVersion)}})
	}
	if hasMore {
		page.NextCursor, err = e.encodeStateListCursor(stateListCursor{SchemaVersion: 4, Prefix: prefix.String(), Limit: limit, Namespace: namespace, Revision: revision, Snapshot: snapshotDigest, After: entries[len(entries)-1].Key, ExpiresAt: expiresAt})
		if err != nil {
			return state.Page{}, err
		}
	}
	return page, nil
}

func (e *Engine) Create(ctx context.Context, key state.Key, data []byte) (state.Version, error) {
	if err := validateStateMutation(key, data); err != nil {
		return "", err
	}
	mutation, version, err := e.newStateDomainMutation(key, "", data, false)
	if err != nil {
		return "", err
	}
	reference, err := stateDomainReferenceForKey(key)
	if err != nil {
		return "", err
	}
	if _, err := e.stateDomainStore().mutate(ctx, reference, mutation); err != nil {
		return "", err
	}
	return version, nil
}

func (e *Engine) CompareAndSwap(ctx context.Context, key state.Key, current state.Version, data []byte) (state.Version, error) {
	if err := validateStateMutation(key, data); err != nil {
		return "", err
	}
	if current == "" {
		return "", domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
	mutation, version, err := e.newStateDomainMutation(key, current, data, false)
	if err != nil {
		return "", err
	}
	reference, err := stateDomainReferenceForKey(key)
	if err != nil {
		return "", err
	}
	if _, err := e.stateDomainStore().mutate(ctx, reference, mutation); err != nil {
		return "", err
	}
	return version, nil
}

func (e *Engine) Delete(ctx context.Context, key state.Key, current state.Version) error {
	if err := validateStateKey(key); err != nil {
		return err
	}
	if current == "" {
		return domain.NewError(domain.ErrorInvalid, "current state version is required")
	}
	mutation, _, err := e.newStateDomainMutation(key, current, nil, true)
	if err != nil {
		return err
	}
	reference, routeErr := stateDomainReferenceForKey(key)
	if routeErr != nil {
		return routeErr
	}
	_, err = e.stateDomainStore().mutate(ctx, reference, mutation)
	return err
}

// Mutate applies an idempotent set of state changes through one consistency-
// domain head. Every key is resolved before the backend is touched so a
// cross-domain request cannot partially publish.
func (e *Engine) Mutate(ctx context.Context, mutation state.Mutation) (state.MutationOutcome, error) {
	normalized, _, err := state.NormalizeMutation(mutation)
	if err != nil {
		return state.MutationOutcome{}, err
	}
	var reference consistencyDomainRef
	changes := make([]consistencyDomainChange, len(normalized.Changes))
	for index, change := range normalized.Changes {
		if change.Delete {
			if err := validateStateKey(change.Key); err != nil {
				return state.MutationOutcome{}, err
			}
		} else if err := validateStateMutation(change.Key, change.Data); err != nil {
			return state.MutationOutcome{}, err
		}
		resolved, routeErr := stateDomainReferenceForKey(change.Key)
		if routeErr != nil {
			return state.MutationOutcome{}, routeErr
		}
		if index == 0 {
			reference = resolved
		} else if resolved != reference {
			return state.MutationOutcome{}, domain.NewError(domain.ErrorInvalid, "atomic state mutation spans consistency domains")
		}
		requirement := domainValueRequirement(change.Requirement)
		changes[index] = consistencyDomainChange{
			Key:             change.Key.String(),
			Require:         requirement,
			ExpectedVersion: string(change.ExpectedVersion),
			Delete:          change.Delete,
			Value:           append([]byte(nil), change.Data...),
		}
	}
	domainMutation := consistencyDomainMutation{ID: normalized.ID, RetainUntil: normalized.RetainUntil, Changes: changes, Result: append([]byte(nil), normalized.Result...)}
	canonical, fingerprint, err := normalizeConsistencyDomainMutation(domainMutation)
	if err != nil {
		return state.MutationOutcome{}, err
	}
	domainOutcome, err := e.stateDomainStore().mutate(ctx, reference, canonical)
	if err != nil {
		return state.MutationOutcome{}, err
	}
	outcome := state.MutationOutcome{
		ID:       normalized.ID,
		Result:   append([]byte(nil), domainOutcome.Result...),
		Replayed: domainOutcome.Replayed,
		Changes:  make([]state.ChangeResult, len(normalized.Changes)),
	}
	for index, change := range normalized.Changes {
		version := state.Version("")
		if !change.Delete {
			version = state.Version(consistencyDomainLogicalVersion(normalized.ID, fingerprint, canonical.Changes[index]))
		}
		outcome.Changes[index] = state.ChangeResult{Key: change.Key, Version: version}
	}
	return outcome, nil
}

func (e *Engine) newStateDomainMutation(key state.Key, expected state.Version, data []byte, remove bool) (consistencyDomainMutation, state.Version, error) {
	mutationID, err := e.ids.OpaqueID()
	if err != nil {
		return consistencyDomainMutation{}, "", err
	}
	requirement := domainValueAbsent
	if expected != "" || remove {
		requirement = domainValuePresent
	}
	change := consistencyDomainChange{Key: key.String(), Require: requirement, ExpectedVersion: string(expected), Delete: remove, Value: append([]byte(nil), data...)}
	mutation := consistencyDomainMutation{ID: mutationID, Changes: []consistencyDomainChange{change}}
	normalized, fingerprint, err := normalizeConsistencyDomainMutation(mutation)
	if err != nil {
		return consistencyDomainMutation{}, "", err
	}
	return normalized, state.Version(consistencyDomainLogicalVersion(mutationID, fingerprint, normalized.Changes[0])), nil
}

func (e *Engine) encodeStateListCursor(cursor stateListCursor) (string, error) {
	body, err := storageformat.EncodeCanonical(cursor)
	if err != nil {
		return "", err
	}
	random, err := e.ids.BearerToken()
	if err != nil {
		return "", err
	}
	nonceMaterial, err := base64.RawURLEncoding.DecodeString(random)
	if err != nil || len(nonceMaterial) < e.cursorAEAD.NonceSize() {
		return "", domain.NewError(domain.ErrorInternal, "secure cursor randomness unavailable")
	}
	nonce := nonceMaterial[:e.cursorAEAD.NonceSize()]
	sealed := e.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, body, []byte("endlessfs-state-cursor-v4"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (e *Engine) decodeStateListCursor(value string) (stateListCursor, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) <= e.cursorAEAD.NonceSize() {
		return stateListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor")
	}
	nonceSize := e.cursorAEAD.NonceSize()
	body, err := e.cursorAEAD.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte("endlessfs-state-cursor-v4"))
	if err != nil {
		return stateListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor")
	}
	var cursor stateListCursor
	if err := decodeCanonicalValue(body, &cursor); err != nil || cursor.SchemaVersion != 4 || cursor.Namespace == "" || cursor.Prefix == "" || cursor.Limit < 1 || cursor.Revision == 0 || cursor.Snapshot == "" || cursor.After == "" || cursor.ExpiresAt.IsZero() {
		return stateListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid state cursor")
	}
	return cursor, nil
}

func validateStateKey(key state.Key) error {
	if !key.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid state key")
	}
	return nil
}

func validateStateMutation(key state.Key, data []byte) error {
	if err := validateStateKey(key); err != nil {
		return err
	}
	if len(data) > state.MaxRecordBytes {
		return domain.NewError(domain.ErrorInvalid, "invalid state record size")
	}
	return nil
}

func parseExistingStateKey(value string) (state.Key, error) {
	parts := strings.Split(value, "/")
	if len(parts) < 1 {
		return state.Key{}, domain.NewError(domain.ErrorInvalid, "invalid stored state key")
	}
	namespace := state.Namespace(parts[0])
	decoded := make([]string, 0, len(parts)-1)
	for _, part := range parts[1:] {
		value, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || base64.RawURLEncoding.EncodeToString(value) != part {
			return state.Key{}, domain.NewError(domain.ErrorInvalid, "invalid stored state key")
		}
		decoded = append(decoded, string(value))
	}
	key, err := state.NewKey(namespace, decoded...)
	if err != nil || key.String() != value {
		return state.Key{}, domain.NewError(domain.ErrorInvalid, "invalid stored state key")
	}
	return key, nil
}
