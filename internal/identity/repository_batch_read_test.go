package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type identityBatchStore struct {
	state.TransactionalStore
	gets       int
	batches    [][]state.Key
	read       func(context.Context, []state.Key) ([]state.Value, error)
	afterBatch func([]state.Key)
}

func (s *identityBatchStore) Get(ctx context.Context, key state.Key) (state.Value, error) {
	s.gets++
	return s.TransactionalStore.Get(ctx, key)
}

func (s *identityBatchStore) GetMany(ctx context.Context, keys []state.Key) ([]state.Value, error) {
	s.batches = append(s.batches, append([]state.Key(nil), keys...))
	if s.read != nil {
		return s.read(ctx, keys)
	}
	values := make([]state.Value, len(keys))
	for i, key := range keys {
		value, err := s.TransactionalStore.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		values[i] = value
	}
	if s.afterBatch != nil {
		s.afterBatch(keys)
	}
	return values, nil
}

func batchIdentityFixture(t *testing.T, count int) (*Repository, *identityBatchStore, domain.UserID, []string) {
	t.Helper()
	base := state.NewMemoryStore()
	repository := NewRepository(base)
	owner := userID(t, 0x51)
	name, err := domain.ParseDisplayName("Batch identity")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateProfile(t.Context(), model.Profile{UserID: owner, DisplayName: name}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateAccount(t.Context(), model.Account{SchemaVersion: model.SchemaVersion, UserID: owner, Status: model.AccountEnabled, CreatedAt: identityEpoch, UpdatedAt: identityEpoch}); err != nil {
		t.Fatal(err)
	}
	index := model.CredentialIndex{SchemaVersion: model.SchemaVersion, UserID: owner}
	for i := range count {
		id := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("credential-%04d", i)))
		index.CredentialIDs = append(index.CredentialIDs, id)
		if err := repository.CreateCredential(t.Context(), model.Credential{SchemaVersion: model.SchemaVersion, UserID: owner, CredentialID: id, PublicKey: "AQ", CreatedAt: identityEpoch, LastUsedAt: identityEpoch}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.CreateCredentialIndex(t.Context(), index); err != nil {
		t.Fatal(err)
	}
	index, _, err = repository.CredentialIndex(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	store := &identityBatchStore{TransactionalStore: base}
	repository.store = store
	return repository, store, owner, index.CredentialIDs
}

func TestRepositoryCredentialsUsesBoundedBatches(t *testing.T) {
	for _, count := range []int{1, 8, 1001} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			repository, store, owner, ids := batchIdentityFixture(t, count)
			credentials, err := repository.Credentials(t.Context(), owner)
			if err != nil || len(credentials) != count {
				t.Fatalf("credentials=%d error=%v", len(credentials), err)
			}
			if store.gets != 1 || len(store.batches) != (count+999)/1000 {
				t.Fatalf("index gets=%d credential batches=%d", store.gets, len(store.batches))
			}
			for _, batch := range store.batches {
				if len(batch) == 0 || len(batch) > 1000 {
					t.Fatalf("unbounded credential batch=%d", len(batch))
				}
			}
			for i, credential := range credentials {
				if credential.CredentialID != ids[i] || credential.UserID != owner {
					t.Fatal("credential order/owner changed")
				}
			}
		})
	}
}

func TestDiscoverableIdentityReadsKnownRecordsTogether(t *testing.T) {
	repository, store, owner, ids := batchIdentityFixture(t, 8)
	service := &Service{repository: repository}
	rawID, _ := base64.RawURLEncoding.DecodeString(ids[0])
	handle, _ := base64.RawURLEncoding.DecodeString(owner.String())
	user, err := service.resolveDiscoverable(t.Context())(rawID, handle)
	if err != nil || user.ID != owner || len(user.Credentials) != 8 {
		t.Fatalf("discovery=%+v %v", user, err)
	}
	if store.gets != 0 || len(store.batches) != 2 || len(store.batches[0]) != 4 || len(store.batches[1]) != 8 {
		t.Fatalf("discovery gets=%d batches=%v", store.gets, store.batches)
	}
}

func TestIdentityBatchFailuresNeverFallBackToUnknownAuthority(t *testing.T) {
	for _, failure := range []string{"unavailable", "missing", "shape", "corrupt", "corrupt-account", "corrupt-profile", "corrupt-index", "wrong-owner", "wrong-account", "wrong-profile", "wrong-index", "disabled", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			repository, store, owner, ids := batchIdentityFixture(t, 2)
			store.read = func(ctx context.Context, keys []state.Key) ([]state.Value, error) {
				switch failure {
				case "unavailable":
					return nil, domain.ErrUnavailable
				case "missing":
					return nil, domain.ErrNotFound
				case "canceled":
					return nil, context.Canceled
				}
				values := make([]state.Value, len(keys))
				for i, key := range keys {
					value, err := store.TransactionalStore.Get(ctx, key)
					if err != nil {
						return nil, err
					}
					values[i] = value
				}
				switch failure {
				case "shape":
					return values[:1], nil
				case "corrupt":
					values[0].Data = []byte(`{}`)
				case "corrupt-account":
					values[1].Data = []byte(`{}`)
				case "corrupt-profile":
					values[2].Data = []byte(`{}`)
				case "corrupt-index":
					values[3].Data = []byte(`{}`)
				case "wrong-owner":
					var credential model.Credential
					if err := state.DecodeJSON(values[0].Data, &credential); err != nil {
						t.Fatal(err)
					}
					credential.UserID = userID(t, 0x52)
					values[0].Data, _ = state.EncodeJSON(&credential)
				case "wrong-account":
					var account model.Account
					if err := state.DecodeJSON(values[1].Data, &account); err != nil {
						t.Fatal(err)
					}
					account.UserID = userID(t, 0x52)
					values[1].Data, _ = state.EncodeJSON(&account)
				case "wrong-profile":
					var profile model.Profile
					if err := state.DecodeJSON(values[2].Data, &profile); err != nil {
						t.Fatal(err)
					}
					profile.UserID = userID(t, 0x52)
					values[2].Data, _ = state.EncodeJSON(&profile)
				case "wrong-index":
					var index model.CredentialIndex
					if err := state.DecodeJSON(values[3].Data, &index); err != nil {
						t.Fatal(err)
					}
					index.UserID = userID(t, 0x52)
					values[3].Data, _ = state.EncodeJSON(&index)
				case "disabled":
					var account model.Account
					if err := state.DecodeJSON(values[1].Data, &account); err != nil {
						t.Fatal(err)
					}
					account.Status = model.AccountDisabled
					values[1].Data, _ = state.EncodeJSON(&account)
				}
				return values, nil
			}
			rawID, _ := base64.RawURLEncoding.DecodeString(ids[0])
			handle, _ := base64.RawURLEncoding.DecodeString(owner.String())
			_, err := (&Service{repository: repository}).resolveDiscoverable(t.Context())(rawID, handle)
			if !errors.Is(err, domain.ErrUnauthenticated) || store.gets != 0 || len(store.batches) != 1 {
				t.Fatalf("failure=%s error=%v gets=%d batches=%d", failure, err, store.gets, len(store.batches))
			}
		})
	}
}

func TestCredentialBatchErrorsAndCancellationReturnNoPartialList(t *testing.T) {
	for _, failure := range []string{"unavailable", "shape", "corrupt", "owner", "canceled-after-batch", "canceled-between-batches"} {
		t.Run(failure, func(t *testing.T) {
			count := 2
			if failure == "canceled-between-batches" {
				count = 1001
			}
			repository, store, owner, _ := batchIdentityFixture(t, count)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store.read = func(ctx context.Context, keys []state.Key) ([]state.Value, error) {
				if failure == "unavailable" {
					return nil, domain.ErrUnavailable
				}
				values := make([]state.Value, len(keys))
				for i, key := range keys {
					value, err := store.TransactionalStore.Get(ctx, key)
					if err != nil {
						return nil, err
					}
					values[i] = value
				}
				switch failure {
				case "shape":
					return values[:1], nil
				case "corrupt":
					values[0].Data = []byte(`{}`)
				case "owner":
					var credential model.Credential
					if err := state.DecodeJSON(values[0].Data, &credential); err != nil {
						t.Fatal(err)
					}
					credential.UserID = userID(t, 0x52)
					values[0].Data, _ = state.EncodeJSON(&credential)
				case "canceled-after-batch", "canceled-between-batches":
					cancel()
				}
				return values, nil
			}
			credentials, err := repository.Credentials(ctx, owner)
			if err == nil || credentials != nil || len(store.batches) != 1 || store.gets != 1 {
				t.Fatalf("partial credentials=%d error=%v batches=%d gets=%d", len(credentials), err, len(store.batches), store.gets)
			}
		})
	}
}

func TestAuthenticationCommitRetainsFreshBatchVersionGuards(t *testing.T) {
	for _, member := range []string{"account", "credential"} {
		t.Run(member, func(t *testing.T) {
			env := newIdentityEnvironment(t, RegistrationPolicy{AllowPublic: true})
			owner, _ := bootstrapIdentity(t, env)
			credential := firstCredential(t, env, owner)
			start, err := env.service.StartAuthentication(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			base := env.repository.store.(state.TransactionalStore)
			store := &identityBatchStore{TransactionalStore: base}
			store.afterBatch = func(keys []state.Key) {
				if len(keys) != 2 {
					return
				}
				store.afterBatch = nil
				if member == "account" {
					account, version, err := getRecord[model.Account](t.Context(), base, keys[0])
					if err != nil {
						t.Fatal(err)
					}
					account.Status = model.AccountDisabled
					if _, err := swapRecord(t.Context(), base, keys[0], version, &account); err != nil {
						t.Fatal(err)
					}
				} else {
					credential, version, err := getRecord[model.Credential](t.Context(), base, keys[1])
					if err != nil {
						t.Fatal(err)
					}
					credential.SignCount++
					if _, err := swapRecord(t.Context(), base, keys[1], version, &credential); err != nil {
						t.Fatal(err)
					}
				}
			}
			env.repository.store = store
			response, _ := json.Marshal(fakeResponse{UserID: owner.String(), CredentialID: credential.CredentialID})
			if _, err := env.service.VerifyAuthentication(t.Context(), start.CeremonyID, start.BrowserBinding, response); !errors.Is(err, domain.ErrPreconditionFailed) {
				t.Fatalf("stale authorization committed=%v", err)
			}
			ceremony, _, err := env.repository.Ceremony(t.Context(), start.CeremonyID)
			if err != nil || ceremony.ConsumedAt != nil {
				t.Fatalf("failed commit consumed ceremony=%v", err)
			}
		})
	}
}

func TestAccountCredentialBatchReadFailsClosed(t *testing.T) {
	for _, failure := range []string{"unavailable", "missing", "shape", "account", "credential", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			repository, store, owner, ids := batchIdentityFixture(t, 1)
			store.read = func(ctx context.Context, keys []state.Key) ([]state.Value, error) {
				if failure == "unavailable" {
					return nil, domain.ErrUnavailable
				}
				if failure == "missing" {
					return nil, domain.ErrNotFound
				}
				values := make([]state.Value, len(keys))
				for i, key := range keys {
					value, err := store.TransactionalStore.Get(ctx, key)
					if err != nil {
						return nil, err
					}
					values[i] = value
				}
				switch failure {
				case "shape":
					return nil, nil
				case "account":
					values[0].Data = []byte(`{}`)
				case "credential":
					values[1].Data = []byte(`{}`)
				}
				return values, nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "canceled" {
				cancel()
			}
			_, _, _, _, err := repository.accountAndCredential(ctx, owner, ids[0])
			if err == nil || store.gets != 0 {
				t.Fatalf("unknown commit authority: %v, gets=%d", err, store.gets)
			}
		})
	}
}
