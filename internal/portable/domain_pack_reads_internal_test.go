package portable

import (
	"context"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestMetadataPackSharedReadCancellationAndRetry(t *testing.T) {
	for _, cancelLeader := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiter", true: "leader"}[cancelLeader], func(t *testing.T) {
			reads := &consistencyDomainPackReads{}
			key := storageformat.DomainPagePackKey(storageformat.DomainNamespace, "owner", storageformat.Digest([]byte("pack")))
			started, release := make(chan struct{}), make(chan struct{})
			leaderCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			leader := make(chan error, 1)
			go func() {
				_, err := reads.read(leaderCtx, key, func() (map[string]storageformat.DomainPage, error) {
					close(started)
					select {
					case <-leaderCtx.Done():
						return nil, domain.WrapError(domain.ErrorUnavailable, "read canceled", leaderCtx.Err())
					case <-release:
						return map[string]storageformat.DomainPage{"valid": {}}, nil
					}
				})
				leader <- err
			}()
			<-started
			waiterCtx, cancelWaiter := context.WithCancel(context.Background())
			defer cancelWaiter()
			ready := make(chan struct{}, 1)
			waiting := &packReadWaitingContext{Context: waiterCtx, ready: ready}
			waiter := make(chan error, 1)
			go func() {
				pages, err := reads.read(waiting, key, func() (map[string]storageformat.DomainPage, error) {
					return map[string]storageformat.DomainPage{"valid": {}}, nil
				})
				if err == nil && len(pages) != 1 {
					err = errors.New("lost immutable result")
				}
				waiter <- err
			}()
			<-ready
			if cancelLeader {
				cancel()
				if err := <-leader; !errors.Is(err, context.Canceled) {
					t.Fatalf("leader: %v", err)
				}
				if err := <-waiter; err != nil {
					t.Fatalf("healthy waiter inherited leader cancellation: %v", err)
				}
			} else {
				cancelWaiter()
				if err := <-waiter; !errors.Is(err, context.Canceled) {
					t.Fatalf("waiter: %v", err)
				}
				close(release)
				if err := <-leader; err != nil {
					t.Fatalf("waiter canceled leader: %v", err)
				}
			}
			if len(reads.loads) != 0 {
				t.Fatal("completed read retained")
			}
		})
	}
}

func TestMetadataPackReadScopeErrorsAndNoIdleCache(t *testing.T) {
	reads := &consistencyDomainPackReads{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := storageformat.DomainPagePackKey(storageformat.DomainNamespace, "owner-a", storageformat.Digest([]byte("pack")))
	if _, err := reads.read(ctx, key, func() (map[string]storageformat.DomainPage, error) { t.Fatal("canceled fetch ran"); return nil, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := reads.read(context.Background(), key, func() (map[string]storageformat.DomainPage, error) {
			close(started)
			<-release
			return nil, domain.ErrInvalid
		})
		finished <- err
	}()
	<-started
	for _, other := range []objectstore.Key{
		storageformat.DomainPagePackKey(storageformat.DomainNamespace, "owner-b", storageformat.Digest([]byte("pack"))),
		storageformat.DomainPagePackKey(storageformat.DomainOwnerControl, "owner-a", storageformat.Digest([]byte("pack"))),
		storageformat.DomainPagePackKey(storageformat.DomainNamespace, "owner-a", storageformat.Digest([]byte("other-pack"))),
	} {
		if _, err := reads.read(context.Background(), other, func() (map[string]storageformat.DomainPage, error) { return nil, nil }); err != nil {
			t.Fatal(err)
		}
	}
	ready := make(chan struct{}, 1)
	waiting := &packReadWaitingContext{Context: context.Background(), ready: ready}
	waiter := make(chan error, 1)
	go func() {
		_, err := reads.read(waiting, key, func() (map[string]storageformat.DomainPage, error) {
			t.Error("duplicate corrupt fetch")
			return nil, nil
		})
		waiter <- err
	}()
	<-ready
	close(release)
	if err := <-finished; !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
	if err := <-waiter; !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
	for range 2 {
		called := false
		_, err := reads.read(context.Background(), key, func() (map[string]storageformat.DomainPage, error) { called = true; return nil, nil })
		if err != nil || !called {
			t.Fatalf("idle pack or error cached: %v", err)
		}
	}
	if len(reads.loads) != 0 {
		t.Fatal("idle cache retained")
	}
}
