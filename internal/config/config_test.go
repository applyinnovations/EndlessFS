package config

import (
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(func(string) (string, bool) { return "", false })
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
		value, ok := values[name]
		return value, ok
	}
}
