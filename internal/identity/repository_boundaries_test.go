package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type identityFaultStore struct {
	get    func(context.Context, state.Key) (state.Value, error)
	list   func(context.Context, state.Prefix, state.PageRequest) (state.Page, error)
	create func(context.Context, state.Key, []byte) (state.Version, error)
	cas    func(context.Context, state.Key, state.Version, []byte) (state.Version, error)
	delete func(context.Context, state.Key, state.Version) error
}

func (s identityFaultStore) Get(ctx context.Context, key state.Key) (state.Value, error) {
	if s.get != nil {
		return s.get(ctx, key)
	}
	return state.Value{}, domain.NewError(domain.ErrorNotFound, "missing")
}
func (s identityFaultStore) List(ctx context.Context, prefix state.Prefix, request state.PageRequest) (state.Page, error) {
	if s.list != nil {
		return s.list(ctx, prefix, request)
	}
	return state.Page{}, nil
}
func (s identityFaultStore) Create(ctx context.Context, key state.Key, data []byte) (state.Version, error) {
	if s.create != nil {
		return s.create(ctx, key, data)
	}
	return "v1", nil
}
func (s identityFaultStore) CompareAndSwap(ctx context.Context, key state.Key, version state.Version, data []byte) (state.Version, error) {
	if s.cas != nil {
		return s.cas(ctx, key, version, data)
	}
	return "v2", nil
}
func (s identityFaultStore) Delete(ctx context.Context, key state.Key, version state.Version) error {
	if s.delete != nil {
		return s.delete(ctx, key, version)
	}
	return nil
}

func TestRepositoryGenericFaultDecodeAndPaginationMatrix(t *testing.T) {
	unavailable := domain.NewError(domain.ErrorUnavailable, "fault")
	store := identityFaultStore{list: func(context.Context, state.Prefix, state.PageRequest) (state.Page, error) {
		return state.Page{}, unavailable
	}}
	repository := NewRepository(store)
	if _, err := repository.UserExists(context.Background()); !errors.Is(err, unavailable) {
		t.Fatalf("user exists list fault = %v", err)
	}
	if _, err := listRecords[model.Account](context.Background(), store, state.MustPrefix(state.NamespaceAccounts)); !errors.Is(err, unavailable) {
		t.Fatalf("list records fault = %v", err)
	}
	if _, _, err := findRecord[model.Invite](context.Background(), store, state.MustPrefix(state.NamespaceInvites), func(model.Invite) bool { return false }); !errors.Is(err, unavailable) {
		t.Fatalf("find records fault = %v", err)
	}
	corrupt := state.Item{Value: state.Value{Data: []byte(`{"corrupt":true}`), Version: "v1"}}
	store.list = func(context.Context, state.Prefix, state.PageRequest) (state.Page, error) {
		return state.Page{Items: []state.Item{corrupt}}, nil
	}
	if _, err := listRecords[model.Account](context.Background(), store, state.MustPrefix(state.NamespaceAccounts)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt list record = %v", err)
	}
	if _, _, err := findRecord[model.Invite](context.Background(), store, state.MustPrefix(state.NamespaceInvites), func(model.Invite) bool { return false }); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt find record = %v", err)
	}
	store.get = func(context.Context, state.Key) (state.Value, error) {
		return state.Value{Data: []byte(`{"corrupt":true}`), Version: "v1"}, nil
	}
	if _, _, err := getRecord[model.Account](context.Background(), store, state.MustKey(state.NamespaceAccounts, "account")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("corrupt get record = %v", err)
	}
	pageCalls := 0
	store.list = func(_ context.Context, _ state.Prefix, request state.PageRequest) (state.Page, error) {
		pageCalls++
		if request.Cursor == "" {
			return state.Page{NextCursor: "next"}, nil
		}
		return state.Page{}, nil
	}
	if records, err := listRecords[model.Account](context.Background(), store, state.MustPrefix(state.NamespaceAccounts)); err != nil || len(records) != 0 || pageCalls != 2 {
		t.Fatalf("paginated list = %+v calls=%d, %v", records, pageCalls, err)
	}
	pageCalls = 0
	if _, _, err := findRecord[model.Invite](context.Background(), store, state.MustPrefix(state.NamespaceInvites), func(model.Invite) bool { return false }); !errors.Is(err, domain.ErrNotFound) || pageCalls != 2 {
		t.Fatalf("paginated find = calls=%d, %v", pageCalls, err)
	}
}

func TestRepositoryWrappersEncodingAndCredentialConsistency(t *testing.T) {
	store := state.NewMemoryStore()
	repository := NewRepository(store)
	owner := userID(t, 0x11)
	other := userID(t, 0x12)
	now := identityEpoch
	account := model.Account{SchemaVersion: model.SchemaVersion, UserID: owner, Status: model.AccountEnabled, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if accounts, err := repository.Accounts(context.Background()); err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %+v, %v", accounts, err)
	}
	if exists, err := repository.UserExists(context.Background()); err != nil || exists {
		t.Fatalf("empty users = %v, %v", exists, err)
	}
	name, _ := domain.ParseDisplayName("Owner")
	if err := repository.CreateProfile(context.Background(), model.Profile{UserID: owner, DisplayName: name}); err != nil {
		t.Fatal(err)
	}
	if exists, err := repository.UserExists(context.Background()); err != nil || !exists {
		t.Fatalf("existing users = %v, %v", exists, err)
	}
	if err := repository.CreateCredentialIndex(context.Background(), model.CredentialIndex{SchemaVersion: model.SchemaVersion, UserID: owner, CredentialIDs: []string{bearer(0x14)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Credentials(context.Background(), owner); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("missing indexed credential = %v", err)
	}
	credential := model.Credential{SchemaVersion: model.SchemaVersion, CredentialID: bearer(0x14), UserID: other, PublicKey: bearer(0x15), CreatedAt: now, LastUsedAt: now}
	if err := repository.CreateCredential(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Credentials(context.Background(), owner); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("foreign indexed credential = %v", err)
	}
	if err := createRecord(context.Background(), store, state.MustKey(state.NamespaceAccounts, "invalid"), struct{ Value chan int }{Value: make(chan int)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("create marshal error = %v", err)
	}
	if _, err := swapRecord(context.Background(), store, state.MustKey(state.NamespaceAccounts, "invalid"), "v1", struct{ Value chan int }{Value: make(chan int)}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("swap marshal error = %v", err)
	}
}

func TestRepositorySessionRevocationFaultMatrix(t *testing.T) {
	owner := userID(t, 0x21)
	now := identityEpoch
	session := model.Session{SchemaVersion: model.SchemaVersion, SessionTokenHash: secret.Hash(bearer(0x22)), UserID: owner, CSRFTokenHash: secret.Hash(bearer(0x23)), CreatedAt: now, ExpiresAt: now.Add(time.Hour), AuthnCredentialIDHash: secret.Hash(bearer(0x24))}
	data, err := state.EncodeJSON(&session)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := domain.NewError(domain.ErrorUnavailable, "fault")
	store := identityFaultStore{list: func(context.Context, state.Prefix, state.PageRequest) (state.Page, error) {
		return state.Page{}, unavailable
	}}
	if err := NewRepository(store).RevokeUserSessions(context.Background(), owner); !errors.Is(err, unavailable) {
		t.Fatalf("session list fault = %v", err)
	}
	store.list = func(context.Context, state.Prefix, state.PageRequest) (state.Page, error) {
		return state.Page{Items: []state.Item{{Value: state.Value{Data: []byte(`{"corrupt":true}`), Version: "v1"}}}}, nil
	}
	if err := NewRepository(store).RevokeUserSessions(context.Background(), owner); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("session decode fault = %v", err)
	}
	item := state.Item{Key: state.MustKey(state.NamespaceSessions, "session"), Value: state.Value{Data: data, Version: "v1"}}
	store.list = func(context.Context, state.Prefix, state.PageRequest) (state.Page, error) {
		return state.Page{Items: []state.Item{item}}, nil
	}
	store.delete = func(context.Context, state.Key, state.Version) error { return unavailable }
	if err := NewRepository(store).RevokeUserSessions(context.Background(), owner); !errors.Is(err, unavailable) {
		t.Fatalf("session delete fault = %v", err)
	}
	for _, ignored := range []error{domain.NewError(domain.ErrorNotFound, "race"), domain.NewError(domain.ErrorPreconditionFailed, "race")} {
		store.delete = func(context.Context, state.Key, state.Version) error { return ignored }
		if err := NewRepository(store).RevokeUserSessions(context.Background(), owner); err != nil {
			t.Fatalf("ignored session delete race = %v", err)
		}
	}
}
