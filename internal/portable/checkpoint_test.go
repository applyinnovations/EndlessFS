package portable_test

import (
	"context"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func TestCheckpointDetectsAuthoritativeCorruption(t *testing.T) {
	backend := objectmemory.New()
	clock := domain.NewFixedClock(time.Date(2037, 1, 1, 0, 0, 0, 0, time.UTC))
	engine := openEngine(t, backend, clock, 21, nil)
	key := state.MustKey(state.NamespaceUsers, "checkpoint-user")
	if _, err := engine.Create(context.Background(), key, []byte("valid")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateCheckpoint(context.Background(), "checkpoint-corruption"); err != nil {
		t.Fatal(err)
	}
	if err := engine.VerifyCheckpoint(context.Background(), "checkpoint-corruption"); err != nil {
		t.Fatalf("VerifyCheckpoint() error = %v", err)
	}
	objects := backend.Export()
	var target string
	for objectKey := range objects {
		if len(objectKey) > len("endlessfs/v1/state/") && objectKey[:len("endlessfs/v1/state/")] == "endlessfs/v1/state/" {
			target = objectKey
			break
		}
	}
	if target == "" {
		t.Fatal("state object not found")
	}
	parsed := objectstore.MustKey(target)
	current, _ := backend.Get(context.Background(), parsed)
	if _, err := backend.Put(context.Background(), parsed, []byte("corrupt"), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: current.Version}); err != nil {
		t.Fatal(err)
	}
	if err := engine.VerifyCheckpoint(context.Background(), "checkpoint-corruption"); err == nil {
		t.Fatal("VerifyCheckpoint() accepted corrupt authoritative object")
	}
}
