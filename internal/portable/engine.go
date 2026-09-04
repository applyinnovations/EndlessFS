// Package portable implements provider and state semantics over an object backend.
package portable

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	superblockSchema   = "superblock-v1"
	writerSetSchema    = "writer-set-v1"
	writeGateSchema    = "write-gate-v1"
	admissionSchema    = "admission-v1"
	stateRecordSchema  = "state-record-v1"
	stateVersionSchema = "state-version-v1"
)

type WriterConfiguration struct {
	WriterSetID         string
	ConfigurationDigest string
	KeyringIdentifiers  []string
	RequiredFeatures    []string
}

type Options struct {
	Backend           objectstore.Backend
	FileBackend       objectstore.FileControlBackend
	Clock             domain.Clock
	IDs               *domain.IDGenerator
	Writer            WriterConfiguration
	LeaseTTL          time.Duration
	UploadTTL         time.Duration
	DownloadTTL       time.Duration
	CursorKey         []byte
	CursorTTL         time.Duration
	Scheduler         Scheduler
	MigrationObserver func(MigrationProgress)
}

const (
	MigrationStageStarted             = "started"
	MigrationStageGateClosed          = "gate-closed"
	MigrationStageDirectoriesVerified = "directories-verified"
	MigrationStageCheckpointInventory = "checkpoint-inventory"
	MigrationStageCheckpointCreated   = "checkpoint-created"
	MigrationStageComplete            = "complete"
)

// MigrationProgress intentionally contains no object key, virtual path,
// provider identifier, or backend-native version. It is safe to forward to
// structured operational logs.
type MigrationProgress struct {
	MigrationID      string
	Stage            string
	Role             string
	CompletedObjects int
	TotalObjects     int
	CompletedBytes   int64
	TotalBytes       int64
	ResumedObjects   int
}

type Scheduler interface {
	Step(context.Context, string) error
}

type SchedulerFunc func(context.Context, string) error

func (f SchedulerFunc) Step(ctx context.Context, step string) error { return f(ctx, step) }

const (
	StepDomainBeforeHeadCommit         = "consistency-domain:before-head-commit"
	StepDomainAfterHeadCommit          = "consistency-domain:after-head-commit"
	StepUploadBatchAfterIntents        = "upload-batch:after-intents"
	StepUploadBatchAfterSessions       = "upload-batch:after-sessions"
	StepUploadBatchAfterActivation     = "upload-batch:after-activation"
	StepUploadBatchCompletionProgress  = "upload-batch-completion:after-progress"
	StepUploadBatchCompletionVerified  = "upload-batch-completion:after-verified"
	StepUploadBatchCompletionPublished = "upload-batch-completion:after-published"
	StepUploadBatchAbortProgress       = "upload-batch-abort:after-progress"
	StepUploadBatchAbortApplied        = "upload-batch-abort:after-provider-effects"
	StepUploadBatchAbortPublished      = "upload-batch-abort:after-published"
)

type Engine struct {
	backend             objectstore.Backend
	fileBackend         objectstore.FileControlBackend
	separateFileBackend bool
	clock               domain.Clock
	ids                 *domain.IDGenerator
	writer              storageformat.WriterSet
	leaseTTL            time.Duration
	uploadTTL           time.Duration
	downloadTTL         time.Duration
	cursorAEAD          cipher.AEAD
	cursorTTL           time.Duration
	scheduler           Scheduler
	migrationObserver   func(MigrationProgress)
}

func Open(ctx context.Context, options Options) (*Engine, error) {
	if options.Backend == nil || options.Clock == nil || options.IDs == nil || options.LeaseTTL <= 0 || len(options.CursorKey) != 32 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid portable engine configuration")
	}
	writer, err := canonicalWriterConfiguration(options.Writer)
	if err != nil {
		return nil, err
	}
	if options.UploadTTL == 0 {
		options.UploadTTL = 10 * time.Minute
	}
	if options.DownloadTTL == 0 {
		options.DownloadTTL = 10 * time.Minute
	}
	if options.CursorTTL == 0 {
		options.CursorTTL = 10 * time.Minute
	}
	if options.UploadTTL <= 0 || options.DownloadTTL <= 0 || options.DownloadTTL > 10*time.Minute || options.CursorTTL <= 0 || options.CursorTTL > time.Hour {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid portable transfer TTL")
	}
	block, err := aes.NewCipher(options.CursorKey)
	if err != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid portable cursor key")
	}
	cursorAEAD, err := cipher.NewGCM(block)
	if err != nil {
		return nil, domain.WrapError(domain.ErrorInternal, "initialize portable cursor protection", err)
	}
	fileBackend := options.FileBackend
	separateFileBackend := fileBackend != nil
	if fileBackend == nil {
		fileBackend = options.Backend
	}
	engine := &Engine{backend: options.Backend, fileBackend: fileBackend, separateFileBackend: separateFileBackend, clock: options.Clock, ids: options.IDs, writer: writer, leaseTTL: options.LeaseTTL, uploadTTL: options.UploadTTL, downloadTTL: options.DownloadTTL, cursorAEAD: cursorAEAD, cursorTTL: options.CursorTTL, scheduler: options.Scheduler, migrationObserver: options.MigrationObserver}
	if err := engine.initialize(ctx); err != nil {
		return nil, err
	}
	return engine, nil
}

func canonicalWriterConfiguration(configuration WriterConfiguration) (storageformat.WriterSet, error) {
	if configuration.WriterSetID == "" || configuration.ConfigurationDigest == "" || len(configuration.KeyringIdentifiers) == 0 {
		return storageformat.WriterSet{}, domain.NewError(domain.ErrorInvalid, "invalid portable writer configuration")
	}
	keyrings := append([]string(nil), configuration.KeyringIdentifiers...)
	features := append([]string(nil), configuration.RequiredFeatures...)
	present := make(map[string]struct{}, len(features))
	for _, feature := range features {
		present[feature] = struct{}{}
	}
	for _, required := range currentStorageSchema().features {
		if _, found := present[required]; !found {
			features = append(features, required)
		}
	}
	sort.Strings(keyrings)
	sort.Strings(features)
	for index, value := range keyrings {
		if value == "" || (index > 0 && value == keyrings[index-1]) {
			return storageformat.WriterSet{}, domain.NewError(domain.ErrorInvalid, "invalid portable keyring identifiers")
		}
	}
	for index, value := range features {
		if value == "" || (index > 0 && value == features[index-1]) {
			return storageformat.WriterSet{}, domain.NewError(domain.ErrorInvalid, "invalid portable required features")
		}
	}
	return storageformat.WriterSet{
		SchemaVersion: 1, WriterSetID: configuration.WriterSetID,
		WriterProtocolVersion: storageformat.WriterProtocolVersion,
		RequiredFeatures:      features, ConfigurationDigest: configuration.ConfigurationDigest,
		KeyringIdentifiers:    keyrings,
		MinimumReaderProtocol: 1, MaximumReaderProtocol: storageformat.WriterProtocolVersion,
		MinimumWriterProtocol: storageformat.WriterProtocolVersion, MaximumWriterProtocol: storageformat.WriterProtocolVersion,
	}, nil
}

func (e *Engine) step(ctx context.Context, name string) error {
	if e.scheduler == nil {
		return nil
	}
	return e.scheduler.Step(ctx, name)
}

func (e *Engine) observeMigration(progress MigrationProgress) {
	if e.migrationObserver != nil {
		e.migrationObserver(progress)
	}
}

func (e *Engine) initialize(ctx context.Context) error {
	bucketID, err := e.ids.OpaqueID()
	if err != nil {
		return err
	}
	superblock := storageformat.Superblock{
		SchemaVersion: 1, FormatID: storageformat.FormatID, BucketID: bucketID,
		CanonicalEncoder: storageformat.CanonicalEncoder, KeyFormatVersion: storageformat.KeyFormatVersion,
		WriterProtocolVersion: storageformat.WriterProtocolVersion, CreatedAt: e.clock.Now().UTC(),
		RequiredFeatures: append([]string(nil), e.writer.RequiredFeatures...),
	}
	superblockBody, err := storageformat.EncodeCanonical(superblock)
	if err != nil {
		return err
	}
	created := false
	if _, err = e.backend.Put(ctx, storageformat.SuperblockKey(), superblockBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		created = true
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	if created {
		if err := e.createOrVerifyEnvelope(ctx, storageformat.WriterSetKey(), writerSetSchema, e.writer); err != nil {
			return err
		}
		return e.createOrVerifyEnvelope(ctx, storageformat.WriteGateKey(), writeGateSchema, storageformat.WriteGate{
			SchemaVersion: 1, Epoch: 1, Mode: storageformat.GateOpen,
			WriterFeatures: append([]string(nil), e.writer.RequiredFeatures...),
		})
	}
	stored, err := e.backend.Get(ctx, storageformat.SuperblockKey())
	if err != nil {
		return err
	}
	var existing storageformat.Superblock
	if err := state.DecodeJSONWithLimit(stored.Body, &existing, storageformat.MaxCanonicalBytes); err != nil {
		return err
	}
	if err := validateCompatibleSuperblock(existing); err != nil {
		return err
	}
	schema, found := detectStorageSchema(existing.RequiredFeatures, e.writer.RequiredFeatures)
	if !found {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable superblock")
	}
	pendingMigration, err := e.storageMigrationPending(ctx)
	if err != nil {
		return err
	}
	if schema.id != currentStorageSchema().id || pendingMigration {
		if err := e.migrateStorageSchemaChain(ctx); err != nil {
			return err
		}
		// A completed migration is not the end of startup validation. In
		// particular, schema 008 opens the global gate before the idempotent
		// consistency-domain unfreeze suffix. Every winning and lagging replica
		// must pass through the ordinary current-schema reconciliation below.
	}
	if err := e.createOrVerifyEnvelope(ctx, storageformat.WriterSetKey(), writerSetSchema, e.writer); err != nil {
		return err
	}
	if err := e.createOrVerifyEnvelope(ctx, storageformat.WriteGateKey(), writeGateSchema, storageformat.WriteGate{
		SchemaVersion: 1, Epoch: 1, Mode: storageformat.GateOpen,
		WriterFeatures: append([]string(nil), e.writer.RequiredFeatures...),
	}); err != nil {
		return err
	}
	gateObject, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	gate, err = e.reconcileGateDomainFreeze(ctx, gateObject, gate)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible write-gate feature binding")
	}
	return nil
}

// reconcileGateDomainFreeze completes only the idempotent suffix already
// authorized by a durable gate transition. It never opens a gate or guesses an
// epoch: an open gate authorizes thawing exactly the immediately preceding
// freeze, while a closing or closed gate preserves its matching freeze. Every
// other combination fails closed.
func (e *Engine) reconcileGateDomainFreeze(ctx context.Context, gateObject objectstore.Object, gate storageformat.WriteGate) (storageformat.WriteGate, error) {
	for range 16 {
		catalog, found, err := e.readDomainCatalogIfPresent(ctx)
		if err != nil {
			return storageformat.WriteGate{}, err
		}
		// An absent or already-thawed catalog cannot disagree with the gate and
		// needs no second provider read. A concurrent closer is ordered by its
		// own gate CAS and any stale migration attempt reconciles against the
		// winner's durable completion markers.
		if !found || catalog.head.FreezeEpoch == 0 {
			return gate, nil
		}
		latestObject, _, latestGate, err := e.readGate(ctx)
		if err != nil {
			return storageformat.WriteGate{}, err
		}
		if latestObject.Version != gateObject.Version {
			gateObject, gate = latestObject, latestGate
			continue
		}
		switch {
		case gate.Mode == storageformat.GateOpen && catalog.head.FreezeEpoch <= gate.Epoch:
			// An open gate authorizes no domain freeze. Usually the catalog is
			// from the immediately preceding closure, but an arbitrarily late
			// migration helper can republish an even older epoch after multiple
			// adjacent edges have completed. Any non-future freeze is therefore
			// stale and safe to help; a future epoch still fails closed below.
			if err := newDomainCatalog(e.backend, e.scheduler).unfreeze(ctx, catalog.head.FreezeEpoch); err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
				return storageformat.WriteGate{}, err
			}
			gateObject, gate = latestObject, latestGate
			continue
		case gate.Mode == storageformat.GateClosing && catalog.head.FreezeEpoch < gate.Epoch:
			// A lagging worker from the preceding migration may publish its
			// idempotent freeze after one or more later migrations won their
			// closing CAS. The current closer cannot freeze at its own epoch
			// until every older suffix is removed, so helping the old unfreeze
			// is both safe and required for forward progress.
			if err := newDomainCatalog(e.backend, e.scheduler).unfreeze(ctx, catalog.head.FreezeEpoch); err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
				return storageformat.WriteGate{}, err
			}
			gateObject, gate = latestObject, latestGate
			continue
		case (gate.Mode == storageformat.GateClosing || gate.Mode == storageformat.GateClosed) && gate.Epoch == catalog.head.FreezeEpoch:
			// Checkpoint closure is intentionally durable across restarts.
			return gate, nil
		default:
			return storageformat.WriteGate{}, domain.NewError(domain.ErrorPreconditionFailed, fmt.Sprintf("write gate and consistency-domain freeze disagree (gateMode=%s gateEpoch=%d catalogFreezeEpoch=%d)", gate.Mode, gate.Epoch, catalog.head.FreezeEpoch))
		}
	}
	return storageformat.WriteGate{}, domain.NewError(domain.ErrorUnavailable, "write gate and consistency-domain freeze remained contended")
}

func validateCompatibleSuperblock(superblock storageformat.Superblock) error {
	if superblock.SchemaVersion != 1 || superblock.FormatID != storageformat.FormatID || superblock.BucketID == "" || superblock.CanonicalEncoder != storageformat.CanonicalEncoder || superblock.KeyFormatVersion != storageformat.KeyFormatVersion || superblock.WriterProtocolVersion != storageformat.WriterProtocolVersion || superblock.CreatedAt.IsZero() || !storageformat.SortedUnique(superblock.RequiredFeatures) {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable superblock")
	}
	return nil
}

func (e *Engine) createOrVerifyEnvelope(ctx context.Context, key objectstore.Key, schema string, payload any) error {
	body, err := storageformat.EncodeEnvelope(schema, key, 1, payload)
	if err != nil {
		return err
	}
	if _, err = e.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	stored, err := e.backend.Get(ctx, key)
	if err != nil {
		return err
	}
	switch expected := payload.(type) {
	case storageformat.WriterSet:
		var envelope storageformat.Envelope
		var actual storageformat.WriterSet
		if err := storageformat.DecodeEnvelope(stored.Body, key, schema, &envelope, &actual); err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, expected) {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable writer set")
		}
	case storageformat.WriteGate:
		var envelope storageformat.Envelope
		var actual storageformat.WriteGate
		if err := storageformat.DecodeEnvelope(stored.Body, key, schema, &envelope, &actual); err != nil {
			return err
		}
		if err := storageformat.ValidateGate(actual); err != nil {
			return err
		}
	}
	return nil
}
