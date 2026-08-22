# marbor UI

The admin dashboard for marbor - React + TypeScript + Vite + Tailwind. Built output is embedded into the `marbor` binary via `go:embed` (`internal/admin/web/dist`), so there is no separate UI deployment.

## Commands

```bash
npm install        # first time only
npm run dev        # vite dev server with HMR (proxies /admin to localhost:8080)
npm run build      # tsc -b && vite build -> ../internal/admin/web/dist
npm run lint       # eslint .
```

Or via the repo's Makefile from the repo root:

```bash
make dev-ui        # dev server, hot reload, hits a backend at localhost:8080
make ui            # production UI build into internal/admin/web/dist
make build         # full build (UI + both Go binaries)
```

## Notes

- The dev server proxies `/admin` and `/login` calls to `http://localhost:8080` - start a marbor backend alongside it (`make backend && ./marbor`).
- **The UI is embedded in the Go binary** (`//go:embed web/dist`). If you change UI code, rebuild it (`make ui` or `make build`) before rebuilding the binary, or the served dashboard will be stale.
- Setting `VITE_FORCE_DEMO=true` builds the public demo instead: base path becomes `/marbor/demo/`, output goes to `ui/dist` rather than the embed directory; mock data lives in `src/lib/mockData.ts` and is unreachable in production builds.
