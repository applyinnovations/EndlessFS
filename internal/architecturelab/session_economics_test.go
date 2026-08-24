package architecturelab

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	"github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestSessionProviderEconomicsBeforeAndAfter(t *testing.T) {
	ctx := context.Background()
	clock := domain.NewFixedClock(time.Date(2049, 3, 4, 5, 6, 7, 0, time.UTC))
	modelEconomics, err := gcs.RegionalStandardFlatEconomics()
	if err != nil {
		t.Fatal(err)
	}

	currentLedger := providerbudget.NewLedger()
	currentBackend := budgettest.WrapClassified(providerbudget.RoleState, objectmemory.New(), currentLedger, func(_ providerbudget.RequestKind, target string) string {
		return storageformat.ClassifyEconomicsTarget(target)
	})
	ids := domain.NewIDGenerator(&currentBatchEntropy{})
	engine, err := portable.Open(ctx, portable.Options{
		Backend: currentBackend, FileBackend: objectmemory.New(), Clock: clock, IDs: ids,
		Writer:   portable.WriterConfiguration{WriterSetID: "session-baseline", ConfigurationDigest: "session-baseline-v1", KeyringIdentifiers: []string{"session-key"}},
		LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x71}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := identity.NewRepository(engine)
	user, _ := domain.ParseUserID("c2Vzc2lvbi11c2VyLTAwMDAwMA")
	if err := repository.CreateAccount(ctx, model.Account{SchemaVersion: model.SchemaVersion, UserID: user, Status: model.AccountEnabled, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	protection := secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x72}, 32)))
	sessions, err := auth.NewSessionManager(repository, ids, clock, time.Hour, "https://endlessfs.test", true, protection)
	if err != nil {
		t.Fatal(err)
	}
	measureCurrent := func(name string, run func() error) {
		t.Helper()
		currentLedger.Reset()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		logCurrentEconomics(t, "before/session/"+name, modelEconomics, currentLedger)
	}
	var issued auth.IssuedSession
	measureCurrent("issue", func() error {
		var err error
		issued, err = sessions.Issue(ctx, user, "credential")
		return err
	})
	var authenticated auth.AuthenticatedSession
	measureCurrent("authenticate", func() error {
		var err error
		authenticated, err = sessions.Authenticate(ctx, issued.Token.Reveal())
		return err
	})
	var rotated auth.IssuedSession
	measureCurrent("rotate", func() error {
		var err error
		rotated, err = sessions.Rotate(ctx, authenticated, "credential")
		return err
	})
	rotatedAuth, err := sessions.Authenticate(ctx, rotated.Token.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	measureCurrent("logout", func() error { return sessions.Logout(ctx, rotatedAuth) })
	if _, err := sessions.Issue(ctx, user, "credential"); err != nil {
		t.Fatal(err)
	}
	measureCurrent("revoke-user-one-session", func() error { return sessions.RevokeUser(ctx, user) })

	prototypeLedger := providerbudget.NewLedger()
	prototypeBase := objectmemory.New()
	prototype := budgettest.Wrap(providerbudget.RoleState, prototypeBase, prototypeLedger)
	sessionKey := objectstore.MustKey("endlessfs/research/session/session.json")
	authHeadKey := objectstore.MustKey("endlessfs/research/session/owner-auth.json")
	if _, err := prototype.Put(ctx, authHeadKey, []byte(`{"enabled":true,"generation":1}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		t.Fatal(err)
	}
	measurePrototype := func(name string, run func() error) {
		t.Helper()
		prototypeLedger.Reset()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		logCurrentEconomics(t, "after/session/"+name, modelEconomics, prototypeLedger)
	}
	var sessionVersion objectstore.NativeVersion
	measurePrototype("issue", func() error {
		var err error
		sessionVersion, err = prototype.Put(ctx, sessionKey, []byte(`{"owner":"user","generation":1}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		return err
	})
	measurePrototype("authenticate", func() error {
		parallel := providerbudget.WithTrace(ctx, providerbudget.Trace{Operation: "session-authenticate", Subsystem: "session-validation", ParallelGroup: "session-and-owner"})
		if _, err := prototype.Get(parallel, sessionKey); err != nil {
			return err
		}
		_, err := prototype.Get(parallel, authHeadKey)
		return err
	})
	nextSessionKey := objectstore.MustKey("endlessfs/research/session/session-next.json")
	var nextVersion objectstore.NativeVersion
	measurePrototype("rotate", func() error {
		var err error
		nextVersion, err = prototype.Put(ctx, nextSessionKey, []byte(`{"owner":"user","generation":1}`), objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
		if err != nil {
			return err
		}
		return prototype.Delete(ctx, sessionKey, objectstore.DeleteCondition{Version: sessionVersion})
	})
	measurePrototype("logout", func() error {
		return prototype.Delete(ctx, nextSessionKey, objectstore.DeleteCondition{Version: nextVersion})
	})
	control, err := openRecordDomain(ctx, prototype, "session-revocation")
	if err != nil {
		t.Fatal(err)
	}
	measurePrototype("revoke-user-one-session", func() error {
		_, err := control.Mutate(ctx, RecordMutation{ID: "revoke-user", Key: "auth-generation", Value: []byte(`{"generation":2,"enabled":true}`)})
		return err
	})
}
