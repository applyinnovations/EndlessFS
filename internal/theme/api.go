// Package theme implements EndlessFS's closed, data-only Theme API.
package theme

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	APIMajor = 2
	APIMinor = 0
)

type TokenKind string

const (
	TokenColor TokenKind = "color"
)

type TokenValue struct {
	Kind  TokenKind `json:"kind"`
	Color string    `json:"color"`
}

type TokenSpec struct {
	ID           string    `json:"id"`
	CSSProperty  string    `json:"cssProperty"`
	Kind         TokenKind `json:"kind"`
	LightDefault string    `json:"lightDefault"`
	DarkDefault  string    `json:"darkDefault"`
}

type ContrastPair struct {
	Foreground string  `json:"foreground"`
	Background string  `json:"background"`
	Minimum    float64 `json:"minimum"`
}

type MediaSlot struct {
	ID               string   `json:"id"`
	Accepted         []string `json:"accepted"`
	MaximumBytes     int64    `json:"maximumBytes"`
	MaximumDimension int      `json:"maximumDimension"`
	MaximumPixels    int64    `json:"maximumPixels"`
	Rendering        string   `json:"rendering"`
}

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func colorToken(id, light, dark string) TokenSpec {
	return TokenSpec{ID: id, CSSProperty: "--efs-" + strings.ReplaceAll(id, ".", "-"), Kind: TokenColor, LightDefault: light, DarkDefault: dark}
}

var tokenSpecs = []TokenSpec{
	colorToken("color.background", "#ffffff", "#0f0f0f"),
	colorToken("color.foreground", "#111111", "#f5f5f5"),
	colorToken("color.text.muted", "#6b6b6b", "#a8a8a8"),
	colorToken("color.border", "#e8e8e8", "#303030"),
	colorToken("color.surface", "#f6f6f6", "#181818"),
	colorToken("color.primary", "#2563eb", "#8bb0ff"),
	colorToken("color.primary.tint", "#eff6ff", "#172442"),
	colorToken("color.success", "#16803a", "#66d58a"),
	colorToken("color.warning", "#d97706", "#f5ba5c"),
	colorToken("color.error", "#d92d20", "#ff9189"),
}

var contrastPairs = []ContrastPair{
	{Foreground: "color.foreground", Background: "color.background", Minimum: 4.5},
	{Foreground: "color.text.muted", Background: "color.background", Minimum: 4.5},
	{Foreground: "color.primary", Background: "color.background", Minimum: 3},
	{Foreground: "color.error", Background: "color.background", Minimum: 3},
}

var mediaSlotIDs = []string{
	"brand.logo", "brand.mark", "brand.favicon",
	"icon.file", "icon.folder",
	"icon.file.image", "icon.file.video", "icon.file.pdf", "icon.file.audio", "icon.file.document", "icon.file.archive", "icon.file.unknown",
}

func TokenRegistry() []TokenSpec       { return append([]TokenSpec(nil), tokenSpecs...) }
func ContrastRegistry() []ContrastPair { return append([]ContrastPair(nil), contrastPairs...) }

func MediaRegistry() []MediaSlot {
	result := make([]MediaSlot, 0, len(mediaSlotIDs))
	for _, id := range mediaSlotIDs {
		rendering := "image"
		maximumDimension := 2048
		if strings.HasPrefix(id, "icon.") {
			rendering, maximumDimension = "mask", 512
		}
		if id == "brand.favicon" {
			rendering, maximumDimension = "favicon", 512
		}
		result = append(result, MediaSlot{ID: id, Accepted: []string{"image/svg+xml", "image/png", "image/webp", "image/avif"}, MaximumBytes: 10 << 20, MaximumDimension: maximumDimension, MaximumPixels: 16_000_000, Rendering: rendering})
	}
	return result
}

func tokenSpecMap() map[string]TokenSpec {
	result := make(map[string]TokenSpec, len(tokenSpecs))
	for _, spec := range tokenSpecs {
		result[spec.ID] = spec
	}
	return result
}

func parseTokenValue(spec TokenSpec, raw json.RawMessage) (TokenValue, error) {
	if spec.Kind != TokenColor {
		return TokenValue{}, domain.NewError(domain.ErrorInternal, "unknown Theme API token kind")
	}
	value := TokenValue{Kind: TokenColor}
	if err := state.DecodeJSON(raw, &value.Color); err != nil || !colorPattern.MatchString(value.Color) {
		return TokenValue{}, domain.NewError(domain.ErrorInvalid, "theme color must use #RRGGBB")
	}
	value.Color = strings.ToLower(value.Color)
	return value, nil
}

func defaultTokenValue(spec TokenSpec, appearance Appearance) (TokenValue, error) {
	value := spec.LightDefault
	if appearance == AppearanceDark {
		value = spec.DarkDefault
	}
	return TokenValue{Kind: TokenColor, Color: value}, nil
}

func (v TokenValue) CSS(TokenSpec) string { return v.Color }
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func CSSVariables(tokens map[string]TokenValue) string {
	ids := make([]string, 0, len(tokens))
	for id := range tokens {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	specs := tokenSpecMap()
	var result strings.Builder
	result.WriteString(":root{")
	for _, id := range ids {
		result.WriteString(specs[id].CSSProperty)
		result.WriteByte(':')
		result.WriteString(tokens[id].CSS(specs[id]))
		result.WriteByte(';')
	}
	result.WriteString("}")
	return result.String()
}
