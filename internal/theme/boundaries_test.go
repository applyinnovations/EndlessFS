package theme

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

func validTestPNG(t *testing.T) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			value.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, value); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func richCustomBundle(t *testing.T) Bundle {
	t.Helper()
	manifest := customManifest()
	manifest.Assets["icon.file"] = AssetReference{Path: "assets/icons.png", X: 0, Y: 0, Width: 4, Height: 4, PixelRatio: 2, Sprite: true}
	return Bundle{Manifest: manifest, Files: map[string][]byte{
		"assets/icons.png": validTestPNG(t),
	}}
}

func TestThemeAPIRegistriesSerializersAndTokenKinds(t *testing.T) {
	if len(ContrastRegistry()) == 0 || len(TokenRegistry()) == 0 || len(MediaRegistry()) == 0 {
		t.Fatal("Theme API registries are incomplete")
	}
	if got := (TokenValue{Kind: TokenColor, Color: "#abcdef"}).CSS(TokenSpec{}); got != "#abcdef" {
		t.Fatalf("color CSS = %q", got)
	}
	if _, err := parseTokenValue(TokenSpec{Kind: "unknown"}, json.RawMessage(`1`)); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("unknown token kind = %v", err)
	}
	if _, err := parseTokenValue(TokenSpec{Kind: TokenColor}, json.RawMessage(`"red"`)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid purpose color = %v", err)
	}
	if _, err := relativeLuminance("red"); err == nil {
		t.Fatal("relativeLuminance accepted a raw CSS color")
	}
}

func TestThemeRichCompileRegistryAssets(t *testing.T) {
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	bundle := richCustomBundle(t)
	resolved, err := NewCompiler(builtins).Compile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resolved.CSS, "@font-face") {
		t.Fatalf("new Theme API unexpectedly controls application fonts: %s", resolved.CSS)
	}
	registry, err := NewRegistry(bundle)
	if err != nil {
		t.Fatal(err)
	}
	metadata := registry.Metadata()
	if len(metadata) != 3 || metadata[0].ID > metadata[1].ID {
		t.Fatalf("registry metadata = %+v", metadata)
	}
	if !registry.Installed(resolved.ID) {
		t.Fatal("custom theme was not installed")
	}
	if dark, fallback := registry.Resolve("missing-dark-theme", false, false, "endlessfs-light", "endlessfs-dark"); !fallback || dark.ID != "endlessfs-dark" {
		t.Fatalf("dark-named fallback = %q %v", dark.ID, fallback)
	}
	css, ok := registry.Asset(resolved.Digest, "theme.css")
	if !ok || css.ContentType != "text/css; charset=utf-8" || len(css.Data) == 0 {
		t.Fatalf("CSS asset = %+v %v", css, ok)
	}
	for _, name := range []string{"", "../theme.css", "folder/theme.css", "bad\\name", "missing.svg"} {
		if _, ok := registry.Asset(resolved.Digest, name); ok {
			t.Fatalf("unsafe/missing asset %q resolved", name)
		}
	}
	if _, ok := registry.Asset("missing-digest", "theme.css"); ok {
		t.Fatal("missing digest resolved")
	}
	primary, fallback, ok := registry.AssetURL(resolved, "icon.file")
	if !ok || primary == "" || fallback == "" {
		t.Fatalf("AssetURL = %q %q %v", primary, fallback, ok)
	}
	if _, _, ok := registry.AssetURL(resolved, "missing.slot"); ok {
		t.Fatal("missing asset slot resolved")
	}
}

func TestThemeBundleSuccessfulDirectoryArchiveAndManifestReferences(t *testing.T) {
	manifest := customManifest()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "theme.json"), encodeManifest(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBundle(directory)
	if err != nil || loaded.Manifest.ID != manifest.ID || len(loaded.Files) != 0 {
		t.Fatalf("LoadBundle directory = %+v, %v", loaded, err)
	}
	archivePath := filepath.Join(t.TempDir(), "valid.efstheme")
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(archiveFile)
	directoryHeader := &zip.FileHeader{Name: "assets/"}
	directoryHeader.SetMode(os.ModeDir | 0755)
	if _, err := archive.CreateHeader(directoryHeader); err != nil {
		t.Fatal(err)
	}
	writeZIP(t, archive, "theme.json", encodeManifest(manifest))
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadBundle(archivePath)
	if err != nil || loaded.Manifest.ID != manifest.ID {
		t.Fatalf("LoadBundle archive = %+v, %v", loaded, err)
	}
	if _, err := loadArchive(archivePath, MaximumCompressedBundleBytes+1); err == nil {
		t.Fatal("archive rejected an excessive caller-supplied compressed size")
	}
	registry, err := LoadRegistry(directory, archivePath)
	if err == nil || registry != nil {
		t.Fatal("duplicate bundle IDs from paths were accepted")
	}
	if _, err := LoadBundle(filepath.Join(directory, "missing")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing bundle = %v", err)
	}
	wrongExtension := filepath.Join(t.TempDir(), "theme.zip")
	if err := os.WriteFile(wrongExtension, []byte("zip"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(wrongExtension); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("wrong archive extension = %v", err)
	}

	for _, reference := range []AssetReference{{Path: "assets/icon.svg"}, {Path: "assets/sprite.png", X: 1, Y: 2, Width: 3, Height: 4, PixelRatio: 2, Sprite: true}} {
		data, err := json.Marshal(reference)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AssetReference
		if err := json.Unmarshal(data, &decoded); err != nil || decoded.Path != reference.Path || decoded.Sprite != reference.Sprite {
			t.Fatalf("asset reference JSON = %+v, %v", decoded, err)
		}
	}
	if _, err := bundleFromFiles(map[string][]byte{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing manifest bundle = %v", err)
	}
	if _, err := bundleFromFiles(map[string][]byte{"theme.json": []byte(`{"unknown":true}`)}); err == nil {
		t.Fatal("invalid manifest bundle was accepted")
	}
	if !validLicense("Apache-2.0") || !validLicense("LicenseRef-Private") || validLicense("LicenseRef-bad/path") || validLicense("https://license") {
		t.Fatal("license allowlist boundary is incorrect")
	}
}

func TestThemeMediaEncodingAndSVGEdgeMatrix(t *testing.T) {
	slot := MediaSlot{Accepted: []string{"image/svg+xml", "image/png", "image/webp", "image/avif"}, MaximumBytes: 1024, MaximumDimension: 64, MaximumPixels: 4096}
	for name, data := range map[string][]byte{"empty.svg": {}, "bad.gif": []byte("GIF89a"), "bad.png": []byte("not-png"), "bad.webp": []byte("bad"), "bad.avif": []byte("bad")} {
		if _, err := validateMedia("assets/"+name, data, slot); err == nil {
			t.Fatalf("invalid media %s accepted", name)
		}
	}
	if _, err := validateMedia("assets/large.svg", bytes.Repeat([]byte{'x'}, 1025), slot); err == nil {
		t.Fatal("oversized media accepted")
	}
	limited := slot
	limited.MaximumDimension = 4
	if _, err := validateMedia("assets/large.png", validTestPNG(t), limited); err == nil {
		t.Fatal("decoded dimension limit was not enforced")
	}
	for name, data := range map[string][]byte{
		"vp8": func() []byte {
			value := make([]byte, 30)
			copy(value[:4], "RIFF")
			copy(value[8:12], "WEBP")
			copy(value[12:16], "VP8 ")
			value[23], value[24], value[25] = 0x9d, 0x01, 0x2a
			binary.LittleEndian.PutUint16(value[26:28], 8)
			binary.LittleEndian.PutUint16(value[28:30], 7)
			return value
		}(),
		"vp8l": func() []byte {
			value := make([]byte, 30)
			copy(value[:4], "RIFF")
			copy(value[8:12], "WEBP")
			copy(value[12:16], "VP8L")
			value[20] = 0x2f
			return value
		}(),
	} {
		width, height, err := decodeWebP(data)
		if err != nil || width < 1 || height < 1 {
			t.Fatalf("%s WebP = %dx%d, %v", name, width, height, err)
		}
	}
	for _, data := range [][]byte{
		append([]byte("RIFF0000WEBPXXXX"), make([]byte, 20)...),
		func() []byte {
			value := make([]byte, 30)
			copy(value[:4], "RIFF")
			copy(value[8:12], "WEBP")
			copy(value[12:16], "VP8 ")
			return value
		}(),
		func() []byte {
			value := make([]byte, 30)
			copy(value[:4], "RIFF")
			copy(value[8:12], "WEBP")
			copy(value[12:16], "VP8L")
			return value
		}(),
	} {
		if _, _, err := decodeWebP(data); err == nil {
			t.Fatal("invalid WebP frame accepted")
		}
	}
	invalidAVIF := make([]byte, 24)
	copy(invalidAVIF[4:8], "ftyp")
	copy(invalidAVIF[8:12], "avif")
	if _, _, err := decodeAVIF(invalidAVIF); err == nil {
		t.Fatal("AVIF without dimensions accepted")
	}
	for _, svg := range []string{
		`<g/>`,
		`<svg width="1" height="1"><g>text</g></svg>`,
		`<svg width="1.5" height="1"/>`,
		`<svg viewBox="0 0 bad 1"/>`,
		`<svg viewBox="0 0 4 5"/>trailing`,
		`<?xml version="1.0"?><svg width="1" height="1"/>`,
		`<svg width="1" height="1"><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g><g/></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></g></svg>`,
	} {
		_, _, _ = sanitizeSVG([]byte(svg))
	}
	if width, height, err := sanitizeSVG([]byte(`<svg viewBox="0 0 4 5"/>`)); err != nil || width != 4 || height != 5 {
		t.Fatalf("viewBox SVG = %dx%d, %v", width, height, err)
	}
	for _, value := range []string{"0", "-1", "1.5", "x"} {
		if _, err := svgDimension(value); err == nil {
			t.Fatalf("SVG dimension %q accepted", value)
		}
	}
	if err := validateSprite(AssetReference{}, ValidatedAsset{}); err != nil {
		t.Fatalf("non-sprite = %v", err)
	}
}

func TestThemeManagerCompleteFacadeAndInvalidState(t *testing.T) {
	registry, err := NewRegistry(richCustomBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	store := state.NewMemoryStore()
	clock := domain.NewFixedClock(time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, create := range []func() (*Manager, error){
		func() (*Manager, error) {
			return NewManager(nil, store, "endlessfs-light", "endlessfs-dark", false, clock)
		},
		func() (*Manager, error) {
			return NewManager(registry, nil, "endlessfs-light", "endlessfs-dark", false, clock)
		},
		func() (*Manager, error) {
			return NewManager(registry, store, "endlessfs-light", "endlessfs-dark", false, nil)
		},
	} {
		if _, err := create(); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid manager = %v", err)
		}
	}
	manager, err := NewManager(registry, store, "endlessfs-light", "endlessfs-dark", false, clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.Metadata()) != 3 || len(manager.TokenRegistry()) == 0 || len(manager.MediaRegistry()) == 0 || manager.DeviceCookieName() != DevDeviceCookieName {
		t.Fatal("manager facade is incomplete")
	}
	userID := themeUserID(t)
	selection, err := manager.SetPreference(context.Background(), userID, "system")
	if err != nil || selection.Resolved.ID != "endlessfs-light" {
		t.Fatalf("SetPreference system = %+v, %v", selection, err)
	}
	selection, err = manager.SetPreference(context.Background(), userID, "com.example.theme")
	if err != nil || selection.Resolved.ID != "com.example.theme" {
		t.Fatalf("SetPreference update = %+v, %v", selection, err)
	}
	if cookie := manager.DeviceCookie(selection); cookie.Name != DevDeviceCookieName || cookie.Secure {
		t.Fatalf("development device cookie = %#v", cookie)
	}
	assetTheme, _ := registry.Theme("com.example.theme")
	assetName := assetFilename(assetTheme.Assets["icon.file"].Media)
	if response, ok := manager.Asset(assetTheme.Digest, assetName); !ok || len(response.Data) == 0 {
		t.Fatalf("manager asset = %+v %v", response, ok)
	}
	key := preferenceKey(themeUserID(t))
	value, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwap(context.Background(), key, value.Version, []byte(`{"schemaVersion":1,"themeID":"UPPER"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Preference(context.Background(), userID); err == nil {
		t.Fatal("invalid stored theme preference was accepted")
	}
}

type themeFaultStore struct {
	get func(context.Context, state.Key) (state.Value, error)
	new func(context.Context, state.Key, []byte) (state.Version, error)
	cas func(context.Context, state.Key, state.Version, []byte) (state.Version, error)
}

func (s themeFaultStore) Get(ctx context.Context, key state.Key) (state.Value, error) {
	if s.get != nil {
		return s.get(ctx, key)
	}
	return state.Value{}, domain.NewError(domain.ErrorNotFound, "missing")
}
func (themeFaultStore) List(context.Context, state.Prefix, state.PageRequest) (state.Page, error) {
	return state.Page{}, domain.NewError(domain.ErrorUnavailable, "unused")
}
func (s themeFaultStore) Create(ctx context.Context, key state.Key, data []byte) (state.Version, error) {
	if s.new != nil {
		return s.new(ctx, key, data)
	}
	return "v1", nil
}
func (s themeFaultStore) CompareAndSwap(ctx context.Context, key state.Key, version state.Version, data []byte) (state.Version, error) {
	if s.cas != nil {
		return s.cas(ctx, key, version, data)
	}
	return "v2", nil
}
func (themeFaultStore) Delete(context.Context, state.Key, state.Version) error { return nil }

func TestThemeManagerFaultAndConcurrencyMatrix(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	clock := domain.NewFixedClock(time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC))
	userID := themeUserID(t)
	unavailable := domain.NewError(domain.ErrorUnavailable, "fault")
	manager := func(store state.Store) *Manager {
		value, createErr := NewManager(registry, store, "endlessfs-light", "endlessfs-dark", true, clock)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return value
	}
	if _, _, err := manager(themeFaultStore{get: func(context.Context, state.Key) (state.Value, error) {
		return state.Value{}, unavailable
	}}).Preference(context.Background(), userID); !errors.Is(err, unavailable) {
		t.Fatalf("preference fault = %v", err)
	}
	if _, err := manager(themeFaultStore{get: func(context.Context, state.Key) (state.Value, error) {
		return state.Value{}, unavailable
	}}).ResolvePreference(context.Background(), userID, false, false); !errors.Is(err, unavailable) {
		t.Fatalf("resolve preference fault = %v", err)
	}
	if _, err := manager(themeFaultStore{new: func(context.Context, state.Key, []byte) (state.Version, error) {
		return "", unavailable
	}}).SetPreference(context.Background(), userID, "system"); !errors.Is(err, unavailable) {
		t.Fatalf("create fault = %v", err)
	}
	if _, err := manager(themeFaultStore{new: func(context.Context, state.Key, []byte) (state.Version, error) {
		return "", domain.NewError(domain.ErrorConflict, "race")
	}}).SetPreference(context.Background(), userID, "system"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("create conflict exhaustion = %v", err)
	}
	existing := state.Value{Data: []byte(`{"schemaVersion":1,"themeID":"system"}`), Version: "v1"}
	if _, err := manager(themeFaultStore{get: func(context.Context, state.Key) (state.Value, error) { return existing, nil }, cas: func(context.Context, state.Key, state.Version, []byte) (state.Version, error) {
		return "", unavailable
	}}).SetPreference(context.Background(), userID, "endlessfs-light"); !errors.Is(err, unavailable) {
		t.Fatalf("CAS fault = %v", err)
	}
	if _, err := manager(themeFaultStore{get: func(context.Context, state.Key) (state.Value, error) { return existing, nil }, cas: func(context.Context, state.Key, state.Version, []byte) (state.Version, error) {
		return "", domain.NewError(domain.ErrorPreconditionFailed, "race")
	}}).SetPreference(context.Background(), userID, "endlessfs-light"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CAS conflict exhaustion = %v", err)
	}
	secure := manager(state.NewMemoryStore())
	selection := secure.Resolve("system", false, false)
	if secure.DeviceCookieName() != SecureDeviceCookieName || !secure.DeviceCookie(selection).Secure {
		t.Fatal("secure device cookie contract is incomplete")
	}
}

func TestThemeCompilerRejectionBranches(t *testing.T) {
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	compiler := NewCompiler(builtins)
	if _, err := NewCompiler(nil).Compile(Bundle{Manifest: customManifest(), Files: map[string][]byte{}}); err == nil {
		t.Fatal("unavailable configured parent accepted")
	}
	invalidDirect := customManifest()
	invalidDirect.ID = "INVALID"
	if _, err := compileBundle(Bundle{Manifest: invalidDirect, Files: map[string][]byte{}}, builtins["endlessfs-light"], false); err == nil {
		t.Fatal("direct compiler accepted invalid metadata")
	}
	manifest := customManifest()
	manifest.Extends = "missing.parent"
	if _, err := compiler.Compile(Bundle{Manifest: manifest, Files: map[string][]byte{}}); err == nil {
		t.Fatal("missing parent accepted")
	}
	for name, mutate := range map[string]func(*Manifest, map[string][]byte){
		"nil tokens":    func(m *Manifest, _ map[string][]byte) { m.Tokens = nil },
		"nil assets":    func(m *Manifest, _ map[string][]byte) { m.Assets = nil },
		"bad color":     func(m *Manifest, _ map[string][]byte) { m.Tokens["color.primary"] = json.RawMessage(`7`) },
		"bad integer":   func(m *Manifest, _ map[string][]byte) { m.Tokens["motion.fast"] = json.RawMessage(`"fast"`) },
		"bad shadow":    func(m *Manifest, _ map[string][]byte) { m.Tokens["elevation.low"] = json.RawMessage(`{"x":100}`) },
		"unknown media": func(m *Manifest, _ map[string][]byte) { m.Assets["unknown"] = AssetReference{Path: "assets/a.png"} },
		"unsafe media path": func(m *Manifest, _ map[string][]byte) {
			m.Assets["icon.file"] = AssetReference{Path: "../a.png"}
		},
		"missing media": func(m *Manifest, _ map[string][]byte) { m.Assets["icon.file"] = AssetReference{Path: "assets/a.png"} },
		"invalid media": func(m *Manifest, files map[string][]byte) {
			m.Assets["icon.file"] = AssetReference{Path: "assets/a.png"}
			files["assets/a.png"] = []byte("bad")
		},
		"invalid sprite": func(m *Manifest, files map[string][]byte) {
			m.Assets["icon.file"] = AssetReference{Path: "assets/a.png", Sprite: true, Width: 100, Height: 100, PixelRatio: 1}
			files["assets/a.png"] = validTestPNG(t)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := customManifest()
			files := map[string][]byte{}
			mutate(&candidate, files)
			if _, err := compiler.Compile(Bundle{Manifest: candidate, Files: files}); err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
	missingTokens := &ResolvedTheme{ID: "parent", Appearance: AppearanceLight, Tokens: map[string]TokenValue{}, Assets: map[string]ResolvedAsset{}}
	if _, err := compileBundle(Bundle{Manifest: customManifest(), Files: map[string][]byte{}}, missingTokens, false); err == nil {
		t.Fatal("incomplete parent tokens accepted")
	}
	completeParent, err := NewCompiler(builtins).Compile(richCustomBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compileBundle(Bundle{Manifest: customManifest(), Files: map[string][]byte{}}, completeParent, false); err != nil {
		t.Fatalf("complete parent inheritance = %v", err)
	}
	incompleteAssets := *builtins["endlessfs-light"]
	incompleteAssets.Assets = make(map[string]ResolvedAsset, len(builtins["endlessfs-light"].Assets)-1)
	for id, asset := range builtins["endlessfs-light"].Assets {
		if id != "icon.file" {
			incompleteAssets.Assets[id] = asset
		}
	}
	assetManifest := customManifest()
	assetManifest.Assets["icon.file"] = AssetReference{Path: "assets/file.png"}
	if _, err := compileBundle(Bundle{Manifest: assetManifest, Files: map[string][]byte{"assets/file.png": validTestPNG(t)}}, &incompleteAssets, false); err == nil {
		t.Fatal("custom theme with a divergent parent asset set accepted")
	}
	builtinManifest := customManifest()
	builtinManifest.ID = "endlessfs-light"
	builtinManifest.Extends = ""
	if _, err := compileBundle(Bundle{Manifest: builtinManifest, Files: map[string][]byte{}}, nil, true); err == nil {
		t.Fatal("incomplete built-in media accepted")
	}
}

func TestThemeRegistryBuildInputsAndFailureBranches(t *testing.T) {
	old := buildInputBundles
	defer func() { buildInputBundles = old }()
	buildInputBundles = nil
	manifest := customManifest()
	mustRegisterBuildInput(string(encodeManifest(manifest)), map[string]string{})
	registry, err := NewRegistry()
	if err != nil || !registry.Installed(manifest.ID) {
		t.Fatalf("registered build input = %v", err)
	}
	shadow := customManifest()
	shadow.ID = "endlessfs-light"
	if _, err := NewRegistry(Bundle{Manifest: shadow, Files: map[string][]byte{}}); err == nil {
		t.Fatal("built-in theme shadow accepted")
	}
	invalid := customManifest()
	invalid.Tokens = nil
	if _, err := NewRegistry(Bundle{Manifest: invalid, Files: map[string][]byte{}}); err == nil {
		t.Fatal("invalid custom registry theme accepted")
	}
	if _, err := LoadRegistry(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing registry path accepted")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("invalid build input did not panic")
		}
	}()
	mustRegisterBuildInput(`{"unknown":true}`, nil)
}

func TestThemeManifestPathAndMetadataBoundaryMatrix(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`"unterminated`), []byte(`{"path":7}`)} {
		var reference AssetReference
		if err := json.Unmarshal(raw, &reference); err == nil {
			t.Fatalf("invalid asset reference %q accepted", raw)
		}
	}
	for _, name := range []string{"", "/absolute", "bad\\path", string([]byte{0xff}), "assets/a/../b.png", "assets//b.png", "assets/a/b/c/d/e/f/g/h/i.png", "outside.png", "assets/code.js", "assets/x\x00.png"} {
		if _, err := normalizeBundlePath(name); err == nil {
			t.Fatalf("unsafe path %q accepted", name)
		}
	}
	for name, mutate := range map[string]func(*Manifest){
		"negative API": func(m *Manifest) { m.ThemeAPI.Minor = -1 },
		"long ID":      func(m *Manifest) { m.ID = strings.Repeat("a", 64) + "." + strings.Repeat("b", 64) },
		"empty name":   func(m *Manifest) { m.Name = "" },
		"bad author":   func(m *Manifest) { m.Author = "bad\nname" },
		"appearance":   func(m *Manifest) { m.Appearance = "sepia" },
	} {
		t.Run(name, func(t *testing.T) {
			manifest := customManifest()
			mutate(&manifest)
			if err := validateManifestMetadata(manifest, false); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	builtin := customManifest()
	builtin.ID, builtin.Extends = "endlessfs-light", "endlessfs-dark"
	if err := validateManifestMetadata(builtin, true); err == nil {
		t.Fatal("built-in with parent accepted")
	}
	tooLarge := filepath.Join(t.TempDir(), "large.efstheme")
	file, err := os.Create(tooLarge)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaximumCompressedBundleBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(tooLarge); err == nil {
		t.Fatal("oversized archive accepted")
	}
	symlinkTarget := filepath.Join(t.TempDir(), "target.efstheme")
	if err := os.WriteFile(symlinkTarget, nil, 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(t.TempDir(), "link.efstheme")
	if err := os.Symlink(symlinkTarget, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(symlink); err == nil {
		t.Fatal("bundle symlink accepted")
	}
	malformed := filepath.Join(t.TempDir(), "malformed.efstheme")
	if err := os.WriteFile(malformed, []byte("not a zip"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(malformed); err == nil {
		t.Fatal("malformed archive accepted")
	}
	tooMany := filepath.Join(t.TempDir(), "too-many.efstheme")
	archiveFile, err := os.Create(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(archiveFile)
	directoryHeader := &zip.FileHeader{Name: "assets/"}
	directoryHeader.SetMode(os.ModeDir | 0755)
	if _, err := archive.CreateHeader(directoryHeader); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= MaximumBundleFiles; index++ {
		entry, createErr := archive.Create("assets/" + strings.Repeat("a", index/26) + string(rune('a'+index%26)) + ".png")
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := entry.Write(nil); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(tooMany); err == nil {
		t.Fatal("archive with too many entries accepted")
	}
	if _, _, err := sanitizeSVG([]byte(`<svg width="1" height="1"><`)); err == nil {
		t.Fatal("malformed SVG accepted")
	}
	if luminance, err := relativeLuminance("#000000"); err != nil || luminance != 0 {
		t.Fatalf("black luminance = %f, %v", luminance, err)
	}
}
