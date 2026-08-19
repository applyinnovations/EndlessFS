package theme

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func customManifest() Manifest {
	return Manifest{SchemaVersion: 2, ThemeAPI: APIVersion{Major: 2, Minor: 0}, ID: "com.example.theme", Name: "Example", Version: "2.0.0", Extends: "endlessfs-light", Appearance: AppearanceLight, Author: "Example", License: "CC-BY-4.0", Tokens: map[string]json.RawMessage{}, Assets: map[string]AssetReference{}}
}

func TestThemeAPITwoIsANewPurposeOnlyContract(t *testing.T) {
	if APIMajor != 2 || APIMinor != 0 {
		t.Fatalf("Theme API = %d.%d, want 2.0", APIMajor, APIMinor)
	}
	want := []string{
		"color.background", "color.border", "color.error", "color.foreground", "color.primary",
		"color.primary.tint", "color.success", "color.surface", "color.text.muted", "color.warning",
	}
	got := make([]string, 0, len(TokenRegistry()))
	for _, spec := range TokenRegistry() {
		got = append(got, spec.ID)
		for _, appearanceName := range []string{"blue", "red", "green", "yellow", "accent", "danger", "canvas"} {
			if strings.Contains(strings.ToLower(spec.ID), appearanceName) {
				t.Errorf("new Theme API contains appearance or legacy token %q", spec.ID)
			}
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("Theme API tokens = %v, want %v", got, want)
	}
	fontBearingManifest := `{"schemaVersion":2,"themeAPI":{"major":2,"minor":0},"id":"com.example.fonts","name":"Fonts","version":"2.0.0","extends":"endlessfs-light","appearance":"light","license":"CC-BY-4.0","tokens":{},"fonts":{},"assets":{}}`
	if _, err := DecodeManifest([]byte(fontBearingManifest)); err == nil {
		t.Fatal("new Theme API accepted a font override surface")
	}

	legacy := customManifest()
	legacy.SchemaVersion = 1
	legacy.ThemeAPI = APIVersion{Major: 1, Minor: 1}
	if _, err := NewCompiler(map[string]*ResolvedTheme{}).Compile(Bundle{Manifest: legacy, Files: map[string][]byte{}}); err == nil {
		t.Fatal("Theme API 1.x bundle was accepted")
	}
}

func TestBuiltinsAndMinimalCustomUseOrdinaryCompletePipeline(t *testing.T) {
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	if len(builtins) != 2 {
		t.Fatalf("built-ins = %d", len(builtins))
	}
	for _, id := range []string{"endlessfs-light", "endlessfs-dark"} {
		theme := builtins[id]
		if theme == nil || len(theme.Tokens) != len(TokenRegistry()) || len(theme.Assets) != len(MediaRegistry()) || theme.Digest == "" || !strings.HasPrefix(theme.CSS, ":root{") {
			t.Fatalf("incomplete built-in %q: %+v", id, theme)
		}
	}
	manifest := customManifest()
	manifest.Tokens["color.primary"] = json.RawMessage(`"#315bd6"`)
	resolved, err := NewCompiler(builtins).Compile(Bundle{Manifest: manifest, Files: map[string][]byte{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Tokens) != len(builtins["endlessfs-light"].Tokens) || resolved.Tokens["color.primary"].Color != "#315bd6" || resolved.Tokens["color.surface"] != builtins["endlessfs-light"].Tokens["color.surface"] || len(resolved.Assets) != len(MediaRegistry()) {
		t.Fatalf("custom inheritance is incomplete")
	}
	registry, err := NewRegistry(Bundle{Manifest: manifest, Files: map[string][]byte{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, fallback := registry.Resolve("missing.theme", false, false, "endlessfs-light", "endlessfs-dark"); !fallback {
		t.Fatal("missing selection did not fall back")
	}
	if safe, _ := registry.Resolve(resolved.ID, true, true, "endlessfs-light", "endlessfs-dark"); safe.ID != "endlessfs-light" {
		t.Fatalf("safe theme = %q", safe.ID)
	}
}

func TestCurrentCustomThemeInheritsItsCompleteParentMedia(t *testing.T) {
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	parentCopy := *builtins["endlessfs-light"]
	parentCopy.Assets = make(map[string]ResolvedAsset, len(builtins["endlessfs-light"].Assets)+1)
	for id, asset := range builtins["endlessfs-light"].Assets {
		parentCopy.Assets[id] = asset
	}
	parentCopy.Assets["icon.futureFeature"] = parentCopy.Assets["icon.file"]
	parents := map[string]*ResolvedTheme{"endlessfs-light": &parentCopy, "endlessfs-dark": builtins["endlessfs-dark"]}
	resolved, err := NewCompiler(parents).Compile(Bundle{Manifest: customManifest(), Files: map[string][]byte{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.Assets["icon.futureFeature"]; !ok {
		t.Fatal("current custom theme did not inherit its complete parent media")
	}
}

func TestMediaPreviewFallbackSlotsAreCompleteAndVersioned(t *testing.T) {
	if APIMajor != 2 || APIMinor != 0 {
		t.Fatalf("Theme API = %d.%d, want 2.0", APIMajor, APIMinor)
	}
	wanted := map[string]bool{
		"icon.file.image": false, "icon.file.video": false, "icon.file.pdf": false, "icon.file.audio": false,
		"icon.file.document": false, "icon.file.archive": false, "icon.file.unknown": false,
	}
	for _, slot := range MediaRegistry() {
		if _, exists := wanted[slot.ID]; exists {
			wanted[slot.ID] = true
		}
	}
	for id, found := range wanted {
		if !found {
			t.Errorf("Theme API is missing %q", id)
		}
	}
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	for id, theme := range builtins {
		digests := make(map[string]string, len(wanted))
		for slot := range wanted {
			asset, found := theme.Assets[slot]
			if !found {
				t.Errorf("built-in %s is missing %s", id, slot)
				continue
			}
			if previous, duplicate := digests[asset.Media.Digest]; duplicate {
				t.Errorf("built-in %s uses the same fallback glyph for %s and %s", id, previous, slot)
			}
			digests[asset.Media.Digest] = slot
		}
	}
}

func TestThemeTokensAreClosedTypedBoundedAndContrastChecked(t *testing.T) {
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	compiler := NewCompiler(builtins)
	tests := map[string]func(*Manifest){
		"unknown token":    func(m *Manifest) { m.Tokens["position.dialog"] = json.RawMessage(`"fixed"`) },
		"raw CSS color":    func(m *Manifest) { m.Tokens["color.primary"] = json.RawMessage(`"red;display:none"`) },
		"dimension string": func(m *Manifest) { m.Tokens["radius.control"] = json.RawMessage(`"12px"`) },
		"out of range":     func(m *Manifest) { m.Tokens["metric.sidebarWidth"] = json.RawMessage(`9999`) },
		"unknown easing":   func(m *Manifest) { m.Tokens["motion.easing"] = json.RawMessage(`"steps(1)"`) },
		"contrast":         func(m *Manifest) { m.Tokens["color.foreground"] = json.RawMessage(`"#ffffff"`) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := customManifest()
			mutate(&manifest)
			if _, err := compiler.Compile(Bundle{Manifest: manifest, Files: map[string][]byte{}}); err == nil {
				t.Fatal("unsafe token was accepted")
			}
		})
	}
	manifest := customManifest()
	manifest.Tokens["color.primary"] = json.RawMessage(`"#315bd6"`)
	resolved, err := compiler.Compile(Bundle{Manifest: manifest, Files: map[string][]byte{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resolved.CSS, "--efs-color-primary:#315bd6") {
		t.Fatalf("typed CSS = %q", resolved.CSS)
	}
}

func TestThemeManifestStrictMetadataAndCompatibility(t *testing.T) {
	valid := customManifest()
	data := encodeManifest(valid)
	if _, err := DecodeManifest(data); err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]string{
		"unknown":      strings.TrimSuffix(string(data), "}") + `,"css":"body{}"}`,
		"duplicate":    `{"schemaVersion":1,"schemaVersion":1}`,
		"trailing":     string(data) + `{}`,
		"invalid UTF8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeManifest([]byte(payload)); err == nil {
				t.Fatal("malformed manifest was accepted")
			}
		})
	}
	builtins, _ := Builtins()
	compiler := NewCompiler(builtins)
	for name, mutate := range map[string]func(*Manifest){
		"schema": func(m *Manifest) { m.SchemaVersion = 1 }, "API major": func(m *Manifest) { m.ThemeAPI.Major = 3 }, "API minor": func(m *Manifest) { m.ThemeAPI.Minor = 1 }, "reserved ID": func(m *Manifest) { m.ID = "endlessfs-shadow" }, "bad ID": func(m *Manifest) { m.ID = "Example" }, "bad version": func(m *Manifest) { m.Version = "latest" }, "remote license": func(m *Manifest) { m.License = "https://license.example" }, "indirect parent": func(m *Manifest) { m.Extends = "com.example.parent" }, "appearance mismatch": func(m *Manifest) { m.Appearance = AppearanceDark },
	} {
		t.Run(name, func(t *testing.T) {
			manifest := customManifest()
			mutate(&manifest)
			if _, err := compiler.Compile(Bundle{Manifest: manifest, Files: map[string][]byte{}}); err == nil {
				t.Fatal("invalid metadata was accepted")
			}
		})
	}
}

func TestSVGSanitizerRejectsActiveContentAndExternalReferences(t *testing.T) {
	safe := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" fill="#123456"/></svg>`)
	if width, height, err := sanitizeSVG(safe); err != nil || width != 64 || height != 64 {
		t.Fatalf("safe SVG = %dx%d, %v", width, height, err)
	}
	unsafe := map[string]string{
		"script":         `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><script>alert(1)</script></svg>`,
		"handler":        `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1" onload="x"><path d="M0 0"/></svg>`,
		"foreign object": `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><foreignObject/></svg>`,
		"external":       `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><path fill="url(https://evil.example/x)"/></svg>`,
		"data":           `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><path href="data:image/png,x"/></svg>`,
		"style":          `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><path style="display:none"/></svg>`,
		"doctype":        `<!DOCTYPE svg><svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>`,
		"text":           `<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><text>spoof</text></svg>`,
	}
	for name, value := range unsafe {
		t.Run(name, func(t *testing.T) {
			if _, _, err := sanitizeSVG([]byte(value)); err == nil {
				t.Fatal("active SVG was accepted")
			}
		})
	}
}

func TestMediaSignaturesDimensionsAndSprites(t *testing.T) {
	imageValue := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			imageValue.Set(x, y, color.RGBA{R: 1, G: 2, B: 3, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageValue); err != nil {
		t.Fatal(err)
	}
	slot := MediaRegistry()[0]
	asset, err := validateMedia("assets/image.png", encoded.Bytes(), slot)
	if err != nil || asset.Width != 16 || asset.Height != 12 {
		t.Fatalf("PNG = %+v, %v", asset, err)
	}
	if _, err := validateMedia("assets/image.webp", encoded.Bytes(), slot); err == nil {
		t.Fatal("extension-spoofed media was accepted")
	}
	webp := make([]byte, 30)
	copy(webp[0:4], "RIFF")
	copy(webp[8:12], "WEBP")
	copy(webp[12:16], "VP8X")
	webp[24], webp[27] = 15, 11
	if width, height, err := decodeWebP(webp); err != nil || width != 16 || height != 12 {
		t.Fatalf("WebP = %dx%d, %v", width, height, err)
	}
	avif := make([]byte, 36)
	copy(avif[4:8], "ftyp")
	copy(avif[8:12], "avif")
	copy(avif[16:20], "ispe")
	binary.BigEndian.PutUint32(avif[24:28], 16)
	binary.BigEndian.PutUint32(avif[28:32], 12)
	if width, height, err := decodeAVIF(avif); err != nil || width != 16 || height != 12 {
		t.Fatalf("AVIF = %dx%d, %v", width, height, err)
	}
	if err := validateSprite(AssetReference{Path: "assets/image.png", Sprite: true, X: 10, Y: 0, Width: 7, Height: 5, PixelRatio: 1}, asset); err == nil {
		t.Fatal("out-of-range sprite was accepted")
	}
}

func TestThemeReferenceClosureAndAssetFallback(t *testing.T) {
	builtins, _ := Builtins()
	manifest := customManifest()
	manifest.Assets["icon.file"] = AssetReference{Path: "assets/icon.svg"}
	files := map[string][]byte{"assets/icon.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32"><path d="M0 0h32v32z" fill="#123456"/></svg>`)}
	resolved, err := NewCompiler(builtins).Compile(Bundle{Manifest: manifest, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Assets["icon.file"].Fallback == nil {
		t.Fatal("custom media has no built-in fallback")
	}
	registry, err := NewRegistry(Bundle{Manifest: manifest, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	primary, fallback, ok := registry.AssetURL(resolved, "icon.file")
	if !ok || primary == fallback {
		t.Fatalf("asset URLs = %q %q", primary, fallback)
	}
	response, ok := registry.Asset(resolved.Digest, strings.TrimPrefix(primary, "/assets/themes/"+resolved.Digest+"/"))
	if !ok || response.ContentType != "image/svg+xml" || len(response.Data) == 0 {
		t.Fatalf("asset response = %+v %v", response, ok)
	}
	files["assets/unreferenced.svg"] = files["assets/icon.svg"]
	if _, err := NewCompiler(builtins).Compile(Bundle{Manifest: manifest, Files: files}); err == nil {
		t.Fatal("unreferenced file was accepted")
	}
}

func TestThemeDigestIsCanonicalAndIDsCannotCollide(t *testing.T) {
	builtins, _ := Builtins()
	manifest := customManifest()
	first, err := NewCompiler(builtins).Compile(Bundle{Manifest: manifest, Files: map[string][]byte{}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCompiler(builtins).Compile(Bundle{Manifest: manifest, Files: map[string][]byte{}})
	if err != nil || first.Digest != second.Digest {
		t.Fatalf("digests = %q %q, %v", first.Digest, second.Digest, err)
	}
	other := manifest
	other.Name = "Other"
	if _, err := NewRegistry(Bundle{Manifest: manifest, Files: map[string][]byte{}}, Bundle{Manifest: other, Files: map[string][]byte{}}); err == nil {
		t.Fatal("duplicate theme ID was accepted")
	}
}

func TestThemeZIPAndDirectoryDefenses(t *testing.T) {
	manifest := customManifest()
	manifestData := encodeManifest(manifest)
	tests := map[string]func(*zip.Writer){
		"traversal": func(writer *zip.Writer) {
			writeZIP(t, writer, "theme.json", manifestData)
			writeZIP(t, writer, "../escape.svg", []byte("x"))
		},
		"duplicate normalized": func(writer *zip.Writer) {
			writeZIP(t, writer, "theme.json", manifestData)
			writeZIP(t, writer, "assets/e\u0301.svg", []byte("x"))
			writeZIP(t, writer, "assets/é.svg", []byte("x"))
		},
		"raw CSS": func(writer *zip.Writer) {
			writeZIP(t, writer, "theme.json", manifestData)
			writeZIP(t, writer, "assets/theme.css", []byte("body{}"))
		},
		"symlink": func(writer *zip.Writer) {
			writeZIP(t, writer, "theme.json", manifestData)
			header := &zip.FileHeader{Name: "assets/link.svg", Method: zip.Store}
			header.SetMode(os.ModeSymlink | 0777)
			entry, _ := writer.CreateHeader(header)
			_, _ = entry.Write([]byte("target"))
		},
		"compression bomb": func(writer *zip.Writer) {
			writeZIP(t, writer, "theme.json", manifestData)
			writeZIP(t, writer, "assets/bomb.svg", bytes.Repeat([]byte{'0'}, 2<<20))
		},
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "unsafe.efstheme")
			file, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(file)
			build(writer)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadBundle(archive); err == nil {
				t.Fatal("unsafe archive was accepted")
			}
		})
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "theme.json"), manifestData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("theme.json", filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(directory); err == nil {
		t.Fatal("directory symlink was accepted")
	}
	hardlinkDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(hardlinkDirectory, "theme.json"), manifestData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(hardlinkDirectory, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(hardlinkDirectory, "assets", "source.svg")
	if err := os.WriteFile(source, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(hardlinkDirectory, "assets", "linked.svg")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(hardlinkDirectory); err == nil {
		t.Fatal("directory hard link was accepted")
	}
}

func writeZIP(t *testing.T, writer *zip.Writer, name string, data []byte) {
	t.Helper()
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(data); err != nil {
		t.Fatal(err)
	}
}
