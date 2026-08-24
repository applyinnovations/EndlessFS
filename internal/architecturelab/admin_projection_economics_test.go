package architecturelab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
)

type unusedWebAuthn struct{}

func (unusedWebAuthn) BeginRegistration(auth.User, []byte) (json.RawMessage, json.RawMessage, error) {
	return nil, nil, errors.New("unused")
}
func (unusedWebAuthn) FinishRegistration(auth.User, json.RawMessage, []byte) (auth.RegistrationResult, error) {
	return auth.RegistrationResult{}, errors.New("unused")
}
func (unusedWebAuthn) BeginAuthentication([]byte) (json.RawMessage, json.RawMessage, error) {
	return nil, nil, errors.New("unused")
}
func (unusedWebAuthn) FinishAuthentication(json.RawMessage, []byte, auth.UserResolver) (auth.AuthenticationResult, error) {
	return auth.AuthenticationResult{}, errors.New("unused")
}

func TestAdminUserProjectionEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	modelEconomics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 128} {
		t.Run(fmt.Sprintf("users-%d", count), func(t *testing.T) {
			current := openCurrentProviderHarness(t, fmt.Sprintf("admin-projection-%d", count))
			repository := identity.NewRepository(current.engine)
			clock := domain.NewFixedClock(time.Date(2049, 9, 10, 11, 12, 13, 0, time.UTC))
			ids := domain.NewIDGenerator(&currentBatchEntropy{})
			protection := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x52}, 32)))
			sessions, err := auth.NewSessionManager(repository, ids, clock, time.Hour, "https://endlessfs.test", true, protection)
			if err != nil {
				t.Fatal(err)
			}
			service, err := identity.NewService(repository, unusedWebAuthn{}, sessions, ids, clock, identity.NewMutablePolicy(identity.RegistrationPolicy{}), secret.Value(""), "https://endlessfs.test")
			if err != nil {
				t.Fatal(err)
			}
			users := make([]domain.UserID, count)
			for index := range users {
				raw := []byte(fmt.Sprintf("user-%011d", index))
				user, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(raw))
				if err != nil {
					t.Fatal(err)
				}
				users[index] = user
				displayName, err := domain.ParseDisplayName(fmt.Sprintf("User %d", index))
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.CreateProfile(ctx, model.Profile{UserID: user, DisplayName: displayName}); err != nil {
					t.Fatal(err)
				}
				if err := repository.CreateAccount(ctx, model.Account{SchemaVersion: model.SchemaVersion, UserID: user, Status: model.AccountEnabled, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.CreateAdminRoles(ctx, model.AdminRoles{SchemaVersion: model.SchemaVersion, UserIDs: []domain.UserID{users[0]}}); err != nil {
				t.Fatal(err)
			}
			current.ledger.Reset()
			actor := auth.AuthenticatedSession{Record: model.Session{UserID: users[0]}}
			page, err := service.AdminUsersPage(ctx, actor, 1000, "")
			if err != nil || len(page.Users) != count {
				t.Fatalf("AdminUsersPage()=%+v, %v", page, err)
			}
			logCurrentEconomics(t, fmt.Sprintf("before/admin/list-users-%d", count), modelEconomics, current.ledger)

			ledger := providerbudget.NewLedger()
			backend := budgettest.Wrap(providerbudget.RoleState, objectmemory.New(), ledger)
			owner, err := openRecordDomain(ctx, backend, fmt.Sprintf("admin-owner-%d", count))
			if err != nil {
				t.Fatal(err)
			}
			admin, err := openRecordDomain(ctx, backend, fmt.Sprintf("admin-role-%d", count))
			if err != nil {
				t.Fatal(err)
			}
			projectionUsers := make([]map[string]any, 0, len(users))
			for index, user := range users {
				projectionUsers = append(projectionUsers, map[string]any{
					"userID": user.String(), "displayName": fmt.Sprintf("User %d", index),
					"status": "enabled", "admin": index == 0,
				})
			}
			rows, _ := json.Marshal(map[string]any{"users": projectionUsers})
			view, err := openDerivedView(ctx, backend, fmt.Sprintf("admin-users-%d", count), 1, rows)
			if err != nil {
				t.Fatal(err)
			}
			ledger.Reset()
			if _, _, err := owner.Get(ctx, "enabled"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := admin.Get(ctx, "role"); err != nil {
				t.Fatal(err)
			}
			if _, err := view.Read(ctx, 1); err != nil {
				t.Fatal(err)
			}
			logCurrentEconomics(t, fmt.Sprintf("after/admin/list-users-%d", count), modelEconomics, ledger)
		})
	}
}
