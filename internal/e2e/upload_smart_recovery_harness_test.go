package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/httpapi"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/objectstore/budgettest"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/theme"
)

// Each application has its own engine, identity repository, session manager,
// drive service and projection cache. Only the provider and signing key are
// shared, matching an ordinary deployment behind one origin. Round-robin
// dispatch deliberately lets admission and completion land on different engines.
type smartRecoveryHarness struct {
	harness
	engines        [2]*portable.Engine
	stateBase      *objectmemory.Backend
	fileBase       *objectmemory.Backend
	stateLedger    *providerbudget.Ledger
	fileLedger     *providerbudget.Ledger
	replicas       [2]atomic.Int64
	uploadReplicas [2]atomic.Int64
}

func newSmartRecoveryHarness(t *testing.T, controlWrap, dataWrap func(http.Handler) http.Handler) *smartRecoveryHarness {
	t.Helper()
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dataListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = controlListener.Close()
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(controlListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	origin, dataOrigin := "http://localhost:"+port, "http://"+dataListener.Addr().String()
	clock, ids := domain.SystemClock{}, domain.SystemIDGenerator()
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	sessionKey := secret.Value(base64.RawURLEncoding.EncodeToString(key))
	bootstrapToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	h := &smartRecoveryHarness{
		harness:   harness{origin: origin, dataOrigin: dataOrigin, previewOrigin: dataOrigin, bootstrapToken: bootstrapToken, clock: clock},
		stateBase: objectmemory.New(), fileBase: objectmemory.New(), stateLedger: providerbudget.NewLedger(), fileLedger: providerbudget.NewLedger(),
	}
	if err := h.fileBase.ConfigureDataPlane(dataOrigin, clock, ids); err != nil {
		t.Fatal(err)
	}
	stateBackend := budgettest.Wrap(providerbudget.RoleState, h.stateBase, h.stateLedger)
	fileBackend := budgettest.Wrap(providerbudget.RoleFile, h.fileBase, h.fileLedger)
	var handlers [2]http.Handler
	for index := range handlers {
		engine, err := portable.Open(context.Background(), portable.Options{
			Backend: stateBackend, FileBackend: fileBackend, Clock: clock, IDs: ids, CursorKey: key, LeaseTTL: time.Minute,
			Writer: portable.WriterConfiguration{WriterSetID: "browser-recovery", ConfigurationDigest: "browser-recovery-v1", KeyringIdentifiers: []string{"session-test"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.engines[index] = engine
		repository := identity.NewRepository(engine)
		webAuthn, err := auth.NewGoWebAuthn("localhost", "EndlessFS browser recovery test", origin)
		if err != nil {
			t.Fatal(err)
		}
		sessions, err := auth.NewSessionManager(repository, ids, clock, 12*time.Hour, origin, false, sessionKey)
		if err != nil {
			t.Fatal(err)
		}
		identityService, err := identity.NewService(repository, webAuthn, sessions, ids, clock, identity.NewMutablePolicy(identity.RegistrationPolicy{AllowPublic: true, AllowInvite: true}), secret.Value(bootstrapToken), origin)
		if err != nil {
			t.Fatal(err)
		}
		driveService, err := drive.NewService(engine.Files(), engine, repository, ids, clock, sessionKey, origin, dataOrigin, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		registry, err := theme.NewRegistry()
		if err != nil {
			t.Fatal(err)
		}
		manager, err := theme.NewManager(registry, engine, "endlessfs-light", "endlessfs-dark", false, clock)
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Config{BaseURL: origin, AllowedOrigin: origin, Secure: false, AllowRegistration: true, InviteRegistration: true, PreviewProvider: "disabled"}
		handlers[index] = httpapi.NewCompleteApplication(cfg, "e2e-recovery", identityService, sessions, driveService, manager)
		if index == 0 {
			h.repository, h.files = repository, engine.Files()
		}
	}
	var dispatch atomic.Uint64
	var controlHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(dispatch.Add(1)-1) % len(handlers)
		h.replicas[index].Add(1)
		if strings.HasPrefix(r.URL.Path, "/api/v1/uploads/") {
			h.uploadReplicas[index].Add(1)
		}
		handlers[index].ServeHTTP(w, r)
	})
	if controlWrap != nil {
		controlHandler = controlWrap(controlHandler)
	}
	var dataHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == origin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "PUT, PATCH, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Upload-Offset")
			w.Header().Set("Access-Control-Expose-Headers", "Upload-Offset")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		h.fileBase.ServeHTTP(w, r)
	})
	if dataWrap != nil {
		dataHandler = dataWrap(dataHandler)
	}
	controlServer, dataServer := &http.Server{Handler: controlHandler}, &http.Server{Handler: dataHandler}
	serveErrors := make(chan error, 2)
	go func() { serveErrors <- controlServer.Serve(controlListener) }()
	go func() { serveErrors <- dataServer.Serve(dataListener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = controlServer.Shutdown(ctx)
		_ = dataServer.Shutdown(ctx)
		for range 2 {
			if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("recovery server: %v", err)
			}
		}
	})
	return h
}

func (h *smartRecoveryHarness) assertProviderMetadataOnly(t *testing.T) {
	t.Helper()
	for _, event := range h.fileLedger.Events() {
		if event.Kind == providerbudget.RequestObjectOpen || event.Kind == providerbudget.RequestObjectGet || event.Kind == providerbudget.RequestObjectCopy {
			t.Fatalf("upload control plane accessed stored file bodies: %s", event.Kind)
		}
	}
	for index := range h.replicas {
		if h.replicas[index].Load() == 0 {
			t.Fatalf("replica %d received no requests", index)
		}
		if h.uploadReplicas[index].Load() == 0 {
			t.Fatalf("replica %d received no actual upload lifecycle requests", index)
		}
	}
}
