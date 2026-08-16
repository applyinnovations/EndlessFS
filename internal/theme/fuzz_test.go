package theme

import (
	"encoding/json"
	"testing"
)

func FuzzThemeBoundaries(f *testing.F) {
	for _, seed := range []string{"theme.json", "../escape", "assets/icon.svg", "assets/%2e%2e/x", "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"1\" height=\"1\"/>", "<svg onload=\"x\">", "#123456", "red;display:none", `{"schemaVersion":1}`, `{"x":0,"y":0,"width":1,"height":1,"pixelRatio":1}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = normalizeBundlePath(input)
		_, _, _ = sanitizeSVG([]byte(input))
		_, _ = DecodeManifest([]byte(input))
		spec := tokenSpecs[0]
		raw, _ := json.Marshal(input)
		_, _ = parseTokenValue(spec, raw)
		_ = validateSprite(AssetReference{Path: "assets/x.png", Sprite: true, X: len(input) % 20, Y: 0, Width: 1, Height: 1, PixelRatio: 1}, ValidatedAsset{ContentType: "image/png", Width: 16, Height: 16})
	})
}
