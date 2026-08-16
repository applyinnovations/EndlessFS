package theme

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	SecureDeviceCookieName = "__Host-endlessfs_theme"
	DevDeviceCookieName    = "endlessfs_theme_dev"
)

type Manager struct {
	registry     *Registry
	store        state.Store
	defaultLight string
	defaultDark  string
	secure       bool
	clock        domain.Clock
}

type AssetDescriptor struct {
	URL         string `json:"url"`
	FallbackURL string `json:"fallbackURL"`
	ContentType string `json:"contentType"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Rendering   string `json:"rendering"`
}
type Selection struct {
	Preference string                     `json:"preference"`
	Resolved   Metadata                   `json:"resolved"`
	Fallback   bool                       `json:"fallback"`
	CSSURL     string                     `json:"cssURL"`
	Assets     map[string]AssetDescriptor `json:"assets"`
}

func NewManager(registry *Registry, store state.Store, defaultLight, defaultDark string, secure bool, clock domain.Clock) (*Manager, error) {
	if registry == nil || store == nil || clock == nil {
		return nil, domain.NewError(domain.ErrorInvalid, "theme manager requires registry, state, and clock")
	}
	light, lightOK := registry.Theme(defaultLight)
	dark, darkOK := registry.Theme(defaultDark)
	if !lightOK || !darkOK || light.Appearance != AppearanceLight || dark.Appearance != AppearanceDark {
		return nil, domain.NewError(domain.ErrorInvalid, "configured default themes are unavailable or have the wrong appearance")
	}
	return &Manager{registry: registry, store: store, defaultLight: defaultLight, defaultDark: defaultDark, secure: secure, clock: clock}, nil
}

func (m *Manager) Metadata() []Metadata       { return m.registry.Metadata() }
func (m *Manager) TokenRegistry() []TokenSpec { return TokenRegistry() }
func (m *Manager) MediaRegistry() []MediaSlot { return MediaRegistry() }

func preferenceKey(userID domain.UserID) state.Key {
	return state.MustKey(state.NamespacePreferences, userID.String(), "theme")
}

func (m *Manager) Preference(ctx context.Context, userID domain.UserID) (string, state.Version, error) {
	value, err := m.store.Get(ctx, preferenceKey(userID))
	if errors.Is(err, domain.ErrNotFound) {
		return "system", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var record model.ThemePreference
	if err := state.DecodeJSON(value.Data, &record); err != nil {
		return "", "", err
	}
	return record.ThemeID, value.Version, nil
}

func (m *Manager) SetPreference(ctx context.Context, userID domain.UserID, themeID string) (Selection, error) {
	if themeID != "system" && !m.registry.Installed(themeID) {
		return Selection{}, domain.NewError(domain.ErrorInvalid, "theme is not installed")
	}
	record := model.ThemePreference{SchemaVersion: model.SchemaVersion, ThemeID: themeID}
	data, err := state.EncodeJSON(&record)
	if err != nil {
		return Selection{}, err
	}
	key := preferenceKey(userID)
	for attempts := 0; attempts < 16; attempts++ {
		value, getErr := m.store.Get(ctx, key)
		if errors.Is(getErr, domain.ErrNotFound) {
			if _, err = m.store.Create(ctx, key, data); errors.Is(err, domain.ErrConflict) {
				continue
			}
			if err != nil {
				return Selection{}, err
			}
			return m.Resolve(themeID, false, false), nil
		}
		if getErr != nil {
			return Selection{}, getErr
		}
		if _, err = m.store.CompareAndSwap(ctx, key, value.Version, data); errors.Is(err, domain.ErrPreconditionFailed) {
			continue
		}
		if err != nil {
			return Selection{}, err
		}
		return m.Resolve(themeID, false, false), nil
	}
	return Selection{}, domain.NewError(domain.ErrorConflict, "theme preference changed concurrently")
}

func (m *Manager) Resolve(preference string, dark, safe bool) Selection {
	theme, fallback := m.registry.Resolve(preference, dark, safe, m.defaultLight, m.defaultDark)
	selection := Selection{Preference: preference, Resolved: theme.Metadata(), Fallback: fallback, CSSURL: "/assets/themes/" + theme.Digest + "/theme.css", Assets: make(map[string]AssetDescriptor, len(theme.Assets))}
	slots := make(map[string]MediaSlot)
	for _, slot := range MediaRegistry() {
		slots[slot.ID] = slot
	}
	for id, asset := range theme.Assets {
		primary, parent, _ := m.registry.AssetURL(theme, id)
		slot := slots[id]
		selection.Assets[id] = AssetDescriptor{URL: primary, FallbackURL: parent, ContentType: asset.Media.ContentType, Width: asset.Media.Width, Height: asset.Media.Height, Rendering: slot.Rendering}
	}
	return selection
}

func (m *Manager) ResolvePreference(ctx context.Context, userID domain.UserID, dark, safe bool) (Selection, error) {
	preference, _, err := m.Preference(ctx, userID)
	if err != nil {
		return Selection{}, err
	}
	return m.Resolve(preference, dark, safe), nil
}

func (m *Manager) ResolveDevice(cookieValue string, dark, safe bool) Selection {
	if cookieValue != "" && !m.registry.Installed(cookieValue) {
		cookieValue = "system"
	}
	return m.Resolve(cookieValue, dark, safe)
}

func (m *Manager) DeviceCookie(selection Selection) *http.Cookie {
	name := SecureDeviceCookieName
	if !m.secure {
		name = DevDeviceCookieName
	}
	// #nosec G124 -- the non-secret appearance cookie is intentionally JS-readable; insecure mode is configuration-limited to loopback development.
	return &http.Cookie{Name: name, Value: selection.Resolved.ID, Path: "/", Expires: m.clock.Now().Add(365 * 24 * time.Hour), MaxAge: 365 * 24 * 60 * 60, Secure: m.secure, HttpOnly: false, SameSite: http.SameSiteStrictMode}
}
func (m *Manager) DeviceCookieName() string {
	if m.secure {
		return SecureDeviceCookieName
	}
	return DevDeviceCookieName
}

func (m *Manager) Asset(digest, name string) (AssetResponse, bool) {
	return m.registry.Asset(digest, name)
}
