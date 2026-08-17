package portable_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/state/statecontract"
)

func TestContractPortableStateStore(t *testing.T) {
	statecontract.Run(t, func(t *testing.T) state.Store {
		return newEngine(t, objectmemory.New(), 1)
	})
}

func TestPortableStateCASAcrossReplicas(t *testing.T) {
	backend := objectmemory.New()
	first := newEngine(t, backend, 2)
	second := newEngine(t, backend, 3)
	key := state.MustKey(state.NamespaceInvites, "shared")
	version, err := first.Create(context.Background(), key, []byte("unused"))
	if err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wait sync.WaitGroup
	for index, engine := range []*portable.Engine{first, second} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, swapErr := engine.CompareAndSwap(context.Background(), key, version, []byte{byte(index)}); swapErr == nil {
				winners.Add(1)
			} else if !errors.Is(swapErr, domain.ErrPreconditionFailed) {
				t.Errorf("CompareAndSwap() error = %v", swapErr)
			}
		}()
	}
	wait.Wait()
	if winners.Load() != 1 {
		t.Fatalf("CAS winners = %d", winners.Load())
	}
}

func TestPortableStateRawCopyPreservesLogicalVersions(t *testing.T) {
	source := objectmemory.New()
	engine := newEngine(t, source, 4)
	key := state.MustKey(state.NamespaceAccounts, "portable")
	version, err := engine.Create(context.Background(), key, []byte(`{"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "portable-copy"); err != nil {
		t.Fatal(err)
	}
	destination := objectmemory.New()
	if err := destination.Import(source.Export()); err != nil {
		t.Fatal(err)
	}
	reopened := newEngine(t, destination, 5)
	if err := reopened.OpenWrites(context.Background(), "portable-copy"); err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get(context.Background(), key)
	if err != nil || value.Version != version || string(value.Data) != `{"enabled":true}` {
		t.Fatalf("reopened Get() = %+v, %v", value, err)
	}
	if _, err := reopened.CompareAndSwap(context.Background(), key, version, []byte(`{"enabled":false}`)); err != nil {
		t.Fatalf("post-copy CAS error = %v", err)
	}
}

func newEngine(t *testing.T, backend *objectmemory.Backend, seed byte) *portable.Engine {
	t.Helper()
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend,
		Clock:   domain.NewFixedClock(time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)),
		IDs:     domain.NewIDGenerator(bytes.NewReader(deterministic(seed, 1<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID:         "d3JpdGVyLXNldC0wMDAx",
			ConfigurationDigest: "config-v1",
			KeyringIdentifiers:  []string{"session-v1"},
		},
		LeaseTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func deterministic(seed byte, size int) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = byte(index*31) + seed
	}
	return value
}
