// Package portable implements provider and state semantics over an object backend.
package portable

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"reflect"
	"sort"
	"sync/atomic"
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
	StepAdmissionAfterCandidate = "admission:after-candidate"
	StepStateAfterAdmitted      = "state:after-admitted"
	StepStateAfterBackend       = "state:after-backend"
)

type Engine struct {
	backend                            objectstore.Backend
	fileBackend                        objectstore.FileControlBackend
	separateFileBackend                bool
	clock                              domain.Clock
	ids                                *domain.IDGenerator
	writer                             storageformat.WriterSet
	leaseTTL                           time.Duration
	uploadTTL                          time.Duration
	downloadTTL                        time.Duration
	cursorAEAD                         cipher.AEAD
	cursorTTL                          time.Duration
	scheduler                          Scheduler
	migrationObserver                  func(MigrationProgress)
	forceResumableOperationPreparation bool // tests exercise the large-plan recovery path deterministically

	admissionSequence atomic.Uint64
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
		return e.migrateStorageSchemaChain(ctx)
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
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(gate.WriterFeatures, e.writer.RequiredFeatures) {
		return domain.NewError(domain.ErrorPreconditionFailed, "incompatible write-gate feature binding")
	}
	return nil
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
