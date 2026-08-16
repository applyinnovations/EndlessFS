# EndlessFS Theme API 1.0

The Theme API is a closed data contract. A bundle may override only identifiers emitted by:

```console
nix develop --command go run ./tools/theme api
```

That command is generated directly from the Go registry used by validation and returns every token’s type, unit, numeric bounds, enum set, light/dark default, contrast pair, font slot, media slot, accepted MIME type, size/dimension bound, and rendering mode. `GET /api/v1/themes` exposes the same non-secret registry to authoring tools. The canonical implementation is `internal/theme/api.go`; this file explains its stable author-facing rules.

## Bundle boundary

A bundle is either a deterministic `.efstheme` ZIP or an equivalent directory containing `theme.json` and referenced files beneath `assets/`. Custom IDs use lowercase reverse-domain syntax, declare semantic version metadata and an SPDX-shaped license, and directly extend exactly `endlessfs-light` or `endlessfs-dark` with the same appearance.

The accepted manifest objects are `tokens`, `fonts`, and `assets`. Unknown fields, tokens, font slots, asset slots, and unreferenced files fail validation. CSS, HTML, JavaScript, WebAssembly, templates, Markdown, remote/data URLs, raw CSS values, selectors, properties, wording, and behavior are not theme inputs.

## Typed values

- Colors use exactly `#RRGGBB` and are normalized to lowercase.
- Dimensions and unitless numbers are finite JSON numbers checked against the emitted bounds; the application appends the fixed unit.
- Durations and weights are bounded JSON integers.
- Density, easing, fitting mode, and logical font choices are closed enums.
- Shadows are objects containing only bounded `x`, `y`, `blur`, `spread`, and a typed color.

Each token maps one-to-one to `--efs-` plus the token ID with dots changed to hyphens. Type-specific Go serializers produce CSS; manifest strings are never concatenated as CSS or HTML.

## Media and fonts

The semantic registry includes brand logo/mark/favicon; every file-operation, settings, passkey, warning, and error icon; and empty-drive, empty-folder, empty-trash, and upload-failure illustrations. Accepted theme images are sanitized SVG, signature-validated PNG/WebP/AVIF, bounded by the emitted per-slot byte/dimension/pixel limits. Structured raster sprite rectangles must remain within the decoded image and use a pixel ratio from 1 through 4.

SVG validation rejects declarations, scripts, event attributes, style attributes, external/data references, `foreignObject`, text/HTML, animation, and unsupported elements. SVG is served as an external image or mask, never inserted into the DOM. Fonts are bounded WOFF2 declarations in the `interface` or `monospace` logical slots, with regular and bold weights; generated family names prevent CSS injection.

## Compatibility, inheritance, and contrast

Major version 1 is required and the application must support at least the declared minor. An older compatible custom theme inherits every newly introduced token/slot from its immutable built-in parent. Explicit missing/invalid references fail compilation; omitted overrides inherit. The compiler checks normal/muted/inverse/selection/error text and focus-ring contrast against the emitted WCAG ratios.

The built-in themes pass the same compiler and completeness checks. A selected unavailable theme falls back safely, `system` resolves through configured light/dark defaults, a failed custom asset has an already-resolved parent URL, and `?safe-theme=1` always selects immutable built-in light without changing stored state.
