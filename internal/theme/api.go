// Package theme implements EndlessFS's closed, data-only Theme API.
package theme

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/state"
)

const (
	APIMajor = 1
	APIMinor = 0
)

type TokenKind string

const (
	TokenColor     TokenKind = "color"
	TokenDimension TokenKind = "dimension"
	TokenNumber    TokenKind = "number"
	TokenInteger   TokenKind = "integer"
	TokenEnum      TokenKind = "enum"
	TokenShadow    TokenKind = "shadow"
)

type Shadow struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Blur   float64 `json:"blur"`
	Spread float64 `json:"spread"`
	Color  string  `json:"color"`
}

type TokenValue struct {
	Kind    TokenKind `json:"kind"`
	Color   string    `json:"color,omitempty"`
	Number  float64   `json:"number,omitempty"`
	Integer int64     `json:"integer,omitempty"`
	Enum    string    `json:"enum,omitempty"`
	Shadow  *Shadow   `json:"shadow,omitempty"`
}

type TokenSpec struct {
	ID           string    `json:"id"`
	CSSProperty  string    `json:"cssProperty"`
	Kind         TokenKind `json:"kind"`
	Unit         string    `json:"unit,omitempty"`
	Minimum      float64   `json:"minimum,omitempty"`
	Maximum      float64   `json:"maximum,omitempty"`
	Enums        []string  `json:"enums,omitempty"`
	LightDefault any       `json:"lightDefault"`
	DarkDefault  any       `json:"darkDefault"`
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

type FontSlot struct {
	ID      string   `json:"id"`
	Weights []string `json:"weights"`
	Styles  []string `json:"styles"`
}

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func token(id string, kind TokenKind, unit string, minimum, maximum float64, light, dark any, enums ...string) TokenSpec {
	return TokenSpec{ID: id, CSSProperty: "--efs-" + strings.ReplaceAll(id, ".", "-"), Kind: kind, Unit: unit, Minimum: minimum, Maximum: maximum, LightDefault: light, DarkDefault: dark, Enums: enums}
}

var tokenSpecs = []TokenSpec{
	token("color.canvas", TokenColor, "", 0, 0, "#f7f8fa", "#101318"), token("color.surface", TokenColor, "", 0, 0, "#ffffff", "#181d24"), token("color.surface.elevated", TokenColor, "", 0, 0, "#ffffff", "#222936"),
	token("color.text.primary", TokenColor, "", 0, 0, "#172033", "#f4f7fb"), token("color.text.muted", TokenColor, "", 0, 0, "#556176", "#b8c2d1"), token("color.text.inverse", TokenColor, "", 0, 0, "#ffffff", "#101318"),
	token("color.border", TokenColor, "", 0, 0, "#c7cfdb", "#465164"), token("color.accent", TokenColor, "", 0, 0, "#2759c7", "#8db4ff"), token("color.success", TokenColor, "", 0, 0, "#176b3a", "#70d99b"), token("color.warning", TokenColor, "", 0, 0, "#7a4b00", "#f2bd55"), token("color.danger", TokenColor, "", 0, 0, "#b42318", "#ff9b91"),
	token("color.selection.background", TokenColor, "", 0, 0, "#2759c7", "#8db4ff"), token("color.selection.text", TokenColor, "", 0, 0, "#ffffff", "#101318"), token("color.overlay", TokenColor, "", 0, 0, "#000000", "#000000"),
	token("color.interaction.hover", TokenColor, "", 0, 0, "#e7ecf5", "#293344"), token("color.interaction.active", TokenColor, "", 0, 0, "#d8e1f0", "#354258"), token("color.interaction.selected", TokenColor, "", 0, 0, "#dce8ff", "#273d63"), token("color.interaction.disabled", TokenColor, "", 0, 0, "#7b8799", "#8792a3"), token("color.interaction.dropTarget", TokenColor, "", 0, 0, "#2759c7", "#8db4ff"), token("color.interaction.focusRing", TokenColor, "", 0, 0, "#174ea6", "#b9d1ff"), token("color.validation.error", TokenColor, "", 0, 0, "#b42318", "#ff9b91"),
	token("color.file.uploading", TokenColor, "", 0, 0, "#2759c7", "#8db4ff"), token("color.file.complete", TokenColor, "", 0, 0, "#176b3a", "#70d99b"), token("color.file.failed", TokenColor, "", 0, 0, "#b42318", "#ff9b91"), token("color.file.shared", TokenColor, "", 0, 0, "#6542b5", "#c3a8ff"), token("color.file.offline", TokenColor, "", 0, 0, "#556176", "#b8c2d1"), token("color.file.trashed", TokenColor, "", 0, 0, "#7a4b00", "#f2bd55"),
	token("type.size.xs", TokenDimension, "px", 10, 18, 12.0, 12.0), token("type.size.sm", TokenDimension, "px", 11, 20, 14.0, 14.0), token("type.size.md", TokenDimension, "px", 12, 24, 16.0, 16.0), token("type.size.lg", TokenDimension, "px", 14, 32, 20.0, 20.0), token("type.size.xl", TokenDimension, "px", 18, 48, 28.0, 28.0),
	token("type.lineHeight.tight", TokenNumber, "", 1, 2, 1.2, 1.2), token("type.lineHeight.normal", TokenNumber, "", 1, 2, 1.5, 1.5), token("type.lineHeight.relaxed", TokenNumber, "", 1, 2.5, 1.75, 1.75), token("type.letterSpacing", TokenDimension, "em", -0.05, 0.2, 0.0, 0.0),
	token("type.weight.regular", TokenInteger, "", 300, 500, 400, 400), token("type.weight.medium", TokenInteger, "", 400, 700, 550, 550), token("type.weight.bold", TokenInteger, "", 600, 900, 700, 700), token("font.interface", TokenEnum, "", 0, 0, "system", "system", "system", "interface"), token("font.monospace", TokenEnum, "", 0, 0, "system-monospace", "system-monospace", "system-monospace", "monospace"),
	token("radius.control", TokenDimension, "px", 0, 32, 8.0, 8.0), token("radius.field", TokenDimension, "px", 0, 32, 8.0, 8.0), token("radius.panel", TokenDimension, "px", 0, 48, 14.0, 14.0), token("radius.dialog", TokenDimension, "px", 0, 48, 16.0, 16.0), token("radius.menu", TokenDimension, "px", 0, 32, 10.0, 10.0), token("radius.badge", TokenDimension, "px", 0, 999, 999.0, 999.0), token("radius.thumbnail", TokenDimension, "px", 0, 32, 8.0, 8.0), token("radius.avatar", TokenDimension, "px", 0, 999, 999.0, 999.0),
	token("spacing.density", TokenEnum, "", 0, 0, "comfortable", "comfortable", "compact", "comfortable"), token("spacing.pageGutter", TokenDimension, "px", 8, 64, 24.0, 24.0), token("spacing.controlPadding", TokenDimension, "px", 4, 32, 12.0, 12.0), token("spacing.componentGap", TokenDimension, "px", 2, 32, 8.0, 8.0), token("spacing.sectionGap", TokenDimension, "px", 8, 64, 24.0, 24.0),
	token("metric.toolbarHeight", TokenDimension, "px", 44, 96, 56.0, 56.0), token("metric.sidebarWidth", TokenDimension, "px", 180, 400, 256.0, 256.0), token("metric.rowHeight", TokenDimension, "px", 36, 72, 48.0, 48.0), token("metric.controlHeight", TokenDimension, "px", 36, 64, 40.0, 40.0), token("metric.thumbnailSize", TokenDimension, "px", 32, 256, 96.0, 96.0), token("metric.iconScale", TokenNumber, "", 0.75, 1.5, 1.0, 1.0), token("metric.targetMinimum", TokenDimension, "px", 24, 64, 44.0, 44.0),
	token("elevation.low", TokenShadow, "", 0, 0, Shadow{X: 0, Y: 1, Blur: 3, Spread: 0, Color: "#000000"}, Shadow{X: 0, Y: 1, Blur: 3, Spread: 0, Color: "#000000"}), token("elevation.medium", TokenShadow, "", 0, 0, Shadow{X: 0, Y: 4, Blur: 12, Spread: 0, Color: "#000000"}, Shadow{X: 0, Y: 4, Blur: 12, Spread: 0, Color: "#000000"}), token("elevation.high", TokenShadow, "", 0, 0, Shadow{X: 0, Y: 12, Blur: 32, Spread: 0, Color: "#000000"}, Shadow{X: 0, Y: 12, Blur: 32, Spread: 0, Color: "#000000"}), token("opacity.overlay", TokenNumber, "", 0, 1, 0.45, 0.65),
	token("motion.duration.fast", TokenInteger, "ms", 0, 1000, 80, 80), token("motion.duration.normal", TokenInteger, "ms", 0, 2000, 160, 160), token("motion.duration.slow", TokenInteger, "ms", 0, 3000, 260, 260), token("motion.easing", TokenEnum, "", 0, 0, "standard", "standard", "linear", "standard", "emphasized"),
	token("brand.logoWidth", TokenDimension, "px", 32, 320, 160.0, 160.0), token("brand.logoHeight", TokenDimension, "px", 16, 160, 48.0, 48.0), token("brand.markSize", TokenDimension, "px", 16, 128, 40.0, 40.0), token("brand.loginIllustrationWidth", TokenDimension, "px", 120, 800, 420.0, 420.0), token("brand.loginIllustrationHeight", TokenDimension, "px", 120, 800, 320.0, 320.0), token("brand.imageFit", TokenEnum, "", 0, 0, "contain", "contain", "contain", "cover", "scale-down"),
}

var contrastPairs = []ContrastPair{
	{Foreground: "color.text.primary", Background: "color.canvas", Minimum: 4.5},
	{Foreground: "color.text.muted", Background: "color.canvas", Minimum: 4.5},
	{Foreground: "color.text.inverse", Background: "color.accent", Minimum: 4.5},
	{Foreground: "color.selection.text", Background: "color.selection.background", Minimum: 4.5},
	{Foreground: "color.interaction.focusRing", Background: "color.canvas", Minimum: 3},
	{Foreground: "color.validation.error", Background: "color.canvas", Minimum: 4.5},
}

var mediaSlotIDs = []string{
	"brand.logo", "brand.mark", "brand.favicon",
	"icon.file", "icon.folder", "icon.upload", "icon.download", "icon.copy", "icon.move", "icon.share", "icon.trash", "icon.restore", "icon.settings", "icon.passkey", "icon.warning", "icon.error",
	"illustration.emptyDrive", "illustration.emptyFolder", "illustration.emptyTrash", "illustration.uploadFailed",
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

func FontRegistry() []FontSlot {
	return []FontSlot{{ID: "interface", Weights: []string{"regular", "bold"}, Styles: []string{"normal"}}, {ID: "monospace", Weights: []string{"regular", "bold"}, Styles: []string{"normal"}}}
}

func tokenSpecMap() map[string]TokenSpec {
	result := make(map[string]TokenSpec, len(tokenSpecs))
	for _, spec := range tokenSpecs {
		result[spec.ID] = spec
	}
	return result
}

func parseTokenValue(spec TokenSpec, raw json.RawMessage) (TokenValue, error) {
	value := TokenValue{Kind: spec.Kind}
	switch spec.Kind {
	case TokenColor:
		if err := state.DecodeJSON(raw, &value.Color); err != nil || !colorPattern.MatchString(value.Color) {
			return TokenValue{}, domain.NewError(domain.ErrorInvalid, "theme color must use #RRGGBB")
		}
		value.Color = strings.ToLower(value.Color)
	case TokenDimension, TokenNumber:
		if err := decodeNumber(raw, &value.Number); err != nil || value.Number < spec.Minimum || value.Number > spec.Maximum {
			return TokenValue{}, domain.NewError(domain.ErrorInvalid, "theme numeric token is outside its range")
		}
	case TokenInteger:
		if err := state.DecodeJSON(raw, &value.Integer); err != nil || float64(value.Integer) < spec.Minimum || float64(value.Integer) > spec.Maximum {
			return TokenValue{}, domain.NewError(domain.ErrorInvalid, "theme integer token is outside its range")
		}
	case TokenEnum:
		if err := state.DecodeJSON(raw, &value.Enum); err != nil || !contains(spec.Enums, value.Enum) {
			return TokenValue{}, domain.NewError(domain.ErrorInvalid, "theme enum token is not allowlisted")
		}
	case TokenShadow:
		var shadow Shadow
		if err := state.DecodeJSON(raw, &shadow); err != nil || !colorPattern.MatchString(shadow.Color) || shadow.X < -64 || shadow.X > 64 || shadow.Y < -64 || shadow.Y > 64 || shadow.Blur < 0 || shadow.Blur > 128 || shadow.Spread < -32 || shadow.Spread > 64 {
			return TokenValue{}, domain.NewError(domain.ErrorInvalid, "invalid structured theme shadow")
		}
		shadow.Color = strings.ToLower(shadow.Color)
		value.Shadow = &shadow
	default:
		return TokenValue{}, domain.NewError(domain.ErrorInternal, "unknown Theme API token kind")
	}
	return value, nil
}

func decodeNumber(raw json.RawMessage, value *float64) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return domain.NewError(domain.ErrorInvalid, "invalid finite number")
	}
	return nil
}

func defaultTokenValue(spec TokenSpec, appearance Appearance) (TokenValue, error) {
	value := spec.LightDefault
	if appearance == AppearanceDark {
		value = spec.DarkDefault
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return TokenValue{}, err
	}
	return parseTokenValue(spec, raw)
}

func (v TokenValue) CSS(spec TokenSpec) string {
	switch v.Kind {
	case TokenColor:
		return v.Color
	case TokenDimension, TokenNumber:
		return formatFloat(v.Number) + spec.Unit
	case TokenInteger:
		return strconv.FormatInt(v.Integer, 10) + spec.Unit
	case TokenEnum:
		switch v.Enum {
		case "system":
			return "system-ui, sans-serif"
		case "system-monospace":
			return "ui-monospace, monospace"
		case "interface":
			return "EFSInterface, system-ui, sans-serif"
		case "monospace":
			return "EFSMonospace, ui-monospace, monospace"
		case "standard":
			return "cubic-bezier(0.2, 0, 0, 1)"
		case "emphasized":
			return "cubic-bezier(0.2, 0, 0, 1.2)"
		default:
			return v.Enum
		}
	case TokenShadow:
		return fmt.Sprintf("%spx %spx %spx %spx %s", formatFloat(v.Shadow.X), formatFloat(v.Shadow.Y), formatFloat(v.Shadow.Blur), formatFloat(v.Shadow.Spread), v.Shadow.Color)
	default:
		return ""
	}
}

func formatFloat(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
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
