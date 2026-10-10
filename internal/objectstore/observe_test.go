package objectstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

func TestContractObservedBackendPreservesProviderVectorAndConditionalBehavior(t *testing.T) {
	var baseline []providerbudget.Event
	for _, enabled := range []bool{false, true} {
		ledger := providerbudget.NewLedger()
		var backend objectstore.Backend = budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
		ctx := context.Background()
		if enabled {
			backend = objectstore.Observe(backend, telemetry.State)
			ctx = telemetry.Context(ctx, telemetry.New(nil, nil))
		}
		key := objectstore.MustKey("endlessfs/v1/test/source")
		body := []byte("sentinel-private-metadata")
		version, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Head(ctx, key); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Verify(ctx, key, objectstore.IntegrityFor(body)); err != nil {
			t.Fatal(err)
		}
		if value, err := backend.Get(ctx, key); err != nil || !bytes.Equal(value.Body, body) {
			t.Fatalf("Get: %+v %v", value, err)
		}
		opened, err := backend.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if value, err := io.ReadAll(opened.Body); err != nil || !bytes.Equal(value, body) {
			t.Fatal("stream changed")
		}
		if err := opened.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.List(ctx, objectstore.ListRequest{Prefix: "endlessfs/v1/test/"}); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("conditional denial changed")
		}
		destination := objectstore.MustKey("endlessfs/v1/test/copy")
		if _, err := backend.Copy(ctx, key, destination, objectstore.CopyCondition{SourceVersion: version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); err != nil {
			t.Fatal(err)
		}
		if err := backend.Delete(ctx, key, objectstore.DeleteCondition{Version: version}); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Get(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("missing-object result changed")
		}
		if !enabled {
			baseline = ledger.Events()
		} else if !reflect.DeepEqual(baseline, ledger.Events()) {
			t.Fatalf("telemetry changed provider vector:\nbefore=%+v\nafter=%+v", baseline, ledger.Events())
		}
	}
}

type atomicOnly struct{ objectstore.Backend }

func TestObservedBackendPreservesOptionalTransferSupport(t *testing.T) {
	raw := objectmemory.New()
	if _, ok := objectstore.Observe(raw, telemetry.Files).(objectstore.DirectTransferBackend); !ok {
		t.Fatal("existing transfer support disappeared")
	}
	if _, ok := objectstore.Observe(atomicOnly{raw}, telemetry.State).(objectstore.DirectTransferBackend); ok {
		t.Fatal("absent transfer support was invented")
	}
}
