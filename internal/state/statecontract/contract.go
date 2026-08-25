// Package statecontract defines the reusable StateStore contract suite.
package statecontract

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type Factory func(t *testing.T) state.Store

func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("create get copy safety", func(t *testing.T) {
		store := factory(t)
		key := state.MustKey(state.NamespaceUsers, "user-a", "profile")
		input := []byte(`{"value":1}`)
		version, err := store.Create(context.Background(), key, input)
		if err != nil || version == "" {
			t.Fatalf("Create() = %q, %v", version, err)
		}
		input[0] = 'x'
		value, err := store.Get(context.Background(), key)
		if err != nil || string(value.Data) != `{"value":1}` || value.Version != version {
			t.Fatalf("Get() = %+v, %v", value, err)
		}
		value.Data[0] = 'x'
		again, _ := store.Get(context.Background(), key)
		if string(again.Data) != `{"value":1}` {
			t.Fatal("Get() exposed mutable store data")
		}
		if _, err := store.Create(context.Background(), key, []byte(`{"value":2}`)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("duplicate Create() error = %v", err)
		}
	})

	t.Run("cas and delete preconditions", func(t *testing.T) {
		store := factory(t)
		key := state.MustKey(state.NamespaceAccounts, "user-a")
		version, _ := store.Create(context.Background(), key, []byte("one"))
		if _, err := store.CompareAndSwap(context.Background(), key, "stale", []byte("two")); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("stale CAS error = %v", err)
		}
		next, err := store.CompareAndSwap(context.Background(), key, version, []byte("two"))
		if err != nil || next == version {
			t.Fatalf("CAS() = %q, %v", next, err)
		}
		if err := store.Delete(context.Background(), key, version); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("stale Delete() error = %v", err)
		}
		if err := store.Delete(context.Background(), key, next); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		if _, err := store.Get(context.Background(), key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("Get() after delete error = %v", err)
		}
	})

	t.Run("single create winner", func(t *testing.T) {
		store := factory(t)
		key := state.MustKey(state.NamespaceBootstrap, "claim")
		var successes atomic.Int32
		var wait sync.WaitGroup
		for index := range 32 {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				if _, err := store.Create(context.Background(), key, []byte(fmt.Sprint(index))); err == nil {
					successes.Add(1)
				} else if !errors.Is(err, domain.ErrConflict) {
					t.Errorf("Create() error = %v", err)
				}
			}(index)
		}
		wait.Wait()
		if successes.Load() != 1 {
			t.Fatalf("successful creates = %d, want 1", successes.Load())
		}
	})

	t.Run("single cas winner", func(t *testing.T) {
		store := factory(t)
		key := state.MustKey(state.NamespaceInvites, "invite")
		version, _ := store.Create(context.Background(), key, []byte("unused"))
		var successes atomic.Int32
		var wait sync.WaitGroup
		for index := range 32 {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				if _, err := store.CompareAndSwap(context.Background(), key, version, []byte(fmt.Sprint(index))); err == nil {
					successes.Add(1)
				} else if !errors.Is(err, domain.ErrPreconditionFailed) {
					t.Errorf("CompareAndSwap() error = %v", err)
				}
			}(index)
		}
		wait.Wait()
		if successes.Load() != 1 {
			t.Fatalf("successful swaps = %d, want 1", successes.Load())
		}
	})

	t.Run("stable scoped pagination", func(t *testing.T) {
		store := factory(t)
		for _, user := range []string{"a", "b"} {
			for index := range 5 {
				key := state.MustKey(state.NamespaceSessions, user, fmt.Sprint(index))
				if _, err := store.Create(context.Background(), key, []byte(user)); err != nil {
					t.Fatal(err)
				}
			}
		}
		prefixA := state.MustPrefix(state.NamespaceSessions, "a")
		first, err := store.List(context.Background(), prefixA, state.PageRequest{Limit: 2})
		if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
			t.Fatalf("first List() = %+v, %v", first, err)
		}
		newKey := state.MustKey(state.NamespaceSessions, "a", "later")
		_, _ = store.Create(context.Background(), newKey, []byte("later"))
		if _, err := store.List(context.Background(), state.MustPrefix(state.NamespaceSessions, "b"), state.PageRequest{Limit: 2, Cursor: first.NextCursor}); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("cross-prefix cursor error = %v", err)
		}
		second, err := store.List(context.Background(), prefixA, state.PageRequest{Limit: 2, Cursor: first.NextCursor})
		if err != nil || len(second.Items) != 2 {
			t.Fatalf("second List() = %+v, %v", second, err)
		}
		third, err := store.List(context.Background(), prefixA, state.PageRequest{Limit: 2, Cursor: second.NextCursor})
		if err != nil || len(third.Items) != 1 || third.NextCursor != "" {
			t.Fatalf("third List() = %+v, %v", third, err)
		}
	})
}
