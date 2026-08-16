package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

var testSecret = base64.RawURLEncoding.EncodeToString(make([]byte, 32))

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
		{name: "unsupported provider", values: map[string]string{"ENDLESSFS_STORAGE_PROVIDER": "gcs"}, want: "exactly mock"},
		{name: "long session", values: map[string]string{"ENDLESSFS_SESSION_TTL": "169h"}, want: "at most"},
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
