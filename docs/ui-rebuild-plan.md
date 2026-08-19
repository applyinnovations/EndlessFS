# EndlessFS new browser UI project plan

Status: project plan; implementation has not started.

## Project mandate

Delete the existing browser UI and build a new EndlessFS interface from an empty frontend workspace.

This is not a reskin, refactor, component conversion, or progressive modernization of the old interface. Do not carry forward its HTML, CSS, JavaScript structure, selectors, layout assumptions, UI copy, visual hierarchy, components, or interaction patterns. The new application is designed from the brand system and the normative product specifications as if no previous frontend existed.

The clean slate applies to the browser application. EndlessFS remains one product, so the provider-portable storage engine, application use cases, HTTP API, passkey identity, direct-transfer boundary, preview service, theme compiler, and security controls remain the platform on which the new UI is built. Changing those boundaries requires a separate specification and tests; copying the old browser implementation does not.

The project launches only as a complete replacement. There is no mixed old/new production shell, compatibility mode, hidden legacy route, or long-lived feature flag.

## Clean-slate rule

The implementation branch begins by removing:

- `internal/web/static/index.html`;
- `internal/web/static/app.css`;
- `internal/web/static/app.js`;
- the asset handler assumptions that only those three files exist;
- browser tests coupled to old selectors, copy, DOM structure, and visual behavior; and
- every obsolete screenshot, fixture, style rule, renderer, event handler, and UI-specific helper.

Nothing from those files is copied into the new project. If a behavior matters, it must be justified by a specification or a product acceptance test and implemented anew.

The following are platform contracts, not frontend code, and remain in force:

- authenticated API routes and strict request/response handling;
- virtual-path, authorization, CSRF, origin, capability, and secret-lifetime rules;
- passkey ceremonies and the two-field identity profile;
- provider-direct upload/download behavior;
- portable file, operation, Trash, share, theme, and preview semantics;
- CSP, security headers, same-origin assets, and text-only rendering of untrusted values;
- data-only theme compilation and safe fallback; and
- Nix as the only public build and test interface.

The dedicated project branch may be temporarily non-releaseable after demolition. It must not merge, publish, or claim completion until the entire new UI and every required gate are green.

## Sources of truth

Build the new application from these inputs, in this order of precedence:

1. [v1 specification](./v1-specification.md), especially sections 3.1, 6, 7, 11–14, 18, 21, 22.10, 22.11, and 24.
2. [v1.1 media-preview specification](./v1.1-media-preview-specification.md), especially sections 8.3, 10, 11, 14, and 15.
3. [Brand guidelines](./brand/README.md) for product voice, density, visual language, and interaction principles.
4. [Design-system board](./brand/assets/endlessfs-design-system.png) and [logo-system board](./brand/assets/endlessfs-logo-system.png) as the visual ground truth.
5. Public HTTP contracts and black-box workflow tests as the behavioral boundary.

The old UI is not a source of truth, a reference implementation, or a design constraint.

The supplied PNG boards are visual references rather than production assets. Production marks, icons, and fonts must be authored or sourced as discrete files, licensed, validated through the theme pipeline, and embedded in the Go binary.

## Product definition

The new browser application is a precision filesystem instrument. Files are the primary surface; navigation and controls exist only to help users operate on them.

### Visual system

- Background, Foreground, Text Muted, Border, and Surface establish the neutral foundation.
- Primary and Primary Tint communicate active, focused, and selected interaction states only.
- Success, Warning, and Error appear only when their semantic meaning is needed.
- Token names always describe purpose. Themes may change their values without changing their names or meaning.
- Components consume typed semantic tokens; raw color values and hue-named aliases do not appear in component rules.
- The Infinite Folder mark is the product identity. It must be monochrome, asymmetric, continuous, and recognizable at favicon scale.
- Typography is compact, neutral, and legible. Inter may be used only from pinned, licensed, embedded WOFF2 assets.
- Borders, radii, shadows, motion, whitespace, and copy are minimized and always functional.

### Files and navigation

- The application opens into the file workspace, not a dashboard.
- A compact rail, toolbar, and breadcrumbs establish location and available actions without competing with files.
- List rows are dense, stable, aligned, and unboxed.
- The media grid is an edge-to-edge contact sheet with square reserved geometry, effectively zero gap, no captions, no card chrome, and overlay selection.
- Collections scroll continuously while server requests remain bounded and paginated.
- Rendering is virtualized for large directories; 1,000 rows and 10,000 tiles do not create unbounded DOM or request counts.
- Loaded files remain interactive while more data, previews, or operations are in progress.

### Interaction model

- Selection, keyboard navigation, menus, sheets, and actions use one predictable model across every view.
- The file viewport is the upload drop target. Drag feedback appears only during an active drag and reserves no permanent space.
- Routine reversible actions execute immediately and offer a brief Undo action.
- Permanent deletion, empty Trash, and similarly irreversible actions require explicit confirmation.
- Substantial secondary work uses a side sheet on wide screens and a full-screen surface on narrow screens.
- Small dialogs are reserved for short, consequential decisions.
- Transfers use a compact, collapsible queue that prioritizes active and failed work and gets out of the way when complete.
- Feedback is concise, non-blocking, and announced accessibly without shifting the workspace.

### Responsive and accessible behavior

- Desktop and 320 CSS-pixel layouts expose the same mental model.
- Every function is keyboard reachable with visible focus and sensible focus restoration.
- Icon-only controls have application-owned accessible names.
- Color is never the only status cue.
- Touch hit areas remain operable without making the visible system spacious.
- Reduced motion is respected.
- Loading, preview, progress, error, and offline states reserve stable geometry.

## Product constraints requiring explicit decisions

- The brand guide mentions automatic deletion after 30 days, while v1 section 9.11 specifies no automatic Trash retention deadline. The new UI must not promise 30-day retention. A retention policy requires its own approved product and storage specification.
- The supplied full UI board defines light mode only. The project must deliver dark mode through the same layout and semantic roles, but cannot claim approved dark visual fidelity until a dark reference board is approved.
- Inter is not supplied. It cannot be fetched at runtime. Approve a version, subset, license, and WOFF2 digest before it becomes a required production asset; otherwise use the embedded/system fallback.
- The browser-facing color contract is `color.background`, `color.foreground`, `color.text.muted`, `color.border`, `color.surface`, `color.primary`, `color.primary.tint`, `color.success`, `color.warning`, and `color.error`.
- Older installed theme inputs are handled, if supported, by an explicit versioned adapter in the Go theme compiler. The new frontend sees only the semantic contract and never knows an old alias such as `color.accent`.
- Continuous browsing consumes bounded API pages progressively. Filtering remains explicitly limited to loaded metadata and never presents itself as full-drive search.

## New project architecture

Create a new browser project inside the Go repository with no package manager, bundler, framework, or generated frontend output:

```text
internal/web/
  handler.go                    explicit embedded-asset allowlist and responses
  handler_test.go               routing, content type, cache, CSP, and fallback tests
  ui/
    index.html                  semantic application shell and live regions
    css/
      reset.css                 application reset and emergency fallbacks
      foundations.css           type, spacing, semantic tokens, focus, motion
      shell.css                 rail, toolbar, workspace, sheets, responsive layout
      components.css            controls, menus, rows, tiles, toasts, dialogs
      views.css                 route-level composition only
    js/
      main.js                   startup and application composition
      platform/
        http.js                 strict same-origin API and problem handling
        session.js              CSRF, authentication state, and logout
        router.js               supported routes and history
        state.js                explicit observable application state
        focus.js                focus movement, trapping, and restoration
        status.js               live regions, toasts, and recoverable errors
      files/
        model.js                directory, filter, sort, and selection state
        list.js                 virtualized list rendering and navigation
        grid.js                 virtualized contact sheet and lazy previews
        commands.js             create, copy, move, Trash, restore, share, Undo
      transfers/
        queue.js                groups, concurrency, confirmed progress, retry/cancel
        direct.js               capability-mediated provider transfer lifecycle
      previews/
        loader.js               bounded resolution, validation, caching, cleanup
        viewer.js               full-viewport viewer, navigation, metadata, actions
      identity/                 sign-in, bootstrap, invite, and recovery flows
      account/                  profile, passkeys, themes, shares, and session
      admin/                    users, roles, invites, and recovery administration
      public/                   read-only public-share browser
```

The exact filenames may change, but these boundaries are mandatory:

- Domain-shaped state is independent of DOM nodes.
- Views render from explicit state and create untrusted values with text nodes only.
- Each feature owns its listeners, observers, timers, abort controllers, object URLs, and cleanup.
- One command layer owns idempotency keys, operation polling, error translation, announcements, and reversible UI.
- The Go handler serves an explicit same-origin asset manifest with exact MIME types and cache policies.
- Security tests scan every JavaScript and CSS source file, not only an entry point.
- No runtime request is made except to the EndlessFS origin or a returned, authorized provider capability origin.

## Build plan

Every behavior begins with a failing test. Work happens on one dedicated replacement branch, which remains non-releaseable until the complete application passes the final gate.

### Stage 0 — Demolish and establish the empty project

1. Record the required product workflows as black-box acceptance tests derived from the specifications, not from old selectors or markup.
2. Delete the entire old HTML, CSS, and JavaScript implementation and its structure-coupled tests.
3. Remove the three-file asset-handler assumptions.
4. Create the new directory structure, empty asset manifest, and test harness.
5. Add a repository check that rejects reintroduction of the deleted files, selectors, or a second browser shell.

Exit: the repository contains no old browser implementation and one clearly bounded, intentionally unfinished new UI project.

### Stage 1 — Define the design foundation

1. Define the semantic color, typography, spacing, density, radius, motion, elevation, control, row, toolbar, and thumbnail contracts.
2. Create production Infinite Folder logo, mark, and favicon files from the logo board.
3. Create the required semantic operation and file-type icon assets.
4. Approve and embed licensed font assets or lock the system fallback decision.
5. Implement the versioned Go theme-compiler boundary that emits only the new semantic contract.
6. Build a complete component/state fixture for light and dark themes at desktop and 320px.
7. Prove contrast, media safety, inheritance, fallback, token completeness, and checksum inventories.

Exit: the visual language exists as tested primitives and production assets before any product screen is assembled.

### Stage 2 — Build the application runtime and shell

1. Implement the explicit Go embedded-asset handler and content-security tests.
2. Build the semantic document shell, pre-paint theme selection, skip navigation, live regions, and application landmarks.
3. Build the state container, API client, session boundary, router, focus manager, status system, and feature cleanup lifecycle.
4. Build the compact rail, toolbar, workspace, menu, toast, sheet, and irreversible-dialog primitives.
5. Prove exact routes, MIME types, caching, CSP, external-request denial, text-only rendering, and cleanup on repeated navigation.

Exit: a secure, responsive application frame exists with no product workflow placeholders exposed as complete.

### Stage 3 — Build identity and entry workflows

1. Build loading, sign-in, bootstrap, public registration, invite registration, and recovery screens.
2. Implement passkey creation and authentication from the documented WebAuthn contracts.
3. Keep raw bootstrap, invite, recovery, ceremony, and session values ephemeral.
4. Test keyboard-only entry, error recovery, expiry, denial, focus behavior, safe-theme fallback, dark mode, and 320px layouts.

Exit: every supported entry path reaches the authenticated application securely and accessibly.

### Stage 4 — Build the file workspace

1. Build breadcrumbs, directory state, progressive page loading, filtering, sorting, and stable loading/error geometry.
2. Build the virtualized dense list and edge-to-edge virtualized media grid.
3. Build pointer and keyboard selection for one item through thousands.
4. Build direct toolbar/menu commands for folder creation, upload, download, share, copy, move, Trash, and Undo.
5. Make the workspace the transient upload drop target with file, folder, recursive-drop, and multi-file fallbacks.
6. Test 1,000-row and 10,000-tile fixtures, bounded DOM/request counts, pagination recovery, selection scale, conflicts, and no layout shift.

Exit: the filesystem workspace is complete and files remain the dominant interface at every supported size.

### Stage 5 — Build transfers, previews, and the viewer

1. Build the compact transfer queue with grouping, concurrency, confirmed progress, cancellation, retry, failure remediation, and automatic collapse.
2. Build visible/overscan-only preview resolution with bounded concurrency, exact artifact validation, retry limits, cache bounds, abort handling, and object-URL cleanup.
3. Build the full-viewport viewer with previous/next navigation, Generate, Regenerate, explicit safe-original preview, Download, metadata, focus trapping/restoration, Escape, and arrow keys.
4. Build metadata and substantial secondary actions as a wide-screen side sheet and narrow-screen full surface.
5. Prove preview-disabled behavior, aspect-ratio preservation, dark/mobile operation, capability secrecy, CSP, offline recovery, and continued interaction during background work.

Exit: transfers and previews feel continuous, compact, recoverable, and independent of ordinary file operation availability.

### Stage 6 — Build the remaining product surfaces

1. Build dense Trash management with restore, conflict handling, permanent deletion, and empty Trash.
2. Build profile, theme, passkey, share, and session settings.
3. Build administration for users, roles, account state, invites, pagination, and recovery links.
4. Build the read-only public file/folder share browser.
5. Test one-time secret handling, final-admin protections, theme selection/fallback, invite/recovery expiry, share revocation, denied states, keyboard operation, and 320px layouts.

Exit: every required v1 browser surface exists in the new project and uses the same design and interaction model.

### Stage 7 — Prove, package, and launch

1. Audit every source file for dead code, duplicate primitives, raw values, leaked listeners, observers, timers, abort controllers, object URLs, and caches.
2. Verify focus, contrast, target geometry, non-color cues, reduced motion, 320px overflow, layout stability, and both built-in themes.
3. Verify large-collection bounds, continuous loading, loaded-content interactivity, operation recovery, and failure isolation.
4. Inspect network requests, browser storage, history, DOM text, and logs for forbidden origins, provider keys, capabilities, and secrets.
5. Run the complete Chromium workflow suite under both built-in themes.
6. Update Theme API documentation, user documentation, threat review where boundaries changed, and v1/v1.1 evidence with exact artifacts.
7. Run every focused Nix check followed by the authoritative `nix flake check`.
8. Confirm that no old browser asset, route, selector, compatibility shell, or fallback implementation remains.

Exit: the new UI is the only browser application in the binary and every applicable specification item has current evidence.

## Acceptance matrix

| Area | Required proof |
|---|---|
| Brand fidelity | Side-by-side review against both boards; exact semantic roles; approved mark geometry; dense rows; edge-to-edge grid; compact sheets/transfers; stable async geometry. |
| Product completeness | Identity, browse, transfer, preview, file operations, Trash, shares, settings, themes, administration, recovery, and public-share workflows meet the specifications. |
| Accessibility | Keyboard-only workflows, semantic names and relationships, visible focus, sensible restoration, live status, dialog/sheet behavior, non-color cues, reduced motion, and 320px operation. |
| Scale and continuity | Bounded rendering for 1,000 rows and 10,000 tiles, visible/overscan preview loading, progressive page consumption, no workspace-wide blocking, and no unexpected layout shift. |
| Security and privacy | Required CSP and headers; text-only untrusted rendering; ephemeral secrets; exact-origin and CSRF enforcement; no provider-key or capability leakage; no unapproved network request. |
| Themes | Purpose-based color names, closed data-only inputs, complete light/dark parents, contrast, inheritance, safe fallback, explicit version handling, and application-owned behavior. |
| Reproducibility | No Node or frontend framework; all assets embedded and licensed; Nix is the only public task interface; the full release gate is green. |
| Clean replacement | No old HTML, CSS, JavaScript, selectors, hidden routes, compatibility shell, duplicate frontend, or unproved placeholder remains. |

Screenshots support design review but are not the test suite. Behavioral, geometry, accessibility, security, scale, and cleanup assertions must fail deterministically when the new contract regresses.

## Project and commit policy

- Use one dedicated replacement branch. Do not merge partially completed stages into a release branch.
- Commit the demolition explicitly so review can prove that no old frontend survived.
- Keep later commits organized by new-project foundation, runtime, and complete product capability—not by conversion of old screens.
- Start every behavior with a failing test; security work includes denial and valid-path tests.
- Never restore old code to make a new-project test pass.
- Include light desktop, light 320px, dark desktop, dark 320px, focus, loading, selected, error, and high-volume captures for every completed surface.
- Record every deliberate deviation from a reference board and tie it to a normative accessibility, privacy, or product requirement.
- Do not call the project complete while production assets are missing, the dark visual direction is unapproved, or any applicable v1/v1.1 evidence is unchecked.

## Definition of done

The project is complete when the old browser UI is gone; the new application is the sole embedded interface; every required route and workflow is built from the brand system and normative contracts; every core workflow works by keyboard at desktop and 320px under both built-in themes; large collections remain bounded and continuous; asynchronous work does not destabilize the workspace; all assets are licensed, validated, and embedded; no forbidden dependency or outbound service exists; and `nix flake check` plus the updated acceptance evidence are green.
