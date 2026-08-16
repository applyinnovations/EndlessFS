package theme

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

type ResolvedFont struct {
	Regular *ValidatedAsset `json:"regular,omitempty"`
	Bold    *ValidatedAsset `json:"bold,omitempty"`
}

type ResolvedAsset struct {
	Reference AssetReference  `json:"reference"`
	Media     ValidatedAsset  `json:"media"`
	Fallback  *ValidatedAsset `json:"fallback,omitempty"`
}

type ResolvedTheme struct {
	ID         string                   `json:"id"`
	Name       string                   `json:"name"`
	Version    string                   `json:"version"`
	ThemeAPI   APIVersion               `json:"themeAPI"`
	Extends    string                   `json:"extends,omitempty"`
	Appearance Appearance               `json:"appearance"`
	Author     string                   `json:"author,omitempty"`
	License    string                   `json:"license"`
	Digest     string                   `json:"digest"`
	Tokens     map[string]TokenValue    `json:"tokens"`
	Fonts      map[string]ResolvedFont  `json:"fonts"`
	Assets     map[string]ResolvedAsset `json:"assets"`
	CSS        string                   `json:"-"`
}

type Metadata struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Version    string     `json:"version"`
	ThemeAPI   APIVersion `json:"themeAPI"`
	Extends    string     `json:"extends,omitempty"`
	Appearance Appearance `json:"appearance"`
	Author     string     `json:"author,omitempty"`
	License    string     `json:"license"`
	Digest     string     `json:"digest"`
}

type Compiler struct{ parents map[string]*ResolvedTheme }

func NewCompiler(parents map[string]*ResolvedTheme) *Compiler {
	copyParents := make(map[string]*ResolvedTheme, len(parents))
	for id, theme := range parents {
		copyParents[id] = theme
	}
	return &Compiler{parents: copyParents}
}

func (c *Compiler) Compile(bundle Bundle) (*ResolvedTheme, error) {
	if err := validateManifestMetadata(bundle.Manifest, false); err != nil {
		return nil, err
	}
	parent, found := c.parents[bundle.Manifest.Extends]
	if !found {
		return nil, fmt.Errorf("theme parent %q is unavailable", bundle.Manifest.Extends)
	}
	if parent.Appearance != bundle.Manifest.Appearance {
		return nil, fmt.Errorf("theme appearance does not match its built-in parent")
	}
	return compileBundle(bundle, parent, false)
}

func compileBundle(bundle Bundle, parent *ResolvedTheme, builtIn bool) (*ResolvedTheme, error) {
	manifest := bundle.Manifest
	if err := validateManifestMetadata(manifest, builtIn); err != nil {
		return nil, err
	}
	if manifest.Tokens == nil || manifest.Fonts == nil || manifest.Assets == nil {
		return nil, fmt.Errorf("theme tokens, fonts, and assets objects are required")
	}
	result := &ResolvedTheme{ID: manifest.ID, Name: manifest.Name, Version: manifest.Version, ThemeAPI: manifest.ThemeAPI, Extends: manifest.Extends, Appearance: manifest.Appearance, Author: manifest.Author, License: manifest.License, Tokens: make(map[string]TokenValue), Fonts: make(map[string]ResolvedFont), Assets: make(map[string]ResolvedAsset)}
	specs := tokenSpecMap()
	if parent == nil {
		for _, spec := range tokenSpecs {
			value, err := defaultTokenValue(spec, manifest.Appearance)
			if err != nil {
				return nil, err
			}
			result.Tokens[spec.ID] = value
		}
	} else {
		for id, value := range parent.Tokens {
			result.Tokens[id] = value
		}
		for id, value := range parent.Fonts {
			result.Fonts[id] = value
		}
		for id, value := range parent.Assets {
			inherited := value
			inherited.Fallback = nil
			result.Assets[id] = inherited
		}
	}
	for id, raw := range manifest.Tokens {
		spec, found := specs[id]
		if !found {
			return nil, fmt.Errorf("unknown Theme API token %q", id)
		}
		value, err := parseTokenValue(spec, raw)
		if err != nil {
			return nil, fmt.Errorf("token %q: %w", id, err)
		}
		result.Tokens[id] = value
	}
	referenced := make(map[string]bool)
	fontSlots := make(map[string]bool)
	for _, slot := range FontRegistry() {
		fontSlots[slot.ID] = true
	}
	for slot, declaration := range manifest.Fonts {
		if !fontSlots[slot] {
			return nil, fmt.Errorf("unknown font slot %q", slot)
		}
		resolved := result.Fonts[slot]
		for weight, name := range map[string]string{"regular": declaration.Regular, "bold": declaration.Bold} {
			if name == "" {
				continue
			}
			normalized, err := normalizeBundlePath(name)
			if err != nil || !strings.HasPrefix(normalized, "assets/") {
				return nil, fmt.Errorf("font slot %q has an invalid path", slot)
			}
			data, found := bundle.Files[normalized]
			if !found || validateWOFF2(normalized, data) != nil {
				return nil, fmt.Errorf("font slot %q references an invalid WOFF2 file", slot)
			}
			digest := sha256.Sum256(data)
			media := &ValidatedAsset{Path: normalized, Digest: base64.RawURLEncoding.EncodeToString(digest[:]), ContentType: "font/woff2", Data: append([]byte(nil), data...)}
			if weight == "regular" {
				resolved.Regular = media
			} else {
				resolved.Bold = media
			}
			referenced[normalized] = true
		}
		result.Fonts[slot] = resolved
	}
	mediaSlots := make(map[string]MediaSlot)
	for _, slot := range MediaRegistry() {
		mediaSlots[slot.ID] = slot
	}
	for slotID, reference := range manifest.Assets {
		slot, found := mediaSlots[slotID]
		if !found {
			return nil, fmt.Errorf("unknown semantic media slot %q", slotID)
		}
		normalized, err := normalizeBundlePath(reference.Path)
		if err != nil || !strings.HasPrefix(normalized, "assets/") {
			return nil, fmt.Errorf("media slot %q has an invalid path", slotID)
		}
		data, found := bundle.Files[normalized]
		if !found {
			return nil, fmt.Errorf("media slot %q references a missing file", slotID)
		}
		media, err := validateMedia(normalized, data, slot)
		if err != nil {
			return nil, err
		}
		reference.Path = normalized
		if err := validateSprite(reference, media); err != nil {
			return nil, fmt.Errorf("media slot %q: %w", slotID, err)
		}
		resolved := ResolvedAsset{Reference: reference, Media: media}
		if parent != nil {
			if inherited, exists := parent.Assets[slotID]; exists {
				fallback := inherited.Media
				resolved.Fallback = &fallback
			}
		}
		result.Assets[slotID] = resolved
		referenced[normalized] = true
	}
	for name := range bundle.Files {
		if !referenced[name] {
			return nil, fmt.Errorf("unreferenced theme file %q", name)
		}
	}
	if len(result.Tokens) != len(tokenSpecs) {
		return nil, fmt.Errorf("resolved theme is missing Theme API tokens")
	}
	if builtIn && len(result.Assets) != len(mediaSlots) {
		return nil, fmt.Errorf("built-in theme is missing semantic media slots")
	}
	if parent != nil && len(result.Assets) != len(parent.Assets) {
		return nil, fmt.Errorf("resolved custom theme is missing inherited media slots")
	}
	if err := validateContrast(result.Tokens); err != nil {
		return nil, err
	}
	digestInput := struct {
		Metadata Metadata                 `json:"metadata"`
		Tokens   map[string]TokenValue    `json:"tokens"`
		Fonts    map[string]ResolvedFont  `json:"fonts"`
		Assets   map[string]ResolvedAsset `json:"assets"`
	}{Metadata: result.Metadata(), Tokens: result.Tokens, Fonts: result.Fonts, Assets: result.Assets}
	digestInput.Metadata.Digest = ""
	canonical, err := json.Marshal(digestInput)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	result.Digest = base64.RawURLEncoding.EncodeToString(digest[:])
	result.CSS = CSSVariables(result.Tokens) + fontCSS(result)
	return result, nil
}

func (t *ResolvedTheme) Metadata() Metadata {
	return Metadata{ID: t.ID, Name: t.Name, Version: t.Version, ThemeAPI: t.ThemeAPI, Extends: t.Extends, Appearance: t.Appearance, Author: t.Author, License: t.License, Digest: t.Digest}
}

func fontCSS(theme *ResolvedTheme) string {
	ids := make([]string, 0, len(theme.Fonts))
	for id := range theme.Fonts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var result strings.Builder
	for _, id := range ids {
		family := "EFSInterface"
		if id == "monospace" {
			family = "EFSMonospace"
		}
		font := theme.Fonts[id]
		for _, item := range []struct {
			asset  *ValidatedAsset
			weight int
		}{{font.Regular, 400}, {font.Bold, 700}} {
			if item.asset == nil {
				continue
			}
			result.WriteString("@font-face{font-family:")
			result.WriteString(family)
			result.WriteString(";src:url('/assets/themes/")
			result.WriteString(theme.Digest)
			result.WriteByte('/')
			result.WriteString(item.asset.Digest)
			result.WriteString(".woff2') format('woff2');font-style:normal;font-weight:")
			result.WriteString(fmt.Sprint(item.weight))
			result.WriteString(";font-display:swap;}")
		}
	}
	return result.String()
}

func validateContrast(tokens map[string]TokenValue) error {
	for _, pair := range contrastPairs {
		foreground, foregroundOK := tokens[pair.Foreground]
		background, backgroundOK := tokens[pair.Background]
		if !foregroundOK || !backgroundOK || contrastRatio(foreground.Color, background.Color)+0.001 < pair.Minimum {
			return fmt.Errorf("theme contrast %s on %s is below %.1f:1", pair.Foreground, pair.Background, pair.Minimum)
		}
	}
	return nil
}

func contrastRatio(left, right string) float64 {
	a, _ := relativeLuminance(left)
	b, _ := relativeLuminance(right)
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}
func relativeLuminance(color string) (float64, error) {
	if !colorPattern.MatchString(color) {
		return 0, fmt.Errorf("invalid color")
	}
	values := make([]float64, 3)
	for index := range 3 {
		parsed, _ := strconv.ParseUint(color[1+index*2:3+index*2], 16, 8)
		channel := float64(parsed) / 255
		if channel <= 0.04045 {
			values[index] = channel / 12.92
		} else {
			values[index] = math.Pow((channel+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*values[0] + 0.7152*values[1] + 0.0722*values[2], nil
}
