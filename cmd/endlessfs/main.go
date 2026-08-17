// Command endlessfs starts the EndlessFS control plane.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	"github.com/applyinnovations/endlessfs/internal/httpapi"
	"github.com/applyinnovations/endlessfs/internal/identity"
	endlesslogging "github.com/applyinnovations/endlessfs/internal/logging"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	gcstore "github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/preview"
	"github.com/applyinnovations/endlessfs/internal/preview/imagegen"
	previewmemory "github.com/applyinnovations/endlessfs/internal/preview/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/theme"
)

var version = "dev"

func main() {
	if imagegen.IsWorkerInvocation() {
		if err := imagegen.RunWorker(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	cfg, err := config.Load()
	if err != nil {
		endlesslogging.NewJSON(os.Stdout, slog.LevelInfo).Error("process_stopped", "result", "error", "error", err.Error())
		os.Exit(1)
	}
	logger := endlesslogging.NewJSON(os.Stdout, cfg.LogLevel)
	if err := run(context.Background(), logger, cfg); err != nil {
		logger.Error("process_stopped", "result", "error", "error", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, cfg config.Config) error {
	if ctx.Err() != nil {
		return nil
	}
	ids := domain.SystemIDGenerator()
	clock := domain.SystemClock{}
	secretBytes, err := base64.RawURLEncoding.DecodeString(cfg.SessionSecret.Reveal())
	if err != nil || len(secretBytes) != 32 {
		return domain.NewError(domain.ErrorInvalid, "invalid storage key material")
	}
	leaseKey := deriveKey("endlessfs-transfer-lease-v1", secretBytes)
	keyringID := base64.RawURLEncoding.EncodeToString(deriveKey("endlessfs-keyring-id-v1", secretBytes))
	configuration := fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%t\x00%s", cfg.AllowedOrigin, cfg.WebAuthnRPID, cfg.WebAuthnRPName, cfg.AllowRegistration, cfg.InviteRegistration, keyringID)
	configurationDigest := base64.RawURLEncoding.EncodeToString(deriveKey("endlessfs-writer-configuration-v1", []byte(configuration)))

	var backend objectstore.Backend
	var fileBackend objectstore.Backend
	var dataHandler http.Handler
	var dataListener net.Listener
	var dataServer *http.Server
	dataOrigin := "https://storage.googleapis.com"
	closeBackend := func() {}
	switch cfg.StorageProvider {
	case "mock":
		dataListenAddress := "127.0.0.1:0"
		if cfg.MockProviderURL != "" {
			parsed, parseErr := url.Parse(cfg.MockProviderURL)
			if parseErr != nil {
				return parseErr
			}
			dataListenAddress = parsed.Host
		}
		dataListener, err = net.Listen("tcp", dataListenAddress)
		if err != nil {
			return err
		}
		defer dataListener.Close()
		dataOrigin = "http://" + dataListener.Addr().String()
		memoryBackend := objectmemory.New()
		if err := memoryBackend.ConfigureDataPlane(dataOrigin, clock, ids); err != nil {
			return err
		}
		backend = memoryBackend
		dataHandler = memoryBackend
	case "gcs":
		gcsBackend, openErr := gcstore.Open(ctx, cfg.GCSFileBucket)
		if openErr != nil {
			return openErr
		}
		if err := gcsBackend.EnableWorkloadIdentityTransfers(leaseKey, cfg.GCSSigningAccount); err != nil {
			_ = gcsBackend.Close()
			return err
		}
		stateBucket := cfg.GCSStateBucket
		if stateBucket == "" {
			stateBucket = cfg.GCSFileBucket
		}
		backend = gcsBackend
		closeBackend = func() { _ = gcsBackend.Close() }
		if stateBucket != cfg.GCSFileBucket {
			stateBackend, stateOpenErr := gcstore.Open(ctx, stateBucket)
			if stateOpenErr != nil {
				_ = gcsBackend.Close()
				return stateOpenErr
			}
			backend = stateBackend
			fileBackend = gcsBackend
			closeBackend = func() {
				_ = stateBackend.Close()
				_ = gcsBackend.Close()
			}
		}
	default:
		return domain.NewError(domain.ErrorInvalid, "unsupported storage provider")
	}
	defer closeBackend()
	engine, err := portable.Open(ctx, portable.Options{
		Backend: backend, FileBackend: fileBackend, Clock: clock, IDs: ids,
		Writer: portable.WriterConfiguration{
			WriterSetID: cfg.WriterSetID, ConfigurationDigest: configurationDigest,
			KeyringIdentifiers: []string{keyringID},
			RequiredFeatures:   []string{"directory-manifests", "fenced-operations", "portable-checkpoints"},
		},
		LeaseTTL: 2 * time.Minute, UploadTTL: cfg.UploadInitTTL, DownloadTTL: cfg.DownloadCapabilityTTL,
		CursorKey: deriveKey("endlessfs-state-cursor-key-v1", secretBytes),
	})
	if err != nil {
		return err
	}
	var store state.Store = engine
	storage := engine.Files()
	repository := identity.NewRepository(store)
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
	previewEnabled := cfg.PreviewProvider != "" && cfg.PreviewProvider != "disabled"
	if previewEnabled {
		if err := validatePreviewCapabilities(cfg.PreviewFormats); err != nil {
			return err
		}
	}
	var previewStore *previewmemory.Store
	if cfg.PreviewProvider != "" && cfg.PreviewProvider != "disabled" {
		if cfg.PreviewProvider != "mock" {
			return domain.NewError(domain.ErrorInvalid, "unsupported preview provider configuration")
		}
		previewKey := cfg.PreviewKeySecret
		if previewKey.Reveal() == "" {
			value, keyErr := ids.BearerToken()
			if keyErr != nil {
				return keyErr
			}
			previewKey = secret.Value(value)
		}
		previewStore, err = previewmemory.New(previewmemory.Options{
			Clock: clock, IDs: ids, Key: previewKey, CapabilityTTL: cfg.DownloadCapabilityTTL, AllowedOrigin: cfg.AllowedOrigin,
		})
		if err != nil {
			return domain.NewError(domain.ErrorInvalid, "invalid configured preview store")
		}
	}
	var previewListener net.Listener
	if previewStore != nil {
		previewListener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer previewListener.Close()
		previewOrigin := "http://" + previewListener.Addr().String()
		if err := previewStore.SetDataPlaneBaseURL(previewOrigin); err != nil {
			return err
		}
	}
	driveService, err := drive.NewService(storage, store, repository, ids, clock, cfg.SessionSecret, cfg.BaseURL, dataOrigin, cfg.TextPreviewMaxBytes)
	if err != nil {
		return err
	}
	var previewService *preview.Service
	if previewEnabled {
		imageGenerator, workerErr := imagegen.NewWorker(imagegen.Options{})
		if workerErr != nil {
			return domain.NewError(domain.ErrorUnavailable, "preview generator worker is unavailable")
		}
		previewService, err = preview.NewService(preview.Options{
			Automatic:        cfg.PreviewAutomatic,
			MaxAge:           cfg.PreviewAutoMaxAge,
			MaxSourceBytes:   cfg.PreviewAutoMaxSourceBytes,
			Resolutions:      cfg.PreviewResolutions,
			MaxConcurrency:   cfg.PreviewMaxConcurrency,
			OperationTimeout: cfg.PreviewOperationTimeout,
			StartupTimeout:   cfg.PreviewStartupTimeout,
			ApplicationState: store,
		}, storage, previewStore, []preview.Generator{imageGenerator}, http.DefaultClient, ids, clock)
		if err != nil {
			return err
		}
	}
	themeRegistry, err := theme.NewRegistry()
	if err != nil {
		return err
	}
	themeManager, err := theme.NewManager(themeRegistry, store, cfg.DefaultLightTheme, cfg.DefaultDarkTheme, cfg.Secure, clock)
	if err != nil {
		return err
	}
	if dataHandler != nil {
		dataServer = &http.Server{Handler: dataHandler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	}
	var previewDataServer *http.Server
	if previewStore != nil {
		previewDataServer = &http.Server{Handler: previewStore, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	}

	applicationHandler := httpapi.NewCompleteApplicationWithLogger(cfg, version, identityService, sessions, driveService, logger, themeManager)
	if previewService != nil {
		applicationHandler = httpapi.NewCompleteApplicationWithPreviewAndLogger(cfg, version, identityService, sessions, driveService, previewService, logger, themeManager)
	}
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           applicationHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	shutdownCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 3)
	if dataServer != nil {
		go func() { errCh <- dataServer.Serve(dataListener) }()
	}
	if previewDataServer != nil {
		go func() { errCh <- previewDataServer.Serve(previewListener) }()
	}
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
		if dataServer != nil {
			if err := dataServer.Shutdown(graceCtx); err != nil {
				return err
			}
		}
		if previewDataServer != nil {
			if err := previewDataServer.Shutdown(graceCtx); err != nil {
				return err
			}
		}
		logger.Info("server_stopped", "result", "graceful")
		return nil
	}
}

func deriveKey(label string, material []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(label))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(material)
	return hash.Sum(nil)
}

func validatePreviewCapabilities(formats []string) error {
	if len(formats) == 0 {
		formats = []string{"image"}
	}
	seen := make(map[string]bool)
	for _, format := range formats {
		if format != "image" || seen[format] {
			return domain.NewError(domain.ErrorInvalid, "configured preview generator is not packaged")
		}
		seen[format] = true
	}
	return nil
}
