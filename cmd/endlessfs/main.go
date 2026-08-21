// Command endlessfs starts the EndlessFS control plane.
package main

import (
	"context"
	"crypto/hmac"
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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/applyinnovations/endlessfs/internal/auth"
	"github.com/applyinnovations/endlessfs/internal/config"
	"github.com/applyinnovations/endlessfs/internal/devfixture"
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
	previewdurable "github.com/applyinnovations/endlessfs/internal/preview/durable"
	"github.com/applyinnovations/endlessfs/internal/preview/imagegen"
	previewmemory "github.com/applyinnovations/endlessfs/internal/preview/memory"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
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
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(shutdownCtx, logger, cfg); err != nil {
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
	previewEnabled := cfg.PreviewProvider != "" && cfg.PreviewProvider != "disabled"
	writerConfiguration, err := buildWriterConfiguration(cfg, keyringID)
	if err != nil {
		return err
	}
	writeTimeout := controlWriteTimeout(previewEnabled, cfg.PreviewOperationTimeout)
	server, controlListener, startupHandler, controlErrors, err := startControlServer(cfg.ListenAddr, writeTimeout, logger)
	if err != nil {
		return err
	}
	defer server.Close()
	defer controlListener.Close()

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
		Writer:   writerConfiguration,
		LeaseTTL: 2 * time.Minute, UploadTTL: cfg.UploadInitTTL, DownloadTTL: cfg.DownloadCapabilityTTL,
		CursorKey: deriveKey("endlessfs-state-cursor-key-v1", secretBytes),
		MigrationObserver: func(progress portable.MigrationProgress) {
			logger.Info("storage_migration_progress",
				"migrationID", progress.MigrationID, "stage", progress.Stage, "role", progress.Role,
				"completedObjects", progress.CompletedObjects, "totalObjects", progress.TotalObjects,
				"completedBytes", progress.CompletedBytes, "totalBytes", progress.TotalBytes,
				"resumedObjects", progress.ResumedObjects,
			)
		},
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
	var previewStore preview.Store
	var previewDataHandler http.Handler
	closePreviewBackend := func() {}
	if cfg.PreviewProvider != "" && cfg.PreviewProvider != "disabled" {
		switch cfg.PreviewProvider {
		case "mock":
			previewKey := cfg.PreviewKeySecret
			if previewKey.Reveal() == "" {
				value, keyErr := ids.BearerToken()
				if keyErr != nil {
					return keyErr
				}
				previewKey = secret.Value(value)
			}
			memoryStore, storeErr := previewmemory.New(previewmemory.Options{
				Clock: clock, IDs: ids, Key: previewKey, CapabilityTTL: cfg.DownloadCapabilityTTL, AllowedOrigin: cfg.AllowedOrigin,
			})
			if storeErr != nil {
				return domain.NewError(domain.ErrorInvalid, "invalid configured preview store")
			}
			previewStore = memoryStore
			previewDataHandler = memoryStore
		case "gcs":
			previewBackend, openErr := gcstore.Open(ctx, cfg.GCSPreviewBucket)
			if openErr != nil {
				return openErr
			}
			previewKeyBytes, decodeErr := base64.RawURLEncoding.DecodeString(cfg.PreviewKeySecret.Reveal())
			if decodeErr != nil || len(previewKeyBytes) < 32 {
				_ = previewBackend.Close()
				return domain.NewError(domain.ErrorInvalid, "invalid preview key material")
			}
			if enableErr := previewBackend.EnableWorkloadIdentityTransfers(deriveKey("endlessfs-preview-transfer-lease-v1", previewKeyBytes), cfg.GCSSigningAccount); enableErr != nil {
				_ = previewBackend.Close()
				return enableErr
			}
			previewStore, err = previewdurable.New(previewdurable.Options{
				Backend: previewBackend, Transfers: previewBackend, Clock: clock, IDs: ids,
				Key: cfg.PreviewKeySecret, CapabilityTTL: cfg.DownloadCapabilityTTL,
				DataOrigin: dataOrigin, AllowedOrigin: cfg.AllowedOrigin, HTTPClient: http.DefaultClient,
			})
			if err != nil {
				_ = previewBackend.Close()
				return err
			}
			closePreviewBackend = func() { _ = previewBackend.Close() }
		default:
			return domain.NewError(domain.ErrorInvalid, "unsupported preview provider configuration")
		}
	}
	defer closePreviewBackend()
	var previewListener net.Listener
	if previewDataHandler != nil {
		previewListener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer previewListener.Close()
		previewOrigin := "http://" + previewListener.Addr().String()
		memoryStore, ok := previewStore.(*previewmemory.Store)
		if !ok {
			return domain.NewError(domain.ErrorInternal, "invalid mock preview store construction")
		}
		if err := memoryStore.SetDataPlaneBaseURL(previewOrigin); err != nil {
			return err
		}
	}
	driveService, err := drive.NewService(storage, store, repository, ids, clock, cfg.SessionSecret, cfg.BaseURL, dataOrigin, cfg.TextPreviewMaxBytes)
	if err != nil {
		return err
	}
	var fixtureSession auth.IssuedSession
	if cfg.LocalFixture {
		fixture, seedErr := devfixture.Seed(ctx, repository, driveService, dataHandler, clock)
		if seedErr != nil {
			return seedErr
		}
		fixtureSession, err = sessions.Issue(ctx, fixture.UserID, fixture.CredentialID)
		if err != nil {
			return domain.NewError(domain.ErrorInternal, "could not issue local fixture session")
		}
	}
	var previewService *preview.Service
	if previewEnabled {
		rawDecoderPath, decoderErr := imagegen.PackagedRawDecoderPath()
		if decoderErr != nil {
			return domain.NewError(domain.ErrorUnavailable, "preview RAW decoder is unavailable")
		}
		imageGenerator, workerErr := imagegen.NewWorker(imagegen.Options{RawDecoderPath: rawDecoderPath})
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
	if previewDataHandler != nil {
		previewDataServer = &http.Server{Handler: previewDataHandler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	}

	applicationHandler := httpapi.NewCompleteApplicationWithLogger(cfg, version, identityService, sessions, driveService, logger, themeManager)
	if previewService != nil {
		applicationHandler = httpapi.NewCompleteApplicationWithPreviewAndLogger(cfg, version, identityService, sessions, driveService, previewService, logger, themeManager)
	}
	if cfg.LocalFixture {
		applicationHandler = devfixture.LoginHandler(applicationHandler, sessions, fixtureSession)
	}
	startupHandler.Activate(applicationHandler)
	logger.Info("server_ready", "listenAddress", controlListener.Addr().String(), "version", version)
	if cfg.LocalFixture {
		logger.Info("local_fixture_ready", "url", cfg.BaseURL+devfixture.LoginPath)
	}

	errCh := make(chan error, 2)
	if dataServer != nil {
		go func() { errCh <- dataServer.Serve(dataListener) }()
	}
	if previewDataServer != nil {
		go func() { errCh <- previewDataServer.Serve(previewListener) }()
	}
	select {
	case err := <-controlErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
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

func controlWriteTimeout(previewEnabled bool, operationTimeout time.Duration) time.Duration {
	baseline := 30 * time.Second
	if previewEnabled && operationTimeout+5*time.Second > baseline {
		return operationTimeout + 5*time.Second
	}
	return baseline
}

func deriveKey(label string, material []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(label))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(material)
	return hash.Sum(nil)
}

func buildWriterConfiguration(cfg config.Config, sessionKeyringID string) (portable.WriterConfiguration, error) {
	if sessionKeyringID == "" {
		return portable.WriterConfiguration{}, domain.NewError(domain.ErrorInvalid, "invalid storage key identifier")
	}
	keyringIdentifiers := []string{sessionKeyringID}
	requiredFeatures := []string{"directory-manifests", "fenced-operations", "portable-checkpoints", storageformat.FeatureRecursiveBytes}
	previewProfile := "disabled"
	if cfg.PreviewProvider != "" && cfg.PreviewProvider != "disabled" {
		if err := validatePreviewCapabilities(cfg.PreviewFormats); err != nil {
			return portable.WriterConfiguration{}, err
		}
		requiredFeatures = append(requiredFeatures, "generated-previews-v1", "preview-integrity-crc32c-v1")
		storeIdentity := "process-local-mock-v1"
		if cfg.PreviewProvider == "gcs" {
			key, err := base64.RawURLEncoding.DecodeString(cfg.PreviewKeySecret.Reveal())
			if err != nil || len(key) < 32 {
				return portable.WriterConfiguration{}, domain.NewError(domain.ErrorInvalid, "invalid preview key material")
			}
			previewKeyringID := base64.RawURLEncoding.EncodeToString(deriveKey("endlessfs-preview-keyring-id-v1", key))
			keyringIdentifiers = append(keyringIdentifiers, previewKeyringID)
			mac := hmac.New(sha256.New, key)
			_, _ = mac.Write([]byte("endlessfs-preview-store-v1\x00" + cfg.PreviewProvider + "\x00" + cfg.GCSPreviewBucket))
			storeIdentity = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		} else if cfg.PreviewProvider != "mock" {
			return portable.WriterConfiguration{}, domain.NewError(domain.ErrorInvalid, "unsupported preview provider configuration")
		}
		formats := append([]string(nil), cfg.PreviewFormats...)
		slices.Sort(formats)
		resolutions := append([]int(nil), cfg.PreviewResolutions...)
		slices.Sort(resolutions)
		resolutionValues := make([]string, len(resolutions))
		for index, resolution := range resolutions {
			resolutionValues[index] = strconv.Itoa(resolution)
		}
		maxAge := "unset"
		if cfg.PreviewAutoMaxAge != nil {
			maxAge = cfg.PreviewAutoMaxAge.String()
		}
		maxSourceBytes := "unset"
		if cfg.PreviewAutoMaxSourceBytes != nil {
			maxSourceBytes = strconv.FormatInt(*cfg.PreviewAutoMaxSourceBytes, 10)
		}
		previewProfile = strings.Join([]string{
			cfg.PreviewProvider, storeIdentity, strconv.FormatBool(cfg.PreviewAutomatic),
			strings.Join(formats, ","), strings.Join(resolutionValues, ","), maxAge, maxSourceBytes,
			strconv.Itoa(cfg.PreviewMaxConcurrency), cfg.PreviewOperationTimeout.String(), cfg.DownloadCapabilityTTL.String(),
		}, "\x00")
	}
	configuration := fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%t\x00%s\x00%s", cfg.AllowedOrigin, cfg.WebAuthnRPID, cfg.WebAuthnRPName, cfg.AllowRegistration, cfg.InviteRegistration, sessionKeyringID, previewProfile)
	return portable.WriterConfiguration{
		WriterSetID:         cfg.WriterSetID,
		ConfigurationDigest: base64.RawURLEncoding.EncodeToString(deriveKey("endlessfs-writer-configuration-v1", []byte(configuration))),
		KeyringIdentifiers:  keyringIdentifiers, RequiredFeatures: requiredFeatures,
	}, nil
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
