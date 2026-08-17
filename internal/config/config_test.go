package config

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testSecret = base64.RawURLEncoding.EncodeToString(make([]byte, 32))

func TestPublicConfigHasNoOptionalMediaBrowserSwitch(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(mapLookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cfg.Public())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "mediaBrowserEnabled") {
		t.Fatalf("media browsing must be unconditional, public config = %s", encoded)
	}
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(mapLookup(nil))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
	if cfg.AllowRegistration {
		t.Fatal("AllowRegistration = true, want false")
	}
	if !cfg.InviteRegistration {
		t.Fatal("InviteRegistration = false, want true")
	}
	if cfg.BaseURL != "http://127.0.0.1:8080" || cfg.AllowedOrigin != cfg.BaseURL || cfg.Secure {
		t.Fatalf("loopback origin configuration = %+v", cfg)
	}
	if cfg.WebAuthnRPID != "127.0.0.1" || cfg.WebAuthnRPName != "EndlessFS" || cfg.SessionTTL != 12*time.Hour {
		t.Fatalf("WebAuthn/session defaults = %+v", cfg)
	}
	if cfg.DefaultLightTheme != "endlessfs-light" || cfg.DefaultDarkTheme != "endlessfs-dark" {
		t.Fatalf("theme defaults = %+v", cfg)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.PreviewProvider != "disabled" || cfg.PreviewAutomatic {
		t.Fatalf("preview defaults = %+v", cfg.Public())
	}
	if strings.Join(cfg.PreviewFormats, ",") != "image" || strings.Join(intStrings(cfg.PreviewResolutions), ",") != "256,512,1600" {
		t.Fatalf("preview capability defaults = formats %v resolutions %v", cfg.PreviewFormats, cfg.PreviewResolutions)
	}
}

func TestParsePreviewConfiguration(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"ENDLESSFS_PREVIEW_PROVIDER":              "mock",
		"ENDLESSFS_PREVIEW_AUTOMATIC":             "false",
		"ENDLESSFS_PREVIEW_FORMATS":               "image",
		"ENDLESSFS_PREVIEW_AUTO_MAX_AGE":          "72h",
		"ENDLESSFS_PREVIEW_AUTO_MAX_SOURCE_BYTES": "10485760",
		"ENDLESSFS_PREVIEW_RESOLUTIONS":           "128,512,2048",
		"ENDLESSFS_PREVIEW_MAX_CONCURRENCY":       "4",
		"ENDLESSFS_PREVIEW_OPERATION_TIMEOUT":     "90s",
		"ENDLESSFS_PREVIEW_STARTUP_TIMEOUT":       "30s",
	}
	cfg, err := Parse(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreviewProvider != "mock" || cfg.PreviewAutomatic {
		t.Fatalf("preview switches = %+v", cfg.Public())
	}
	if cfg.PreviewAutoMaxAge == nil || *cfg.PreviewAutoMaxAge != 72*time.Hour {
		t.Fatalf("PreviewAutoMaxAge = %v", cfg.PreviewAutoMaxAge)
	}
	if cfg.PreviewAutoMaxSourceBytes == nil || *cfg.PreviewAutoMaxSourceBytes != 10<<20 {
		t.Fatalf("PreviewAutoMaxSourceBytes = %v", cfg.PreviewAutoMaxSourceBytes)
	}
	if cfg.PreviewMaxConcurrency != 4 || cfg.PreviewOperationTimeout != 90*time.Second || cfg.PreviewStartupTimeout != 30*time.Second {
		t.Fatalf("preview execution limits = %+v", cfg)
	}
	public := cfg.Public()
	if !public.PreviewConfigured || public.PreviewAutomatic || strings.Join(public.PreviewFormats, ",") != "image" || public.PreviewMaxConcurrency != 4 {
		t.Fatalf("public preview configuration = %+v", public)
	}
}

func TestParseRejectsInvalidPreviewConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field string
		value string
		want  string
	}{
		{name: "removed browser switch", field: "ENDLESSFS_MEDIA_BROWSER_ENABLED", value: "false", want: "always available"},
		{name: "unknown provider", field: "ENDLESSFS_PREVIEW_PROVIDER", value: "s3", want: "disabled or mock"},
		{name: "unpackaged generator", field: "ENDLESSFS_PREVIEW_FORMATS", value: "video", want: "not packaged"},
		{name: "unknown generator", field: "ENDLESSFS_PREVIEW_FORMATS", value: "office", want: "unknown"},
		{name: "duplicate generator", field: "ENDLESSFS_PREVIEW_FORMATS", value: "image,image", want: "duplicate"},
		{name: "empty generator", field: "ENDLESSFS_PREVIEW_FORMATS", value: "", want: "at least one"},
		{name: "zero max age", field: "ENDLESSFS_PREVIEW_AUTO_MAX_AGE", value: "0s", want: "greater than zero"},
		{name: "zero max bytes", field: "ENDLESSFS_PREVIEW_AUTO_MAX_SOURCE_BYTES", value: "0", want: "from 1"},
		{name: "duplicate resolution", field: "ENDLESSFS_PREVIEW_RESOLUTIONS", value: "256,256", want: "strictly increasing"},
		{name: "descending resolution", field: "ENDLESSFS_PREVIEW_RESOLUTIONS", value: "512,256", want: "strictly increasing"},
		{name: "small resolution", field: "ENDLESSFS_PREVIEW_RESOLUTIONS", value: "63", want: "64 through 4096"},
		{name: "too many resolutions", field: "ENDLESSFS_PREVIEW_RESOLUTIONS", value: "64,128,256,512,1024", want: "at most 4"},
		{name: "excess concurrency", field: "ENDLESSFS_PREVIEW_MAX_CONCURRENCY", value: "9", want: "1 through 8"},
		{name: "long operation", field: "ENDLESSFS_PREVIEW_OPERATION_TIMEOUT", value: "6m", want: "at most 5m0s"},
		{name: "long startup", field: "ENDLESSFS_PREVIEW_STARTUP_TIMEOUT", value: "61s", want: "at most 1m0s"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(mapLookup(map[string]string{test.field: test.value}))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestURLAndLoopbackHelperBoundaryMatrix(t *testing.T) {
	t.Parallel()
	if _, err := parseBaseURL("%"); err == nil || !strings.Contains(err.Error(), "absolute HTTP(S) origin") {
		t.Fatalf("malformed base URL error = %v", err)
	}
	if _, err := parseBaseURL("ftp://example.com"); err == nil || !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("unsupported base URL scheme error = %v", err)
	}
	if normalized := normalizeLoopbackHost(""); normalized != "127.0.0.1" {
		t.Fatalf("normalized empty loopback host = %q", normalized)
	}
}

func TestLoadReadsValidatedProcessEnvironment(t *testing.T) {
	keys := []string{
		"ENDLESSFS_LISTEN_ADDR", "ENDLESSFS_BASE_URL", "ALLOW_REGISTRATION", "INVITE_REGISTRATION",
		"ENDLESSFS_STORAGE_PROVIDER", "ENDLESSFS_MOCK_PROVIDER_URL", "ENDLESSFS_BOOTSTRAP_TOKEN",
		"ENDLESSFS_GCS_FILE_BUCKET", "ENDLESSFS_GCS_STATE_BUCKET", "ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT", "ENDLESSFS_WRITER_SET_ID",
		"ENDLESSFS_SESSION_SECRET", "ENDLESSFS_WEBAUTHN_RP_ID", "ENDLESSFS_WEBAUTHN_RP_NAME",
		"ENDLESSFS_SESSION_TTL", "ENDLESSFS_DOWNLOAD_CAPABILITY_TTL", "ENDLESSFS_UPLOAD_INIT_TTL",
		"ENDLESSFS_TEXT_PREVIEW_MAX_BYTES", "ENDLESSFS_DEFAULT_LIGHT_THEME", "ENDLESSFS_DEFAULT_DARK_THEME",
		"ENDLESSFS_LOG_LEVEL", "ENDLESSFS_MEDIA_BROWSER_ENABLED", "ENDLESSFS_PREVIEW_PROVIDER",
		"ENDLESSFS_PREVIEW_AUTOMATIC", "ENDLESSFS_PREVIEW_FORMATS", "ENDLESSFS_PREVIEW_AUTO_MAX_AGE",
		"ENDLESSFS_PREVIEW_AUTO_MAX_SOURCE_BYTES", "ENDLESSFS_PREVIEW_RESOLUTIONS",
		"ENDLESSFS_PREVIEW_MAX_CONCURRENCY", "ENDLESSFS_PREVIEW_OPERATION_TIMEOUT",
		"ENDLESSFS_PREVIEW_STARTUP_TIMEOUT", "ENDLESSFS_PREVIEW_KEY_SECRET",
	}
	for _, key := range keys {
		value, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	if err := os.Setenv("ENDLESSFS_SESSION_SECRET", testSecret); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.SessionSecret.Reveal() != testSecret {
		t.Fatalf("Load = %+v, %v", cfg.Public(), err)
	}
}

func TestParseRegistrationMatrix(t *testing.T) {
	t.Parallel()

	for _, allow := range []string{"false", "true"} {
		for _, invite := range []string{"false", "true"} {
			allow, invite := allow, invite
			t.Run(allow+"/"+invite, func(t *testing.T) {
				t.Parallel()
				values := map[string]string{
					"ALLOW_REGISTRATION":  allow,
					"INVITE_REGISTRATION": invite,
				}
				cfg, err := Parse(mapLookup(values))
				if err != nil {
					t.Fatalf("Parse() error = %v", err)
				}
				if cfg.AllowRegistration != (allow == "true") {
					t.Fatalf("AllowRegistration = %v", cfg.AllowRegistration)
				}
				if cfg.InviteRegistration != (invite == "true") {
					t.Fatalf("InviteRegistration = %v", cfg.InviteRegistration)
				}
			})
		}
	}
}

func TestParseRejectsUnsafeOrMalformedValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "public listener", values: map[string]string{"ENDLESSFS_LISTEN_ADDR": "0.0.0.0:8080"}, want: "loopback"},
		{name: "missing port", values: map[string]string{"ENDLESSFS_LISTEN_ADDR": "localhost"}, want: "host:port"},
		{name: "loose boolean", values: map[string]string{"ALLOW_REGISTRATION": "TRUE"}, want: "exactly true or false"},
		{name: "numeric boolean", values: map[string]string{"INVITE_REGISTRATION": "1"}, want: "exactly true or false"},
		{name: "missing session secret", values: map[string]string{"ENDLESSFS_SESSION_SECRET": ""}, want: "required"},
		{name: "weak session secret", values: map[string]string{"ENDLESSFS_SESSION_SECRET": "not-random"}, want: "32 random bytes"},
		{name: "weak bootstrap token", values: map[string]string{"ENDLESSFS_BOOTSTRAP_TOKEN": "not-random"}, want: "32 random bytes"},
		{name: "HTTP public origin", values: map[string]string{"ENDLESSFS_BASE_URL": "http://example.com", "ENDLESSFS_LISTEN_ADDR": "0.0.0.0:8080"}, want: "HTTP is permitted only"},
		{name: "wildcard origin", values: map[string]string{"ENDLESSFS_BASE_URL": "https://*.example.com"}, want: "wildcard"},
		{name: "origin path", values: map[string]string{"ENDLESSFS_BASE_URL": "https://drive.example.com/path"}, want: "without credentials"},
		{name: "RP mismatch", values: map[string]string{"ENDLESSFS_BASE_URL": "https://drive.example.com", "ENDLESSFS_WEBAUTHN_RP_ID": "example.com"}, want: "exactly match"},
		{name: "unsupported provider", values: map[string]string{"ENDLESSFS_STORAGE_PROVIDER": "azure"}, want: "mock or gcs"},
		{name: "gcs missing file bucket", values: map[string]string{"ENDLESSFS_STORAGE_PROVIDER": "gcs", "ENDLESSFS_WRITER_SET_ID": "EREREREREREREREREREREQ"}, want: "GCS_FILE_BUCKET"},
		{name: "gcs missing writer set", values: map[string]string{"ENDLESSFS_STORAGE_PROVIDER": "gcs", "ENDLESSFS_GCS_FILE_BUCKET": "endlessfs-test"}, want: "WRITER_SET_ID"},
		{name: "gcs forbids mock endpoint", values: map[string]string{"ENDLESSFS_STORAGE_PROVIDER": "gcs", "ENDLESSFS_GCS_FILE_BUCKET": "endlessfs-test", "ENDLESSFS_WRITER_SET_ID": "EREREREREREREREREREREQ", "ENDLESSFS_MOCK_PROVIDER_URL": "http://127.0.0.1:9090"}, want: "unavailable with GCS"},
		{name: "mock forbids GCS file bucket", values: map[string]string{"ENDLESSFS_GCS_FILE_BUCKET": "endlessfs-test"}, want: "unavailable with mock"},
		{name: "mock forbids GCS state bucket", values: map[string]string{"ENDLESSFS_GCS_STATE_BUCKET": "endlessfs-state"}, want: "unavailable with mock"},
		{name: "invalid writer set", values: map[string]string{"ENDLESSFS_WRITER_SET_ID": "not-base64url"}, want: "canonical base64url"},
		{name: "invalid GCS signing account", values: map[string]string{"ENDLESSFS_STORAGE_PROVIDER": "gcs", "ENDLESSFS_GCS_FILE_BUCKET": "endlessfs-test", "ENDLESSFS_WRITER_SET_ID": "EREREREREREREREREREREQ", "ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT": "owner@example.com"}, want: "service-account email"},
		{name: "GCS signing account invalid character", values: map[string]string{"ENDLESSFS_STORAGE_PROVIDER": "gcs", "ENDLESSFS_GCS_FILE_BUCKET": "endlessfs-test", "ENDLESSFS_WRITER_SET_ID": "EREREREREREREREREREREQ", "ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT": "writer_name@example-project.iam.gserviceaccount.com"}, want: "service-account email"},
		{name: "empty RP name", values: map[string]string{"ENDLESSFS_WEBAUTHN_RP_NAME": " "}, want: "relying-party display name"},
		{name: "long session", values: map[string]string{"ENDLESSFS_SESSION_TTL": "169h"}, want: "at most"},
		{name: "invalid upload duration", values: map[string]string{"ENDLESSFS_UPLOAD_INIT_TTL": "forever"}, want: "duration"},
		{name: "invalid dark theme", values: map[string]string{"ENDLESSFS_DEFAULT_DARK_THEME": ""}, want: "invalid theme ID"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(mapLookup(test.values))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestParseGCSUsesExplicitFileBucketAndPortableWriterConfiguration(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"ENDLESSFS_STORAGE_PROVIDER":            "gcs",
		"ENDLESSFS_GCS_FILE_BUCKET":             "endlessfs-production",
		"ENDLESSFS_WRITER_SET_ID":               "EREREREREREREREREREREQ",
		"ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT": "endlessfs-writer@example-project.iam.gserviceaccount.com",
	}
	cfg, err := Parse(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageProvider != "gcs" || cfg.GCSFileBucket != values["ENDLESSFS_GCS_FILE_BUCKET"] || cfg.GCSStateBucket != cfg.GCSFileBucket || cfg.GCSSigningAccount != values["ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT"] || cfg.WriterSetID != values["ENDLESSFS_WRITER_SET_ID"] {
		t.Fatalf("GCS configuration = %+v", cfg)
	}
}

func TestParseGCSAllowsSeparateOrSharedStateBucket(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"ENDLESSFS_STORAGE_PROVIDER": "gcs",
		"ENDLESSFS_GCS_FILE_BUCKET":  "endlessfs-files",
		"ENDLESSFS_WRITER_SET_ID":    "EREREREREREREREREREREQ",
	}
	for name, stateBucket := range map[string]string{
		"separate": "endlessfs-state",
		"shared":   "endlessfs-files",
	} {
		t.Run(name, func(t *testing.T) {
			values := make(map[string]string, len(base)+1)
			for key, value := range base {
				values[key] = value
			}
			values["ENDLESSFS_GCS_STATE_BUCKET"] = stateBucket
			cfg, err := Parse(mapLookup(values))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.GCSFileBucket != "endlessfs-files" || cfg.GCSStateBucket != stateBucket {
				t.Fatalf("GCS buckets = files %q, state %q", cfg.GCSFileBucket, cfg.GCSStateBucket)
			}
		})
	}
}

func TestParseSecureConfiguration(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"ENDLESSFS_BASE_URL":         "https://drive.example.com:8443",
		"ENDLESSFS_LISTEN_ADDR":      "0.0.0.0:8443",
		"ENDLESSFS_WEBAUTHN_RP_ID":   "drive.example.com",
		"ENDLESSFS_WEBAUTHN_RP_NAME": "Private Drive",
		"ENDLESSFS_SESSION_TTL":      "24h",
		"ENDLESSFS_BOOTSTRAP_TOKEN":  testSecret,
		"ENDLESSFS_STORAGE_PROVIDER": "mock",
	}
	cfg, err := Parse(mapLookup(values))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Secure || cfg.AllowedOrigin != "https://drive.example.com:8443" || cfg.WebAuthnRPID != "drive.example.com" {
		t.Fatalf("secure configuration = %+v", cfg)
	}
	if cfg.BootstrapToken.Reveal() != testSecret || cfg.SessionSecret.Reveal() != testSecret {
		t.Fatal("validated secrets were not retained internally")
	}
}

func TestParseTransferConfiguration(t *testing.T) {
	values := map[string]string{
		"ENDLESSFS_MOCK_PROVIDER_URL":       "http://127.0.0.1:9090",
		"ENDLESSFS_DOWNLOAD_CAPABILITY_TTL": "90s",
		"ENDLESSFS_UPLOAD_INIT_TTL":         "7m",
		"ENDLESSFS_TEXT_PREVIEW_MAX_BYTES":  "2097152",
	}
	cfg, err := Parse(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MockProviderURL != values["ENDLESSFS_MOCK_PROVIDER_URL"] || cfg.DownloadCapabilityTTL != 90*time.Second || cfg.UploadInitTTL != 7*time.Minute || cfg.TextPreviewMaxBytes != 2<<20 {
		t.Fatalf("transfer config = %+v", cfg)
	}
	public := cfg.Public()
	if public.MaximumUploadInitializations != 100 || public.DefaultTransferConcurrency != 4 || public.MaximumTransferConcurrency != 8 {
		t.Fatalf("public limits = %+v", public)
	}
}

func TestParseLogLevel(t *testing.T) {
	for value, expected := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		t.Run(value, func(t *testing.T) {
			cfg, err := Parse(mapLookup(map[string]string{"ENDLESSFS_LOG_LEVEL": value}))
			if err != nil || cfg.LogLevel != expected {
				t.Fatalf("Parse() = level %v, %v", cfg.LogLevel, err)
			}
		})
	}
	if _, err := Parse(mapLookup(map[string]string{"ENDLESSFS_LOG_LEVEL": "verbose"})); err == nil {
		t.Fatal("Parse accepted an undocumented log level")
	}
}

func TestParseRejectsUnsafeTransferConfiguration(t *testing.T) {
	for name, testCase := range map[string][2]string{
		"remote mock":     {"ENDLESSFS_MOCK_PROVIDER_URL", "http://storage.example:9090"},
		"mock path":       {"ENDLESSFS_MOCK_PROVIDER_URL", "http://127.0.0.1:9090/cap"},
		"long download":   {"ENDLESSFS_DOWNLOAD_CAPABILITY_TTL", "11m"},
		"text too large":  {"ENDLESSFS_TEXT_PREVIEW_MAX_BYTES", "999999999"},
		"unsafe theme ID": {"ENDLESSFS_DEFAULT_LIGHT_THEME", "theme);display:none"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(mapLookup(map[string]string{testCase[0]: testCase[1]})); err == nil {
				t.Fatal("Parse accepted unsafe transfer configuration")
			}
		})
	}
}

func TestPublicContainsOnlyNonSecretPolicy(t *testing.T) {
	t.Parallel()

	public := (Config{AllowRegistration: true, InviteRegistration: false}).Public()
	if !public.AllowRegistration || public.InviteRegistration {
		t.Fatalf("Public() = %+v", public)
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"127.0.0.1:8080",
		"localhost:0",
		"[::1]:8080",
		"0.0.0.0:8080",
		"not-an-address",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, listenAddress string) {
		_, _ = Parse(mapLookup(map[string]string{
			"ENDLESSFS_LISTEN_ADDR": listenAddress,
		}))
	})
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name == "ENDLESSFS_SESSION_SECRET" {
			value, exists := values[name]
			if exists {
				return value, true
			}
			return testSecret, true
		}
		value, ok := values[name]
		return value, ok
	}
}

func intStrings(values []int) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strconv.Itoa(value)
	}
	return result
}
