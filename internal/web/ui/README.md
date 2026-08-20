# Browser source layout

EndlessFS serves one immutable `/assets/ui.js` script and one immutable
`/assets/ui.css` stylesheet. Their source is split by application domain and
assembled in `internal/web/web.go`; no runtime loader or frontend build tool is
required.

JavaScript source order is significant. `js/core.js` opens the private
application scope, the domain files add behavior to that scope, and
`js/bootstrap.js` wires events and closes it. Add or reorder a domain only by
updating `applicationScriptSources` and its boundary test together.

CSS source order is also significant because the files form one cascade.
Foundation tokens and primitives load first, domain presentation follows, and
responsive rules load last. Update `applicationStylesheetSources` and its
boundary test together when changing that order.

The public asset URLs stay stable so the embedded application, CSP, cache
policy, and theme contract do not depend on the internal source layout.
