// Command endlessfs starts the EndlessFS control plane.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/httpapi"
	"github.com/applyinnovations/endlessfs/internal/identity"
	"github.com/applyinnovations/endlessfs/internal/state"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(context.Background(), logger); err != nil {
		logger.Error("process_stopped", "result", "error", "error", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store := state.NewMemoryStore()
	repository := identity.NewRepository(store)
	ids := domain.SystemIDGenerator()
	clock := domain.SystemClock{}
	webAuthn, err := auth.NewGoWebAuthn(cfg.WebAuthnRPID, cfg.WebAuthnRPName, cfg.AllowedOrigin)
	if err != nil {
		return err
	}
	sessions, err := auth.NewSessionManager(repository, ids, clock, cfg.SessionTTL, cfg.AllowedOrigin, cfg.Secure, cfg.SessionSecret)
	if err != nil {
		return err
	}
	policy := identity.NewMutablePolicy(identity.RegistrationPolicy{
		AllowPublic: cfg.AllowRegistration, AllowInvite: cfg.InviteRegistration,
	})
	identityService, err := identity.NewService(repository, webAuthn, sessions, ids, clock, policy, cfg.BootstrapToken, cfg.BaseURL)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpapi.NewApplication(cfg, version, identityService, sessions),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	shutdownCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server_started", "listenAddress", cfg.ListenAddr, "version", version)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-shutdownCtx.Done():
		graceCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(graceCtx); err != nil {
			return err
		}
		logger.Info("server_stopped", "result", "graceful")
		return nil
	}
}
