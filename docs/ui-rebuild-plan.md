# EndlessFS new browser UI project plan

Status: active clean-slate implementation plan.

## Project mandate

Delete the existing browser UI and build a new EndlessFS interface from an empty frontend workspace.

This is not a reskin, refactor, component conversion, or progressive modernization of the old interface. Do not carry forward its HTML, CSS, JavaScript structure, selectors, layout assumptions, UI copy, visual hierarchy, components, or interaction patterns. The new application is designed from the brand system and the normative product specifications as if no previous frontend existed.

The clean slate applies to the browser application and its theme system. EndlessFS remains one product, so the provider-portable storage engine, application use cases, HTTP API, passkey identity, direct-transfer boundary, preview service, and security controls remain the platform on which the new UI is built. The legacy Theme API is not a platform constraint: it is deleted only after the finished UI has established the complete replacement contract.

The project launches only as a complete replacement. There is no mixed old/new production shell, compatibility mode, hidden legacy route, or long-lived feature flag.

## Clean-slate rule

The implementation branch begins by removing:

- `internal/web/static/index.html`;
- `internal/web/static/app.css`;
- `internal/web/static/app.js`;
- the asset handler assumptions that only those three files exist;
- browser tests coupled to old selectors, copy, DOM structure, and visual behavior; and
- every obsolete screenshot, fixture, style rule, renderer, event handler, and UI-specific helper;
- Theme API 1.x, its aliases, manifests, generated CSS contract, compatibility behavior, authoring documentation, and implementation-coupled tests, after the new UI contract is complete.

Nothing from those files is copied into the new project. If a behavior matters, it must be justified by a specification or a product acceptance test and implemented anew.

The following are platform contracts, not frontend code, and remain in force:

- authenticated API routes and strict request/response handling;
- virtual-path, authorization, CSRF, origin, capability, and secret-lifetime rules;
- passkey ceremonies and the two-field identity profile;
- provider-direct upload/download behavior;
- portable file, operation, Trash, share, and preview semantics;
- CSP, security headers, same-origin assets, and text-only rendering of untrusted values;
- the security boundary that any replacement theme input remains closed, data-only, and safely recoverable; and
- Nix as the only public build and test interface.

The dedicated project branch may be temporarily non-releaseable after demolition. It must not merge, publish, or claim completion until the entire new UI and every required gate pass.

## Sources of truth

Build the new application from these inputs, in this order of precedence:

1. [v1 specification](./v1-specification.md), especially sections 3.1, 6, 7, 11–14, 18, 21, 22.10, 22.11, and 24.
2. [v1.1 media-preview specification](./v1.1-media-preview-specification.md), especially sections 8.3, 10, 11, 14, and 15.
3. [Brand guidelines](./brand/README.md) for canonical token values, density, visual language, and interaction principles.
4. [Design-system board](./brand/assets/endlessfs-design-system.png) and [logo-system board](./brand/assets/endlessfs-logo-system.png) as ground truth for visual relationships, component intent, and the approved mark geometry.
5. Public HTTP contracts and black-box workflow tests as the behavioral boundary.

The old UI is not a source of truth, a reference implementation, or a design constraint.

The written brand guide, not pixels sampled from either board, is authoritative for token values. A board swatch that differs from the guide is illustrative and must not enter CSS, theme data, tests, or generated assets.

The supplied PNG boards are visual references rather than production assets. Production marks, icons, and fonts must be authored or sourced as discrete files, licensed, validated through their application-asset or theme-media boundary, and embedded in the Go binary.

## Product definition

The new browser application is a precision filesystem instrument. Files are the primary surface; navigation and controls exist only to help users operate on them.

### Visual system

- Background, Foreground, Text Muted, Border, and Surface establish the neutral foundation.
- Primary and Primary Tint communicate active, focused, and selected interaction states only.
- Success, Warning, and Error appear only when their semantic meaning is needed.
- Token names always describe purpose. Themes may change their values without changing their names or meaning.
- Components consume typed semantic tokens; raw color values and hue-named aliases do not appear in component rules.
- The Infinite Folder mark shown on the logo board is approved. Recreate it as an optimized production SVG, then derive the monochrome product mark and favicon from that one geometry source.
- Typography is compact, neutral, and legible. Pin Inter v4.0 at upstream commit `2ce9119398be143fa289c3e180824db1b7ed803e`; embed only licensed WOFF2 assets for Regular 400, Medium 500, and Semibold 600, with exact file digests recorded in the repository.
- Plain surfaces, restrained neutral structure, and typography establish hierarchy. Reject decorative cards, nested containers, gradients, unnecessary shadows, gratuitous rounding, and spacing used only to fill a layout.
- Visible text is limited to concise labels, verbs, names, values, counts, status, and minimal recovery instructions. The operational UI contains no descriptive, promotional, privacy, or conversational prose.

### Files and navigation

- The application opens into the file workspace, not a dashboard.
- A compact rail, toolbar, and breadcrumbs establish location and available actions without competing with files.
- List rows are dense, stable, aligned, and unboxed.
- Dense tables use compact stable headers, predictable column widths, aligned numeric metadata, restrained dividers, and responsive column reorganization without changing row identity or height during updates.
- The media grid is an edge-to-edge contact sheet with square reserved geometry, effectively zero gap, no captions, no card chrome, and overlay selection. A file without a preview receives a deterministic file-type icon and short type/extension cue, never a broken image or indefinite placeholder.
- The storage view is a deterministic, accessible treemap of the current path. Area encodes recursive consumed bytes from the aggregate tree; the largest useful entries remain direct and every omitted positive byte total is represented by an exact **Remaining items** tile.
- Large directory tiles reveal one adaptive second level only when their available geometry can present it legibly. One bounded, owner-scoped hierarchy response supplies that detail; the browser never performs recursive or per-directory lookup fan-out, and the total interactive tree remains capped independently of directory size.
- Activating a **Remaining items** tile opens its represented directory in the detail view, sorts largest first, keeps the filter controls closed, and applies the exact maximum-size cutoff for the omitted set so progressive detail never becomes repeated manual navigation.
- File-browser URLs are complete navigation snapshots: current path, open file preview, search, metadata filters and disclosure state, sort, and presentation are canonically serialized and restored across refresh, back/forward navigation, and copied links. Infinite-scroll batch depth is deliberately excluded because it is transient rendering state. Parsing is allowlisted and bounded so invalid state fails to deterministic defaults.
- Collections scroll continuously while server requests remain bounded and paginated.
- “Design for 1,000” is a scale mindset, not a universal fixture size, literal rendering requirement, or acceptance threshold. Each container anticipates disproportionate growth and chooses virtualization, aggregation, grouping, filtering, progressive detail, or summary-plus-exception presentation appropriate to its content.
- Large directories remain comfortable to browse and select without unbounded DOM or request counts; they do not render every row or tile simultaneously merely to prove scale.
- Loaded files remain interactive while more data, previews, or operations are in progress.

### Interaction model

- Selection, keyboard navigation, menus, sheets, and actions use one predictable model across every view.
- The file viewport is the upload drop target. Drag feedback appears only during an active drag and reserves no permanent space.
- Routine reversible actions execute immediately and offer a brief Undo action.
- Permanent deletion, empty Trash, and similarly irreversible actions require explicit confirmation.
- Primary actions use Foreground; secondary actions use Surface or Background; icon-only controls are used only for established actions with accessible names. Hover, focus, selected, active, pressed, and disabled states are distinct without changing control geometry.
- Substantial secondary work uses a side sheet on wide screens and a full-screen surface on narrow screens.
- Small dialogs are reserved for short, consequential decisions.
- Transfers use a dedicated right-side monitoring sheet on wide screens and a full-screen sheet on small screens. The sheet aggregates routine completion, prioritizes active and failed work, bounds rendered rows, and keeps large transfer sets inspectable without compressing the file browser into a floating card. A persistent header action reopens the sheet while transfer history exists. Ordinary progress uses Foreground, Error is reserved for actionable failure, and completed work creates no persistent Success decoration.
- Feedback is concise, non-blocking, and announced accessibly without shifting the workspace.
- Floating UI uses predictable safe regions and collision handling. It never covers the current selection, focused element, primary actions, or failure remediation; it collapses, repositions, or closes when no longer needed and restores focus deterministically.
- Motion makes state changes feel immediate and elegant without delaying input. It uses reserved geometry, never causes layout shift or unexpected reordering, and preserves the same result when reduced motion is enabled.

### Responsive and accessible behavior

- Desktop and 320 CSS-pixel layouts expose the same mental model.
- Every function is keyboard reachable with visible focus and sensible focus restoration.
- Icon-only controls have application-owned accessible names.
- Color is never the only status cue.
- Touch hit areas remain operable without making the visible system spacious.
- Orientation changes preserve location, selection, active work, focus intent, and scroll context.
- Common actions stay visible or at most one menu level deep; narrow layouts have no horizontal page scrolling.
- Reduced motion is respected without removing state cues.
- Loading, preview, progress, error, and offline states reserve stable geometry. Spinners are limited to compact actions; directories, grids, tables, and sheets use same-geometry placeholders or progressive content and never become workspace-wide spinner screens.

## Resolved decisions and remaining design input

- Application Trash and backend object retention are separate layers. Moving a file to Trash keeps it visible and restorable until explicit restore, permanent deletion, or empty Trash; the application presents no automatic Trash deadline. After permanent deletion, a configured storage provider may retain provider-native object versions under its own soft-delete or retention policy, such as 30 days. That policy is non-portable infrastructure protection and never becomes an EndlessFS UI restore promise.
- Inter v4.0 is selected and pinned to commit `2ce9119398be143fa289c3e180824db1b7ed803e` under the SIL Open Font License 1.1. Stage 1 imports the 400, 500, and 600 WOFF2 assets, records each digest and license, and embeds them; runtime font requests are prohibited.
- The Infinite Folder mark on the supplied imagery is approved. Stage 1 creates and validates one production SVG source and derives the favicon/product variants from it.
- The supplied full UI board defines light mode only. The project derives a complete dark parent from the same semantic roles and layout, but final dark-mode visual-fidelity approval remains pending until a dark reference board is supplied or the derived dark fixture is explicitly approved.
- The browser-facing color contract is `color.background`, `color.foreground`, `color.text.muted`, `color.border`, `color.surface`, `color.primary`, `color.primary.tint`, `color.success`, `color.warning`, and `color.error`.
- The exact values in the brand-guide table are canonical. Board pixels and board annotations do not override them.
- Theme API 1.x and every installed theme authored for it are unsupported by the replacement. There is no alias adapter, compatibility compiler, migration mode, dual contract, or legacy fallback. After the rebuilt UI freezes its complete semantic surface, the old API is deleted and the new API is authored directly from that surface.
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
2. Define deterministic geometry, accessibility, visual-restraint, text-sparsity, responsive, scale, and performance contracts before implementation begins.
3. Add `nix run .#test-ui-benchmark` and a committed benchmark method for representative and stress workloads chosen according to each container's data shape, interaction model, and plausible operating scale. Calibrate and freeze each component's first budgets before that component is built.
4. Delete the entire old HTML, CSS, and JavaScript implementation and its structure-coupled tests.
5. Remove the three-file asset-handler assumptions.
6. Create the new directory structure, empty asset manifest, test harness, and benchmark harness.
7. Add a repository check that rejects reintroduction of the deleted files, selectors, or a second browser shell.

Exit: the repository contains no old browser implementation; the new project has measurable contracts and one clearly bounded, intentionally unfinished browser workspace.

### Stage 1 — Define the design foundation

1. Define the semantic color contract from the exact written guideline values, plus typography, spacing, density, radius, motion, elevation, focus, control, row, toolbar, thumbnail, and reserved-geometry contracts.
2. Recreate the approved Infinite Folder mark as optimized static-subset SVG, validate it against the logo board at product and favicon sizes, and derive the monochrome product mark and favicon files from that source.
3. Create the required semantic operation and file-type icon assets, including deterministic non-previewable-file representations.
4. Import Inter v4.0 WOFF2 assets for weights 400, 500, and 600 from the pinned upstream commit; record exact digests and the OFL-1.1 license, embed them, and prove that no runtime font request occurs.
5. Define compact primary, secondary, icon-only, grouped, pressed, disabled, focus, selected, active, and destructive control states with no geometry changes between states.
6. Define dense table anatomy, responsive column priorities, numeric alignment, truncation/full-value behavior, selection treatment, and row-height invariants.
7. Define zero-shift loading geometry, compact spinner limits, floating-surface safe regions, collision behavior, closure, and focus restoration.
8. Define short, state-driven motion using reserved geometry and equivalent reduced-motion outcomes.
9. Build the application-owned light and dark foundations directly from the canonical semantic contract, without importing or conforming to Theme API 1.x.
10. Build a complete component/state fixture for light and dark at desktop and 320px.
11. Prove contrast, token completeness, source-value authority, visual restraint, text sparsity, and checksum inventories for the application-owned foundation.

Exit: the visual language exists as tested primitives and production assets before any product screen is assembled.

### Stage 2 — Build the application runtime and shell

1. Implement the explicit Go embedded-asset handler and content-security tests.
2. Build the semantic document shell, pre-paint system appearance selection, skip navigation, live regions, and application landmarks.
3. Build the state container, API client, session boundary, router, focus manager, status system, and feature cleanup lifecycle.
4. Build the compact rail, toolbar, workspace, menu, toast, sheet, and irreversible-dialog primitives with their complete control states and floating-surface collision rules.
5. Build a layout-stability observer in the Chromium harness and fail tests on unexpected post-render layout shift.
6. Prove exact routes, MIME types, caching, CSP, external-request denial, text-only untrusted rendering, absence of operational prose, and cleanup on repeated navigation.

Exit: a secure, responsive application frame exists with no product workflow placeholders exposed as complete.

### Stage 3 — Build identity and entry workflows

1. Build loading, sign-in, bootstrap, public registration, invite registration, and recovery screens.
2. Implement passkey creation and authentication from the documented WebAuthn contracts.
3. Keep raw bootstrap, invite, recovery, ceremony, and session values ephemeral.
4. Test keyboard-only entry, error recovery, expiry, denial, focus behavior, dark mode, and 320px layouts.

Exit: every supported entry path reaches the authenticated application securely and accessibly.

### Stage 4 — Build the file workspace

1. Build breadcrumbs, directory state, progressive page loading, filtering, sorting, and stable loading/error geometry.
2. Build the virtualized dense list, compact stable table headers, edge-to-edge virtualized media grid with deterministic file-type fallbacks, and bounded accessible storage treemap from the supplied aggregate tree.
3. Build pointer and keyboard selection for one item through thousands.
4. Build direct toolbar/menu commands for folder creation, upload, download, share, copy, move, Trash, and Undo.
5. Make the workspace the transient upload drop target with file, folder, recursive-drop, and multi-file fallbacks.
6. Benchmark representative and stress-scale directories through list, grid, and storage-map navigation, scrolling, filtering, sorting, range selection, command invocation, preview loading, pagination recovery, and responsive reflow. Assert bounded live DOM, bounded requests, non-overlapping deterministic treemap geometry, preserved context, and zero unexpected layout shift; do not render all items at once merely to satisfy a fixture.

Exit: the filesystem workspace is complete and files remain the dominant interface at every supported size.

### Stage 5 — Build transfers, previews, and the viewer

1. Build the compact transfer queue with grouping, automatically tuned bounded concurrency, confirmed progress, batch cancellation, bounded automatic retry with jitter and correlated-failure backoff, bulk failed-item retry, aggregate completion, and automatic retirement from the current view.
2. Keep a closed-schema, device-local transfer ledger in IndexedDB, partitioned by authenticated owner only to prevent cross-account exposure on a shared browser profile. Persist safe request metadata, confirmed offsets, terminal state, retry schedule, and optional browser file handles; never persist file bytes, capability URLs or headers, session/CSRF material, provider-native identifiers, or absolute local paths. Restore this secondary state after the file workspace is interactive and issue zero provider-backed status requests merely because ledger records exist. A restored item without a reacquired source becomes locally `needs-source`; a resumable item with a source re-enters the ordinary bounded queue through its stable idempotency key. Use the owner-scoped upload-status route only for ambiguous active recovery after a real idempotent admission conflict, require explicit source reconnection when a browser handle is unavailable, and make clear that closing the browser pauses work because no service worker or account-wide server queue is introduced.
3. Discover dropped directory trees incrementally, admit discovered files in bounded batches, and render one continuous virtual window with collapsed group summaries plus active/failure exceptions. Do not use manual “load more” controls or rebuild the full visible tree for each progress sample.
4. Build visible/overscan-only preview resolution with bounded concurrency, exact artifact validation, retry limits, cache bounds, abort handling, and object-URL cleanup.
5. Build the full-viewport viewer with previous/next navigation, Generate, Regenerate, explicit safe-original preview, Download, metadata, focus trapping/restoration, Escape, and arrow keys.
6. Build metadata and substantial secondary actions as a wide-screen side sheet and narrow-screen full surface.
7. Benchmark representative and stress-scale transfer queues through incremental discovery, enqueue, progress updates, batch cancellation, automatic and bulk retry, completion aggregation, failure search/filtering, ledger restore, source reconnection, virtual scrolling, and narrow-layout presentation. Assert bounded live DOM and per-progress update work, persistent failure visibility, responsive controls, and zero unexpected layout shift.
8. Prove preview-disabled behavior, aspect-ratio preservation, deterministic file-type fallback, dark/mobile operation, capability secrecy, CSP, offline recovery, and continued interaction during background work.

Exit: transfers and previews feel continuous, compact, recoverable, and independent of ordinary file operation availability.

### Stage 6 — Build the remaining product surfaces

1. Build dense Trash management with restore, conflict handling, permanent deletion, and empty Trash. Selected restore, selected permanent deletion, and Undo submit one atomic batch and consume terminal results without redundant polling; they never loop one provider-backed mutation per row. UI text describes application Trash only and never presents a provider-native retention window as an application guarantee.
2. Build profile, passkey, share, and session settings.
3. Build administration for users, roles, account state, invites, pagination, and recovery links.
4. Build the read-only public file/folder share browser.
5. Benchmark representative and stress-scale batch operations through selection, immediate reversible Trash actions, aggregate progress, partial failure, retry, Undo, and irreversible confirmation where required. Assert bounded command concurrency, compact summary-plus-exception rendering, usable cancellation/remediation, and zero unexpected layout shift.
6. Test one-time secret handling, final-admin protections, invite/recovery expiry, share revocation, denied states, keyboard operation, and 320px layouts.

Exit: every required v1 browser surface exists in the new project and uses the same design and interaction model.

### Stage 7 — Replace the theme system from the finished UI

1. Inventory the completed UI's semantic color, typography, spacing, shape, metric, motion, elevation, interaction, file-state, brand, and media requirements; classify deterministic geometry and pinned typography as application-owned rather than theme input.
2. Delete Theme API 1.x in full: registry, manifests, aliases, compiler assumptions, generated properties, compatibility logic, authoring documentation, fixtures, and tests tied to the old contract.
3. Specify a new major Theme API whose closed purpose-named tokens and semantic assets map exactly to the completed UI inventory.
4. Build new immutable light and dark parents and a new data-only Go compiler with strict parsing, media validation, contrast validation, reference closure, canonical digests, and safe reset. Pinned application fonts stay outside theme input.
5. Build theme selection and theme settings against only the new API; old bundles fail closed as unsupported and are never adapted.
6. Prove that components consume only the new contract, no old token or generated property exists, custom input cannot add behavior or prose, and unavailable or invalid selections recover to the immutable new light parent.

Exit: the finished application can be themed only through the new purpose-based contract, with no code or data path back to Theme API 1.x.

### Stage 8 — Prove, package, and launch

1. Audit every source file for dead code, duplicate primitives, raw values, leaked listeners, observers, timers, abort controllers, object URLs, and caches.
2. Verify focus, complete control states, contrast, target geometry, non-color cues, equivalent reduced-motion outcomes, 320px overflow, orientation context preservation, layout stability, and both built-in themes.
3. Verify dense-table behavior, non-previewable-file representations, floating-surface non-occlusion, compact loading behavior, visual restraint, absence of operational prose, and both built-in themes.
4. Run the committed UI benchmark suite and verify large-collection bounds, continuous loading, loaded-content interactivity, operation recovery, queue aggregation, batch-operation control, and failure isolation against the frozen budgets.
5. Inspect network requests, browser storage, history, DOM text, and logs for forbidden origins, provider keys, capabilities, and secrets.
6. Run the complete Chromium workflow suite under both built-in themes.
7. Update Theme API documentation, user documentation, threat review where boundaries changed, and v1/v1.1 evidence with exact artifacts.
8. Run every focused Nix check followed by the authoritative `nix flake check`.
9. Confirm that no old browser asset, route, selector, compatibility shell, or fallback implementation remains.

Exit: the new UI is the only browser application in the binary and every applicable specification item has current evidence.

## Scale and benchmark contract

`nix run .#test-ui-benchmark` is a required project gate, not an informal profiling command. It runs without network access or wall-clock sleeps and emits a versioned result artifact suitable for release evidence.

- Go benchmarks exercise directory indexing/filtering/sorting/selection, transfer-state reduction, and batch-command state at representative and stress workloads selected for each component. The rationale, data shape, allocation ceiling, and operation-count ceiling are committed with each fixture.
- Go-controlled Chromium benchmarks use fixed viewports, deterministic data, injected clocks, and repeatable input sequences. They measure first usable content, scroll and keyboard response, filter/sort response, queue-update response, batch-progress response, and orientation/context restoration.
- Structural gates assert zero unexpected post-render layout shift, bounded live item nodes relative to viewport plus overscan, bounded request and preview concurrency, bounded command concurrency, and no workspace-wide blocking state.
- Timing and memory budgets are calibrated on the declared Nix benchmark environment at Stage 0, recorded before implementation, and then treated as absolute regression limits. The old UI supplies no baseline and no target.
- Workloads test how each container manages growth; they do not turn illustrative quantities into product-wide criteria. Passing never requires every fixture item to exist simultaneously in the DOM or to receive equal visual detail.

## Acceptance matrix

| Area | Required proof |
|---|---|
| Brand fidelity | Written guideline values override board pixels; exact semantic roles; Inter v4.0 pin/digests; approved SVG mark geometry; dense rows; edge-to-edge grid; compact sheets/transfers. |
| Visual restraint and text | Plain hierarchy; no gratuitous cards, rounding, shadows, gradients, or space-filling; no operational prose; only concise labels, values, counts, state, and recovery text. |
| Controls and floating UI | Complete compact control states without geometry changes; safe-region placement; no occlusion of selection, focus, actions, or failure recovery; deterministic close and focus restoration. |
| Loading and motion | Same-geometry loading states; compact-action spinners only; immediate state-driven motion; equivalent reduced-motion outcomes; zero unexpected layout shift. |
| Files and metadata | Stable compact columns, numeric alignment, deterministic truncation/full values, responsive column priorities, and complete non-previewable file-type representations. |
| Product completeness | Identity, browse, transfer, preview, file operations, Trash, shares, settings, themes, administration, recovery, and public-share workflows meet the specifications. |
| Trash and retention | Application Trash remains until explicit action; irreversible actions are confirmed; provider-native soft-delete/retention is neither presented as Trash nor promised as an application recovery path. |
| Accessibility and responsive behavior | Keyboard-only workflows, semantic names/relationships, visible focus, live status, non-color cues, 320px operation, no horizontal page scroll, one-level access to common actions, and preserved context across orientation. |
| Scale and continuity | Component-appropriate representative and stress benchmarks pass with bounded work, usable aggregation/remediation, progressive page consumption, and loaded-content interactivity; no illustrative quantity becomes a universal threshold. |
| Security and privacy | Required CSP and headers; text-only untrusted rendering; ephemeral secrets; exact-origin and CSRF enforcement; no provider-key or capability leakage; no unapproved network request. |
| Themes | Purpose-based color names, closed data-only inputs, complete light/dark parents, contrast, inheritance, safe fallback, explicit version handling, and application-owned behavior. |
| Reproducibility | No Node or frontend framework; all assets embedded and licensed; Nix is the only public task interface; the full release gate passes. |
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
- Do not call the project complete while production assets are missing, UI benchmark budgets are failing, the dark visual direction is unapproved, or any applicable v1/v1.1 evidence is unchecked.

## Definition of done

The project is complete when the old browser UI is gone; the new application is the sole embedded interface; every required route and workflow is built from the brand system and normative contracts; every core workflow works by keyboard at desktop and 320px under both built-in themes; component-appropriate benchmark gates pass; growing collections remain bounded, understandable, and continuous; motion is elegant without layout shift or nondeterminism; all assets are licensed, validated, and embedded; no forbidden dependency or outbound service exists; and `nix flake check` plus the updated acceptance evidence pass.
