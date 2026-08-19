# EndlessFS browser UI rebuild plan

Status: planning only. This document does not claim that the current browser UI conforms to the new brand system.

## Outcome

Replace the browser presentation layer with a files-first, dense, precise interface based on the imported EndlessFS brand system, without changing the provider-portable storage model, weakening a security or accessibility invariant, or introducing a frontend toolchain.

“From scratch” applies to the embedded browser shell, application CSS, DOM rendering, and interaction organization. The existing HTTP API, application use cases, domain rules, direct-transfer boundary, preview service, theme compiler, and security controls remain the behavioral foundation unless a separately tested product requirement proves that a narrow API change is necessary.

## Sources of truth

Use the following precedence when requirements differ:

1. [v1 specification](./v1-specification.md), especially sections 3.1, 6, 7, 11–14, 18, 21, 22.10, 22.11, and 24.
2. [v1.1 media-preview specification](./v1.1-media-preview-specification.md), especially sections 8.3, 10, 11, 14, and 15.
3. [Brand guidelines](./brand/README.md) for product voice, visual language, density, and interaction direction.
4. [Light-mode design-system board](./brand/assets/endlessfs-design-system.png) and [logo-system board](./brand/assets/endlessfs-logo-system.png) as the visual ground truth.
5. The current UI only as a functional regression oracle, not as a visual reference.

The supplied PNG boards are reference artifacts, not production assets. Production logos, icons, and fonts must be authored or sourced separately, validated by the existing theme pipeline, licensed, and embedded in the Go binary.

## Ground-truth interpretation

The reference boards establish these non-negotiable visual qualities:

- Monochrome canvas, typography, and structure dominate. Blue appears only for active, focused, and selected states; semantic colors appear only when their meaning is needed.
- File content dominates a compact navigation rail and toolbar. Controls are aligned, restrained, and no larger than operability requires.
- List rows are dense and scan like a professional filesystem table, with stable columns, subtle dividers, aligned metadata, and no per-row cards.
- Media tiles form an edge-to-edge contact sheet with stable square geometry, effectively no gaps, no captions, and selection drawn as an overlay instead of surrounding chrome.
- Secondary work happens in a contextual side sheet on wide screens and a full-screen surface on narrow screens. Small centered dialogs are reserved for short, consequential decisions.
- Transfer and outcome UI is compact and transient. Routine success does not become persistent green decoration.
- The Infinite Folder mark is monochrome, asymmetric, continuous, and recognizable before the infinity motif becomes apparent. The current text `∞` placeholder is not acceptable production branding.
- Corners, borders, shadows, copy, and whitespace are used sparingly. Geometry remains stable while data and previews load.

Every visual review must compare the implementation directly with both reference boards at readable scale. The prose guidelines are not a substitute for inspecting the images.

## Current-state diagnosis

The existing browser already supplies important behavior that must survive the rewrite: passkey flows, direct resumable transfers, file operations, public shares, admin/settings workflows, a virtualized preview grid, accessible live regions, CSP-compatible DOM construction, safe theme resolution, and Go-controlled Chromium coverage.

The presentation conflicts with the new direction in several concrete ways:

| Current implementation | Required direction |
|---|---|
| Monolithic `index.html`, 204-line stylesheet, and roughly 1,880-line JavaScript closure couple state, API calls, rendering, and interactions. | Small browser modules with explicit state, rendering, and interaction boundaries, served directly with no bundler. |
| A text `∞` stands in for the product mark. | A validated Infinite Folder mark and favicon derived from the logo board. |
| The Drive permanently reserves a bordered upload drop zone. | The file surface becomes the drop target and reveals feedback only during an active drag. |
| Grid items use padded, rounded cards with filenames and metadata. | Edge-to-edge square media, no default captions or card chrome, stable overlay selection. |
| Panels, settings cards, dialogs, spacing, and headings use a spacious SaaS treatment. | Dense filesystem-tool geometry, sheets for substantial secondary UI, and compact type hierarchy. |
| Moving ordinary files to Trash asks for confirmation. | Execute the reversible action immediately and offer a brief Undo path; keep confirmation for permanent deletion and empty Trash. |
| Manual “Load more” controls expose API pagination as the primary browsing model. | Preserve bounded server pagination but progressively fetch into a continuous virtualized presentation. |
| Current built-in light tokens use a blue-gray palette and large radii. | Adopt the brand’s exact neutral and interaction palette, then tune typed density/radius tokens without weakening custom-theme safety. |
| Loading and routine workflow copy is often explanatory or promotional. | Direct, brief, functional language that appears only when it helps the user act or recover. |

## Product and specification decisions

- The brand guide mentions automatic deletion after 30 days, but v1 section 9.11 explicitly says Trash has no automatic retention deadline. The rebuild must not display or imply a 30-day policy. Such retention needs its own approved specification and storage-lifecycle implementation.
- The light-mode board is the only supplied full UI reference. Layout and interaction must remain identical in dark mode, but dark colors should continue through the current safe built-in theme until an approved dark visual board exists. Dark mode cannot be omitted or treated as complete based on the light board.
- Inter is not included in the attachment, and runtime-fetched fonts are forbidden. Use Inter only after adding pinned, licensed WOFF2 files to the validated built-in theme assets and recording their license; otherwise retain the neutral embedded/system fallback.
- Continuous browsing does not remove bounded server pagination. The client requests successive pages as needed, virtualizes rendered rows/tiles, preserves errors and retry boundaries, and never claims that loaded-item filtering searches unloaded content.
- A compact visible control may use a larger invisible hit area where appropriate, but keyboard focus, semantic names, pointer behavior, and the 320 CSS-pixel layout remain application-owned and testable.

## Target browser architecture

Keep the production artifact as one Go binary with self-contained semantic HTML, application-owned CSS, validated theme media, and minimal vanilla JavaScript.

Refactor toward these boundaries:

```text
internal/web/static/
  index.html                 semantic shell and top-level live regions
  css/                       reset, foundations, shell, components, and view rules
  js/app.js                  startup and route composition only
  js/core/                   API client, errors, state, routing, focus, announcements
  js/files/                  list/grid models, virtualization, selection, operations
  js/transfers/              queue, direct transfer lifecycle, retry/cancel rendering
  js/previews/               lazy preview loading, cache, viewer, metadata
  js/account/                registration, passkeys, settings, themes, administration
  js/public/                 public-share browser
```

The exact filenames may change during implementation, but the boundaries must hold:

- API and domain-shaped state are independent of DOM nodes.
- Rendering consumes explicit state and creates untrusted values with text nodes only.
- Route modules own their event handlers and cleanup, including abort controllers, object URLs, observers, and focus restoration.
- One shared command layer owns idempotency keys, operation polling, error translation, announcements, and optimistic/reversible UI.
- The Go asset handler serves an explicit same-origin allowlist of embedded files with exact content types and cache policies.
- Static security tests scan every JavaScript module, not only one entry file, for persistent storage, HTML injection, inline style, service-worker, and outbound-network violations.
- No package manager, framework, preprocessor, generated JavaScript bundle, or runtime third-party asset is introduced.

## Delivery sequence

Each phase starts with a failing observable test, implements the smallest complete vertical slice, and finishes with focused Nix checks. Each replaced route remains usable at the end of its phase; the production binary must never expose a placeholder route or an empty alternate UI.

### Phase 0 — Freeze behavior and visual acceptance

1. Inventory every current route, operation, state, keyboard path, live announcement, API call, and sensitive-value lifetime.
2. Extend characterization tests so all supported workflows are covered before markup is replaced, including conflict, offline, empty, loading, denied, expired, retry, cancellation, and preview-disabled states.
3. Add deterministic browser fixtures for 1,000 list entries and 10,000 grid entries.
4. Create a component/state fixture covering every control, row, tile, menu, toast, sheet, dialog, transfer state, empty/error state, and typography level in both built-in themes at desktop and 320px.
5. Record a measurement sheet from the reference boards: rail/toolbar proportions, row rhythm, control geometry, type hierarchy, dividers, thumbnail geometry, and selected/focus overlays. Do not invent measurements from the current UI.

Exit: the regression and visual acceptance matrix is reviewed before implementation begins.

### Phase 1 — Brand assets and typed foundations

1. Write failing theme/media tests for the required logo, mark, favicon, file-operation icons, exact brand colors, compact density, contrast, and safe fallback.
2. Produce clean, individually addressable SVG assets from the logo and UI reference boards. Validate favicon-scale legibility and the static SVG subset; never ship a crop of the reference board as an application asset.
3. Decide and document the Inter font source, version, subset, license, WOFF2 hashes, and inventory entry before embedding it. If those inputs are not approved, keep the system fallback.
4. Update existing typed theme tokens first. Add a Theme API token or semantic media slot only when no existing typed contract can express a required visual role; bump the compatible minor API and prove older-theme inheritance when adding one.
5. Update both immutable built-in themes through the ordinary compiler and update the theme conformance fixture before application CSS consumes the new values.

Exit: `theme-check`, `test-theme`, media validation, contrast, inheritance, and fallback tests pass, and the light foundation matches the reference palette and mark.

### Phase 2 — New shell, navigation, and identity entry

1. Replace the page skeleton with minimal semantic landmarks, skip navigation, a compact rail/top toolbar, stable workspace geometry, and pre-paint theme resolution.
2. Implement responsive navigation with the same mental model at desktop and 320px; collapsed controls retain accessible names and predictable focus order.
3. Rebuild loading, sign-in, bootstrap, invite registration, and recovery surfaces with brief functional copy and the real product mark.
4. Establish shared primitives for buttons, fields, toolbar groups, status messages, toasts, menus, side sheets/full-screen sheets, and irreversible-action dialogs.
5. Test keyboard-only bootstrap/sign-in/recovery, focus visibility/restoration, no layout overflow, no external requests, and both built-in themes.

Exit: identity entry and the authenticated shell are complete and regression-safe; old shell styles and handlers are removed rather than layered underneath.

### Phase 3 — Drive list as the primary vertical slice

1. Rebuild breadcrumbs, directory loading, stable skeletons, loaded-item filtering, sorting, selection, and the dense file table.
2. Make rows virtualized or equivalently bounded for large directories while keeping semantic navigation and selection usable by keyboard and assistive technology.
3. Replace per-row modal action selection with direct, context-appropriate toolbar/menu actions. Selection remains legible from one item through thousands.
4. Turn the entire file viewport into the drop target. Show a non-shifting drag overlay only while dragging; retain file, folder, and recursive-drop fallbacks.
5. Replace routine Trash confirmation with immediate execution plus a bounded Undo toast. Confirm only permanent deletion, empty Trash, or another genuinely irreversible action.
6. Progressively request additional server pages near the loaded boundary, expose recoverable page failures without blocking loaded files, and keep filtering labeled as loaded-item filtering.

Exit: browse, create folder, select, upload initiation, download, share, copy, move, and Trash work in the new list UI with pointer and keyboard input.

### Phase 4 — Media grid, viewer, and metadata

1. Write failing geometry and bounded-DOM tests for an edge-to-edge grid: square reserved frames, effectively zero gap, no captions, no card chrome, overlay selection, and no cumulative layout shift when images resolve or fail.
2. Preserve visible/overscan-only preview requests, bounded concurrency, content validation, object-URL cleanup, retry limits, provider isolation, and preview-disabled behavior.
3. Rebuild the viewer as an accessible full-viewport surface with stable controls, previous/next navigation, Generate/Regenerate, explicit safe-original preview, Download, metadata, focus trapping/restoration, Escape, and arrow keys.
4. Present substantial metadata/actions as a side sheet on wide layouts and a full-screen secondary surface on narrow layouts.
5. Re-run the 10,000-entry fixture, aspect-ratio checks, dark/mobile workflow, CSP, capability secrecy, and external-origin assertions.

Exit: v1.1 MP-002, MP-008, MP-009, MP-013, and MP-014 behavior remains proven in the new UI.

### Phase 5 — Transfers and operational feedback

1. Rebuild the upload queue as a compact collapsible tray with grouped progress, failures prioritized over completed items, and explicit retry/cancel actions.
2. Continue to display confirmed uploaded bytes and label indeterminate work accurately.
3. Add a single toast region for concise outcomes and Undo. Toasts must not cover primary file actions, trap focus, or remain indefinitely.
4. Preserve working-file interaction while transfers, operation polling, previews, and page loading continue.
5. Exercise high-volume transfer fixtures, cancellation, lost responses, resume, retry, conflict policies, offline/online transitions, and non-disruptive announcements.

Exit: transfer status is compact, scalable, recoverable, and never blocks unrelated file work.

### Phase 6 — Trash, settings, administration, and public shares

1. Rebuild Trash as a dense recovery table with compact restore controls and visually distinct permanent deletion.
2. Replace settings card grids with efficient sections/sheets for profile, theme, passkeys, shares, and session actions.
3. Rebuild administration around dense invite/user tables, preserving one-time secret handling, final-admin protections, pagination, recovery, and status distinctions.
4. Rebuild public file/folder shares with the same file-browsing model, while keeping the token out of subresource URLs and preserving read-only scope.
5. Run the complete invite, second-passkey, safe-theme, admin, recovery, public-share, revoke, expired/denied, and 320px keyboard workflows.

Exit: every v1 browser view exists in the new system with no legacy route-level presentation remaining.

### Phase 7 — Hardening, proof, and cutover

1. Delete superseded markup, CSS, renderers, event handlers, and tests; do not retain a hidden legacy shell.
2. Audit listener, observer, timer, abort-controller, object-URL, and cache cleanup across repeated navigation.
3. Verify no horizontal overflow at 320px, no unexpected layout shifts, distinct focus/hover/active/selected states, reduced-motion behavior, and sufficient contrast in both built-in themes.
4. Verify 1,000-row and 10,000-tile DOM/request bounds, continuous page loading, loaded-content interactivity, and failure recovery.
5. Inspect all network requests and browser storage; allow only the EndlessFS origin and explicit capability origins, and persist no secret responses.
6. Update user documentation, Theme API inventory, threat review when boundaries changed, and v1/v1.1 evidence with exact test artifacts.
7. Run focused checks followed by the authoritative `nix flake check` before making any completion claim.

Exit: every item in the acceptance matrix and specification section 24 is evidenced, both built-in themes pass, and no unchecked UI claim remains.

## Acceptance matrix

| Area | Required proof |
|---|---|
| Visual fidelity | Side-by-side review against both imported boards; exact light tokens; approved mark geometry; dense rows; edge-to-edge grid; compact sheets/transfers; stable async geometry. |
| Functional parity | Existing identity, browse, transfer, preview, file operation, Trash, share, settings, theme, admin, recovery, and public-share workflows pass without backend regression. |
| Accessibility | Keyboard-only workflows, semantic names/relationships, visible focus, sensible restoration, live status, dialog/sheet behavior, non-color cues, reduced motion, and 320px operation. |
| Scale and continuity | Bounded rendered DOM for 1,000 rows and 10,000 tiles, visible/overscan preview loading, progressive page fetch, no whole-workspace blocking, no unexpected layout shift. |
| Security and privacy | CSP and headers unchanged or stronger; text-only rendering of untrusted values; no persistent secrets; exact-origin/CSRF behavior preserved; no provider key/capability leakage; no unapproved network request. |
| Themes | Closed data-only boundary remains intact; complete light/dark parents; contrast and fallback pass; older compatible themes inherit any additive slots; application owns layout and behavior. |
| Reproducibility | No Node or frontend framework; all assets embedded and licensed; Nix remains the only public task interface; `nix flake check` is green. |

Screenshots are review evidence, not the only test. Behavioral, geometry, accessibility, security, and scale assertions must fail deterministically when the contract regresses.

## Review and commit strategy

- Keep phases reviewable and land one complete vertical slice at a time.
- Begin each behavior or security change with the regression/denial test required by `AGENTS.md`.
- Include light desktop, light 320px, dark desktop, dark 320px, focus, loading, selected, error, and high-volume captures for each affected surface.
- Call out every deliberate divergence from a reference board and tie it to a normative accessibility, privacy, or functional requirement.
- Do not call the rebuild complete while the dark visual direction is unapproved, required production assets are missing, or any applicable v1/v1.1 evidence is unchecked.

## Definition of done

The rebuild is complete only when the imported boards are visibly reflected across every supported route, all existing behavior is preserved or deliberately re-specified, every core workflow works by keyboard at desktop and 320px under both built-in themes, large collections remain bounded and continuous, async work does not destabilize the layout, all assets are validated and embedded, no forbidden dependency or outbound service was added, and `nix flake check` plus the updated acceptance evidence are green.
