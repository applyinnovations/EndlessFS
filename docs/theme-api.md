# EndlessFS Theme API 2.0

Theme API 2.0 is the closed, data-only appearance contract derived from the rebuilt browser UI. It is a new major contract. Theme API 1.x bundles are unsupported and are never upgraded, aliased, translated, or partially loaded.

The canonical registry is emitted by:

```console
nix develop --command go run ./tools/theme api
```

`GET /api/v1/themes` exposes the same non-secret registry. The Go implementation in `internal/theme/api.go` is authoritative.

## Bundle boundary

A bundle is a deterministic `.efstheme` ZIP or an equivalent directory containing `theme.json` and referenced media beneath `assets/`. A custom theme directly extends `endlessfs-light` or `endlessfs-dark` and declares the same appearance.

The only accepted manifest input objects are `tokens` and `assets`. Unknown fields, tokens, asset slots, files, and references fail validation. Theme input cannot contain fonts, CSS, HTML, JavaScript, WebAssembly, templates, Markdown, remote/data URLs, selectors, properties, wording, or behavior.

```json
{
  "schemaVersion": 2,
  "themeAPI": { "major": 2, "minor": 0 },
  "id": "com.example.endlessfs",
  "name": "Example",
  "version": "2.0.0",
  "extends": "endlessfs-light",
  "appearance": "light",
  "license": "CC-BY-4.0",
  "tokens": {
    "color.primary": "#315bd6",
    "color.primary.tint": "#eef3ff"
  },
  "assets": {
    "brand.mark": "assets/mark.svg",
    "brand.favicon": "assets/mark.svg"
  }
}
```

## Purpose-based color contract

The complete 2.0 token registry is:

- `color.background`
- `color.foreground`
- `color.text.muted`
- `color.border`
- `color.surface`
- `color.primary`
- `color.primary.tint`
- `color.success`
- `color.warning`
- `color.error`

Names describe purpose, never hue. Values use exactly `#RRGGBB`, normalize to lowercase, and compile one-to-one to `--efs-` plus the token ID with dots changed to hyphens. Raw manifest strings are never concatenated into CSS. The compiler validates foreground, muted text, primary interaction, and error contrast against the background.

Typography, geometry, density, motion, hit targets, and layout are application-owned invariants. Inter 4.0 Regular 400, Medium 500, and Semibold 600 are pinned, licensed, and embedded by the browser asset manifest; themes cannot replace them or alter layout metrics.

## Semantic media

The closed media registry contains the brand logo, mark, and favicon; folder and generic file icons; and image, video, PDF, audio, document, archive, and unknown file-type icons. Accepted inputs are sanitized SVG or signature-validated PNG, WebP, and AVIF within the emitted per-slot byte, dimension, and pixel bounds. Structured raster sprite rectangles must remain inside the decoded image and use a pixel ratio from 1 through 4.

SVG validation rejects declarations, scripts, event attributes, style attributes, external/data references, `foreignObject`, text/HTML, animation, and unsupported elements. SVG is served as an external image or mask and is never inserted into the DOM.

## Versioning, inheritance, and recovery

Version 2.0 requires `schemaVersion: 2` and `themeAPI: {"major":2,"minor":0}` exactly. There is no compatibility path from any other schema or API version. A future contract change requires an explicit new specification and major-version decision; silent reinterpretation is forbidden.

A valid 2.0 custom theme inherits omitted tokens and media from its immutable 2.0 parent. An explicit missing or invalid reference fails compilation. Invalid bundles never enter the runtime registry. An unavailable selection resolves to an immutable built-in, and `?safe-theme=1` always selects the built-in light theme without changing stored state.
