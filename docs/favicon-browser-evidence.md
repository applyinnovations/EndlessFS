# Favicon and browser image-policy evidence

The application now serves the conventional /favicon.ico URL as an embedded
image/vnd.microsoft.icon asset. It contains PNG frames at 16, 32 and 48 pixels,
derived from the existing endless-folder SVG with librsvg 2.62.3 from the
repository's pinned Nix input and packaged by a Go helper. The fixed URL uses
one-hour caching rather than immutable caching. Existing theme-selected SVG
favicons remain unchanged.

The HTTP regression first returned 404, then verifies ICO structure, bounds,
decoded frame sizes, nonblank content and the existing security headers. No
provider request, durable record, runtime dependency or theme contract changes.

Live Chrome Log/Audits events reproduced the reported img-src violations on
reload. The rejected URL was a data:image/svg+xml favicon badge with the explicit
data-codex-favicon-badge marker injected by the Codex Chrome extension. The
application's same-origin brand and file icons loaded successfully. This is
separate from the earlier GCS upload CORS failure.

The application CSP deliberately allows same-origin images, preview blobs and
configured provider origins; it does not allow arbitrary data URLs. Widening
that policy is unnecessary for any application image and would change its
security contract. No supported badge-disable option was established from the
official extension documentation. Browser Use blocks chrome://extensions, so
extension settings could not be inspected or changed through that interface.
An extension-side fix is still needed if the warning recurs while agent control
injects the badge. No personal file or provider-capability URL is retained here.
