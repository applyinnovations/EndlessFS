// Package config parses and validates EndlessFS process configuration.
package config

import (
	"fmt"
	"net"
	"os"
	"strings"
)

const defaultListenAddr = "127.0.0.1:8080"

// Config is the validated startup configuration currently implemented by the
// Milestone 0 scaffold. It will grow only alongside validation and tests.
type Config struct {
	ListenAddr         string
	AllowRegistration  bool
	InviteRegistration bool
}

// PublicConfig contains the non-secret settings safe to expose to browsers.
type PublicConfig struct {
	AllowRegistration  bool `json:"allowRegistration"`
	InviteRegistration bool `json:"inviteRegistration"`
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

	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return Config{}, fmt.Errorf("ENDLESSFS_LISTEN_ADDR: expected host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return Config{}, fmt.Errorf("ENDLESSFS_LISTEN_ADDR: Milestone 0 permits loopback listeners only")
	}

	allowRegistration, err := parseBool(lookup, "ALLOW_REGISTRATION", false)
	if err != nil {
		return Config{}, err
	}
	inviteRegistration, err := parseBool(lookup, "INVITE_REGISTRATION", true)
	if err != nil {
		return Config{}, err
	}

	return Config{
		ListenAddr:         listenAddr,
		AllowRegistration:  allowRegistration,
		InviteRegistration: inviteRegistration,
	}, nil
}

// Public returns a copy containing no secrets.
func (c Config) Public() PublicConfig {
	return PublicConfig{
		AllowRegistration:  c.AllowRegistration,
		InviteRegistration: c.InviteRegistration,
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
