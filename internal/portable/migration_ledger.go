package portable

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// storageSchemaID identifies one complete canonical storage-set schema epoch.
// It is deliberately independent of application release numbers: releases map
// to an epoch, while migrations connect adjacent epochs.
type storageSchemaID string

type storageMigrationID string

const (
	storageSchema001 storageSchemaID = "endlessfs-portable-v1/schema-001"
	storageSchema002 storageSchemaID = "endlessfs-portable-v1/schema-002"
	storageSchema003 storageSchemaID = "endlessfs-portable-v1/schema-003"
	storageSchema004 storageSchemaID = "endlessfs-portable-v1/schema-004"
	storageSchema005 storageSchemaID = "endlessfs-portable-v1/schema-005"
	storageSchema006 storageSchemaID = "endlessfs-portable-v1/schema-006"
	storageSchema007 storageSchemaID = "endlessfs-portable-v1/schema-007"
	storageSchema008 storageSchemaID = "endlessfs-portable-v1/schema-008"
	storageSchema009 storageSchemaID = "endlessfs-portable-v1/schema-009"
	storageSchema010 storageSchemaID = "endlessfs-portable-v1/schema-010"
	storageSchema011 storageSchemaID = "endlessfs-portable-v1/schema-011"

	storageMigration001To002 storageMigrationID = "schema-001-to-002"
	storageMigration002To003 storageMigrationID = "schema-002-to-003"
	storageMigration003To004 storageMigrationID = "schema-003-to-004"
	storageMigration004To005 storageMigrationID = "schema-004-to-005"
	storageMigration005To006 storageMigrationID = "schema-005-to-006"
	storageMigration006To007 storageMigrationID = "schema-006-to-007"
	storageMigration007To008 storageMigrationID = "schema-007-to-008"
	storageMigration008To009 storageMigrationID = "schema-008-to-009"
	storageMigration009To010 storageMigrationID = "schema-009-to-010"
	storageMigration010To011 storageMigrationID = "schema-010-to-011"
)

type storageSchemaReleaseBoundary struct {
	first  string
	schema storageSchemaID
}

// StorageSchemaReleaseRange is a release interval that wrote one storage
// schema. Before is exclusive and empty for the current interval. The interval
// is derived from consecutive entries in the append-only release ledger.
type StorageSchemaReleaseRange struct {
	First  string
	Before string
}

// StorageSchemaHistoryEntry is a read-only snapshot of one append-only ledger
// entry. It is exposed from this internal package so release qualification can
// derive its fixture matrix from the production ledger instead of duplicating
// migration knowledge in test code.
type StorageSchemaHistoryEntry struct {
	ID          string
	Features    []string
	GateBinding string
	Releases    []StorageSchemaReleaseRange
	Successor   string
	MigrationID string
}

type releaseVersion struct {
	major int
	minor int
	patch int
}

type storageMigrationRun func(*Engine, context.Context, storageMigration, objectstore.Object, storageformat.Superblock) error
type storageMigrationAuthorityVerifier func(*Engine, context.Context, storageMigration) error

type storageMigration struct {
	id              storageMigrationID
	from            storageSchemaID
	to              storageSchemaID
	checkpointID    string
	run             storageMigrationRun
	verifyAuthority storageMigrationAuthorityVerifier
}

type storageGateBinding string

const (
	storageGateLegacyUnbound storageGateBinding = "legacy-unbound"
	storageGateFeatureBound  storageGateBinding = "writer-features"
)

type storageSchemaDefinition struct {
	id                    storageSchemaID
	features              []string
	gateBinding           storageGateBinding
	migrationFromPrevious *storageMigration
}

var schemaMigration001To002 = storageMigration{
	id: storageMigration001To002, from: storageSchema001, to: storageSchema002,
	checkpointID: schema001To002CheckpointID,
}

var schemaMigration002To003 = storageMigration{
	id: storageMigration002To003, from: storageSchema002, to: storageSchema003,
	checkpointID: "automatic-storage-schema-002-to-003",
}

var schemaMigration003To004 = storageMigration{
	id: storageMigration003To004, from: storageSchema003, to: storageSchema004,
	checkpointID: "automatic-storage-schema-003-to-004",
}

var schemaMigration004To005 = storageMigration{
	id: storageMigration004To005, from: storageSchema004, to: storageSchema005,
	checkpointID: "automatic-storage-schema-004-to-005",
}

var schemaMigration005To006 = storageMigration{
	id: storageMigration005To006, from: storageSchema005, to: storageSchema006,
	checkpointID: "automatic-storage-schema-005-to-006",
}

var schemaMigration006To007 = storageMigration{
	id: storageMigration006To007, from: storageSchema006, to: storageSchema007,
	checkpointID: "automatic-storage-schema-006-to-007",
}

var schemaMigration007To008 = storageMigration{
	id: storageMigration007To008, from: storageSchema007, to: storageSchema008,
	checkpointID: "automatic-storage-schema-007-to-008",
}

var schemaMigration008To009 = storageMigration{
	id: storageMigration008To009, from: storageSchema008, to: storageSchema009,
	checkpointID: "automatic-storage-schema-008-to-009",
}

var schemaMigration009To010 = storageMigration{
	id: storageMigration009To010, from: storageSchema009, to: storageSchema010,
	checkpointID: "automatic-storage-schema-009-to-010",
}

var schemaMigration010To011 = storageMigration{
	id: storageMigration010To011, from: storageSchema010, to: storageSchema011,
	checkpointID: "automatic-storage-schema-010-to-011",
}

// storageSchemaLedger is append-only. Extend it by adding one definition whose
// migrationFromPrevious connects the prior terminal epoch to the new epoch;
// never insert, reorder, or rewrite an existing entry.
var storageSchemaLedger = []storageSchemaDefinition{
	{id: storageSchema001, gateBinding: storageGateLegacyUnbound},
	{
		id: storageSchema002, features: []string{storageformat.FeatureRecursiveBytes}, gateBinding: storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration001To002,
	},
	{
		id:                    storageSchema003,
		features:              []string{storageformat.FeatureRecursiveBytes, storageformat.FeatureRecursiveFileCounts},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration002To003,
	},
	{
		id: storageSchema004,
		features: []string{
			storageformat.FeatureDirectoryDigests,
			storageformat.FeatureDuplicateCatalog,
			storageformat.FeatureMetadataCheckpoints,
			storageformat.FeaturePagedOperations,
			storageformat.FeatureDirectoryIndexes,
			storageformat.FeatureStateIndexes,
			storageformat.FeatureProviderFingerprints,
			storageformat.FeatureRecursiveBytes,
			storageformat.FeatureRecursiveFileCounts,
		},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration003To004,
	},
	{
		id: storageSchema005,
		features: []string{
			storageformat.FeatureDirectoryDigests,
			storageformat.FeatureDuplicateCatalog,
			storageformat.FeatureMetadataCheckpoints,
			storageformat.FeaturePagedOperations,
			storageformat.FeatureDirectoryIndexes,
			storageformat.FeatureStateIndexes,
			storageformat.FeatureProviderFingerprints,
			storageformat.FeatureRecursiveBytes,
			storageformat.FeatureRecursiveFileCounts,
			storageformat.FeatureResumableOperations,
		},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration004To005,
	},
	{
		id: storageSchema006,
		features: []string{
			storageformat.FeatureDirectoryDigests,
			storageformat.FeatureDuplicateCatalog,
			storageformat.FeatureMetadataCheckpoints,
			storageformat.FeaturePagedOperations,
			storageformat.FeatureDirectoryIndexes,
			storageformat.FeatureNamespaceSnapshots,
			storageformat.FeatureStateIndexes,
			storageformat.FeatureProviderFingerprints,
			storageformat.FeatureRecursiveBytes,
			storageformat.FeatureRecursiveFileCounts,
			storageformat.FeatureResumableOperations,
		},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration005To006,
	},
	{
		id: storageSchema007,
		features: []string{
			storageformat.FeatureDirectoryDigests,
			storageformat.FeatureDuplicateCatalog,
			storageformat.FeatureMetadataCheckpoints,
			storageformat.FeaturePagedOperations,
			storageformat.FeatureDirectoryIndexes,
			storageformat.FeatureNamespaceSnapshots,
			storageformat.FeatureStateIndexes,
			storageformat.FeatureProviderFingerprints,
			storageformat.FeatureRecursiveBytes,
			storageformat.FeatureRecursiveFileCounts,
			storageformat.FeatureResumableOperations,
			storageformat.FeatureUserDirectoryCatalog,
		},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration006To007,
	},
	{
		id: storageSchema008,
		features: []string{
			storageformat.FeatureConsistencyDomains,
			storageformat.FeatureDirectoryDigests,
			storageformat.FeatureDuplicateCatalog,
			storageformat.FeatureMetadataCheckpoints,
			storageformat.FeatureOwnerNamespaceGraph,
			storageformat.FeaturePagedOperations,
			storageformat.FeatureDirectoryIndexes,
			storageformat.FeatureNamespaceSnapshots,
			storageformat.FeatureStateIndexes,
			storageformat.FeatureProviderFingerprints,
			storageformat.FeatureDerivedProjections,
			storageformat.FeatureRecursiveBytes,
			storageformat.FeatureRecursiveFileCounts,
			storageformat.FeatureResumableOperations,
			storageformat.FeatureUserDirectoryCatalog,
		},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration007To008,
	},
	{
		id: storageSchema009,
		features: []string{
			storageformat.FeatureConsistencyDomains,
			storageformat.FeatureDirectoryDigests,
			storageformat.FeatureDuplicateCatalog,
			storageformat.FeatureMetadataCheckpoints,
			storageformat.FeatureOwnerNamespaceGraph,
			storageformat.FeaturePagedOperations,
			storageformat.FeatureDirectoryIndexes,
			storageformat.FeatureNamespaceSnapshots,
			storageformat.FeatureStateIndexes,
			storageformat.FeatureProviderFingerprints,
			storageformat.FeatureDerivedProjections,
			storageformat.FeatureRecursiveBytes,
			storageformat.FeatureRecursiveFileCounts,
			storageformat.FeatureResumableOperations,
			storageformat.FeatureTransactionalState,
			storageformat.FeatureUserDirectoryCatalog,
		},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration008To009,
	},
	{
		id: storageSchema010,
		features: []string{
			storageformat.FeatureConsistencyDomains,
			storageformat.FeatureDirectoryDigests,
			storageformat.FeatureDuplicateCatalog,
			storageformat.FeatureMetadataCheckpoints,
			storageformat.FeatureOwnerNamespaceGraph,
			storageformat.FeaturePagedOperations,
			storageformat.FeatureDirectoryIndexes,
			storageformat.FeatureNamespaceSnapshots,
			storageformat.FeatureStateIndexes,
			storageformat.FeatureProviderFingerprints,
			storageformat.FeatureDerivedProjections,
			storageformat.FeatureRecursiveBytes,
			storageformat.FeatureRecursiveFileCounts,
			storageformat.FeatureResumableOperations,
			storageformat.FeatureStateConservation,
			storageformat.FeatureTransactionalState,
			storageformat.FeatureUserDirectoryCatalog,
		},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration009To010,
	},
	{
		id: storageSchema011,
		features: []string{
			storageformat.FeatureConsistencyDomains,
			storageformat.FeatureDirectoryDigests,
			storageformat.FeatureDuplicateCatalog,
			storageformat.FeatureMetadataCheckpoints,
			storageformat.FeatureOwnerNamespaceGraph,
			storageformat.FeaturePackedDomainPages,
			storageformat.FeaturePagedOperations,
			storageformat.FeatureDirectoryIndexes,
			storageformat.FeatureNamespaceSnapshots,
			storageformat.FeatureStateIndexes,
			storageformat.FeatureProviderFingerprints,
			storageformat.FeatureDerivedProjections,
			storageformat.FeatureRecursiveBytes,
			storageformat.FeatureRecursiveFileCounts,
			storageformat.FeatureResumableOperations,
			storageformat.FeatureStateConservation,
			storageformat.FeatureTransactionalState,
			storageformat.FeatureUploadTransactions,
			storageformat.FeatureUserDirectoryCatalog,
		},
		gateBinding:           storageGateFeatureBound,
		migrationFromPrevious: &schemaMigration010To011,
	},
}

// storageSchemaReleaseLedger is also append-only. A boundary is the first
// release written with its schema and remains valid until the next boundary.
// Schema epochs that were never tagged still remain in storageSchemaLedger.
var storageSchemaReleaseLedger = []storageSchemaReleaseBoundary{
	{first: "v0.1.0", schema: storageSchema001},
	{first: "v0.1.5", schema: storageSchema003},
	{first: "v0.2.0", schema: storageSchema005},
	{first: "v0.3.0", schema: storageSchema006},
	{first: "v0.4.0", schema: storageSchema009},
	{first: "v0.5.0", schema: storageSchema010},
	{first: "v0.7.0", schema: storageSchema011},
}

func init() {
	// Go's initialization dependency analysis follows method bodies back to the
	// ledger. Assigning runners after the immutable definitions are built keeps
	// the registered edge implementation explicit without an initialization
	// cycle.
	schemaMigration001To002.run = (*Engine).runStorageMigration001To002
	schemaMigration002To003.run = (*Engine).runStorageMigration002To003
	schemaMigration003To004.run = (*Engine).runStorageMigration003To004
	schemaMigration004To005.run = (*Engine).runStorageMigration004To005
	schemaMigration005To006.run = (*Engine).runStorageMigration005To006
	schemaMigration006To007.run = (*Engine).runStorageMigration006To007
	schemaMigration007To008.run = (*Engine).runStorageMigration007To008
	schemaMigration008To009.run = (*Engine).runStorageMigration008To009
	schemaMigration009To010.run = (*Engine).runStorageMigration009To010
	schemaMigration009To010.verifyAuthority = (*Engine).verifySchema010Authority
	schemaMigration010To011.run = (*Engine).runStorageMigration010To011
	schemaMigration010To011.verifyAuthority = (*Engine).verifySchema011Authority
}

func currentStorageSchema() storageSchemaDefinition {
	return storageSchemaLedger[len(storageSchemaLedger)-1]
}

// StorageSchemaHistory returns a defensive snapshot in migration order.
func StorageSchemaHistory() []StorageSchemaHistoryEntry {
	history := make([]StorageSchemaHistoryEntry, 0, len(storageSchemaLedger))
	for index, schema := range storageSchemaLedger {
		entry := StorageSchemaHistoryEntry{
			ID: schema.id.String(), Features: append([]string(nil), schema.features...), GateBinding: string(schema.gateBinding),
			Releases: releaseRangesForSchema(schema.id),
		}
		if index+1 < len(storageSchemaLedger) {
			migration := storageSchemaLedger[index+1].migrationFromPrevious
			if migration != nil {
				entry.Successor = migration.to.String()
				entry.MigrationID = migration.id.String()
			}
		}
		history = append(history, entry)
	}
	return history
}

// StorageSchemaForRelease resolves a release through the ledger's declared
// validity intervals. It never infers a schema from a fixture filename.
func StorageSchemaForRelease(release string) (string, bool) {
	version, err := parseReleaseVersion(release)
	if err != nil {
		return "", false
	}
	for index := len(storageSchemaReleaseLedger) - 1; index >= 0; index-- {
		boundary := storageSchemaReleaseLedger[index]
		first, parseErr := parseReleaseVersion(boundary.first)
		if parseErr == nil && !version.less(first) {
			return boundary.schema.String(), true
		}
	}
	return "", false
}

func (id storageSchemaID) String() string    { return string(id) }
func (id storageMigrationID) String() string { return string(id) }

func releaseRangesForSchema(id storageSchemaID) []StorageSchemaReleaseRange {
	var ranges []StorageSchemaReleaseRange
	for index, boundary := range storageSchemaReleaseLedger {
		if boundary.schema != id {
			continue
		}
		interval := StorageSchemaReleaseRange{First: boundary.first}
		if index+1 < len(storageSchemaReleaseLedger) {
			interval.Before = storageSchemaReleaseLedger[index+1].first
		}
		ranges = append(ranges, interval)
	}
	return ranges
}

func parseReleaseVersion(value string) (releaseVersion, error) {
	if !strings.HasPrefix(value, "v") {
		return releaseVersion{}, fmt.Errorf("release %q has no v prefix", value)
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return releaseVersion{}, fmt.Errorf("release %q is not vMAJOR.MINOR.PATCH", value)
	}
	values := make([]int, len(parts))
	for index, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return releaseVersion{}, fmt.Errorf("release %q is not canonical", value)
		}
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return releaseVersion{}, fmt.Errorf("release %q is not canonical", value)
		}
		values[index] = parsed
	}
	return releaseVersion{major: values[0], minor: values[1], patch: values[2]}, nil
}

func (version releaseVersion) less(other releaseVersion) bool {
	if version.major != other.major {
		return version.major < other.major
	}
	if version.minor != other.minor {
		return version.minor < other.minor
	}
	return version.patch < other.patch
}

// MigrationStepName scopes a deterministic scheduler boundary to one ledger
// edge so crash and concurrency tests can target every step independently.
func MigrationStepName(migrationID, boundary string) string {
	return "storage-migration:" + migrationID + ":" + boundary
}

func storageMigrationPath(from storageSchemaID) ([]storageMigration, error) {
	conservationIndex, _ := schemaIndex(storageSchema010)
	for index, schema := range storageSchemaLedger {
		if schema.id != from {
			continue
		}
		if index == len(storageSchemaLedger)-1 {
			return nil, nil
		}
		path := make([]storageMigration, 0, len(storageSchemaLedger)-index-1)
		for position := index + 1; position < len(storageSchemaLedger); position++ {
			migration := storageSchemaLedger[position].migrationFromPrevious
			if migration == nil || migration.from != storageSchemaLedger[position-1].id || migration.to != storageSchemaLedger[position].id {
				return nil, fmt.Errorf("invalid storage schema ledger edge into %q", storageSchemaLedger[position].id)
			}
			if position >= conservationIndex && migration.verifyAuthority == nil {
				return nil, fmt.Errorf("storage schema ledger edge into %q has no pre-activation authority verifier", storageSchemaLedger[position].id)
			}
			path = append(path, *migration)
		}
		return path, nil
	}
	return nil, fmt.Errorf("unknown storage schema %q", from)
}

func schemaDefinition(id storageSchemaID) (storageSchemaDefinition, bool) {
	for _, schema := range storageSchemaLedger {
		if schema.id == id {
			return schema, true
		}
	}
	return storageSchemaDefinition{}, false
}

func schemaIndex(id storageSchemaID) (int, bool) {
	for index, schema := range storageSchemaLedger {
		if schema.id == id {
			return index, true
		}
	}
	return 0, false
}

func schemaFeatures(id storageSchemaID, current []string) ([]string, bool) {
	schema, found := schemaDefinition(id)
	if !found {
		return nil, false
	}
	features := make([]string, 0, len(current))
	for _, feature := range current {
		if !ledgerManagesFeature(feature) {
			features = append(features, feature)
		}
	}
	features = append(features, schema.features...)
	sort.Strings(features)
	return features, true
}

func ledgerManagesFeature(feature string) bool {
	for _, schema := range storageSchemaLedger {
		for _, managed := range schema.features {
			if feature == managed {
				return true
			}
		}
	}
	return false
}

func detectStorageSchema(features, current []string) (storageSchemaDefinition, bool) {
	for _, schema := range storageSchemaLedger {
		expected, _ := schemaFeatures(schema.id, current)
		if equalStrings(features, expected) {
			return schema, true
		}
	}
	return storageSchemaDefinition{}, false
}

// detectWriteGateSchema interprets the gate using the binding representation
// declared by the complete historical schema epoch. Schema 001 predates the
// WriterFeatures field, so its gate is intentionally unbound even when its
// writer set carries non-ledger application features. Later epochs require an
// exact feature-bound signature and never accept an empty legacy gate.
func detectWriteGateSchema(features, current []string) (storageSchemaDefinition, bool) {
	for _, schema := range storageSchemaLedger {
		switch schema.gateBinding {
		case storageGateLegacyUnbound:
			if len(features) == 0 {
				return schema, true
			}
		case storageGateFeatureBound:
			expected, _ := schemaFeatures(schema.id, current)
			if equalStrings(features, expected) {
				return schema, true
			}
		}
	}
	return storageSchemaDefinition{}, false
}

func schemaAtLeast(features []string, minimum storageSchemaID, current []string) bool {
	detected, found := detectStorageSchema(features, current)
	if !found {
		return false
	}
	detectedIndex, _ := schemaIndex(detected.id)
	minimumIndex, found := schemaIndex(minimum)
	return found && detectedIndex >= minimumIndex
}

func writeGateSchemaAtLeast(features []string, minimum storageSchemaID, current []string) bool {
	detected, found := detectWriteGateSchema(features, current)
	if !found {
		return false
	}
	detectedIndex, _ := schemaIndex(detected.id)
	minimumIndex, found := schemaIndex(minimum)
	return found && detectedIndex >= minimumIndex
}

func migrationForCheckpoint(checkpointID string) (storageMigration, bool) {
	if checkpointID == "" {
		return storageMigration{}, false
	}
	for _, schema := range storageSchemaLedger {
		if schema.migrationFromPrevious != nil && schema.migrationFromPrevious.checkpointID == checkpointID {
			return *schema.migrationFromPrevious, true
		}
	}
	return storageMigration{}, false
}

func (e *Engine) storageMigrationPending(ctx context.Context) (bool, error) {
	if object, err := e.backend.Get(ctx, storageformat.WriterSetKey()); err == nil {
		var envelope storageformat.Envelope
		var writer storageformat.WriterSet
		if err := storageformat.DecodeEnvelope(object.Body, object.Key, writerSetSchema, &envelope, &writer); err != nil {
			return false, err
		}
		if schema, found := detectStorageSchema(writer.RequiredFeatures, e.writer.RequiredFeatures); found && schema.id != currentStorageSchema().id {
			return true, nil
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return false, err
	}
	if object, err := e.backend.Get(ctx, storageformat.WriteGateKey()); err == nil {
		var envelope storageformat.Envelope
		var gate storageformat.WriteGate
		if err := storageformat.DecodeEnvelope(object.Body, object.Key, writeGateSchema, &envelope, &gate); err != nil {
			return false, err
		}
		if _, found := migrationForCheckpoint(gate.CheckpointID); found {
			return true, nil
		}
		if schema, found := detectWriteGateSchema(gate.WriterFeatures, e.writer.RequiredFeatures); found && schema.id != currentStorageSchema().id {
			return true, nil
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return false, err
	}
	return false, nil
}

func (e *Engine) migrateStorageSchemaChain(ctx context.Context) error {
	for range len(storageSchemaLedger) * 8 {
		superblockObject, err := e.backend.Get(ctx, storageformat.SuperblockKey())
		if err != nil {
			return err
		}
		var superblock storageformat.Superblock
		if err := decodeCanonicalSuperblock(superblockObject.Body, &superblock); err != nil {
			return err
		}
		if err := validateCompatibleSuperblock(superblock); err != nil {
			return err
		}

		gateObject, _, gate, err := e.readGate(ctx)
		if err != nil {
			return err
		}
		if migration, found := migrationForCheckpoint(gate.CheckpointID); found {
			if runErr := migration.run(e, ctx, migration, superblockObject, superblock); runErr != nil {
				if err := e.resolveMigrationRunError(ctx, migration, runErr); err != nil {
					return err
				}
			}
			continue
		}

		schema, found := detectStorageSchema(superblock.RequiredFeatures, e.writer.RequiredFeatures)
		if !found {
			return domain.NewError(domain.ErrorPreconditionFailed, "unregistered portable storage schema")
		}
		if gate.Mode == storageformat.GateOpen && schemaAtLeast(superblock.RequiredFeatures, storageSchema008, e.writer.RequiredFeatures) {
			// Opening a migration checkpoint authorizes the idempotent domain-
			// unfreeze suffix. Complete that suffix before selecting a following
			// edge. This is required both after a process restart and when a
			// concurrent replica observes the opened gate before the winning
			// replica has finished thawing the catalog.
			reconciledGate, reconcileErr := e.reconcileGateDomainFreeze(ctx, gateObject, gate)
			if reconcileErr != nil {
				return reconcileErr
			}
			if reconciledGate.Mode != storageformat.GateOpen || reconciledGate.Epoch != gate.Epoch || reconciledGate.CheckpointID != "" {
				continue
			}
			if !equalStrings(reconciledGate.WriterFeatures, gate.WriterFeatures) {
				continue
			}
		}
		path, err := storageMigrationPath(schema.id)
		if err != nil {
			return domain.WrapError(domain.ErrorPreconditionFailed, "resolve storage migration path", err)
		}
		if len(path) == 0 {
			pending, pendingErr := e.storageMigrationPending(ctx)
			if pendingErr != nil {
				return pendingErr
			}
			if !pending {
				return nil
			}
			return domain.NewError(domain.ErrorPreconditionFailed, "storage schema markers disagree without a resumable migration checkpoint")
		}
		migration := path[0]
		if runErr := migration.run(e, ctx, migration, superblockObject, superblock); runErr != nil {
			if err := e.resolveMigrationRunError(ctx, migration, runErr); err != nil {
				return err
			}
		}
	}
	return domain.NewError(domain.ErrorUnavailable, "storage schema migration chain did not converge")
}

// A replica can retain a predecessor object reference or lose any provider
// response while another replica completes the remaining schema suffix. An
// error is superseded only when independent reads of every durable completion
// marker prove that this edge (or a later edge) completed. An incomplete or
// unreadable marker set preserves the original error and remains fail-closed.
func (e *Engine) resolveMigrationRunError(ctx context.Context, migration storageMigration, runErr error) error {
	complete, err := e.storageMigrationComplete(ctx, migration)
	if err == nil && complete {
		return nil
	}
	return runErr
}
