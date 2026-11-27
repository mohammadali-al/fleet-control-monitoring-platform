# Fleet panel front-end (React SPA)

The operator panel UI is a React single-page application bundled with **esbuild**.
The built artifacts are committed and embedded into `fleet-server` via `web/embed.go`,
so `go build ./...`, `./scripts/build.sh`, and CI do **not** require Node.js.

## Rebuilding the bundle

Requires Node.js and npm:

```sh
./web/build.sh
```

Or explicitly:

```sh
npm --prefix web ci
npm --prefix web run build
```

This compiles `web/src/main.jsx` and the design-system stylesheets under `web/assets/css/`
into:

- `web/assets/dist/app.js`
- `web/assets/dist/app.css`
- `web/dist/index.html`

React, React DOM, and React Router are bundled locally (no runtime CDN requests).
Fonts under `web/assets/fonts/` and vendored `web/assets/vendor/xterm/` are self-hosted.

## Adding / porting a page route

Each panel page lives in its own component file under `web/src/pages/` and is registered
in `web/src/routes.js`:

1. Create `web/src/pages/<Name>Page.jsx` exporting the page component (it renders inside
   the persistent `<Shell>` via `<Outlet />`; set `document.title` with `usePageTitle(...)`).
2. In `web/src/routes.js`, import the component and set `component: <Name>Page` on that
   route's entry in `ROUTES`.
3. In `internal/panel/server.go`, move `<name>` out of `legacyPages` once its React route
   replaces the legacy `.html` page (or keep `.html` redirecting to `/<name>`).
4. Run `./web/build.sh` and commit the updated bundle alongside your source changes.

Un-ported entries in `ROUTES` keep `component: null`: the sidebar links to their legacy
`/<name>.html` page, and direct SPA navigation to `/<name>` redirects to `/<name>.html`.
