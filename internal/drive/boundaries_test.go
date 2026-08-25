package drive

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type repositoryFaultStore struct {
	get    func(context.Context, state.Key) (state.Value, error)
	list   func(context.Context, state.Prefix, state.PageRequest) (state.Page, error)
	create func(context.Context, state.Key, []byte) (state.Version, error)
	cas    func(context.Context, state.Key, state.Version, []byte) (state.Version, error)
}

func (s repositoryFaultStore) Get(ctx context.Context, key state.Key) (state.Value, error) {
	if s.get != nil {
		return s.get(ctx, key)
	}
	return state.Value{}, domain.NewError(domain.ErrorNotFound, "missing")
}
func (s repositoryFaultStore) List(ctx context.Context, prefix state.Prefix, request state.PageRequest) (state.Page, error) {
	if s.list != nil {
		return s.list(ctx, prefix, request)
	}
	return state.Page{}, nil
}
func (s repositoryFaultStore) Create(ctx context.Context, key state.Key, data []byte) (state.Version, error) {
	if s.create != nil {
		return s.create(ctx, key, data)
	}
	return "v1", nil
}
func (s repositoryFaultStore) CompareAndSwap(ctx context.Context, key state.Key, version state.Version, data []byte) (state.Version, error) {
	if s.cas != nil {
		return s.cas(ctx, key, version, data)
	}
	return "v2", nil
}
func (repositoryFaultStore) Delete(context.Context, state.Key, state.Version) error { return nil }

func driveTestUserID(t *testing.T) domain.UserID {
	t.Helper()
	value, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRepositoryFaultDecodeAndPaginationMatrix(t *testing.T) {
	owner := driveTestUserID(t)
	unavailable := domain.NewError(domain.ErrorUnavailable, "fault")
	repository := newRepository(repositoryFaultStore{
		list: func(context.Context, state.Prefix, state.PageRequest) (state.Page, error) { return state.Page{}, unavailable },
		get:  func(context.Context, state.Key) (state.Value, error) { return state.Value{}, unavailable },
	})
	if _, err := repository.shares(context.Background(), owner); !errors.Is(err, unavailable) {
		t.Fatalf("shares list fault = %v", err)
	}
	if _, _, err := repository.shareByID(context.Background(), owner, "missing"); !errors.Is(err, unavailable) {
		t.Fatalf("share by ID read fault = %v", err)
	}
	corrupt := state.Item{Value: state.Value{Data: []byte(`{"corrupt":true}`), Version: "v1"}}
	repository = newRepository(repositoryFaultStore{list: func(context.Context, state.Prefix, state.PageRequest) (state.Page, error) {
		return state.Page{Items: []state.Item{corrupt}}, nil
	}})
	if _, err := listRecords[model.Share](context.Background(), repository.store, state.MustPrefix(state.NamespaceShares)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("share list corrupt record = %v", err)
	}
	repository = newRepository(repositoryFaultStore{get: func(context.Context, state.Key) (state.Value, error) {
		return state.Value{Data: []byte(`{"corrupt":true}`), Version: "v1"}, nil
	}})
	if _, _, err := getRecord[model.Share](context.Background(), repository.store, state.MustKey(state.NamespaceShares, "hash")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("get corrupt record = %v", err)
	}
	pageCalls := 0
	repository = newRepository(repositoryFaultStore{list: func(_ context.Context, _ state.Prefix, request state.PageRequest) (state.Page, error) {
		pageCalls++
		if request.Cursor == "" {
			return state.Page{NextCursor: "next"}, nil
		}
		return state.Page{}, nil
	}})
	if records, err := listRecords[model.Share](context.Background(), repository.store, state.MustPrefix(state.NamespaceShares)); err != nil || len(records) != 0 || pageCalls != 2 {
		t.Fatalf("paginated empty list = %+v calls=%d, %v", records, pageCalls, err)
	}
}

func TestRepositoryEncodeAndMutationErrorBranches(t *testing.T) {
	owner := driveTestUserID(t)
	repository := newRepository(repositoryFaultStore{})
	if err := repository.create(context.Background(), state.MustKey(state.NamespaceTrash, "invalid"), struct{ Value chan int }{Value: make(chan int)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("create marshal error = %v", err)
	}
	if err := repository.updateShare(context.Background(), model.Share{}, "v1"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("update invalid share = %v", err)
	}
	if err := createOrMatch(domain.NewError(domain.ErrorUnavailable, "fault")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("non-conflict create = %v", err)
	}
	if _, _, err := repository.shareByID(context.Background(), owner, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing share = %v", err)
	}
}
