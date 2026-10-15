# 0003. A server-rendered UI: Go, templ and htmx, no JavaScript build

Status: Accepted

## Context

Argo CD's UI is a React single-page application built with npm. Reproducing
it that way would add a second language, a node_modules tree of hundreds of
packages to audit and keep patched, and a build step to the release, for a
team whose code is Go. The UI also runs inside the Nomad agent, under Nomad's
strict Content-Security-Policy (`default-src 'none'; script-src 'self'`,
no inline scripts, no `eval`).

## Decision

Render the UI on the server in Go with [templ](https://templ.guide) templates,
and make it interactive with [htmx](https://htmx.org) and its server-sent
events extension, vendored as two files checked against their published
`sha512` integrity (`scripts/vendor-htmx.sh`). No npm, no bundler; a hundred
lines of plain JavaScript cover what htmx does not (menus, keyboard
shortcuts, log scrolling).

- Every state of a page has its own URL (`?panel=diff`, `?node=…`), so links,
  history and reloads work, and a request renders either the whole page or,
  for htmx, only the fragment it targets.
- Live updates arrive on an event stream that only signals that something
  changed; the page then asks for the regions it shows, rendered by the same
  templates.
- Writes go through htmx requests that the server checks for same-origin
  headers; nothing relies on inline scripts or `eval`.

## Consequences

- The UI is Go code, type-checked with the rest and tested with plain Go HTTP
  tests; the end-to-end suite drives it in Chromium and fails on any script
  error or CSP violation.
- The release needs only the Go toolchain; `make generate` regenerates the
  templates with a pinned templ version, and `make check-generate` keeps them
  in sync.
- Rich client-side features (a drag-and-zoom graph, instant filtering of
  thousands of resources) cost more than in a SPA: the tree is laid out on the
  server, and filters are applied there.
