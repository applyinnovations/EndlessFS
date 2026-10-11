package portable

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSchema012AuthorityVerifierBindsEveryAdjacentEdgeField(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*storageMigration)
	}{
		{name: "id", mutate: func(edge *storageMigration) { edge.id = storageMigration010To011 }},
		{name: "source", mutate: func(edge *storageMigration) { edge.from = storageSchema010 }},
		{name: "target", mutate: func(edge *storageMigration) { edge.to = storageSchema011 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			edge := schemaMigration011To012
			test.mutate(&edge)
			reads := 0
			engine := &Engine{backend: &migrationScriptBackend{Backend: objectmemory.New(), get: func(context.Context, objectstore.Key) (objectstore.Object, error) {
				reads++
				return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "unexpected authority read")
			}}}
			if err := engine.verifySchema012Authority(t.Context(), edge); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("wrong edge accepted: %v", err)
			}
			if reads != 0 {
				t.Fatal("wrong edge accessed authority before validating its binding")
			}
		})
	}
}

func TestSchema012AuthorityVerifierRequiresItsCompleteFrozenAuthority(t *testing.T) {
	for _, test := range []struct {
		name        string
		closeID     string
		closing     bool
		unfrozen    bool
		unavailable bool
		want        error
	}{
		{name: "open", want: domain.ErrPreconditionFailed},
		{name: "closing", closing: true, want: domain.ErrPreconditionFailed},
		{name: "closed-marker-without-frozen-authority", unfrozen: true, want: domain.ErrPreconditionFailed},
		{name: "another-checkpoint", closeID: "another-checkpoint", want: domain.ErrPreconditionFailed},
		{name: "unavailable-gate", closeID: schemaMigration011To012.checkpointID, unavailable: true, want: domain.ErrUnavailable},
		{name: "frozen-authority", closeID: schemaMigration011To012.checkpointID},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			backend, engine := currentMigrationEngine(t)
			key := state.MustKey(state.NamespacePreferences, "WVhXWVhXWVhXWVhXWVhXWQ")
			body := []byte(`{"themeID":"endlessfs-dark"}`)
			if _, err := engine.Create(ctx, key, body); err != nil {
				t.Fatal(err)
			}
			before, err := engine.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if test.closeID != "" {
				if err := engine.CloseWrites(ctx, test.closeID); err != nil {
					t.Fatal(err)
				}
			}
			if test.closing || test.unfrozen {
				rewriteMigrationGate(t, backend, func(gate *storageformat.WriteGate) {
					gate.Mode = storageformat.GateClosed
					if test.closing {
						gate.Mode = storageformat.GateClosing
					}
					gate.CheckpointID = schemaMigration011To012.checkpointID
				})
			}
			writes := 0
			engine.backend = &migrationScriptBackend{Backend: backend,
				get: func(ctx context.Context, key objectstore.Key) (objectstore.Object, error) {
					if test.unavailable && key == storageformat.WriteGateKey() {
						return objectstore.Object{}, domain.NewError(domain.ErrorUnavailable, "injected authority gate read failure")
					}
					return backend.Get(ctx, key)
				},
				put: func(ctx context.Context, key objectstore.Key, body []byte, condition objectstore.PutCondition) (objectstore.NativeVersion, error) {
					writes++
					return backend.Put(ctx, key, body, condition)
				},
			}
			if err := engine.verifySchema012Authority(ctx, schemaMigration011To012); !errors.Is(err, test.want) {
				t.Fatalf("authority verification = %v; want %v", err, test.want)
			}
			if writes != 0 {
				t.Fatal("authority verification mutated the storage set")
			}
			engine.backend = backend
			after, err := engine.Get(ctx, key)
			if err != nil || after.Version != before.Version || !bytes.Equal(after.Data, body) {
				t.Fatalf("authority changed during verification: %+v, %v", after, err)
			}
		})
	}
}
