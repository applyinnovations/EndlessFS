package portable_test

import (
	"bytes"
	"context"
	"encoding/base64"
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

func TestEightReplicaConcurrentCASHasOneWinner(t *testing.T) {
	backend := objectmemory.New()
	engines := make([]*portable.Engine, 8)
	for index := range engines {
		engines[index] = newEngine(t, backend, byte(10+index))
	}
	key := state.MustKey(state.NamespaceInvites, "eight-replica-cas")
	version, err := engines[0].Create(context.Background(), key, []byte("initial"))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var winners atomic.Int32
	var wait sync.WaitGroup
	for index, engine := range engines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if _, swapErr := engine.CompareAndSwap(context.Background(), key, version, []byte{byte(index)}); swapErr == nil {
				winners.Add(1)
			} else if !errors.Is(swapErr, domain.ErrPreconditionFailed) {
				t.Errorf("CompareAndSwap() error = %v", swapErr)
			}
		}()
	}
	close(start)
	wait.Wait()
	if winners.Load() != 1 {
		t.Fatalf("CAS winners = %d", winners.Load())
	}
}

func TestPortableStateCursorMovesAcrossReplicasAndKeepsImmutableSnapshot(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC))
	first := openEngine(t, backend, clock, 31, nil)
	second := openEngine(t, backend, clock, 32, nil)
	prefix := state.MustPrefix(state.NamespaceSessions, "cursor-owner")
	versions := make(map[string]state.Version)
	for index := range 5 {
		key := state.MustKey(state.NamespaceSessions, "cursor-owner", string(rune('a'+index)))
		version, err := first.Create(context.Background(), key, []byte{byte('a' + index)})
		if err != nil {
			t.Fatal(err)
		}
		versions[key.String()] = version
	}
	page, err := first.List(context.Background(), prefix, state.PageRequest{Limit: 2})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("first List() = %+v, %v", page, err)
	}
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if decodeErr != nil || bytes.Contains(decoded, []byte("endlessfs/v1/")) || bytes.Contains(decoded, []byte("cursor-owner")) {
		t.Fatalf("state cursor exposed internal scope: %q, %v", decoded, decodeErr)
	}
	tampered := page.NextCursor[:len(page.NextCursor)-1] + "A"
	if _, err := second.List(context.Background(), prefix, state.PageRequest{Limit: 2, Cursor: tampered}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("tampered cursor error = %v", err)
	}
	changed := state.MustKey(state.NamespaceSessions, "cursor-owner", "c")
	if _, err := first.CompareAndSwap(context.Background(), changed, versions[changed.String()], []byte("changed")); err != nil {
		t.Fatal(err)
	}
	page, err = second.List(context.Background(), prefix, state.PageRequest{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(page.Items) != 2 || string(page.Items[0].Value.Data) != "c" {
		t.Fatalf("cross-replica snapshot page = %+v, %v", page, err)
	}
	clock.Advance(11 * time.Minute)
	if _, err := first.List(context.Background(), prefix, state.PageRequest{Limit: 2, Cursor: page.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("expired cursor error = %v", err)
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
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func deterministic(seed byte, size int) []byte {
	value := make([]byte, size)
	state := uint64(seed) + 0x9e3779b97f4a7c15
	for index := range value {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		value[index] = byte(state >> 29)
	}
	return value
}
