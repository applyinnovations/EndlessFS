package theme

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

type Registry struct {
	themes  map[string]*ResolvedTheme
	digests map[string]*ResolvedTheme
}

var buildInputBundles []Bundle

//lint:ignore U1000 invoked only by Nix-generated custom_build_inputs.go
func mustRegisterBuildInput(manifestJSON string, files map[string]string) {
	manifest, err := DecodeManifest([]byte(manifestJSON))
	if err != nil {
		panic(err)
	}
	decoded := make(map[string][]byte, len(files))
	for name, content := range files {
		decoded[name] = []byte(content)
	}
	buildInputBundles = append(buildInputBundles, Bundle{Manifest: manifest, Files: decoded})
}

func NewRegistry(customBundles ...Bundle) (*Registry, error) {
	customBundles = append(append([]Bundle(nil), buildInputBundles...), customBundles...)
	builtins, err := Builtins()
	if err != nil {
		return nil, err
	}
	registry := &Registry{themes: make(map[string]*ResolvedTheme), digests: make(map[string]*ResolvedTheme)}
	for id, theme := range builtins {
		registry.themes[id] = theme
		registry.digests[theme.Digest] = theme
	}
	compiler := NewCompiler(builtins)
	for _, bundle := range customBundles {
		if _, exists := registry.themes[bundle.Manifest.ID]; exists {
			return nil, fmt.Errorf("theme ID %q shadows an installed theme", bundle.Manifest.ID)
		}
		compiled, err := compiler.Compile(bundle)
		if err != nil {
			return nil, err
		}
		if _, exists := registry.digests[compiled.Digest]; exists {
			return nil, fmt.Errorf("theme content digest collision")
		}
		registry.themes[compiled.ID] = compiled
		registry.digests[compiled.Digest] = compiled
	}
	return registry, nil
}

func LoadRegistry(paths ...string) (*Registry, error) {
	bundles := make([]Bundle, 0, len(paths))
	for _, name := range paths {
		bundle, err := LoadBundle(name)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, bundle)
	}
	return NewRegistry(bundles...)
}

func (r *Registry) Theme(id string) (*ResolvedTheme, bool) {
	value, ok := r.themes[id]
	return value, ok
}
func (r *Registry) Installed(id string) bool { _, ok := r.themes[id]; return ok }

func (r *Registry) Metadata() []Metadata {
	values := make([]Metadata, 0, len(r.themes))
	for _, theme := range r.themes {
		values = append(values, theme.Metadata())
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	return values
}

func (r *Registry) Resolve(selected string, dark, safe bool, defaultLight, defaultDark string) (*ResolvedTheme, bool) {
	if safe {
		return r.themes["endlessfs-light"], selected != "endlessfs-light"
	}
	wanted := selected
	if wanted == "" || wanted == "system" {
		wanted = defaultLight
		if dark {
			wanted = defaultDark
		}
	}
	if theme, found := r.themes[wanted]; found {
		return theme, false
	}
	if strings.Contains(strings.ToLower(wanted), "dark") {
		if theme, found := r.themes["endlessfs-dark"]; found {
			return theme, true
		}
	}
	return r.themes["endlessfs-light"], true
}

type AssetResponse struct {
	ContentType string
	Data        []byte
}

func (r *Registry) Asset(digest, name string) (AssetResponse, bool) {
	theme, found := r.digests[digest]
	if !found || name == "" || path.Base(name) != name || strings.ContainsAny(name, "\\\x00") {
		return AssetResponse{}, false
	}
	if name == "theme.css" {
		return AssetResponse{ContentType: "text/css; charset=utf-8", Data: []byte(theme.CSS)}, true
	}
	for _, asset := range theme.Assets {
		if assetFilename(asset.Media) == name {
			return AssetResponse{ContentType: asset.Media.ContentType, Data: append([]byte(nil), asset.Media.Data...)}, true
		}
	}
	for _, font := range theme.Fonts {
		for _, asset := range []*ValidatedAsset{font.Regular, font.Bold} {
			if asset != nil && assetFilename(*asset) == name {
				return AssetResponse{ContentType: "font/woff2", Data: append([]byte(nil), asset.Data...)}, true
			}
		}
	}
	return AssetResponse{}, false
}

func assetFilename(asset ValidatedAsset) string {
	extension := map[string]string{"image/svg+xml": ".svg", "image/png": ".png", "image/webp": ".webp", "image/avif": ".avif", "font/woff2": ".woff2"}[asset.ContentType]
	return asset.Digest + extension
}

func (r *Registry) AssetURL(theme *ResolvedTheme, slot string) (string, string, bool) {
	asset, found := theme.Assets[slot]
	if !found {
		return "", "", false
	}
	primary := "/assets/themes/" + theme.Digest + "/" + assetFilename(asset.Media)
	fallback := primary
	if theme.Extends != "" && asset.Fallback != nil {
		if parent, found := r.themes[theme.Extends]; found {
			fallback = "/assets/themes/" + parent.Digest + "/" + assetFilename(*asset.Fallback)
		}
	}
	return primary, fallback, true
}
