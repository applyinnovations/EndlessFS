// Package objectstorecontract defines the reusable atomic object-backend contract.
package objectstorecontract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
)

type Factory func(*testing.T) objectstore.Backend

func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("create get and copy safety", func(t *testing.T) {
		backend := factory(t)
		key := objectstore.MustKey("endlessfs/v1/state/users/a.json")
		input := []byte("one")
		version, err := backend.Put(context.Background(), key, input, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if err != nil || version == "" {
			t.Fatalf("Put(create) = %q, %v", version, err)
		}
		input[0] = 'x'
		got, err := backend.Get(context.Background(), key)
		if err != nil || string(got.Body) != "one" || got.Version != version {
			t.Fatalf("Get() = %+v, %v", got, err)
		}
		got.Body[0] = 'x'
		again, _ := backend.Get(context.Background(), key)
		if string(again.Body) != "one" {
			t.Fatal("Get exposed mutable backend data")
		}
		if _, err := backend.Put(context.Background(), key, []byte("two"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("duplicate create error = %v", err)
		}
	})

	t.Run("single conditional winner", func(t *testing.T) {
		backend := factory(t)
		key := objectstore.MustKey("endlessfs/v1/control/write-gate.json")
		version, err := backend.Put(context.Background(), key, []byte("open"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if err != nil {
			t.Fatal(err)
		}
		var winners atomic.Int32
		var wait sync.WaitGroup
		for index := range 32 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, putErr := backend.Put(context.Background(), key, []byte(fmt.Sprintf("writer-%d", index)), objectstore.PutCondition{Mode: objectstore.PutMatch, Version: version})
				if putErr == nil {
					winners.Add(1)
				} else if !errors.Is(putErr, domain.ErrPreconditionFailed) {
					t.Errorf("Put(match) error = %v", putErr)
				}
			}()
		}
		wait.Wait()
		if winners.Load() != 1 {
			t.Fatalf("conditional winners = %d, want 1", winners.Load())
		}
	})

	t.Run("strong paginated list", func(t *testing.T) {
		backend := factory(t)
		for index := range 7 {
			key := objectstore.MustKey(fmt.Sprintf("endlessfs/v1/admissions/1/%02d.json", index))
			if _, err := backend.Put(context.Background(), key, []byte{byte(index)}, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
				t.Fatal(err)
			}
		}
		var keys []string
		request := objectstore.ListRequest{Prefix: "endlessfs/v1/admissions/1/", Limit: 3}
		for {
			page, err := backend.List(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range page.Objects {
				keys = append(keys, item.Key.String())
			}
			if page.NextCursor == "" {
				break
			}
			request.Cursor = page.NextCursor
		}
		if len(keys) != 7 || !sort.StringsAreSorted(keys) {
			t.Fatalf("listed keys = %v", keys)
		}
	})

	t.Run("conditional copy and delete", func(t *testing.T) {
		backend := factory(t)
		source := objectstore.MustKey("endlessfs/v1/staging/u/op/artifact")
		destination := objectstore.MustKey("endlessfs/v1/fs/u/blobs/blob")
		sourceVersion, _ := backend.Put(context.Background(), source, []byte("payload"), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		result, err := backend.Copy(context.Background(), source, destination, objectstore.CopyCondition{SourceVersion: sourceVersion, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}})
		if err != nil || result.Version == "" {
			t.Fatalf("Copy() = %+v, %v", result, err)
		}
		copied, _ := backend.Get(context.Background(), destination)
		if !bytes.Equal(copied.Body, []byte("payload")) {
			t.Fatalf("copied body = %q", copied.Body)
		}
		if err := backend.Delete(context.Background(), source, objectstore.DeleteCondition{Version: "stale"}); !errors.Is(err, domain.ErrPreconditionFailed) {
			t.Fatalf("stale Delete() error = %v", err)
		}
		if err := backend.Delete(context.Background(), source, objectstore.DeleteCondition{Version: sourceVersion}); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Get(context.Background(), source); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("Get(deleted) error = %v", err)
		}
	})
}
