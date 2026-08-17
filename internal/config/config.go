// Package config parses and validates EndlessFS process configuration.
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/secret"
)

const (
	defaultListenAddr           = "127.0.0.1:8080"
	defaultSessionTTL           = 12 * time.Hour
	maximumSessionTTL           = 7 * 24 * time.Hour
	defaultDownloadTTL          = time.Minute
	maximumDownloadTTL          = 10 * time.Minute
	defaultUploadTTL            = 5 * time.Minute
	maximumUploadTTL            = time.Hour
	defaultTextPreviewMax int64 = 1 << 20
)

// Config is the validated process configuration. Secret fields use a redacting
// value type and are never included in PublicConfig.
type Config struct {
	ListenAddr            string
	BaseURL               string
	AllowedOrigin         string
	Secure                bool
	StorageProvider       string
	MockProviderURL       string
	GCSBucket             string
	GCSSigningAccount     string
	WriterSetID           string
	AllowRegistration     bool
	InviteRegistration    bool
	BootstrapToken        secret.Value
	SessionSecret         secret.Value
	WebAuthnRPID          string
	WebAuthnRPName        string
	SessionTTL            time.Duration
	DownloadCapabilityTTL time.Duration
	UploadInitTTL         time.Duration
	TextPreviewMaxBytes   int64
	DefaultLightTheme     string
	DefaultDarkTheme      string
	LogLevel              slog.Level
}

// PublicConfig contains the non-secret settings safe to expose to browsers.
type PublicConfig struct {
	AllowRegistration            bool `json:"allowRegistration"`
	InviteRegistration           bool `json:"inviteRegistration"`
	PasskeysAvailable            bool `json:"passkeysAvailable"`
	MaximumUploadInitializations int  `json:"maximumUploadInitializations"`
	DefaultTransferConcurrency   int  `json:"defaultTransferConcurrency"`
	MaximumTransferConcurrency   int  `json:"maximumTransferConcurrency"`
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return Parse(os.LookupEnv)
}

// Parse reads configuration through lookup so tests never mutate global state.
func Parse(lookup func(string) (string, bool)) (Config, error) {
	listenAddr := defaultListenAddr
	if value, ok := lookup("ENDLESSFS_LISTEN_ADDR"); ok {
		listenAddr = strings.TrimSpace(value)
	}

	listenHost, listenPort, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return Config{}, fmt.Errorf("ENDLESSFS_LISTEN_ADDR: expected host:port: %w", err)
	}
	listenLoopback := isLoopbackHost(listenHost)

	baseURLValue, hasBaseURL := lookup("ENDLESSFS_BASE_URL")
	if !hasBaseURL {
		if !listenLoopback {
			return Config{}, fmt.Errorf("ENDLESSFS_BASE_URL: required for non-loopback listeners")
		}
		baseURLValue = "http://" + net.JoinHostPort(normalizeLoopbackHost(listenHost), listenPort)
	}
	baseURL, err := parseBaseURL(strings.TrimSpace(baseURLValue))
	if err != nil {
		return Config{}, fmt.Errorf("ENDLESSFS_BASE_URL: %w", err)
	}
	secure := baseURL.Scheme == "https"
	if !secure && (!listenLoopback || !isLoopbackHost(baseURL.Hostname())) {
		return Config{}, fmt.Errorf("ENDLESSFS_BASE_URL: HTTP is permitted only for loopback development")
	}

	allowRegistration, err := parseBool(lookup, "ALLOW_REGISTRATION", false)
	if err != nil {
		return Config{}, err
	}
	inviteRegistration, err := parseBool(lookup, "INVITE_REGISTRATION", true)
	if err != nil {
		return Config{}, err
	}

	storageProvider := "mock"
	if value, ok := lookup("ENDLESSFS_STORAGE_PROVIDER"); ok {
		storageProvider = strings.TrimSpace(value)
	}
	if storageProvider != "mock" && storageProvider != "gcs" {
		return Config{}, fmt.Errorf("ENDLESSFS_STORAGE_PROVIDER: expected exactly mock or gcs")
	}
	mockProviderURL := ""
	if value, ok := lookup("ENDLESSFS_MOCK_PROVIDER_URL"); ok {
		parsed, parseErr := parseMockProviderURL(strings.TrimSpace(value))
		if parseErr != nil {
			return Config{}, fmt.Errorf("ENDLESSFS_MOCK_PROVIDER_URL: %w", parseErr)
		}
		mockProviderURL = strings.TrimSuffix(parsed.String(), "/")
	}
	gcsBucket := ""
	if value, ok := lookup("ENDLESSFS_GCS_BUCKET"); ok {
		gcsBucket = strings.TrimSpace(value)
	}
	gcsSigningAccount := ""
	if value, ok := lookup("ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT"); ok {
		gcsSigningAccount = strings.TrimSpace(value)
		if !validGCSServiceAccount(gcsSigningAccount) {
			return Config{}, fmt.Errorf("ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT: expected a service-account email")
		}
	}
	writerSetID := "AAAAAAAAAAAAAAAAAAAAAA"
	if value, ok := lookup("ENDLESSFS_WRITER_SET_ID"); ok {
		writerSetID = strings.TrimSpace(value)
	}
	if storageProvider == "gcs" {
		if gcsBucket == "" {
			return Config{}, fmt.Errorf("ENDLESSFS_GCS_BUCKET: required for GCS")
		}
		if _, configured := lookup("ENDLESSFS_WRITER_SET_ID"); !configured {
			return Config{}, fmt.Errorf("ENDLESSFS_WRITER_SET_ID: required for GCS")
		}
		if mockProviderURL != "" {
			return Config{}, fmt.Errorf("ENDLESSFS_MOCK_PROVIDER_URL: unavailable with GCS")
		}
	} else if gcsBucket != "" || gcsSigningAccount != "" {
		return Config{}, fmt.Errorf("GCS configuration is unavailable with mock storage")
	}
	if !validRandomIdentifier(writerSetID) {
		return Config{}, fmt.Errorf("ENDLESSFS_WRITER_SET_ID: expected canonical base64url encoding of at least 16 bytes")
	}

	bootstrapToken, err := parseOptionalBearer(lookup, "ENDLESSFS_BOOTSTRAP_TOKEN")
	if err != nil {
		return Config{}, err
	}
	sessionSecret, err := parseRequiredBearer(lookup, "ENDLESSFS_SESSION_SECRET")
	if err != nil {
		return Config{}, err
	}

	rpID := baseURL.Hostname()
	if value, ok := lookup("ENDLESSFS_WEBAUTHN_RP_ID"); ok {
		rpID = strings.TrimSpace(value)
	}
	if rpID == "" || !strings.EqualFold(rpID, baseURL.Hostname()) {
		return Config{}, fmt.Errorf("ENDLESSFS_WEBAUTHN_RP_ID: must exactly match the base URL hostname")
	}

	rpName := "EndlessFS"
	if value, ok := lookup("ENDLESSFS_WEBAUTHN_RP_NAME"); ok {
		rpName = strings.TrimSpace(value)
	}
	if rpName == "" || len(rpName) > 100 || strings.ContainsAny(rpName, "\r\n\x00") {
		return Config{}, fmt.Errorf("ENDLESSFS_WEBAUTHN_RP_NAME: invalid relying-party display name")
	}

	sessionTTL, err := parseDuration(lookup, "ENDLESSFS_SESSION_TTL", defaultSessionTTL, maximumSessionTTL)
	if err != nil {
		return Config{}, err
	}
	downloadTTL, err := parseDuration(lookup, "ENDLESSFS_DOWNLOAD_CAPABILITY_TTL", defaultDownloadTTL, maximumDownloadTTL)
	if err != nil {
		return Config{}, err
	}
	uploadTTL, err := parseDuration(lookup, "ENDLESSFS_UPLOAD_INIT_TTL", defaultUploadTTL, maximumUploadTTL)
	if err != nil {
		return Config{}, err
	}
	textPreviewMax, err := parsePositiveInt64(lookup, "ENDLESSFS_TEXT_PREVIEW_MAX_BYTES", defaultTextPreviewMax, 16<<20)
	if err != nil {
		return Config{}, err
	}
	defaultLightTheme, err := parseThemeID(lookup, "ENDLESSFS_DEFAULT_LIGHT_THEME", "endlessfs-light")
	if err != nil {
		return Config{}, err
	}
	defaultDarkTheme, err := parseThemeID(lookup, "ENDLESSFS_DEFAULT_DARK_THEME", "endlessfs-dark")
	if err != nil {
		return Config{}, err
	}
	logLevel, err := parseLogLevel(lookup)
	if err != nil {
		return Config{}, err
	}

	return Config{
		ListenAddr:            listenAddr,
		BaseURL:               strings.TrimSuffix(baseURL.String(), "/"),
		AllowedOrigin:         strings.TrimSuffix(baseURL.String(), "/"),
		Secure:                secure,
		StorageProvider:       storageProvider,
		MockProviderURL:       mockProviderURL,
		GCSBucket:             gcsBucket,
		GCSSigningAccount:     gcsSigningAccount,
		WriterSetID:           writerSetID,
		AllowRegistration:     allowRegistration,
		InviteRegistration:    inviteRegistration,
		BootstrapToken:        bootstrapToken,
		SessionSecret:         sessionSecret,
		WebAuthnRPID:          strings.ToLower(rpID),
		WebAuthnRPName:        rpName,
		SessionTTL:            sessionTTL,
		DownloadCapabilityTTL: downloadTTL,
		UploadInitTTL:         uploadTTL,
		TextPreviewMaxBytes:   textPreviewMax,
		DefaultLightTheme:     defaultLightTheme,
		DefaultDarkTheme:      defaultDarkTheme,
		LogLevel:              logLevel,
	}, nil
}

func validRandomIdentifier(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) >= 16 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validGCSServiceAccount(value string) bool {
	if len(value) < len("a@b.iam.gserviceaccount.com") || len(value) > 254 || value != strings.ToLower(value) || !strings.HasSuffix(value, ".iam.gserviceaccount.com") || strings.Count(value, "@") != 1 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '.' || character == '@' {
			continue
		}
		return false
	}
	return true
}

// Public returns a copy containing no secrets.
func (c Config) Public() PublicConfig {
	return PublicConfig{
		AllowRegistration:            c.AllowRegistration,
		InviteRegistration:           c.InviteRegistration,
		PasskeysAvailable:            true,
		MaximumUploadInitializations: 100,
		DefaultTransferConcurrency:   4,
		MaximumTransferConcurrency:   8,
	}
}

func parseMockProviderURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Port() == "" || !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("expected an HTTP loopback origin with an explicit port")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("must be an origin without credentials, path, query, or fragment")
	}
	return parsed, nil
}

func parseBaseURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("expected an absolute HTTP(S) origin")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("must be an origin without credentials, path, query, or fragment")
	}
	if strings.Contains(parsed.Hostname(), "*") {
		return nil, fmt.Errorf("wildcard hosts are forbidden")
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func normalizeLoopbackHost(host string) string {
	if host == "" {
		return "127.0.0.1"
	}
	return host
}

func parseOptionalBearer(lookup func(string) (string, bool), name string) (secret.Value, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", nil
	}
	if !secret.ValidBearerToken(value) {
		return "", fmt.Errorf("%s: expected canonical base64url encoding of 32 random bytes", name)
	}
	return secret.Value(value), nil
}

func parseRequiredBearer(lookup func(string) (string, bool), name string) (secret.Value, error) {
	value, err := parseOptionalBearer(lookup, name)
	if err != nil {
		return "", err
	}
	if value.Reveal() == "" {
		return "", fmt.Errorf("%s: required", name)
	}
	return value, nil
}

func parseDuration(lookup func(string) (string, bool), name string, fallback, maximum time.Duration) (time.Duration, error) {
	value, ok := lookup(name)
	if !ok {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 || duration > maximum {
		return 0, fmt.Errorf("%s: expected duration greater than zero and at most %s", name, maximum)
	}
	return duration, nil
}

func parsePositiveInt64(lookup func(string) (string, bool), name string, fallback, maximum int64) (int64, error) {
	value, ok := lookup(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 || parsed > maximum {
		return 0, fmt.Errorf("%s: expected an integer from 1 through %d", name, maximum)
	}
	return parsed, nil
}

func parseThemeID(lookup func(string) (string, bool), name, fallback string) (string, error) {
	value, ok := lookup(name)
	if !ok {
		value = fallback
	}
	if len(value) < 1 || len(value) > 128 {
		return "", fmt.Errorf("%s: invalid theme ID", name)
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '.') {
			return "", fmt.Errorf("%s: invalid theme ID", name)
		}
	}
	return value, nil
}

func parseLogLevel(lookup func(string) (string, bool)) (slog.Level, error) {
	value, ok := lookup("ENDLESSFS_LOG_LEVEL")
	if !ok {
		return slog.LevelInfo, nil
	}
	switch value {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("ENDLESSFS_LOG_LEVEL: expected exactly debug, info, warn, or error")
	}
}

func parseBool(lookup func(string) (string, bool), name string, fallback bool) (bool, error) {
	value, ok := lookup(name)
	if !ok {
		return fallback, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s: expected exactly true or false", name)
	}
}
