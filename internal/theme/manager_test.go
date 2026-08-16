package theme

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func themeUserID(t *testing.T) domain.UserID {
	t.Helper()
	value, err := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestThemePreferenceIsSeparatePersistentAndSafelyResolved(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	store := state.NewMemoryStore()
	clock := domain.NewFixedClock(time.Date(2033, 1, 2, 3, 4, 5, 0, time.UTC))
	manager, err := NewManager(registry, store, "endlessfs-light", "endlessfs-dark", true, clock)
	if err != nil {
		t.Fatal(err)
	}
	userID := themeUserID(t)
	ctx := context.Background()
	preference, _, err := manager.Preference(ctx, userID)
	if err != nil || preference != "system" {
		t.Fatalf("default preference = %q, %v", preference, err)
	}
	dark := manager.Resolve("system", true, false)
	if dark.Resolved.ID != "endlessfs-dark" || dark.Fallback {
		t.Fatalf("dark system = %+v", dark)
	}
	selected, err := manager.SetPreference(ctx, userID, "endlessfs-dark")
	if err != nil || selected.Resolved.ID != "endlessfs-dark" {
		t.Fatalf("SetPreference = %+v, %v", selected, err)
	}
	second, err := NewManager(registry, store, "endlessfs-light", "endlessfs-dark", true, clock)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := second.ResolvePreference(ctx, userID, false, false)
	if err != nil || persisted.Preference != "endlessfs-dark" || persisted.Resolved.ID != "endlessfs-dark" {
		t.Fatalf("persistent preference = %+v, %v", persisted, err)
	}
	if _, err := manager.SetPreference(ctx, userID, "missing.theme"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing selection = %v", err)
	}
	safe, err := manager.ResolvePreference(ctx, userID, true, true)
	if err != nil || safe.Resolved.ID != "endlessfs-light" || !safe.Fallback {
		t.Fatalf("safe selection = %+v, %v", safe, err)
	}
	cookie := manager.DeviceCookie(selected)
	if cookie.Name != SecureDeviceCookieName || cookie.Value != "endlessfs-dark" || cookie.HttpOnly || !cookie.Secure || !cookie.Expires.Equal(clock.Now().Add(365*24*time.Hour)) {
		t.Fatalf("device cookie = %+v", cookie)
	}
	device := manager.ResolveDevice("uninstalled.theme", true, false)
	if device.Resolved.ID != "endlessfs-dark" {
		t.Fatalf("invalid device cookie resolution = %+v", device)
	}
}

func TestThemeManagerRejectsWrongAppearanceDefaults(t *testing.T) {
	registry, _ := NewRegistry()
	store := state.NewMemoryStore()
	clock := domain.NewFixedClock(time.Now())
	if _, err := NewManager(registry, store, "endlessfs-dark", "endlessfs-light", false, clock); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("wrong defaults = %v", err)
	}
}
