// Package portable implements provider and state semantics over an object backend.
package portable

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	superblockSchema  = "superblock-v1"
	writerSetSchema   = "writer-set-v1"
	writeGateSchema   = "write-gate-v1"
	admissionSchema   = "admission-v1"
	stateRecordSchema = "state-record-v1"
)

type WriterConfiguration struct {
	WriterSetID         string
	ConfigurationDigest string
	KeyringIdentifiers  []string
	RequiredFeatures    []string
}

type Options struct {
	Backend  objectstore.Backend
	Clock    domain.Clock
	IDs      *domain.IDGenerator
	Writer   WriterConfiguration
	LeaseTTL time.Duration
}

type stateListSnapshot struct {
	prefix string
	limit  int
	items  []state.Item
	index  int
}

type Engine struct {
	backend  objectstore.Backend
	clock    domain.Clock
	ids      *domain.IDGenerator
	writer   storageformat.WriterSet
	leaseTTL time.Duration

	snapshotMu        sync.Mutex
	snapshots         map[string]*stateListSnapshot
	admissionSequence atomic.Uint64
}

func Open(ctx context.Context, options Options) (*Engine, error) {
	if options.Backend == nil || options.Clock == nil || options.IDs == nil || options.Writer.WriterSetID == "" || options.Writer.ConfigurationDigest == "" || len(options.Writer.KeyringIdentifiers) == 0 || options.LeaseTTL <= 0 {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid portable engine configuration")
	}
	keyrings := append([]string(nil), options.Writer.KeyringIdentifiers...)
	features := append([]string(nil), options.Writer.RequiredFeatures...)
	sort.Strings(keyrings)
	sort.Strings(features)
	writer := storageformat.WriterSet{
		SchemaVersion: 1, WriterSetID: options.Writer.WriterSetID,
		WriterProtocolVersion: storageformat.WriterProtocolVersion,
		RequiredFeatures:      features, ConfigurationDigest: options.Writer.ConfigurationDigest,
		KeyringIdentifiers:    keyrings,
		MinimumReaderProtocol: 1, MaximumReaderProtocol: storageformat.WriterProtocolVersion,
		MinimumWriterProtocol: storageformat.WriterProtocolVersion, MaximumWriterProtocol: storageformat.WriterProtocolVersion,
	}
	engine := &Engine{backend: options.Backend, clock: options.Clock, ids: options.IDs, writer: writer, leaseTTL: options.LeaseTTL, snapshots: make(map[string]*stateListSnapshot)}
	if err := engine.initialize(ctx); err != nil {
		return nil, err
	}
	return engine, nil
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
	if _, err = e.backend.Put(ctx, storageformat.SuperblockKey(), superblockBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return err
		}
		stored, getErr := e.backend.Get(ctx, storageformat.SuperblockKey())
		if getErr != nil {
			return getErr
		}
		var existing storageformat.Superblock
		if decodeErr := state.DecodeJSONWithLimit(stored.Body, &existing, storageformat.MaxCanonicalBytes); decodeErr != nil {
			return decodeErr
		}
		if existing.FormatID != storageformat.FormatID || existing.CanonicalEncoder != storageformat.CanonicalEncoder || existing.KeyFormatVersion != storageformat.KeyFormatVersion || existing.WriterProtocolVersion != storageformat.WriterProtocolVersion || !reflect.DeepEqual(existing.RequiredFeatures, e.writer.RequiredFeatures) {
			return domain.NewError(domain.ErrorPreconditionFailed, "incompatible portable superblock")
		}
	}
	if err := e.createOrVerifyEnvelope(ctx, storageformat.WriterSetKey(), writerSetSchema, e.writer); err != nil {
		return err
	}
	return e.createOrVerifyEnvelope(ctx, storageformat.WriteGateKey(), writeGateSchema, storageformat.WriteGate{SchemaVersion: 1, Epoch: 1, Mode: storageformat.GateOpen})
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

func envelopeVersion(body []byte) state.Version {
	var envelope storageformat.Envelope
	_ = state.DecodeJSONWithLimit(body, &envelope, storageformat.MaxCanonicalBytes)
	return state.Version(envelope.LogicalVersion)
}
