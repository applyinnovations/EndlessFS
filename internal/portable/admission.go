package portable

import (
	"context"
	"time"

	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func (e *Engine) readGate(ctx context.Context) (objectstore.Object, storageformat.Envelope, storageformat.WriteGate, error) {
	object, err := e.backend.Get(ctx, storageformat.WriteGateKey())
	if err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.WriteGate{}, err
	}
	var envelope storageformat.Envelope
	var gate storageformat.WriteGate
	if err := storageformat.DecodeEnvelope(object.Body, storageformat.WriteGateKey(), writeGateSchema, &envelope, &gate); err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.WriteGate{}, err
	}
	if err := storageformat.ValidateGate(gate); err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.WriteGate{}, err
	}
	return object, envelope, gate, nil
}

func expired(now time.Time, deadline time.Time) bool { return !now.Before(deadline) }
