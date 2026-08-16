// Package config parses and validates EndlessFS process configuration.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/secret"
)

const (
	defaultListenAddr = "127.0.0.1:8080"
	defaultSessionTTL = 12 * time.Hour
	maximumSessionTTL = 7 * 24 * time.Hour
)

// Config is the validated process configuration. Secret fields use a redacting
// value type and are never included in PublicConfig.
type Config struct {
	ListenAddr         string
	BaseURL            string
	AllowedOrigin      string
	Secure             bool
	StorageProvider    string
	AllowRegistration  bool
	InviteRegistration bool
	BootstrapToken     secret.Value
	SessionSecret      secret.Value
	WebAuthnRPID       string
	WebAuthnRPName     string
	SessionTTL         time.Duration
}

// PublicConfig contains the non-secret settings safe to expose to browsers.
type PublicConfig struct {
	AllowRegistration  bool `json:"allowRegistration"`
	InviteRegistration bool `json:"inviteRegistration"`
	PasskeysAvailable  bool `json:"passkeysAvailable"`
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
	if storageProvider != "mock" {
		return Config{}, fmt.Errorf("ENDLESSFS_STORAGE_PROVIDER: v1 supports exactly mock")
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

	return Config{
		ListenAddr:         listenAddr,
		BaseURL:            strings.TrimSuffix(baseURL.String(), "/"),
		AllowedOrigin:      strings.TrimSuffix(baseURL.String(), "/"),
		Secure:             secure,
		StorageProvider:    storageProvider,
		AllowRegistration:  allowRegistration,
		InviteRegistration: inviteRegistration,
		BootstrapToken:     bootstrapToken,
		SessionSecret:      sessionSecret,
		WebAuthnRPID:       strings.ToLower(rpID),
		WebAuthnRPName:     rpName,
		SessionTTL:         sessionTTL,
	}, nil
}

// Public returns a copy containing no secrets.
func (c Config) Public() PublicConfig {
	return PublicConfig{
		AllowRegistration:  c.AllowRegistration,
		InviteRegistration: c.InviteRegistration,
		PasskeysAvailable:  true,
	}
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
